package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/neovasili/spotistats/internal/model"
)

// Cache lifetimes from docs/SPECS.md 6.2. The data changes once a night, so an hour at the
// edge is free correctness rather than a staleness risk.
const (
	browserMaxAge = 60 * time.Second
	edgeMaxAge    = time.Hour
)

// Metrics is the measure envelope every response carrying a duration must include (§6.4).
//
// MsPlayedExact is a SUBSET of MsPlayed, not a parallel total: an api-sourced play has no
// real duration, so it contributes its track's full length to MsPlayed and nothing to
// MsPlayedExact. EstimatedRatio is therefore 1 - exact/total, and a client that renders
// MsPlayed without surfacing a non-zero ratio is presenting an estimate as a measurement.
type Metrics struct {
	Plays          int64   `json:"plays"`
	PlaysExact     int64   `json:"playsExact"`
	MsPlayed       int64   `json:"msPlayed"`
	MsPlayedExact  int64   `json:"msPlayedExact"`
	EstimatedRatio float64 `json:"estimatedRatio"`
}

func metricsOf(a model.Aggregate) Metrics {
	return Metrics{
		Plays:          a.Plays,
		PlaysExact:     a.PlaysExact,
		MsPlayed:       a.MsPlayed,
		MsPlayedExact:  a.MsPlayedExact,
		EstimatedRatio: round4(a.EstimatedRatio()),
	}
}

// round4 keeps the ratio readable in JSON without implying more precision than it has.
func round4(f float64) float64 {
	return float64(int64(f*10000+0.5)) / 10000
}

// tsPtr renders an instant, or nil when unset, so a client can tell "no data" from "epoch".
func tsPtr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := model.FormatTS(t)
	return &s
}

func writeJSON(w http.ResponseWriter, r *http.Request, log *slog.Logger, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", cacheControl())
	// Compression happens at CloudFront, which needs to know the response varies by it.
	w.Header().Add("Vary", "Accept-Encoding")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent, so this can only be logged.
		log.ErrorContext(r.Context(), "api: encode response", "path", r.URL.Path, "err", err)
	}
}

func cacheControl() string {
	return "public, max-age=" + seconds(browserMaxAge) + ", s-maxage=" + seconds(edgeMaxAge)
}

func seconds(d time.Duration) string {
	return itoa(int64(d / time.Second))
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
