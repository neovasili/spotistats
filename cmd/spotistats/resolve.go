package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/neovasili/spotistats/internal/backfill"
	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/spotify"
)

// runResolve upgrades placeholder track rows to real Spotify identity.
//
// # What this fixes
//
// The imported history supplies no artist or album ID, so the importer writes a placeholder
// track row whose artistIds are NAME KEYS derived from the export's text (docs/SPECS.md 4.2).
// Until a track is resolved, its plays attribute to `nm:metallica` rather than to Metallica --
// and an artist with some tracks resolved and some not appears TWICE, splitting its history
// across two leaderboard rows where one is usually invisible below the top N.
//
// # Why it is a separate, bounded command
//
// It shares the Spotify quota with capture, which is the one job that must not fail: the
// recently-played endpoint returns a rolling window of ~50 plays, so consecutive capture
// failures lose listening permanently, and no reconcile can recover it. Track resolution loses
// nothing by being slow — the plays are already stored, and a later pass upgrades seventeen
// years of aggregates without reimporting anything.
//
// So this is deliberately a manual, budgeted command rather than a background job: the operator
// decides when to spend quota, and `--limit` decides how much. It stops cleanly on a 429 and is
// resumable, so the worst case is a short run.
func runResolve(ctx context.Context, args []string) error {
	fs := newFlagSet("resolve", "resolve [flags]")
	limit := fs.Int("limit", 300, "resolve at most N tracks this run (0 for all)")
	rps := fs.Int("rps", 3, "Spotify requests per second")
	dryRun := fs.Bool("dry-run", false, "report the backlog without spending any API quota")
	timeout := fs.Duration("timeout", 30*time.Minute, "overall deadline")
	if err := fs.Parse(args); err != nil {
		return err
	}

	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	cfg := config.Load()
	deps, err := config.Build(runCtx, cfg, config.BuildOptions{
		NeedStore:         true,
		NeedSpotify:       !*dryRun,
		VerifyStoreConfig: true,
	})
	if err != nil {
		return err
	}

	ids, err := backfill.PlayedTrackIDs(runCtx, deps.Store)
	if err != nil {
		return err
	}

	enricher := backfill.NewEnricher(deps.Store, deps.Spotify, deps.Logger)
	todo, err := enricher.Unresolved(runCtx, ids)
	if err != nil {
		return err
	}

	fmt.Printf("Track identity\n")
	fmt.Printf("  tracks ever played:   %d\n", len(ids))
	fmt.Printf("  still on placeholders:%d\n", len(todo))
	if len(todo) == 0 {
		fmt.Println("\nEvery played track is resolved to a real Spotify ID.")
		return nil
	}

	if *dryRun {
		// The estimate is the honest headline: at roughly 500 requests per rate-limit window
		// this is a multi-week job, and an operator planning around "about an hour" would be
		// badly misled.
		fmt.Printf("\n  At ~500 requests per rate-limit window that is about %d window(s).\n",
			(len(todo)+499)/500)
		fmt.Println("  Nothing was requested: --dry-run spends no quota.")
		return nil
	}

	n := *limit
	if n <= 0 || n > len(todo) {
		n = len(todo)
	}
	fmt.Printf("\nResolving %d of them at %d req/s\n", n, *rps)

	stats, err := enricher.Enrich(runCtx, todo, n, func(done, total int) {
		fmt.Printf("  %d/%d resolved\r", done, total)
	})
	fmt.Println()
	fmt.Printf("  fetched:        %d\n", stats.Fetched)
	fmt.Printf("  tracks written: %d\n", stats.TracksWritten)
	fmt.Printf("  albums written: %d\n", stats.AlbumsWritten)
	fmt.Printf("  artists written:%d\n", stats.ArtistsWritten)

	if err != nil {
		var rl *spotify.RateLimitError
		if errors.As(err, &rl) {
			// Not a failure. Hitting the quota is the expected end of a run, and everything
			// fetched before it is already durable.
			fmt.Printf("\n  Spotify quota reached; it asks for %s.\n", rl.RetryAfter.Round(time.Minute))
			fmt.Printf("  %d track(s) still to go. Re-run after the cooldown.\n",
				len(todo)-stats.Fetched)
			fmt.Println("\n  Run `spotistats rollup -all` once a batch is done: resolution only")
			fmt.Println("  rewrites the track rows, and a full reconcile is what moves the")
			fmt.Println("  seventeen years of artist and album aggregates onto real identity.")
			return nil
		}
		return err
	}

	fmt.Printf("\n  %d track(s) still to go.\n", len(todo)-stats.Fetched)
	fmt.Println("  Run `spotistats rollup -all` to rewrite the aggregates onto real identity.")
	return nil
}
