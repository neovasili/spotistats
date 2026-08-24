// Package enrich orchestrates external artist enrichment from MusicBrainz and TheAudioDB.
//
// # Why this is its own job and not part of capture
//
// Capture's contract is never to lose a play. MusicBrainz allows one request per second per IP
// and answers 503 to EVERYTHING from that IP once exceeded; TheAudioDB allows 30 a minute on a
// free key. Putting either in the capture path would make play durability depend on two
// third-party services with hard throttles and no SLA.
//
// So this is a separate, resumable, budget-bounded job that can fail completely without
// touching a single play, aggregate or leaderboard.
package enrich

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/musicbrainz"
	"github.com/neovasili/spotistats/internal/store"
	"github.com/neovasili/spotistats/internal/theaudiodb"
)

// MusicBrainzAPI is the slice of the MusicBrainz client this package needs.
type MusicBrainzAPI interface {
	ResolveSpotifyArtists(ctx context.Context, spotifyIDs []string) (map[string]string, error)
	Artist(ctx context.Context, mbid string) (musicbrainz.Artist, bool, error)
}

// AudioDBAPI is the slice of TheAudioDB client this package needs.
//
// Optional: a run with no key configured still does the MusicBrainz half, which is where every
// structured fact comes from. Prose and artwork are the part that degrades.
type AudioDBAPI interface {
	ArtistByMBID(ctx context.Context, mbid string) (theaudiodb.Artist, bool, error)
}

// Config configures an Enricher.
type Config struct {
	Store       *store.Store
	MusicBrainz MusicBrainzAPI
	// AudioDB may be nil. See AudioDBAPI.
	AudioDB AudioDBAPI
	// Language is the biography language to keep. Empty means English.
	Language string
	Now      func() time.Time
	Logger   *slog.Logger
}

// Enricher resolves and stores external artist facts.
type Enricher struct {
	store *store.Store
	mb    MusicBrainzAPI
	adb   AudioDBAPI
	lang  string
	now   func() time.Time
	log   *slog.Logger
}

// New builds an Enricher.
func New(cfg Config) (*Enricher, error) {
	if cfg.Store == nil {
		return nil, errors.New("enrich: a store is required")
	}
	if cfg.MusicBrainz == nil {
		return nil, errors.New("enrich: a MusicBrainz client is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Enricher{
		store: cfg.Store, mb: cfg.MusicBrainz, adb: cfg.AudioDB,
		lang: cfg.Language, now: now, log: log,
	}, nil
}

// ErrLockHeld means another run is already in progress. Callers should treat it as a no-op
// rather than an error: the work is being done, just not by them.
var ErrLockHeld = errors.New("enrich: another run is in progress")

// Options bounds one run.
type Options struct {
	// Limit caps how many artists are enriched. Zero means no cap.
	Limit int
	// Force re-enriches artists whose row is still fresh.
	Force bool
	// ArtistID enriches exactly one artist, ignoring staleness. Used by `--artist`.
	ArtistID string
}

// Result reports what a run did.
type Result struct {
	Candidates int
	// Skipped counts artists whose EXTERNAL row was still fresh.
	Skipped    int
	Resolved   int
	Unresolved int
	// FactsWritten and ProseWritten count the halves that actually populated, which is what
	// makes a partially-degraded run legible.
	FactsWritten int
	ProseWritten int
	// SourceErrors counts per-artist failures by source. These are logged and skipped, never
	// fatal: one artist 404ing must not abandon the other 199.
	SourceErrors map[string]int
	// Remaining is how many candidates were not attempted, whether from Limit or a deadline.
	Remaining int
	Duration  time.Duration
}

// UnresolvedRatio is the share of attempted artists that could not be resolved.
//
// This is the figure worth alarming on rather than the raw count: a sudden jump means an
// upstream shape change, where a slowly rising count just means more obscure artists.
func (r Result) UnresolvedRatio() float64 {
	attempted := r.Resolved + r.Unresolved
	if attempted == 0 {
		return 0
	}
	return float64(r.Unresolved) / float64(attempted)
}

// LogAttrs renders the result for structured logging.
func (r Result) LogAttrs() []any {
	return []any{
		"candidates", r.Candidates, "skipped", r.Skipped,
		"resolved", r.Resolved, "unresolved", r.Unresolved,
		"factsWritten", r.FactsWritten, "proseWritten", r.ProseWritten,
		"sourceErrors", r.SourceErrors, "remaining", r.Remaining,
		"unresolvedRatio", r.UnresolvedRatio(), "durationMs", r.Duration.Milliseconds(),
	}
}

// LockTTL bounds how long one run may hold the single-flight lock.
//
// Longer than the Lambda's 5-minute timeout so a normal run never has its own lease expire
// underneath it, and short enough that a killed run does not block enrichment for long.
const LockTTL = 15 * time.Minute

// Run enriches artists, oldest work first.
//
// Single-flight: overlapping runs would double the real request rate against two per-IP limits
// and earn a 503 for everything. A second concurrent run returns ErrLockHeld rather than
// competing.
func (e *Enricher) Run(ctx context.Context, opts Options) (Result, error) {
	started := e.now()
	res := Result{SourceErrors: map[string]int{}}

	if err := e.store.AcquireEnrichLock(ctx, LockTTL); err != nil {
		if errors.Is(err, store.ErrEnrichLockHeld) {
			// Not a failure. The other run is doing the work.
			e.log.InfoContext(ctx, "enrich: another run holds the lock; exiting")
			return res, ErrLockHeld
		}
		return res, fmt.Errorf("enrich: acquire lock: %w", err)
	}
	defer func() {
		// Best-effort: the lease expires anyway, so failing here costs latency, not
		// correctness, and must not fail a run that already did the work.
		if err := e.store.ReleaseEnrichLock(ctx); err != nil {
			e.log.WarnContext(ctx, "enrich: could not release the lock; it will expire",
				"err", err)
		}
	}()

	candidates, err := e.workList(ctx, opts)
	if err != nil {
		return res, err
	}
	res.Candidates = len(candidates)

	todo, skipped, err := e.filterFresh(ctx, candidates, opts)
	if err != nil {
		return res, err
	}
	res.Skipped = skipped
	if opts.Limit > 0 && len(todo) > opts.Limit {
		res.Remaining = len(todo) - opts.Limit
		todo = todo[:opts.Limit]
	}
	if len(todo) == 0 {
		res.Duration = e.now().Sub(started)
		return res, nil
	}

	// Resolve every MBID up front, in batches of 100. This is the single most valuable
	// optimisation available: at 1 req/s, per-artist resolution turns a 20-second job into a
	// 30-minute one.
	mbids, err := e.resolveMBIDs(ctx, todo)
	if err != nil {
		return res, err
	}

	for i, id := range todo {
		if err := ctx.Err(); err != nil {
			// A deadline is not a failure: report what is left so the next run continues.
			res.Remaining += len(todo) - i
			res.Duration = e.now().Sub(started)
			return res, nil
		}

		profile, perr := e.enrichOne(ctx, id, mbids[id], &res)
		if perr != nil {
			// Per-artist errors are logged and skipped. One artist failing must not abandon
			// the rest of the batch, and whatever DID resolve is still worth storing.
			e.log.WarnContext(ctx, "enrich: artist failed; continuing",
				"artistId", id, "err", perr)
		}
		if err := e.store.PutArtistProfile(ctx, profile); err != nil {
			// Running out of TIME is not a store failure, even though it surfaces as one.
			//
			// The check at the top of this loop catches a deadline that has already passed,
			// but the deadline can also land mid-write -- and it did in production, where a
			// 5-minute Lambda timeout produced "PutItem: context deadline exceeded" and a
			// FAILED invocation for a job whose whole design is to be cut off and resumed.
			// The alarm that fired was reporting the timeout, not a broken table.
			if isDeadline(err) {
				res.Remaining += len(todo) - i
				res.Duration = e.now().Sub(started)
				return res, nil
			}
			// A genuine store failure IS fatal: it means the next run repeats this work, and
			// if the table is unavailable it will repeat all of it.
			return res, fmt.Errorf("enrich: write profile %s: %w", id, err)
		}
		if profile.Resolved() {
			res.Resolved++
		} else {
			res.Unresolved++
		}
		if profile.Sources.Facts != "" {
			res.FactsWritten++
		}
		if profile.Sources.Prose != "" {
			res.ProseWritten++
		}

		if err := e.store.PutExternalEnrichCursor(ctx, id); err != nil {
			e.log.WarnContext(ctx, "enrich: could not checkpoint; the next run may repeat work",
				"artistId", id, "err", err)
		}
	}

	res.Duration = e.now().Sub(started)
	return res, nil
}

// enrichOne builds one profile, tolerating a failure in either source.
//
// Always returns a profile: an unresolvable artist yields a tombstone rather than nothing, so
// the nightly job stops re-asking about an answer MusicBrainz has already given.
func (e *Enricher) enrichOne(
	ctx context.Context, spotifyID, mbid string, res *Result,
) (model.ArtistProfile, error) {
	tombstone := model.ArtistProfile{ArtistID: spotifyID, RefreshedAt: e.now()}
	if mbid == "" {
		return tombstone, nil
	}

	facts, found, err := e.mb.Artist(ctx, mbid)
	if err != nil {
		res.SourceErrors[model.SourceMusicBrainz]++
		return tombstone, fmt.Errorf("musicbrainz: %w", err)
	}
	if !found {
		// The MBID resolved but the entity is gone. Tombstone it: a dangling MBID will not
		// start existing tomorrow.
		return tombstone, nil
	}
	profile := musicbrainz.ToProfile(spotifyID, facts)
	profile.RefreshedAt = e.now()

	if e.adb == nil {
		return profile, nil
	}
	extras, found, err := e.adb.ArtistByMBID(ctx, mbid)
	if err != nil {
		// The facts half is already good. Returning it with an error lets the caller log the
		// degradation and still persist what resolved.
		res.SourceErrors[model.SourceTheAudioDB]++
		return profile, fmt.Errorf("theaudiodb: %w", err)
	}
	if !found {
		return profile, nil
	}
	return theaudiodb.Merge(profile, extras, e.lang), nil
}

// resolveMBIDs maps Spotify IDs to MBIDs, preferring manual overrides.
//
// Overrides are consulted FIRST so they can correct an artist MusicBrainz linked to the wrong
// entity, not merely one it has not linked at all.
func (e *Enricher) resolveMBIDs(ctx context.Context, ids []string) (map[string]string, error) {
	overrides, err := e.store.GetMBIDOverrides(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("enrich: read mbid overrides: %w", err)
	}

	needLookup := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := overrides[id]; !ok {
			needLookup = append(needLookup, id)
		}
	}

	resolved, err := e.mb.ResolveSpotifyArtists(ctx, needLookup)
	if err != nil {
		// A resolution failure is fatal for the run: without MBIDs there is nothing to do, and
		// writing tombstones for everything would poison the cache for 180 days.
		return nil, fmt.Errorf("enrich: resolve mbids: %w", err)
	}

	out := make(map[string]string, len(ids))
	for id, mbid := range resolved {
		out[id] = mbid
	}
	for id, mbid := range overrides {
		out[id] = mbid
	}
	return out, nil
}

// workList is every artist ever played, or the single artist a run was pointed at.
func (e *Enricher) workList(ctx context.Context, opts Options) ([]string, error) {
	if opts.ArtistID != "" {
		return []string{opts.ArtistID}, nil
	}
	var ids []string
	for agg, err := range e.store.QueryAggregates(ctx, model.DimArtist, model.PeriodAll, "") {
		if err != nil {
			return nil, fmt.Errorf("enrich: list artists: %w", err)
		}
		id := agg.Key.EntityID
		// Name-keyed artists have no Spotify identity, so MusicBrainz has no URL to match and
		// there is nothing to look up. They resolve once Spotify enrichment gives them a real
		// ID, and asking about them meanwhile spends a rate-limited request on a guaranteed
		// miss.
		if id == "" || model.IsNameKey(id) {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// filterFresh drops artists whose EXTERNAL row is younger than ExternalStaleAfter.
func (e *Enricher) filterFresh(
	ctx context.Context, ids []string, opts Options,
) (todo []string, skipped int, err error) {
	if opts.Force || opts.ArtistID != "" {
		return ids, 0, nil
	}
	existing, err := e.store.GetArtistProfiles(ctx, ids)
	if err != nil {
		return nil, 0, fmt.Errorf("enrich: read existing profiles: %w", err)
	}
	for _, id := range ids {
		p, ok := existing[id]
		if ok && !e.store.ExternalStale(p.RefreshedAt) {
			skipped++
			continue
		}
		todo = append(todo, id)
	}
	return todo, skipped, nil
}

// isDeadline reports whether err is the run simply running out of time.
//
// It must look through the store's error wrapper, which is why errors.Is is used rather than a
// string match: store.Error wraps the SDK error, which wraps context.DeadlineExceeded.
func isDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
