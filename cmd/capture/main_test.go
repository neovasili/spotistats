package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/neovasili/spotistats/internal/ingest"
	"github.com/neovasili/spotistats/internal/metrics"
	"github.com/neovasili/spotistats/internal/spotify"
)

// emitted runs publishMetrics and returns the resulting metric values.
func emitted(t *testing.T, res ingest.Result, err error) map[string]float64 {
	t.Helper()
	var buf bytes.Buffer
	em := metrics.NewEMF(&buf)
	publishMetrics(em, res, err)
	if ferr := em.Flush(); ferr != nil {
		t.Fatal(ferr)
	}

	var doc map[string]any
	if jerr := json.Unmarshal(buf.Bytes(), &doc); jerr != nil {
		t.Fatalf("EMF output invalid: %v\n%s", jerr, buf.String())
	}
	out := map[string]float64{}
	for k, v := range doc {
		if f, ok := v.(float64); ok {
			out[k] = f
		}
	}
	return out
}

// TestPublishMetricsMapping is the alarm contract. Each assertion corresponds to an alarm in
// infra/stack.go; a regression here would leave a real failure unnoticed in production.
func TestPublishMetricsMapping(t *testing.T) {
	tests := []struct {
		name string
		res  ingest.Result
		err  error
		want map[string]float64
		// absent lists metrics that must NOT be emitted, since a stray 1 would fire an alarm
		// for something that did not happen.
		absent []string
	}{
		{
			name: "clean run",
			res:  ingest.Result{Inserted: 7, Duplicates: 2},
			want: map[string]float64{
				metrics.PlaysIngested:    7,
				metrics.PlaysDuplicate:   2,
				metrics.PlaysGapDetected: 0,
				metrics.GenresDegraded:   0,
				metrics.CaptureFailed:    0,
			},
			absent: []string{metrics.TokenRefreshFailed},
		},
		{
			name: "saturated page raises the gap metric",
			res:  ingest.Result{Inserted: 50, Saturated: true, GapRecorded: true},
			want: map[string]float64{
				metrics.PlaysGapDetected: 1,
				metrics.CaptureFailed:    0,
			},
		},
		{
			name: "degraded genres are reported but not a failure",
			res:  ingest.Result{Inserted: 3, GenresDegraded: true},
			want: map[string]float64{
				metrics.GenresDegraded: 1,
				// The plays were still ingested, so the run did not fail.
				metrics.CaptureFailed: 0,
				metrics.PlaysIngested: 3,
			},
		},
		{
			name: "generic failure",
			res:  ingest.Result{},
			err:  errors.New("dynamodb throttled"),
			want: map[string]float64{metrics.CaptureFailed: 1},
			// A generic failure must not look like a revoked token: that alarm tells a human
			// to re-run the interactive auth flow.
			absent: []string{metrics.TokenRefreshFailed},
		},
		{
			name: "revoked token gets its own metric",
			res:  ingest.Result{},
			err:  fmt.Errorf("capture: %w", spotify.ErrRefreshTokenInvalid),
			want: map[string]float64{
				metrics.CaptureFailed:      1,
				metrics.TokenRefreshFailed: 1,
			},
		},
		{
			name: "revoked token wrapped several layers deep is still detected",
			res:  ingest.Result{},
			err:  fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", spotify.ErrRefreshTokenInvalid)),
			want: map[string]float64{metrics.TokenRefreshFailed: 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := emitted(t, tc.res, tc.err)
			for name, want := range tc.want {
				if got[name] != want {
					t.Errorf("%s = %v, want %v", name, got[name], want)
				}
			}
			for _, name := range tc.absent {
				if _, present := got[name]; present {
					t.Errorf("%s was emitted (%v) but must not be", name, got[name])
				}
			}
		})
	}
}

// Every metric an alarm watches must actually be emitted by some path, or the alarm is
// decoration.
func TestEveryAlarmedMetricIsEmitted(t *testing.T) {
	// A run exercising every flag at once.
	got := emitted(t, ingest.Result{
		Inserted: 1, Duplicates: 1, GapRecorded: true, GenresDegraded: true,
	}, fmt.Errorf("wrapped: %w", spotify.ErrRefreshTokenInvalid))

	for _, name := range []string{
		metrics.PlaysIngested,
		metrics.PlaysDuplicate,
		metrics.PlaysGapDetected,
		metrics.GenresDegraded,
		metrics.CaptureFailed,
		metrics.TokenRefreshFailed,
	} {
		if _, ok := got[name]; !ok {
			t.Errorf("metric %q is alarmed on in infra/ but never emitted", name)
		}
	}
}
