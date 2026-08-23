package backfill_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/neovasili/spotistats/internal/backfill"
)

func readSample(t *testing.T) []backfill.Record {
	t.Helper()
	var out []backfill.Record
	if err := backfill.ReadFile(filepath.Join("testdata", "sample.json"), func(r backfill.Record) error {
		out = append(out, r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReadFileStreamsEveryRecord(t *testing.T) {
	if got := len(readSample(t)); got != 7 {
		t.Fatalf("records = %d, want 7", got)
	}
}

func TestTrackID(t *testing.T) {
	tests := []struct{ uri, want string }{
		{"spotify:track:6cw6IpCUQSviIZYo7T00WU", "6cw6IpCUQSviIZYo7T00WU"},
		{"", ""},
		// An episode URI must never be mistaken for a track: it would attribute a podcast to
		// a track ID that means something else entirely.
		{"spotify:episode:5Xt5DXGzch68nYYamXrNxZ", ""},
		{"spotify:local:Artist:Album:Title:213", ""},
		{"spotify:album:1DFixLWuPkv3KT3TnV35m3", ""},
	}
	for _, tc := range tests {
		if got := (backfill.Record{TrackURI: tc.uri}).TrackID(); got != tc.want {
			t.Errorf("TrackID(%q) = %q, want %q", tc.uri, got, tc.want)
		}
	}
}

func TestPlayedAtIsUTCSecondPrecision(t *testing.T) {
	got, err := backfill.Record{TS: "2009-10-31T18:29:21Z"}.PlayedAt()
	if err != nil {
		t.Fatal(err)
	}
	if got.Location() != nil && got.UTC() != got {
		t.Error("PlayedAt must be UTC")
	}
	if got.Format("2006-01-02T15:04:05Z") != "2009-10-31T18:29:21Z" {
		t.Errorf("round trip = %s", got)
	}
	if _, err := (backfill.Record{TS: "31/10/2009"}).PlayedAt(); err == nil {
		t.Error("a malformed timestamp must be rejected, not silently zeroed")
	}
}

// TestFilterClassifiesEveryRecord walks the sample corpus, which contains one of each thing
// the real export throws at the importer.
func TestFilterClassifiesEveryRecord(t *testing.T) {
	recs := readSample(t)
	got := make([]string, 0, len(recs))
	for _, r := range recs {
		got = append(got, string(r.Filter(30_000)))
	}
	want := []string{
		"",                              // a normal completed play
		string(backfill.SkipTooShort),   // 4s skip
		string(backfill.SkipPodcast),    // episode
		string(backfill.SkipNoTrackURI), // local file
		string(backfill.SkipAudiobook),  // audiobook, even though it has a track URI
		string(backfill.SkipBadTS),      // unparseable ts
		string(backfill.SkipTooShort),   // 0 ms, though reason_end says trackdone
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

// A zero-duration record is never a play, even with --min-ms 0. model.Play.Validate rejects
// it anyway; catching it in the filter keeps that from aborting a 400,000-record run.
func TestFilterRejectsZeroDurationEvenWithNoMinimum(t *testing.T) {
	r := backfill.Record{TS: "2015-03-14T21:04:33Z", TrackURI: "spotify:track:t1", MsPlayed: 0}
	if got := r.Filter(0); got != backfill.SkipTooShort {
		t.Errorf("Filter(0) on a 0ms record = %q, want it skipped", got)
	}
	r.MsPlayed = 1
	if got := r.Filter(0); got != "" {
		t.Errorf("Filter(0) on a 1ms record = %q, want it kept", got)
	}
}

// The audiobook check must precede the track-URI check: an audiobook chapter can carry a
// spotify_track_uri, and classifying it as music would count a book as listening.
func TestFilterPrefersContentTypeOverTrackURI(t *testing.T) {
	r := backfill.Record{
		TS: "2015-03-14T21:04:33Z", MsPlayed: 600_000,
		TrackURI: "spotify:track:t1", AudiobookURI: "spotify:audiobook:b1",
	}
	if got := r.Filter(30_000); got != backfill.SkipAudiobook {
		t.Errorf("Filter = %q, want the audiobook skip", got)
	}
}

func TestFilesRejectsAnEmptyDirectory(t *testing.T) {
	if _, err := backfill.Files(t.TempDir()); err == nil {
		t.Error("an export directory with no history files must be an error, not an empty import")
	}
}

// Video files are music played with a video stream, not podcasts, and excluding them would
// silently drop real listening.
func TestFilesIncludesAudioAndVideo(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"Streaming_History_Audio_2015.json",
		"Streaming_History_Video_2025.json",
		"ReadMeFirst_ExtendedStreamingHistory.pdf",
		"Userdata.json",
	} {
		if err := writeFile(t, filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := backfill.Files(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("files = %v, want the two Streaming_History files only", got)
	}
}

func writeFile(t *testing.T, path string) error {
	t.Helper()
	return os.WriteFile(path, []byte("[]"), 0o600)
}
