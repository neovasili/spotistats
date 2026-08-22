package storetest

import (
	"testing"

	"github.com/neovasili/spotistats/internal/model"
)

// TrackOpt customises the synthetic track a seeded play refers to.
type TrackOpt func(*model.Track)

// WithArtists sets the artists on the play's track.
func WithArtists(ids ...string) TrackOpt {
	return func(t *model.Track) { t.ArtistIDs = ids }
}

// WithAlbum sets the album, or clears it when passed "".
func WithAlbum(id string) TrackOpt {
	return func(t *model.Track) { t.AlbumID = id }
}

// WithDuration sets the track duration, which is also the estimated msPlayed of an
// api-sourced play.
func WithDuration(ms int64) TrackOpt {
	return func(t *model.Track) { t.DurationMs = ms }
}

// Track builds a synthetic track with sane defaults.
func Track(id string, opts ...TrackOpt) model.Track {
	t := model.Track{
		ID:         id,
		Name:       "Track " + id,
		DurationMs: 200_000,
		AlbumID:    "al1",
		ArtistIDs:  []string{"ar1"},
	}
	for _, o := range opts {
		o(&t)
	}
	return t
}

// APIPlay builds an api-sourced play at the given RFC3339 instant. msPlayed is the track
// duration and the play is marked estimated, matching what the real client produces.
func APIPlay(t *testing.T, instant, trackID string, opts ...TrackOpt) model.Play {
	t.Helper()
	ts, err := model.ParseTS(instant)
	if err != nil {
		t.Fatalf("storetest: parse %q: %v", instant, err)
	}
	p, err := model.NewAPIPlay(ts, Track(trackID, opts...))
	if err != nil {
		t.Fatalf("storetest: build api play: %v", err)
	}
	return p
}

// ExportPlay builds an export-sourced play with an exact duration.
func ExportPlay(t *testing.T, instant, trackID string, msPlayed int64, opts ...TrackOpt) model.Play {
	t.Helper()
	ts, err := model.ParseTS(instant)
	if err != nil {
		t.Fatalf("storetest: parse %q: %v", instant, err)
	}
	p, err := model.NewExportPlay(ts, msPlayed, Track(trackID, opts...),
		model.ExportFields{Platform: "ios", Country: "ES", ReasonEnd: "trackdone"})
	if err != nil {
		t.Fatalf("storetest: build export play: %v", err)
	}
	return p
}

// Genres maps artist IDs to the genres used by the corpus, so aggregate tests can supply
// the same genre resolution the real pipeline would.
func Genres() map[string][]string {
	return map[string][]string{
		"ar1": {"symphonic metal", "gothic metal"},
		"ar2": {"gothic metal"}, // deliberately overlaps ar1, to exercise dedup
		"ar3": {"dutch metal"},
		"ar4": nil, // artists with no genres are the common case
	}
}

// GenresFor resolves the union of genres across a play's artists.
func GenresFor(p model.Play) []string {
	m := Genres()
	var out []string
	for _, id := range p.ArtistIDs {
		out = append(out, m[id]...)
	}
	return out
}

// Corpus is a fixed, deliberately awkward set of plays used by the cross-dimension
// invariant tests. Its properties matter:
//
//   - It spans two local months and two local years, including instants that fall in a
//     different UTC month than local month, so partition fan-out is exercised.
//   - It mixes api and export sources, so the estimated/exact split is non-trivial.
//   - Some plays have two artists (making artist totals exceed the overall total) and some
//     have an artist with no genres (making genre totals fall short of it).
//   - One track has no album, so album totals are also incomplete.
func Corpus(t *testing.T) []model.Play {
	t.Helper()
	return []model.Play{
		// --- local December 2025 (UTC December) ---
		APIPlay(t, "2025-12-15T10:00:00.000Z", "t1", WithArtists("ar1"), WithDuration(210_000)),
		ExportPlay(t, "2025-12-15T11:00:00.000Z", "t1", 180_000, WithArtists("ar1"), WithDuration(210_000)),
		APIPlay(t, "2025-12-20T22:00:00.000Z", "t2", WithArtists("ar1", "ar2"), WithDuration(240_000)),

		// The decisive instant: 23:30Z on New Year's Eve is already 2026 in Madrid, so it
		// belongs to local year 2026 while living in the UTC 2025-12 partition.
		APIPlay(t, "2025-12-31T23:30:00.000Z", "t3", WithArtists("ar3"), WithDuration(300_000)),

		// --- local January 2026 ---
		ExportPlay(t, "2026-01-05T09:00:00.000Z", "t2", 120_000, WithArtists("ar1", "ar2"), WithDuration(240_000)),
		APIPlay(t, "2026-01-05T09:30:00.000Z", "t4", WithArtists("ar4"), WithDuration(150_000)),
		// No album, so it contributes nothing to any album aggregate.
		APIPlay(t, "2026-01-06T18:00:00.000Z", "t5", WithArtists("ar4"), WithAlbum(""), WithDuration(190_000)),
		ExportPlay(t, "2026-01-31T23:45:00.000Z", "t1", 205_000, WithArtists("ar1"), WithDuration(210_000)),

		// --- local February 2026 (23:45Z on Jan 31 is Feb 1 in Madrid) ---
		APIPlay(t, "2026-02-10T12:00:00.000Z", "t3", WithArtists("ar3"), WithDuration(300_000)),
		ExportPlay(t, "2026-02-10T13:00:00.000Z", "t3", 275_000, WithArtists("ar3"), WithDuration(300_000)),
	}
}
