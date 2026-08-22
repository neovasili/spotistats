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
func (r *Rollup) RefreshLeaderboards(ctx context.Context) (int, error) {
	periods := r.activePeriods()
	written := 0

	for _, dim := range []model.Dim{model.DimTrack, model.DimArtist, model.DimAlbum, model.DimGenre} {
		for _, period := range periods {
			board, err := r.buildLeaderboard(ctx, dim, period)
			if err != nil {
				return written, err
			}
			// An empty board is still written: it overwrites a stale one from a period that has
			// since been emptied, and lets /top distinguish "nothing here" from "not computed".
			if err := r.store.PutLeaderboard(ctx, board); err != nil {
				return written, fmt.Errorf("rollup: write leaderboard %s/%s: %w", dim, period, err)
			}
			written++
		}
	}
	return written, nil
}

func (r *Rollup) buildLeaderboard(
	ctx context.Context, dim model.Dim, period model.Period,
) (store.Leaderboard, error) {
	var aggs []model.Aggregate
	for a, err := range r.store.QueryAggregates(ctx, dim, period, "") {
		if err != nil {
			return store.Leaderboard{}, fmt.Errorf("rollup: read %s/%s: %w", dim, period, err)
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

	names, images := r.resolveDisplay(ctx, dim, aggs)

	entries := make([]store.LeaderboardEntry, 0, len(aggs))
	for _, a := range aggs {
		id := a.Key.EntityID
		entries = append(entries, store.LeaderboardEntry{
			ID: id, Name: names[id], Plays: a.Plays, MsPlayed: a.MsPlayed, ImageURL: images[id],
		})
	}

	return store.Leaderboard{
		Dim: dim, Period: period, Metric: leaderboardMetric,
		Entries:    entries,
		ComputedAt: model.FormatTS(r.now()),
	}, nil
}

// resolveDisplay batch-reads names and images for a set of aggregates.
func (r *Rollup) resolveDisplay(
	ctx context.Context, dim model.Dim, aggs []model.Aggregate,
) (names, images map[string]string) {
	names = make(map[string]string, len(aggs))
	images = make(map[string]string, len(aggs))

	// A genre aggregate is keyed by the genre string, so it is its own name and has no image.
	if dim == model.DimGenre {
		for _, a := range aggs {
			names[a.Key.EntityID] = a.Key.EntityID
		}
		return names, images
	}

	ids := make([]string, 0, len(aggs))
	for _, a := range aggs {
		ids = append(ids, a.Key.EntityID)
	}

	switch dim {
	case model.DimTrack:
		tracks, err := r.store.GetTracks(ctx, ids)
		if err != nil {
			return names, images
		}
		// A track's artwork is its album's, so collect the albums in one further batch rather
		// than one lookup per track.
		albumIDs := make([]string, 0, len(tracks))
		for _, t := range tracks {
			names[t.ID] = t.Name
			if t.AlbumID != "" {
				albumIDs = append(albumIDs, t.AlbumID)
			}
		}
		albums, err := r.store.GetAlbums(ctx, albumIDs)
		if err == nil {
			for _, t := range tracks {
				if al, ok := albums[t.AlbumID]; ok {
					images[t.ID] = al.ImageURL
				}
			}
		}
	case model.DimArtist:
		artists, err := r.store.GetArtists(ctx, ids)
		if err != nil {
			return names, images
		}
		for _, a := range artists {
			names[a.ID] = a.Name
			images[a.ID] = a.ImageURL
		}
	case model.DimAlbum:
		albums, err := r.store.GetAlbums(ctx, ids)
		if err != nil {
			return names, images
		}
		for _, a := range albums {
			names[a.ID] = a.Name
			images[a.ID] = a.ImageURL
		}
	}
	return names, images
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
	if withCoverage {
		genres = newGenreCache(r.store)
	}

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

		// Genre coverage, counted PER PLAY. Summing the genre aggregates cannot give this: a
		// play with three genres contributes to three rows, so the sum overstates coverage and
		// capping it at the total silently reports 100%.
		g, gerr := genres.For(ctx, p.ArtistIDs)
		if gerr != nil {
			r.log.WarnContext(ctx, "rollup: genre lookup failed during the coverage pass",
				"trackId", p.TrackID, "err", gerr)
		}
		if len(model.FactsFor(p, g).Genres) > 0 {
			cov.PlaysWithGenre++
			cov.MsWithGenre += p.MsPlayed
		}
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
