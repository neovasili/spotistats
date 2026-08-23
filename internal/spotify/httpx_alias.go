package spotify

import (
	"time"

	"github.com/neovasili/spotistats/internal/httpx"
)

// The HTTP transport this package used to own now lives in internal/httpx, because
// internal/musicbrainz and internal/theaudiodb need exactly the same retry, backoff and
// rate-limiting behaviour and none of it was ever Spotify-specific.
//
// These aliases exist so the extraction did not have to touch every call site in
// internal/ingest, internal/config and cmd/ in the same change. They are not deprecated
// shims to be cleaned up later: `spotify.Clock` reads better at a Spotify call site than
// `httpx.Clock` does, and an alias costs nothing.
type (
	// Clock is the injectable time source. spotifytest.FakeClock satisfies it.
	Clock = httpx.Clock
	// Doer is the minimal HTTP client interface; *http.Client satisfies it.
	Doer = httpx.Doer
	// Limiter throttles outbound requests.
	Limiter = httpx.Limiter
	// RetryPolicy configures backoff, jitter and the retry ceiling.
	RetryPolicy = httpx.RetryPolicy
	// APIError is a non-2xx response.
	APIError = httpx.APIError
	// RateLimitError is a 429 whose Retry-After exceeded the policy's ceiling.
	RateLimitError = httpx.RateLimitError
)

// SystemClock returns the real clock.
func SystemClock() Clock { return httpx.SystemClock() }

// DefaultRetryPolicy is the policy every Spotify client uses unless overridden.
func DefaultRetryPolicy() RetryPolicy { return httpx.DefaultRetryPolicy() }

// NewWindowLimiter permits at most n requests per rolling window.
func NewWindowLimiter(n int, window time.Duration, clk Clock) Limiter {
	return httpx.NewWindowLimiter(n, window, clk)
}
