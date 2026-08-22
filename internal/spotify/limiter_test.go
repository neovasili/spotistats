package spotify

import (
	"context"
	"testing"
	"time"

	"github.com/neovasili/spotistats/internal/spotify/spotifytest"
)

func TestWindowLimiterAllowsUpToN(t *testing.T) {
	clk := spotifytest.NewFakeClock(epoch)
	l := NewWindowLimiter(3, 30*time.Second, clk)

	// The first three are free.
	for i := 0; i < 3; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("Wait %d: %v", i+1, err)
		}
	}
	if got := clk.Slept(); len(got) != 0 {
		t.Errorf("slept %v within the budget, want none", got)
	}
}

func TestWindowLimiterBlocksWhenFull(t *testing.T) {
	clk := spotifytest.NewFakeClock(epoch)
	l := NewWindowLimiter(2, 30*time.Second, clk)

	for i := 0; i < 2; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// The third must wait out the window from the oldest stamp.
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	slept := clk.Slept()
	if len(slept) != 1 {
		t.Fatalf("sleeps = %v, want exactly one", slept)
	}
	if slept[0] != 30*time.Second {
		t.Errorf("slept %v, want the full window (the oldest stamp was at t=0)", slept[0])
	}
}

func TestWindowLimiterSlidesForward(t *testing.T) {
	clk := spotifytest.NewFakeClock(epoch)
	l := NewWindowLimiter(2, 30*time.Second, clk)

	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk.Advance(10 * time.Second)
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Now full. The oldest stamp is at t=0, so the next slot frees at t=30, i.e. 20s away.
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	slept := clk.Slept()
	if len(slept) != 1 || slept[0] != 20*time.Second {
		t.Errorf("slept %v, want a single 20s wait", slept)
	}
}

func TestWindowLimiterExpiredStampsFreeCapacity(t *testing.T) {
	clk := spotifytest.NewFakeClock(epoch)
	l := NewWindowLimiter(2, 30*time.Second, clk)

	for i := 0; i < 2; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// Step past the window; both stamps age out and no wait is needed.
	clk.Advance(31 * time.Second)
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := clk.Slept(); len(got) != 0 {
		t.Errorf("slept %v, want none once the window has passed", got)
	}
}

func TestWindowLimiterRespectsContext(t *testing.T) {
	clk := spotifytest.NewFakeClock(epoch)
	l := NewWindowLimiter(1, 30*time.Second, clk)

	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Wait(ctx); err == nil {
		t.Error("Wait succeeded on a cancelled context")
	}
}

// A non-positive budget or window means "no limiting", which the client treats as a nil
// Limiter. The 2-hourly capture job makes about three calls and needs none.
func TestNewWindowLimiterDisabled(t *testing.T) {
	clk := spotifytest.NewFakeClock(epoch)
	for _, tc := range []struct {
		n int
		w time.Duration
	}{
		{0, time.Second}, {-1, time.Second}, {5, 0}, {5, -time.Second},
	} {
		if l := NewWindowLimiter(tc.n, tc.w, clk); l != nil {
			t.Errorf("NewWindowLimiter(%d, %v) = %v, want nil", tc.n, tc.w, l)
		}
	}
}

// TestWindowLimiterIsUsedByRetrier proves the limiter sits inside the retry loop, so a
// burst of retries cannot bypass the budget.
func TestWindowLimiterIsUsedByRetrier(t *testing.T) {
	clk := spotifytest.NewFakeClock(epoch)
	doer := spotifytest.NewScriptedDoer(
		spotifytest.Step{Status: 503},
		spotifytest.Step{Status: 503},
		spotifytest.Step{Status: 200, Body: `{}`},
	)
	r := &retrier{
		doer:    doer,
		policy:  testPolicy(),
		clock:   clk,
		limiter: NewWindowLimiter(1, 10*time.Second, clk),
	}

	resp, err := r.do(context.Background(), req(t, "GET", "https://api.spotify.com/v1/me"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// Three attempts against a budget of one per 10s: two backoff sleeps plus at least
	// one limiter sleep. If the limiter were outside the retry loop there would be two.
	if got := len(clk.Slept()); got < 3 {
		t.Errorf("sleeps = %d (%v), want at least 3 -- retries must also pass the limiter",
			got, clk.Slept())
	}
}
