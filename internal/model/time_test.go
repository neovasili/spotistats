package model

import (
	"sort"
	"testing"
	"time"
)

// TestFormatTSFixedWidth is the regression guard for the reason TimestampFormat exists.
// Go's RFC3339Nano layout strips trailing zeros, so .120 renders as ".12Z" -- variable
// width, which breaks the lexical ordering DynamoDB relies on for sort keys.
func TestFormatTSFixedWidth(t *testing.T) {
	tests := []struct {
		name string
		ns   int
		want string
	}{
		{"trailing zero is kept", 120_000_000, "2025-03-14T21:04:33.120Z"},
		{"two trailing zeros are kept", 900_000_000, "2025-03-14T21:04:33.900Z"},
		{"leading zero is kept", 45_000_000, "2025-03-14T21:04:33.045Z"},
		{"two leading zeros are kept", 5_000_000, "2025-03-14T21:04:33.005Z"},
		{"no fraction renders as .000", 0, "2025-03-14T21:04:33.000Z"},
		{"sub-millisecond is truncated", 123_456_789, "2025-03-14T21:04:33.123Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatTS(time.Date(2025, 3, 14, 21, 4, 33, tc.ns, time.UTC))
			if got != tc.want {
				t.Errorf("FormatTS = %q, want %q", got, tc.want)
			}
			if len(got) != len("2025-03-14T21:04:33.000Z") {
				t.Errorf("FormatTS produced width %d, want fixed width %d: %q",
					len(got), len("2025-03-14T21:04:33.000Z"), got)
			}
		})
	}
}

// TestFormatTSLexicalOrderMatchesChronological is the property that actually matters:
// sorting the formatted strings must give the same order as sorting the instants.
// RFC3339Nano fails this -- ".123Z" sorts before ".12Z" even though .120 < .123.
func TestFormatTSLexicalOrderMatchesChronological(t *testing.T) {
	base := time.Date(2025, 3, 14, 21, 4, 33, 0, time.UTC)
	millis := []int{0, 5, 45, 120, 123, 200, 900, 999}

	instants := make([]time.Time, 0, len(millis))
	for _, ms := range millis {
		instants = append(instants, base.Add(time.Duration(ms)*time.Millisecond))
	}

	formatted := make([]string, 0, len(instants))
	for _, ts := range instants {
		formatted = append(formatted, FormatTS(ts))
	}

	shuffled := append([]string(nil), formatted...)
	sort.Strings(shuffled)

	for i := range formatted {
		if shuffled[i] != formatted[i] {
			t.Fatalf("lexical sort diverged from chronological order at index %d:\n"+
				" chronological: %v\n lexical:       %v", i, formatted, shuffled)
		}
	}
}

func TestFormatTSNormalisesToUTC(t *testing.T) {
	madrid := MustCalendar("Europe/Madrid").Location()
	// 2025-07-01T02:30 CEST (+02:00) is 00:30Z.
	got := FormatTS(time.Date(2025, 7, 1, 2, 30, 0, 0, madrid))
	if want := "2025-07-01T00:30:00.000Z"; got != want {
		t.Errorf("FormatTS = %q, want %q", got, want)
	}
}

func TestParseTSRoundTrip(t *testing.T) {
	want := time.Date(2025, 3, 14, 21, 4, 33, 123_000_000, time.UTC)
	got, err := ParseTS(FormatTS(want))
	if err != nil {
		t.Fatalf("ParseTS: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("round trip = %v, want %v", got, want)
	}
}

func TestParseSpotifyTS(t *testing.T) {
	tests := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"2025-03-14T21:04:33.123Z", time.Date(2025, 3, 14, 21, 4, 33, 123_000_000, time.UTC), true},
		{"2025-03-14T21:04:33Z", time.Date(2025, 3, 14, 21, 4, 33, 0, time.UTC), true},
		{"2025-03-14T21:04:33.12Z", time.Date(2025, 3, 14, 21, 4, 33, 120_000_000, time.UTC), true},
		{"not a timestamp", time.Time{}, false},
		{"", time.Time{}, false},
	}
	for _, tc := range tests {
		got, err := ParseSpotifyTS(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("ParseSpotifyTS(%q) err = %v, wantOK %v", tc.in, err, tc.ok)
			continue
		}
		if tc.ok && !got.Equal(tc.want) {
			t.Errorf("ParseSpotifyTS(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseExportTS(t *testing.T) {
	want := time.Date(2025, 3, 14, 21, 4, 33, 0, time.UTC)
	for _, in := range []string{"2025-03-14T21:04:33Z", "2025-03-14 21:04:33"} {
		got, err := ParseExportTS(in)
		if err != nil {
			t.Errorf("ParseExportTS(%q): %v", in, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("ParseExportTS(%q) = %v, want %v", in, got, want)
		}
	}
	// The older minute-precision form used by the basic (non-extended) export.
	got, err := ParseExportTS("2025-03-14 21:04")
	if err != nil {
		t.Fatalf("ParseExportTS minute form: %v", err)
	}
	if !got.Equal(time.Date(2025, 3, 14, 21, 4, 0, 0, time.UTC)) {
		t.Errorf("minute form = %v", got)
	}
}

func TestUnixMillisRoundTrip(t *testing.T) {
	want := time.Date(2025, 3, 14, 21, 4, 33, 123_000_000, time.UTC)
	if got := FromUnixMillis(UnixMillis(want)); !got.Equal(want) {
		t.Errorf("round trip = %v, want %v", got, want)
	}
}
