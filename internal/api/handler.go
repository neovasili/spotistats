package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// BasePath is the prefix every route lives under. CloudFront routes /api/* to the query
// Lambda, so the path is part of the deployed contract.
const BasePath = "/api/v1"

// Config configures a Handler.
type Config struct {
	Store    *store.Store
	Calendar model.Calendar
	Now      func() time.Time
	Logger   *slog.Logger
}

// Handler serves the query API.
type Handler struct {
	store *store.Store
	cal   model.Calendar
	now   func() time.Time
	log   *slog.Logger
	mux   *http.ServeMux
}

// New validates cfg and returns a Handler ready to serve.
func New(cfg Config) (*Handler, error) {
	if cfg.Store == nil {
		return nil, errors.New("api: a store is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	h := &Handler{store: cfg.Store, cal: cfg.Calendar, now: now, log: log}
	h.mux = h.routes()
	return h, nil
}

// ServeHTTP makes Handler an http.Handler, which is what lets the Lambda adapter and the
// local server share one implementation.
//
// Method filtering happens here rather than per route. The API is entirely read-only, so
// "only GET and HEAD" is a property of the whole surface; enforcing it per route would let a
// POST fall through to the catch-all and be reported as an unknown path, which misdirects the
// caller.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, r, h.log, &apiError{
			status:  http.StatusMethodNotAllowed,
			Code:    CodeMethodNotAllowed,
			Message: "this API is read-only; only GET and HEAD are supported",
		})
		return
	}
	h.mux.ServeHTTP(w, r)
}

// handlerFunc is an endpoint that may fail. Centralising error rendering keeps every handler
// free of response-writing boilerplate and guarantees a uniform envelope.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

func (h *Handler) wrap(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			writeError(w, r, h.log, err)
		}
	}
}

func (h *Handler) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// Method-qualified patterns mean a POST to a read-only endpoint gets a 405 from the mux
	// rather than being silently treated as a GET.
	mux.HandleFunc("GET "+BasePath+"/meta", h.wrap(h.handleMeta))
	mux.HandleFunc("GET "+BasePath+"/stats", h.wrap(h.handleStats))
	mux.HandleFunc("GET "+BasePath+"/top", h.wrap(h.handleTop))
	mux.HandleFunc("GET "+BasePath+"/list", h.wrap(h.handleList))
	mux.HandleFunc("GET "+BasePath+"/plays", h.wrap(h.handlePlays))
	mux.HandleFunc("GET "+BasePath+"/timeline", h.wrap(h.handleTimeline))

	// Health check for the local server and any future uptime probe. Deliberately uncached.
	mux.HandleFunc("GET "+BasePath+"/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})

	// Anything else under the API prefix is a 404 in the documented envelope rather than
	// net/http's bare text, so a client only ever has to parse one error shape.
	mux.HandleFunc(BasePath+"/", h.wrap(func(_ http.ResponseWriter, r *http.Request) error {
		return notFound("no such endpoint: %s", r.URL.Path)
	}))

	return mux
}

// resolveRange turns either a period or an explicit from/to pair into the list of period keys
// a query should sum, plus the label to report.
//
// It exists because /stats and /timeline both accept either form and must agree on what a
// range means.
func (h *Handler) periodsBetween(from, to model.Period, gran model.Granularity) ([]model.Period, error) {
	start, _, err := h.cal.Bounds(from)
	if err != nil {
		return nil, badRequest(CodeInvalidPeriod, "from=%s: %v", from, err)
	}
	_, end, err := h.cal.Bounds(to)
	if err != nil {
		return nil, badRequest(CodeInvalidPeriod, "to=%s: %v", to, err)
	}
	if !start.Before(end) {
		return nil, badRequest(CodeInvalidRange, "from=%s is not before to=%s", from, to)
	}

	// Walk the local calendar rather than adding fixed durations: a local day can be 23 or 25
	// hours across a DST transition, so arithmetic on instants would drift.
	var out []model.Period
	const maxBuckets = 4096 // ~11 years of days; a guard against a pathological range
	cur := start
	for i := 0; cur.Before(end); i++ {
		if i >= maxBuckets {
			return nil, badRequest(CodeInvalidRange,
				"range covers more than %d %s buckets; narrow it or use a coarser bucket",
				maxBuckets, gran)
		}
		var p model.Period
		switch gran {
		case model.GranularityDay:
			p = h.cal.Day(cur)
		case model.GranularityYear:
			p = h.cal.Year(cur)
		default:
			p = h.cal.Month(cur)
		}
		out = append(out, p)

		_, next, berr := h.cal.Bounds(p)
		if berr != nil {
			return nil, berr
		}
		cur = next
	}
	return out, nil
}
