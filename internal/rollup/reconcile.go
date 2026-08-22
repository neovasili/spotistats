package rollup

import (
	"context"
	"fmt"
	"time"

	"github.com/neovasili/spotistats/internal/model"
)

// ReconcileResult reports what a reconcile found.
type ReconcileResult struct {
	PlaysRead      int
	RowsChecked    int
	RowsCorrected  int
	PropagatedRows int
	// From and To bound the reconciled range, for logging.
	From, To time.Time
}

// Reconcile repairs aggregate drift over the trailing windowDays.
//
// See the package doc for why this recomputes month-and-finer rows and then PROPAGATES the
// difference upward, rather than recomputing year and all-time rows directly: a windowed read
// cannot determine an all-time counter, and treating it as if it could would destroy history.
func (r *Rollup) Reconcile(ctx context.Context, windowDays int) (ReconcileResult, error) {
	if windowDays <= 0 {
		windowDays = r.window
	}

	// Expand the window to whole local months. A month row can only be recomputed from ALL of
	// that month's plays, so a window ending mid-month must still read the whole month.
	now := r.now()
	windowStart := now.AddDate(0, 0, -windowDays)
	months := r.monthsBetween(windowStart, now)
	if len(months) == 0 {
		return ReconcileResult{From: windowStart, To: now}, nil
	}

	from, _, err := r.cal.Bounds(months[0])
	if err != nil {
		return ReconcileResult{}, err
	}
	_, to, err := r.cal.Bounds(months[len(months)-1])
	if err != nil {
		return ReconcileResult{}, err
	}

	res := ReconcileResult{From: from, To: to}
	r.log.InfoContext(ctx, "rollup: reconciling",
		"months", len(months), "from", model.FormatTS(from), "to", model.FormatTS(to))

	recomputed, plays, err := r.recomputeFromPlays(ctx, from, to)
	if err != nil {
		return res, err
	}
	res.PlaysRead = plays

	// Only rows whose period is fully inside the read range can be compared against a
	// recomputed absolute value. Everything coarser is repaired by propagation.
	inRange := make(map[model.Period]bool, len(months))
	for _, m := range months {
		inRange[m] = true
	}

	var absolute []model.Aggregate
	var corrections []model.AggDelta

	// Any stored row for these months that the recompute did NOT produce has no plays behind
	// it any more -- a deleted play, or a superseded api-sourced month. It must be zeroed, or a
	// phantom entity lingers in every leaderboard.
	stored, err := r.storedRowsFor(ctx, months)
	if err != nil {
		return res, err
	}
	for key := range stored {
		if _, ok := recomputed[key]; !ok {
			recomputed[key] = model.Aggregate{Key: key}
		}
	}

	keys := make([]model.AggKey, 0, len(recomputed))
	for k := range recomputed {
		keys = append(keys, k)
	}
	current, err := r.store.BatchGetAggregates(ctx, keys)
	if err != nil {
		return res, err
	}

	for key, want := range recomputed {
		if !inRange[key.Period] && key.Period.Granularity() != model.GranularityDay {
			continue
		}
		res.RowsChecked++

		have := current[key]
		if aggregatesEqual(have, want) {
			continue
		}
		res.RowsCorrected++

		// The month or day row is rewritten absolutely: the recompute knows its true value.
		want.Key = key
		absolute = append(absolute, want)

		// The year and all-time rows get the DIFFERENCE. A delta is meaningful against a
		// counter of any span; an absolute value from a windowed read is not.
		if d := diffDelta(key, have, want); d != nil {
			corrections = append(corrections, d...)
		}
	}

	if len(absolute) > 0 {
		if err := r.store.PutAggregates(ctx, absolute); err != nil {
			return res, fmt.Errorf("rollup: write corrected rows: %w", err)
		}
	}
	if len(corrections) > 0 {
		if err := r.store.ApplyDeltas(ctx, model.MergeDeltas(corrections)); err != nil {
			return res, fmt.Errorf("rollup: propagate corrections: %w", err)
		}
		res.PropagatedRows = len(model.MergeDeltas(corrections))
	}

	if res.RowsCorrected > 0 {
		r.log.WarnContext(ctx, "rollup: corrected aggregate drift",
			"rowsChecked", res.RowsChecked, "rowsCorrected", res.RowsCorrected,
			"propagated", res.PropagatedRows)
	}
	return res, nil
}

// ReconcileAll rebuilds every aggregate from the complete play history.
//
// Deliberately not part of the nightly run: it rewrites every aggregate row, so on a large
// dataset it costs real write capacity. It is the repair for drift that originated outside any
// window -- a bad backfill, or a bug that has been miscounting for months.
func (r *Rollup) ReconcileAll(ctx context.Context, from, to time.Time) (ReconcileResult, error) {
	if from.IsZero() {
		// Spotify launched in 2008; nothing can predate it.
		from = time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if to.IsZero() {
		to = r.now().Add(24 * time.Hour)
	}
	res := ReconcileResult{From: from, To: to}

	recomputed, plays, err := r.recomputeFromPlays(ctx, from, to)
	if err != nil {
		return res, err
	}
	res.PlaysRead = plays

	// Absolute rewrite of everything, including the year and all-time rows: with the full
	// history in hand the recomputed values ARE the truth.
	all := make([]model.Aggregate, 0, len(recomputed))
	for key, a := range recomputed {
		a.Key = key
		all = append(all, a)
	}
	res.RowsChecked = len(all)
	res.RowsCorrected = len(all)

	if err := r.store.PutAggregates(ctx, all); err != nil {
		return res, fmt.Errorf("rollup: full rewrite: %w", err)
	}
	r.log.InfoContext(ctx, "rollup: full reconcile complete",
		"playsRead", plays, "rowsWritten", len(all))
	return res, nil
}

// recomputeFromPlays streams plays in [from, to) and accumulates the aggregates they imply.
func (r *Rollup) recomputeFromPlays(
	ctx context.Context, from, to time.Time,
) (map[model.AggKey]model.Aggregate, int, error) {
	// Genres live on artist rows, so they are resolved lazily and cached: a play's artists
	// recur constantly, and re-reading them per play would dominate the run.
	genres := newGenreCache(r.store)

	acc := map[model.AggKey]model.Aggregate{}
	plays := 0

	for p, err := range r.store.Plays(ctx, from, to, storePlayFilter()) {
		if err != nil {
			return nil, plays, fmt.Errorf("rollup: read plays: %w", err)
		}
		plays++

		g, gerr := genres.For(ctx, p.ArtistIDs)
		if gerr != nil {
			// A missing artist row means missing genre attribution, not a reason to abandon the
			// reconcile: everything else about the play is still recomputable.
			r.log.WarnContext(ctx, "rollup: genre resolution failed during reconcile",
				"trackId", p.TrackID, "err", gerr)
		}

		for _, d := range model.AggregateDeltas(model.FactsFor(p, g), r.cal) {
			a := acc[d.Key]
			a.Key = d.Key
			a.Plays += d.Plays
			a.PlaysExact += d.PlaysExact
			a.MsPlayed += d.MsPlayed
			a.MsPlayedExact += d.MsPlayedExact
			if !d.FirstPlayedAt.IsZero() &&
				(a.FirstPlayedAt.IsZero() || d.FirstPlayedAt.Before(a.FirstPlayedAt)) {
				a.FirstPlayedAt = d.FirstPlayedAt
			}
			if d.LastPlayedAt.After(a.LastPlayedAt) {
				a.LastPlayedAt = d.LastPlayedAt
			}
			acc[d.Key] = a
		}
	}
	return acc, plays, nil
}

// storedRowsFor returns the keys currently stored for the given months, so rows that no longer
// have plays behind them can be zeroed rather than left as phantoms.
func (r *Rollup) storedRowsFor(
	ctx context.Context, months []model.Period,
) (map[model.AggKey]struct{}, error) {
	out := map[model.AggKey]struct{}{}
	for _, m := range months {
		for _, dim := range model.AllDims() {
			for a, err := range r.store.QueryAggregates(ctx, dim, m, "") {
				if err != nil {
					return nil, fmt.Errorf("rollup: read stored rows for %s: %w", m, err)
				}
				if a.Key.Dim != dim {
					continue
				}
				out[a.Key] = struct{}{}
			}
		}
	}
	return out, nil
}

// monthsBetween lists the local months a range touches, oldest first.
func (r *Rollup) monthsBetween(from, to time.Time) []model.Period {
	if !from.Before(to) {
		return nil
	}
	var out []model.Period
	cur := from
	seen := map[model.Period]bool{}
	// Bounded: a window measured in days cannot span more than a few years of months.
	for i := 0; cur.Before(to) && i < 512; i++ {
		m := r.cal.Month(cur)
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
		_, next, err := r.cal.Bounds(m)
		if err != nil {
			break
		}
		cur = next
	}
	return out
}

// diffDelta produces the corrections to push up the hierarchy from a corrected leaf row.
//
// A day row rolls into its month, year and all-time; a month row into its year and all-time.
// The day row's month is intentionally NOT corrected here -- month rows are recomputed
// absolutely in the same pass, so adding a delta on top would double-count.
func diffDelta(key model.AggKey, have, want model.Aggregate) []model.AggDelta {
	d := model.AggDelta{
		Plays:         want.Plays - have.Plays,
		PlaysExact:    want.PlaysExact - have.PlaysExact,
		MsPlayed:      want.MsPlayed - have.MsPlayed,
		MsPlayedExact: want.MsPlayedExact - have.MsPlayedExact,
	}
	if d.Plays == 0 && d.PlaysExact == 0 && d.MsPlayed == 0 && d.MsPlayedExact == 0 {
		return nil
	}

	// Only a month row propagates. Day rows exist solely for DimTotal and are already covered
	// by that dimension's month row, which is recomputed absolutely.
	if key.Period.Granularity() != model.GranularityMonth {
		return nil
	}

	year := model.Period(string(key.Period)[:4])
	out := make([]model.AggDelta, 0, 2)
	for _, p := range []model.Period{year, model.PeriodAll} {
		c := d
		c.Key = model.AggKey{Dim: key.Dim, Period: p, EntityID: key.EntityID}
		// Bounds are left alone: a correction cannot narrow a min or max, and the recomputed
		// leaf rows carry accurate ones.
		out = append(out, c)
	}
	return out
}

// aggregatesEqual compares the counters only. The firstPlayedAt and lastPlayedAt bounds are
// best-effort at write time (docs/SPECS.md 5.2), so treating a bound mismatch as drift would
// report a correction on nearly every row and drown the real signal.
func aggregatesEqual(a, b model.Aggregate) bool {
	return a.Plays == b.Plays &&
		a.PlaysExact == b.PlaysExact &&
		a.MsPlayed == b.MsPlayed &&
		a.MsPlayedExact == b.MsPlayedExact
}
