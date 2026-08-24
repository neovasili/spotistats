package backfill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/neovasili/spotistats/internal/spotify"
	"github.com/neovasili/spotistats/internal/store"
)

// DefaultResolveLimit is how many tracks one unattended run will resolve.
//
// The figure is set by what it must NOT do rather than by how fast it could go. Spotify's
// development-mode window is roughly 500 requests before a 429 whose Retry-After runs 7.5 to 18
// hours, and capture spends about 150 a day on that same quota -- one recently-played per
// half-hourly run plus metadata for newly seen tracks.
//
// Capture is the job that must not fail: recently-played returns a rolling ~50-play window, so
// consecutive failures lose listening PERMANENTLY and no reconcile recovers it. 200 leaves
// generous headroom under the observed limit, which is the whole point: a resolver that never
// provokes the 429 cannot take capture down with it.
//
// The cost of that caution is honest and worth stating: at 200 a day a twelve-thousand-track
// backlog takes about two months. Unattended and slow beats fast and requiring discipline.
const DefaultResolveLimit = 200

// ResolveOptions configures one resolution run.
type ResolveOptions struct {
	// Limit caps how many tracks are fetched. Zero uses DefaultResolveLimit; negative means
	// "every remaining track", which only a human at a terminal should ask for.
	Limit int

	// Force ignores an active cooldown. For an operator who knows the quota has recovered
	// sooner than the API claimed; never for a scheduled run.
	Force bool

	// Progress is called periodically so a long run can report a live count.
	Progress func(done, total int)
}

// ResolveResult reports what a run did.
type ResolveResult struct {
	// Backlog is how many tracks were still on placeholder identity when the run began.
	Backlog int
	// Fetched, and the dimension rows written as a side effect of fetching.
	Fetched        int
	TracksWritten  int
	AlbumsWritten  int
	ArtistsWritten int
	// Remaining after this run.
	Remaining int

	// SuspendedUntil is set when the run stopped early, or did not start, because the API
	// quota is spent. Zero otherwise.
	SuspendedUntil time.Time
	// Skipped reports that the run did nothing because a cooldown was already active.
	Skipped bool

	Duration time.Duration
}

// Resolver upgrades placeholder track rows to real Spotify identity.
//
// It is shared by `spotistats resolve` and the scheduled resolve Lambda deliberately: the
// quota-safety rules -- the cooldown, the budget, stopping cleanly on a 429 -- are the entire
// substance of this job, and a second copy of them would eventually disagree with the first.
type Resolver struct {
	store    *store.Store
	enricher *Enricher
	log      *slog.Logger
	now      func() time.Time
}

func NewResolver(st *store.Store, api SpotifyAPI, log *slog.Logger, now func() time.Time) *Resolver {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if now == nil {
		now = time.Now
	}
	return &Resolver{store: st, enricher: NewEnricher(st, api, log), log: log, now: now}
}

// Backlog reports how many played tracks are still on placeholder identity, spending no API
// quota. It is what makes "how far along am I?" answerable without starting a run.
func (r *Resolver) Backlog(ctx context.Context) (int, error) {
	ids, err := PlayedTrackIDs(ctx, r.store)
	if err != nil {
		return 0, err
	}
	todo, err := r.enricher.Unresolved(ctx, ids)
	if err != nil {
		return 0, err
	}
	return len(todo), nil
}

// Run resolves a bounded batch.
//
// A spent quota is NOT an error. It is the expected ending: everything fetched before it is
// already durable, the backlog is unchanged by stopping, and returning an error would make a
// scheduled invocation fail nightly for a condition that is normal.
func (r *Resolver) Run(ctx context.Context, opts ResolveOptions) (ResolveResult, error) {
	started := r.now()
	res := ResolveResult{}

	if !opts.Force {
		until, reason, err := r.store.ResolveCooldownUntil(ctx)
		if err != nil {
			return res, err
		}
		if !until.IsZero() {
			// Stop ASKING, not merely stop succeeding. Every request made during a cooldown is
			// quota taken from capture for nothing.
			r.log.InfoContext(ctx, "resolve: cooling down; spending no quota",
				"until", until, "reason", reason)
			res.Skipped = true
			res.SuspendedUntil = until
			res.Duration = r.now().Sub(started)
			return res, nil
		}
	}

	ids, err := PlayedTrackIDs(ctx, r.store)
	if err != nil {
		return res, err
	}
	todo, err := r.enricher.Unresolved(ctx, ids)
	if err != nil {
		return res, err
	}
	res.Backlog = len(todo)
	res.Remaining = len(todo)
	if len(todo) == 0 {
		res.Duration = r.now().Sub(started)
		return res, nil
	}

	limit := opts.Limit
	switch {
	case limit == 0:
		limit = DefaultResolveLimit
	case limit < 0:
		limit = len(todo)
	}

	stats, rerr := r.enricher.Enrich(ctx, todo, limit, opts.Progress)
	res.Fetched = stats.Fetched
	res.TracksWritten = stats.TracksWritten
	res.AlbumsWritten = stats.AlbumsWritten
	res.ArtistsWritten = stats.ArtistsWritten
	res.Remaining = len(todo) - stats.Fetched
	res.Duration = r.now().Sub(started)

	if rerr != nil {
		var rl *spotify.RateLimitError
		if errors.As(rerr, &rl) {
			until := r.now().Add(rl.RetryAfter)
			// The API's own Retry-After, not a guess: observed cooldowns span 7.5 to 18 hours,
			// so any fixed figure is either wasteful or useless.
			if perr := r.store.PutResolveCooldown(ctx, until, "spotify 429"); perr != nil {
				// Failing to record it is worth shouting about -- the next run would spend
				// quota into a wall -- but it does not undo the work already stored.
				r.log.ErrorContext(ctx, "resolve: could not record the cooldown; the next run "+
					"may waste quota", "err", perr)
			}
			res.SuspendedUntil = until
			r.log.InfoContext(ctx, "resolve: quota spent; suspending",
				"until", until, "fetched", res.Fetched, "remaining", res.Remaining)
			return res, nil
		}
		return res, fmt.Errorf("resolve: %w", rerr)
	}
	return res, nil
}
