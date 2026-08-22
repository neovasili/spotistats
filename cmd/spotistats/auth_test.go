package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestCallbackHandlerRejectsStateMismatch: a mismatched state means the callback did not
// originate from this authorisation request, so the code must be discarded.
func TestCallbackHandlerRejectsStateMismatch(t *testing.T) {
	out := make(chan callbackResult, 1)
	h := callbackHandler("/callback", "expected-state", out)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/callback?code=abc&state=attacker-state", nil))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	res := <-out
	if res.err == nil {
		t.Fatal("state mismatch was accepted")
	}
	if res.code != "" {
		t.Errorf("code %q leaked despite the state mismatch", res.code)
	}
}

func TestCallbackHandlerAcceptsValidCallback(t *testing.T) {
	out := make(chan callbackResult, 1)
	h := callbackHandler("/callback", "s1", out)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/callback?code=the-code&state=s1", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	res := <-out
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if res.code != "the-code" {
		t.Errorf("code = %q, want the-code", res.code)
	}
	// The browser tab should say something useful.
	if body := rec.Body.String(); body == "" || !contains(body, "Authorised") {
		t.Errorf("callback page body = %q", body)
	}
}

// Pressing Cancel on the consent screen produces error=access_denied.
func TestCallbackHandlerSurfacesSpotifyError(t *testing.T) {
	out := make(chan callbackResult, 1)
	h := callbackHandler("/callback", "s1", out)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/callback?error=access_denied&state=s1", nil))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	res := <-out
	if res.err == nil {
		t.Fatal("access_denied was not surfaced as an error")
	}
	if !contains(res.err.Error(), "access_denied") {
		t.Errorf("error = %v, want it to name access_denied", res.err)
	}
}

func TestCallbackHandlerRejectsMissingCode(t *testing.T) {
	out := make(chan callbackResult, 1)
	h := callbackHandler("/callback", "s1", out)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/callback?state=s1", nil))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if res := <-out; res.err == nil {
		t.Fatal("a callback with no code was accepted")
	}
}

func TestRandomStateIsUnpredictable(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		s, err := randomState()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != 32 {
			t.Fatalf("state %q has length %d, want 32 hex chars", s, len(s))
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("state %q repeated", s)
		}
		seen[s] = struct{}{}
	}
}

func TestPortOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://127.0.0.1:8888/callback", "8888"},
		{"http://127.0.0.1/callback", "80"},
		{"https://example.com/callback", "443"},
		{"https://example.com:9443/callback", "9443"},
	} {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := portOf(u); got != tc.want {
			t.Errorf("portOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
