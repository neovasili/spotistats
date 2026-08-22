package rollup_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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

	n, err := newRollup(t, st).RefreshLeaderboards(ctx)
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

	if _, err := r.RefreshLeaderboards(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := st.GetLeaderboard(ctx, model.DimTrack, model.PeriodAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RefreshLeaderboards(ctx); err != nil {
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
