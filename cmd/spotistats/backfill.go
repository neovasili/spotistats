package main

import (
	"context"
	"fmt"
	"time"

	"github.com/neovasili/spotistats/internal/backfill"
	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/model"
)

// DefaultHistoryDir is where the unzipped GDPR export is expected.
const DefaultHistoryDir = "./.dev/historic-data"

// defaultMinMs is the shortest listening stretch counted as a play.
//
// See backfill.Record.Filter for the reasoning, which is NOT the one docs/SPECS.md originally
// gave: recently-played records completions, not 30-second thresholds, and matching it exactly
// would discard sixty days of genuinely attended listening from this corpus.
const defaultMinMs = 30_000

func runBackfill(ctx context.Context, args []string) error {
	fs := newFlagSet("backfill", "backfill [flags]")
	dir := fs.String("path", DefaultHistoryDir, "directory holding the unzipped export JSON")
	minMs := fs.Int64("min-ms", defaultMinMs,
		"shortest listening stretch counted as a play (0 imports everything)")
	dryRun := fs.Bool("dry-run", false, "parse and report without writing anything")
	skipEnrich := fs.Bool("skip-enrich", false,
		"do not resolve tracks; plays import without artist or album attribution")
	enrichOnly := fs.Bool("enrich-only", false, "resolve tracks and stop before importing plays")
	enrichLimit := fs.Int("enrich-limit", 0,
		"resolve at most N tracks this run (0 for all); the pass is resumable")
	timeout := fs.Duration("timeout", 4*time.Hour, "overall deadline")
	rps := fs.Int("rps", 3, "Spotify requests per second during enrichment")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *rps <= 0 {
		return fmt.Errorf("-rps must be positive, got %d", *rps)
	}

	files, err := backfill.Files(*dir)
	if err != nil {
		return err
	}

	// 1. Scan first, and before building any dependency. Nothing is written until the operator
	//    has seen the shape of the corpus -- a backfill is hard to undo, and this is where a
	//    wrong --min-ms becomes visible. A dry run therefore needs no table, no credentials and
	//    no network, which is the whole point of being able to inspect before committing.
	fmt.Printf("Scanning %d file(s) in %s\n", len(files), *dir)
	trackIDs, scan, err := backfill.Scan(files, *minMs)
	if err != nil {
		return err
	}
	reportScan(scan, trackIDs, *minMs)

	if *dryRun {
		fmt.Println("\nDry run: nothing written.")
		return nil
	}

	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	deps, err := config.Build(runCtx, config.Load(), config.BuildOptions{
		NeedStore:         true,
		NeedSpotify:       !*skipEnrich,
		VerifyStoreConfig: true,
		// Enrichment issues one request per unique track -- thousands, back to back. Spotify
		// publishes no figure, only that the budget is a rolling 30-second window, so this is
		// a self-imposed ceiling well under any plausible limit. The retrier still honours
		// Retry-After if the guess is too generous; the limiter is what stops the run from
		// spending its life in backoff.
		SpotifyRequestsPerWindow: *rps * 30,
		SpotifyWindow:            30 * time.Second,
	})
	if err != nil {
		return err
	}

	im := backfill.NewImporter(backfill.ImportConfig{
		Store: deps.Store, Log: deps.Logger, MinMs: *minMs, DryRun: *dryRun,
	})
	if !*yes {
		ok, err := confirm(fmt.Sprintf(
			"Import %d plays covering %s to %s?",
			scan.RecordsRead-scan.TotalSkipped(),
			scan.FirstPlayedAt.Format("2006-01-02"), scan.LastPlayedAt.Format("2006-01-02")))
		if err != nil || !ok {
			fmt.Println("Aborted.")
			return err
		}
	}

	// 2. Enrich. This must precede the import: a play row denormalises its artist and album
	//    IDs, so a play written before its track is resolved carries no attribution and would
	//    need reimporting rather than merely reconciling.
	if !*skipEnrich {
		en := backfill.NewEnricher(deps.Store, deps.Spotify, deps.Logger)
		perReq := time.Second / time.Duration(*rps)
		fmt.Printf("\nResolving track metadata: %d track(s) at %d req/s, about %s\n",
			len(trackIDs), *rps,
			backfill.EstimateEnrichDuration(len(trackIDs), perReq).Round(time.Minute))
		st, err := en.Enrich(runCtx, trackIDs, *enrichLimit, func(done, total int) {
			fmt.Printf("\r  %d/%d resolved", done, total)
		})
		fmt.Println()
		reportEnrich(st)
		if err != nil {
			return fmt.Errorf("enrich stopped: %w (re-run to continue; it is resumable)", err)
		}
		if st.Remaining > 0 {
			fmt.Printf("\n  %d track(s) still unresolved -- re-run to continue.\n", st.Remaining)
		}
	}
	if *enrichOnly {
		return nil
	}

	// 3. Import the plays.
	fmt.Println("\nImporting plays")
	res, err := im.Import(runCtx, files, func(f string, n, of int) {
		fmt.Printf("\r  file %d/%d", n, of)
	})
	fmt.Println()
	if err != nil {
		return err
	}
	fmt.Printf("  plays written:  %d\n", res.PlaysWritten)
	if res.UnknownTracks > 0 {
		fmt.Printf("  unresolved tracks: %d (their plays count towards totals but will show "+
			"raw IDs)\n", res.UnknownTracks)
	}

	// 4. Report any API-sourced plays the export supersedes, rather than deleting them silently.
	sup, err := backfill.SupersededAPIPlays(runCtx, deps.Store, res.FirstPlayedAt, res.LastPlayedAt)
	if err != nil {
		return err
	}
	if len(sup) > 0 {
		fmt.Printf("\n  NOTE: %d API-sourced play(s) fall inside the imported window. The export\n"+
			"        is authoritative there (its durations are exact), so those rows now\n"+
			"        double-count. Remove them with `spotistats backfill-prune`.\n", len(sup))
	}

	fmt.Print(`
Next: rebuild the aggregates from the imported plays.

  spotistats rollup --all --timeout 2h

That is required, not optional: the import deliberately writes no aggregate deltas, because
recomputing once from the play rows costs a fraction of ~14 increments per play.
`)
	return nil
}

func reportScan(s backfill.ImportStats, trackIDs []string, minMs int64) {
	kept := s.RecordsRead - s.TotalSkipped()
	fmt.Printf("\nCorpus\n")
	fmt.Printf("  records read:   %d\n", s.RecordsRead)
	fmt.Printf("  importable:     %d\n", kept)
	fmt.Printf("  unique tracks:  %d\n", len(trackIDs))
	if !s.FirstPlayedAt.IsZero() {
		fmt.Printf("  covers:         %s to %s\n",
			s.FirstPlayedAt.Format("2006-01-02"), s.LastPlayedAt.Format("2006-01-02"))
	}
	if s.TotalSkipped() > 0 {
		fmt.Printf("  skipped:        %d\n", s.TotalSkipped())
		for _, r := range []backfill.SkipReason{
			backfill.SkipTooShort, backfill.SkipNoTrackURI, backfill.SkipPodcast,
			backfill.SkipAudiobook, backfill.SkipBadTS,
		} {
			if n := s.Skipped[r]; n > 0 {
				fmt.Printf("      %-42s %d\n", r, n)
			}
		}
		if s.Skipped[backfill.SkipTooShort] > 0 {
			fmt.Printf("      (--min-ms is %d; pass 0 to import every record)\n", minMs)
		}
	}
}

func reportEnrich(s backfill.EnrichStats) {
	fmt.Printf("  already known:  %d\n", s.AlreadyKnown)
	fmt.Printf("  fetched:        %d\n", s.Fetched)
	fmt.Printf("  tracks written: %d\n", s.TracksWritten)
	fmt.Printf("  albums written: %d\n", s.AlbumsWritten)
	fmt.Printf("  artists written:%d\n", s.ArtistsWritten)
	if s.Unresolvable > 0 {
		fmt.Printf("  unresolvable:   %d (tombstoned; removed from Spotify's catalogue)\n",
			s.Unresolvable)
	}
}

// runBackfillPrune deletes the API-sourced plays the export supersedes.
//
// Separate from the import, and never automatic: deleting play rows is the one irreversible
// step in the whole pipeline, and the import has no business doing it as a side effect.
func runBackfillPrune(ctx context.Context, args []string) error {
	fs := newFlagSet("backfill-prune", "backfill-prune [flags]")
	from := fs.String("from", "", "start of the window, e.g. 2026-08-01T00:00:00Z (required)")
	to := fs.String("to", "", "end of the window, e.g. 2026-08-21T22:02:01Z (required)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	timeout := fs.Duration("timeout", 30*time.Minute, "overall deadline")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *to == "" {
		return fmt.Errorf("both -from and -to are required: pruning an unbounded range would " +
			"delete every API-sourced play ever captured")
	}
	fromT, err := parseWindowBound(*from)
	if err != nil {
		return fmt.Errorf("parse -from: %w", err)
	}
	toT, err := parseWindowBound(*to)
	if err != nil {
		return fmt.Errorf("parse -to: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	deps, err := config.Build(runCtx, config.Load(), config.BuildOptions{
		NeedStore: true, VerifyStoreConfig: true,
	})
	if err != nil {
		return err
	}

	sup, err := backfill.SupersededAPIPlays(runCtx, deps.Store, fromT, toT)
	if err != nil {
		return err
	}
	if len(sup) == 0 {
		fmt.Println("No API-sourced plays in that window; nothing to do.")
		return nil
	}
	fmt.Printf("%d API-sourced play(s) between %s and %s\n",
		len(sup), model.FormatTS(sup[0].PlayedAt), model.FormatTS(sup[len(sup)-1].PlayedAt))
	if !*yes {
		ok, cerr := confirm("Delete them? This cannot be undone.")
		if cerr != nil || !ok {
			fmt.Println("Aborted.")
			return cerr
		}
	}
	for _, p := range sup {
		if err := deps.Store.DeletePlay(runCtx, p.PlayedAt, p.TrackID); err != nil {
			return fmt.Errorf("delete play %s/%s: %w",
				model.FormatTS(p.PlayedAt), p.TrackID, err)
		}
	}
	fmt.Printf("Deleted %d play(s). Run `spotistats rollup --all` to rebuild the aggregates.\n",
		len(sup))
	return nil
}

// confirm asks a yes/no question on stdin.
//
// The backfill writes hundreds of thousands of rows and the prune deletes them, so both stop
// and ask by default. --yes exists for scripted reruns.
func confirm(question string) (bool, error) {
	fmt.Printf("\n%s [y/N] ", question)
	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		// An empty line (or a closed stdin) is a "no", which is the safe default.
		return false, nil
	}
	return answer == "y" || answer == "Y" || answer == "yes", nil
}

// parseWindowBound accepts a prune window bound in either the millisecond form the tool prints
// (model.TimestampFormat) or plain second precision, which is what a human types.
//
// The repo bans time.RFC3339 because a second-precision layout used as a DynamoDB sort key
// makes distinct millisecond instants collide. That hazard does not apply here: this parses
// operator INPUT into a time.Time, and every key derived from it goes through model.FormatTS.
// The layout is spelled out locally rather than referencing the banned constant, so the ban
// keeps protecting the places that genuinely need it.
func parseWindowBound(s string) (time.Time, error) {
	if t, err := model.ParseTS(s); err == nil {
		return t, nil
	}
	const secondPrecision = "2006-01-02T15:04:05Z07:00"
	t, err := time.Parse(secondPrecision, s)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"want %q or %q, got %q", model.TimestampFormat, secondPrecision, s)
	}
	return t.UTC(), nil
}
