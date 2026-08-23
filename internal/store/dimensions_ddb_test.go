package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
	"github.com/neovasili/spotistats/internal/store/storetest"
)

func TestDimensionRoundTrips(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	t.Run("track", func(t *testing.T) {
		want := model.Track{
			ID: "t1", Name: "Ice Queen", DurationMs: 314_000, AlbumID: "al1",
			ArtistIDs: []string{"ar1"}, Popularity: 62, ISRC: "NLA320400123",
			URI: "spotify:track:t1",
		}
		if err := s.PutTrack(ctx, want); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetTrack(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		want.RefreshedAt = storetest.FixedNow
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
	})

	t.Run("artist with genres", func(t *testing.T) {
		want := model.Artist{
			ID: "ar1", Name: "Within Temptation",
			Genres:     []string{"symphonic metal", "gothic metal"},
			Popularity: 62, Followers: 2_500_000,
			ImageURL: "https://i.scdn.co/image/ar1",
		}
		if err := s.PutArtist(ctx, want); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetArtist(ctx, "ar1")
		if err != nil {
			t.Fatal(err)
		}
		want.RefreshedAt = storetest.FixedNow
		// PutArtist writes the full object, so it stamps enrichedAt as well as refreshedAt.
		want.EnrichedAt = storetest.FixedNow
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
	})

	t.Run("artist with no genres", func(t *testing.T) {
		// The common case: most artists carry none at all.
		if err := s.PutArtist(ctx, model.Artist{ID: "ar4", Name: "Nobody"}); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetArtist(ctx, "ar4")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Genres) != 0 {
			t.Errorf("Genres = %v, want empty", got.Genres)
		}
	})

	t.Run("album preserves release precision", func(t *testing.T) {
		want := model.Album{
			ID: "al2", Name: "Mother Earth", ReleaseDate: "1998",
			ReleaseDatePrecision: "year", TotalTracks: 10, ArtistIDs: []string{"ar1"},
		}
		if err := s.PutAlbum(ctx, want); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetAlbum(ctx, "al2")
		if err != nil {
			t.Fatal(err)
		}
		if got.ReleaseDate != "1998" || got.ReleaseDatePrecision != "year" {
			t.Errorf("release = (%q, %q), want the year kept verbatim",
				got.ReleaseDate, got.ReleaseDatePrecision)
		}
	})
}

func TestDimensionNotFound(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	if _, err := s.GetTrack(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTrack err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetArtist(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetArtist err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetAlbum(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAlbum err = %v, want ErrNotFound", err)
	}
}

func TestBatchGetDimensions(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	// 150 tracks spans two BatchGetItem chunks.
	const n = 150
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := trackID(i)
		ids = append(ids, id)
		if err := s.PutTrack(ctx, model.Track{ID: id, Name: "Track " + id, DurationMs: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.GetTracks(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("got %d tracks, want %d", len(got), n)
	}
	if got[trackID(42)].DurationMs != 43 {
		t.Errorf("track 42 duration = %d, want 43", got[trackID(42)].DurationMs)
	}

	// Blanks, duplicates and unknown IDs are all tolerated.
	mixed, err := s.GetTracks(ctx, []string{ids[0], ids[0], "", "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mixed) != 1 {
		t.Errorf("got %d results, want 1", len(mixed))
	}

	if empty, err := s.GetTracks(ctx, nil); err != nil || len(empty) != 0 {
		t.Errorf("GetTracks(nil) = (%v, %v)", empty, err)
	}
}

func TestGetArtistsResolvesGenresForAggregation(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	for id, genres := range storetest.Genres() {
		if err := s.PutArtist(ctx, model.Artist{ID: id, Name: "A " + id, Genres: genres}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.GetArtists(ctx, []string{"ar1", "ar2", "ar3", "ar4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d artists, want 4", len(got))
	}

	// This is exactly how the capture job resolves the genres a play contributes to.
	var union []string
	for _, id := range []string{"ar1", "ar2"} {
		union = append(union, got[id].Genres...)
	}
	p := storetest.APIPlay(t, "2025-03-14T21:04:33.123Z", "t1", storetest.WithArtists("ar1", "ar2"))
	facts := model.FactsFor(p, union)
	// ar1 and ar2 both carry "gothic metal", which must collapse to one.
	if diff := cmp.Diff([]string{"gothic metal", "symphonic metal"}, facts.Genres); diff != "" {
		t.Errorf("resolved genres (-want +got):\n%s", diff)
	}
}

// Tombstones stop the enrichment pass re-requesting IDs that can never resolve.
func TestPutMissingTombstones(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	for _, tc := range []struct {
		dim model.Dim
		id  string
		get func() (bool, error)
	}{
		{model.DimTrack, "deadtrack", func() (bool, error) {
			v, err := s.GetTrack(ctx, "deadtrack")
			return v.Missing, err
		}},
		{model.DimArtist, "deadartist", func() (bool, error) {
			v, err := s.GetArtist(ctx, "deadartist")
			return v.Missing, err
		}},
		{model.DimAlbum, "deadalbum", func() (bool, error) {
			v, err := s.GetAlbum(ctx, "deadalbum")
			return v.Missing, err
		}},
	} {
		t.Run(string(tc.dim), func(t *testing.T) {
			if err := s.PutMissing(ctx, tc.dim, tc.id); err != nil {
				t.Fatal(err)
			}
			missing, err := tc.get()
			if err != nil {
				t.Fatalf("tombstone should be readable: %v", err)
			}
			if !missing {
				t.Error("Missing = false; the tombstone did not persist")
			}
		})
	}

	// Dimensions with no metadata rows must be refused rather than silently written.
	if err := s.PutMissing(ctx, model.DimGenre, "x"); err == nil {
		t.Error("PutMissing accepted DimGenre, which has no metadata rows")
	}
	if err := s.PutMissing(ctx, model.DimTotal, "x"); err == nil {
		t.Error("PutMissing accepted DimTotal")
	}
	if err := s.PutMissing(ctx, model.DimTrack, ""); err == nil {
		t.Error("PutMissing accepted an empty id")
	}
}

func TestIsStale(t *testing.T) {
	s := storetest.NewStore(t)
	// A zero refreshedAt means the row predates the field, or is a tombstone: refresh it.
	if !s.IsStale(time.Time{}) {
		t.Error("a zero refreshedAt should be stale")
	}
	if s.IsStale(storetest.FixedNow) {
		t.Error("a row written now should not be stale")
	}
	if s.IsStale(storetest.FixedNow.Add(-store.DimensionStaleAfter + time.Hour)) {
		t.Error("a row just inside the window should not be stale")
	}
	if !s.IsStale(storetest.FixedNow.Add(-store.DimensionStaleAfter - time.Hour)) {
		t.Error("a row past the window should be stale")
	}
}

// ---------------------------------------------------------------------------
// state
// ---------------------------------------------------------------------------

func TestPollCursorRoundTrip(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	// A missing cursor is the first run, not an error.
	zero, err := s.GetPollCursor(ctx)
	if err != nil {
		t.Fatalf("GetPollCursor on an empty table must not fail: %v", err)
	}
	if !zero.LastPlayedAt.IsZero() {
		t.Errorf("expected a zero cursor, got %+v", zero)
	}

	want := model.PollCursor{
		LastPlayedAt: mustTS(t, "2025-03-14T21:04:33.123Z"),
		LastRunAt:    mustTS(t, "2025-03-14T22:00:00.000Z"),
		LastStatus:   "ok",
	}
	if err := s.PutPollCursor(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPollCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestGapMarkers(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	first := model.GapMarker{
		DetectedAt:    mustTS(t, "2025-03-14T20:00:00.000Z"),
		WindowStart:   mustTS(t, "2025-03-14T18:00:00.000Z"),
		WindowEnd:     mustTS(t, "2025-03-14T20:00:00.000Z"),
		ItemsReturned: 50, Limit: 50,
	}
	second := model.GapMarker{
		DetectedAt:    mustTS(t, "2025-03-14T22:00:00.000Z"),
		ItemsReturned: 50, Limit: 50,
	}
	// Written out of order to prove the sort key orders them.
	for _, g := range []model.GapMarker{second, first} {
		if err := s.PutGapMarker(ctx, g); err != nil {
			t.Fatal(err)
		}
	}

	got := collect(t, s.GapMarkers(ctx))
	if len(got) != 2 {
		t.Fatalf("got %d markers, want 2", len(got))
	}
	if !got[0].DetectedAt.Equal(first.DetectedAt) {
		t.Errorf("markers are not chronological: first is %v", got[0].DetectedAt)
	}
	if got[0].ItemsReturned != 50 || got[0].Limit != 50 {
		t.Errorf("marker payload = %+v", got[0])
	}
	if !got[0].WindowStart.Equal(first.WindowStart) {
		t.Errorf("WindowStart = %v, want %v", got[0].WindowStart, first.WindowStart)
	}
}

// A month claim is conditional so an importer must decide explicitly whether to supersede.
func TestIngestMarkerIsConditional(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	m := model.IngestMarker{
		Month: "2025-03", Source: model.SourceExport,
		ImportedAt: storetest.FixedNow, PlayCount: 1234,
	}
	if err := s.PutIngestMarker(ctx, m); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetIngestMarker(ctx, "2025-03")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(m, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	// A second claim must be refused, not silently applied.
	again := m
	again.PlayCount = 9999
	if err := s.PutIngestMarker(ctx, again); !errors.Is(err, store.ErrAlreadyExists) {
		t.Errorf("err = %v, want ErrAlreadyExists", err)
	}
	if got, _ := s.GetIngestMarker(ctx, "2025-03"); got.PlayCount != 1234 {
		t.Errorf("PlayCount = %d; a refused claim must not modify the row", got.PlayCount)
	}

	// Superseding is explicit.
	if err := s.ReplaceIngestMarker(ctx, again); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetIngestMarker(ctx, "2025-03"); got.PlayCount != 9999 {
		t.Errorf("PlayCount = %d, want the superseded 9999", got.PlayCount)
	}
}

func TestIngestMarkerValidation(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		m    model.IngestMarker
	}{
		{"year granularity", model.IngestMarker{Month: "2025", Source: model.SourceExport}},
		{"day granularity", model.IngestMarker{Month: "2025-03-14", Source: model.SourceExport}},
		{"all", model.IngestMarker{Month: model.PeriodAll, Source: model.SourceExport}},
		{"unknown source", model.IngestMarker{Month: "2025-03", Source: model.Source("guess")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.PutIngestMarker(ctx, tc.m); err == nil {
				t.Error("expected a validation error")
			}
		})
	}
}

func TestIngestMarkerNotFound(t *testing.T) {
	s := storetest.NewStore(t)
	if _, err := s.GetIngestMarker(context.Background(), "2025-03"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// VerifyConfig
// ---------------------------------------------------------------------------

// TestVerifyConfigDetectsTimezoneChange is the guard that makes a changed timezone a loud
// startup failure. Silently proceeding would derive new period keys under one calendar while
// existing history was derived under another -- an inconsistency no query could detect and
// no reconcile could repair, because the raw plays record no zone.
func TestVerifyConfigDetectsTimezoneChange(t *testing.T) {
	c := storetest.RequireDynamoDB(t)
	table := storetest.CreateTable(t, c)
	ctx := context.Background()

	madridStore := storetest.NewStoreOnTable(t, c, table, storetest.WithTimezone("Europe/Madrid"))
	if err := madridStore.VerifyConfig(ctx); err != nil {
		t.Fatalf("first VerifyConfig should initialise the row: %v", err)
	}
	// Idempotent.
	if err := madridStore.VerifyConfig(ctx); err != nil {
		t.Fatalf("second VerifyConfig: %v", err)
	}

	lisbonStore := storetest.NewStoreOnTable(t, c, table, storetest.WithTimezone("Europe/Lisbon"))
	err := lisbonStore.VerifyConfig(ctx)
	if !errors.Is(err, store.ErrConfigMismatch) {
		t.Fatalf("err = %v, want ErrConfigMismatch", err)
	}
	if !contains(err.Error(), "Europe/Madrid") || !contains(err.Error(), "Europe/Lisbon") {
		t.Errorf("message %q should name both zones", err.Error())
	}
}

func TestVerifyConfigAcceptsMatchingConfig(t *testing.T) {
	c := storetest.RequireDynamoDB(t)
	table := storetest.CreateTable(t, c)
	ctx := context.Background()

	a := storetest.NewStoreOnTable(t, c, table)
	if err := a.VerifyConfig(ctx); err != nil {
		t.Fatal(err)
	}
	// A second process with identical configuration must be accepted.
	b := storetest.NewStoreOnTable(t, c, table)
	if err := b.VerifyConfig(ctx); err != nil {
		t.Errorf("a matching configuration was rejected: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// leaderboards and histograms
// ---------------------------------------------------------------------------

func TestLeaderboardRoundTrip(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	want := store.Leaderboard{
		Dim: model.DimArtist, Period: "2025", Metric: "ms",
		Entries: []store.LeaderboardEntry{
			{ID: "ar1", Name: "Within Temptation", Plays: 120, MsPlayed: 25_000_000,
				ImageURL: "https://i.scdn.co/image/ar1"},
			{ID: "ar3", Name: "Nightwish", Plays: 80, MsPlayed: 18_000_000},
		},
	}
	if err := s.PutLeaderboard(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLeaderboard(ctx, model.DimArtist, "2025")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want.Entries, got.Entries); diff != "" {
		t.Errorf("entries (-want +got):\n%s", diff)
	}
	// Ordering is the whole point of materialising these.
	if got.Entries[0].MsPlayed < got.Entries[1].MsPlayed {
		t.Error("stored order was not preserved")
	}
	if got.ComputedAt == "" {
		t.Error("ComputedAt was not stamped")
	}

	if _, err := s.GetLeaderboard(ctx, model.DimTrack, "2025"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for an uncomputed leaderboard", err)
	}
}

func TestHistogramRoundTrip(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	want := store.Histogram{
		Period: "2025", Kind: store.HistogramHour,
		Plays:    map[int]int64{0: 3, 9: 40, 23: 12},
		MsPlayed: map[int]int64{0: 600_000, 9: 8_000_000, 23: 2_400_000},
	}
	if err := s.PutHistogram(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetHistogram(ctx, "2025", store.HistogramHour)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	// Hour and weekday are separate rows in the same partition.
	dow := store.Histogram{
		Period: "2025", Kind: store.HistogramWeekday,
		Plays:    map[int]int64{0: 5, 6: 9},
		MsPlayed: map[int]int64{0: 1000, 6: 2000},
	}
	if err := s.PutHistogram(ctx, dow); err != nil {
		t.Fatal(err)
	}
	gotHour, err := s.GetHistogram(ctx, "2025", store.HistogramHour)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotHour.Plays) != 3 {
		t.Errorf("the weekday write clobbered the hour row: %+v", gotHour)
	}

	if _, err := s.GetHistogram(ctx, "2099", store.HistogramHour); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestTombstoneExpiry pins the boundary. A tombstone is a NEGATIVE cache entry and unlike a
// cached name it can be wrong -- a transient upstream fault nulls an ID exactly as a dead one
// does -- so it must expire, or one blip leaves an entity nameless forever.
func TestTombstoneExpiry(t *testing.T) {
	s := storetest.NewStore(t)

	if s.TombstoneExpired(storetest.FixedNow.Add(-store.TombstoneRetryAfter + time.Hour)) {
		t.Error("a tombstone inside the retry window expired; capture would re-ask about " +
			"genuinely dead IDs on every run")
	}
	if !s.TombstoneExpired(storetest.FixedNow.Add(-store.TombstoneRetryAfter - time.Hour)) {
		t.Error("a tombstone past the retry window did not expire; a wrongly-tombstoned " +
			"entity could never recover its name")
	}
	if !s.TombstoneExpired(time.Time{}) {
		t.Error("a zero refreshedAt must count as expired")
	}
	// Retrying a tombstone must be cheaper than refreshing a name, not more expensive.
	if store.TombstoneRetryAfter >= store.DimensionStaleAfter {
		t.Errorf("TombstoneRetryAfter (%v) >= DimensionStaleAfter (%v): a negative cache entry "+
			"is being trusted longer than a positive one", store.TombstoneRetryAfter,
			store.DimensionStaleAfter)
	}
}

// TestPutArtistNameSemantics pins the three properties PutArtistName exists for. Each one was
// a real defect: a Put here clobbered enrichment, stamping enrichedAt suppressed the genre
// fetch forever, and the reserved keyword "missing" failed every request with a
// ValidationException that the caller reported only as degraded genres.
func TestPutArtistNameSemantics(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	t.Run("creates a named row that is not marked enriched", func(t *testing.T) {
		if err := s.PutArtistName(ctx, "ar-new", "Nightwish"); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetArtist(ctx, "ar-new")
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "Nightwish" {
			t.Errorf("name = %q, want Nightwish", got.Name)
		}
		if !got.EnrichedAt.IsZero() {
			t.Error("enrichedAt stamped by a name-only write; the genre pass would skip " +
				"this artist forever")
		}
		if got.RefreshedAt.IsZero() {
			t.Error("refreshedAt not stamped")
		}
	})

	t.Run("preserves genres and images on an enriched row", func(t *testing.T) {
		if err := s.PutArtist(ctx, model.Artist{
			ID: "ar-full", Name: "Epica", Genres: []string{"symphonic metal"},
			Popularity: 60, ImageURL: "https://example.test/e.jpg", Followers: 900_000,
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutArtistName(ctx, "ar-full", "Epica (renamed)"); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetArtist(ctx, "ar-full")
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "Epica (renamed)" {
			t.Errorf("name = %q, want the update to have applied", got.Name)
		}
		if len(got.Genres) != 1 || got.Genres[0] != "symphonic metal" {
			t.Errorf("genres = %v, want preserved", got.Genres)
		}
		if got.Popularity != 60 || got.Followers != 900_000 || got.ImageURL == "" {
			t.Errorf("enriched fields clobbered: %+v", got)
		}
		if got.EnrichedAt.IsZero() {
			t.Error("enrichedAt cleared by a name write")
		}
	})

	t.Run("leaves a tombstone alone", func(t *testing.T) {
		if err := s.PutMissing(ctx, model.DimArtist, "ar-dead"); err != nil {
			t.Fatal(err)
		}
		// The condition fails, which is the expected outcome and must not surface as an error.
		if err := s.PutArtistName(ctx, "ar-dead", "Should Not Apply"); err != nil {
			t.Fatalf("a tombstoned row must not be an error: %v", err)
		}
		got, err := s.GetArtist(ctx, "ar-dead")
		if err != nil {
			t.Fatal(err)
		}
		if !got.Missing {
			t.Error("tombstone cleared by a name write")
		}
		if got.Name != "" {
			t.Errorf("name = %q, want the tombstone left unnamed", got.Name)
		}
	})

	t.Run("rejects empty input", func(t *testing.T) {
		if err := s.PutArtistName(ctx, "", "x"); err == nil {
			t.Error("accepted an empty id")
		}
		if err := s.PutArtistName(ctx, "ar1", ""); err == nil {
			t.Error("accepted an empty name")
		}
	})
}

// TestResolveLabels pins the display rule that the rollup and the query API share.
//
// It is one implementation on purpose: both had their own copy and they drifted, which is how
// the dashboard ended up showing bare album titles with no artist.
func TestResolveLabels(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	if err := s.PutArtist(ctx, model.Artist{ID: "ar1", Name: "Within Temptation"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutArtist(ctx, model.Artist{ID: "ar2", Name: "Tarja Turunen"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAlbum(ctx, model.Album{
		ID: "al1", Name: "Bleed Out", ImageURL: "https://example.test/al1.jpg",
		ArtistIDs: []string{"ar1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTrack(ctx, model.Track{
		ID: "t1", Name: "Bad Things", AlbumID: "al1", ArtistIDs: []string{"ar1", "ar2"},
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("a track carries both its album and its artist", func(t *testing.T) {
		got, err := s.ResolveLabels(ctx, model.DimTrack, []string{"t1"})
		if err != nil {
			t.Fatal(err)
		}
		want := store.Label{
			Name: "Bad Things", AlbumName: "Bleed Out", ArtistName: "Within Temptation",
			ImageURL: "https://example.test/al1.jpg",
		}
		if diff := cmp.Diff(want, got["t1"]); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
	})

	t.Run("an album carries its artist", func(t *testing.T) {
		got, err := s.ResolveLabels(ctx, model.DimAlbum, []string{"al1"})
		if err != nil {
			t.Fatal(err)
		}
		if got["al1"].ArtistName != "Within Temptation" {
			t.Errorf("artistName = %q, want Within Temptation", got["al1"].ArtistName)
		}
		if got["al1"].AlbumName != "" {
			t.Error("an album must not repeat its own name as album context")
		}
	})

	t.Run("an artist has no context to add", func(t *testing.T) {
		got, err := s.ResolveLabels(ctx, model.DimArtist, []string{"ar1"})
		if err != nil {
			t.Fatal(err)
		}
		if got["ar1"].ArtistName != "" || got["ar1"].AlbumName != "" {
			t.Errorf("artist label carries redundant context: %+v", got["ar1"])
		}
	})

	t.Run("the primary artist wins for a collaboration", func(t *testing.T) {
		// Spotify orders credits with the primary first. Showing every collaborator would
		// overflow the label on exactly the releases whose titles are already long.
		if got := store.PrimaryArtist([]string{"ar1", "ar2"}); got != "ar1" {
			t.Errorf("PrimaryArtist = %q, want ar1", got)
		}
		if got := store.PrimaryArtist(nil); got != "" {
			t.Errorf("PrimaryArtist(nil) = %q, want empty", got)
		}
	})

	t.Run("a genre is its own name", func(t *testing.T) {
		got, err := s.ResolveLabels(ctx, model.DimGenre, []string{"symphonic metal"})
		if err != nil {
			t.Fatal(err)
		}
		if got["symphonic metal"].Name != "symphonic metal" {
			t.Errorf("genre label = %+v", got["symphonic metal"])
		}
	})

	t.Run("a missing album leaves the track named but without context", func(t *testing.T) {
		// Partial enrichment must degrade to a bare title, never to an error or a blank name.
		if err := s.PutTrack(ctx, model.Track{
			ID: "t2", Name: "Orphan", AlbumID: "al-missing", ArtistIDs: []string{"ar-missing"},
		}); err != nil {
			t.Fatal(err)
		}
		got, err := s.ResolveLabels(ctx, model.DimTrack, []string{"t2"})
		if err != nil {
			t.Fatal(err)
		}
		if got["t2"].Name != "Orphan" {
			t.Errorf("name = %q, want Orphan", got["t2"].Name)
		}
		if got["t2"].AlbumName != "" || got["t2"].ArtistName != "" {
			t.Errorf("invented context for missing rows: %+v", got["t2"])
		}
	})
}

// TestArtworkSurvivesTheRoundTrip pins that both image URLs reach a leaderboard label.
//
// A track has no artwork of its own -- the cover shown for a track everywhere in Spotify's own
// clients is its ALBUM's art -- so the track case is the one that can silently lose it.
func TestArtworkSurvivesTheRoundTrip(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	if err := s.PutArtist(ctx, model.Artist{
		ID: "ar1", Name: "Within Temptation",
		ImageURL: "https://i.scdn.co/image/ar-big", ThumbURL: "https://i.scdn.co/image/ar-small",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAlbum(ctx, model.Album{
		ID: "al1", Name: "Bleed Out", ArtistIDs: []string{"ar1"},
		ImageURL: "https://i.scdn.co/image/al-big", ThumbURL: "https://i.scdn.co/image/al-small",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTrack(ctx, model.Track{
		ID: "t1", Name: "Bad Things", AlbumID: "al1", ArtistIDs: []string{"ar1"},
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("artist keeps both sizes", func(t *testing.T) {
		got, err := s.ResolveLabels(ctx, model.DimArtist, []string{"ar1"})
		if err != nil {
			t.Fatal(err)
		}
		if got["ar1"].ImageURL != "https://i.scdn.co/image/ar-big" ||
			got["ar1"].ThumbURL != "https://i.scdn.co/image/ar-small" {
			t.Errorf("label = %+v", got["ar1"])
		}
	})

	t.Run("track inherits its album's artwork", func(t *testing.T) {
		got, err := s.ResolveLabels(ctx, model.DimTrack, []string{"t1"})
		if err != nil {
			t.Fatal(err)
		}
		if got["t1"].ImageURL != "https://i.scdn.co/image/al-big" ||
			got["t1"].ThumbURL != "https://i.scdn.co/image/al-small" {
			t.Errorf("track label = %+v, want the album's artwork", got["t1"])
		}
	})

	t.Run("a track with no album has no artwork rather than a wrong one", func(t *testing.T) {
		if err := s.PutTrack(ctx, model.Track{ID: "t2", Name: "Orphan"}); err != nil {
			t.Fatal(err)
		}
		got, err := s.ResolveLabels(ctx, model.DimTrack, []string{"t2"})
		if err != nil {
			t.Fatal(err)
		}
		if got["t2"].ImageURL != "" || got["t2"].ThumbURL != "" {
			t.Errorf("invented artwork: %+v", got["t2"])
		}
	})
}
