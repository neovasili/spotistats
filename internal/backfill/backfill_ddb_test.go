package backfill_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/neovasili/spotistats/internal/backfill"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/spotify"
	"github.com/neovasili/spotistats/internal/store"
	"github.com/neovasili/spotistats/internal/store/storetest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Shutdown()
	os.Exit(code)
}

// fakeAPI answers TrackDetail from a fixed catalogue, counting calls.
type fakeAPI struct {
	detail map[string]spotify.TrackDetail
	calls  []string
	err    error
}

func (f *fakeAPI) TrackDetail(_ context.Context, id string) (spotify.TrackDetail, bool, error) {
	f.calls = append(f.calls, id)
	if f.err != nil {
		return spotify.TrackDetail{}, false, f.err
	}
	d, ok := f.detail[id]
	return d, ok, nil
}

func detailFor(trackID, trackName, albumID, albumName, artistID, artistName string) spotify.TrackDetail {
	return spotify.TrackDetail{
		Track: model.Track{
			ID: trackID, Name: trackName, AlbumID: albumID,
			ArtistIDs: []string{artistID}, DurationMs: 240_000,
		},
		Album:   model.Album{ID: albumID, Name: albumName, ArtistIDs: []string{artistID}},
		Artists: []model.Artist{{ID: artistID, Name: artistName}},
	}
}

func newAPI() *fakeAPI {
	return &fakeAPI{detail: map[string]spotify.TrackDetail{
		"t1": detailFor("t1", "Stand My Ground", "al1", "The Silent Force", "ar1", "Within Temptation"),
		"t2": detailFor("t2", "Angels", "al2", "Century Child", "ar2", "Nightwish"),
	}}
}

// TestEnrichPopulatesAllThreeDimensions: one request per track must yield the track, its album
// and its artists, because the export supplies none of their IDs and re-fetching albums and
// artists separately would triple the slowest phase.
func TestEnrichPopulatesAllThreeDimensions(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	api := newAPI()

	en := backfill.NewEnricher(st, api, nil)
	stats, err := en.Enrich(ctx, []string{"t1", "t2"}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Fetched != 2 || stats.TracksWritten != 2 {
		t.Errorf("stats = %+v, want 2 fetched and written", stats)
	}
	if stats.AlbumsWritten != 2 || stats.ArtistsWritten != 2 {
		t.Errorf("albums/artists = %d/%d, want 2/2", stats.AlbumsWritten, stats.ArtistsWritten)
	}

	// The payoff: a track resolves to a full label without any further API call.
	labels, err := st.ResolveLabels(ctx, model.DimTrack, []string{"t1"})
	if err != nil {
		t.Fatal(err)
	}
	l := labels["t1"]
	if l.Name != "Stand My Ground" || l.AlbumName != "The Silent Force" ||
		l.ArtistName != "Within Temptation" {
		t.Errorf("label = %+v, want the full context", l)
	}
}

// The dimension rows are the cursor: rerunning must not re-request what is already resolved.
// Without this the pass is not resumable and an interrupted 14,000-request run starts over.
func TestEnrichIsResumable(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	api := newAPI()
	en := backfill.NewEnricher(st, api, nil)

	if _, err := en.Enrich(ctx, []string{"t1"}, 0, nil); err != nil {
		t.Fatal(err)
	}
	before := len(api.calls)

	stats, err := en.Enrich(ctx, []string{"t1", "t2"}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.AlreadyKnown != 1 {
		t.Errorf("AlreadyKnown = %d, want 1", stats.AlreadyKnown)
	}
	if got := len(api.calls) - before; got != 1 {
		t.Errorf("made %d calls on the second pass, want only the unresolved track", got)
	}
}

// A --enrich-limit run must report exactly what is left, or an operator cannot tell a finished
// pass from a truncated one.
func TestEnrichLimitReportsRemaining(t *testing.T) {
	st := storetest.NewStore(t)
	api := newAPI()
	en := backfill.NewEnricher(st, api, nil)

	stats, err := en.Enrich(context.Background(), []string{"t1", "t2"}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Fetched != 1 {
		t.Errorf("Fetched = %d, want 1", stats.Fetched)
	}
	if stats.Remaining != 1 {
		t.Errorf("Remaining = %d, want 1", stats.Remaining)
	}
}

// A track Spotify no longer knows must be tombstoned, not retried forever.
func TestEnrichTombstonesUnresolvableTracks(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	api := newAPI()
	en := backfill.NewEnricher(st, api, nil)

	stats, err := en.Enrich(ctx, []string{"t-gone"}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Unresolvable != 1 {
		t.Errorf("Unresolvable = %d, want 1", stats.Unresolvable)
	}
	// And a rerun must not ask again.
	before := len(api.calls)
	if _, err := en.Enrich(ctx, []string{"t-gone"}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != before {
		t.Error("re-requested a tombstoned track; a dead ID would burn a request every run")
	}
}

func TestImportWritesExactDurations(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// Enrich first, exactly as the CLI orders it: a play row denormalises its album and artist
	// IDs, so importing before resolving loses attribution permanently.
	if _, err := backfill.NewEnricher(st, newAPI(), nil).
		Enrich(ctx, []string{"t1"}, 0, nil); err != nil {
		t.Fatal(err)
	}

	im := backfill.NewImporter(backfill.ImportConfig{Store: st, MinMs: 30_000})
	files := []string{"testdata/sample.json"}
	res, err := im.Import(ctx, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.PlaysWritten != 1 {
		t.Fatalf("PlaysWritten = %d, want 1 (only one sample record is importable)", res.PlaysWritten)
	}

	var got []model.Play
	for p, err := range st.Plays(ctx, res.FirstPlayedAt.Add(-time.Hour),
		res.LastPlayedAt.Add(time.Hour), store.PlayFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	if len(got) != 1 {
		t.Fatalf("stored plays = %d, want 1", len(got))
	}
	p := got[0]
	if p.Source != model.SourceExport {
		t.Errorf("Source = %q, want export", p.Source)
	}
	if p.MsEstimated {
		t.Error("an export play must be exact, never estimated")
	}
	if p.MsPlayed != 240_000 {
		t.Errorf("MsPlayed = %d, want the exact 240000 from the export", p.MsPlayed)
	}
	// Attribution came from the enriched track row, not from the export.
	if p.AlbumID != "al1" || len(p.ArtistIDs) != 1 || p.ArtistIDs[0] != "ar1" {
		t.Errorf("attribution = album %q artists %v, want al1/[ar1]", p.AlbumID, p.ArtistIDs)
	}
}

// Importing twice must not double the totals. BatchWriteItem cannot do conditional writes, so
// this relies on the row being keyed by (playedAt, trackID) and fully derived from the record.
func TestImportIsIdempotent(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	im := backfill.NewImporter(backfill.ImportConfig{Store: st, MinMs: 30_000})
	files := []string{"testdata/sample.json"}

	for range 2 {
		if _, err := im.Import(ctx, files, nil); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, err := range st.Plays(ctx,
		mustTime(t, "2015-01-01T00:00:00.000Z"), mustTime(t, "2015-12-31T00:00:00.000Z"),
		store.PlayFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 1 {
		t.Errorf("stored plays after two imports = %d, want 1", n)
	}
}

// A dry run must write nothing at all.
func TestImportDryRunWritesNothing(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	im := backfill.NewImporter(backfill.ImportConfig{Store: st, MinMs: 30_000, DryRun: true})

	if _, err := im.Import(ctx, []string{"testdata/sample.json"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, err := range st.Plays(ctx,
		mustTime(t, "2015-01-01T00:00:00.000Z"), mustTime(t, "2015-12-31T00:00:00.000Z"),
		store.PlayFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("a dry run wrote a play row")
	}
}

// TestSupersededWindowIsNotTheMonth guards the destructive version of the precedence rule.
//
// docs/SPECS.md 4.2 originally deleted every api-sourced play in any month an export claimed.
// The export ends mid-August and capture began the next day, so a month rule would delete
// captured plays that exist in no other source.
func TestSupersededWindowIsNotTheMonth(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// An API play inside the export window, and one after it.
	inside := storetest.APIPlay(t, "2026-08-10T10:00:00.000Z", "t1")
	after := storetest.APIPlay(t, "2026-08-25T10:00:00.000Z", "t1")
	for _, p := range []model.Play{inside, after} {
		if _, err := st.PutPlay(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	sup, err := backfill.SupersededAPIPlays(ctx, st,
		mustTime(t, "2026-08-01T00:00:00.000Z"), mustTime(t, "2026-08-21T22:02:01.000Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sup) != 1 {
		t.Fatalf("superseded = %d, want only the play inside the window", len(sup))
	}
	if !sup[0].PlayedAt.Equal(inside.PlayedAt) {
		t.Errorf("superseded the wrong play: %s", model.FormatTS(sup[0].PlayedAt))
	}
}

// An unbounded window must supersede nothing, so a missing flag cannot mean "delete everything".
func TestSupersededRequiresABoundedWindow(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	if _, err := st.PutPlay(ctx, storetest.APIPlay(t, "2026-08-10T10:00:00.000Z", "t1")); err != nil {
		t.Fatal(err)
	}
	for _, w := range [][2]time.Time{
		{{}, {}},
		{mustTime(t, "2026-08-01T00:00:00.000Z"), {}},
		// Inverted: to before from.
		{mustTime(t, "2026-08-21T00:00:00.000Z"), mustTime(t, "2026-08-01T00:00:00.000Z")},
	} {
		got, err := backfill.SupersededAPIPlays(ctx, st, w[0], w[1])
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("window %v returned %d plays; an unusable window must match nothing", w, len(got))
		}
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := model.ParseTS(s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// TestImportGivesFullAttributionWithoutTheAPI is the point of the whole name-key design.
//
// Before it, importing 400,000 plays produced correct totals and 15% artist attribution,
// because the export supplies artist names but no IDs and resolving 13,000 tracks is a
// weeks-long job under a dev-mode quota. Every play must be attributed from the export alone.
func TestImportGivesFullAttributionWithoutTheAPI(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// No enrichment at all: this is the "API is rate-limited" case.
	im := backfill.NewImporter(backfill.ImportConfig{Store: st, MinMs: 30_000})
	res, err := im.Import(ctx, []string{"testdata/sample.json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.PlaysWritten != 1 {
		t.Fatalf("PlaysWritten = %d, want 1", res.PlaysWritten)
	}

	// The play carries the export's names, which is what late binding resolves from.
	var p model.Play
	for got, err := range st.Plays(ctx,
		mustTime(t, "2015-01-01T00:00:00.000Z"), mustTime(t, "2015-12-31T00:00:00.000Z"),
		store.PlayFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		p = got
	}
	if p.Export.ArtistName != "Within Temptation" || p.Export.AlbumName != "The Silent Force" {
		t.Errorf("export names not stored: %+v", p.Export)
	}
	if p.Export.TrackName != "Stand My Ground" {
		t.Errorf("TrackName = %q", p.Export.TrackName)
	}

	// Attribution resolves to name keys, so the play reaches an artist and an album row.
	facts := model.FactsForTrack(p, model.Track{}, nil)
	if len(facts.ArtistIDs) != 1 || !model.IsNameKey(facts.ArtistIDs[0]) {
		t.Errorf("ArtistIDs = %v, want one name key", facts.ArtistIDs)
	}
	if !model.IsNameKey(facts.AlbumID) {
		t.Errorf("AlbumID = %q, want a name key", facts.AlbumID)
	}

	// And the placeholder rows exist, so the dashboard shows names rather than "nm:..." keys.
	labels, err := st.ResolveLabels(ctx, model.DimArtist, facts.ArtistIDs)
	if err != nil {
		t.Fatal(err)
	}
	if got := labels[facts.ArtistIDs[0]].Name; got != "Within Temptation" {
		t.Errorf("artist label = %q, want the export's original casing", got)
	}
	trackLabels, err := st.ResolveLabels(ctx, model.DimTrack, []string{"t1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := trackLabels["t1"]; got.Name != "Stand My Ground" ||
		got.ArtistName != "Within Temptation" || got.AlbumName != "The Silent Force" {
		t.Errorf("track label = %+v, want full context from the export", got)
	}
}

// A placeholder track must still count as unresolved, or enrichment would never upgrade it off
// fallback identity — which is the state enrichment exists to escape.
func TestPlaceholderTracksRemainEnrichable(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	im := backfill.NewImporter(backfill.ImportConfig{Store: st, MinMs: 30_000})
	if _, err := im.Import(ctx, []string{"testdata/sample.json"}, nil); err != nil {
		t.Fatal(err)
	}

	api := newAPI()
	stats, err := backfill.NewEnricher(st, api, nil).Enrich(ctx, []string{"t1"}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Fetched != 1 {
		t.Fatalf("Fetched = %d; the placeholder was mistaken for a resolved track", stats.Fetched)
	}

	// After enrichment the track carries real Spotify IDs, so a reconcile switches to them.
	got, err := st.GetTracks(ctx, []string{"t1"})
	if err != nil {
		t.Fatal(err)
	}
	tr := got["t1"]
	if model.IsNameKey(tr.AlbumID) || len(tr.ArtistIDs) == 0 || model.IsNameKey(tr.ArtistIDs[0]) {
		t.Errorf("still name-keyed after enrichment: %+v", tr)
	}
	// And an already-enriched track is not re-requested.
	before := len(api.calls)
	if _, err := backfill.NewEnricher(st, api, nil).Enrich(ctx, []string{"t1"}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != before {
		t.Error("re-requested a fully resolved track")
	}
}
