package spotify

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/neovasili/spotistats/internal/spotify/spotifytest"
)

func TestAuthorizeURL(t *testing.T) {
	got, err := AuthorizeURL(AuthorizeURLParams{
		ClientID:    "abc123",
		RedirectURI: "http://127.0.0.1:8888/callback",
		State:       "deadbeef",
		ShowDialog:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "accounts.spotify.com" || u.Path != "/authorize" {
		t.Errorf("endpoint = %s%s", u.Host, u.Path)
	}
	q := u.Query()
	if q.Get("client_id") != "abc123" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want code (the flow needs a refresh token)", q.Get("response_type"))
	}
	// Must round-trip byte for byte: Spotify validates it again on the token exchange.
	if q.Get("redirect_uri") != "http://127.0.0.1:8888/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("state") != "deadbeef" {
		t.Errorf("state = %q", q.Get("state"))
	}
	if q.Get("show_dialog") != "true" {
		t.Errorf("show_dialog = %q", q.Get("show_dialog"))
	}

	// Exactly the two scopes Spotistats needs, and no more.
	scopes := strings.Fields(q.Get("scope"))
	if diff := cmp.Diff([]string{"user-read-recently-played", "user-top-read"}, scopes); diff != "" {
		t.Errorf("scopes (-want +got):\n%s", diff)
	}
}

func TestAuthorizeURLValidation(t *testing.T) {
	if _, err := AuthorizeURL(AuthorizeURLParams{RedirectURI: "http://127.0.0.1/x"}); err == nil {
		t.Error("accepted an empty client ID")
	}
	if _, err := AuthorizeURL(AuthorizeURLParams{ClientID: "x"}); err == nil {
		t.Error("accepted an empty redirect URI")
	}
}

func TestRequiredScopes(t *testing.T) {
	// Requesting more than these enlarges the consent screen and the blast radius of a
	// leaked token for no benefit.
	if diff := cmp.Diff([]string{"user-read-recently-played", "user-top-read"}, RequiredScopes()); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func newExchanger(t *testing.T, steps ...spotifytest.Step) (*AuthCodeExchanger, *spotifytest.ScriptedDoer) {
	t.Helper()
	doer := spotifytest.NewScriptedDoer(steps...)
	p := DefaultRetryPolicy()
	p.Rand = spotifytest.FixedRand(1.0)
	e, err := NewAuthCodeExchanger(AuthCodeConfig{
		Credentials: Credentials{ClientID: "cid", ClientSecret: "csec"},
		RedirectURI: "http://127.0.0.1:8888/callback",
		Doer:        doer,
		Clock:       spotifytest.NewFakeClock(epoch),
		Retry:       p,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, doer
}

func TestExchangeSuccess(t *testing.T) {
	e, doer := newExchanger(t, spotifytest.Step{
		Status: 200,
		Body: `{"access_token":"acc","token_type":"Bearer","expires_in":3600,` +
			`"refresh_token":"ref","scope":"user-read-recently-played user-top-read"}`,
	})

	tok, err := e.Exchange(context.Background(), "the-code")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "acc" || tok.RefreshToken != "ref" {
		t.Errorf("tokens = %+v", tok)
	}
	if missing := tok.HasScopes(RequiredScopes()...); len(missing) != 0 {
		t.Errorf("missing scopes = %v, want none", missing)
	}

	body := doer.RequestBodies()[0]
	for _, want := range []string{
		"grant_type=authorization_code",
		"code=the-code",
		// Spotify validates this, it does not merely echo it.
		"redirect_uri=http%3A%2F%2F127.0.0.1%3A8888%2Fcallback",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q lacks %q", body, want)
		}
	}
}

// A response with no refresh token makes the whole unattended pipeline impossible, so it
// must fail loudly rather than appear to succeed.
func TestExchangeRequiresRefreshToken(t *testing.T) {
	e, _ := newExchanger(t, spotifytest.Step{
		Status: 200,
		Body:   `{"access_token":"acc","token_type":"Bearer","expires_in":3600}`,
	})
	_, err := e.Exchange(context.Background(), "code")
	if err == nil {
		t.Fatal("accepted a response with no refresh_token")
	}
	if !strings.Contains(err.Error(), "refresh_token") {
		t.Errorf("error = %v, want it to name the missing field", err)
	}
}

func TestExchangeRejectsEmptyCode(t *testing.T) {
	e, doer := newExchanger(t)
	if _, err := e.Exchange(context.Background(), ""); err == nil {
		t.Error("accepted an empty code")
	}
	if doer.Calls() != 0 {
		t.Error("an empty code must be rejected before any HTTP call")
	}
}

// An expired or reused code returns invalid_grant, which is terminal.
func TestExchangeInvalidGrantIsTerminal(t *testing.T) {
	e, doer := newExchanger(t, spotifytest.Step{
		Status: 400,
		Body:   `{"error":"invalid_grant","error_description":"Invalid authorization code"}`,
	})
	_, err := e.Exchange(context.Background(), "stale-code")
	if !errors.Is(err, ErrRefreshTokenInvalid) {
		t.Errorf("err = %v, want it to wrap ErrRefreshTokenInvalid", err)
	}
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != "invalid_grant" {
		t.Errorf("err = %v, want *AuthError with invalid_grant", err)
	}
	if doer.Calls() != 1 {
		t.Errorf("HTTP calls = %d, want exactly 1 -- a dead code must not be retried", doer.Calls())
	}
}

func TestExchangeRetriesServerErrors(t *testing.T) {
	e, doer := newExchanger(t,
		spotifytest.Step{Status: 503},
		spotifytest.Step{Status: 200, Body: `{"access_token":"a","refresh_token":"r","expires_in":3600}`},
	)
	if _, err := e.Exchange(context.Background(), "code"); err != nil {
		t.Fatal(err)
	}
	if doer.Calls() != 2 {
		t.Errorf("HTTP calls = %d, want 2", doer.Calls())
	}
}

func TestTokensHasScopes(t *testing.T) {
	tok := Tokens{Scope: "user-read-recently-played"}
	missing := tok.HasScopes("user-read-recently-played", "user-top-read")
	if diff := cmp.Diff([]string{"user-top-read"}, missing); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if got := (Tokens{Scope: ""}).HasScopes("a"); len(got) != 1 {
		t.Errorf("empty scope reported %v", got)
	}
}

func TestNewAuthCodeExchangerValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  AuthCodeConfig
	}{
		{"no client id", AuthCodeConfig{Credentials: Credentials{ClientSecret: "s"}, RedirectURI: "u"}},
		{"no secret", AuthCodeConfig{Credentials: Credentials{ClientID: "c"}, RedirectURI: "u"}},
		{"no redirect", AuthCodeConfig{Credentials: Credentials{ClientID: "c", ClientSecret: "s"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewAuthCodeExchanger(tc.cfg); err == nil {
				t.Error("expected a validation error")
			}
		})
	}
}
