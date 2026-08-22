package api

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/neovasili/spotistats/internal/model"
)

// Limits from docs/SPECS.md 6.2.
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// params parses and validates a request's query string against a whitelist.
//
// Errors accumulate rather than short-circuiting, so a request with three problems reports
// the first deterministically instead of depending on map iteration order.
type params struct {
	values url.Values
	first  *apiError
}

// newParams rejects any parameter not in allowed.
//
// Rejecting rather than ignoring is deliberate (§6.2): `?perido=2025` silently ignored would
// return all-time figures that look entirely plausible and are wrong. A 400 surfaces the typo
// at the moment it is made.
func newParams(r *http.Request, allowed ...string) *params {
	p := &params{values: r.URL.Query()}

	permitted := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		permitted[a] = struct{}{}
	}

	var unknown []string
	for name := range p.values {
		if _, ok := permitted[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		sort.Strings(allowed)
		p.fail(badRequest(CodeUnknownParameter,
			"unknown query parameter(s): %s. Accepted for this endpoint: %s",
			strings.Join(unknown, ", "), strings.Join(allowed, ", ")))
	}
	return p
}

func (p *params) fail(e *apiError) {
	if p.first == nil {
		p.first = e
	}
}

// err returns the first validation failure, or nil.
func (p *params) err() error {
	if p.first == nil {
		return nil
	}
	return p.first
}

func (p *params) has(name string) bool { return p.values.Has(name) }

// str returns a string parameter, or def when absent.
func (p *params) str(name, def string) string {
	if v := p.values.Get(name); v != "" {
		return v
	}
	return def
}

// required returns a parameter that must be present and non-empty.
func (p *params) required(name string) string {
	v := p.values.Get(name)
	if v == "" {
		p.fail(badRequest(CodeMissingParameter, "%s is required", name))
	}
	return v
}

// limit parses `limit`, defaulting to DefaultLimit and capping at MaxLimit.
//
// The cap is a hard ceiling rather than an error: a client asking for more than the maximum
// gets the maximum, which is friendlier than a 400 and still bounds the work.
func (p *params) limit() int {
	raw := p.values.Get("limit")
	if raw == "" {
		return DefaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		p.fail(badRequest(CodeInvalidParameter, "limit must be an integer, got %q", raw))
		return DefaultLimit
	}
	if n < 1 {
		p.fail(badRequest(CodeInvalidParameter, "limit must be at least 1, got %d", n))
		return DefaultLimit
	}
	if n > MaxLimit {
		return MaxLimit
	}
	return n
}

// dim parses a dimension. entityOnly rejects TOTAL, which has no entity to name.
func (p *params) dim(name string, entityOnly bool) model.Dim {
	raw := p.required(name)
	if raw == "" {
		return ""
	}
	d := model.Dim(strings.ToUpper(raw))
	if !d.Valid() {
		p.fail(badRequest(CodeInvalidDimension,
			"%s must be one of track, artist, album, genre, total; got %q", name, raw))
		return ""
	}
	if entityOnly && d == model.DimTotal {
		p.fail(badRequest(CodeInvalidDimension,
			"%s=total has no entities; use /meta for overall figures", name))
		return ""
	}
	return d
}

// period parses a period, defaulting to all-time when absent.
func (p *params) period(name string) model.Period {
	raw := p.values.Get(name)
	if raw == "" {
		return model.PeriodAll
	}
	period, err := model.ParsePeriod(raw)
	if err != nil {
		p.fail(badRequest(CodeInvalidPeriod,
			"%s must be ALL, YYYY, YYYY-MM or YYYY-MM-DD; got %q", name, raw))
		return model.PeriodAll
	}
	return period
}

// requiredPeriod parses a period that must be present and must not be all-time.
//
// Presence is checked BEFORE parsing so an omitted parameter reports MISSING_PARAMETER rather
// than INVALID_PERIOD: telling a caller their absent parameter is malformed sends them looking
// for a syntax error that does not exist.
func (p *params) requiredPeriod(name string) model.Period {
	if !p.has(name) {
		p.fail(badRequest(CodeMissingParameter, "%s is required", name))
		return model.PeriodAll
	}
	period := p.period(name)
	if period == model.PeriodAll {
		p.fail(badRequest(CodeInvalidPeriod, "%s must be a specific period, not ALL", name))
	}
	return period
}

// metric parses the measure to rank or report by.
type metric string

const (
	metricPlays   metric = "plays"
	metricMs      metric = "ms"
	metricMsExact metric = "msExact"
)

func (p *params) metric(def metric) metric {
	raw := p.values.Get("metric")
	if raw == "" {
		return def
	}
	switch metric(raw) {
	case metricPlays, metricMs, metricMsExact:
		return metric(raw)
	}
	p.fail(badRequest(CodeInvalidParameter,
		"metric must be plays, ms or msExact; got %q", raw))
	return def
}

// value extracts the measure this metric names from an aggregate, which is what /top and
// /list sort by.
func (m metric) value(a model.Aggregate) int64 {
	switch m {
	case metricPlays:
		return a.Plays
	case metricMsExact:
		return a.MsPlayedExact
	default:
		return a.MsPlayed
	}
}

// order is the sort direction.
func (p *params) descending() bool {
	switch raw := p.values.Get("order"); raw {
	case "", "desc":
		return true
	case "asc":
		return false
	default:
		p.fail(badRequest(CodeInvalidParameter, "order must be asc or desc; got %q", raw))
		return true
	}
}

// bucket is the granularity of a timeline series.
func (p *params) bucket() model.Granularity {
	switch raw := p.values.Get("bucket"); raw {
	case "", "month":
		return model.GranularityMonth
	case "day":
		return model.GranularityDay
	case "year":
		return model.GranularityYear
	default:
		p.fail(badRequest(CodeInvalidParameter, "bucket must be day, month or year; got %q", raw))
		return model.GranularityMonth
	}
}

// timestamp parses an RFC3339 instant.
func (p *params) timestamp(name string) (time.Time, bool) {
	raw := p.values.Get(name)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{model.TimestampFormat, "2006-01-02T15:04:05Z07:00", "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	p.fail(badRequest(CodeInvalidParameter,
		"%s must be an RFC3339 instant or YYYY-MM-DD; got %q", name, raw))
	return time.Time{}, false
}
