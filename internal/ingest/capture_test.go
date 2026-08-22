package ingest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/neovasili/spotistats/internal/ingest"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/spotify"
	"github.com/neovasili/spotistats/internal/store"
	"github.com/neovasili/spotistats/internal/store/storetest"
)

func mustTS(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := model.ParseTS(s)
	if err != nil {
		t.Fatalf("ParseTS(%q): %v", s, err)
	}
	return ts
}

func newCapturer(t *testing.T, api ingest.SpotifyAPI, st *store.Store, limit int) *ingest.Capturer {
	t.Helper()
	c, err := ingest.New(ingest.Config{
		Spotify: api, Store: st, Limit: limit,
		Now: func() time.Time { return storetest.FixedNow },
	})
	if err != nil {
		t.Fatalf("ingest.New: %v", err)
	}
	return c
}

func TestNewValidation(t *testing.T) {
	if _, err := ingest.New(ingest.Config{}); err == nil {
		t.Error("New accepted an empty config")
	}
	if _, err := ingest.New(ingest.Config{Spotify: &fakeSpotify{}}); err == nil {
		t.Error("New accepted a config with no store")
	}
}

// TestCaptureFirstRun: an empty cursor means "fetch the most recent page", so no `after` is
// sent, and the cursor must end up at the newest play.
func TestCaptureFirstRun(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	p1 := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar1"))
	p2 := storetest.APIPlay(t, "2025-03-14T11:00:00.000Z", "t2", storetest.WithArtists("ar3"))
	api := &fakeSpotify{
		pages:   []spotify.RecentlyPlayedPage{pageOf(t, 50, p1, p2)},
		artists: artistsWithGenres(),
	}

	res, err := newCapturer(t, api, st, 50).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Fetched != 2 || res.Inserted != 2 || res.Duplicates != 0 {
		t.Errorf("result = fetched %d inserted %d dup %d, want 2/2/0",
			res.Fetched, res.Inserted, res.Duplicates)
	}
	if after := api.RequestedAfter(); len(after) != 1 || !after[0].IsZero() {
		t.Errorf("requested after = %v, want a single zero cursor on the first run", after)
	}
	if !res.CursorAdvancedTo.Equal(p2.PlayedAt) {
		t.Errorf("cursor = %v, want the newest play %v", res.CursorAdvancedTo, p2.PlayedAt)
	}

	cursor, err := st.GetPollCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.LastPlayedAt.Equal(p2.PlayedAt) {
		t.Errorf("stored cursor = %v, want %v", cursor.LastPlayedAt, p2.PlayedAt)
	}
	if cursor.LastStatus != "ok" {
		t.Errorf("status = %q, want ok", cursor.LastStatus)
	}
}

// The second run must send the cursor the first one stored.
func TestCaptureUsesStoredCursor(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	p1 := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar1"))
	p2 := storetest.APIPlay(t, "2025-03-14T12:00:00.000Z", "t2", storetest.WithArtists("ar1"))
	api := &fakeSpotify{
		pages:   []spotify.RecentlyPlayedPage{pageOf(t, 50, p1), pageOf(t, 50, p2)},
		artists: artistsWithGenres(),
	}
	c := newCapturer(t, api, st, 50)

	if _, err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}

	after := api.RequestedAfter()
	if len(after) != 2 {
		t.Fatalf("calls = %d, want 2", len(after))
	}
	if !after[0].IsZero() {
		t.Errorf("first call sent after=%v, want zero", after[0])
	}
	if !after[1].Equal(p1.PlayedAt) {
		t.Errorf("second call sent after=%v, want the first run's newest play %v", after[1], p1.PlayedAt)
	}
}

// TestCaptureReplayIsIdempotent: an overlapping window is the normal case, and it must not
// inflate any aggregate.
func TestCaptureReplayIsIdempotent(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	p := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar1"))
	page := pageOf(t, 50, p)
	api := &fakeSpotify{pages: []spotify.RecentlyPlayedPage{page, page}, artists: artistsWithGenres()}
	c := newCapturer(t, api, st, 50)

	first, err := c.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Inserted != 1 {
		t.Fatalf("first run inserted %d, want 1", first.Inserted)
	}

	totalKey := model.AggKey{Dim: model.DimTotal, Period: model.PeriodAll, EntityID: model.TotalEntityID}
	before, err := st.GetAggregate(ctx, totalKey)
	if err != nil {
		t.Fatal(err)
	}

	second, err := c.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Inserted != 0 || second.Duplicates != 1 {
		t.Errorf("second run inserted %d dup %d, want 0/1", second.Inserted, second.Duplicates)
	}
	if second.DeltasApplied != 0 {
		t.Errorf("second run applied %d deltas; a duplicate must not touch aggregates",
			second.DeltasApplied)
	}

	after, err := st.GetAggregate(ctx, totalKey)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(before, after); diff != "" {
		t.Errorf("aggregates changed on replay (-before +after):\n%s", diff)
	}
}

// TestCaptureGenreAttributionForNewArtist is the regression test for the ordering fix.
//
// docs/SPECS.md 4.1 originally enriched artists AFTER recording plays. Genres live on the
// artist object, so a brand-new artist would have no row when its play was recorded, and
// the play would contribute zero genre deltas -- silently, forever.
func TestCaptureGenreAttributionForNewArtist(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// ar1 has never been seen before: no artist row exists.
	if _, err := st.GetArtist(ctx, "ar1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("precondition: ar1 should be unknown, got %v", err)
	}

	p := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar1"))
	api := &fakeSpotify{pages: []spotify.RecentlyPlayedPage{pageOf(t, 50, p)}, artists: artistsWithGenres()}

	res, err := newCapturer(t, api, st, 50).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.GenresDegraded {
		t.Fatal("GenresDegraded = true despite the artist resolving fine")
	}
	if res.ArtistsWritten != 1 {
		t.Errorf("ArtistsWritten = %d, want 1", res.ArtistsWritten)
	}

	// ar1 carries "symphonic metal" and "gothic metal", so both genre rows must exist.
	for _, g := range []string{"symphonic metal", "gothic metal"} {
		agg, err := st.GetAggregate(ctx, model.AggKey{
			Dim: model.DimGenre, Period: model.PeriodAll, EntityID: g,
		})
		if err != nil {
			t.Errorf("genre %q has no aggregate; a first-ever artist lost its genre "+
				"attribution (enrichment must precede recording): %v", g, err)
			continue
		}
		if agg.Plays != 1 {
			t.Errorf("genre %q plays = %d, want 1", g, agg.Plays)
		}
	}
}

// TestCaptureDegradesRatherThanLosingPlays: if artist resolution fails we still record the
// plays, because the endpoint retains only ~50 and a rolled window loses them permanently,
// whereas missing genre aggregates are repaired by the nightly reconcile.
func TestCaptureDegradesRatherThanLosingPlays(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	p := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar1"))
	api := &fakeSpotify{
		pages:      []spotify.RecentlyPlayedPage{pageOf(t, 50, p)},
		artistsErr: errors.New("429 rate limited"),
	}

	res, err := newCapturer(t, api, st, 50).Run(ctx)
	if err != nil {
		t.Fatalf("Run must not fail when only genre resolution failed: %v", err)
	}
	if !res.GenresDegraded {
		t.Error("GenresDegraded = false despite artist resolution failing")
	}
	// The play itself is stored, and the cursor advanced.
	if res.Inserted != 1 {
		t.Errorf("Inserted = %d, want 1 -- the play must not be lost", res.Inserted)
	}
	if !res.CursorAdvancedTo.Equal(p.PlayedAt) {
		t.Error("cursor did not advance; the window would be re-read forever")
	}
	// Track and total aggregates are complete...
	if agg, err := st.GetAggregate(ctx, model.AggKey{
		Dim: model.DimTrack, Period: model.PeriodAll, EntityID: "t1",
	}); err != nil || agg.Plays != 1 {
		t.Errorf("track aggregate = %+v, %v", agg, err)
	}
	// ...only the genre rows are missing, which reconcile repairs.
	if _, err := st.GetAggregate(ctx, model.AggKey{
		Dim: model.DimGenre, Period: model.PeriodAll, EntityID: "gothic metal",
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected no genre aggregate in degraded mode, got %v", err)
	}
	// The degradation is recorded on the cursor so an operator can see it happened.
	cursor, err := st.GetPollCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.LastStatus != "ok-degraded-genres" {
		t.Errorf("status = %q, want it to record the degradation", cursor.LastStatus)
	}
}

// TestCaptureSaturationWritesGapMarker: a full page means listening may have outrun the
// interval, and the endpoint cannot page back, so the loss would be permanent.
func TestCaptureSaturationWritesGapMarker(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// A limit of 2 with 2 plays reproduces saturation without a 50-play fixture.
	p1 := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar1"))
	p2 := storetest.APIPlay(t, "2025-03-14T11:00:00.000Z", "t2", storetest.WithArtists("ar1"))
	api := &fakeSpotify{pages: []spotify.RecentlyPlayedPage{pageOf(t, 2, p1, p2)}, artists: artistsWithGenres()}

	res, err := newCapturer(t, api, st, 2).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Saturated || !res.GapRecorded {
		t.Errorf("saturated=%v gapRecorded=%v, want both true", res.Saturated, res.GapRecorded)
	}

	var markers []model.GapMarker
	for g, err := range st.GapMarkers(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		markers = append(markers, g)
	}
	if len(markers) != 1 {
		t.Fatalf("gap markers = %d, want 1", len(markers))
	}
	if markers[0].ItemsReturned != 2 || markers[0].Limit != 2 {
		t.Errorf("marker = %+v", markers[0])
	}
	if !markers[0].DetectedAt.Equal(storetest.FixedNow) {
		t.Errorf("DetectedAt = %v, want the run clock", markers[0].DetectedAt)
	}
}

func TestCaptureNoGapMarkerOnPartialPage(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	p := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar1"))
	api := &fakeSpotify{pages: []spotify.RecentlyPlayedPage{pageOf(t, 50, p)}, artists: artistsWithGenres()}

	res, err := newCapturer(t, api, st, 50).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Saturated || res.GapRecorded {
		t.Errorf("a 1-of-50 page must not be saturated: %+v", res)
	}
}

func TestCaptureEmptyPage(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	api := &fakeSpotify{pages: []spotify.RecentlyPlayedPage{pageOf(t, 50)}}

	res, err := newCapturer(t, api, st, 50).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fetched != 0 || res.Inserted != 0 {
		t.Errorf("result = %+v, want nothing ingested", res)
	}
	// No artist call is warranted when there is nothing to attribute.
	if calls := api.ArtistCalls(); len(calls) != 0 {
		t.Errorf("artist calls = %v, want none for an empty page", calls)
	}
	// The run still records that it happened.
	cursor, err := st.GetPollCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.LastRunAt.IsZero() {
		t.Error("LastRunAt not stamped on an empty run")
	}
}

// TestCaptureFetchFailureLeavesCursorAlone: nothing was ingested, so the cursor must not
// move or the window would be skipped.
func TestCaptureFetchFailureLeavesCursorAlone(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	api := &fakeSpotify{pageErrs: []error{errors.New("503 service unavailable")}}

	if _, err := newCapturer(t, api, st, 50).Run(ctx); err == nil {
		t.Fatal("Run succeeded despite the fetch failing")
	}
	cursor, err := st.GetPollCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.LastPlayedAt.IsZero() {
		t.Errorf("cursor = %v, want it untouched after a failed fetch", cursor.LastPlayedAt)
	}
}

// TestCaptureCursorNeverRewinds: Spotify's ordering guarantees are unstated, so a page whose
// newest item predates the stored cursor must not move it backwards.
func TestCaptureCursorNeverRewinds(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	ahead := mustTS(t, "2025-06-01T00:00:00.000Z")
	if err := st.PutPollCursor(ctx, model.PollCursor{LastPlayedAt: ahead}); err != nil {
		t.Fatal(err)
	}

	old := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar1"))
	api := &fakeSpotify{pages: []spotify.RecentlyPlayedPage{pageOf(t, 50, old)}, artists: artistsWithGenres()}

	res, err := newCapturer(t, api, st, 50).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.CursorAdvancedTo.Equal(ahead) {
		t.Errorf("cursor = %v, want it held at %v", res.CursorAdvancedTo, ahead)
	}
	cursor, err := st.GetPollCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.LastPlayedAt.Equal(ahead) {
		t.Errorf("stored cursor rewound to %v", cursor.LastPlayedAt)
	}
	// The play is still ingested; only the cursor is pinned.
	if res.Inserted != 1 {
		t.Errorf("Inserted = %d, want 1", res.Inserted)
	}
}

// TestCaptureTombstonesUnresolvableArtist: without a tombstone the enrichment pass would
// re-request the same dead ID on every run forever.
func TestCaptureTombstonesUnresolvableArtist(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	p := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("deadartist"))
	// The fake's catalogue does not contain it, so it is reported missing.
	api := &fakeSpotify{pages: []spotify.RecentlyPlayedPage{pageOf(t, 50, p), pageOf(t, 50, p)}, artists: artistsWithGenres()}
	c := newCapturer(t, api, st, 50)

	res, err := c.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tombstoned != 1 {
		t.Errorf("Tombstoned = %d, want 1", res.Tombstoned)
	}
	got, err := st.GetArtist(ctx, "deadartist")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Missing {
		t.Error("artist row is not a tombstone")
	}

	// A second run must not ask about it again.
	if _, err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}
	calls := api.ArtistCalls()
	if len(calls) != 1 {
		t.Errorf("artist calls = %v, want exactly one -- a tombstoned ID must not be re-requested", calls)
	}
}

// TestCaptureSkipsKnownFreshArtists: quota is scarce in development mode, so a known artist
// must not be re-fetched.
func TestCaptureSkipsKnownFreshArtists(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	if err := st.PutArtist(ctx, model.Artist{
		ID: "ar1", Name: "Within Temptation", Genres: []string{"symphonic metal"},
	}); err != nil {
		t.Fatal(err)
	}

	p := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar1"))
	api := &fakeSpotify{pages: []spotify.RecentlyPlayedPage{pageOf(t, 50, p)}, artists: artistsWithGenres()}

	res, err := newCapturer(t, api, st, 50).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls := api.ArtistCalls(); len(calls) != 0 {
		t.Errorf("artist calls = %v, want none for an already-fresh artist", calls)
	}
	if res.ArtistsWritten != 0 {
		t.Errorf("ArtistsWritten = %d, want 0", res.ArtistsWritten)
	}
	// The stored genre is still applied.
	if agg, err := st.GetAggregate(ctx, model.AggKey{
		Dim: model.DimGenre, Period: model.PeriodAll, EntityID: "symphonic metal",
	}); err != nil || agg.Plays != 1 {
		t.Errorf("genre aggregate = %+v, %v", agg, err)
	}
}

// TestCaptureWritesEmbeddedMetadataWithoutExtraCalls: recently-played embeds full track and
// simplified album objects, so persisting them costs nothing.
func TestCaptureWritesEmbeddedMetadata(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	p := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1",
		storetest.WithArtists("ar1"), storetest.WithAlbum("al1"))
	api := &fakeSpotify{pages: []spotify.RecentlyPlayedPage{pageOf(t, 50, p)}, artists: artistsWithGenres()}

	res, err := newCapturer(t, api, st, 50).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.TracksWritten != 1 || res.AlbumsWritten != 1 {
		t.Errorf("written tracks=%d albums=%d, want 1/1", res.TracksWritten, res.AlbumsWritten)
	}

	tr, err := st.GetTrack(ctx, "t1")
	if err != nil {
		t.Fatalf("track metadata missing: %v", err)
	}
	if tr.Name != "Track t1" {
		t.Errorf("track name = %q", tr.Name)
	}
	al, err := st.GetAlbum(ctx, "al1")
	if err != nil {
		t.Fatalf("album metadata missing: %v", err)
	}
	if al.ReleaseDatePrecision != "day" {
		t.Errorf("album precision = %q", al.ReleaseDatePrecision)
	}

	// A second run must not rewrite fresh metadata.
	api.pages = append(api.pages, pageOf(t, 50, p))
	res2, err := newCapturer(t, api, st, 50).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res2.TracksWritten != 0 || res2.AlbumsWritten != 0 {
		t.Errorf("second run rewrote fresh metadata: tracks=%d albums=%d",
			res2.TracksWritten, res2.AlbumsWritten)
	}
}

// Multi-artist plays must get the deduplicated union of their artists' genres.
func TestCaptureDedupesGenresAcrossArtists(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// ar1 = {symphonic metal, gothic metal}, ar2 = {gothic metal}.
	p := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar1", "ar2"))
	api := &fakeSpotify{pages: []spotify.RecentlyPlayedPage{pageOf(t, 50, p)}, artists: artistsWithGenres()}

	if _, err := newCapturer(t, api, st, 50).Run(ctx); err != nil {
		t.Fatal(err)
	}
	// "gothic metal" is shared, so it must count once, not twice.
	agg, err := st.GetAggregate(ctx, model.AggKey{
		Dim: model.DimGenre, Period: model.PeriodAll, EntityID: "gothic metal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if agg.Plays != 1 {
		t.Errorf("shared genre plays = %d, want 1 (deduplicated across artists)", agg.Plays)
	}
}

// The canonical query must work end to end through the pipeline.
func TestCapturePeriodKeysAreLocal(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// 23:30Z on New Year's Eve is already 2026 in Madrid.
	p := storetest.APIPlay(t, "2025-12-31T23:30:00.000Z", "t1", storetest.WithArtists("ar1"))
	api := &fakeSpotify{pages: []spotify.RecentlyPlayedPage{pageOf(t, 50, p)}, artists: artistsWithGenres()}

	if _, err := newCapturer(t, api, st, 50).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAggregate(ctx, model.AggKey{
		Dim: model.DimArtist, Period: "2026", EntityID: "ar1",
	}); err != nil {
		t.Errorf("expected the play under local year 2026: %v", err)
	}
	if _, err := st.GetAggregate(ctx, model.AggKey{
		Dim: model.DimArtist, Period: "2025", EntityID: "ar1",
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("play also filed under UTC year 2025; period keys must be local (%v)", err)
	}
}

// TestCaptureRespectsFreshTombstone: tombstones expiring must not degrade into "no negative
// cache at all", which would re-request known-dead IDs on every single run and burn
// development-mode rate-limit budget.
func TestCaptureRespectsFreshTombstone(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	if err := st.PutMissing(ctx, model.DimArtist, "ar-dead"); err != nil {
		t.Fatal(err)
	}

	p := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar-dead"))
	api := &fakeSpotify{pages: []spotify.RecentlyPlayedPage{pageOf(t, 50, p)}}

	if _, err := newCapturer(t, api, st, 50).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := api.ArtistCalls(); len(calls) != 0 {
		t.Errorf("artist calls = %v, want none: the tombstone is still fresh", calls)
	}
}

// TestCaptureNamesArtistsWhenEnrichmentFails reproduces the exact production failure that put
// 31 raw Spotify IDs on the dashboard: GET /v1/artists errored, so no artist row was written
// at all -- even though the name was sitting in the recently-played payload we already had.
//
// Names must come from the embedded object and survive a failed enrichment pass. The run is
// still reported as degraded, because the genres genuinely are missing.
func TestCaptureNamesArtistsWhenEnrichmentFails(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	p := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar1", "ar2"))
	api := &fakeSpotify{
		pages:      []spotify.RecentlyPlayedPage{pageOf(t, 50, p)},
		artistsErr: errors.New("503 from GET /v1/artists"),
	}

	res, err := newCapturer(t, api, st, 50).Run(ctx)
	if err != nil {
		t.Fatalf("capture must not fail when only enrichment does: %v", err)
	}
	if !res.GenresDegraded {
		t.Error("GenresDegraded = false, but the artist fetch failed")
	}
	if res.ArtistsNamed != 2 {
		t.Errorf("ArtistsNamed = %d, want 2", res.ArtistsNamed)
	}

	// The payoff: both artists have a display name despite the failure.
	got, err := st.GetArtists(ctx, []string{"ar1", "ar2"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ar1", "ar2"} {
		a, ok := got[id]
		if !ok {
			t.Errorf("artist %s has no row; the dashboard would render its raw ID", id)
			continue
		}
		if a.Name != "Artist "+id {
			t.Errorf("artist %s name = %q, want %q", id, a.Name, "Artist "+id)
		}
		// ...and it is not mistaken for enriched, or genres would never be fetched.
		if !a.EnrichedAt.IsZero() {
			t.Errorf("artist %s has enrichedAt set from a name-only stub; the genre pass "+
				"would skip it forever", id)
		}
	}
}

// TestCaptureEnrichesAfterNameOnlyStub: the follow-up run must still fetch genres for an
// artist that was named from the embedded object. This is what the enrichedAt/refreshedAt
// split exists for -- gating on row age alone would suppress the fetch permanently.
func TestCaptureEnrichesAfterNameOnlyStub(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	p := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar1"))

	// Run 1: enrichment fails, the name lands.
	failing := &fakeSpotify{
		pages:      []spotify.RecentlyPlayedPage{pageOf(t, 50, p)},
		artistsErr: errors.New("503"),
	}
	if _, err := newCapturer(t, failing, st, 50).Run(ctx); err != nil {
		t.Fatal(err)
	}

	// Run 2: a later play by the same artist, enrichment now working.
	p2 := storetest.APIPlay(t, "2025-03-14T11:00:00.000Z", "t2", storetest.WithArtists("ar1"))
	working := &fakeSpotify{
		pages:   []spotify.RecentlyPlayedPage{pageOf(t, 50, p2)},
		artists: artistsWithGenres(),
	}
	res, err := newCapturer(t, working, st, 50).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.GenresDegraded {
		t.Error("GenresDegraded = true on the recovery run")
	}
	if calls := working.ArtistCalls(); len(calls) == 0 {
		t.Fatal("no artist fetch on the second run: a name-only stub suppressed enrichment, " +
			"so genres would never arrive")
	}

	got, err := st.GetArtists(ctx, []string{"ar1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["ar1"].Genres) == 0 {
		t.Error("artist still has no genres after a successful enrichment run")
	}
	if got["ar1"].Name == "" {
		t.Error("enrichment erased the name")
	}
}

// TestCaptureNameStubDoesNotClobberEnrichedRow: PutArtistName is an UpdateItem precisely so a
// simplified object's empty fields cannot overwrite genres. A Put would undo enrichment on
// every single poll.
func TestCaptureNameStubDoesNotClobberEnrichedRow(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	if err := st.PutArtist(ctx, model.Artist{
		ID: "ar1", Name: "Within Temptation", Genres: []string{"symphonic metal"},
		Popularity: 71, ImageURL: "https://example.test/ar1.jpg",
	}); err != nil {
		t.Fatal(err)
	}

	p := storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1", storetest.WithArtists("ar1"))
	api := &fakeSpotify{
		pages:      []spotify.RecentlyPlayedPage{pageOf(t, 50, p)},
		artistsErr: errors.New("503"),
	}
	if _, err := newCapturer(t, api, st, 50).Run(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetArtists(ctx, []string{"ar1"})
	if err != nil {
		t.Fatal(err)
	}
	a := got["ar1"]
	// Assert the stub write actually RAN before checking what it preserved: an update that
	// silently failed would preserve everything and pass this test vacuously, which is how
	// the reserved-keyword bug in PutArtistName initially went unnoticed here.
	if a.Name != "Artist ar1" {
		t.Fatalf("name = %q, want the embedded name: the stub write did not run, so this "+
			"test proves nothing about clobbering", a.Name)
	}
	if len(a.Genres) != 1 || a.Genres[0] != "symphonic metal" {
		t.Errorf("genres = %v, want the enriched value preserved", a.Genres)
	}
	if a.ImageURL == "" {
		t.Error("imageUrl erased by a name-only stub")
	}
	if a.Popularity != 71 {
		t.Errorf("popularity = %d, want 71 preserved", a.Popularity)
	}
}
