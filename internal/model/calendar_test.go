package model

import (
	"errors"
	"testing"
	"time"
)

// TestNewCalendarLoadsEmbeddedTZData proves the `_ "time/tzdata"` import in tzdata.go
// is doing its job. Without it this still passes on a dev machine (which has system
// zoneinfo) but fails on the provided.al2023 Lambda runtime, which ships none -- so the
// test is a canary for the import being deleted, not a full substitute for it.
func TestNewCalendarLoadsEmbeddedTZData(t *testing.T) {
	for _, zone := range []string{"Europe/Madrid", "America/Havana", "UTC", "Australia/Lord_Howe"} {
		c, err := NewCalendar(zone)
		if err != nil {
			t.Fatalf("NewCalendar(%q): %v (is the time/tzdata import still present?)", zone, err)
		}
		if c.Name() != zone {
			t.Errorf("Name() = %q, want %q", c.Name(), zone)
		}
	}
	if _, err := NewCalendar("Not/AZone"); err == nil {
		t.Error("NewCalendar accepted a bogus zone")
	}
	c, err := NewCalendar("")
	if err != nil || c.Location() != time.UTC {
		t.Errorf(`NewCalendar("") = (%v, %v), want UTC`, c.Location(), err)
	}
}

// TestCalendarPeriodKeysAreLocal is the core of the timezone model: period keys are
// derived in the local zone even though the instant is stored as UTC.
func TestCalendarPeriodKeysAreLocal(t *testing.T) {
	madrid := MustCalendar("Europe/Madrid")
	utc := MustCalendar("UTC")

	tests := []struct {
		name                         string
		instant                      string
		wantYear, wantMonth, wantDay Period
		wantUTCYear, wantUTCMonth    Period
		wantUTCDay                   Period
	}{
		{
			// The decisive case: 30 minutes before midnight UTC on New Year's Eve is
			// already 2026 in Madrid. Deriving in UTC would file this play under 2025.
			name:     "year boundary CET (+1)",
			instant:  "2025-12-31T23:30:00.000Z",
			wantYear: "2026", wantMonth: "2026-01", wantDay: "2026-01-01",
			wantUTCYear: "2025", wantUTCMonth: "2025-12", wantUTCDay: "2025-12-31",
		},
		{
			name:     "month boundary CET (+1)",
			instant:  "2025-02-28T23:30:00.000Z",
			wantYear: "2025", wantMonth: "2025-03", wantDay: "2025-03-01",
			wantUTCYear: "2025", wantUTCMonth: "2025-02", wantUTCDay: "2025-02-28",
		},
		{
			// In summer Madrid is UTC+2, so the boundary moves an hour earlier.
			name:     "day boundary CEST (+2)",
			instant:  "2025-06-14T22:30:00.000Z",
			wantYear: "2025", wantMonth: "2025-06", wantDay: "2025-06-15",
			wantUTCYear: "2025", wantUTCMonth: "2025-06", wantUTCDay: "2025-06-14",
		},
		{
			name:     "well inside a day is unaffected",
			instant:  "2025-03-14T12:00:00.000Z",
			wantYear: "2025", wantMonth: "2025-03", wantDay: "2025-03-14",
			wantUTCYear: "2025", wantUTCMonth: "2025-03", wantUTCDay: "2025-03-14",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts, err := ParseTS(tc.instant)
			if err != nil {
				t.Fatalf("ParseTS: %v", err)
			}
			if got := madrid.Year(ts); got != tc.wantYear {
				t.Errorf("madrid.Year = %q, want %q", got, tc.wantYear)
			}
			if got := madrid.Month(ts); got != tc.wantMonth {
				t.Errorf("madrid.Month = %q, want %q", got, tc.wantMonth)
			}
			if got := madrid.Day(ts); got != tc.wantDay {
				t.Errorf("madrid.Day = %q, want %q", got, tc.wantDay)
			}
			// The same instant under a UTC calendar must differ -- proof the zone is
			// genuinely injected rather than hardcoded.
			if got := utc.Year(ts); got != tc.wantUTCYear {
				t.Errorf("utc.Year = %q, want %q", got, tc.wantUTCYear)
			}
			if got := utc.Month(ts); got != tc.wantUTCMonth {
				t.Errorf("utc.Month = %q, want %q", got, tc.wantUTCMonth)
			}
			if got := utc.Day(ts); got != tc.wantUTCDay {
				t.Errorf("utc.Day = %q, want %q", got, tc.wantUTCDay)
			}
		})
	}
}

// TestCalendarDSTSpringForward: Madrid skips 02:00->03:00 on the last Sunday of March,
// so the local day has 23 hours and local hour 2 does not exist. Period keys are
// unaffected; the hour histogram legitimately shows zero plays in hour 2 that day.
func TestCalendarDSTSpringForward(t *testing.T) {
	madrid := MustCalendar("Europe/Madrid")
	for _, tc := range []struct {
		instant  string
		wantDay  Period
		wantHour int
	}{
		{"2025-03-30T00:30:00.000Z", "2025-03-30", 1}, // CET, +1
		{"2025-03-30T01:30:00.000Z", "2025-03-30", 3}, // CEST, +2 -- hour 2 skipped
	} {
		ts, err := ParseTS(tc.instant)
		if err != nil {
			t.Fatal(err)
		}
		if got := madrid.Day(ts); got != tc.wantDay {
			t.Errorf("%s: Day = %q, want %q", tc.instant, got, tc.wantDay)
		}
		if got := madrid.HourOfDay(ts); got != tc.wantHour {
			t.Errorf("%s: HourOfDay = %d, want %d", tc.instant, got, tc.wantHour)
		}
	}
}

// TestCalendarDSTFallBack: Madrid repeats 02:00-02:59 on the last Sunday of October.
// Two distinct UTC instants both map to local hour 2 on the same day. This is correct
// and unambiguous precisely because we always start from a UTC instant -- the UTC to
// local mapping is total, only the reverse direction is ambiguous.
func TestCalendarDSTFallBack(t *testing.T) {
	madrid := MustCalendar("Europe/Madrid")
	for _, instant := range []string{"2025-10-26T00:30:00.000Z", "2025-10-26T01:30:00.000Z"} {
		ts, err := ParseTS(instant)
		if err != nil {
			t.Fatal(err)
		}
		if got := madrid.Day(ts); got != "2025-10-26" {
			t.Errorf("%s: Day = %q, want 2025-10-26", instant, got)
		}
		if got := madrid.HourOfDay(ts); got != 2 {
			t.Errorf("%s: HourOfDay = %d, want 2 (the repeated hour)", instant, got)
		}
	}
}

// TestCalendarBoundsRoundTrip is the property that guards firstInstantOf.
//
// America/Havana is in the zone list deliberately: Cuba starts DST at 00:00 local, so
// local midnight does not exist on that date and time.Date returns the PREVIOUS
// calendar day. A naive Bounds implementation passes for Europe/Madrid (which
// transitions at 02:00/03:00) and fails here.
func TestCalendarBoundsRoundTrip(t *testing.T) {
	zones := []string{"UTC", "Europe/Madrid", "America/Havana", "Australia/Lord_Howe"}
	periods := []Period{
		"2025", "2024", // years, incl. a leap year
		"2025-01", "2025-03", "2025-10", "2025-12", "2024-02",
		"2025-01-01", "2025-03-09", "2025-03-30", "2025-10-26", "2025-11-02",
		"2025-12-31", "2024-02-29", "2025-06-15",
	}

	for _, zone := range zones {
		c := MustCalendar(zone)
		for _, p := range periods {
			t.Run(zone+"/"+string(p), func(t *testing.T) {
				start, end, err := c.Bounds(p)
				if err != nil {
					t.Fatalf("Bounds(%q): %v", p, err)
				}
				if !start.Before(end) {
					t.Fatalf("start %v is not before end %v", start, end)
				}

				key := func(ts time.Time) Period {
					switch p.Granularity() {
					case GranularityYear:
						return c.Year(ts)
					case GranularityMonth:
						return c.Month(ts)
					default:
						return c.Day(ts)
					}
				}

				// The first instant is inside the period.
				if got := key(start); got != p {
					t.Errorf("key(start=%s) = %q, want %q", FormatTS(start), got, p)
				}
				// The instant just before it is NOT -- i.e. start really is the boundary.
				if got := key(start.Add(-time.Millisecond)); got == p {
					t.Errorf("key(start-1ms=%s) = %q, but start is not the first instant",
						FormatTS(start.Add(-time.Millisecond)), got)
				}
				// The last instant is inside, and end itself is outside (half-open).
				if got := key(end.Add(-time.Millisecond)); got != p {
					t.Errorf("key(end-1ms=%s) = %q, want %q", FormatTS(end.Add(-time.Millisecond)), got, p)
				}
				if got := key(end); got == p {
					t.Errorf("key(end=%s) = %q, but end must be exclusive", FormatTS(end), got)
				}
			})
		}
	}
}

// TestCalendarBoundsHavanaMidnightGap pins the exact instant the property test protects,
// so a regression reports the real value rather than just "property violated".
func TestCalendarBoundsHavanaMidnightGap(t *testing.T) {
	c := MustCalendar("America/Havana")
	start, _, err := c.Bounds("2025-03-09")
	if err != nil {
		t.Fatal(err)
	}
	// Local midnight does not exist; the day truly begins at 01:00 -0400 = 05:00Z.
	if want := "2025-03-09T05:00:00.000Z"; FormatTS(start) != want {
		t.Errorf("Bounds start = %s, want %s (naive time.Date yields 2025-03-08T23:00 -0500)",
			FormatTS(start), want)
	}
}

func TestCalendarBoundsDayLengthAcrossDST(t *testing.T) {
	madrid := MustCalendar("Europe/Madrid")
	for _, tc := range []struct {
		day  Period
		want time.Duration
	}{
		{"2025-03-30", 23 * time.Hour}, // spring forward
		{"2025-10-26", 25 * time.Hour}, // fall back
		{"2025-06-15", 24 * time.Hour},
	} {
		start, end, err := madrid.Bounds(tc.day)
		if err != nil {
			t.Fatal(err)
		}
		if got := end.Sub(start); got != tc.want {
			t.Errorf("%s length = %v, want %v", tc.day, got, tc.want)
		}
	}
}

func TestCalendarBoundsUnbounded(t *testing.T) {
	c := MustCalendar("Europe/Madrid")
	if _, _, err := c.Bounds(PeriodAll); !errors.Is(err, ErrUnboundedPeriod) {
		t.Errorf("Bounds(ALL) err = %v, want ErrUnboundedPeriod", err)
	}
	if _, _, err := c.Bounds("nope"); !errors.Is(err, ErrInvalidPeriod) {
		t.Errorf("Bounds(invalid) err = %v, want ErrInvalidPeriod", err)
	}
}

func TestCalendarWeekday(t *testing.T) {
	madrid := MustCalendar("Europe/Madrid")
	// 2025-06-14T22:30Z is 2025-06-15T00:30 CEST -- a Sunday locally, Saturday in UTC.
	ts, err := ParseTS("2025-06-14T22:30:00.000Z")
	if err != nil {
		t.Fatal(err)
	}
	if got := madrid.Weekday(ts); got != time.Sunday {
		t.Errorf("madrid.Weekday = %v, want Sunday", got)
	}
	if got := MustCalendar("UTC").Weekday(ts); got != time.Saturday {
		t.Errorf("utc.Weekday = %v, want Saturday", got)
	}
}

// The zero Calendar must not panic; it behaves as UTC.
func TestZeroCalendarIsUTC(t *testing.T) {
	var c Calendar
	if c.Location() != time.UTC {
		t.Errorf("zero Calendar Location = %v, want UTC", c.Location())
	}
	if c.Name() != "UTC" {
		t.Errorf("zero Calendar Name = %q, want UTC", c.Name())
	}
	ts := time.Date(2025, 3, 14, 12, 0, 0, 0, time.UTC)
	if got := c.Day(ts); got != "2025-03-14" {
		t.Errorf("zero Calendar Day = %q", got)
	}
}
