package spotify

import (
	"context"
	"sync"
	"time"
)

// Limiter throttles outbound requests.
type Limiter interface {
	// Wait blocks until another request may be issued, or ctx is done.
	Wait(ctx context.Context) error
}

// NewWindowLimiter permits at most n requests per rolling window.
//
// Spotify does not publish its actual limit -- the documentation says only that the
// budget is computed over a rolling 30-second window -- so n is a self-imposed
// conservative guess and must stay configurable. It exists mainly for the history
// backfill, which issues thousands of metadata lookups and would otherwise discover the
// limit by collecting 429s. The 2-hourly capture job makes about three calls per run and
// needs no limiter at all.
//
// It uses the injected Clock, so tests never sleep in real time.
func NewWindowLimiter(n int, window time.Duration, clk Clock) Limiter {
	if n <= 0 || window <= 0 {
		return nil
	}
	if clk == nil {
		clk = SystemClock()
	}
	return &windowLimiter{n: n, window: window, clock: clk, stamps: make([]time.Time, 0, n)}
}

type windowLimiter struct {
	n      int
	window time.Duration
	clock  Clock

	mu     sync.Mutex
	stamps []time.Time // request times inside the current window, oldest first
}

func (l *windowLimiter) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		l.mu.Lock()
		now := l.clock.Now()
		cutoff := now.Add(-l.window)
		// Drop everything that has aged out of the window.
		keep := l.stamps[:0]
		for _, s := range l.stamps {
			if s.After(cutoff) {
				keep = append(keep, s)
			}
		}
		l.stamps = keep

		if len(l.stamps) < l.n {
			l.stamps = append(l.stamps, now)
			l.mu.Unlock()
			return nil
		}
		// Full: the earliest slot frees up one window after the oldest request.
		wait := l.stamps[0].Add(l.window).Sub(now)
		l.mu.Unlock()

		if wait <= 0 {
			// The clock did not advance (a fake clock that must be advanced by the
			// test); yield rather than spin forever.
			wait = time.Millisecond
		}
		if err := l.clock.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}
