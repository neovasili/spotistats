package rollup_test

import (
	"context"
	"testing"
	"time"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/rollup"
	"github.com/neovasili/spotistats/internal/store"
	"github.com/neovasili/spotistats/internal/store/storetest"
)

// The corpus spans local Dec 2025 to Feb 2026, so "now" sits just after it and a wide window
// covers the whole thing.
var testNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func newRollup(t *testing.T, st *store.Store, opts ...func(*rollup.Config)) *rollup.Rollup {
	t.Helper()
	cfg := rollup.Config{
		Store:    st,
		Calendar: model.MustCalendar(storetest.DefaultTimezone),
		Now:      func() time.Time { return testNow },
		// Wide enough to cover the whole corpus.
		WindowDays: 120,
	}
	for _, o := range opts {
		o(&cfg)
	}
	r, err := rollup.New(cfg)
	if err != nil {
		t.Fatalf("rollup.New: %v", err)
	}
	return r
}

// seedCorpus records the shared corpus and returns the store.
func seedCorpus(t *testing.T) *store.Store {
	t.Helper()
	st := storetest.NewStore(t)
	ctx := context.Background()
	for id, genres := range storetest.Genres() {
		if err := st.PutArtist(ctx, model.Artist{ID: id, Name: "Artist " + id, Genres: genres}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range storetest.Corpus(t) {
		if _, err := st.RecordPlay(ctx, p, storetest.GenresFor(p)); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// A reconcile over correct data must change nothing. Without this, every other assertion here
// could pass while the reconciler silently rewrote correct rows.
func TestReconcileIsANoOpOnCleanData(t *testing.T) {
	st := seedCorpus(t)
	ctx := context.Background()

	res, err := newRollup(t, st).Reconcile(ctx, 120)
	if err != nil {
		t.Fatal(err)
	}
	if res.PlaysRead != 10 {
		t.Errorf("PlaysRead = %d, want the corpus size 10", res.PlaysRead)
	}
	if res.RowsChecked == 0 {
		t.Error("RowsChecked = 0; the reconcile examined nothing")
	}
	if res.RowsCorrected != 0 {
		t.Errorf("RowsCorrected = %d, want 0 on clean data", res.RowsCorrected)
	}
	if res.PropagatedRows != 0 {
		t.Errorf("PropagatedRows = %d, want 0", res.PropagatedRows)
	}
}

// TestReconcileRepairsDroppedAggregates simulates the failure the reconciler exists for: a
// capture run that wrote the play but died before applying its aggregate deltas.
func TestReconcileRepairsDroppedAggregates(t *testing.T) {
	st := seedCorpus(t)
	ctx := context.Background()

	// Zero one month row and its year and all-time counterparts, as if the deltas never landed.
	trackMonth := model.AggKey{Dim: model.DimTrack, Period: "2025-12", EntityID: "t1"}
	before, err := st.GetAggregate(ctx, trackMonth)
	if err != nil {
		t.Fatal(err)
	}
	if before.Plays == 0 {
		t.Fatal("precondition: expected plays in 2025-12 for t1")
	}

	yearKey := model.AggKey{Dim: model.DimTrack, Period: "2025", EntityID: "t1"}
	allKey := model.AggKey{Dim: model.DimTrack, Period: model.PeriodAll, EntityID: "t1"}
	yearBefore, _ := st.GetAggregate(ctx, yearKey)
	allBefore, _ := st.GetAggregate(ctx, allKey)

	// Subtract the month's contribution everywhere, mimicking the lost update.
	if err := st.PutAggregate(ctx, model.Aggregate{Key: trackMonth}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutAggregate(ctx, model.Aggregate{
		Key:   yearKey,
		Plays: yearBefore.Plays - before.Plays, PlaysExact: yearBefore.PlaysExact - before.PlaysExact,
		MsPlayed:      yearBefore.MsPlayed - before.MsPlayed,
		MsPlayedExact: yearBefore.MsPlayedExact - before.MsPlayedExact,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutAggregate(ctx, model.Aggregate{
		Key:   allKey,
		Plays: allBefore.Plays - before.Plays, PlaysExact: allBefore.PlaysExact - before.PlaysExact,
		MsPlayed:      allBefore.MsPlayed - before.MsPlayed,
		MsPlayedExact: allBefore.MsPlayedExact - before.MsPlayedExact,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := newRollup(t, st).Reconcile(ctx, 120)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsCorrected == 0 {
		t.Fatal("RowsCorrected = 0; the drift was not detected")
	}
	if res.PropagatedRows == 0 {
		t.Error("PropagatedRows = 0; the year and all-time rows were not repaired")
	}

	// The month row is restored absolutely...
	after, err := st.GetAggregate(ctx, trackMonth)
	if err != nil {
		t.Fatal(err)
	}
	if after.Plays != before.Plays || after.MsPlayed != before.MsPlayed {
		t.Errorf("month row = (%d plays, %d ms), want (%d, %d)",
			after.Plays, after.MsPlayed, before.Plays, before.MsPlayed)
	}
	// ...and the year and all-time rows are back to their originals, which is the part a
	// windowed recompute could not do on its own.
	yearAfter, _ := st.GetAggregate(ctx, yearKey)
	if yearAfter.Plays != yearBefore.Plays || yearAfter.MsPlayed != yearBefore.MsPlayed {
		t.Errorf("year row = (%d, %d), want (%d, %d)",
			yearAfter.Plays, yearAfter.MsPlayed, yearBefore.Plays, yearBefore.MsPlayed)
	}
	allAfter, _ := st.GetAggregate(ctx, allKey)
	if allAfter.Plays != allBefore.Plays || allAfter.MsPlayed != allBefore.MsPlayed {
		t.Errorf("all-time row = (%d, %d), want (%d, %d)",
			allAfter.Plays, allAfter.MsPlayed, allBefore.Plays, allBefore.MsPlayed)
	}
}

// TestReconcileDoesNotClobberAllTimeFromAWindow is the regression test for the design defect in
// docs/SPECS.md 4.3.
//
// A naive reading -- "recompute aggregates for the trailing 45 days and compare to stored
// counters" -- would compare a window's worth of plays against an all-time counter, report
// enormous phantom drift, and then overwrite the all-time row with the window's value. That
// would destroy history. A narrow window must leave older totals alone.
func TestReconcileDoesNotClobberAllTimeFromAWindow(t *testing.T) {
	st := seedCorpus(t)
	ctx := context.Background()

	allKey := model.AggKey{Dim: model.DimTotal, Period: model.PeriodAll, EntityID: model.TotalEntityID}
	before, err := st.GetAggregate(ctx, allKey)
	if err != nil {
		t.Fatal(err)
	}
	if before.Plays != 10 {
		t.Fatalf("precondition: all-time plays = %d, want 10", before.Plays)
	}

	// A 20-day window from testNow covers only Feb 2026 -- 3 of the 10 corpus plays.
	res, err := newRollup(t, st).Reconcile(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if res.PlaysRead >= 10 {
		t.Fatalf("PlaysRead = %d; a 20-day window should not see the whole corpus", res.PlaysRead)
	}

	after, err := st.GetAggregate(ctx, allKey)
	if err != nil {
		t.Fatal(err)
	}
	if after.Plays != before.Plays {
		t.Errorf("all-time plays went from %d to %d after a narrow-window reconcile; "+
			"a window must never overwrite an all-time counter", before.Plays, after.Plays)
	}
	if after.MsPlayed != before.MsPlayed {
		t.Errorf("all-time ms went from %d to %d", before.MsPlayed, after.MsPlayed)
	}
	if res.RowsCorrected != 0 {
		t.Errorf("RowsCorrected = %d; clean data in the window should need no correction",
			res.RowsCorrected)
	}
}

// A row with no plays behind it any more must be zeroed, or a deleted play leaves a phantom
// entity in every leaderboard forever.
func TestReconcileZeroesOrphanedRows(t *testing.T) {
	st := seedCorpus(t)
	ctx := context.Background()

	orphan := model.AggKey{Dim: model.DimTrack, Period: "2026-02", EntityID: "ghost"}
	if err := st.PutAggregate(ctx, model.Aggregate{
		Key: orphan, Plays: 99, MsPlayed: 9_900_000,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := newRollup(t, st).Reconcile(ctx, 120)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsCorrected == 0 {
		t.Fatal("the orphan was not detected")
	}
	after, err := st.GetAggregate(ctx, orphan)
	if err != nil {
		t.Fatal(err)
	}
	if after.Plays != 0 || after.MsPlayed != 0 {
		t.Errorf("orphan = (%d plays, %d ms), want zeroed", after.Plays, after.MsPlayed)
	}
}

// The reconcile must re-derive genres, since a degraded capture records plays without them.
func TestReconcileRestoresGenreAttribution(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// Record plays with NO genres, as a degraded capture would.
	for _, p := range storetest.Corpus(t) {
		if _, err := st.RecordPlay(ctx, p, nil); err != nil {
			t.Fatal(err)
		}
	}
	genreKey := model.AggKey{Dim: model.DimGenre, Period: model.PeriodAll, EntityID: "gothic metal"}
	if _, err := st.GetAggregate(ctx, genreKey); err == nil {
		t.Fatal("precondition: expected no genre aggregate")
	}

	// Now the artist rows arrive, as the next capture's enrichment would provide.
	for id, genres := range storetest.Genres() {
		if err := st.PutArtist(ctx, model.Artist{ID: id, Name: "Artist " + id, Genres: genres}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := newRollup(t, st).Reconcile(ctx, 120); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetAggregate(ctx, genreKey)
	if err != nil {
		t.Fatalf("genre attribution not restored: %v", err)
	}
	if got.Plays == 0 {
		t.Error("genre row has no plays")
	}
}

// A full reconcile rewrites everything from history and must reproduce the same totals.
func TestReconcileAllMatchesIncrementalTotals(t *testing.T) {
	st := seedCorpus(t)
	ctx := context.Background()

	allKey := model.AggKey{Dim: model.DimTotal, Period: model.PeriodAll, EntityID: model.TotalEntityID}
	before, err := st.GetAggregate(ctx, allKey)
	if err != nil {
		t.Fatal(err)
	}

	res, err := newRollup(t, st).ReconcileAll(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if res.PlaysRead != 10 {
		t.Errorf("PlaysRead = %d, want 10", res.PlaysRead)
	}

	after, err := st.GetAggregate(ctx, allKey)
	if err != nil {
		t.Fatal(err)
	}
	if after.Plays != before.Plays || after.MsPlayed != before.MsPlayed ||
		after.MsPlayedExact != before.MsPlayedExact {
		t.Errorf("full reconcile changed the totals: (%d,%d,%d) -> (%d,%d,%d)",
			before.Plays, before.MsPlayed, before.MsPlayedExact,
			after.Plays, after.MsPlayed, after.MsPlayedExact)
	}
	// And a full pass makes the bounds exact, unlike the write-time best-effort ones.
	if after.FirstPlayedAt.After(after.LastPlayedAt) {
		t.Errorf("bounds inverted after a full reconcile: %v > %v",
			after.FirstPlayedAt, after.LastPlayedAt)
	}
}

// Cross-dimension invariants must survive a reconcile.
func TestReconcilePreservesCrossDimensionInvariants(t *testing.T) {
	st := seedCorpus(t)
	ctx := context.Background()

	if _, err := newRollup(t, st).Reconcile(ctx, 120); err != nil {
		t.Fatal(err)
	}

	total, err := st.GetAggregate(ctx, model.AggKey{
		Dim: model.DimTotal, Period: model.PeriodAll, EntityID: model.TotalEntityID,
	})
	if err != nil {
		t.Fatal(err)
	}

	sum := func(dim model.Dim) int64 {
		var n int64
		for a, err := range st.QueryAggregates(ctx, dim, model.PeriodAll, "") {
			if err != nil {
				t.Fatal(err)
			}
			if a.Key.Dim == dim {
				n += a.Plays
			}
		}
		return n
	}

	if got := sum(model.DimTrack); got != total.Plays {
		t.Errorf("sum(TRACK) = %d, want exactly TOTAL %d", got, total.Plays)
	}
	if got := sum(model.DimArtist); got < total.Plays {
		t.Errorf("sum(ARTIST) = %d, want >= TOTAL %d", got, total.Plays)
	}
	if got := sum(model.DimAlbum); got > total.Plays {
		t.Errorf("sum(ALBUM) = %d, want <= TOTAL %d", got, total.Plays)
	}
}

// TestFullReconcileRemovesOrphanedRows is the regression test for a bug that shipped twice over.
//
// A full reconcile rewrites what it computes. That is not enough: when an entity's IDENTITY
// changes -- as it does when artist attribution converges from name keys onto real Spotify IDs --
// the old key survives with its old numbers, and the dashboard lists one artist twice with its
// history split between the two rows.
//
// Only a full reconcile may delete: it has read every play, so anything it did not compute is
// unsupported. A windowed pass has seen a window and must leave everything else alone.
func TestFullReconcileRemovesOrphanedRows(t *testing.T) {
	st := seedCorpus(t)
	ctx := context.Background()

	// A row no play supports: exactly what a superseded identity leaves behind.
	orphan := model.AggKey{
		Dim: model.DimArtist, Period: model.PeriodAll, EntityID: model.NameKey("Ghost Artist"),
	}
	if err := st.PutAggregates(ctx, []model.Aggregate{{
		Key: orphan, Plays: 999, MsPlayed: 9_999_999,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAggregate(ctx, orphan); err != nil {
		t.Fatalf("the orphan was not stored: %v", err)
	}

	res, err := newRollup(t, st).ReconcileAll(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsDeleted == 0 {
		t.Error("RowsDeleted = 0; the orphan survived and would appear beside real entities")
	}
	if _, err := st.GetAggregate(ctx, orphan); err == nil {
		t.Error("the orphaned aggregate row still exists after a full reconcile")
	}
}

// A WINDOWED reconcile must never delete: it sees one window, so every row outside it would
// look unsupported.
func TestWindowedReconcileDeletesNothing(t *testing.T) {
	st := seedCorpus(t)
	ctx := context.Background()

	// A row far outside any plausible window, standing in for the rest of history.
	old := model.AggKey{
		Dim: model.DimArtist, Period: model.Period("2009"), EntityID: "ar-ancient",
	}
	if err := st.PutAggregates(ctx, []model.Aggregate{{
		Key: old, Plays: 42, MsPlayed: 420_000,
	}}); err != nil {
		t.Fatal(err)
	}

	res, err := newRollup(t, st).Reconcile(ctx, 45)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsDeleted != 0 {
		t.Errorf("RowsDeleted = %d; a windowed reconcile deleted rows it cannot judge",
			res.RowsDeleted)
	}
	if _, err := st.GetAggregate(ctx, old); err != nil {
		t.Errorf("a windowed reconcile destroyed history outside its window: %v", err)
	}
}
