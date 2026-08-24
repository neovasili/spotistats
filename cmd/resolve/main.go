// Command resolve is the track-identity Lambda: it upgrades placeholder track rows to real
// Spotify identity, a bounded batch per night.
//
// # Why this is budgeted rather than fast
//
// It shares Spotify's quota with capture, and capture is the one job in this system whose
// failure is unrecoverable: recently-played returns a rolling ~50-play window, so consecutive
// failures lose listening PERMANENTLY and no reconcile can bring it back. Resolution loses
// nothing by being slow — the plays are already stored, and a later reconcile upgrades seventeen
// years of aggregates without reimporting anything.
//
// So the default batch is well under the observed rate-limit window, and a 429 writes a cooldown
// row that stops the NEXT run from asking at all. Stopping asking is the point: a request made
// during a cooldown is quota taken from capture for nothing.
package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/neovasili/spotistats/internal/backfill"
	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/metrics"
)

// EnvResolveLimit overrides how many tracks one invocation resolves.
const EnvResolveLimit = "SPOTISTATS_RESOLVE_LIMIT"

// Event optionally bounds one invocation, so a manual `aws lambda invoke` can take a smaller
// bite or override a cooldown without a redeploy.
type Event struct {
	Limit int  `json:"limit"`
	Force bool `json:"force"`
}

// Response mirrors the CLI's report.
type Response struct {
	Backlog        int    `json:"backlog"`
	Fetched        int    `json:"fetched"`
	TracksWritten  int    `json:"tracksWritten"`
	AlbumsWritten  int    `json:"albumsWritten"`
	ArtistsWritten int    `json:"artistsWritten"`
	Remaining      int    `json:"remaining"`
	Skipped        bool   `json:"skipped"`
	SuspendedUntil string `json:"suspendedUntil,omitempty"`
	DurationMs     int64  `json:"durationMs"`
}

func main() {
	lambda.Start(handle)
}

func handle(ctx context.Context, ev Event) (Response, error) {
	cfg := config.Load()
	deps, err := config.Build(ctx, cfg, config.BuildOptions{
		NeedStore:         true,
		NeedSpotify:       true,
		VerifyStoreConfig: true,
	})
	if err != nil {
		return Response{}, err
	}
	log := deps.Logger

	limit := ev.Limit
	if limit == 0 {
		limit = envInt(EnvResolveLimit, backfill.DefaultResolveLimit)
	}

	// If Spotify issued a replacement refresh token that could not be persisted, retry the
	// write before the container is frozen. The previous token is probably already invalid, so
	// losing the new one means every future run -- including capture's -- fails to
	// authenticate.
	defer func() {
		pending, ok := deps.TokenSource.PendingRotation()
		if !ok {
			return
		}
		retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if perr := deps.TokenStore.Put(retryCtx, pending); perr != nil {
			log.ErrorContext(ctx, "resolve: a rotated refresh token could not be persisted; "+
				"future runs will fail to authenticate until `spotistats auth login` is re-run",
				"err", perr)
			return
		}
		deps.TokenSource.ClearPendingRotation()
		log.WarnContext(ctx, "resolve: rotated refresh token persisted on retry")
	}()

	resolver := backfill.NewResolver(deps.Store, deps.Spotify, log, nil)
	res, err := resolver.Run(ctx, backfill.ResolveOptions{Limit: limit, Force: ev.Force})
	emit(ctx, log, res, err)
	if err != nil {
		return Response{}, err
	}

	out := Response{
		Backlog: res.Backlog, Fetched: res.Fetched,
		TracksWritten: res.TracksWritten, AlbumsWritten: res.AlbumsWritten,
		ArtistsWritten: res.ArtistsWritten, Remaining: res.Remaining,
		Skipped: res.Skipped, DurationMs: res.Duration.Milliseconds(),
	}
	if !res.SuspendedUntil.IsZero() {
		out.SuspendedUntil = res.SuspendedUntil.Format(time.RFC1123)
	}

	log.InfoContext(ctx, "resolve: run complete",
		"backlog", res.Backlog, "fetched", res.Fetched, "remaining", res.Remaining,
		"skipped", res.Skipped, "suspendedUntil", out.SuspendedUntil)
	return out, nil
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n != 0 {
			return n
		}
	}
	return def
}

// emit publishes EMF metrics.
//
// ResolveFailed excludes a spent quota deliberately. Hitting the rate limit is the expected end
// of a run under a shared quota, so alarming on it would page every night for normal operation
// and train the reader to ignore the channel.
func emit(ctx context.Context, log *slog.Logger, res backfill.ResolveResult, runErr error) {
	em := metrics.New()
	em.Put(metrics.ResolveRun, metrics.UnitCount, 1)
	em.Put(metrics.ResolveFailed, metrics.UnitCount, metrics.Bool(runErr != nil))
	em.Put(metrics.ResolveFetched, metrics.UnitCount, float64(res.Fetched))
	em.Put(metrics.ResolveRemaining, metrics.UnitCount, float64(res.Remaining))
	em.Put(metrics.ResolveSuspended, metrics.UnitCount, metrics.Bool(res.Skipped))
	if err := em.Flush(); err != nil {
		// A metrics failure must never fail the run: the work is already durable.
		log.WarnContext(ctx, "resolve: could not emit metrics", "err", err)
	}
}
