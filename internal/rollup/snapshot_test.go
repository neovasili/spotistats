package rollup_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/rollup"
	"github.com/neovasili/spotistats/internal/store"
	"github.com/neovasili/spotistats/internal/store/storetest"
)

// runFull runs the whole nightly job into a temp directory and returns the parsed dashboard.
func runFull(t *testing.T, st *store.Store) (rollup.Dashboard, string) {
	t.Helper()
	dir := t.TempDir()
	r := newRollup(t, st, func(c *rollup.Config) {
		c.Publisher = rollup.NewDirPublisher(dir)
	})
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SnapshotsWritten != 3 {
		t.Fatalf("snapshots = %d, want 3", res.SnapshotsWritten)
	}

	body, err := os.ReadFile(filepath.Join(dir, rollup.FileDashboard))
	if err != nil {
		t.Fatal(err)
	}
	var d rollup.Dashboard
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatalf("dashboard.json is not valid JSON: %v", err)
	}
	return d, dir
}

func TestLeaderboardsAreMaterialised(t *testing.T) {
	st := seedCorpus(t)
	ctx := context.Background()

	n, _, err := newRollup(t, st).RefreshLeaderboards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no leaderboards written")
	}

	board, err := st.GetLeaderboard(ctx, model.DimArtist, model.PeriodAll)
	if err != nil {
		t.Fatalf("all-time artist board missing: %v", err)
	}
	if len(board.Entries) == 0 {
		t.Fatal("board is empty")
	}
	// Descending by minutes, with names resolved so the dashboard needs no further lookups.
	for i := 1; i < len(board.Entries); i++ {
		if board.Entries[i-1].MsPlayed < board.Entries[i].MsPlayed {
			t.Errorf("board not descending at %d", i)
		}
	}
	for _, e := range board.Entries {
		if e.Name == "" {
			t.Errorf("entry %s has no name; the dashboard would show a raw ID", e.ID)
		}
	}
	if board.Metric != "ms" {
		t.Errorf("Metric = %q, want ms", board.Metric)
	}
}

// A board must be stable between runs, or the dashboard appears to change when nothing did.
func TestLeaderboardsAreDeterministic(t *testing.T) {
	st := seedCorpus(t)
	ctx := context.Background()
	r := newRollup(t, st)

	if _, _, err := r.RefreshLeaderboards(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := st.GetLeaderboard(ctx, model.DimTrack, model.PeriodAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.RefreshLeaderboards(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := st.GetLeaderboard(ctx, model.DimTrack, model.PeriodAll)
	if err != nil {
		t.Fatal(err)
	}

	if len(first.Entries) != len(second.Entries) {
		t.Fatalf("entry count changed: %d then %d", len(first.Entries), len(second.Entries))
	}
	for i := range first.Entries {
		if first.Entries[i].ID != second.Entries[i].ID {
			t.Errorf("position %d changed from %s to %s between identical runs",
				i, first.Entries[i].ID, second.Entries[i].ID)
		}
	}
}

// Buckets must be dense and in the local timezone.
func TestHistogramsAreDense(t *testing.T) {
	st := seedCorpus(t)
	ctx := context.Background()

	if _, err := newRollup(t, st).RefreshHistograms(ctx); err != nil {
		t.Fatal(err)
	}

	hour, err := st.GetHistogram(ctx, model.PeriodAll, store.HistogramHour)
	if err != nil {
		t.Fatal(err)
	}
	if len(hour.Plays) != 24 {
		t.Errorf("hour buckets = %d, want all 24 present", len(hour.Plays))
	}
	var total int64
	for _, v := range hour.Plays {
		total += v
	}
	if total != 10 {
		t.Errorf("hour histogram totals %d plays, want the corpus size 10", total)
	}

	weekday, err := st.GetHistogram(ctx, model.PeriodAll, store.HistogramWeekday)
	if err != nil {
		t.Fatal(err)
	}
	if len(weekday.Plays) != 7 {
		t.Errorf("weekday buckets = %d, want 7", len(weekday.Plays))
	}

	// A play at 22:00Z on 20 Dec is 23:00 in Madrid, so hour 23 must be populated and hour 22
	// must not have absorbed it.
	if hour.Plays[23] == 0 {
		t.Error("hour 23 is empty; buckets must be local, not UTC")
	}
}

func TestDashboardSnapshot(t *testing.T) {
	st := seedCorpus(t)
	d, dir := runFull(t, st)

	if d.GeneratedAt == "" || d.Timezone != "Europe/Madrid" {
		t.Errorf("generatedAt=%q timezone=%q", d.GeneratedAt, d.Timezone)
	}
	if d.AllTime.Plays != 10 {
		t.Errorf("allTime.plays = %d, want 10", d.AllTime.Plays)
	}
	// The corpus mixes api and export sources.
	if d.AllTime.EstimatedRatio <= 0 || d.AllTime.EstimatedRatio >= 1 {
		t.Errorf("estimatedRatio = %v, want strictly between 0 and 1", d.AllTime.EstimatedRatio)
	}
	if d.KPIs.DistinctTracks != 5 {
		t.Errorf("distinctTracks = %d, want 5", d.KPIs.DistinctTracks)
	}
	if d.KPIs.DistinctArtists != 4 {
		t.Errorf("distinctArtists = %d, want 4", d.KPIs.DistinctArtists)
	}

	if len(d.Top.Artists) == 0 || len(d.Top.Tracks) == 0 {
		t.Error("top lists are empty")
	}
	for i, e := range d.Top.Artists {
		if e.Rank != i+1 {
			t.Errorf("artist %d has rank %d", i, e.Rank)
		}
		if e.Name == "" {
			t.Error("artist entry has no name")
		}
	}

	// The calendar must be dense: a heatmap fed only non-zero days cannot lay out a grid.
	if len(d.Calendar) < 360 {
		t.Errorf("calendar has %d days, want a dense trailing 12 months", len(d.Calendar))
	}
	var withPlays int
	for _, c := range d.Calendar {
		if c.Date == "" {
			t.Fatal("calendar entry has no date")
		}
		if c.Plays > 0 {
			withPlays++
		}
	}
	if withPlays == 0 {
		t.Error("no calendar day has plays")
	}

	// Rhythm charts are dense too.
	if len(d.Rhythm.HourOfDay) != 24 {
		t.Errorf("hourOfDay = %d buckets, want 24", len(d.Rhythm.HourOfDay))
	}
	if len(d.Rhythm.Weekday) != 7 {
		t.Errorf("weekday = %d buckets, want 7", len(d.Rhythm.Weekday))
	}

	// The genre caveat must reach the client: genres are many-to-many and cannot be drawn as a
	// part-to-whole chart.
	var genreNote bool
	for _, n := range d.Notes {
		if contains(n, "several genres") {
			genreNote = true
		}
	}
	if !genreNote {
		t.Error("no note explaining that genres do not sum to the total")
	}
	if d.GenreCoverage < 0 || d.GenreCoverage > 1 {
		t.Errorf("genreCoverage = %v, want within [0,1]", d.GenreCoverage)
	}

	// The other two files exist and parse.
	for _, name := range []string{rollup.FileCatalog, rollup.FileMeta} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			t.Errorf("%s is not valid JSON: %v", name, err)
		}
	}
}

func TestCatalogSnapshot(t *testing.T) {
	st := seedCorpus(t)
	_, dir := runFull(t, st)

	body, err := os.ReadFile(filepath.Join(dir, rollup.FileCatalog))
	if err != nil {
		t.Fatal(err)
	}
	var c rollup.Catalog
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatal(err)
	}

	if len(c.Artists) != 4 || len(c.Tracks) != 5 {
		t.Errorf("catalog has %d artists and %d tracks, want 4 and 5",
			len(c.Artists), len(c.Tracks))
	}
	// Sorted by name, so the index is stable between runs.
	for i := 1; i < len(c.Artists); i++ {
		if c.Artists[i-1][1] > c.Artists[i][1] {
			t.Errorf("artists not sorted at %d", i)
		}
	}
	for _, pair := range c.Artists {
		if pair[0] == "" || pair[1] == "" {
			t.Errorf("catalog entry %v has an empty field", pair)
		}
	}
}

// The dashboard is the landing page, so a half-written file would be served as a parse error.
func TestDirPublisherWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	p := rollup.NewDirPublisher(dir)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := p.Publish(ctx, "x.json", []byte(`{"n":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only the published file", names)
	}
	// And no CDN to purge.
	if err := p.Invalidate(ctx, []string{"/data/*"}); err != nil {
		t.Errorf("Invalidate = %v, want nil for a local publisher", err)
	}
}

// A run with no publisher configured must still reconcile: that is the reconcile-only mode.
func TestRunWithoutPublisher(t *testing.T) {
	st := seedCorpus(t)
	res, err := newRollup(t, st).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.SnapshotsWritten != 0 {
		t.Errorf("snapshots = %d, want 0 with no publisher", res.SnapshotsWritten)
	}
	if res.LeaderboardsWritten == 0 {
		t.Error("leaderboards were not refreshed")
	}
	// Spotify is not configured in tests, and its absence must not fail the run.
	if !res.SkippedSpotify {
		t.Error("SkippedSpotify = false despite no Spotify client")
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

// TestCoverageIsExactNotWriteOrder guards the bug that running the rollup exposed.
//
// The all-time firstPlayedAt/lastPlayedAt aggregate attributes are set by WRITE order, not play
// order, so ingesting out of order leaves them wrong -- and a windowed reconcile cannot correct
// an all-time bound. The dashboard must report the figures from the full-history pass instead,
// and must not claim they are exact when no such pass has run.
func TestCoverageIsExactNotWriteOrder(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	for id, genres := range storetest.Genres() {
		if err := st.PutArtist(ctx, model.Artist{ID: id, Name: "Artist " + id, Genres: genres}); err != nil {
			t.Fatal(err)
		}
	}

	// Written newest-first, which is what makes the write-time bounds disagree with reality.
	corpus := storetest.Corpus(t)
	for i := len(corpus) - 1; i >= 0; i-- {
		if _, err := st.RecordPlay(ctx, corpus[i], storetest.GenresFor(corpus[i])); err != nil {
			t.Fatal(err)
		}
	}

	// Before any full pass the aggregate bound is the newest play, not the oldest.
	agg, err := st.GetAggregate(ctx, model.AggKey{
		Dim: model.DimTotal, Period: model.PeriodAll, EntityID: model.TotalEntityID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !agg.FirstPlayedAt.After(agg.LastPlayedAt) {
		t.Log("note: write-time bounds happen to be ordered here; the assertion below is the " +
			"one that matters")
	}

	d, _ := runFull(t, st)

	if d.Coverage.FirstPlayedAt == nil || d.Coverage.LastPlayedAt == nil {
		t.Fatal("coverage not reported")
	}
	// The corpus starts 2025-12-15 and ends 2026-02-10.
	if *d.Coverage.FirstPlayedAt != "2025-12-15T10:00:00.000Z" {
		t.Errorf("firstPlayedAt = %s, want the true earliest play 2025-12-15T10:00:00.000Z "+
			"(the aggregate attribute would give the first play WRITTEN)",
			*d.Coverage.FirstPlayedAt)
	}
	if *d.Coverage.LastPlayedAt != "2026-02-10T13:00:00.000Z" {
		t.Errorf("lastPlayedAt = %s, want 2026-02-10T13:00:00.000Z", *d.Coverage.LastPlayedAt)
	}
	if d.Coverage.Approximate {
		t.Error("approximate = true after a full coverage pass produced exact bounds")
	}
}

// TestGenreCoverageIsPerPlay: the corpus has two plays by an artist with no genres, so coverage
// must be strictly below 1. Summing the genre aggregates and capping at the total would report a
// confident 100%.
func TestGenreCoverageIsPerPlay(t *testing.T) {
	st := seedCorpus(t)
	d, _ := runFull(t, st)

	if d.GenreCoverage <= 0 {
		t.Fatalf("genreCoverage = %v, want > 0", d.GenreCoverage)
	}
	if d.GenreCoverage >= 1 {
		t.Errorf("genreCoverage = %v, want < 1: the corpus contains plays by an artist with no "+
			"genres, so coverage cannot be total", d.GenreCoverage)
	}
}

// Without a full pass the dashboard must admit the bounds are approximate rather than presenting
// write-order artefacts as fact.
func TestCoverageMarkedApproximateWithoutAFullPass(t *testing.T) {
	st := seedCorpus(t)
	ctx := context.Background()

	// Render snapshots directly, skipping the histogram/coverage pass.
	dir := t.TempDir()
	r := newRollup(t, st, func(c *rollup.Config) { c.Publisher = rollup.NewDirPublisher(dir) })
	if _, err := r.RenderSnapshots(ctx); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dir, rollup.FileDashboard))
	if err != nil {
		t.Fatal(err)
	}
	var d rollup.Dashboard
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	if !d.Coverage.Approximate {
		t.Error("approximate = false with no coverage row; the bounds are write-order artefacts")
	}
	if d.GenreCoverage != 0 {
		t.Errorf("genreCoverage = %v, want 0 when no full pass has run", d.GenreCoverage)
	}
}

// TestUnresolvedNamesAreReported guards against a silent failure mode seen in production: the
// dashboard rendered raw Spotify IDs where artist names belonged, and nothing anywhere said so.
//
// The cause is upstream -- an artist whose dimension row was never written, because capture could
// not reach GET /v1/artists -- but the rollup is where it becomes visible, so the rollup has to
// report it rather than quietly baking IDs into the snapshot.
func TestUnresolvedNamesAreReported(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// Plays recorded, but NO artist rows written -- exactly what a degraded capture leaves behind.
	for _, p := range storetest.Corpus(t) {
		if _, err := st.RecordPlay(ctx, p, nil); err != nil {
			t.Fatal(err)
		}
	}

	written, unresolved, err := newRollup(t, st).RefreshLeaderboards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if written == 0 {
		t.Fatal("no leaderboards written")
	}
	if unresolved == 0 {
		t.Error("unresolved = 0, but no artist, track or album rows exist; the dashboard would " +
			"render raw IDs with nothing reporting it")
	}

	// And the run surfaces it too, so an operator sees it without reading the snapshot.
	dir := t.TempDir()
	r := newRollup(t, st, func(c *rollup.Config) { c.Publisher = rollup.NewDirPublisher(dir) })
	res, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.UnresolvedNames == 0 {
		t.Error("Result.UnresolvedNames = 0; the condition is invisible to the operator")
	}
}

// Once the dimension rows exist, nothing should be unresolved.
func TestNamesResolveWhenDimensionsExist(t *testing.T) {
	st := seedCorpus(t)
	ctx := context.Background()
	// seedCorpus writes artists; add the tracks and albums the leaderboards also name.
	for _, id := range []string{"t1", "t2", "t3", "t4", "t5"} {
		if err := st.PutTrack(ctx, model.Track{ID: id, Name: "Track " + id, AlbumID: "al1"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutAlbum(ctx, model.Album{ID: "al1", Name: "The Album"}); err != nil {
		t.Fatal(err)
	}

	_, unresolved, err := newRollup(t, st).RefreshLeaderboards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unresolved != 0 {
		t.Errorf("unresolved = %d, want 0 now that every dimension row exists", unresolved)
	}
}

// TestSnapshotReportsGenresUnavailable pins the distinction between "no genres yet" and "genres
// cannot exist". Spotify removed the artist `genres` field from the Web API in February 2026,
// so the second case is now permanent, and the dashboard must say so rather than render an
// empty chart that reads as a bug.
func TestSnapshotReportsGenresUnavailable(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// Plays exist, but no artist carries a genre -- exactly what the live API now returns.
	for _, p := range storetest.Corpus(t) {
		if _, err := st.RecordPlay(ctx, p, nil); err != nil {
			t.Fatal(err)
		}
	}

	dash, _ := runFull(t, st)
	if dash.GenresAvailable {
		t.Error("GenresAvailable = true with no genre data; the UI would draw an empty chart")
	}
	if len(dash.Top.Genres) != 0 {
		t.Errorf("Top.Genres = %d entries, want none", len(dash.Top.Genres))
	}
	// The note must explain the cause, not merely state emptiness.
	var explained bool
	for _, n := range dash.Notes {
		if strings.Contains(n, "Genre data is unavailable") {
			explained = true
		}
		if strings.Contains(n, "do not sum to the total") {
			t.Error("kept the many-to-many caveat for genres that do not exist")
		}
	}
	if !explained {
		t.Errorf("no note explains why genres are missing; notes = %q", dash.Notes)
	}
}

// And the inverse: with genre data present, the caveat comes back and the flag flips.
func TestSnapshotReportsGenresAvailable(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	for _, p := range storetest.Corpus(t) {
		if _, err := st.RecordPlay(ctx, p, []string{"symphonic metal"}); err != nil {
			t.Fatal(err)
		}
	}

	dash, _ := runFull(t, st)
	if !dash.GenresAvailable {
		t.Fatal("GenresAvailable = false despite genre aggregates existing")
	}
	var caveated bool
	for _, n := range dash.Notes {
		if strings.Contains(n, "do not sum to the total") {
			caveated = true
		}
		if strings.Contains(n, "Genre data is unavailable") {
			t.Error("claimed genres are unavailable while serving genre data")
		}
	}
	if !caveated {
		t.Error("lost the many-to-many caveat, which stops the chart being read as part-to-whole")
	}
}

// TestSnapshotFlagsPartialArtistAttribution guards the worst failure of the history import.
//
// The GDPR export names artists as free text and gives no artist ID, so a play imported before
// its track is resolved counts towards TOTAL and towards no ARTIST. Measured against the real
// export, that made the true top artist vanish from the top five while the artists shown read
// at roughly a quarter of their real totals -- and looked completely plausible.
//
// A ranking whose ORDER is wrong must say so.
func TestSnapshotFlagsPartialArtistAttribution(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// Two attributed plays and three with no artist at all, as an unresolved import leaves them.
	for _, p := range storetest.Corpus(t)[:2] {
		if _, err := st.RecordPlay(ctx, p, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i, ts := range []string{
		"2026-02-01T10:00:00.000Z", "2026-02-01T11:00:00.000Z", "2026-02-01T12:00:00.000Z",
	} {
		p := storetest.APIPlay(t, ts, fmt.Sprintf("orphan-%d", i))
		p.ArtistIDs = nil
		p.AlbumID = ""
		if _, err := st.RecordPlay(ctx, p, nil); err != nil {
			t.Fatal(err)
		}
	}

	dash, _ := runFull(t, st)
	if dash.ArtistCoverage <= 0 || dash.ArtistCoverage >= 0.99 {
		t.Errorf("ArtistCoverage = %v, want a partial value", dash.ArtistCoverage)
	}
	var warned bool
	for _, n := range dash.Notes {
		if strings.Contains(n, "INCOMPLETE") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no note warns that artist figures are incomplete; notes = %q", dash.Notes)
	}
}

// At full attribution the caveat must disappear, or it becomes a permanent disclaimer nobody
// reads.
func TestSnapshotDoesNotWarnAtFullAttribution(t *testing.T) {
	st := seedCorpus(t)
	dash, _ := runFull(t, st)
	if dash.ArtistCoverage < 0.99 {
		t.Fatalf("ArtistCoverage = %v, want full for the seeded corpus", dash.ArtistCoverage)
	}
	for _, n := range dash.Notes {
		if strings.Contains(n, "INCOMPLETE") {
			t.Errorf("warned about attribution at full coverage: %q", n)
		}
	}
}

// TestPartlyEnrichedArtistDoesNotSplit is the regression test for a bug that shipped.
//
// Identity resolves per TRACK, so an artist whose catalogue is only partly enriched contributed
// some plays under its real Spotify ID and the rest under a name key. "Disturbed" appeared twice
// on the dashboard, at 1,656h and 554h, when the truth was the sum.
func TestPartlyEnrichedArtistDoesNotSplit(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// One artist, two tracks: one enriched with a real Spotify ID, one still name-keyed.
	if err := st.PutArtist(ctx, model.Artist{ID: "ar-real", Name: "Disturbed"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTrack(ctx, model.Track{
		ID: "t-enriched", Name: "Stricken", ArtistIDs: []string{"ar-real"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTrack(ctx, model.Track{
		ID: "t-placeholder", Name: "Indestructible",
		ArtistIDs: []string{model.NameKey("Disturbed")},
	}); err != nil {
		t.Fatal(err)
	}

	for i, tid := range []string{"t-enriched", "t-placeholder"} {
		p, err := model.NewExportPlay(
			mustParse(t, fmt.Sprintf("2026-02-0%dT10:00:00.000Z", i+1)), 300_000,
			model.Track{ID: tid},
			model.ExportFields{TrackName: "x", ArtistName: "Disturbed"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutPlay(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	dash, _ := runFull(t, st)

	var hits []string
	var total int64
	for _, e := range dash.Top.Artists {
		if e.Name == "Disturbed" || model.IsNameKey(e.ID) {
			hits = append(hits, e.ID)
			total += e.MsPlayed
		}
	}
	if len(hits) != 1 {
		t.Fatalf("Disturbed appears under %d ids (%v); a partly-enriched artist split in two",
			len(hits), hits)
	}
	if model.IsNameKey(hits[0]) {
		t.Errorf("converged on the name key %q rather than the known Spotify ID", hits[0])
	}
	if total != 600_000 {
		t.Errorf("msPlayed = %d, want both plays (600000) under one artist", total)
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := model.ParseTS(s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}
