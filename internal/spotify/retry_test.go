package spotify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neovasili/spotistats/internal/spotify/spotifytest"
)

var epoch = time.Date(2025, 3, 14, 21, 0, 0, 0, time.UTC)

func newTestRetrier(d Doer, clk Clock, p RetryPolicy) *retrier {
	return &retrier{doer: d, policy: p, clock: clk}
}

// testPolicy pins jitter to its upper bound so backoff delays are exact integers.
func testPolicy() RetryPolicy {
	p := DefaultRetryPolicy()
	p.Rand = spotifytest.FixedRand(1.0)
	return p
}

func req(t *testing.T, method, url string) func() (*http.Request, error) {
	t.Helper()
	return func() (*http.Request, error) {
		return http.NewRequest(method, url, nil)
	}
}

// TestRetrierDecisionTable is the executable form of the retry contract.
func TestRetrierDecisionTable(t *testing.T) {
	transportErr := errors.New("connection reset")

	tests := []struct {
		name      string
		steps     []spotifytest.Step
		wantCalls int
		wantSlept int  // number of sleeps
		wantOK    bool // 2xx returned
		check     func(t *testing.T, err error)
	}{
		{
			name:      "200 returns immediately",
			steps:     []spotifytest.Step{{Status: 200, Body: `{}`}},
			wantCalls: 1, wantSlept: 0, wantOK: true,
		},
		{
			name:      "204 is a success",
			steps:     []spotifytest.Step{{Status: 204}},
			wantCalls: 1, wantSlept: 0, wantOK: true,
		},
		{
			name: "500 is retried then succeeds",
			steps: []spotifytest.Step{
				{Status: 500, Body: `{"error":{"status":500,"message":"oops"}}`},
				{Status: 200, Body: `{}`},
			},
			wantCalls: 2, wantSlept: 1, wantOK: true,
		},
		{
			name: "502, 503 and 504 are all retryable",
			steps: []spotifytest.Step{
				{Status: 502}, {Status: 503}, {Status: 504}, {Status: 200, Body: `{}`},
			},
			wantCalls: 4, wantSlept: 3, wantOK: true,
		},
		{
			name: "exhausting attempts on 500 returns the APIError",
			steps: []spotifytest.Step{
				{Status: 500}, {Status: 500}, {Status: 500}, {Status: 500}, {Status: 500},
			},
			// MaxAttempts is 5, and the last attempt does not sleep.
			wantCalls: 5, wantSlept: 4,
			check: func(t *testing.T, err error) {
				var e *APIError
				if !errors.As(err, &e) {
					t.Fatalf("err = %T (%v), want *APIError", err, err)
				}
				if e.StatusCode != 500 {
					t.Errorf("StatusCode = %d, want 500", e.StatusCode)
				}
			},
		},
		{
			name:      "401 is not retried; the auth layer owns it",
			steps:     []spotifytest.Step{{Status: 401, Body: `{"error":{"status":401,"message":"expired"}}`}},
			wantCalls: 1, wantSlept: 0,
			check: func(t *testing.T, err error) {
				var e *APIError
				if !errors.As(err, &e) {
					t.Fatalf("err = %T, want *APIError", err)
				}
				if e.StatusCode != 401 {
					t.Errorf("StatusCode = %d, want 401", e.StatusCode)
				}
				if e.Retryable() {
					t.Error("401 must not report Retryable")
				}
				if e.Message != "expired" {
					t.Errorf("Message = %q, want the parsed Spotify message", e.Message)
				}
			},
		},
		{
			name:      "403 is terminal",
			steps:     []spotifytest.Step{{Status: 403}},
			wantCalls: 1, wantSlept: 0,
			check: func(t *testing.T, err error) {
				var e *APIError
				if !errors.As(err, &e) || e.StatusCode != 403 {
					t.Fatalf("err = %v, want *APIError with 403", err)
				}
			},
		},
		{
			name:      "404 is terminal",
			steps:     []spotifytest.Step{{Status: 404}},
			wantCalls: 1, wantSlept: 0,
			check: func(t *testing.T, err error) {
				var e *APIError
				if !errors.As(err, &e) || e.StatusCode != 404 {
					t.Fatalf("err = %v, want *APIError with 404", err)
				}
			},
		},
		{
			name: "429 with Retry-After is honoured then succeeds",
			steps: []spotifytest.Step{
				{Status: 429, Header: spotifytest.RetryAfterHeader(2)},
				{Status: 200, Body: `{}`},
			},
			wantCalls: 2, wantSlept: 1, wantOK: true,
		},
		{
			name: "429 without Retry-After falls back to backoff",
			steps: []spotifytest.Step{
				{Status: 429},
				{Status: 200, Body: `{}`},
			},
			wantCalls: 2, wantSlept: 1, wantOK: true,
		},
		{
			name: "429 with an unparseable Retry-After falls back to backoff",
			steps: []spotifytest.Step{
				{Status: 429, Header: http.Header{"Retry-After": []string{"soon"}}},
				{Status: 200, Body: `{}`},
			},
			wantCalls: 2, wantSlept: 1, wantOK: true,
		},
		{
			name: "transport error is retried",
			steps: []spotifytest.Step{
				{Err: transportErr},
				{Status: 200, Body: `{}`},
			},
			wantCalls: 2, wantSlept: 1, wantOK: true,
		},
		{
			name: "exhausting attempts on transport errors returns the last one",
			steps: []spotifytest.Step{
				{Err: transportErr}, {Err: transportErr}, {Err: transportErr},
				{Err: transportErr}, {Err: transportErr},
			},
			wantCalls: 5, wantSlept: 4,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, transportErr) {
					t.Errorf("err = %v, want the transport error", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doer := spotifytest.NewScriptedDoer(tc.steps...)
			clk := spotifytest.NewFakeClock(epoch)
			r := newTestRetrier(doer, clk, testPolicy())

			resp, err := r.do(context.Background(), req(t, http.MethodGet, "https://api.spotify.com/v1/me"))
			if resp != nil {
				_ = resp.Body.Close()
			}

			if tc.wantOK {
				if err != nil {
					t.Fatalf("do() error = %v, want success", err)
				}
				if resp == nil {
					t.Fatal("do() returned a nil response with no error")
				}
			} else {
				if err == nil {
					t.Fatal("do() succeeded, want an error")
				}
				if tc.check != nil {
					tc.check(t, err)
				}
			}

			if got := doer.Calls(); got != tc.wantCalls {
				t.Errorf("HTTP calls = %d, want %d", got, tc.wantCalls)
			}
			if got := len(clk.Slept()); got != tc.wantSlept {
				t.Errorf("sleeps = %d (%v), want %d", got, clk.Slept(), tc.wantSlept)
			}
		})
	}
}

// TestRetrierHonoursRetryAfterExactly pins the pad: sleeping for precisely the
// advertised interval tends to land back on Spotify's window boundary.
func TestRetrierHonoursRetryAfterExactly(t *testing.T) {
	doer := spotifytest.NewScriptedDoer(
		spotifytest.Step{Status: 429, Header: spotifytest.RetryAfterHeader(7)},
		spotifytest.Step{Status: 200, Body: `{}`},
	)
	clk := spotifytest.NewFakeClock(epoch)
	r := newTestRetrier(doer, clk, testPolicy())

	resp, err := r.do(context.Background(), req(t, http.MethodGet, "https://api.spotify.com/v1/me"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	slept := clk.Slept()
	if len(slept) != 1 {
		t.Fatalf("sleeps = %v, want exactly one", slept)
	}
	if want := 7*time.Second + retryAfterPad; slept[0] != want {
		t.Errorf("slept %v, want Retry-After plus the pad (%v)", slept[0], want)
	}
}

// TestRetrierRateLimitCeiling: an excessive Retry-After must fail fast, without sleeping.
func TestRetrierRateLimitCeiling(t *testing.T) {
	doer := spotifytest.NewScriptedDoer(
		spotifytest.Step{Status: 429, Header: spotifytest.RetryAfterHeader(600)},
	)
	clk := spotifytest.NewFakeClock(epoch)
	p := testPolicy()
	p.MaxRetryAfter = 60 * time.Second
	r := newTestRetrier(doer, clk, p)

	_, err := r.do(context.Background(), req(t, http.MethodGet, "https://api.spotify.com/v1/me"))

	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %T (%v), want *RateLimitError", err, err)
	}
	if want := 600*time.Second + retryAfterPad; rl.RetryAfter != want {
		t.Errorf("RetryAfter = %v, want %v", rl.RetryAfter, want)
	}
	if got := clk.Slept(); len(got) != 0 {
		t.Errorf("slept %v, want no sleep at all when over the ceiling", got)
	}
	if got := doer.Calls(); got != 1 {
		t.Errorf("HTTP calls = %d, want 1", got)
	}
}

// TestRetrierNeverSleepsPastDeadline: blocking until a context deadline only to return a
// context error wastes the remaining budget and hides the real cause.
func TestRetrierNeverSleepsPastDeadline(t *testing.T) {
	doer := spotifytest.NewScriptedDoer(
		spotifytest.Step{Status: 429, Header: spotifytest.RetryAfterHeader(30)},
		spotifytest.Step{Status: 200, Body: `{}`},
	)
	// This test is the one place the fake clock must share a timeline with the real one:
	// context deadlines are real-time, and the retrier compares clock.Now() against
	// ctx.Deadline(). Seeding the fake at time.Now() keeps that comparison meaningful --
	// seeding it at a fixed past date would expire the context before the first attempt.
	now := time.Now()
	clk := spotifytest.NewFakeClock(now)
	r := newTestRetrier(doer, clk, testPolicy())

	// A deadline five seconds out cannot accommodate a 31-second wait.
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(5*time.Second))
	defer cancel()

	_, err := r.do(ctx, req(t, http.MethodGet, "https://api.spotify.com/v1/me"))
	if err == nil {
		t.Fatal("do() succeeded, want failure")
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Errorf("err = %T (%v), want *RateLimitError carrying the cause", err, err)
	}
	if got := clk.Slept(); len(got) != 0 {
		t.Errorf("slept %v, want no sleep past the deadline", got)
	}
	if got := doer.Calls(); got != 1 {
		t.Errorf("HTTP calls = %d, want 1", got)
	}
}

func TestRetrierCancelledContext(t *testing.T) {
	doer := spotifytest.NewScriptedDoer(spotifytest.Step{Status: 200, Body: `{}`})
	clk := spotifytest.NewFakeClock(epoch)
	r := newTestRetrier(doer, clk, testPolicy())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.do(ctx, req(t, http.MethodGet, "https://api.spotify.com/v1/me")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := doer.Calls(); got != 0 {
		t.Errorf("HTTP calls = %d, want 0 for an already-cancelled context", got)
	}
}

// TestRetrierRebuildsRequestPerAttempt is why do() takes a factory: a *http.Request with
// a consumed body cannot be replayed.
func TestRetrierRebuildsRequestPerAttempt(t *testing.T) {
	doer := spotifytest.NewScriptedDoer(
		spotifytest.Step{Status: 503},
		spotifytest.Step{Status: 503},
		spotifytest.Step{Status: 200, Body: `{}`},
	)
	clk := spotifytest.NewFakeClock(epoch)
	r := newTestRetrier(doer, clk, testPolicy())

	const payload = "grant_type=refresh_token&refresh_token=abc"
	factory := func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, "https://accounts.spotify.com/api/token",
			strings.NewReader(payload))
	}

	resp, err := r.do(context.Background(), factory)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	bodies := doer.RequestBodies()
	if len(bodies) != 3 {
		t.Fatalf("bodies = %v, want 3", bodies)
	}
	for i, b := range bodies {
		if b != payload {
			t.Errorf("attempt %d body = %q, want %q -- each attempt needs a fresh body", i+1, b, payload)
		}
	}
}

func TestRetrierFactoryError(t *testing.T) {
	doer := spotifytest.NewScriptedDoer()
	r := newTestRetrier(doer, spotifytest.NewFakeClock(epoch), testPolicy())
	want := errors.New("bad url")

	_, err := r.do(context.Background(), func() (*http.Request, error) { return nil, want })
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want the factory error", err)
	}
	if doer.Calls() != 0 {
		t.Error("a failing factory must not produce an HTTP call")
	}
}

// trackingBody reports whether it was drained and closed.
type trackingBody struct {
	r      io.Reader
	closed atomic.Bool
	read   atomic.Bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	b.read.Store(true)
	return b.r.Read(p)
}

func (b *trackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

// TestRetrierClosesDiscardedBodies guards the classic leak: an un-closed body on a
// retried attempt holds its connection out of the pool until the GC finalises it.
func TestRetrierClosesDiscardedBodies(t *testing.T) {
	bodies := []*trackingBody{
		{r: strings.NewReader(`{"error":{"status":503,"message":"busy"}}`)},
		{r: strings.NewReader(`{"error":{"status":503,"message":"busy"}}`)},
	}
	var n atomic.Int32

	doer := doerFunc(func(rq *http.Request) (*http.Response, error) {
		i := int(n.Add(1)) - 1
		if i < len(bodies) {
			return &http.Response{
				StatusCode: 503, Status: "503 Service Unavailable",
				Header: http.Header{}, Body: bodies[i], Request: rq,
			}, nil
		}
		return &http.Response{
			StatusCode: 200, Status: "200 OK", Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(`{}`)), Request: rq,
		}, nil
	})

	r := newTestRetrier(doer, spotifytest.NewFakeClock(epoch), testPolicy())
	resp, err := r.do(context.Background(), req(t, http.MethodGet, "https://api.spotify.com/v1/me"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	for i, b := range bodies {
		if !b.closed.Load() {
			t.Errorf("discarded response %d was not closed", i+1)
		}
		if !b.read.Load() {
			t.Errorf("discarded response %d was not drained", i+1)
		}
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// TestRetrierSuiteIsFast: the whole point of the injected clock. A real-clock retry
// suite covering the same schedule would take over a minute.
func TestRetrierSuiteIsFast(t *testing.T) {
	start := time.Now()

	steps := make([]spotifytest.Step, 0, 5)
	for i := 0; i < 4; i++ {
		steps = append(steps, spotifytest.Step{Status: 429, Header: spotifytest.RetryAfterHeader(10)})
	}
	steps = append(steps, spotifytest.Step{Status: 200, Body: `{}`})

	clk := spotifytest.NewFakeClock(epoch)
	r := newTestRetrier(spotifytest.NewScriptedDoer(steps...), clk, testPolicy())
	resp, err := r.do(context.Background(), req(t, http.MethodGet, "https://api.spotify.com/v1/me"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if want := 44 * time.Second; clk.TotalSlept() != want {
		t.Errorf("simulated sleep = %v, want %v", clk.TotalSlept(), want)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("real elapsed time %v exceeds 50ms; is something sleeping for real?", elapsed)
	}
}

// ---------------------------------------------------------------------------
// parseRetryAfter / backoff
// ---------------------------------------------------------------------------

func TestParseRetryAfter(t *testing.T) {
	now := epoch
	tests := []struct {
		name   string
		in     string
		want   time.Duration
		wantOK bool
	}{
		{"delta seconds", "30", 30 * time.Second, true},
		{"zero seconds", "0", 0, true},
		{"large delta", "3600", time.Hour, true},
		{"http date in the future", now.Add(45 * time.Second).UTC().Format(http.TimeFormat), 45 * time.Second, true},
		{"http date in the past clamps to zero", now.Add(-time.Minute).UTC().Format(http.TimeFormat), 0, true},
		{"empty", "", 0, false},
		{"garbage", "soon", 0, false},
		{"negative seconds", "-5", 0, false},
		{"float", "1.5", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.in, now)
			if ok != tc.wantOK {
				t.Fatalf("parseRetryAfter(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestBackoffEqualJitterBounds pins the schedule at both jitter extremes. Equal jitter
// keeps the minimum delay monotonically increasing, which is what makes it assertable.
func TestBackoffEqualJitterBounds(t *testing.T) {
	p := DefaultRetryPolicy() // base 250ms, max 20s

	for attempt, want := range []struct{ lo, hi time.Duration }{
		{125 * time.Millisecond, 250 * time.Millisecond},
		{250 * time.Millisecond, 500 * time.Millisecond},
		{500 * time.Millisecond, time.Second},
		{time.Second, 2 * time.Second},
	} {
		pLo, pHi := p, p
		pLo.Rand = spotifytest.FixedRand(0.0)
		pHi.Rand = spotifytest.FixedRand(1.0)

		if got := pLo.backoff(attempt); got != want.lo {
			t.Errorf("attempt %d lower bound = %v, want %v", attempt, got, want.lo)
		}
		if got := pHi.backoff(attempt); got != want.hi {
			t.Errorf("attempt %d upper bound = %v, want %v", attempt, got, want.hi)
		}
	}
}

func TestBackoffIsCapped(t *testing.T) {
	p := DefaultRetryPolicy()
	p.Rand = spotifytest.FixedRand(1.0)
	for _, attempt := range []int{10, 20, 100} {
		if got := p.backoff(attempt); got > p.MaxDelay {
			t.Errorf("backoff(%d) = %v, want <= MaxDelay %v", attempt, got, p.MaxDelay)
		}
	}
}

func TestBackoffMinimumIsMonotonic(t *testing.T) {
	p := DefaultRetryPolicy()
	p.Rand = spotifytest.FixedRand(0.0)
	prev := time.Duration(0)
	for attempt := 0; attempt < 7; attempt++ {
		got := p.backoff(attempt)
		if got < prev {
			t.Errorf("backoff(%d) = %v decreased from %v; equal jitter must keep the floor rising",
				attempt, got, prev)
		}
		prev = got
	}
}

func TestRetryPolicyZeroValueUsesDefaults(t *testing.T) {
	var p RetryPolicy
	d := p.withDefaults()
	want := DefaultRetryPolicy()
	if d.MaxAttempts != want.MaxAttempts || d.BaseDelay != want.BaseDelay ||
		d.MaxDelay != want.MaxDelay || d.MaxRetryAfter != want.MaxRetryAfter {
		t.Errorf("withDefaults() = %+v, want the DefaultRetryPolicy values", d)
	}
	if d.Rand == nil {
		t.Error("withDefaults left Rand nil")
	}
}
