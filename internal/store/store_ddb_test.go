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

var madrid = model.MustCalendar(storetest.DefaultTimezone)

func mustTS(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := model.ParseTS(s)
	if err != nil {
		t.Fatalf("ParseTS(%q): %v", s, err)
	}
	return ts
}

func collect[V any](t *testing.T, seq func(func(V, error) bool)) []V {
	t.Helper()
	var out []V
	for v, err := range seq {
		if err != nil {
			t.Fatalf("iteration: %v", err)
		}
		out = append(out, v)
	}
	return out
}

// ---------------------------------------------------------------------------
// idempotency -- the headline behaviour of the ingestion path
// ---------------------------------------------------------------------------

// TestPutPlayIsIdempotent: re-reading an overlapping capture window is normal, so a
// duplicate is (false, nil) rather than an error.
func TestPutPlayIsIdempotent(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	p := storetest.APIPlay(t, "2025-03-14T21:04:33.123Z", "t1")

	inserted, err := s.PutPlay(ctx, p)
	if err != nil {
		t.Fatalf("first PutPlay: %v", err)
	}
	if !inserted {
		t.Fatal("first PutPlay reported inserted=false")
	}

	inserted, err = s.PutPlay(ctx, p)
	if err != nil {
		t.Fatalf("second PutPlay returned an error; a duplicate is expected, not exceptional: %v", err)
	}
	if inserted {
		t.Error("second PutPlay reported inserted=true")
	}
}

// TestRecordPlayTwiceLeavesAggregatesUnchanged is the single most important test in the
// package: it proves the conditional insert gates the aggregate updates, so replaying a
// capture window cannot inflate any counter.
func TestRecordPlayTwiceLeavesAggregatesUnchanged(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	p := storetest.APIPlay(t, "2025-03-14T21:04:33.123Z", "t1", storetest.WithArtists("ar1"))
	genres := storetest.GenresFor(p)

	first, err := s.RecordPlay(ctx, p, genres)
	if err != nil {
		t.Fatalf("first RecordPlay: %v", err)
	}
	if !first.Inserted || first.DeltasApplied == 0 {
		t.Fatalf("first RecordPlay = %+v", first)
	}

	// Snapshot every aggregate the play touched.
	keys := make([]model.AggKey, 0, first.DeltasApplied)
	for _, d := range model.AggregateDeltas(model.FactsFor(p, genres), madrid) {
		keys = append(keys, d.Key)
	}
	before, err := s.BatchGetAggregates(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(keys) {
		t.Fatalf("stored %d aggregates, want %d", len(before), len(keys))
	}

	second, err := s.RecordPlay(ctx, p, genres)
	if err != nil {
		t.Fatalf("second RecordPlay: %v", err)
	}
	if second.Inserted {
		t.Error("second RecordPlay reported Inserted=true")
	}
	if second.DeltasApplied != 0 {
		t.Errorf("second RecordPlay applied %d deltas; a duplicate must not touch aggregates",
			second.DeltasApplied)
	}

	after, err := s.BatchGetAggregates(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(before, after); diff != "" {
		t.Errorf("aggregates changed after a duplicate RecordPlay (-before +after):\n%s", diff)
	}
}

func TestRecordPlayMatchesPureDeltas(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	p := storetest.ExportPlay(t, "2025-03-14T21:04:33.123Z", "t1", 187_433,
		storetest.WithArtists("ar1", "ar2"))
	genres := storetest.GenresFor(p)

	if _, err := s.RecordPlay(ctx, p, genres); err != nil {
		t.Fatal(err)
	}

	// The stored rows must equal what the pure function said they should be.
	for _, d := range model.AggregateDeltas(model.FactsFor(p, genres), madrid) {
		got, err := s.GetAggregate(ctx, d.Key)
		if err != nil {
			t.Fatalf("GetAggregate(%s): %v", d.Key, err)
		}
		if got.Plays != d.Plays || got.PlaysExact != d.PlaysExact ||
			got.MsPlayed != d.MsPlayed || got.MsPlayedExact != d.MsPlayedExact {
			t.Errorf("%s stored (%d,%d,%d,%d), want (%d,%d,%d,%d)", d.Key,
				got.Plays, got.PlaysExact, got.MsPlayed, got.MsPlayedExact,
				d.Plays, d.PlaysExact, d.MsPlayed, d.MsPlayedExact)
		}
		if !got.FirstPlayedAt.Equal(p.PlayedAt) || !got.LastPlayedAt.Equal(p.PlayedAt) {
			t.Errorf("%s bounds = (%v,%v), want both %v", d.Key,
				got.FirstPlayedAt, got.LastPlayedAt, p.PlayedAt)
		}
	}
}

func TestPutPlayRejectsInvalid(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	// A play whose duration is zero means a mapping bug upstream, so it must be rejected
	// rather than silently contributing nothing forever.
	bad := model.Play{PlayedAt: mustTS(t, "2025-03-14T21:04:33.123Z"), TrackID: "t1",
		Source: model.SourceAPI, MsEstimated: true, MsPlayed: 0}
	if _, err := s.PutPlay(ctx, bad); !errors.Is(err, model.ErrInvalidPlay) {
		t.Errorf("err = %v, want ErrInvalidPlay", err)
	}
}

// ---------------------------------------------------------------------------
// aggregate accumulation
// ---------------------------------------------------------------------------

func TestApplyDeltasAccumulates(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	k := model.AggKey{Dim: model.DimArtist, Period: "2025", EntityID: "ar1"}
	at := mustTS(t, "2025-03-14T21:04:33.123Z")

	d := model.AggDelta{
		Key: k, Plays: 1, PlaysExact: 1, MsPlayed: 1000, MsPlayedExact: 1000,
		FirstPlayedAt: at, LastPlayedAt: at,
	}
	for i := 0; i < 5; i++ {
		if err := s.ApplyDeltas(ctx, []model.AggDelta{d}); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}

	got, err := s.GetAggregate(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plays != 5 || got.PlaysExact != 5 || got.MsPlayed != 5000 || got.MsPlayedExact != 5000 {
		t.Errorf("after 5 applies = (%d,%d,%d,%d), want (5,5,5000,5000)",
			got.Plays, got.PlaysExact, got.MsPlayed, got.MsPlayedExact)
	}
}

// firstPlayedAt must survive later writes; lastPlayedAt advances.
func TestApplyDeltasBoundsBehaviour(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	k := model.AggKey{Dim: model.DimTrack, Period: "2025", EntityID: "t1"}
	early := mustTS(t, "2025-03-01T10:00:00.000Z")
	late := mustTS(t, "2025-03-20T10:00:00.000Z")

	mk := func(ts time.Time) model.AggDelta {
		return model.AggDelta{Key: k, Plays: 1, MsPlayed: 100, FirstPlayedAt: ts, LastPlayedAt: ts}
	}
	if err := s.ApplyDeltas(ctx, []model.AggDelta{mk(early)}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyDeltas(ctx, []model.AggDelta{mk(late)}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetAggregate(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	if !got.FirstPlayedAt.Equal(early) {
		t.Errorf("FirstPlayedAt = %v, want it pinned to the earliest %v", got.FirstPlayedAt, early)
	}
	if !got.LastPlayedAt.Equal(late) {
		t.Errorf("LastPlayedAt = %v, want %v", got.LastPlayedAt, late)
	}
}

// TestMergeDeltasEqualsPerPlayApply: merging in memory is an optimisation, so it must be
// observationally identical to applying every delta individually.
func TestMergeDeltasEqualsPerPlayApply(t *testing.T) {
	ctx := context.Background()
	plays := storetest.Corpus(t)

	var all []model.AggDelta
	for _, p := range plays {
		all = append(all, model.AggregateDeltas(model.FactsFor(p, storetest.GenresFor(p)), madrid)...)
	}

	individual := storetest.NewStore(t)
	if err := individual.ApplyDeltas(ctx, all); err != nil {
		t.Fatal(err)
	}

	merged := storetest.NewStore(t)
	if err := merged.ApplyDeltas(ctx, model.MergeDeltas(all)); err != nil {
		t.Fatal(err)
	}

	keys := make([]model.AggKey, 0, len(all))
	seen := map[model.AggKey]bool{}
	for _, d := range all {
		if !seen[d.Key] {
			seen[d.Key] = true
			keys = append(keys, d.Key)
		}
	}

	a, err := individual.BatchGetAggregates(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	b, err := merged.BatchGetAggregates(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(a, b); diff != "" {
		t.Errorf("merged and per-delta application diverged (-individual +merged):\n%s", diff)
	}
	if len(model.MergeDeltas(all)) >= len(all) {
		t.Error("MergeDeltas did not reduce the write count for a multi-play batch")
	}
}

func TestBatchGetAggregatesLargeSet(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	// 250 keys exercises three chunks past the 100-key BatchGetItem limit.
	const n = 250
	keys := make([]model.AggKey, 0, n)
	deltas := make([]model.AggDelta, 0, n)
	for i := 0; i < n; i++ {
		k := model.AggKey{Dim: model.DimTrack, Period: "2025", EntityID: trackID(i)}
		keys = append(keys, k)
		deltas = append(deltas, model.AggDelta{Key: k, Plays: 1, MsPlayed: int64(i + 1)})
	}
	if err := s.ApplyDeltas(ctx, deltas); err != nil {
		t.Fatal(err)
	}

	got, err := s.BatchGetAggregates(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("got %d aggregates, want %d", len(got), n)
	}
	for i, k := range keys {
		if got[k].MsPlayed != int64(i+1) {
			t.Errorf("%s MsPlayed = %d, want %d", k, got[k].MsPlayed, i+1)
		}
	}
}

func TestBatchGetAggregatesHandlesDuplicateKeys(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	k := model.AggKey{Dim: model.DimTrack, Period: "2025", EntityID: "t1"}
	if err := s.ApplyDeltas(ctx, []model.AggDelta{{Key: k, Plays: 1, MsPlayed: 10}}); err != nil {
		t.Fatal(err)
	}
	// DynamoDB rejects a batch containing the same key twice, so the store must dedupe.
	got, err := s.BatchGetAggregates(ctx, []model.AggKey{k, k, k})
	if err != nil {
		t.Fatalf("duplicate keys in a batch must be deduped, not rejected: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d results, want 1", len(got))
	}
}

func TestBatchGetAggregatesMissingKeysAreAbsent(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	present := model.AggKey{Dim: model.DimTrack, Period: "2025", EntityID: "t1"}
	absent := model.AggKey{Dim: model.DimTrack, Period: "2025", EntityID: "nope"}
	if err := s.ApplyDeltas(ctx, []model.AggDelta{{Key: present, Plays: 1, MsPlayed: 10}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.BatchGetAggregates(ctx, []model.AggKey{present, absent})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[absent]; ok {
		t.Error("an absent key must not appear in the result map")
	}
	if len(got) != 1 {
		t.Errorf("got %d results, want 1", len(got))
	}
}

func TestGetAggregateNotFound(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	k := model.AggKey{Dim: model.DimTrack, Period: "2025", EntityID: "nothing"}

	if _, err := s.GetAggregate(ctx, k); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	// The convenience form treats "no plays" as a zero rather than an error.
	z, err := s.GetAggregateOrZero(ctx, k)
	if err != nil {
		t.Fatalf("GetAggregateOrZero: %v", err)
	}
	if z.Plays != 0 || z.Key != k {
		t.Errorf("zero aggregate = %+v", z)
	}
}

func TestPutAggregatesBatch(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	// 60 rows spans three BatchWriteItem chunks.
	const n = 60
	aggs := make([]model.Aggregate, 0, n)
	for i := 0; i < n; i++ {
		aggs = append(aggs, model.Aggregate{
			Key:      model.AggKey{Dim: model.DimArtist, Period: "2025", EntityID: trackID(i)},
			Plays:    int64(i),
			MsPlayed: int64(i * 100),
		})
	}
	if err := s.PutAggregates(ctx, aggs); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAggregate(ctx, aggs[42].Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plays != 42 || got.MsPlayed != 4200 {
		t.Errorf("aggs[42] = (%d, %d)", got.Plays, got.MsPlayed)
	}
}

// PutAggregate replaces absolutely -- it is the reconcile path, not an increment.
func TestPutAggregateOverwrites(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	k := model.AggKey{Dim: model.DimTrack, Period: "2025", EntityID: "t1"}

	if err := s.ApplyDeltas(ctx, []model.AggDelta{{Key: k, Plays: 99, MsPlayed: 9900}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAggregate(ctx, model.Aggregate{Key: k, Plays: 3, MsPlayed: 300}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAggregate(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plays != 3 || got.MsPlayed != 300 {
		t.Errorf("after PutAggregate = (%d, %d), want the absolute (3, 300)", got.Plays, got.MsPlayed)
	}
}

// TestQueryAggregatesHeatmapPrefix exercises the folded key layout: a year partition holds
// its own total at SK "ALL" plus that year's day rows, so a begins_with on the year returns
// days only.
func TestQueryAggregatesHeatmapPrefix(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	for _, day := range []string{"2025-03-01", "2025-03-02", "2025-12-31"} {
		k := model.AggKey{Dim: model.DimTotal, Period: model.Period(day), EntityID: model.TotalEntityID}
		if err := s.ApplyDeltas(ctx, []model.AggDelta{{Key: k, Plays: 1, MsPlayed: 100}}); err != nil {
			t.Fatal(err)
		}
	}
	yearKey := model.AggKey{Dim: model.DimTotal, Period: "2025", EntityID: model.TotalEntityID}
	if err := s.ApplyDeltas(ctx, []model.AggDelta{{Key: yearKey, Plays: 3, MsPlayed: 300}}); err != nil {
		t.Fatal(err)
	}

	days := collect(t, s.QueryAggregates(ctx, model.DimTotal, "2025", "2025-"))
	if len(days) != 3 {
		t.Fatalf("prefix query returned %d rows, want the 3 day rows:\n%+v", len(days), days)
	}
	for _, d := range days {
		if d.Key.Period.Granularity() != model.GranularityDay {
			t.Errorf("prefix query returned a non-day row: %s", d.Key)
		}
	}

	// Without a prefix the year total is included too.
	all := collect(t, s.QueryAggregates(ctx, model.DimTotal, "2025", ""))
	if len(all) != 4 {
		t.Errorf("unprefixed query returned %d rows, want 4 (3 days + the year total)", len(all))
	}
}

// ---------------------------------------------------------------------------
// play scanning
// ---------------------------------------------------------------------------

// TestPlaysSpansTwoUTCPartitionsForALocalMonth is the payoff of the UTC-partition decision:
// local March in Madrid begins in the UTC February partition, and the caller never has to
// know.
func TestPlaysSpansTwoUTCPartitionsForALocalMonth(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	// 23:30Z on 28 Feb is already 1 March in Madrid.
	inFeb := storetest.APIPlay(t, "2025-02-28T23:30:00.000Z", "t1")
	midMarch := storetest.APIPlay(t, "2025-03-15T12:00:00.000Z", "t2")
	// 22:30Z on 31 March is already 1 April in Madrid, so it must be EXCLUDED.
	inApril := storetest.APIPlay(t, "2025-03-31T22:30:00.000Z", "t3")
	// Well before the window.
	early := storetest.APIPlay(t, "2025-02-10T12:00:00.000Z", "t4")

	for _, p := range []model.Play{inFeb, midMarch, inApril, early} {
		if _, err := s.PutPlay(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	start, end, err := madrid.Bounds("2025-03")
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, s.Plays(ctx, start, end, store.PlayFilter{}))

	var ids []string
	for _, p := range got {
		ids = append(ids, p.TrackID)
	}
	if diff := cmp.Diff([]string{"t1", "t2"}, ids); diff != "" {
		t.Errorf("local March plays (-want +got):\n%s\n"+
			"t1 lives in the UTC 2025-02 partition; t3 is local April; t4 is out of range", diff)
	}
}

func TestPlaysOrderedOldestFirst(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	instants := []string{
		"2025-03-14T21:04:33.120Z",
		"2025-03-14T21:04:33.123Z", // the pair a variable-width format would invert
		"2025-03-14T09:00:00.000Z",
		"2025-03-20T10:00:00.000Z",
	}
	for i, in := range instants {
		if _, err := s.PutPlay(ctx, storetest.APIPlay(t, in, trackID(i))); err != nil {
			t.Fatal(err)
		}
	}

	got := collect(t, s.Plays(ctx,
		mustTS(t, "2025-03-01T00:00:00.000Z"),
		mustTS(t, "2025-04-01T00:00:00.000Z"),
		store.PlayFilter{}))

	if len(got) != len(instants) {
		t.Fatalf("got %d plays, want %d", len(got), len(instants))
	}
	for i := 1; i < len(got); i++ {
		if !got[i-1].PlayedAt.Before(got[i].PlayedAt) {
			t.Errorf("plays not ascending at %d: %s then %s", i,
				model.FormatTS(got[i-1].PlayedAt), model.FormatTS(got[i].PlayedAt))
		}
	}
}

func TestPlaysSourceFilter(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	if _, err := s.PutPlay(ctx, storetest.APIPlay(t, "2025-03-14T10:00:00.000Z", "t1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPlay(ctx, storetest.ExportPlay(t, "2025-03-14T11:00:00.000Z", "t2", 1000)); err != nil {
		t.Fatal(err)
	}

	from := mustTS(t, "2025-03-01T00:00:00.000Z")
	to := mustTS(t, "2025-04-01T00:00:00.000Z")

	all := collect(t, s.Plays(ctx, from, to, store.PlayFilter{}))
	if len(all) != 2 {
		t.Fatalf("unfiltered = %d, want 2", len(all))
	}
	exports := collect(t, s.Plays(ctx, from, to, store.PlayFilter{Source: model.SourceExport}))
	if len(exports) != 1 || exports[0].TrackID != "t2" {
		t.Errorf("export-filtered = %+v, want just t2", exports)
	}
	apis := collect(t, s.Plays(ctx, from, to, store.PlayFilter{Source: model.SourceAPI}))
	if len(apis) != 1 || apis[0].TrackID != "t1" {
		t.Errorf("api-filtered = %+v, want just t1", apis)
	}
}

func TestPlaysEmptyRange(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	at := mustTS(t, "2025-03-14T21:04:33.123Z")
	if got := collect(t, s.Plays(ctx, at, at, store.PlayFilter{})); len(got) != 0 {
		t.Errorf("empty range returned %d plays", len(got))
	}
}

// TestPlaysOfTrackGSI1ProjectionTrap documents a real hazard: GSI1 uses an INCLUDE
// projection, so attributes outside it come back as zero values rather than raising an
// error. A caller that needs artists or the album must re-read the base table.
func TestPlaysOfTrackGSI1ProjectionTrap(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	p := storetest.APIPlay(t, "2025-03-14T21:04:33.123Z", "t1",
		storetest.WithArtists("ar1", "ar2"), storetest.WithAlbum("al1"))
	if _, err := s.PutPlay(ctx, p); err != nil {
		t.Fatal(err)
	}

	got := collect(t, s.PlaysOfTrack(ctx, "t1",
		mustTS(t, "2025-01-01T00:00:00.000Z"),
		mustTS(t, "2026-01-01T00:00:00.000Z")))
	if len(got) != 1 {
		t.Fatalf("got %d plays, want 1", len(got))
	}
	g := got[0]

	// Projected attributes survive.
	if !g.PlayedAt.Equal(p.PlayedAt) {
		t.Errorf("PlayedAt = %v, want %v", g.PlayedAt, p.PlayedAt)
	}
	if g.TrackID != "t1" || g.MsPlayed != p.MsPlayed || g.Source != model.SourceAPI {
		t.Errorf("projected fields = %+v", g)
	}
	// Unprojected ones do not, silently.
	if g.AlbumID != "" {
		t.Errorf("AlbumID = %q; albumId is not in the GSI1 projection so it must be empty", g.AlbumID)
	}
	if len(g.ArtistIDs) != 0 {
		t.Errorf("ArtistIDs = %v; artistIds is not in the GSI1 projection", g.ArtistIDs)
	}
	// The base table still has everything.
	base := collect(t, s.Plays(ctx,
		mustTS(t, "2025-03-01T00:00:00.000Z"), mustTS(t, "2025-04-01T00:00:00.000Z"),
		store.PlayFilter{}))
	if len(base) != 1 || base[0].AlbumID != "al1" || len(base[0].ArtistIDs) != 2 {
		t.Errorf("base-table read lost data: %+v", base)
	}
}

func TestDeletePlay(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	p := storetest.APIPlay(t, "2025-03-14T21:04:33.123Z", "t1")
	if _, err := s.PutPlay(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePlay(ctx, p.PlayedAt, p.TrackID); err != nil {
		t.Fatal(err)
	}
	got := collect(t, s.Plays(ctx,
		mustTS(t, "2025-03-01T00:00:00.000Z"), mustTS(t, "2025-04-01T00:00:00.000Z"),
		store.PlayFilter{}))
	if len(got) != 0 {
		t.Errorf("play survived deletion: %+v", got)
	}
	// Deleting again is a no-op, so the importer can be re-run.
	if err := s.DeletePlay(ctx, p.PlayedAt, p.TrackID); err != nil {
		t.Errorf("second DeletePlay: %v", err)
	}
}

// ---------------------------------------------------------------------------
// cross-dimension invariants
// ---------------------------------------------------------------------------

// TestCorpusCrossDimensionInvariants records the structural relationships between
// dimensions, so nobody later "fixes" one of them into being wrong.
//
// Note the GENRE relationship in particular. Genres are a MANY-TO-MANY labelling: a play
// whose artists carry three genres contributes one play to each of three genre rows, so the
// genre sum can EXCEED the overall total -- while plays by genre-less artists (the common
// case) contribute nothing, pulling it down. There is therefore no ordering bound between
// the genre sum and the total in either direction. The quantity that IS bounded is the
// number of plays carrying at least one genre.
func TestCorpusCrossDimensionInvariants(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	plays := storetest.Corpus(t)
	for _, p := range plays {
		if _, err := s.RecordPlay(ctx, p, storetest.GenresFor(p)); err != nil {
			t.Fatalf("RecordPlay: %v", err)
		}
	}

	total, err := s.GetAggregate(ctx, model.AggKey{
		Dim: model.DimTotal, Period: model.PeriodAll, EntityID: model.TotalEntityID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total.Plays != int64(len(plays)) {
		t.Fatalf("TOTAL plays = %d, want %d", total.Plays, len(plays))
	}

	sum := func(dim model.Dim) (plays, ms int64) {
		for a, err := range s.QueryAggregates(ctx, dim, model.PeriodAll, "") {
			if err != nil {
				t.Fatal(err)
			}
			plays += a.Plays
			ms += a.MsPlayed
		}
		return
	}

	// Exactly one track per play, so this is an equality.
	trackPlays, trackMs := sum(model.DimTrack)
	if trackPlays != total.Plays {
		t.Errorf("sum(TRACK.plays) = %d, want exactly TOTAL %d", trackPlays, total.Plays)
	}
	if trackMs != total.MsPlayed {
		t.Errorf("sum(TRACK.ms) = %d, want exactly TOTAL %d", trackMs, total.MsPlayed)
	}

	// Multi-artist plays are credited to each artist, so this over-counts.
	artistPlays, _ := sum(model.DimArtist)
	if artistPlays < total.Plays {
		t.Errorf("sum(ARTIST.plays) = %d, want >= TOTAL %d", artistPlays, total.Plays)
	}
	if artistPlays == total.Plays {
		t.Error("the corpus should contain a multi-artist play, making the artist sum exceed the total")
	}

	// Some plays have no album, so this under-counts.
	albumPlays, _ := sum(model.DimAlbum)
	if albumPlays > total.Plays {
		t.Errorf("sum(ALBUM.plays) = %d, want <= TOTAL %d", albumPlays, total.Plays)
	}
	if albumPlays == total.Plays {
		t.Error("the corpus should contain an album-less play, making the album sum fall short")
	}

	// Genres: verify against the independently computed expectation rather than an
	// ordering bound, since none holds.
	var wantGenrePlays, playsWithAGenre int64
	for _, p := range plays {
		n := int64(len(model.FactsFor(p, storetest.GenresFor(p)).Genres))
		wantGenrePlays += n
		if n > 0 {
			playsWithAGenre++
		}
	}
	genrePlays, _ := sum(model.DimGenre)
	if genrePlays != wantGenrePlays {
		t.Errorf("sum(GENRE.plays) = %d, want %d", genrePlays, wantGenrePlays)
	}
	if playsWithAGenre > total.Plays {
		t.Errorf("plays carrying a genre = %d, cannot exceed TOTAL %d", playsWithAGenre, total.Plays)
	}
	if genrePlays <= total.Plays {
		t.Errorf("sum(GENRE.plays) = %d <= TOTAL %d; this corpus has multi-genre plays so it "+
			"should exceed the total -- the 'genre sum <= total' intuition is wrong",
			genrePlays, total.Plays)
	}

	// Day rows in a year partition must sum to that year's total.
	for _, year := range []model.Period{"2025", "2026"} {
		yearAgg, err := s.GetAggregateOrZero(ctx, model.AggKey{
			Dim: model.DimTotal, Period: year, EntityID: model.TotalEntityID,
		})
		if err != nil {
			t.Fatal(err)
		}
		var dayPlays, dayMs int64
		for a, err := range s.QueryAggregates(ctx, model.DimTotal, year, string(year)+"-") {
			if err != nil {
				t.Fatal(err)
			}
			dayPlays += a.Plays
			dayMs += a.MsPlayed
		}
		if dayPlays != yearAgg.Plays {
			t.Errorf("%s: day rows sum to %d plays, year total is %d", year, dayPlays, yearAgg.Plays)
		}
		if dayMs != yearAgg.MsPlayed {
			t.Errorf("%s: day rows sum to %d ms, year total is %d", year, dayMs, yearAgg.MsPlayed)
		}
	}

	// The estimated/exact split must stay a subset relationship.
	if total.MsPlayedExact > total.MsPlayed {
		t.Errorf("MsPlayedExact %d exceeds MsPlayed %d", total.MsPlayedExact, total.MsPlayed)
	}
	if r := total.EstimatedRatio(); r <= 0 || r >= 1 {
		t.Errorf("EstimatedRatio = %v; the corpus mixes sources so it should be strictly between 0 and 1", r)
	}
}

// The corpus straddles New Year in Madrid, so a play stored in the UTC 2025-12 partition
// belongs to local year 2026.
func TestCorpusYearBoundaryIsLocal(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	for _, p := range storetest.Corpus(t) {
		if _, err := s.RecordPlay(ctx, p, storetest.GenresFor(p)); err != nil {
			t.Fatal(err)
		}
	}

	// t3 played at 2025-12-31T23:30Z is local 2026-01-01.
	jan, err := s.GetAggregate(ctx, model.AggKey{
		Dim: model.DimTotal, Period: "2026-01-01", EntityID: model.TotalEntityID,
	})
	if err != nil {
		t.Fatalf("the New Year's Eve play should be filed under local 2026-01-01: %v", err)
	}
	if jan.Plays != 1 {
		t.Errorf("2026-01-01 plays = %d, want 1", jan.Plays)
	}
	// And it must NOT be under UTC's 2025-12-31.
	if _, err := s.GetAggregate(ctx, model.AggKey{
		Dim: model.DimTotal, Period: "2025-12-31", EntityID: model.TotalEntityID,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Error("a row exists for UTC day 2025-12-31; period keys must be local")
	}
}

// TestCanonicalQuery is the query docs/SPECS.md is written around: minutes listened to one
// artist in one year, answered by a single GetItem.
func TestCanonicalQuery(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	for _, p := range storetest.Corpus(t) {
		if _, err := s.RecordPlay(ctx, p, storetest.GenresFor(p)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.GetAggregate(ctx, model.AggKey{
		Dim: model.DimArtist, Period: "2026", EntityID: "ar1",
	})
	if err != nil {
		t.Fatalf("GetAggregate: %v", err)
	}
	// Local 2026 plays crediting ar1: t2 export 120000 and t1 export 205000.
	if got.Plays != 2 {
		t.Errorf("plays = %d, want 2", got.Plays)
	}
	if want := int64(325_000); got.MsPlayed != want {
		t.Errorf("msPlayed = %d, want %d", got.MsPlayed, want)
	}
	// Both are export-sourced, so the figure is exact.
	if got.MsPlayedExact != got.MsPlayed {
		t.Errorf("msPlayedExact = %d, want it to equal msPlayed %d", got.MsPlayedExact, got.MsPlayed)
	}
	if r := got.EstimatedRatio(); r != 0 {
		t.Errorf("EstimatedRatio = %v, want 0 for all-exact data", r)
	}
}

func trackID(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return "t" + string(digits[i])
	}
	return "t" + string(digits[i/100%10]) + string(digits[i/10%10]) + string(digits[i%10])
}
