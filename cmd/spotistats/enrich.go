package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/spotify"
)

// runEnrich backfills artist names and genres for artists already in the aggregates.
//
// Capture only ever sees the artists on the page it just fetched, so an artist that stopped
// being played before its metadata was written is never revisited: its row stays absent and
// the dashboard renders its raw Spotify ID forever. That is not a hypothetical -- it is what
// 28 of 31 artists looked like after GET /v1/artists began returning 403.
//
// The artist set comes from the AGG#ARTIST#ALL partition, which is every artist ever played,
// so this is complete rather than limited to whatever the leaderboards happen to show.
func runEnrich(ctx context.Context, args []string) error {
	fs := newFlagSet("enrich", "enrich [flags]")
	limit := fs.Int("limit", 200, "maximum artists to fetch in this run (0 for no limit)")
	force := fs.Bool("force", false,
		"re-fetch artists that already look enriched, refreshing stale genres")
	timeout := fs.Duration("timeout", 15*time.Minute, "overall deadline")
	if err := fs.Parse(args); err != nil {
		return err
	}

	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	deps, err := config.Build(runCtx, config.Load(), config.BuildOptions{
		NeedStore:         true,
		NeedSpotify:       true,
		VerifyStoreConfig: true,
	})
	if err != nil {
		return err
	}
	st, api := deps.Store, deps.Spotify

	// Every artist that has ever been played.
	var ids []string
	for agg, err := range st.QueryAggregates(runCtx, model.DimArtist, model.PeriodAll, "") {
		if err != nil {
			return fmt.Errorf("enrich: list artists: %w", err)
		}
		if agg.Key.EntityID != "" {
			ids = append(ids, agg.Key.EntityID)
		}
	}
	if len(ids) == 0 {
		fmt.Println("No artists in the aggregates yet; run a capture pass first.")
		return nil
	}

	existing, err := st.GetArtists(runCtx, ids)
	if err != nil {
		return fmt.Errorf("enrich: read artist rows: %w", err)
	}

	var todo []string
	var tombstoned int
	for _, id := range ids {
		a, ok := existing[id]
		switch {
		case !ok, a.EnrichedAt.IsZero():
			todo = append(todo, id)
		case a.Missing:
			// Respect a fresh tombstone; it exists to stop us re-asking about dead IDs.
			if st.TombstoneExpired(a.RefreshedAt) {
				todo = append(todo, id)
			} else {
				tombstoned++
			}
		case *force, st.IsStale(a.EnrichedAt):
			todo = append(todo, id)
		}
	}

	fmt.Printf("Artists\n  known:        %d\n  need work:    %d\n", len(ids), len(todo))
	if tombstoned > 0 {
		fmt.Printf("  tombstoned:   %d (skipped; retried once the tombstone expires)\n", tombstoned)
	}
	if len(todo) == 0 {
		fmt.Println("\nNothing to do.")
		return nil
	}

	// Cap the run rather than the work: one request per artist since Spotify removed the
	// batch endpoint, so a large backfill can outrun the rate limit. Report what was deferred
	// instead of silently truncating -- a partial run that looks complete is worse than a
	// partial run that says so.
	deferred := 0
	if *limit > 0 && len(todo) > *limit {
		deferred = len(todo) - *limit
		todo = todo[:*limit]
	}

	var named, enriched, missing int
	for i, id := range todo {
		a, found, err := api.Artist(runCtx, id)
		if err != nil {
			// Stop, but keep what already landed: every successful write is durable, so the
			// next run resumes from here rather than starting over.
			fmt.Printf("\nStopped after %d of %d: %v\n", i, len(todo), err)
			reportEnrich(named, enriched, missing, deferred+len(todo)-i)
			if isForbidden(err) {
				fmt.Print(forbiddenNote)
			}
			return err
		}
		if !found {
			if err := st.PutMissing(runCtx, model.DimArtist, id); err != nil {
				return fmt.Errorf("enrich: tombstone %s: %w", id, err)
			}
			missing++
			continue
		}
		if err := st.PutArtist(runCtx, a); err != nil {
			return fmt.Errorf("enrich: write artist %s: %w", id, err)
		}
		enriched++
		if len(a.Genres) > 0 {
			named++
		}
	}

	reportEnrich(named, enriched, missing, deferred)
	fmt.Println("\nRun `spotistats rollup` next so the leaderboards pick up the names.")
	return nil
}

func reportEnrich(withGenres, enriched, missing, deferred int) {
	fmt.Printf("\n  written:      %d\n", enriched)
	fmt.Printf("  with genres:  %d (Spotify leaves most artists unclassified)\n", withGenres)
	if missing > 0 {
		fmt.Printf("  unresolvable: %d (tombstoned)\n", missing)
	}
	if deferred > 0 {
		fmt.Printf("  DEFERRED:     %d not attempted this run -- re-run to continue\n", deferred)
	}
}

func isForbidden(err error) bool {
	var apiErr *spotify.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 403
}

const forbiddenNote = `
NOTE: 403 means this app cannot read the endpoint at all. Spotify's February 2026
      Web API change restricted Development Mode apps: batch multi-gets were removed
      outright, and each app is limited to five allowlisted users. Check that the
      Spotify account is allowlisted in the app's dashboard settings.
`
