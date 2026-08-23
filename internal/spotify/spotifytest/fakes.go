// Package spotifytest provides deterministic fakes for testing the Spotify client.
//
// It deliberately does NOT import internal/spotify. Go interfaces are structural, so
// these types satisfy spotify.Clock, spotify.Doer, spotify.RefreshTokenStore and
// spotify.TokenSource without naming them -- which keeps `package spotify` internal
// tests free of an import cycle.
package spotifytest

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/neovasili/spotistats/internal/httpx/httpxtest"
)

// ---------------------------------------------------------------------------
// FakeClock
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ScriptedDoer
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Token helpers
// ---------------------------------------------------------------------------

// MemoryRefreshTokenStore is an in-memory RefreshTokenStore. GetErr and PutErr inject
// failures; PutErr in particular exercises the rotation-persistence path, which is the
// one failure mode in the token flow that needs human attention.
type MemoryRefreshTokenStore struct {
	mu       sync.Mutex
	value    string
	getCalls int
	putCalls int

	GetErr error
	PutErr error
}

func NewMemoryRefreshTokenStore(initial string) *MemoryRefreshTokenStore {
	return &MemoryRefreshTokenStore{value: initial}
}

func (s *MemoryRefreshTokenStore) Get(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCalls++
	if s.GetErr != nil {
		return "", s.GetErr
	}
	return s.value, nil
}

func (s *MemoryRefreshTokenStore) Put(_ context.Context, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putCalls++
	if s.PutErr != nil {
		return s.PutErr
	}
	s.value = v
	return nil
}

func (s *MemoryRefreshTokenStore) Value() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value
}

func (s *MemoryRefreshTokenStore) GetCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCalls
}

func (s *MemoryRefreshTokenStore) PutCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putCalls
}

// StaticTokenSource is a TokenSource that always returns the same access token.
type StaticTokenSource string

func (s StaticTokenSource) Token(context.Context) (string, error) { return string(s), nil }

// ---------------------------------------------------------------------------
// Determinism helpers
// ---------------------------------------------------------------------------

// JSONHeader is the header set most fixtures need.
func JSONHeader() http.Header {
	return http.Header{"Content-Type": []string{"application/json"}}
}

// The generic transport fakes moved to internal/httpx/httpxtest when the retry, backoff and
// rate-limiting code was extracted: none of them is Spotify-specific, and three packages now
// need them. Keeping them here also made httpx's own tests import spotify, which imports
// httpx — an import cycle.
//
// These aliases mean no existing test had to change in the same commit as the extraction.
type (
	// FakeClock is a controllable Clock that records every sleep without performing one.
	FakeClock = httpxtest.FakeClock
	// Step is one scripted HTTP response.
	Step = httpxtest.Step
	// ScriptedDoer replays a fixed sequence of responses.
	ScriptedDoer = httpxtest.ScriptedDoer
)

// ErrScriptExhausted is returned when more requests are made than were scripted.
var ErrScriptExhausted = httpxtest.ErrScriptExhausted

// NewFakeClock returns a clock starting at the given instant.
func NewFakeClock(start time.Time) *FakeClock { return httpxtest.NewFakeClock(start) }

// NewScriptedDoer returns a Doer that replays steps in order.
func NewScriptedDoer(steps ...Step) *ScriptedDoer { return httpxtest.NewScriptedDoer(steps...) }

// FixedRand returns a jitter source that always yields v.
func FixedRand(v float64) func() float64 { return httpxtest.FixedRand(v) }

// RetryAfterHeader builds a Retry-After header with a delta-seconds value.
func RetryAfterHeader(seconds int) http.Header { return httpxtest.RetryAfterHeader(seconds) }
