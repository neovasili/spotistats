package enrich_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/neovasili/spotistats/internal/enrich"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/musicbrainz"
	"github.com/neovasili/spotistats/internal/store"
	"github.com/neovasili/spotistats/internal/store/storetest"
	"github.com/neovasili/spotistats/internal/theaudiodb"
)

func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Shutdown()
	os.Exit(code)
}

// fakeMB scripts MusicBrainz.
type fakeMB struct {
	// resolved maps Spotify ID -> MBID. Absent means unlinked.
	resolved map[string]string
	// artists maps MBID -> facts. Absent means 404.
	artists map[string]musicbrainz.Artist
	// artistErr fails the per-artist lookup for one MBID.
	artistErr map[string]error
	// resolveErr fails the whole batch resolution.
	resolveErr   error
	resolveCalls int
	artistCalls  []string
	// afterArtist runs after each per-artist lookup, which happens INSIDE the loop and so
	// after the top-of-loop ctx.Err() guard. Cancelling here is the only way to reach the
	// mid-write path -- cancelling any earlier is caught by the guard instead, which is a
	// different branch and was never broken.
	afterArtist func(call int)
}

func (f *fakeMB) ResolveSpotifyArtists(_ context.Context, ids []string) (map[string]string, error) {
	f.resolveCalls++
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	out := map[string]string{}
	for _, id := range ids {
		if mbid, ok := f.resolved[id]; ok {
			out[id] = mbid
		}
	}
	return out, nil
}

func (f *fakeMB) Artist(_ context.Context, mbid string) (musicbrainz.Artist, bool, error) {
	f.artistCalls = append(f.artistCalls, mbid)
	if f.afterArtist != nil {
		f.afterArtist(len(f.artistCalls))
	}
	if err, ok := f.artistErr[mbid]; ok {
		return musicbrainz.Artist{}, false, err
	}
	a, ok := f.artists[mbid]
	return a, ok, nil
}

// fakeADB scripts TheAudioDB.
type fakeADB struct {
	artists map[string]theaudiodb.Artist
	errs    map[string]error
	calls   []string
}

func (f *fakeADB) ArtistByMBID(_ context.Context, mbid string) (theaudiodb.Artist, bool, error) {
	f.calls = append(f.calls, mbid)
	if err, ok := f.errs[mbid]; ok {
		return theaudiodb.Artist{}, false, err
	}
	a, ok := f.artists[mbid]
	return a, ok, nil
}

// seedArtists records a play for each artist so they appear in AGG#ARTIST#ALL, which is the
// enricher's work list.
func seedArtists(t *testing.T, st *store.Store, ids ...string) {
	t.Helper()
	ctx := context.Background()
	for i, id := range ids {
		p := storetest.APIPlay(t, fmt.Sprintf("2026-02-%02dT10:00:00.000Z", 1+i%28),
			fmt.Sprintf("t-%s", id), storetest.WithArtists(id))
		if _, err := st.RecordPlay(ctx, p, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func newEnricher(t *testing.T, st *store.Store, mb *fakeMB, adb *fakeADB) *enrich.Enricher {
	t.Helper()
	cfg := enrich.Config{Store: st, MusicBrainz: mb, Now: func() time.Time { return storetest.FixedNow }}
	if adb != nil {
		cfg.AudioDB = adb
	}
	e, err := enrich.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func groupFacts(mbid, name string) musicbrainz.Artist {
	return musicbrainz.Artist{
		ID: mbid, Name: name, Type: "Group", Country: "NL",
		LifeSpan: &musicbrainz.LifeSpan{Begin: "1995-04"},
		Genres:   []musicbrainz.Genre{{Name: "symphonic metal", Count: 24}},
	}
}

func TestRunEnrichesBothHalves(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	seedArtists(t, st, "ar1")

	mb := &fakeMB{
		resolved: map[string]string{"ar1": "mb1"},
		artists:  map[string]musicbrainz.Artist{"mb1": groupFacts("mb1", "Within Temptation")},
	}
	adb := &fakeADB{artists: map[string]theaudiodb.Artist{
		"mb1": {ID: "111478", Biography: "Prose.", Thumb: "https://r2.theaudiodb.com/t.jpg"},
	}}

	res, err := newEnricher(t, st, mb, adb).Run(ctx, enrich.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resolved != 1 || res.Unresolved != 0 {
		t.Errorf("resolved/unresolved = %d/%d", res.Resolved, res.Unresolved)
	}
	if res.FactsWritten != 1 || res.ProseWritten != 1 {
		t.Errorf("facts/prose = %d/%d", res.FactsWritten, res.ProseWritten)
	}

	got, err := st.GetArtistProfile(ctx, "ar1")
	if err != nil {
		t.Fatal(err)
	}
	if got.MBID != "mb1" || got.Country != "NL" || got.BeganAt != "1995-04" {
		t.Errorf("facts not stored: %+v", got)
	}
	if got.Biography != "Prose." || got.Images.Thumb == "" {
		t.Errorf("prose/images not stored: %+v", got)
	}
	if got.Sources.Facts != model.SourceMusicBrainz || got.Sources.Prose != model.SourceTheAudioDB {
		t.Errorf("provenance = %+v", got.Sources)
	}
}

// TestOneArtistFailingDoesNotAbandonTheRest is the orchestrator's core contract. One artist
// 404ing or erroring on TheAudioDB must not cost the other 199 their enrichment.
func TestOneArtistFailingDoesNotAbandonTheRest(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	seedArtists(t, st, "ar1", "ar2", "ar3")

	mb := &fakeMB{
		resolved: map[string]string{"ar1": "mb1", "ar2": "mb2", "ar3": "mb3"},
		artists: map[string]musicbrainz.Artist{
			"mb1": groupFacts("mb1", "One"),
			"mb3": groupFacts("mb3", "Three"),
		},
		// The middle artist errors outright.
		artistErr: map[string]error{"mb2": errors.New("503 forever")},
	}
	adb := &fakeADB{
		artists: map[string]theaudiodb.Artist{"mb1": {ID: "1", Biography: "Prose."}},
		// And the first one fails on the OTHER source, after its facts already resolved.
		errs: map[string]error{"mb3": errors.New("429")},
	}

	res, err := newEnricher(t, st, mb, adb).Run(ctx, enrich.Options{})
	if err != nil {
		t.Fatalf("a per-artist failure must not fail the run: %v", err)
	}
	if res.Candidates != 3 {
		t.Errorf("candidates = %d, want 3", res.Candidates)
	}
	// Every artist got a row: two resolved, one tombstoned.
	if res.Resolved != 2 || res.Unresolved != 1 {
		t.Errorf("resolved/unresolved = %d/%d, want 2/1", res.Resolved, res.Unresolved)
	}
	if res.SourceErrors[model.SourceMusicBrainz] != 1 {
		t.Errorf("musicbrainz errors = %d, want 1", res.SourceErrors[model.SourceMusicBrainz])
	}
	if res.SourceErrors[model.SourceTheAudioDB] != 1 {
		t.Errorf("theaudiodb errors = %d, want 1", res.SourceErrors[model.SourceTheAudioDB])
	}

	// ar3's FACTS must have persisted despite its prose failing: a partially-populated row is
	// normal, and discarding the half that worked would waste a rate-limited request.
	p3, err := st.GetArtistProfile(ctx, "ar3")
	if err != nil {
		t.Fatal(err)
	}
	if p3.MBID != "mb3" || p3.Sources.Facts != model.SourceMusicBrainz {
		t.Errorf("ar3 lost its facts when prose failed: %+v", p3)
	}
	if p3.Sources.Prose != "" {
		t.Errorf("ar3 claims prose it never got: %+v", p3.Sources)
	}
}

// An artist MusicBrainz has never linked gets a tombstone, so the nightly job stops asking.
func TestUnlinkedArtistIsTombstoned(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	seedArtists(t, st, "ar-unlinked")

	mb := &fakeMB{resolved: map[string]string{}}
	res, err := newEnricher(t, st, mb, nil).Run(ctx, enrich.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Unresolved != 1 {
		t.Errorf("unresolved = %d, want 1", res.Unresolved)
	}
	got, err := st.GetArtistProfile(ctx, "ar-unlinked")
	if err != nil {
		t.Fatalf("no tombstone written: %v", err)
	}
	if got.Resolved() || got.RefreshedAt.IsZero() {
		t.Errorf("not a usable tombstone: %+v", got)
	}
	// And no per-artist lookup was wasted on it.
	if len(mb.artistCalls) != 0 {
		t.Errorf("looked up %v for an unlinked artist", mb.artistCalls)
	}
}

// Name-keyed artists have no Spotify identity, so MusicBrainz has no URL to match. Asking about
// them spends a 1-req/s budget on a guaranteed miss.
func TestNameKeyedArtistsAreNotLookedUp(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	seedArtists(t, st, "ar-real", model.NameKey("Some Band"))

	mb := &fakeMB{resolved: map[string]string{"ar-real": "mb1"},
		artists: map[string]musicbrainz.Artist{"mb1": groupFacts("mb1", "Real")}}

	res, err := newEnricher(t, st, mb, nil).Run(ctx, enrich.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidates != 1 {
		t.Errorf("candidates = %d, want only the real Spotify ID", res.Candidates)
	}
}

// A fresh row is skipped. Formation year does not move, so refreshing it nightly spends a
// rate-limited request on an unchanged answer.
func TestFreshProfilesAreSkipped(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	seedArtists(t, st, "ar1")

	mb := &fakeMB{resolved: map[string]string{"ar1": "mb1"},
		artists: map[string]musicbrainz.Artist{"mb1": groupFacts("mb1", "A")}}
	e := newEnricher(t, st, mb, nil)

	if _, err := e.Run(ctx, enrich.Options{}); err != nil {
		t.Fatal(err)
	}
	before := len(mb.artistCalls)

	res, err := e.Run(ctx, enrich.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || res.Resolved != 0 {
		t.Errorf("skipped/resolved = %d/%d, want 1/0", res.Skipped, res.Resolved)
	}
	if len(mb.artistCalls) != before {
		t.Error("re-fetched a fresh artist")
	}

	t.Run("force overrides", func(t *testing.T) {
		res, err := e.Run(ctx, enrich.Options{Force: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.Resolved != 1 {
			t.Errorf("resolved = %d with Force", res.Resolved)
		}
	})
}

func TestLimitReportsRemaining(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	seedArtists(t, st, "ar1", "ar2", "ar3")

	mb := &fakeMB{
		resolved: map[string]string{"ar1": "mb1", "ar2": "mb2", "ar3": "mb3"},
		artists: map[string]musicbrainz.Artist{
			"mb1": groupFacts("mb1", "A"), "mb2": groupFacts("mb2", "B"),
			"mb3": groupFacts("mb3", "C"),
		},
	}
	res, err := newEnricher(t, st, mb, nil).Run(ctx, enrich.Options{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resolved != 2 || res.Remaining != 1 {
		t.Errorf("resolved/remaining = %d/%d, want 2/1", res.Resolved, res.Remaining)
	}
}

// MBIDs are resolved in ONE batched call, not one per artist. At 1 req/s this is the difference
// between a 20-second job and a 30-minute one.
func TestMBIDsAreResolvedInOneBatch(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	ids := make([]string, 0, 20)
	for i := range 20 {
		ids = append(ids, fmt.Sprintf("ar%02d", i))
	}
	seedArtists(t, st, ids...)

	mb := &fakeMB{resolved: map[string]string{}}
	if _, err := newEnricher(t, st, mb, nil).Run(ctx, enrich.Options{}); err != nil {
		t.Fatal(err)
	}
	if mb.resolveCalls != 1 {
		t.Errorf("resolve calls = %d for 20 artists, want 1 batched call", mb.resolveCalls)
	}
}

// A manual override is consulted FIRST, so it fixes an artist linked to the WRONG entity, not
// only an unlinked one.
func TestOverrideBeatsTheAssertedLink(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	seedArtists(t, st, "ar1")
	if err := st.PutMBIDOverride(ctx, "ar1", "mb-correct"); err != nil {
		t.Fatal(err)
	}

	mb := &fakeMB{
		// MusicBrainz asserts a DIFFERENT, wrong link.
		resolved: map[string]string{"ar1": "mb-wrong"},
		artists: map[string]musicbrainz.Artist{
			"mb-correct": groupFacts("mb-correct", "Right"),
			"mb-wrong":   groupFacts("mb-wrong", "Wrong"),
		},
	}
	if _, err := newEnricher(t, st, mb, nil).Run(ctx, enrich.Options{}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetArtistProfile(ctx, "ar1")
	if err != nil {
		t.Fatal(err)
	}
	if got.MBID != "mb-correct" {
		t.Errorf("MBID = %q, want the override to win", got.MBID)
	}
}

// A resolution failure is fatal for the run: without MBIDs there is nothing to do, and writing
// tombstones for everything would poison the cache for 180 days.
func TestResolutionFailureDoesNotTombstoneEveryone(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	seedArtists(t, st, "ar1", "ar2")

	mb := &fakeMB{resolveErr: errors.New("503 for everything")}
	_, err := newEnricher(t, st, mb, nil).Run(ctx, enrich.Options{})
	if err == nil {
		t.Fatal("a total resolution failure must be an error, not silent tombstoning")
	}
	if _, gerr := st.GetArtistProfile(ctx, "ar1"); gerr == nil {
		t.Error("a tombstone was written despite resolution failing; it would suppress " +
			"retries for 180 days")
	}
}

// Without a TheAudioDB key the MusicBrainz half still runs, which is where every structured
// fact comes from. Only prose and artwork degrade.
func TestRunsWithoutTheAudioDB(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	seedArtists(t, st, "ar1")

	mb := &fakeMB{resolved: map[string]string{"ar1": "mb1"},
		artists: map[string]musicbrainz.Artist{"mb1": groupFacts("mb1", "A")}}

	res, err := newEnricher(t, st, mb, nil).Run(ctx, enrich.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FactsWritten != 1 || res.ProseWritten != 0 {
		t.Errorf("facts/prose = %d/%d", res.FactsWritten, res.ProseWritten)
	}
	got, err := st.GetArtistProfile(ctx, "ar1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Country != "NL" || got.Biography != "" {
		t.Errorf("profile = %+v", got)
	}
}

func TestUnresolvedRatioIsTheAlarmableFigure(t *testing.T) {
	// A raw count rises slowly as obscure artists accumulate; a sudden RATIO jump means an
	// upstream shape change, which is the thing worth waking someone for.
	for _, tc := range []struct {
		res  enrich.Result
		want float64
	}{
		{enrich.Result{Resolved: 9, Unresolved: 1}, 0.1},
		{enrich.Result{Resolved: 0, Unresolved: 0}, 0},
		{enrich.Result{Resolved: 0, Unresolved: 5}, 1},
	} {
		if got := tc.res.UnresolvedRatio(); got != tc.want {
			t.Errorf("UnresolvedRatio(%+v) = %v, want %v", tc.res, got, tc.want)
		}
	}
}

// TestSingleFlightLock is what actually enforces the constraint reserved concurrency was
// supposed to.
//
// Both upstream rate limits are per-IP, so two overlapping runs double the real request rate and
// MusicBrainz answers 503 to EVERYTHING from that IP -- including the other run. A Lambda
// reservation cannot be relied on: AWS rejects ANY reservation on an account whose total
// concurrency limit is 10, and this deployment is on such an account. A conditional write needs
// no quota, and additionally covers a manual invoke landing during the scheduled run.
func TestSingleFlightLock(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	seedArtists(t, st, "ar1")

	mb := &fakeMB{resolved: map[string]string{"ar1": "mb1"},
		artists: map[string]musicbrainz.Artist{"mb1": groupFacts("mb1", "A")}}

	// Hold the lock as if a run were already in flight.
	if err := st.AcquireEnrichLock(ctx, enrich.LockTTL); err != nil {
		t.Fatal(err)
	}

	_, err := newEnricher(t, st, mb, nil).Run(ctx, enrich.Options{})
	if !errors.Is(err, enrich.ErrLockHeld) {
		t.Fatalf("Run = %v, want ErrLockHeld while another run holds it", err)
	}
	// And it did no work.
	if mb.resolveCalls != 0 {
		t.Errorf("made %d resolve calls while locked out", mb.resolveCalls)
	}

	t.Run("the lock is released so the next run proceeds", func(t *testing.T) {
		if err := st.ReleaseEnrichLock(ctx); err != nil {
			t.Fatal(err)
		}
		res, err := newEnricher(t, st, mb, nil).Run(ctx, enrich.Options{})
		if err != nil {
			t.Fatalf("Run after release = %v", err)
		}
		if res.Resolved != 1 {
			t.Errorf("resolved = %d", res.Resolved)
		}
	})

	t.Run("a normal run releases its own lock", func(t *testing.T) {
		// Otherwise every second run would be a no-op until the lease lapsed.
		if err := st.AcquireEnrichLock(ctx, enrich.LockTTL); err != nil {
			t.Errorf("the lock was not released by the completed run: %v", err)
		}
		if err := st.ReleaseEnrichLock(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// A run killed mid-flight -- Lambda timeout, OOM, a deploy -- must not hold the lock forever.
// That would silently stop all enrichment, which is a worse failure than the overlap it
// prevents.
func TestExpiredLockIsReclaimable(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// A lease that has already lapsed, as a killed run leaves behind. The store's clock is
	// fixed at FixedNow, so a negative TTL is an expired lock.
	if err := st.AcquireEnrichLock(ctx, -time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.AcquireEnrichLock(ctx, enrich.LockTTL); err != nil {
		t.Errorf("an expired lock was not reclaimable: %v", err)
	}
}

// The TTL must exceed the Lambda timeout, or a normal run's own lease expires underneath it and
// a concurrent invoke could start mid-run.
func TestLockTTLExceedsTheLambdaTimeout(t *testing.T) {
	const lambdaTimeout = 5 * time.Minute
	if enrich.LockTTL <= lambdaTimeout {
		t.Errorf("LockTTL (%v) <= the Lambda timeout (%v): a run could outlive its own lock",
			enrich.LockTTL, lambdaTimeout)
	}
}

// TestDeadlineMidWriteIsACleanStop guards a defect that reached production.
//
// The loop checks ctx.Err() at the top of each iteration, so a deadline that has already passed
// ends the run cleanly. But the deadline can also land in the MIDDLE of a write, and the store
// then returns "PutItem: context deadline exceeded" -- which the fatal-store-error branch
// reported as a failed run. The 5-minute Lambda timeout produced exactly that: a FAILED
// invocation, and a fired ExternalEnrichFailed alarm, for a job whose entire design is to be cut
// off partway and resumed by the next run.
//
// Running out of time is the expected ending here, not an error.
func TestDeadlineMidWriteIsACleanStop(t *testing.T) {
	st := storetest.NewStore(t)
	seedArtists(t, st, "ar1", "ar2", "ar3")

	mb := &fakeMB{
		resolved: map[string]string{"ar1": "mb1", "ar2": "mb2", "ar3": "mb3"},
		artists: map[string]musicbrainz.Artist{
			"mb1": groupFacts("mb1", "A"), "mb2": groupFacts("mb2", "B"),
			"mb3": groupFacts("mb3", "C"),
		},
	}

	// Cancelled during the SECOND artist's lookup. That point matters: it is inside the loop
	// and past the top-of-loop ctx.Err() guard, so the cancellation can only surface from the
	// store write. Cancelling any earlier exercises the guard, which was never broken.
	ctx, cancel := context.WithCancel(context.Background())
	mb.afterArtist = func(call int) {
		if call == 2 {
			cancel()
		}
	}

	res, err := newEnricher(t, st, mb, nil).Run(ctx, enrich.Options{})
	if err != nil {
		t.Fatalf("a deadline was reported as a run failure: %v", err)
	}
	// The first artist completed before the cancellation, so the run has real work to report.
	if res.Resolved != 1 {
		t.Errorf("resolved = %d, want 1: the work done before the cutoff must still count",
			res.Resolved)
	}
	// And it must say how much is left, or the operator cannot tell a finished run from a
	// truncated one.
	if res.Remaining != 2 {
		t.Errorf("remaining = %d, want 2; the next run needs to know there is work", res.Remaining)
	}
}

// The lock must actually be RELEASED on a clean finish, not left to expire.
//
// In production the enrich role had no dynamodb:DeleteItem, so every release failed with
// AccessDenied, the lock survived its whole 15-minute TTL, and the retry after a failed run
// found it held and exited having done nothing. This asserts the release happens; the IAM half
// is asserted in infra.
func TestLockIsReleasedSoAnImmediateRerunCanProceed(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	seedArtists(t, st, "ar1")

	mb := &fakeMB{
		resolved: map[string]string{"ar1": "mb1"},
		artists:  map[string]musicbrainz.Artist{"mb1": groupFacts("mb1", "A")},
	}
	if _, err := newEnricher(t, st, mb, nil).Run(ctx, enrich.Options{}); err != nil {
		t.Fatal(err)
	}

	// A second run immediately afterwards must NOT be turned away by a stale lock. It has
	// nothing to do (the profile is fresh), which is a different outcome from being blocked.
	res, err := newEnricher(t, st, mb, nil).Run(ctx, enrich.Options{})
	if err != nil {
		t.Fatalf("the second run was refused, so the lock was never released: %v", err)
	}
	if res.Skipped != 1 {
		t.Errorf("skipped = %d, want 1: the second run should have seen a fresh profile "+
			"rather than being blocked by a held lock", res.Skipped)
	}
}
