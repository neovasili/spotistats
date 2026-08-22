package spotify

import (
	"context"
	"net/http"
	"time"
)

// Clock is the package's only source of time and its only way to block. Injecting it
// is what lets the retry and rate-limit suites assert on backoff schedules without
// actually sleeping: the fake advances a counter and returns immediately, so tests that
// exercise minutes of backoff finish in microseconds.
type Clock interface {
	Now() time.Time
	// Sleep blocks for d or until ctx is done, whichever comes first, returning
	// ctx.Err() in the latter case.
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock returns a Clock backed by the real wall clock.
func SystemClock() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Doer is the HTTP seam. *http.Client satisfies it, and so does a scripted fake.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}
