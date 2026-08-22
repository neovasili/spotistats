package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/ingest"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/spotify"
)

func runPoll(ctx context.Context, args []string) error {
	fs := newFlagSet("poll", "poll [flags]")
	limit := fs.Int("limit", 0, "page size, 1-50 (default: the configured capture limit)")
	dryRun := fs.Bool("dry-run", false, "fetch and report, but write nothing")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall deadline for the run")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := config.Load()
	if *limit > 0 {
		cfg.CaptureLimit = *limit
	}

	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	// A rotation that could not be persisted needs a human, so surface it loudly.
	var rotationErr error
	deps, err := config.Build(runCtx, cfg, config.BuildOptions{
		NeedStore:         true,
		NeedSpotify:       true,
		VerifyStoreConfig: true,
		OnRotationError: func(_ context.Context, e error) {
			rotationErr = e
		},
	})
	if err != nil {
		return err
	}

	// If Spotify issued a new refresh token that could not be stored, retry the write
	// before exiting: the previous token is probably already invalid, so losing the new one
	// means the next unattended run cannot authenticate at all.
	defer func() {
		if pending, ok := deps.TokenSource.PendingRotation(); ok {
			retryCtx, c := context.WithTimeout(context.Background(), 20*time.Second)
			defer c()
			if err := deps.TokenStore.Put(retryCtx, pending); err != nil {
				fmt.Printf("\nACTION REQUIRED: a rotated refresh token could not be stored "+
					"(%v).\nThe next run may fail to authenticate; re-run `%s auth login`.\n",
					err, progName)
				return
			}
			deps.TokenSource.ClearPendingRotation()
			bullet("recovered: the rotated refresh token was stored on retry")
		}
	}()

	if *dryRun {
		return pollDryRun(runCtx, deps, cfg)
	}

	capturer, err := ingest.New(ingest.Config{
		Spotify: deps.Spotify,
		Store:   deps.Store,
		Limit:   cfg.CaptureLimit,
		Logger:  deps.Logger,
	})
	if err != nil {
		return err
	}

	res, err := capturer.Run(runCtx)
	deps.Logger.InfoContext(runCtx, "capture run complete", res.LogAttrs()...)
	// A run that failed before fetching anything has nothing to report, and an all-zeros
	// table above the error message is just noise.
	if err == nil || res.Fetched > 0 {
		reportResult(res, cfg)
	}

	if err != nil {
		if errors.Is(err, spotify.ErrRefreshTokenInvalid) {
			return fmt.Errorf("the stored refresh token is invalid or revoked; "+
				"re-run `%s auth login`: %w", progName, err)
		}
		return err
	}
	if rotationErr != nil {
		bullet("warning: %v", rotationErr)
	}
	return nil
}

// pollDryRun reports what a run would ingest without writing anything.
func pollDryRun(ctx context.Context, deps *config.Deps, cfg config.Config) error {
	cursor, err := deps.Store.GetPollCursor(ctx)
	if err != nil {
		return err
	}
	page, err := deps.Spotify.RecentlyPlayed(ctx, spotify.RecentlyPlayedOptions{
		Limit: cfg.CaptureLimit,
		After: cursor.LastPlayedAt,
	})
	if err != nil {
		return err
	}

	heading("Dry run - nothing written")
	bullet("cursor:    %s", tsOrDash(cursor.LastPlayedAt))
	bullet("fetched:   %d play(s) (limit %d)", len(page.Plays), cfg.CaptureLimit)
	if len(page.Plays) > 0 {
		bullet("oldest:    %s", tsOrDash(page.OldestPlayedAt))
		bullet("newest:    %s", tsOrDash(page.NewestPlayedAt))
	}
	if page.Saturated {
		bullet("SATURATED: the page came back full; plays may already have been lost")
	}
	fmt.Println()
	for _, p := range page.Plays {
		name := p.TrackID
		if tr, ok := page.Tracks[p.TrackID]; ok && tr.Name != "" {
			name = tr.Name
		}
		bullet("%s  %s", model.FormatTS(p.PlayedAt), name)
	}
	return nil
}

func reportResult(res ingest.Result, cfg config.Config) {
	heading("Capture run")
	bullet("requested after: %s", tsOrDash(res.RequestedAfter))
	bullet("fetched:         %d", res.Fetched)
	bullet("inserted:        %d", res.Inserted)
	bullet("duplicates:      %d", res.Duplicates)
	bullet("aggregate writes:%d", res.DeltasApplied)
	bullet("artists written: %d (fetched %d, tombstoned %d)",
		res.ArtistsWritten, res.ArtistsFetched, res.Tombstoned)
	bullet("metadata written:%d track(s), %d album(s)", res.TracksWritten, res.AlbumsWritten)
	bullet("cursor now:      %s", tsOrDash(res.CursorAdvancedTo))

	if res.GenresDegraded {
		fmt.Println()
		bullet("DEGRADED: artist genres could not be resolved, so genre aggregates are")
		bullet("          incomplete for this batch. The nightly reconcile repairs them.")
	}
	if res.GapRecorded {
		fmt.Println()
		bullet("GAP RECORDED: the page came back full (%d of %d), so listening may have",
			res.Fetched, cfg.CaptureLimit)
		bullet("              outrun the polling interval. recently-played cannot page back,")
		bullet("              so anything missed is unrecoverable. Consider polling more often.")
	}

	// These four are logged on every run to settle whether `after` returns the oldest or the
	// newest matching items, which decides whether a saturated page means plays were merely
	// at risk or definitely lost.
	if res.Fetched > 0 {
		heading("Cursor semantics (see docs/SPECS.md 4.1)")
		bullet("requested after: %s", tsOrDash(res.RequestedAfter))
		bullet("oldest returned: %s", tsOrDash(res.OldestPlayedAt))
		bullet("newest returned: %s", tsOrDash(res.NewestPlayedAt))
		bullet("echoed after:    %s", tsOrDash(res.EchoedAfter))
		bullet("echoed before:   %s", tsOrDash(res.EchoedBefore))
		bullet("has next page:   %v", res.HasNext)
	}
}

func tsOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return model.FormatTS(t)
}
