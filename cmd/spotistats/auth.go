package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/spotify"
)

func runAuth(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("auth needs a subcommand: login or status")
	}
	switch args[0] {
	case "login":
		return runAuthLogin(ctx, args[1:])
	case "status":
		return runAuthStatus(ctx, args[1:])
	case "-h", "--help":
		fmt.Printf("Usage: %s auth <login|status>\n", progName)
		return nil
	default:
		return fmt.Errorf("unknown auth subcommand %q (want login or status)", args[0])
	}
}

// ---------------------------------------------------------------------------
// auth login
// ---------------------------------------------------------------------------

func runAuthLogin(ctx context.Context, args []string) error {
	fs := newFlagSet("auth login", "auth login [flags]")
	noBrowser := fs.Bool("no-browser", false, "print the authorisation URL instead of opening a browser")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the browser callback")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := config.Load()
	creds, err := cfg.ResolveCredentials(ctx)
	if err != nil {
		return fmt.Errorf("resolve Spotify credentials: %w", err)
	}
	tokenStore, err := cfg.ResolveTokenStore(ctx)
	if err != nil {
		return fmt.Errorf("resolve token store: %w", err)
	}

	// The listener must bind the exact host and port of the registered redirect URI, since
	// Spotify validates the value byte for byte on the token exchange.
	redirect, err := url.Parse(cfg.RedirectURI)
	if err != nil {
		return fmt.Errorf("parse %s=%q: %w", config.EnvRedirectURI, cfg.RedirectURI, err)
	}
	if redirect.Hostname() == "localhost" {
		return fmt.Errorf("redirect URI %q uses `localhost`, which Spotify rejects; use the "+
			"loopback IP literal http://127.0.0.1:%s%s instead",
			cfg.RedirectURI, portOf(redirect), redirect.Path)
	}

	state, err := randomState()
	if err != nil {
		return err
	}

	authURL, err := spotify.AuthorizeURL(spotify.AuthorizeURLParams{
		ClientID:    creds.ClientID,
		RedirectURI: cfg.RedirectURI,
		State:       state,
		Scopes:      spotify.RequiredScopes(),
		ShowDialog:  true,
	})
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(redirect.Hostname(), portOf(redirect))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s (is another process using it?): %w", addr, err)
	}
	defer func() { _ = ln.Close() }()

	result := make(chan callbackResult, 1)
	srv := &http.Server{
		Handler:           callbackHandler(redirect.Path, state, result),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	heading("Authorise Spotistats with Spotify")
	bullet("scopes:   %s", strings.Join(spotify.RequiredScopes(), ", "))
	bullet("callback: %s", cfg.RedirectURI)
	fmt.Printf("\nOpen this URL and click Agree:\n\n%s\n\n", authURL)

	if !*noBrowser {
		if err := openBrowser(authURL); err != nil {
			bullet("could not open a browser automatically (%v); use the URL above", err)
		}
	}
	bullet("waiting up to %s for the callback...", *timeout)

	waitCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	var cb callbackResult
	select {
	case cb = <-result:
	case <-waitCtx.Done():
		return fmt.Errorf("timed out waiting for the browser callback after %s", *timeout)
	}
	if cb.err != nil {
		return cb.err
	}

	// The code is single-use and expires in about a minute, so exchange it immediately.
	exchanger, err := spotify.NewAuthCodeExchanger(spotify.AuthCodeConfig{
		Credentials: creds,
		RedirectURI: cfg.RedirectURI,
		Retry:       spotify.DefaultRetryPolicy(),
		Logger:      cfg.Logger(),
	})
	if err != nil {
		return err
	}
	tokens, err := exchanger.Exchange(ctx, cb.code)
	if err != nil {
		return fmt.Errorf("exchange authorisation code: %w", err)
	}

	if missing := tokens.HasScopes(spotify.RequiredScopes()...); len(missing) > 0 {
		return fmt.Errorf("authorisation granted without the required scopes %v; "+
			"re-run and accept all of them", missing)
	}

	if err := tokenStore.Put(ctx, tokens.RefreshToken); err != nil {
		return fmt.Errorf("store refresh token: %w", err)
	}

	heading("Authorised")
	bullet("granted scopes: %s", tokens.Scope)
	if fs, ok := tokenStore.(*config.FileTokenStore); ok {
		bullet("refresh token written to %s (mode 0600)", fs.Path())
		bullet("this file is local-development storage and is unencrypted at rest")
	} else {
		bullet("refresh token written to SSM parameter %s", cfg.RefreshTokenParam())
	}
	bullet("keep a copy in your password manager: losing it means repeating this flow")
	fmt.Printf("\nVerify with:  %s auth status\n", progName)
	return nil
}

type callbackResult struct {
	code string
	err  error
}

// callbackHandler serves the loopback redirect, validating the state parameter.
func callbackHandler(path, wantState string, out chan<- callbackResult) http.Handler {
	mux := http.NewServeMux()
	if path == "" {
		path = "/"
	}
	var once bool
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		send := func(res callbackResult) {
			if once {
				return
			}
			once = true
			out <- res
		}

		if e := q.Get("error"); e != "" {
			// The most common value here is access_denied, i.e. the user pressed Cancel.
			http.Error(w, "Authorisation failed: "+e, http.StatusBadRequest)
			send(callbackResult{err: fmt.Errorf("Spotify returned error=%q", e)})
			return
		}

		// A mismatched state means the response did not originate from this request.
		if got := q.Get("state"); got != wantState {
			http.Error(w, "State mismatch; authorisation rejected.", http.StatusBadRequest)
			send(callbackResult{err: errors.New("state parameter did not match; " +
				"the callback did not originate from this request")})
			return
		}

		code := q.Get("code")
		if code == "" {
			http.Error(w, "No authorisation code in the callback.", http.StatusBadRequest)
			send(callbackResult{err: errors.New("callback carried no authorisation code")})
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8">` +
			`<title>Spotistats</title></head><body style="font-family:system-ui;padding:3rem">` +
			`<h1>Authorised</h1><p>You can close this tab and return to the terminal.</p>` +
			`</body></html>`))
		send(callbackResult{code: code})
	})
	return mux
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func openBrowser(target string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	return exec.Command(cmd, append(args, target)...).Start()
}

// ---------------------------------------------------------------------------
// auth status
// ---------------------------------------------------------------------------

func runAuthStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("auth status", "auth status")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := config.Load()
	deps, err := config.Build(ctx, cfg, config.BuildOptions{NeedSpotify: true})
	if err != nil {
		return err
	}

	heading("Token")
	if cfg.UsesLocalTokenFile() {
		bullet("store: file %s", cfg.TokenFile)
	} else {
		bullet("store: SSM %s", cfg.RefreshTokenParam())
	}

	// Exercising the real refresh is the only meaningful check: a stored token that Spotify
	// has revoked is indistinguishable from a good one until it is used.
	if _, err := deps.TokenSource.Token(ctx); err != nil {
		if errors.Is(err, spotify.ErrRefreshTokenInvalid) {
			return fmt.Errorf("the stored refresh token is invalid or revoked; "+
				"re-run `%s auth login`: %w", progName, err)
		}
		return fmt.Errorf("refresh access token: %w", err)
	}
	bullet("refresh: ok (exchanged for an access token)")

	page, err := deps.Spotify.RecentlyPlayed(ctx, spotify.RecentlyPlayedOptions{Limit: 5})
	if err != nil {
		return fmt.Errorf("call recently-played: %w", err)
	}

	heading("Most recent plays (%d returned)", len(page.Plays))
	if len(page.Plays) == 0 {
		bullet("none - Spotify reports no recent listening")
	}
	// Newest first reads better for a human, so reverse the pipeline's oldest-first order.
	for i := len(page.Plays) - 1; i >= 0; i-- {
		p := page.Plays[i]
		name := p.TrackID
		if tr, ok := page.Tracks[p.TrackID]; ok && tr.Name != "" {
			name = tr.Name
		}
		bullet("%s  %s", p.PlayedAt.Format(time.RFC1123), name)
	}
	fmt.Println()
	bullet("note: durations are estimates - recently-played carries no ms_played")
	return nil
}
