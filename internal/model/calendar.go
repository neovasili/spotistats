package model

import (
	"fmt"
	"time"
)

// Calendar derives calendar period keys in the listener's local timezone.
//
// The split between stored timestamps and derived keys is deliberate and is the single
// most consequential decision in the data model:
//
//   - Timestamps are ALWAYS stored as UTC instants (see FormatTS). They are canonical
//     and unambiguous.
//   - Period keys are ALWAYS derived in the local zone, because "minutes listened in
//     2025" means the local calendar year. A play at 2025-12-31T23:30:00Z is Madrid
//     2026-01-01T00:30 and belongs to 2026; deriving in UTC would permanently misfile
//     every play in the one-to-two-hour boundary band.
//   - The PLAY# storage partition, by contrast, is derived in UTC -- see
//     PlayPartitionUTC for why.
type Calendar struct {
	loc  *time.Location
	name string
}

// NewCalendar loads the named IANA zone (for example "Europe/Madrid"). An empty name
// means UTC. The zone database is embedded in the binary; see tzdata.go.
func NewCalendar(tzName string) (Calendar, error) {
	if tzName == "" {
		return Calendar{loc: time.UTC, name: "UTC"}, nil
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return Calendar{}, fmt.Errorf("model: load timezone %q: %w", tzName, err)
	}
	return Calendar{loc: loc, name: tzName}, nil
}

// MustCalendar is NewCalendar for tests and package-level defaults. It panics.
func MustCalendar(tzName string) Calendar {
	c, err := NewCalendar(tzName)
	if err != nil {
		panic(err)
	}
	return c
}

// Location returns the zone period keys are derived in.
func (c Calendar) Location() *time.Location {
	if c.loc == nil {
		return time.UTC
	}
	return c.loc
}

// Name returns the configured zone name. It is persisted in the STATE/CONFIG row so a
// changed timezone becomes a loud startup failure rather than silently misfiled keys.
func (c Calendar) Name() string {
	if c.name == "" {
		return "UTC"
	}
	return c.name
}

// Day returns the local calendar day of t, e.g. 2025-03-14.
func (c Calendar) Day(t time.Time) Period {
	return Period(t.In(c.Location()).Format(layoutDay))
}

// Month returns the local calendar month of t, e.g. 2025-03.
func (c Calendar) Month(t time.Time) Period {
	return Period(t.In(c.Location()).Format(layoutMonth))
}

// Year returns the local calendar year of t, e.g. 2025.
func (c Calendar) Year(t time.Time) Period {
	return Period(t.In(c.Location()).Format(layoutYear))
}

// HourOfDay returns the local hour of t in [0,24). The listening-rhythm histogram uses
// local hours; a "by hour of day" chart in UTC would be meaningless.
func (c Calendar) HourOfDay(t time.Time) int {
	return t.In(c.Location()).Hour()
}

// Weekday returns the local weekday of t.
func (c Calendar) Weekday(t time.Time) time.Weekday {
	return t.In(c.Location()).Weekday()
}

// Bounds returns the half-open UTC instant range [start, end) covered by p in c's
// zone. It is the inverse of Day/Month/Year and is what the reconciler and the
// timeline endpoint use to turn a period back into a scan range.
//
// PeriodAll has no bounds; it returns ErrUnboundedPeriod so callers must handle
// all-time explicitly rather than silently scanning from the zero time.
func (c Calendar) Bounds(p Period) (start, end time.Time, err error) {
	switch p.Granularity() {
	case GranularityAll:
		if p == PeriodAll {
			return time.Time{}, time.Time{}, ErrUnboundedPeriod
		}
		return time.Time{}, time.Time{}, fmt.Errorf("%w: %q", ErrInvalidPeriod, p)

	case GranularityYear:
		t, perr := time.Parse(layoutYear, string(p))
		if perr != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("%w: %q", ErrInvalidPeriod, p)
		}
		y := t.Year()
		return c.firstInstantOf(y, time.January, 1), c.firstInstantOf(y+1, time.January, 1), nil

	case GranularityMonth:
		t, perr := time.Parse(layoutMonth, string(p))
		if perr != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("%w: %q", ErrInvalidPeriod, p)
		}
		y, m := t.Year(), t.Month()
		// time.Date normalises month 13 to January of the next year, so no special case.
		return c.firstInstantOf(y, m, 1), c.firstInstantOf(y, m+1, 1), nil

	case GranularityDay:
		t, perr := time.Parse(layoutDay, string(p))
		if perr != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("%w: %q", ErrInvalidPeriod, p)
		}
		y, m, d := t.Date()
		// Adding 24h would be wrong across a DST transition (a local day can be 23 or
		// 25 hours); ask for the next calendar day instead.
		return c.firstInstantOf(y, m, d), c.firstInstantOf(y, m, d+1), nil
	}
	return time.Time{}, time.Time{}, fmt.Errorf("%w: %q", ErrInvalidPeriod, p)
}

// firstInstantOf returns the earliest UTC instant whose local calendar date in c's
// zone is exactly y-m-d. Out-of-range values are normalised the way time.Date does,
// so (2025, time.December+1, 1) means 2026-01-01 and (2025, 3, 32) means 2025-04-01.
//
// This exists because time.Date alone is not sufficient at midnight DST transitions,
// and time.Date's behaviour there is explicitly documented as "not guaranteed":
//
//   - Gap at midnight. America/Havana starts DST at 00:00 local, so local midnight
//     simply does not exist on that date. Verified: time.Date(2025, 3, 9, 0,0,0,0,
//     Havana) returns 2025-03-08T23:00 -0500, i.e. the PREVIOUS calendar day. The true
//     first instant of local 2025-03-09 is 2025-03-09T05:00Z (local 01:00 -0400).
//   - Ambiguity at midnight. When the clock falls back through midnight, local 00:00
//     occurs twice and only the earlier instant is the true start of the day.
//
// Both are handled by nudging from time.Date's answer to the real boundary. Europe/
// Madrid transitions at 02:00/03:00 and is unaffected, but relying on that would make
// the code correct only by accident of the configured zone.
//
// The scan is minute-granular, which is exact for every transition in the modern era
// (all are on whole or half hours). Sub-minute historical LMT offsets predate Spotify
// by a century and are out of scope.
func (c Calendar) firstInstantOf(y int, m time.Month, d int) time.Time {
	loc := c.Location()
	t := time.Date(y, m, d, 0, 0, 0, 0, loc)

	// time.Date normalises the requested date; compare against the normalised form.
	wantY, wantM, wantD := time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Date()

	const step = time.Minute
	const maxSteps = 48 * 60 // no real transition shifts a date by anywhere near this

	// Climb out of a gap: advance until the local date actually matches.
	for i := 0; i < maxSteps; i++ {
		ly, lm, ld := t.In(loc).Date()
		if ly == wantY && lm == wantM && ld == wantD {
			break
		}
		t = t.Add(step)
	}

	// Retreat to the earliest instant that still reports this date, which matters when
	// local midnight is ambiguous.
	for i := 0; i < maxSteps; i++ {
		prev := t.Add(-step)
		ly, lm, ld := prev.In(loc).Date()
		if ly != wantY || lm != wantM || ld != wantD {
			break
		}
		t = prev
	}

	return t.UTC()
}
