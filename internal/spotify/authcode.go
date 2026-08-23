package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/neovasili/spotistats/internal/httpx"
)

// Scopes Spotistats needs, and nothing more.
//
//   - user-read-recently-played is the only source of play events.
//   - user-top-read provides Spotify's own top-items rankings, stored alongside the
//     computed leaderboards.
//
// Requesting anything else would enlarge the consent screen and the blast radius of a
// leaked token for no benefit.
const (
	ScopeRecentlyPlayed = "user-read-recently-played"
	ScopeTopRead        = "user-top-read"
)

// RequiredScopes is the exact scope set requested during authorisation.
func RequiredScopes() []string { return []string{ScopeRecentlyPlayed, ScopeTopRead} }

// DefaultAuthorizeURL is Spotify's authorisation endpoint.
const DefaultAuthorizeURL = "https://accounts.spotify.com/authorize"

// AuthorizeURLParams builds the authorisation URL the user must visit.
type AuthorizeURLParams struct {
	ClientID string
	// RedirectURI must match a URI registered on the app byte for byte. Spotify forbids
	// `localhost` and permits plain HTTP only for an explicit loopback IP literal such as
	// http://127.0.0.1:8888/callback.
	RedirectURI string
	// State is echoed back on the callback and must be compared, to reject a response that
	// did not originate from this request.
	State  string
	Scopes []string
	// ShowDialog forces the consent screen even when the user has already authorised, which
	// makes re-running the flow predictable.
	ShowDialog bool
	BaseURL    string
}

// AuthorizeURL renders the authorisation URL.
func AuthorizeURL(p AuthorizeURLParams) (string, error) {
	if p.ClientID == "" {
		return "", errors.New("spotify: client ID is required")
	}
	if p.RedirectURI == "" {
		return "", errors.New("spotify: redirect URI is required")
	}
	base := p.BaseURL
	if base == "" {
		base = DefaultAuthorizeURL
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("spotify: parse authorize URL: %w", err)
	}
	scopes := p.Scopes
	if len(scopes) == 0 {
		scopes = RequiredScopes()
	}

	q := url.Values{
		"client_id":     {p.ClientID},
		"response_type": {"code"},
		"redirect_uri":  {p.RedirectURI},
		"scope":         {strings.Join(scopes, " ")},
	}
	if p.State != "" {
		q.Set("state", p.State)
	}
	if p.ShowDialog {
		q.Set("show_dialog", "true")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// AuthCodeExchanger exchanges an authorisation code for tokens.
//
// This is the one-time interactive half of the OAuth flow, kept separate from
// RefreshingTokenSource: it runs from an operator's terminal, needs no token store, and
// produces the refresh token that everything afterwards depends on.
type AuthCodeExchanger struct {
	creds    Credentials
	redirect string
	retrier  *httpx.Retrier
	tokenURL string
}

// AuthCodeConfig configures an AuthCodeExchanger.
type AuthCodeConfig struct {
	Credentials Credentials
	RedirectURI string
	Doer        Doer
	Clock       Clock
	Retry       RetryPolicy
	TokenURL    string
	Logger      *slog.Logger
}

// Tokens is the result of a successful code exchange.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	Scope        string
	ExpiresIn    int
}

// HasScopes reports whether every scope in want was granted.
func (t Tokens) HasScopes(want ...string) (missing []string) {
	granted := make(map[string]struct{})
	for _, s := range strings.Fields(t.Scope) {
		granted[s] = struct{}{}
	}
	for _, w := range want {
		if _, ok := granted[w]; !ok {
			missing = append(missing, w)
		}
	}
	return missing
}

// NewAuthCodeExchanger validates cfg and returns an exchanger.
func NewAuthCodeExchanger(cfg AuthCodeConfig) (*AuthCodeExchanger, error) {
	if cfg.Credentials.ClientID == "" || cfg.Credentials.ClientSecret == "" {
		return nil, errors.New("spotify: client ID and secret are required")
	}
	if cfg.RedirectURI == "" {
		return nil, errors.New("spotify: redirect URI is required")
	}
	doer := cfg.Doer
	if doer == nil {
		doer = &http.Client{Timeout: 15 * time.Second}
	}
	clock := cfg.Clock
	if clock == nil {
		clock = SystemClock()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = DefaultTokenURL
	}
	return &AuthCodeExchanger{
		creds:    cfg.Credentials,
		redirect: cfg.RedirectURI,
		retrier:  httpx.NewRetrier(httpx.RetrierConfig{Doer: doer, Policy: cfg.Retry, Clock: clock, Log: log}),
		tokenURL: tokenURL,
	}, nil
}

// Exchange trades an authorisation code for an access and refresh token pair.
//
// The code is single-use and expires within about a minute, so this must run promptly after
// the callback.
func (e *AuthCodeExchanger) Exchange(ctx context.Context, code string) (Tokens, error) {
	if code == "" {
		return Tokens{}, errors.New("spotify: authorisation code is empty")
	}

	form := url.Values{
		"grant_type": {"authorization_code"},
		"code":       {code},
		// Validated by Spotify, not merely echoed: it must match the registered URI exactly.
		"redirect_uri": {e.redirect},
	}
	body := form.Encode()

	resp, err := e.retrier.Do(ctx, func() (*http.Request, error) {
		r, herr := http.NewRequestWithContext(ctx, http.MethodPost, e.tokenURL, strings.NewReader(body))
		if herr != nil {
			return nil, herr
		}
		r.Header.Set("Authorization", e.creds.basicAuth())
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r, nil
	})
	if err != nil {
		return Tokens{}, asAuthError(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, httpx.MaxErrorBodyBytes)).Decode(&tr); err != nil {
		return Tokens{}, fmt.Errorf("spotify: decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return Tokens{}, errors.New("spotify: token response contained no access_token")
	}
	if tr.RefreshToken == "" {
		// Without this the whole unattended pipeline is impossible, so fail loudly rather
		// than store nothing and appear to succeed.
		return Tokens{}, errors.New("spotify: token response contained no refresh_token; " +
			"check the authorisation used response_type=code")
	}

	return Tokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		Scope:        tr.Scope,
		ExpiresIn:    tr.ExpiresIn,
	}, nil
}
