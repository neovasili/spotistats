// Package httpxtest provides fakes for the internal/httpx transport.
//
// It lives beside httpx rather than inside spotifytest because none of it is Spotify-specific:
// a fake clock, a scripted Doer and a fixed jitter source are what ANY client built on this
// transport needs to test retry and rate-limiting behaviour, and there are now three of them.
//
// Keeping these in spotifytest also made httpx's own tests import spotify, which imports
// httpx -- an import cycle.
package httpxtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// FakeClock is a Clock whose Sleep returns immediately, recording the requested
// duration and advancing Now by it. A retry suite exercising minutes of backoff
// therefore finishes in microseconds, and the backoff schedule becomes directly
// assertable via Slept.
type FakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep records d, advances the clock by it, and returns immediately. It still honours
// an already-cancelled context so cancellation paths stay testable.
func (c *FakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	return nil
}

// Slept returns every duration passed to Sleep, in order.
func (c *FakeClock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// TotalSlept is the sum of every requested sleep.
func (c *FakeClock) TotalSlept() time.Duration {
	var total time.Duration
	for _, d := range c.Slept() {
		total += d
	}
	return total
}

// Advance moves the clock without recording a sleep.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// ErrScriptExhausted is returned when more requests are made than were scripted.
var ErrScriptExhausted = errors.New("httpxtest: scripted doer exhausted")

// Step is one scripted HTTP outcome. Err takes precedence over Status when set,
// modelling a transport failure.
type Step struct {
	Status int
	Header http.Header
	Body   string
	Err    error
}

// ScriptedDoer replays a fixed sequence of responses and records every request it saw.
// Use it for retry and backoff matrices, where determinism matters more than exercising
// real net/http; use httptest.NewServer for endpoint tests that should parse real
// headers off a real socket.
type ScriptedDoer struct {
	mu       sync.Mutex
	steps    []Step
	next     int
	requests []*http.Request
	bodies   []string
}

func NewScriptedDoer(steps ...Step) *ScriptedDoer {
	return &ScriptedDoer{steps: steps}
}

func (d *ScriptedDoer) Do(req *http.Request) (*http.Response, error) {
	// Capture the body before the transport would consume it; retries rebuild the
	// request, so this is how a test proves each attempt sent the same payload.
	var body string
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err == nil {
			body = string(b)
		}
		_ = req.Body.Close()
	}

	d.mu.Lock()
	d.requests = append(d.requests, req)
	d.bodies = append(d.bodies, body)
	if d.next >= len(d.steps) {
		n := d.next
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: request %d (%s %s) was not scripted",
			ErrScriptExhausted, n+1, req.Method, req.URL)
	}
	step := d.steps[d.next]
	d.next++
	d.mu.Unlock()

	if step.Err != nil {
		return nil, step.Err
	}
	h := step.Header
	if h == nil {
		h = http.Header{}
	}
	status := step.Status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(step.Body)),
		Request:    req,
	}, nil
}

// Requests returns every request received, in order.
func (d *ScriptedDoer) Requests() []*http.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*http.Request(nil), d.requests...)
}

// RequestBodies returns the body of every request received, in order.
func (d *ScriptedDoer) RequestBodies() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.bodies...)
}

// Calls is the number of requests received.
func (d *ScriptedDoer) Calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests)
}

// Remaining is the number of unconsumed scripted steps.
func (d *ScriptedDoer) Remaining() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.steps) - d.next
}

// FixedRand returns a jitter source that always yields v. Passing 0.0 and 1.0 pins the
// lower and upper bounds of a jittered backoff, making the schedule exactly assertable.
func FixedRand(v float64) func() float64 { return func() float64 { return v } }

// RetryAfterHeader builds a Retry-After header with a delta-seconds value.
func RetryAfterHeader(seconds int) http.Header {
	return http.Header{"Retry-After": []string{fmt.Sprintf("%d", seconds)}}
}
