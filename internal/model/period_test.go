package model

import (
	"errors"
	"testing"
)

func TestParsePeriod(t *testing.T) {
	tests := []struct {
		in    string
		want  Period
		gran  Granularity
		valid bool
	}{
		// Accepted shapes.
		{"ALL", PeriodAll, GranularityAll, true},
		{"2025", "2025", GranularityYear, true},
		{"2025-03", "2025-03", GranularityMonth, true},
		{"2025-03-14", "2025-03-14", GranularityDay, true},
		{"2024-02-29", "2024-02-29", GranularityDay, true}, // leap year

		// time.Parse already rejects these; the tests pin that we rely on it.
		{"2025-3-4", "", 0, false},   // single-digit month and day
		{"2025-3-04", "", 0, false},  // single-digit month
		{"2025-13-01", "", 0, false}, // month out of range
		{"2025-00-01", "", 0, false}, // month zero
		{"2025-02-30", "", 0, false}, // day out of range
		{"2025-02-29", "", 0, false}, // not a leap year
		{"2025-01-32", "", 0, false},

		// Wrong shape entirely -- these are what the length switch catches.
		{"20250304", "", 0, false},
		{"2025-03-14T00:00:00Z", "", 0, false},
		{"all", "", 0, false}, // case-sensitive
		{"All", "", 0, false},
		{"", "", 0, false},
		{"25", "", 0, false},
		{"202", "", 0, false},
		{"abcd", "", 0, false},
		{"2025-ab", "", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParsePeriod(tc.in)
			if tc.valid != (err == nil) {
				t.Fatalf("ParsePeriod(%q) = (%q, %v), wantValid %v", tc.in, got, err, tc.valid)
			}
			if !tc.valid {
				if !errors.Is(err, ErrInvalidPeriod) {
					t.Errorf("error %v does not wrap ErrInvalidPeriod", err)
				}
				return
			}
			if got != tc.want {
				t.Errorf("ParsePeriod(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if g := got.Granularity(); g != tc.gran {
				t.Errorf("Granularity = %v, want %v", g, tc.gran)
			}
			if !got.Valid() {
				t.Errorf("Valid() = false for accepted period %q", got)
			}
		})
	}
}

func TestGranularityString(t *testing.T) {
	for _, tc := range []struct {
		g    Granularity
		want string
	}{
		{GranularityAll, "all"},
		{GranularityYear, "year"},
		{GranularityMonth, "month"},
		{GranularityDay, "day"},
	} {
		if got := tc.g.String(); got != tc.want {
			t.Errorf("Granularity(%d).String() = %q, want %q", uint8(tc.g), got, tc.want)
		}
	}
}

func TestMustPeriodPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustPeriod did not panic on invalid input")
		}
	}()
	MustPeriod("nope")
}
