package spotify

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neovasili/spotistats/internal/spotify/spotifytest"
)

const (
	testClientID     = "test-client-id"
	testClientSecret = "test-client-secret"
)

func tokenBody(access, refresh string, expiresIn int) string {
	if refresh == "" {
		return fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":%d,"scope":"user-read-recently-played"}`,
			access, expiresIn)
	}
	return fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":%d,"refresh_token":%q,"scope":"user-read-recently-played"}`,
		access, expiresIn, refresh)
}

func newTokenSource(t *testing.T, store RefreshTokenStore, clk Clock, steps ...spotifytest.Step) (*RefreshingTokenSource, *spotifytest.ScriptedDoer) {
	t.Helper()
	doer := spotifytest.NewScriptedDoer(steps...)
	p := DefaultRetryPolicy()
	p.Rand = spotifytest.FixedRand(1.0)
	ts, err := NewRefreshingTokenSource(TokenSourceConfig{
		Credentials: Credentials{ClientID: testClientID, ClientSecret: testClientSecret},
		Store:       store,
		Doer:        doer,
		Clock:       clk,
		Retry:       p,
		TokenURL:    "https://accounts.spotify.com/api/token",
	})
	if err != nil {
		t.Fatalf("NewRefreshingTokenSource: %v", err)
	}
	return ts, doer
}

func TestTokenSourceRefreshAndCache(t *testing.T) {
	store := spotifytest.NewMemoryRefreshTokenStore("refresh-0")
	clk := spotifytest.NewFakeClock(epoch)
	ts, doer := newTokenSource(t, store, clk,
		spotifytest.Step{Status: 200, Header: spotifytest.JSONHeader(), Body: tokenBody("access-1", "", 3600)},
	)

	got, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "access-1" {
		t.Errorf("token = %q, want access-1", got)
	}

	// A second call inside the validity window must not hit the network.
	if got, err = ts.Token(context.Background()); err != nil || got != "access-1" {
		t.Fatalf("cached Token = (%q, %v)", got, err)
	}
	if n := doer.Calls(); n != 1 {
		t.Errorf("HTTP calls = %d, want 1 -- the token should be cached", n)
	}

	// No rotation was offered, so the stored token is untouched.
	if store.PutCalls() != 0 {
		t.Errorf("Put calls = %d, want 0 when no new refresh token is issued", store.PutCalls())
	}
	if store.Value() != "refresh-0" {
		t.Errorf("stored refresh token = %q, want refresh-0", store.Value())
	}
}

func TestTokenSourceSendsCorrectRequest(t *testing.T) {
	store := spotifytest.NewMemoryRefreshTokenStore("refresh-0")
	clk := spotifytest.NewFakeClock(epoch)
	ts, doer := newTokenSource(t, store, clk,
		spotifytest.Step{Status: 200, Body: tokenBody("access-1", "", 3600)},
	)
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}

	reqs := doer.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", r.Method)
	}
	if got := r.URL.String(); got != "https://accounts.spotify.com/api/token" {
		t.Errorf("url = %s", got)
	}
	if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", got)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(testClientID+":"+testClientSecret))
	if got := r.Header.Get("Authorization"); got != wantAuth {
		t.Errorf("Authorization = %q, want HTTP Basic with the client credentials", got)
	}
	body := doer.RequestBodies()[0]
	if !strings.Contains(body, "grant_type=refresh_token") {
		t.Errorf("body %q lacks grant_type=refresh_token", body)
	}
	if !strings.Contains(body, "refresh_token=refresh-0") {
		t.Errorf("body %q lacks the stored refresh token", body)
	}
}

func TestTokenSourceSkewTriggersProactiveRefresh(t *testing.T) {
	store := spotifytest.NewMemoryRefreshTokenStore("refresh-0")
	clk := spotifytest.NewFakeClock(epoch)
	ts, doer := newTokenSource(t, store, clk,
		spotifytest.Step{Status: 200, Body: tokenBody("access-1", "", 3600)},
		spotifytest.Step{Status: 200, Body: tokenBody("access-2", "", 3600)},
	)

	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Move to inside the 60s skew window: still nominally valid, but due for refresh.
	clk.Advance(3600*time.Second - 30*time.Second)
	got, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "access-2" {
		t.Errorf("token = %q, want access-2 -- the skew window must force an early refresh", got)
	}
	if n := doer.Calls(); n != 2 {
		t.Errorf("HTTP calls = %d, want 2", n)
	}
}

// TestTokenSourceRotationPersisted: the happy path of the rotation flow.
func TestTokenSourceRotationPersisted(t *testing.T) {
	store := spotifytest.NewMemoryRefreshTokenStore("refresh-0")
	clk := spotifytest.NewFakeClock(epoch)
	ts, _ := newTokenSource(t, store, clk,
		spotifytest.Step{Status: 200, Body: tokenBody("access-1", "refresh-1", 3600)},
	)

	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.PutCalls() != 1 {
		t.Errorf("Put calls = %d, want exactly 1", store.PutCalls())
	}
	if store.Value() != "refresh-1" {
		t.Errorf("stored token = %q, want the rotated refresh-1", store.Value())
	}
	if _, pending := ts.PendingRotation(); pending {
		t.Error("PendingRotation reported work outstanding after a successful Put")
	}
}

// TestTokenSourceRotationPutFails is the dangerous path. Spotify has probably already
// invalidated the old token, so the replacement exists only in memory.
func TestTokenSourceRotationPutFails(t *testing.T) {
	store := spotifytest.NewMemoryRefreshTokenStore("refresh-0")
	store.PutErr = errors.New("ssm unavailable")
	clk := spotifytest.NewFakeClock(epoch)

	var rotErrs []error
	var mu sync.Mutex
	doer := spotifytest.NewScriptedDoer(
		spotifytest.Step{Status: 200, Body: tokenBody("access-1", "refresh-1", 3600)},
	)
	ts, err := NewRefreshingTokenSource(TokenSourceConfig{
		Credentials: Credentials{ClientID: testClientID, ClientSecret: testClientSecret},
		Store:       store,
		Doer:        doer,
		Clock:       clk,
		OnRotationError: func(_ context.Context, e error) {
			mu.Lock()
			defer mu.Unlock()
			rotErrs = append(rotErrs, e)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The access token must still be returned: failing here would guarantee the loss
	// rather than merely risk it.
	got, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token returned an error; a failed rotation must not fail the call: %v", err)
	}
	if got != "access-1" {
		t.Errorf("token = %q, want access-1", got)
	}

	mu.Lock()
	n := len(rotErrs)
	var first error
	if n > 0 {
		first = rotErrs[0]
	}
	mu.Unlock()

	if n != 1 {
		t.Fatalf("OnRotationError fired %d times, want 1", n)
	}
	if !errors.Is(first, ErrRotationPersistFailed) {
		t.Errorf("callback error = %v, want it to wrap ErrRotationPersistFailed", first)
	}

	pending, ok := ts.PendingRotation()
	if !ok {
		t.Fatal("PendingRotation reported nothing outstanding after a failed Put")
	}
	if pending != "refresh-1" {
		t.Errorf("PendingRotation = %q, want the rotated refresh-1", pending)
	}
}

// TestTokenSourcePendingRotationIsUsedOnNextRefresh: once rotated, the in-memory token is
// the only valid one, so a subsequent refresh must not fall back to the stale stored value.
func TestTokenSourcePendingRotationIsUsedOnNextRefresh(t *testing.T) {
	store := spotifytest.NewMemoryRefreshTokenStore("refresh-0")
	store.PutErr = errors.New("ssm unavailable")
	clk := spotifytest.NewFakeClock(epoch)
	ts, doer := newTokenSource(t, store, clk,
		spotifytest.Step{Status: 200, Body: tokenBody("access-1", "refresh-1", 3600)},
		spotifytest.Step{Status: 200, Body: tokenBody("access-2", "", 3600)},
	)

	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts.Invalidate()
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}

	bodies := doer.RequestBodies()
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	if !strings.Contains(bodies[1], "refresh_token=refresh-1") {
		t.Errorf("second request used %q; it must use the rotated token, not the stale stored one", bodies[1])
	}
}

func TestTokenSourceClearPendingRotation(t *testing.T) {
	store := spotifytest.NewMemoryRefreshTokenStore("refresh-0")
	store.PutErr = errors.New("boom")
	clk := spotifytest.NewFakeClock(epoch)
	ts, _ := newTokenSource(t, store, clk,
		spotifytest.Step{Status: 200, Body: tokenBody("access-1", "refresh-1", 3600)},
	)
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := ts.PendingRotation(); !ok {
		t.Fatal("expected a pending rotation")
	}
	ts.ClearPendingRotation()
	if _, ok := ts.PendingRotation(); ok {
		t.Error("ClearPendingRotation did not clear it")
	}
}

// TestTokenSourceInvalidGrantIsTerminal: retrying a revoked token is pointless and burns
// quota, so it must be exactly one attempt.
func TestTokenSourceInvalidGrantIsTerminal(t *testing.T) {
	store := spotifytest.NewMemoryRefreshTokenStore("refresh-0")
	clk := spotifytest.NewFakeClock(epoch)
	ts, doer := newTokenSource(t, store, clk,
		spotifytest.Step{
			Status: 400,
			Body:   `{"error":"invalid_grant","error_description":"Refresh token revoked"}`,
		},
	)

	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("Token succeeded, want failure")
	}
	if !errors.Is(err, ErrRefreshTokenInvalid) {
		t.Errorf("err = %v, want it to wrap ErrRefreshTokenInvalid", err)
	}
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %T, want *AuthError", err)
	}
	if ae.Code != "invalid_grant" {
		t.Errorf("Code = %q, want invalid_grant", ae.Code)
	}
	if ae.Description != "Refresh token revoked" {
		t.Errorf("Description = %q", ae.Description)
	}
	if !ae.Terminal() {
		t.Error("invalid_grant must be Terminal")
	}
	if n := doer.Calls(); n != 1 {
		t.Errorf("HTTP calls = %d, want exactly 1 -- a revoked token must not be retried", n)
	}
	if got := clk.TotalSlept(); got != 0 {
		t.Errorf("slept %v, want none", got)
	}
}

func TestTokenSourceInvalidClientIsTerminal(t *testing.T) {
	store := spotifytest.NewMemoryRefreshTokenStore("refresh-0")
	ts, doer := newTokenSource(t, store, spotifytest.NewFakeClock(epoch),
		spotifytest.Step{Status: 400, Body: `{"error":"invalid_client","error_description":"Invalid client"}`},
	)
	_, err := ts.Token(context.Background())
	if !errors.Is(err, ErrRefreshTokenInvalid) {
		t.Errorf("err = %v, want ErrRefreshTokenInvalid", err)
	}
	if n := doer.Calls(); n != 1 {
		t.Errorf("HTTP calls = %d, want 1", n)
	}
}

// A 5xx from the accounts endpoint is transient and must be retried.
func TestTokenSourceServerErrorIsRetried(t *testing.T) {
	store := spotifytest.NewMemoryRefreshTokenStore("refresh-0")
	clk := spotifytest.NewFakeClock(epoch)
	ts, doer := newTokenSource(t, store, clk,
		spotifytest.Step{Status: 503},
		spotifytest.Step{Status: 503},
		spotifytest.Step{Status: 200, Body: tokenBody("access-1", "", 3600)},
	)

	got, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "access-1" {
		t.Errorf("token = %q", got)
	}
	if n := doer.Calls(); n != 3 {
		t.Errorf("HTTP calls = %d, want 3", n)
	}
	// Each retry rebuilds the form body -- the reason do() takes a factory.
	for i, b := range doer.RequestBodies() {
		if !strings.Contains(b, "refresh_token=refresh-0") {
			t.Errorf("attempt %d body = %q, want the form payload replayed", i+1, b)
		}
	}
}

func TestTokenSourceStoreReadFailure(t *testing.T) {
	store := spotifytest.NewMemoryRefreshTokenStore("refresh-0")
	store.GetErr = errors.New("ssm denied")
	ts, doer := newTokenSource(t, store, spotifytest.NewFakeClock(epoch))

	if _, err := ts.Token(context.Background()); err == nil {
		t.Fatal("Token succeeded despite an unreadable store")
	}
	if n := doer.Calls(); n != 0 {
		t.Errorf("HTTP calls = %d, want 0", n)
	}
}

func TestTokenSourceEmptyStoredToken(t *testing.T) {
	ts, doer := newTokenSource(t, spotifytest.NewMemoryRefreshTokenStore(""), spotifytest.NewFakeClock(epoch))
	_, err := ts.Token(context.Background())
	if !errors.Is(err, ErrRefreshTokenInvalid) {
		t.Errorf("err = %v, want ErrRefreshTokenInvalid", err)
	}
	if n := doer.Calls(); n != 0 {
		t.Errorf("HTTP calls = %d, want 0", n)
	}
}

func TestTokenSourceMissingAccessToken(t *testing.T) {
	ts, _ := newTokenSource(t, spotifytest.NewMemoryRefreshTokenStore("refresh-0"),
		spotifytest.NewFakeClock(epoch),
		spotifytest.Step{Status: 200, Body: `{"token_type":"Bearer","expires_in":3600}`},
	)
	if _, err := ts.Token(context.Background()); err == nil {
		t.Error("Token accepted a response with no access_token")
	}
}

func TestTokenSourceMissingExpiresInGetsDefaultTTL(t *testing.T) {
	clk := spotifytest.NewFakeClock(epoch)
	ts, doer := newTokenSource(t, spotifytest.NewMemoryRefreshTokenStore("refresh-0"), clk,
		spotifytest.Step{Status: 200, Body: `{"access_token":"a1","token_type":"Bearer"}`},
		spotifytest.Step{Status: 200, Body: tokenBody("a2", "", 3600)},
	)
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Well inside the default one-hour TTL: must still be cached, not treated as eternal
	// nor as already expired.
	clk.Advance(30 * time.Minute)
	if got, _ := ts.Token(context.Background()); got != "a1" {
		t.Errorf("token = %q, want the cached a1", got)
	}
	if n := doer.Calls(); n != 1 {
		t.Errorf("HTTP calls = %d, want 1", n)
	}
}

// TestTokenSourceConcurrentRefreshCollapses: batch enrichment issues parallel calls, and
// without serialisation each would trigger its own refresh.
func TestTokenSourceConcurrentRefreshCollapses(t *testing.T) {
	store := spotifytest.NewMemoryRefreshTokenStore("refresh-0")
	clk := spotifytest.NewFakeClock(epoch)
	ts, doer := newTokenSource(t, store, clk,
		spotifytest.Step{Status: 200, Body: tokenBody("access-1", "", 3600)},
	)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	toks := make([]string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			toks[i], errs[i] = ts.Token(context.Background())
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if toks[i] != "access-1" {
			t.Errorf("goroutine %d token = %q", i, toks[i])
		}
	}
	if got := doer.Calls(); got != 1 {
		t.Errorf("HTTP calls = %d, want exactly 1 for %d concurrent callers", got, n)
	}
}

func TestNewRefreshingTokenSourceValidation(t *testing.T) {
	store := spotifytest.NewMemoryRefreshTokenStore("r")
	tests := []struct {
		name string
		cfg  TokenSourceConfig
	}{
		{"no client id", TokenSourceConfig{Credentials: Credentials{ClientSecret: "s"}, Store: store}},
		{"no client secret", TokenSourceConfig{Credentials: Credentials{ClientID: "c"}, Store: store}},
		{"no store", TokenSourceConfig{Credentials: Credentials{ClientID: "c", ClientSecret: "s"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRefreshingTokenSource(tc.cfg); err == nil {
				t.Error("expected a validation error")
			}
		})
	}
	// Defaults are filled in for everything else.
	ts, err := NewRefreshingTokenSource(TokenSourceConfig{
		Credentials: Credentials{ClientID: "c", ClientSecret: "s"},
		Store:       store,
	})
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if ts.tokenURL != DefaultTokenURL {
		t.Errorf("tokenURL = %q, want the default", ts.tokenURL)
	}
	if ts.skew != defaultTokenSkew {
		t.Errorf("skew = %v, want %v", ts.skew, defaultTokenSkew)
	}
}

func TestAuthErrorTerminalCodes(t *testing.T) {
	for _, tc := range []struct {
		code string
		want bool
	}{
		{"invalid_grant", true},
		{"invalid_client", true},
		{"invalid_request", true},
		{"unsupported_grant_type", true},
		{"server_error", false},
		{"temporarily_unavailable", false},
		{"", false},
	} {
		e := &AuthError{StatusCode: 400, Code: tc.code}
		if got := e.Terminal(); got != tc.want {
			t.Errorf("AuthError{Code:%q}.Terminal() = %v, want %v", tc.code, got, tc.want)
		}
		if tc.want && !errors.Is(e, ErrRefreshTokenInvalid) {
			t.Errorf("terminal AuthError{Code:%q} must unwrap to ErrRefreshTokenInvalid", tc.code)
		}
		if !tc.want && errors.Is(e, ErrRefreshTokenInvalid) {
			t.Errorf("non-terminal AuthError{Code:%q} must not unwrap to ErrRefreshTokenInvalid", tc.code)
		}
	}
}
