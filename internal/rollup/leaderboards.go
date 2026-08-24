package rollup

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/spotify"
	"github.com/neovasili/spotistats/internal/store"
)

// leaderboardMetric is what the materialised boards rank by.
//
// Minutes rather than play count: a three-minute pop song and a twenty-minute prog track are not
// equivalent, and "what did I actually spend time on" is the question the dashboard answers.
// The API can still rank by plays, computing it on the fly.
const leaderboardMetric = "ms"

// RefreshLeaderboards materialises the TOP# rows the dashboard and /top read.
//
// Without them /top must read a whole aggregate partition and sort it in the handler, because
// DynamoDB orders by key and the measure is an attribute. Precomputing turns each dashboard
// widget into a single GetItem.
func (r *Rollup) RefreshLeaderboards(ctx context.Context) (written, unresolved int, err error) {
	periods := r.activePeriods()

	for _, dim := range []model.Dim{model.DimTrack, model.DimArtist, model.DimAlbum, model.DimGenre} {
		for _, period := range periods {
			board, missing, berr := r.buildLeaderboard(ctx, dim, period)
			if berr != nil {
				return written, unresolved, berr
			}
			if period == model.PeriodAll {
				// Counted once, from the all-time board, so the figure is "how many entities
				// lack a name" rather than a sum inflated by every period.
				unresolved += missing
			}
			// An empty board is still written: it overwrites a stale one from a period that has
			// since been emptied, and lets /top distinguish "nothing here" from "not computed".
			if perr := r.store.PutLeaderboard(ctx, board); perr != nil {
				return written, unresolved, fmt.Errorf(
					"rollup: write leaderboard %s/%s: %w", dim, period, perr)
			}
			written++
		}
	}
	return written, unresolved, nil
}

func (r *Rollup) buildLeaderboard(
	ctx context.Context, dim model.Dim, period model.Period,
) (store.Leaderboard, int, error) {
	var aggs []model.Aggregate
	for a, err := range r.store.QueryAggregates(ctx, dim, period, "") {
		if err != nil {
			return store.Leaderboard{}, 0, fmt.Errorf("rollup: read %s/%s: %w", dim, period, err)
		}
		if a.Key.Dim != dim {
			continue
		}
		aggs = append(aggs, a)
	}

	// Tie-break on entity ID so the stored order is deterministic: a board that reshuffles
	// between nights would make the dashboard appear to change when nothing did.
	sort.SliceStable(aggs, func(i, j int) bool {
		if aggs[i].MsPlayed != aggs[j].MsPlayed {
			return aggs[i].MsPlayed > aggs[j].MsPlayed
		}
		return aggs[i].Key.EntityID < aggs[j].Key.EntityID
	})
	if len(aggs) > r.topN {
		aggs = aggs[:r.topN]
	}

	shown, unresolved := r.resolveDisplay(ctx, dim, aggs)

	entries := make([]store.LeaderboardEntry, 0, len(aggs))
	for _, a := range aggs {
		id := a.Key.EntityID
		d := shown[id]
		entries = append(entries, store.LeaderboardEntry{
			ID: id, Name: d.Name, Plays: a.Plays, MsPlayed: a.MsPlayed,
			ImageURL: d.ImageURL, ThumbURL: d.ThumbURL,
			ArtistName: d.ArtistName, AlbumName: d.AlbumName,
		})
	}

	return store.Leaderboard{
		Dim: dim, Period: period, Metric: leaderboardMetric,
		Entries:    entries,
		ComputedAt: model.FormatTS(r.now()),
	}, unresolved, nil
}

// resolveDisplay batch-reads names and images for a set of aggregates.
//
// A missing name is not fatal -- the dashboard falls back to showing the raw ID -- but it IS
// reported, because silently rendering `4uLU6hMCjMI75M1A2tKUQC` where a name belongs looks like a
// frontend bug and gives no clue that the real cause is an unenriched dimension row upstream.
// resolveDisplay maps entity IDs to their display information.
//
// The rule itself -- including which artist to show for a collaboration -- lives in
// store.ResolveLabels, shared with the query API so the two cannot drift apart.
func (r *Rollup) resolveDisplay(
	ctx context.Context, dim model.Dim, aggs []model.Aggregate,
) (map[string]store.Label, int) {
	ids := make([]string, 0, len(aggs))
	for _, a := range aggs {
		ids = append(ids, a.Key.EntityID)
	}
	labels, err := r.store.ResolveLabels(ctx, dim, ids)
	if err != nil {
		r.log.ErrorContext(ctx, "rollup: dimension lookup failed; the leaderboard will show "+
			"raw IDs instead of names", "dim", dim, "err", err)
	}

	var unresolved int
	for _, id := range ids {
		if labels[id].Name == "" {
			unresolved++
		}
	}
	if unresolved > 0 {
		r.log.WarnContext(ctx, "rollup: entities have no name and will render as raw IDs. "+
			"Their dimension rows are missing or unenriched -- for artists that means capture "+
			"could not reach GET /v1/artists, which also costs genre attribution.",
			"dim", dim, "unresolved", unresolved, "of", len(ids))
	}
	return labels, unresolved
}

// activePeriods lists the periods worth materialising: all-time, the current and previous year,
// and the trailing few months. Older periods are immutable once reconciled, so recomputing them
// nightly would be waste.
func (r *Rollup) activePeriods() []model.Period {
	now := r.now()
	out := []model.Period{model.PeriodAll}

	year := r.cal.Year(now)
	out = append(out, year)
	if prev := r.cal.Year(now.AddDate(-1, 0, 0)); prev != year {
		out = append(out, prev)
	}

	// Three months back covers a late-arriving backfill without recomputing history.
	seen := map[model.Period]bool{}
	for i := 0; i < 3; i++ {
		m := r.cal.Month(now.AddDate(0, -i, 0))
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// RefreshHistograms recomputes the listening-rhythm histograms AND the exact coverage row, in a
// single pass over the play history.
//
// Merged deliberately: both need every play, and streaming history twice would double the cost
// of the most expensive part of the nightly run.
//
// Buckets are local-time (docs/SPECS.md 5.2): an hour-of-day chart in UTC would show someone in
// Madrid going to bed an hour earlier in winter. That is also why these cannot be derived from
// the aggregate rows -- those record no hour.
func (r *Rollup) RefreshHistograms(ctx context.Context) (int, error) {
	written := 0

	// The all-time pass also produces the coverage row, so it runs first and separately.
	n, coverage, err := r.histogramPass(ctx, model.PeriodAll, true)
	written += n
	if err != nil {
		return written, err
	}
	if err := r.store.PutCoverage(ctx, coverage); err != nil {
		return written, fmt.Errorf("rollup: write coverage: %w", err)
	}
	// Per-artist top items come out of the same pass. Counted separately from `written`, which
	// means histogram rows.
	if r.lastTopItems != nil {
		n, terr := r.writeArtistTopItems(ctx, r.lastTopItems)
		if terr != nil {
			// The figures are already correct in the aggregates; only the per-artist convenience
			// rows are missing, and the next nightly run rewrites them.
			r.log.ErrorContext(ctx, "rollup: could not write artist top items",
				"written", n, "err", terr)
		} else {
			r.log.InfoContext(ctx, "rollup: artist top items written", "artists", n)
		}
		r.lastTopItems = nil
	}

	n, _, err = r.histogramPass(ctx, r.cal.Year(r.now()), false)
	written += n
	return written, err
}

// histogramPass streams one period's plays, filling the hour and weekday histograms and, when
// asked, the coverage figures.
func (r *Rollup) histogramPass(
	ctx context.Context, period model.Period, withCoverage bool,
) (int, model.CoverageRow, error) {
	hour := store.Histogram{
		Period: period, Kind: store.HistogramHour,
		Plays: map[int]int64{}, MsPlayed: map[int]int64{},
	}
	weekday := store.Histogram{
		Period: period, Kind: store.HistogramWeekday,
		Plays: map[int]int64{}, MsPlayed: map[int]int64{},
	}
	cov := model.CoverageRow{ComputedAt: r.now()}

	from, to, err := r.rangeFor(period)
	if err != nil {
		return 0, cov, err
	}

	// Genres are resolved only when computing coverage, and cached: the same artists recur
	// constantly, so an uncached lookup would dominate the pass.
	var genres *genreCache
	var tracks *trackCache
	// Per-artist top albums and tracks ride along on the all-time coverage pass. It already
	// streams every play and resolves each one's identity, which is the expensive half; nothing
	// indexes "the albums of this artist", so this is the one place the question can be answered
	// without a second full pass.
	var topAcc *artistTopAccumulator
	if withCoverage {
		genres = newGenreCache(r.store)
		tracks = newTrackCache(r.store)
		topAcc = newArtistTopAccumulator()
	}

	// Coverage needs each play's attribution, which needs its track row. Resolving those one
	// at a time costs a round trip per distinct track -- thirteen thousand of them over full
	// history -- so the plays are buffered here and the tracks batch-loaded once below.
	// A play is a few dozen bytes; four hundred thousand of them are a few tens of megabytes.
	var buffered []model.Play
	var trackIDs []string
	seenTrack := map[string]bool{}

	for p, perr := range r.store.Plays(ctx, from, to, store.PlayFilter{}) {
		if perr != nil {
			return 0, cov, fmt.Errorf("rollup: read plays for %s: %w", period, perr)
		}
		h := r.cal.HourOfDay(p.PlayedAt)
		w := int(r.cal.Weekday(p.PlayedAt))
		hour.Plays[h]++
		hour.MsPlayed[h] += p.MsPlayed
		weekday.Plays[w]++
		weekday.MsPlayed[w] += p.MsPlayed

		if !withCoverage {
			continue
		}
		buffered = append(buffered, p)
		if p.TrackID != "" && !seenTrack[p.TrackID] {
			seenTrack[p.TrackID] = true
			trackIDs = append(trackIDs, p.TrackID)
		}
	}

	var canon *canonicaliser
	if withCoverage {
		if err := tracks.Warm(ctx, trackIDs); err != nil {
			r.log.WarnContext(ctx, "rollup: batch track prefetch failed; falling back to "+
				"per-track lookups", "err", err)
		}
		var cerr error
		canon, cerr = newCanonicaliser(ctx, r.store, tracks.Loaded())
		if cerr != nil {
			return 0, cov, fmt.Errorf("rollup: build canonical ID index: %w", cerr)
		}
	}
	for _, p := range buffered {

		// Exact bounds, which the aggregate attributes cannot provide: those are set by write
		// order, not play order.
		if cov.FirstPlayedAt.IsZero() || p.PlayedAt.Before(cov.FirstPlayedAt) {
			cov.FirstPlayedAt = p.PlayedAt
		}
		if p.PlayedAt.After(cov.LastPlayedAt) {
			cov.LastPlayedAt = p.PlayedAt
		}
		cov.TotalPlays++
		cov.TotalMs += p.MsPlayed

		// Attribution is resolved by exactly the rule the reconcile uses. If these diverged,
		// the coverage figure would describe a different dataset from the one the leaderboards
		// were built from -- and this number is what the UI trusts to decide whether to warn
		// that the rankings are unreliable.
		track, terr := tracks.For(ctx, p.TrackID)
		if terr != nil {
			r.log.WarnContext(ctx, "rollup: track lookup failed during the coverage pass",
				"trackId", p.TrackID, "err", terr)
		}
		facts := canon.Apply(model.FactsForTrack(p, track, nil))

		// Genre coverage, counted PER PLAY. Summing the genre aggregates cannot give this: a
		// play with three genres contributes to three rows, so the sum overstates coverage and
		// capping it at the total silently reports 100%.
		g, gerr := genres.For(ctx, facts.ArtistIDs)
		if gerr != nil {
			r.log.WarnContext(ctx, "rollup: genre lookup failed during the coverage pass",
				"trackId", p.TrackID, "err", gerr)
		}
		if len(model.FactsFor(p, g).Genres) > 0 {
			cov.PlaysWithGenre++
			cov.MsWithGenre += p.MsPlayed
		}

		// Artist attribution, also per play -- and counted only where at least one artist is a
		// REAL Spotify ID.
		//
		// A name key is attribution of a sort, and counting it as such is why this figure read
		// 1.0 while 39% of listening time sat on name-keyed rows and the artist rankings were
		// split. Every surface agreed the data was fine: coverage said 100%, the name-resolution
		// check said every leaderboard entry was named -- a name-keyed artist has a perfectly
		// good name, the name IS its identity -- and the canonicaliser quietly merged whichever
		// names some other track happened to supply a mapping for.
		//
		// What the dashboard needs to know is not "does this play name an artist" but "can this
		// play be RANKED against the others", and only a resolved ID answers that: two name keys
		// that are really one artist split its history, and no downstream check can see it.
		if hasResolvedArtist(facts.ArtistIDs) {
			cov.PlaysWithArtist++
			cov.MsWithArtist += p.MsPlayed
		}

		// The export's names are the fallback for a track resolved only partway through the
		// history: its later plays carry a real row, its earlier ones only this text.
		topAcc.Add(facts, firstNonEmpty(track.Name, p.Export.TrackName), p.Export.AlbumName)
	}

	// Hand the accumulator to the caller, which owns writing it: histogramPass is called for
	// several periods and only the all-time one carries coverage, so only that one has it.
	if topAcc != nil {
		r.lastTopItems = topAcc
	}

	// Dense buckets: a bar chart missing hour 3 entirely reads as "no data" rather than "no
	// listening", and the client should not have to reconstruct the gaps.
	for i := 0; i < 24; i++ {
		if _, ok := hour.Plays[i]; !ok {
			hour.Plays[i] = 0
			hour.MsPlayed[i] = 0
		}
	}
	for i := 0; i < 7; i++ {
		if _, ok := weekday.Plays[i]; !ok {
			weekday.Plays[i] = 0
			weekday.MsPlayed[i] = 0
		}
	}

	if err := r.store.PutHistogram(ctx, hour); err != nil {
		return 0, cov, err
	}
	if err := r.store.PutHistogram(ctx, weekday); err != nil {
		return 1, cov, err
	}
	return 2, cov, nil
}

// rangeFor turns a period into a scan range, treating all-time as "everything".
func (r *Rollup) rangeFor(period model.Period) (from, to time.Time, err error) {
	if period == model.PeriodAll {
		return time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC), r.now().Add(24 * time.Hour), nil
	}
	return r.cal.Bounds(period)
}

// refreshSpotifyTopItems stores Spotify's own rankings alongside the computed ones.
//
// They will disagree: Spotify's weighting is undocumented and its windows are approximate. Both
// are kept, and the UI labels which is which, because "Spotify says" and "your data says" are
// different claims and conflating them would be dishonest.
func (r *Rollup) refreshSpotifyTopItems(ctx context.Context) error {
	for _, tr := range spotify.AllTimeRanges() {
		artists, err := r.spotify.TopArtists(ctx, tr, spotify.MaxTopItemsLimit, 0)
		if err != nil {
			return fmt.Errorf("top artists (%s): %w", tr, err)
		}
		entries := make([]store.LeaderboardEntry, 0, len(artists))
		for _, a := range artists {
			entries = append(entries, store.LeaderboardEntry{
				ID: a.ID, Name: a.Name, ImageURL: a.ImageURL,
			})
		}
		if err := r.store.PutLeaderboard(ctx, store.Leaderboard{
			Dim: model.DimArtist, Period: spotifyPeriod(tr),
			Metric: "spotify", Entries: entries, ComputedAt: model.FormatTS(r.now()),
		}); err != nil {
			return err
		}

		tracks, err := r.spotify.TopTracks(ctx, tr, spotify.MaxTopItemsLimit, 0)
		if err != nil {
			return fmt.Errorf("top tracks (%s): %w", tr, err)
		}
		trackEntries := make([]store.LeaderboardEntry, 0, len(tracks))
		for _, t := range tracks {
			trackEntries = append(trackEntries, store.LeaderboardEntry{ID: t.ID, Name: t.Name})
		}
		if err := r.store.PutLeaderboard(ctx, store.Leaderboard{
			Dim: model.DimTrack, Period: spotifyPeriod(tr),
			Metric: "spotify", Entries: trackEntries, ComputedAt: model.FormatTS(r.now()),
		}); err != nil {
			return err
		}
	}
	return nil
}

// spotifyPeriod encodes a Spotify time range as a period key.
//
// Deliberately NOT one of the calendar periods: Spotify's windows are rolling and approximate,
// so filing them under "2026" would imply they mean the calendar year, which they do not.
func spotifyPeriod(tr spotify.TimeRange) model.Period {
	return model.Period("SPOTIFY-" + string(tr))
}

// hasResolvedArtist reports whether any of the given artist IDs is a real Spotify ID rather
// than a name-derived placeholder.
//
// "Any" rather than "all" deliberately: a collaboration where one artist resolved and another
// did not is still rankable for the one that did, and treating the whole play as unattributed
// would understate a resolved artist to protect an unresolved one.
func hasResolvedArtist(ids []string) bool {
	for _, id := range ids {
		if id != "" && !model.IsNameKey(id) {
			return true
		}
	}
	return false
}
