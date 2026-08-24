package main

import (
	"context"
	"fmt"
	"time"

	"github.com/neovasili/spotistats/internal/backfill"
	"github.com/neovasili/spotistats/internal/config"
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
	limit := fs.Int("limit", backfill.DefaultResolveLimit,
		"resolve at most N tracks this run (-1 for every remaining track)")
	force := fs.Bool("force", false, "ignore an active cooldown (only if the quota recovered early)")
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

	resolver := backfill.NewResolver(deps.Store, deps.Spotify, deps.Logger, nil)

	backlog, err := resolver.Backlog(runCtx)
	if err != nil {
		return err
	}
	fmt.Printf("Track identity\n")
	fmt.Printf("  still on placeholders: %d\n", backlog)
	if backlog == 0 {
		fmt.Println("\nEvery played track is resolved to a real Spotify ID.")
		return nil
	}
	if *dryRun {
		// The estimate is the honest headline: at roughly 500 requests per rate-limit window
		// this is a multi-week job, and an operator planning around "about an hour" would be
		// badly misled.
		fmt.Printf("\n  About %d rate-limit window(s) of work (~500 requests each).\n",
			(backlog+499)/500)
		fmt.Printf("  Unattended at %d/day that is roughly %d day(s).\n",
			backfill.DefaultResolveLimit, (backlog+backfill.DefaultResolveLimit-1)/backfill.DefaultResolveLimit)
		fmt.Println("  Nothing was requested: --dry-run spends no quota.")
		return nil
	}

	res, err := resolver.Run(runCtx, backfill.ResolveOptions{
		Limit: *limit,
		Force: *force,
		Progress: func(done, total int) {
			fmt.Printf("  %d/%d resolved\r", done, total)
		},
	})
	if err != nil {
		return err
	}
	fmt.Println()

	if res.Skipped {
		fmt.Printf("  Cooling down until %s -- Spotify's quota is spent and capture needs it.\n",
			res.SuspendedUntil.Local().Format("15:04 on 2 Jan"))
		fmt.Println("  Pass -force only if you know the quota has recovered early.")
		return nil
	}

	fmt.Printf("  fetched:        %d\n", res.Fetched)
	fmt.Printf("  tracks written: %d\n", res.TracksWritten)
	fmt.Printf("  albums written: %d\n", res.AlbumsWritten)
	fmt.Printf("  artists written:%d\n", res.ArtistsWritten)
	fmt.Printf("  remaining:      %d\n", res.Remaining)

	if !res.SuspendedUntil.IsZero() {
		fmt.Printf("\n  Quota spent; suspended until %s.\n",
			res.SuspendedUntil.Local().Format("15:04 on 2 Jan"))
	}
	if res.Fetched > 0 {
		fmt.Println("\n  Resolution only rewrites track rows. Run `spotistats rollup -all` to move")
		fmt.Println("  the seventeen years of artist and album aggregates onto real identity.")
	}
	return nil
}
