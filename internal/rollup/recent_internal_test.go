package rollup

import (
	"testing"
	"time"

	"github.com/neovasili/spotistats/internal/model"
)

// The window arithmetic is unit-tested against a hand-built history rather than through a seeded
// table, because the cases that matter are calendar edges -- a short February, a Monday, a year
// boundary -- and reaching those through a fixed test clock is not possible.

var madrid = model.MustCalendar("Europe/Madrid")

// histOf builds a history with one play-minute on each named date, so a summed range equals the
// number of days in it and every assertion below reads as a day count.
func histOf(dates ...string) *history {
	h := &history{byYear: map[string]*PeriodValue{}}
	for _, d := range dates {
		h.days = append(h.days, DayValue{Date: d, Plays: 1, MsPlayed: 60_000})
	}
	return h
}

// days enumerates an inclusive date range, so a test can state "all of February" in one line.
func days(from, to string) []string {
	start, _ := time.Parse(isoDate, from)
	end, _ := time.Parse(isoDate, to)
	var out []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(isoDate))
	}
	return out
}

func at(date string, hour int) time.Time {
	d, err := time.ParseInLocation(isoDate, date, madrid.Location())
	if err != nil {
		panic(err)
	}
	return d.Add(time.Duration(hour) * time.Hour)
}

func TestMonthToDateCutsThePreviousMonthAtTheSameDay(t *testing.T) {
	// Mid-August: fifteen days of this month against the first fifteen of July.
	h := histOf(append(days("2026-07-01", "2026-07-31"), days("2026-08-01", "2026-08-31")...)...)
	got := monthToDate(madrid, h, at("2026-08-15", 13))

	if got.Period != "2026-08" {
		t.Errorf("period = %q, want 2026-08", got.Period)
	}
	if got.Elapsed != 15 {
		t.Errorf("elapsed = %d, want 15", got.Elapsed)
	}
	if got.Metrics.Plays != 15 {
		t.Errorf("this month = %d days, want 15", got.Metrics.Plays)
	}
	if got.PreviousToDate.Plays != 15 {
		t.Errorf("previous to date = %d days, want 15 (1--15 July)", got.PreviousToDate.Plays)
	}
}

func TestMonthToDateClampsToAShorterPreviousMonth(t *testing.T) {
	// The case a naive "same day of last month" gets wrong. On 31 March the current stretch is
	// 31 days and February has 28, so an unclamped cut would read three days INTO March and
	// compare the month against part of itself.
	h := histOf(append(days("2026-02-01", "2026-02-28"), days("2026-03-01", "2026-03-31")...)...)
	got := monthToDate(madrid, h, at("2026-03-31", 23))

	if got.Elapsed != 31 {
		t.Errorf("elapsed = %d, want 31", got.Elapsed)
	}
	if got.Metrics.Plays != 31 {
		t.Errorf("this month = %d days, want 31", got.Metrics.Plays)
	}
	if got.PreviousToDate.Plays != 28 {
		t.Errorf("previous to date = %d days, want 28 (all of February, clamped)", got.PreviousToDate.Plays)
	}
}

func TestMonthToDateOnTheFirstOfTheMonth(t *testing.T) {
	// One day against one day. The comparison is nearly meaningless this early and that is the
	// reader's problem, not the arithmetic's -- what matters is that it is not a whole month.
	h := histOf(append(days("2026-07-01", "2026-07-31"), "2026-08-01")...)
	got := monthToDate(madrid, h, at("2026-08-01", 9))

	if got.Elapsed != 1 {
		t.Errorf("elapsed = %d, want 1", got.Elapsed)
	}
	if got.Metrics.Plays != 1 || got.PreviousToDate.Plays != 1 {
		t.Errorf("got %d vs %d, want 1 vs 1", got.Metrics.Plays, got.PreviousToDate.Plays)
	}
}

func TestMonthToDateCrossesTheYearBoundary(t *testing.T) {
	h := histOf(append(days("2025-12-01", "2025-12-31"), days("2026-01-01", "2026-01-10")...)...)
	got := monthToDate(madrid, h, at("2026-01-10", 20))

	if got.Period != "2026-01" {
		t.Errorf("period = %q, want 2026-01", got.Period)
	}
	if got.Metrics.Plays != 10 {
		t.Errorf("this month = %d days, want 10", got.Metrics.Plays)
	}
	if got.PreviousToDate.Plays != 10 {
		t.Errorf("previous to date = %d days, want 10 (1--10 December)", got.PreviousToDate.Plays)
	}
}

func TestWeekToDateStartsOnMonday(t *testing.T) {
	// Thursday 27 August 2026: Monday the 24th through Thursday the 27th is four days, against
	// Monday the 17th through Thursday the 20th.
	h := histOf(days("2026-08-01", "2026-08-31")...)
	got := weekToDate(madrid, h, at("2026-08-27", 15))

	if got.Period != "2026-08-24" {
		t.Errorf("week start = %q, want 2026-08-24 (the Monday)", got.Period)
	}
	if got.Elapsed != 4 {
		t.Errorf("elapsed = %d, want 4", got.Elapsed)
	}
	if got.Metrics.Plays != 4 {
		t.Errorf("this week = %d days, want 4", got.Metrics.Plays)
	}
	if got.PreviousToDate.Plays != 4 {
		t.Errorf("previous to date = %d days, want 4", got.PreviousToDate.Plays)
	}
}

func TestWeekToDateOnAMondayIsOneDay(t *testing.T) {
	// The boundary a (weekday+6)%7 expression gets wrong if it is written as weekday-1.
	h := histOf(days("2026-08-01", "2026-08-31")...)
	got := weekToDate(madrid, h, at("2026-08-17", 8))

	if got.Period != "2026-08-17" {
		t.Errorf("week start = %q, want the same Monday", got.Period)
	}
	if got.Elapsed != 1 {
		t.Errorf("elapsed = %d, want 1", got.Elapsed)
	}
	if got.Metrics.Plays != 1 {
		t.Errorf("this week = %d days, want 1", got.Metrics.Plays)
	}
}

func TestWeekToDateOnASundayIsAWholeWeek(t *testing.T) {
	// The other boundary. Sunday is Go's weekday 0, so a naive "days since Monday = weekday - 1"
	// would give -1 here and start the week in the future.
	h := histOf(days("2026-08-01", "2026-08-31")...)
	got := weekToDate(madrid, h, at("2026-08-23", 22))

	if got.Period != "2026-08-17" {
		t.Errorf("week start = %q, want 2026-08-17 (the preceding Monday)", got.Period)
	}
	if got.Elapsed != 7 {
		t.Errorf("elapsed = %d, want 7", got.Elapsed)
	}
	if got.Metrics.Plays != 7 {
		t.Errorf("this week = %d days, want 7", got.Metrics.Plays)
	}
}

func TestWeekToDateCrossesTheYearBoundary(t *testing.T) {
	// Thursday 1 January 2026: the week began on Monday 29 December 2025.
	h := histOf(append(days("2025-12-15", "2025-12-31"), "2026-01-01")...)
	got := weekToDate(madrid, h, at("2026-01-01", 11))

	if got.Period != "2025-12-29" {
		t.Errorf("week start = %q, want 2025-12-29", got.Period)
	}
	if got.Elapsed != 4 {
		t.Errorf("elapsed = %d, want 4", got.Elapsed)
	}
	if got.Metrics.Plays != 4 {
		t.Errorf("this week = %d days, want 4 (29, 30, 31 Dec + 1 Jan)", got.Metrics.Plays)
	}
	if got.PreviousToDate.Plays != 4 {
		t.Errorf("previous to date = %d days, want 4 (22--25 Dec)", got.PreviousToDate.Plays)
	}
}

func TestWeekToDateSpansTheSpringForwardWithoutLosingADay(t *testing.T) {
	// Europe/Madrid springs forward on Sunday 29 March 2026. Adding 24-hour durations across
	// that boundary would land an hour off and shift a date; AddDate works on calendar days.
	h := histOf(days("2026-03-20", "2026-04-05")...)
	got := weekToDate(madrid, h, at("2026-03-31", 12))

	if got.Period != "2026-03-30" {
		t.Errorf("week start = %q, want 2026-03-30", got.Period)
	}
	if got.Elapsed != 2 {
		t.Errorf("elapsed = %d, want 2", got.Elapsed)
	}
	if got.PreviousToDate.Plays != 2 {
		t.Errorf("previous to date = %d days, want 2 (23--24 March)", got.PreviousToDate.Plays)
	}
}

func TestSilentDaysCountAsZeroRatherThanAsMissing(t *testing.T) {
	// A day row exists only for a day with plays, so an absent day is a real zero. The window
	// must not skip it and shift the comparison onto a different stretch of the calendar.
	h := histOf("2026-08-24", "2026-08-27") // Tuesday to Wednesday silent
	got := weekToDate(madrid, h, at("2026-08-27", 15))

	if got.Elapsed != 4 {
		t.Errorf("elapsed = %d, want 4", got.Elapsed)
	}
	if got.Metrics.Plays != 2 {
		t.Errorf("this week = %d plays, want 2 across a four-day window", got.Metrics.Plays)
	}
}

func TestBetweenIsInclusiveAtBothEnds(t *testing.T) {
	h := histOf(days("2026-08-01", "2026-08-10")...)

	if got := h.Between("2026-08-03", "2026-08-05").Plays; got != 3 {
		t.Errorf("3rd to 5th = %d, want 3", got)
	}
	if got := h.Between("2026-08-04", "2026-08-04").Plays; got != 1 {
		t.Errorf("a single day = %d, want 1", got)
	}
	// An inverted or empty range is zero rather than a panic or a full-history sum.
	if got := h.Between("2026-08-05", "2026-08-03").Plays; got != 0 {
		t.Errorf("inverted range = %d, want 0", got)
	}
	if got := h.Between("", "2026-08-03").Plays; got != 0 {
		t.Errorf("empty bound = %d, want 0", got)
	}
}
