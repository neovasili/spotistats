package httpx

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// retryAfterPad is added to a server-supplied Retry-After. Sleeping for exactly the
// advertised duration tends to land on the boundary of Spotify's rolling window and get
// throttled again; a small pad avoids a second round trip.
const retryAfterPad = time.Second

// MaxErrorBodyBytes caps how much of an error body is read before it is discarded, so a
// pathological response cannot exhaust memory while still leaving enough to diagnose with.
const MaxErrorBodyBytes = 8 << 10

// RetryPolicy configures the retry loop. The zero value is usable: every field falls
// back to the DefaultRetryPolicy value.
type RetryPolicy struct {
	// MaxAttempts is the total number of HTTP attempts, not the number of retries.
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration

	// MaxRetryAfter is the longest server-requested wait that will actually be slept.
	// Beyond it the call fails with *RateLimitError instead of blocking.
	//
	// This matters operationally: a badly exceeded quota can produce a multi-minute
	// Retry-After, and a capture Lambda with a 120s timeout that blocks on it dies at
	// the runtime deadline having logged nothing. Failing fast lets the run end cleanly;
	// the next scheduled run re-reads the same window, which is harmless because
	// ingestion is idempotent.
	MaxRetryAfter time.Duration

	// Rand returns a value in [0,1) for jitter. Nil uses the global generator. Tests
	// inject a fixed value to pin the jitter bounds exactly.
	Rand func() float64
}

// DefaultRetryPolicy is one attempt plus four retries, with capped exponential backoff.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:   5,
		BaseDelay:     250 * time.Millisecond,
		MaxDelay:      20 * time.Second,
		MaxRetryAfter: 60 * time.Second,
	}
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	d := DefaultRetryPolicy()
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = d.MaxAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = d.BaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = d.MaxDelay
	}
	if p.MaxRetryAfter <= 0 {
		p.MaxRetryAfter = d.MaxRetryAfter
	}
	if p.Rand == nil {
		p.Rand = rand.Float64
	}
	return p
}

// backoff returns the delay before the retry that follows the given zero-based attempt.
//
// It uses EQUAL jitter -- the delay lands in [50%, 100%] of the exponential target --
// rather than full jitter over [0, target]. The minimum delay therefore still grows
// monotonically with the attempt number, which keeps the schedule assertable with bounds
// in tests while retaining enough randomness to break up synchronised retries.
func (p RetryPolicy) backoff(attempt int) time.Duration {
	p = p.withDefaults()
	target := p.BaseDelay
	for i := 0; i < attempt && target < p.MaxDelay; i++ {
		target *= 2
	}
	if target > p.MaxDelay {
		target = p.MaxDelay
	}
	half := target / 2
	return half + time.Duration(p.Rand()*float64(half))
}

// parseRetryAfter handles both RFC 7231 forms of the header: delta-seconds, and an
// absolute HTTP-date. Returns false when the header is absent or unparseable.
func parseRetryAfter(h string, now time.Time) (time.Duration, bool) {
	if h == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(h); err == nil {
		d := when.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

type Retrier struct {
	doer    Doer
	policy  RetryPolicy
	clock   Clock
	limiter Limiter
	log     *slog.Logger
}

// RetrierConfig configures a Retrier. Only Doer is required.
type RetrierConfig struct {
	Doer   Doer
	Policy RetryPolicy
	Clock  Clock
	// Limiter is optional. Nil means unthrottled, which is right for a job making a handful
	// of calls and wrong for one making thousands -- see NewWindowLimiter.
	Limiter Limiter
	Log     *slog.Logger
}

// NewRetrier builds a Retrier.
func NewRetrier(cfg RetrierConfig) *Retrier {
	clock := cfg.Clock
	if clock == nil {
		clock = SystemClock()
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Retrier{
		doer: cfg.Doer, policy: cfg.Policy, clock: clock,
		limiter: cfg.Limiter, log: log,
	}
}

// do issues the request, retrying per the policy, and returns the first response whose
// status is 2xx. Any other outcome is returned as an error: *RateLimitError for a 429
// that could not be waited out, *APIError for other non-2xx statuses, or the transport
// error.
//
// It takes a request FACTORY rather than a *http.Request. That sidesteps rewinding the
// body on retry -- which matters for the token endpoint's form POST -- and gives every
// attempt a freshly derived request.
//
// On a 2xx the caller owns the response body. On every other path the body is drained
// and closed here; leaking it would exhaust the connection pool.
func (r *Retrier) Do(ctx context.Context, newReq func() (*http.Request, error)) (*http.Response, error) {
	policy := r.policy.withDefaults()
	clock := r.clock
	if clock == nil {
		clock = SystemClock()
	}
	log := r.log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	var lastErr error

	for attempt := 0; attempt < policy.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		if r.limiter != nil {
			if err := r.limiter.Wait(ctx); err != nil {
				if lastErr != nil {
					return nil, lastErr
				}
				return nil, err
			}
		}

		req, err := newReq()
		if err != nil {
			return nil, err
		}

		resp, err := r.doer.Do(req)
		isLast := attempt == policy.MaxAttempts-1

		// Transport failure: retryable.
		if err != nil {
			lastErr = err
			if isLast {
				return nil, err
			}
			d := policy.backoff(attempt)
			log.DebugContext(ctx, "spotify: transport error, retrying",
				"attempt", attempt+1, "of", policy.MaxAttempts, "delay", d, "err", err)
			if serr := r.delay(ctx, clock, d); serr != nil {
				return nil, err
			}
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		apiErr := apiErrorFrom(req, resp)

		if resp.StatusCode == http.StatusTooManyRequests {
			wait, ok := parseRetryAfter(resp.Header.Get("Retry-After"), clock.Now())
			if ok {
				wait += retryAfterPad
				// Fail fast rather than block past the caller's own deadline.
				if wait > policy.MaxRetryAfter {
					log.WarnContext(ctx, "spotify: rate limited beyond the policy ceiling, giving up",
						"retryAfter", wait, "ceiling", policy.MaxRetryAfter)
					return nil, &RateLimitError{APIError: *apiErr, RetryAfter: wait}
				}
				if isLast {
					return nil, &RateLimitError{APIError: *apiErr, RetryAfter: wait}
				}
				log.InfoContext(ctx, "spotify: rate limited, honouring Retry-After",
					"attempt", attempt+1, "delay", wait)
				if serr := r.delay(ctx, clock, wait); serr != nil {
					return nil, &RateLimitError{APIError: *apiErr, RetryAfter: wait}
				}
				lastErr = apiErr
				continue
			}
			// Missing or unparseable header: treat it like any other retryable status.
		}

		if !apiErr.Retryable() {
			return nil, apiErr
		}

		lastErr = apiErr
		if isLast {
			return nil, apiErr
		}
		d := policy.backoff(attempt)
		log.DebugContext(ctx, "spotify: retryable status, retrying",
			"status", resp.StatusCode, "attempt", attempt+1, "delay", d)
		if serr := r.delay(ctx, clock, d); serr != nil {
			return nil, apiErr
		}
	}

	return nil, lastErr
}

// delay sleeps for d, but refuses to sleep past the context deadline: blocking until the
// deadline only to return a context error wastes the remaining budget and loses the
// underlying cause.
func (r *Retrier) delay(ctx context.Context, clock Clock, d time.Duration) error {
	if dl, ok := ctx.Deadline(); ok && clock.Now().Add(d).After(dl) {
		return context.DeadlineExceeded
	}
	return clock.Sleep(ctx, d)
}

// apiErrorFrom builds an *APIError from a non-2xx response, consuming and closing the
// body. Spotify's Web API error shape is {"error":{"status":N,"message":"..."}}.
func apiErrorFrom(req *http.Request, resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	e := &APIError{
		StatusCode: resp.StatusCode,
		Status:     http.StatusText(resp.StatusCode),
		RequestID:  firstHeader(resp.Header, "X-Request-Id", "Cf-Ray"),
		Body:       body,
	}
	if req != nil {
		e.Method = req.Method
		if req.URL != nil {
			e.Path = req.URL.Path
		}
	}

	var wire struct {
		Error struct {
			Status  int    `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error.Message != "" {
		e.Message = wire.Error.Message
	}
	return e
}

func firstHeader(h http.Header, names ...string) string {
	for _, n := range names {
		if v := h.Get(n); v != "" {
			return v
		}
	}
	return ""
}
