package model

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

var madrid = MustCalendar("Europe/Madrid")

func mustTS(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := ParseTS(s)
	if err != nil {
		t.Fatalf("ParseTS(%q): %v", s, err)
	}
	return ts
}

// keyStrings renders deltas as "PK | SK" in order, for readable diffs.
func keyStrings(ds []AggDelta) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Key.PK()+" | "+d.Key.SK())
	}
	return out
}

// expectedLen is the documented size contract of AggregateDeltas.
func expectedLen(f PlayFacts) int {
	n := 4 + 3 + 3*len(f.ArtistIDs) + 3*len(f.Genres)
	if f.AlbumID != "" {
		n += 3
	}
	return n
}

// assertDeltaInvariants checks every property that must hold for the deltas of a single
// play, so each table case below gets these ten assertions for free.
func assertDeltaInvariants(t *testing.T, ds []AggDelta, f PlayFacts) {
	t.Helper()

	// 1. No duplicate keys -- a duplicate would double-count on write.
	seen := map[AggKey]int{}
	for i, d := range ds {
		if prev, dup := seen[d.Key]; dup {
			t.Errorf("invariant 1: duplicate key %s at indices %d and %d", d.Key, prev, i)
		}
		seen[d.Key] = i
	}

	// 9. Exact size contract.
	if got, want := len(ds), expectedLen(f); got != want {
		t.Errorf("invariant 9: len = %d, want %d\nkeys:\n  %s",
			got, want, strings.Join(keyStrings(ds), "\n  "))
	}

	dayRows := 0
	for _, d := range ds {
		// 2. One play contributes exactly one play to every row it touches.
		if d.Plays != 1 {
			t.Errorf("invariant 2: %s Plays = %d, want 1", d.Key, d.Plays)
		}
		// 3. Every row gets the full duration.
		if d.MsPlayed != f.MsPlayed {
			t.Errorf("invariant 3: %s MsPlayed = %d, want %d", d.Key, d.MsPlayed, f.MsPlayed)
		}
		// 4. The fidelity split.
		if f.Estimated {
			if d.MsPlayedExact != 0 || d.PlaysExact != 0 {
				t.Errorf("invariant 4: %s estimated play leaked into exact counters (ms=%d plays=%d)",
					d.Key, d.MsPlayedExact, d.PlaysExact)
			}
		} else {
			if d.MsPlayedExact != f.MsPlayed || d.PlaysExact != 1 {
				t.Errorf("invariant 4: %s exact play not fully counted (ms=%d want %d, plays=%d want 1)",
					d.Key, d.MsPlayedExact, f.MsPlayed, d.PlaysExact)
			}
		}
		// 5. The subset relationship that makes EstimatedRatio meaningful.
		if d.MsPlayedExact > d.MsPlayed {
			t.Errorf("invariant 5: %s MsPlayedExact %d > MsPlayed %d", d.Key, d.MsPlayedExact, d.MsPlayed)
		}
		// 6. A single play's bounds are both its own timestamp.
		if !d.FirstPlayedAt.Equal(f.PlayedAt) || !d.LastPlayedAt.Equal(f.PlayedAt) {
			t.Errorf("invariant 6: %s bounds = (%v, %v), want both %v",
				d.Key, d.FirstPlayedAt, d.LastPlayedAt, f.PlayedAt)
		}
		// 7 & 8. Day granularity and the TOTAL entity id.
		if d.Key.Period.Granularity() == GranularityDay {
			dayRows++
			if d.Key.Dim != DimTotal {
				t.Errorf("invariant 7: %s has day granularity but dimension %q", d.Key, d.Key.Dim)
			}
		}
		if d.Key.Dim == DimTotal && d.Key.EntityID != TotalEntityID {
			t.Errorf("invariant 8: %s DimTotal entityID = %q, want %q", d.Key, d.Key.EntityID, TotalEntityID)
		}
		// 10. Keys are structurally valid and well-prefixed.
		if err := d.Key.Validate(); err != nil {
			t.Errorf("invariant 10: %s failed Validate: %v", d.Key, err)
		}
		if !strings.HasPrefix(d.Key.PK(), aggKeyPrefix) {
			t.Errorf("invariant 10: PK %q lacks prefix %q", d.Key.PK(), aggKeyPrefix)
		}
	}

	if dayRows != 1 {
		t.Errorf("invariant 7: %d day-granularity rows, want exactly 1 (DimTotal only)", dayRows)
	}
}

// ---------------------------------------------------------------------------
// AggregateDeltas
// ---------------------------------------------------------------------------

func TestAggregateDeltasKeysExplicit(t *testing.T) {
	f := PlayFacts{
		PlayedAt:  mustTS(t, "2025-03-14T21:04:33.123Z"), // Madrid 22:04 CET
		TrackID:   "t1",
		AlbumID:   "al1",
		ArtistIDs: []string{"ar1"},
		Genres:    []string{"symphonic metal"},
		MsPlayed:  231_000,
	}
	got := AggregateDeltas(f, madrid)
	assertDeltaInvariants(t, got, f)

	// The full expected key set, in the documented order:
	// TOTAL, TRACK, ALBUM, ARTIST, GENRE; within a dim ALL, year, month, day.
	want := []string{
		"AGG#TOTAL#ALL | ALL",
		"AGG#TOTAL#2025 | ALL",
		"AGG#TOTAL#2025-03 | ALL",
		"AGG#TOTAL#2025 | 2025-03-14", // day folded into the year partition
		"AGG#TRACK#ALL | t1",
		"AGG#TRACK#2025 | t1",
		"AGG#TRACK#2025-03 | t1",
		"AGG#ALBUM#ALL | al1",
		"AGG#ALBUM#2025 | al1",
		"AGG#ALBUM#2025-03 | al1",
		"AGG#ARTIST#ALL | ar1",
		"AGG#ARTIST#2025 | ar1",
		"AGG#ARTIST#2025-03 | ar1",
		"AGG#GENRE#ALL | symphonic metal",
		"AGG#GENRE#2025 | symphonic metal",
		"AGG#GENRE#2025-03 | symphonic metal",
	}
	if diff := cmp.Diff(want, keyStrings(got)); diff != "" {
		t.Errorf("keys mismatch (-want +got):\n%s", diff)
	}
}

// TestAggregateDeltasSpecWorkedExample pins the arithmetic docs/SPECS.md 5.2 states in
// prose: "a track with 2 artists and 3 genres costs 4 + 3 + 6 + 3 + 9 = 25 updates".
// If this number ever changes, the cost model in the spec is wrong too.
func TestAggregateDeltasSpecWorkedExample(t *testing.T) {
	f := PlayFacts{
		PlayedAt:  mustTS(t, "2025-03-14T21:04:33.123Z"),
		TrackID:   "t1",
		AlbumID:   "al1",
		ArtistIDs: []string{"ar1", "ar2"},
		Genres:    []string{"gothic metal", "symphonic metal", "dutch metal"},
		MsPlayed:  231_000,
	}
	got := AggregateDeltas(f, madrid)
	assertDeltaInvariants(t, got, f)
	if len(got) != 25 {
		t.Errorf("len = %d, want exactly 25 (docs/SPECS.md 5.2 worked example)", len(got))
	}
}

func TestAggregateDeltasShapes(t *testing.T) {
	at := mustTS(t, "2025-03-14T21:04:33.123Z")

	tests := []struct {
		name    string
		facts   PlayFacts
		wantLen int
		absent  []string // key-prefix substrings that must not appear
	}{
		{
			name: "no album",
			facts: PlayFacts{PlayedAt: at, TrackID: "t1", MsPlayed: 1000,
				ArtistIDs: []string{"ar1", "ar2"},
				Genres:    []string{"a", "b", "c"}},
			wantLen: 22,
			absent:  []string{"AGG#ALBUM#"},
		},
		{
			name: "no genres produces no synthetic bucket",
			facts: PlayFacts{PlayedAt: at, TrackID: "t1", AlbumID: "al1", MsPlayed: 1000,
				ArtistIDs: []string{"ar1"}},
			wantLen: 13,
			absent:  []string{"AGG#GENRE#"},
		},
		{
			name: "no artists",
			facts: PlayFacts{PlayedAt: at, TrackID: "t1", AlbumID: "al1", MsPlayed: 1000,
				Genres: []string{"a"}},
			wantLen: 13,
			absent:  []string{"AGG#ARTIST#"},
		},
		{
			name:    "track only",
			facts:   PlayFacts{PlayedAt: at, TrackID: "t1", MsPlayed: 1000},
			wantLen: 7,
			absent:  []string{"AGG#ALBUM#", "AGG#ARTIST#", "AGG#GENRE#"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AggregateDeltas(tc.facts, madrid)
			assertDeltaInvariants(t, got, tc.facts)
			if len(got) != tc.wantLen {
				t.Errorf("len = %d, want %d", len(got), tc.wantLen)
			}
			keys := strings.Join(keyStrings(got), "\n")
			for _, a := range tc.absent {
				if strings.Contains(keys, a) {
					t.Errorf("unexpected %q in:\n%s", a, keys)
				}
			}
		})
	}
}

// TestAggregateDeltasFidelity is the estimated-vs-exact contract from docs/SPECS.md 2.2
// and 6.4 -- the detail most likely to be silently wrong.
func TestAggregateDeltasFidelity(t *testing.T) {
	at := mustTS(t, "2025-03-14T21:04:33.123Z")

	t.Run("estimated contributes nothing exact", func(t *testing.T) {
		f := PlayFacts{PlayedAt: at, TrackID: "t1", MsPlayed: 231_000, Estimated: true}
		ds := AggregateDeltas(f, madrid)
		assertDeltaInvariants(t, ds, f)
		for _, d := range ds {
			if d.MsPlayed != 231_000 || d.MsPlayedExact != 0 || d.PlaysExact != 0 {
				t.Fatalf("%s = (ms %d, exact %d, playsExact %d), want (231000, 0, 0)",
					d.Key, d.MsPlayed, d.MsPlayedExact, d.PlaysExact)
			}
		}
	})

	t.Run("exact contributes fully", func(t *testing.T) {
		f := PlayFacts{PlayedAt: at, TrackID: "t1", MsPlayed: 187_433, Estimated: false}
		ds := AggregateDeltas(f, madrid)
		assertDeltaInvariants(t, ds, f)
		for _, d := range ds {
			if d.MsPlayed != 187_433 || d.MsPlayedExact != 187_433 || d.PlaysExact != 1 {
				t.Fatalf("%s = (ms %d, exact %d, playsExact %d), want (187433, 187433, 1)",
					d.Key, d.MsPlayed, d.MsPlayedExact, d.PlaysExact)
			}
		}
	})
}

// TestAggregateDeltasPeriodBoundaries is the proof of the timezone model: period keys
// come from the LOCAL calendar even though the instant is stored as UTC.
func TestAggregateDeltasPeriodBoundaries(t *testing.T) {
	tests := []struct {
		name                            string
		instant                         string
		cal                             Calendar
		wantYear, wantMonth, wantDayKey string
	}{
		{
			// 30 minutes before midnight UTC on New Year's Eve is already 2026 in Madrid.
			name:    "year boundary rolls into the next year locally",
			instant: "2025-12-31T23:30:00.000Z", cal: madrid,
			wantYear: "2026", wantMonth: "2026-01", wantDayKey: "2026-01-01",
		},
		{
			name:    "the same instant under UTC stays in 2025",
			instant: "2025-12-31T23:30:00.000Z", cal: MustCalendar("UTC"),
			wantYear: "2025", wantMonth: "2025-12", wantDayKey: "2025-12-31",
		},
		{
			name: "month boundary", instant: "2025-02-28T23:30:00.000Z", cal: madrid,
			wantYear: "2025", wantMonth: "2025-03", wantDayKey: "2025-03-01",
		},
		{
			name: "day boundary in CEST is an hour earlier", instant: "2025-06-14T22:30:00.000Z", cal: madrid,
			wantYear: "2025", wantMonth: "2025-06", wantDayKey: "2025-06-15",
		},
		{
			name: "DST spring forward", instant: "2025-03-30T01:30:00.000Z", cal: madrid,
			wantYear: "2025", wantMonth: "2025-03", wantDayKey: "2025-03-30",
		},
		{
			name: "DST fall back", instant: "2025-10-26T01:30:00.000Z", cal: madrid,
			wantYear: "2025", wantMonth: "2025-10", wantDayKey: "2025-10-26",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := PlayFacts{PlayedAt: mustTS(t, tc.instant), TrackID: "t1", MsPlayed: 1000}
			ds := AggregateDeltas(f, tc.cal)
			assertDeltaInvariants(t, ds, f)

			// deltas[1] is TOTAL/year, [2] TOTAL/month, [3] TOTAL/day by contract.
			if got := string(ds[1].Key.Period); got != tc.wantYear {
				t.Errorf("year = %q, want %q", got, tc.wantYear)
			}
			if got := string(ds[2].Key.Period); got != tc.wantMonth {
				t.Errorf("month = %q, want %q", got, tc.wantMonth)
			}
			if got := string(ds[3].Key.Period); got != tc.wantDayKey {
				t.Errorf("day = %q, want %q", got, tc.wantDayKey)
			}
			// The day row must live in its local year's partition.
			if got, want := ds[3].Key.PK(), "AGG#TOTAL#"+tc.wantYear; got != want {
				t.Errorf("day row PK = %q, want %q", got, want)
			}
		})
	}
}

func TestAggregateDeltasDeterministic(t *testing.T) {
	f := PlayFacts{
		PlayedAt:  mustTS(t, "2025-03-14T21:04:33.123Z"),
		TrackID:   "t1",
		AlbumID:   "al1",
		ArtistIDs: []string{"ar2", "ar1"}, // first-seen order must be preserved, not sorted
		Genres:    []string{"a", "b"},
		MsPlayed:  1000,
	}
	first := AggregateDeltas(f, madrid)
	second := AggregateDeltas(f, madrid)
	if diff := cmp.Diff(first, second); diff != "" {
		t.Errorf("two calls differ (-first +second):\n%s", diff)
	}
	// Artist order is meaningful: the primary artist comes first.
	keys := keyStrings(first)
	iAr2, iAr1 := -1, -1
	for i, k := range keys {
		if k == "AGG#ARTIST#ALL | ar2" {
			iAr2 = i
		}
		if k == "AGG#ARTIST#ALL | ar1" {
			iAr1 = i
		}
	}
	if iAr2 == -1 || iAr1 == -1 || iAr2 > iAr1 {
		t.Errorf("artist first-seen order not preserved: ar2 at %d, ar1 at %d", iAr2, iAr1)
	}
}

func TestAggregateDeltasZeroPlayedAtDoesNotPanic(t *testing.T) {
	// Validate is the gate for bad plays; AggregateDeltas must still be total.
	f := PlayFacts{TrackID: "t1", MsPlayed: 1000}
	ds := AggregateDeltas(f, madrid)
	if len(ds) != 7 {
		t.Errorf("len = %d, want 7", len(ds))
	}
}

// ---------------------------------------------------------------------------
// FactsFor: genre dedup / normalisation
// ---------------------------------------------------------------------------

func TestFactsForNormalisesAndDedupes(t *testing.T) {
	at := mustTS(t, "2025-03-14T21:04:33.123Z")
	p := Play{
		PlayedAt: at, TrackID: "t1", AlbumID: "al1",
		ArtistIDs: []string{"ar1", "ar1", "ar2"},
		MsPlayed:  1000, Source: SourceExport,
	}
	// Two artists both tagged "gothic metal", in assorted casing and spacing.
	f := FactsFor(p, []string{"Gothic Metal", "  gothic   metal ", "GOTHIC METAL", "symphonic metal", ""})

	if diff := cmp.Diff([]string{"ar1", "ar2"}, f.ArtistIDs); diff != "" {
		t.Errorf("artists (-want +got):\n%s", diff)
	}
	// Deduplicated across artists and sorted.
	if diff := cmp.Diff([]string{"gothic metal", "symphonic metal"}, f.Genres); diff != "" {
		t.Errorf("genres (-want +got):\n%s", diff)
	}
	if f.Estimated {
		t.Error("Estimated = true for an export play")
	}
}

func TestNormalizeGenre(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Gothic Metal", "gothic metal"},
		{"  gothic   metal  ", "gothic metal"},
		{"GOTHIC\tMETAL", "gothic metal"},
		{"symphonic-metal", "symphonic-metal"}, // not slugified, hyphens preserved
		{"", ""},
		{"   ", ""},
	} {
		if got := NormalizeGenre(tc.in); got != tc.want {
			t.Errorf("NormalizeGenre(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// MergeDeltas
// ---------------------------------------------------------------------------

func TestMergeDeltas(t *testing.T) {
	early := mustTS(t, "2025-03-11T10:00:00.000Z")
	late := mustTS(t, "2025-03-14T21:04:33.123Z")

	a := PlayFacts{PlayedAt: early, TrackID: "t1", MsPlayed: 1000, Estimated: true}
	b := PlayFacts{PlayedAt: late, TrackID: "t1", MsPlayed: 2000, Estimated: false}

	merged := MergeDeltas(append(AggregateDeltas(a, madrid), AggregateDeltas(b, madrid)...))

	// Both plays fall in March 2025 locally, so all-time/year/month rows collapse; the
	// two day rows stay separate.
	byKey := map[string]AggDelta{}
	for _, d := range merged {
		byKey[d.Key.String()] = d
	}

	total := byKey["AGG#TOTAL#ALL / ALL"]
	if total.Plays != 2 {
		t.Errorf("TOTAL/ALL Plays = %d, want 2", total.Plays)
	}
	if total.MsPlayed != 3000 {
		t.Errorf("TOTAL/ALL MsPlayed = %d, want 3000", total.MsPlayed)
	}
	// Only the export play is exact.
	if total.PlaysExact != 1 || total.MsPlayedExact != 2000 {
		t.Errorf("TOTAL/ALL exact = (%d plays, %d ms), want (1, 2000)", total.PlaysExact, total.MsPlayedExact)
	}
	if !total.FirstPlayedAt.Equal(early) {
		t.Errorf("FirstPlayedAt = %v, want the minimum %v", total.FirstPlayedAt, early)
	}
	if !total.LastPlayedAt.Equal(late) {
		t.Errorf("LastPlayedAt = %v, want the maximum %v", total.LastPlayedAt, late)
	}

	if _, ok := byKey["AGG#TOTAL#2025 / 2025-03-11"]; !ok {
		t.Error("day row for 2025-03-11 missing")
	}
	if _, ok := byKey["AGG#TOTAL#2025 / 2025-03-14"]; !ok {
		t.Error("day row for 2025-03-14 missing")
	}
}

func TestMergeDeltasOrderIsFirstAppearance(t *testing.T) {
	f := PlayFacts{PlayedAt: mustTS(t, "2025-03-14T21:04:33.123Z"), TrackID: "t1", MsPlayed: 1000}
	ds := AggregateDeltas(f, madrid)
	merged := MergeDeltas(append(append([]AggDelta{}, ds...), ds...))
	if diff := cmp.Diff(keyStrings(ds), keyStrings(merged)); diff != "" {
		t.Errorf("merged key order changed (-want +got):\n%s", diff)
	}
	for _, d := range merged {
		if d.Plays != 2 {
			t.Errorf("%s Plays = %d, want 2", d.Key, d.Plays)
		}
	}
}

func TestMergeDeltasShortInputs(t *testing.T) {
	if got := MergeDeltas(nil); got != nil {
		t.Errorf("MergeDeltas(nil) = %v, want nil", got)
	}
	one := []AggDelta{{Key: AggKey{Dim: DimTrack, Period: "2025", EntityID: "t1"}, Plays: 1}}
	if diff := cmp.Diff(one, MergeDeltas(one)); diff != "" {
		t.Errorf("single delta changed:\n%s", diff)
	}
}

// ---------------------------------------------------------------------------
// AggKey
// ---------------------------------------------------------------------------

func TestAggKeyPKSKAndRoundTrip(t *testing.T) {
	tests := []struct {
		key    AggKey
		wantPK string
		wantSK string
	}{
		{AggKey{DimTotal, PeriodAll, TotalEntityID}, "AGG#TOTAL#ALL", "ALL"},
		{AggKey{DimTotal, "2025", TotalEntityID}, "AGG#TOTAL#2025", "ALL"},
		{AggKey{DimTotal, "2025-03", TotalEntityID}, "AGG#TOTAL#2025-03", "ALL"},
		// The exception: a day row lives in its year's partition.
		{AggKey{DimTotal, "2025-03-14", TotalEntityID}, "AGG#TOTAL#2025", "2025-03-14"},
		{AggKey{DimTrack, "2025", "t1"}, "AGG#TRACK#2025", "t1"},
		{AggKey{DimArtist, PeriodAll, "ar1"}, "AGG#ARTIST#ALL", "ar1"},
		{AggKey{DimAlbum, "2025-03", "al1"}, "AGG#ALBUM#2025-03", "al1"},
		{AggKey{DimGenre, "2025", "gothic metal"}, "AGG#GENRE#2025", "gothic metal"},
	}
	for _, tc := range tests {
		t.Run(tc.wantPK+"/"+tc.wantSK, func(t *testing.T) {
			if got := tc.key.PK(); got != tc.wantPK {
				t.Errorf("PK = %q, want %q", got, tc.wantPK)
			}
			if got := tc.key.SK(); got != tc.wantSK {
				t.Errorf("SK = %q, want %q", got, tc.wantSK)
			}
			back, err := ParseAggKey(tc.wantPK, tc.wantSK)
			if err != nil {
				t.Fatalf("ParseAggKey(%q, %q): %v", tc.wantPK, tc.wantSK, err)
			}
			if back != tc.key {
				t.Errorf("round trip = %+v, want %+v", back, tc.key)
			}
		})
	}
}

func TestParseAggKeyRejects(t *testing.T) {
	tests := []struct{ name, pk, sk string }{
		{"missing prefix", "TOTAL#2025", "ALL"},
		{"no dim separator", "AGG#TOTAL2025", "ALL"},
		{"unknown dim", "AGG#PLAYLIST#2025", "x"},
		{"bad period", "AGG#TRACK#2025-3", "t1"},
		{"empty sk for entity dim", "AGG#TRACK#2025", ""},
		{"total sk neither ALL nor a day", "AGG#TOTAL#2025", "nonsense"},
		{"total day row in a month partition", "AGG#TOTAL#2025-03", "2025-03-14"},
		{"total day row in the wrong year", "AGG#TOTAL#2024", "2025-03-14"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseAggKey(tc.pk, tc.sk); !errors.Is(err, ErrInvalidAggKey) {
				t.Errorf("ParseAggKey(%q, %q) err = %v, want ErrInvalidAggKey", tc.pk, tc.sk, err)
			}
		})
	}
}

func TestAggKeyValidate(t *testing.T) {
	tests := []struct {
		name    string
		key     AggKey
		wantErr bool
	}{
		{"total all", AggKey{DimTotal, PeriodAll, TotalEntityID}, false},
		{"total day is allowed", AggKey{DimTotal, "2025-03-14", TotalEntityID}, false},
		{"track month", AggKey{DimTrack, "2025-03", "t1"}, false},
		{"total with a real entity id", AggKey{DimTotal, "2025", "t1"}, true},
		{"entity dim with empty id", AggKey{DimTrack, "2025", ""}, true},
		{"unknown dim", AggKey{Dim("NOPE"), "2025", "x"}, true},
		{"invalid period", AggKey{DimTrack, "2025-3", "t1"}, true},
		// The asymmetry from docs/SPECS.md 5.2: day rows exist only for TOTAL.
		{"track at day granularity", AggKey{DimTrack, "2025-03-14", "t1"}, true},
		{"artist at day granularity", AggKey{DimArtist, "2025-03-14", "ar1"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.key.Validate()
			if tc.wantErr != (err != nil) {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestEstimatedRatio(t *testing.T) {
	tests := []struct {
		name      string
		ms, exact int64
		want      float64
	}{
		// docs/SPECS.md 6.4's own example.
		{"spec example", 8_420_000, 6_110_000, 0.2744},
		{"all exact", 1000, 1000, 0},
		{"all estimated", 1000, 0, 1},
		{"no listening time", 0, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := Aggregate{MsPlayed: tc.ms, MsPlayedExact: tc.exact}
			got := a.EstimatedRatio()
			if diff := got - tc.want; diff > 0.001 || diff < -0.001 {
				t.Errorf("EstimatedRatio = %v, want ~%v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Fuzz
// ---------------------------------------------------------------------------

func FuzzAggregateDeltas(f *testing.F) {
	f.Add(int64(1_700_000_000_000), uint8(2), uint8(3), int64(231_000), true, true)
	f.Add(int64(0), uint8(0), uint8(0), int64(1), false, false)
	f.Add(int64(-6_000_000_000_000), uint8(7), uint8(11), int64(999_999), false, true)

	f.Fuzz(func(t *testing.T, ms int64, nArtists, nGenres uint8, msPlayed int64, estimated, hasAlbum bool) {
		// Keep the generated shape bounded but still varied.
		nArtists %= 12
		nGenres %= 12
		if msPlayed < 1 {
			msPlayed = 1
		}
		// Unix millis far outside the representable range are not interesting.
		if ms > 4_000_000_000_000 || ms < -4_000_000_000_000 {
			ms = 0
		}

		facts := PlayFacts{
			PlayedAt:  FromUnixMillis(ms),
			TrackID:   "t1",
			MsPlayed:  msPlayed,
			Estimated: estimated,
		}
		if hasAlbum {
			facts.AlbumID = "al1"
		}
		for i := 0; i < int(nArtists); i++ {
			facts.ArtistIDs = append(facts.ArtistIDs, fmt.Sprintf("ar%d", i))
		}
		for i := 0; i < int(nGenres); i++ {
			facts.Genres = append(facts.Genres, fmt.Sprintf("genre %d", i))
		}

		ds := AggregateDeltas(facts, madrid)

		if got, want := len(ds), expectedLen(facts); got != want {
			t.Fatalf("len = %d, want %d", got, want)
		}
		seen := map[AggKey]struct{}{}
		days := 0
		for _, d := range ds {
			if _, dup := seen[d.Key]; dup {
				t.Fatalf("duplicate key %s", d.Key)
			}
			seen[d.Key] = struct{}{}
			if d.MsPlayedExact > d.MsPlayed {
				t.Fatalf("%s exact %d > total %d", d.Key, d.MsPlayedExact, d.MsPlayed)
			}
			if err := d.Key.Validate(); err != nil {
				t.Fatalf("%s invalid: %v", d.Key, err)
			}
			if d.Key.Period.Granularity() == GranularityDay {
				days++
			}
			// The key must survive a storage round trip.
			back, err := ParseAggKey(d.Key.PK(), d.Key.SK())
			if err != nil {
				t.Fatalf("ParseAggKey(%q,%q): %v", d.Key.PK(), d.Key.SK(), err)
			}
			if back != d.Key {
				t.Fatalf("round trip %+v != %+v", back, d.Key)
			}
		}
		if days != 1 {
			t.Fatalf("%d day rows, want 1", days)
		}
	})
}
