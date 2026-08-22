package store

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/google/go-cmp/cmp"
	"github.com/neovasili/spotistats/internal/model"
)

func mustTS(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := model.ParseTS(s)
	if err != nil {
		t.Fatalf("ParseTS(%q): %v", s, err)
	}
	return ts
}

// TestPlayPartitionIsUTC pins the deliberate asymmetry: the storage partition is UTC even
// though aggregate period keys are local. A local partition would strand every existing
// row if the configured timezone ever changed.
func TestPlayPartitionIsUTC(t *testing.T) {
	tests := []struct {
		instant string
		want    string
		note    string
	}{
		{"2025-03-14T21:04:33.123Z", "PLAY#2025-03", "mid-month"},
		{"2025-02-28T23:30:00.000Z", "PLAY#2025-02", "Madrid says March, UTC says February"},
		{"2025-12-31T23:30:00.000Z", "PLAY#2025-12", "Madrid says 2026-01, UTC says 2025-12"},
		{"2025-01-01T00:00:00.000Z", "PLAY#2025-01", "exact month start"},
	}
	for _, tc := range tests {
		t.Run(tc.note, func(t *testing.T) {
			if got := PlayPartition(mustTS(t, tc.instant)); got != tc.want {
				t.Errorf("PlayPartition(%s) = %q, want %q", tc.instant, got, tc.want)
			}
		})
	}

	// A non-UTC input must still be normalised.
	madrid := model.MustCalendar("Europe/Madrid").Location()
	local := time.Date(2026, 1, 1, 0, 30, 0, 0, madrid) // = 2025-12-31T23:30Z
	if got := PlayPartition(local); got != "PLAY#2025-12" {
		t.Errorf("PlayPartition(local time) = %q, want PLAY#2025-12", got)
	}
}

func TestPlaySKRoundTrip(t *testing.T) {
	ts := mustTS(t, "2025-03-14T21:04:33.123Z")
	sk := PlaySK(ts, "4uLU6hMCjMI75M1A2tKUQC")
	if want := "2025-03-14T21:04:33.123Z#4uLU6hMCjMI75M1A2tKUQC"; sk != want {
		t.Errorf("PlaySK = %q, want %q", sk, want)
	}

	gotTS, gotID, err := ParsePlaySK(sk)
	if err != nil {
		t.Fatalf("ParsePlaySK: %v", err)
	}
	if !gotTS.Equal(ts) {
		t.Errorf("timestamp = %v, want %v", gotTS, ts)
	}
	if gotID != "4uLU6hMCjMI75M1A2tKUQC" {
		t.Errorf("trackID = %q", gotID)
	}
}

// Play sort keys are compared lexically by DynamoDB, so their order must match chronology.
// This is the storage-level consequence of the fixed-width timestamp format.
func TestPlaySKSortsChronologically(t *testing.T) {
	base := "2025-03-14T21:04:33"
	// .120 and .123 are the pair that RFC3339Nano would invert.
	a := PlaySK(mustTS(t, base+".120Z"), "tA")
	b := PlaySK(mustTS(t, base+".123Z"), "tB")
	if !(a < b) {
		t.Errorf("%q must sort before %q", a, b)
	}
}

func TestParsePlaySKRejects(t *testing.T) {
	for _, tc := range []struct{ name, sk string }{
		{"no separator", "2025-03-14T21:04:33.123Z"},
		{"empty track id", "2025-03-14T21:04:33.123Z#"},
		{"bad timestamp", "not-a-time#t1"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ParsePlaySK(tc.sk); err == nil {
				t.Errorf("ParsePlaySK(%q) succeeded, want an error", tc.sk)
			}
		})
	}
}

// TestPlayPartitionsBetweenSpansLocalMonth is the payoff of the UTC-partition decision:
// callers ask for a local calendar month and this owns the two-partition fan-out.
func TestPlayPartitionsBetweenSpansLocalMonth(t *testing.T) {
	madrid := model.MustCalendar("Europe/Madrid")

	// Local March 2025 begins 2025-02-28T23:00Z (CET) and ends 2025-03-31T22:00Z (CEST).
	start, end, err := madrid.Bounds("2025-03")
	if err != nil {
		t.Fatal(err)
	}
	got := PlayPartitionsBetween(start, end)
	want := []string{"PLAY#2025-02", "PLAY#2025-03"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("a local month must fan out to two UTC partitions (-want +got):\n%s", diff)
	}
}

func TestPlayPartitionsBetween(t *testing.T) {
	tests := []struct {
		name     string
		from, to string
		want     []string
	}{
		{
			name: "single partition", from: "2025-03-01T00:00:00.000Z", to: "2025-03-31T23:59:59.999Z",
			want: []string{"PLAY#2025-03"},
		},
		{
			// The exclusive end must not pull in the following partition.
			name: "range ending exactly on a month boundary",
			from: "2025-01-01T00:00:00.000Z", to: "2025-02-01T00:00:00.000Z",
			want: []string{"PLAY#2025-01"},
		},
		{
			name: "one nanosecond into the next month",
			from: "2025-01-01T00:00:00.000Z", to: "2025-02-01T00:00:00.001Z",
			want: []string{"PLAY#2025-01", "PLAY#2025-02"},
		},
		{
			name: "spans a year boundary",
			from: "2024-11-15T00:00:00.000Z", to: "2025-02-10T00:00:00.000Z",
			want: []string{"PLAY#2024-11", "PLAY#2024-12", "PLAY#2025-01", "PLAY#2025-02"},
		},
		{
			name: "empty range", from: "2025-03-01T00:00:00.000Z", to: "2025-03-01T00:00:00.000Z",
			want: nil,
		},
		{
			name: "inverted range", from: "2025-03-05T00:00:00.000Z", to: "2025-03-01T00:00:00.000Z",
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PlayPartitionsBetween(mustTS(t, tc.from), mustTS(t, tc.to))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

func TestPlayPartitionsBetweenLongRange(t *testing.T) {
	// Two full years plus a month.
	got := PlayPartitionsBetween(
		mustTS(t, "2023-01-15T00:00:00.000Z"),
		mustTS(t, "2025-02-01T00:00:00.001Z"),
	)
	if len(got) != 26 {
		t.Errorf("partitions = %d, want 26", len(got))
	}
	if got[0] != "PLAY#2023-01" || got[len(got)-1] != "PLAY#2025-02" {
		t.Errorf("bounds = %q .. %q", got[0], got[len(got)-1])
	}
}

func TestDimensionKeys(t *testing.T) {
	if got := TrackPK("t1"); got != "TRACK#t1" {
		t.Errorf("TrackPK = %q", got)
	}
	if got := ArtistPK("ar1"); got != "ARTIST#ar1" {
		t.Errorf("ArtistPK = %q", got)
	}
	if got := AlbumPK("al1"); got != "ALBUM#al1" {
		t.Errorf("AlbumPK = %q", got)
	}
	// GSI1 groups plays by track, so it shares the track key space by design.
	if got := TrackGSI1PK("t1"); got != "TRACK#t1" {
		t.Errorf("TrackGSI1PK = %q", got)
	}
}

func TestDimensionPK(t *testing.T) {
	for _, tc := range []struct {
		dim     model.Dim
		want    string
		wantErr bool
	}{
		{model.DimTrack, "TRACK#x", false},
		{model.DimArtist, "ARTIST#x", false},
		{model.DimAlbum, "ALBUM#x", false},
		// TOTAL and GENRE have no metadata rows: there is no entity to describe.
		{model.DimTotal, "", true},
		{model.DimGenre, "", true},
		{model.Dim("NOPE"), "", true},
	} {
		got, err := DimensionPK(tc.dim, "x")
		if tc.wantErr != (err != nil) {
			t.Errorf("DimensionPK(%q) err = %v, wantErr %v", tc.dim, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("DimensionPK(%q) = %q, want %q", tc.dim, got, tc.want)
		}
	}
}

func TestDerivedKeys(t *testing.T) {
	if got := TopPK(model.DimArtist, "2025"); got != "TOP#ARTIST#2025" {
		t.Errorf("TopPK = %q", got)
	}
	if got := TopPK(model.DimTrack, model.PeriodAll); got != "TOP#TRACK#ALL" {
		t.Errorf("TopPK = %q", got)
	}
	if got := HistPK("2025"); got != "HIST#2025" {
		t.Errorf("HistPK = %q", got)
	}
	if got := IngestSK("2025-03"); got != "INGEST#2025-03" {
		t.Errorf("IngestSK = %q", got)
	}
	ts := mustTS(t, "2025-03-14T21:04:33.123Z")
	if got := GapSK(ts); got != "GAP#2025-03-14T21:04:33.123Z" {
		t.Errorf("GapSK = %q", got)
	}
}

// Gap markers must order naturally within the STATE partition.
func TestGapSKSortsChronologically(t *testing.T) {
	a := GapSK(mustTS(t, "2025-03-14T21:00:00.000Z"))
	b := GapSK(mustTS(t, "2025-03-14T22:00:00.000Z"))
	if !(a < b) {
		t.Errorf("%q must sort before %q", a, b)
	}
}

func TestClassifyNilError(t *testing.T) {
	if err := classify("Get", "PK", "SK", nil); err != nil {
		t.Errorf("classify(nil) = %v, want nil", err)
	}
}

func TestClassifyWrapsUnknownErrors(t *testing.T) {
	base := errors.New("network unreachable")
	err := classify("PutItem", "PLAY#2025-03", "sk", base)

	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("err = %T, want *store.Error", err)
	}
	if se.Op != "PutItem" || se.PK != "PLAY#2025-03" || se.SK != "sk" {
		t.Errorf("context = %+v", se)
	}
	if !errors.Is(err, base) {
		t.Error("the original error must remain reachable via errors.Is")
	}
	if !contains(err.Error(), "PLAY#2025-03") {
		t.Errorf("message %q should name the key", err.Error())
	}
}

func TestErrorMessageWithoutSortKey(t *testing.T) {
	e := &Error{Op: "Query", PK: "STATE", Err: ErrNotFound}
	if got := e.Error(); contains(got, "/") {
		t.Errorf("message %q should omit an empty sort key", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})())
}

// TestClassifyResourceNotFound guards the diagnosis of a production outage.
//
// A DynamoDB client pointed at the wrong region reports ResourceNotFoundException, which names
// neither the table nor the region and reads like a missing table. Mapping it to a distinct
// sentinel lets the layer above say what actually went wrong.
func TestClassifyResourceNotFound(t *testing.T) {
	err := classify("GetItem", "STATE", "CONFIG", &ddbtypes.ResourceNotFoundException{
		Message: aws.String("Requested resource not found"),
	})

	if !errors.Is(err, ErrTableNotFound) {
		t.Errorf("err = %v, want it to wrap ErrTableNotFound", err)
	}
	// And it must not be mistaken for the conditional-write outcome, which is routine.
	if errors.Is(err, ErrAlreadyExists) {
		t.Error("a missing table was classified as an already-existing item")
	}
	var se *Error
	if !errors.As(err, &se) || se.Op != "GetItem" {
		t.Errorf("err = %v, want it to carry the operation", err)
	}
}
