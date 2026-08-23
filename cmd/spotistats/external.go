package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/enrich"
)

// runEnrichExternal resolves MusicBrainz and TheAudioDB facts for played artists.
//
// Separate from `enrich` (which fills Spotify metadata) because the two answer to different
// rate limits and different staleness windows, and sharing a command would mean sharing a
// cursor between jobs that walk the same list at very different speeds.
func runEnrichExternal(ctx context.Context, args []string) error {
	fs := newFlagSet("enrich-external", "enrich-external [flags]")
	limit := fs.Int("limit", 0, "enrich at most N artists (0 for all); the pass is resumable")
	force := fs.Bool("force", false, "re-enrich artists whose profile is still fresh")
	artist := fs.String("artist", "", "enrich exactly one Spotify artist ID, ignoring staleness")
	timeout := fs.Duration("timeout", 30*time.Minute, "overall deadline")
	if err := fs.Parse(args); err != nil {
		return err
	}

	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	cfg := config.Load()
	deps, err := config.Build(runCtx, cfg, config.BuildOptions{
		NeedStore:         true,
		NeedExternal:      true,
		VerifyStoreConfig: true,
	})
	if err != nil {
		return err
	}

	e, err := enrich.New(enrich.Config{
		Store:       deps.Store,
		MusicBrainz: deps.MusicBrainz,
		AudioDB:     deps.AudioDB,
		Language:    cfg.BiographyLanguage,
		Logger:      deps.Logger,
	})
	if err != nil {
		return err
	}

	if deps.AudioDB == nil {
		fmt.Println("NOTE: no TheAudioDB key configured. Structured facts will be stored;")
		fmt.Println("      biography and artwork will not.")
	}
	fmt.Println("Enriching artists from MusicBrainz" +
		map[bool]string{true: " and TheAudioDB", false: ""}[deps.AudioDB != nil])

	res, err := e.Run(runCtx, enrich.Options{
		Limit: *limit, Force: *force, ArtistID: *artist,
	})
	if errors.Is(err, enrich.ErrLockHeld) {
		// Two overlapping runs would double the real request rate against two per-IP limits.
		fmt.Println("\nAnother enrichment run is in progress. Nothing to do.")
		return nil
	}
	reportExternal(res)
	return err
}

func reportExternal(res enrich.Result) {
	fmt.Printf("\nExternal enrichment\n")
	bullet("candidates:   %d", res.Candidates)
	if res.Skipped > 0 {
		bullet("skipped:      %d (profile still fresh)", res.Skipped)
	}
	bullet("resolved:     %d", res.Resolved)
	bullet("unresolved:   %d (%.0f%% of attempts)", res.Unresolved, res.UnresolvedRatio()*100)
	bullet("facts stored: %d", res.FactsWritten)
	bullet("prose stored: %d", res.ProseWritten)
	for source, n := range res.SourceErrors {
		bullet("%s errors: %d (logged and skipped)", source, n)
	}
	if res.Remaining > 0 {
		bullet("remaining:    %d not attempted -- re-run to continue", res.Remaining)
	}
	bullet("took:         %s", res.Duration.Round(time.Millisecond))

	if res.Unresolved > 0 {
		fmt.Print(`
  NOTE: an unresolved artist is one MusicBrainz has no Spotify link for. There is
        deliberately no name-search fallback: a fuzzy match attaches the wrong
        biography and members to a real band, and nothing downstream can detect
        it. Fix one by hand with:

          spotistats mbid set <spotifyArtistId> <musicBrainzId>
`)
	}
}

// runMBID manages the manual MBID overrides.
func runMBID(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: mbid <set|clear> ...")
	}
	sub, rest := args[0], args[1:]

	runCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	deps, err := config.Build(runCtx, config.Load(), config.BuildOptions{
		NeedStore: true, VerifyStoreConfig: true,
	})
	if err != nil {
		return err
	}

	switch sub {
	case "set":
		if len(rest) != 2 {
			return fmt.Errorf("usage: mbid set <spotifyArtistId> <musicBrainzId>")
		}
		if err := deps.Store.PutMBIDOverride(runCtx, rest[0], rest[1]); err != nil {
			return err
		}
		fmt.Printf("Override recorded: %s -> %s\n", rest[0], rest[1])
		fmt.Println("Run `spotistats enrich-external -artist " + rest[0] + "` to apply it.")
		return nil

	case "clear":
		if len(rest) != 1 {
			return fmt.Errorf("usage: mbid clear <spotifyArtistId>")
		}
		if err := deps.Store.DeleteMBIDOverride(runCtx, rest[0]); err != nil {
			return err
		}
		fmt.Printf("Override cleared for %s\n", rest[0])
		return nil

	default:
		return fmt.Errorf("unknown mbid subcommand %q (want set or clear)", sub)
	}
}
