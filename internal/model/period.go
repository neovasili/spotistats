package model

import (
	"errors"
	"fmt"
	"time"
)

// Period identifies a calendar bucket used as part of an aggregate key. The four
// accepted shapes are exactly:
//
//	ALL           all time
//	YYYY          a calendar year,  e.g. 2025
//	YYYY-MM       a calendar month, e.g. 2025-03
//	YYYY-MM-DD    a calendar day,   e.g. 2025-03-14
//
// Periods are always derived in the listener's local timezone -- see Calendar.
type Period string

// PeriodAll is the all-time period.
const PeriodAll Period = "ALL"

// Granularity is the resolution of a Period.
type Granularity uint8

const (
	GranularityAll Granularity = iota
	GranularityYear
	GranularityMonth
	GranularityDay
)

func (g Granularity) String() string {
	switch g {
	case GranularityAll:
		return "all"
	case GranularityYear:
		return "year"
	case GranularityMonth:
		return "month"
	case GranularityDay:
		return "day"
	default:
		return fmt.Sprintf("Granularity(%d)", uint8(g))
	}
}

// ErrInvalidPeriod is returned for any string that is not one of the four accepted
// Period shapes. The query API surfaces it as INVALID_PERIOD (docs/SPECS.md 6.2).
var ErrInvalidPeriod = errors.New("model: invalid period")

// Period layouts, keyed by string length. Length is what selects the layout.
const (
	layoutYear  = "2006"
	layoutMonth = "2006-01"
	layoutDay   = "2006-01-02"
)

// ParsePeriod validates s and returns it as a Period. It accepts exactly "ALL",
// "YYYY", "YYYY-MM" and "YYYY-MM-DD", case-sensitively.
//
// Validation is by length-dispatch followed by time.Parse. No regex is needed:
// time.Parse's numeric layouts are already strict, rejecting "2025-3-4" (single
// digits), "2025-13-01" and "2025-00-01" (month out of range), and "2025-02-29" while
// correctly accepting "2024-02-29". Verified empirically. The length switch exists to
// choose the layout and to reject anything that is not one of the four shapes -- for
// example "20250304", which no layout would otherwise catch as a wrong *shape*.
func ParsePeriod(s string) (Period, error) {
	switch len(s) {
	case len(PeriodAll):
		if s == string(PeriodAll) {
			return PeriodAll, nil
		}
	case len(layoutYear):
		if _, err := time.Parse(layoutYear, s); err == nil {
			return Period(s), nil
		}
	case len(layoutMonth):
		if _, err := time.Parse(layoutMonth, s); err == nil {
			return Period(s), nil
		}
	case len(layoutDay):
		if _, err := time.Parse(layoutDay, s); err == nil {
			return Period(s), nil
		}
	}
	return "", fmt.Errorf("%w: %q (want ALL, YYYY, YYYY-MM or YYYY-MM-DD)", ErrInvalidPeriod, s)
}

// MustPeriod is ParsePeriod for tests and constant literals. It panics on invalid input.
func MustPeriod(s string) Period {
	p, err := ParsePeriod(s)
	if err != nil {
		panic(err)
	}
	return p
}

// Granularity reports the resolution of p. It returns GranularityAll for both the
// all-time period and any malformed value, so callers that skipped ParsePeriod
// degrade predictably; use Valid to distinguish.
func (p Period) Granularity() Granularity {
	switch len(p) {
	case len(layoutYear):
		return GranularityYear
	case len(layoutMonth):
		return GranularityMonth
	case len(layoutDay):
		return GranularityDay
	default:
		return GranularityAll
	}
}

// Valid reports whether p is one of the four accepted shapes.
func (p Period) Valid() bool {
	_, err := ParsePeriod(string(p))
	return err == nil
}

func (p Period) String() string { return string(p) }
