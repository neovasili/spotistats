package main

import (
	"context"
	"fmt"
	"time"

	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/rollup"
)

// DefaultDataDir is where a local rollup writes its snapshots, matching what `serve -data`
// expects by default.
const DefaultDataDir = "./.dev/data"

func runRollup(ctx context.Context, args []string) error {
	fs := newFlagSet("rollup", "rollup [flags]")
	dataDir := fs.String("data", DefaultDataDir, "directory to render snapshots into")
	window := fs.Int("window", 0, "reconcile window in days (default: the built-in 45)")
	all := fs.Bool("all", false,
		"reconcile the ENTIRE history rather than a window; rewrites every aggregate row")
	noRender := fs.Bool("no-render", false, "reconcile and refresh only, writing no snapshots")
	timeout := fs.Duration("timeout", 15*time.Minute, "overall deadline")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := config.Load()
	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	deps, err := config.Build(runCtx, cfg, config.BuildOptions{
		NeedStore:         true,
		VerifyStoreConfig: true,
	})
	if err != nil {
		return err
	}
	cal, err := cfg.Calendar()
	if err != nil {
		return err
	}

	rcfg := rollup.Config{
		Store:      deps.Store,
		Calendar:   cal,
		WindowDays: *window,
		Logger:     deps.Logger,
	}
	var publisher *rollup.DirPublisher
	if !*noRender {
		publisher = rollup.NewDirPublisher(*dataDir)
		rcfg.Publisher = publisher
	}

	r, err := rollup.New(rcfg)
	if err != nil {
		return err
	}

	// --all is separated from the normal run because it rewrites every aggregate row: on a real
	// dataset that is a meaningful write-capacity bill, and it is the repair for drift that
	// originated outside any window rather than routine maintenance.
	if *all {
		heading("Full reconcile — rewriting every aggregate from history")
		res, rerr := r.ReconcileAll(runCtx, time.Time{}, time.Time{})
		bullet("plays read:   %d", res.PlaysRead)
		bullet("rows written: %d", res.RowsCorrected)
		if res.RowsDeleted > 0 {
			// Stale rows are how the same artist ended up listed twice, so say when they go.
			bullet("rows deleted: %d (orphaned: no play supports them any more)",
				res.RowsDeleted)
		}
		if rerr != nil {
			return rerr
		}
		bullet("done — run `%s rollup` to refresh leaderboards and snapshots", progName)
		return nil
	}

	res, err := r.Run(runCtx)

	heading("Rollup")
	bullet("plays read:      %d", res.PlaysRead)
	bullet("rows checked:    %d", res.RowsChecked)
	bullet("rows corrected:  %d", res.RowsCorrected)
	bullet("propagated:      %d year/all-time rows", res.PropagatedRows)
	bullet("leaderboards:    %d", res.LeaderboardsWritten)
	bullet("unresolved names:%d", res.UnresolvedNames)
	bullet("histograms:      %d", res.HistogramsWritten)
	bullet("snapshots:       %d", res.SnapshotsWritten)
	bullet("took:            %s", res.Duration.Round(time.Millisecond))

	if res.UnresolvedNames > 0 {
		fmt.Println()
		bullet("NOTE: %d entity/entities will render as raw IDs because their dimension row is",
			res.UnresolvedNames)
		bullet("      missing or unenriched. For artists that means capture could not reach")
		bullet("      GET /v1/artists, which also costs genre attribution. Run a capture pass")
		bullet("      and then this command again.")
	}

	if res.RowsCorrected > 0 {
		fmt.Println()
		bullet("NOTE: %d aggregate row(s) had drifted and were corrected. That means a capture",
			res.RowsCorrected)
		bullet("      run died between writing a play and applying its aggregates. The system")
		bullet("      self-heals; a persistently non-zero count is worth investigating.")
	}

	if err != nil {
		return err
	}
	if publisher != nil {
		fmt.Printf("\nSnapshots in %s\n  %s serve -data %s\n",
			publisher.Dir(), progName, publisher.Dir())
	}
	return nil
}
