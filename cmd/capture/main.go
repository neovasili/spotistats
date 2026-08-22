// Command capture is the scheduled Lambda that ingests the Spotify recently-played feed.
//
// It runs every two hours rather than nightly. The endpoint retains only about 50 plays and
// cannot page back into history, so a nightly run would silently and permanently lose data
// on any day with more than roughly three hours of listening. See docs/SPECS.md 2.1.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"

	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/ingest"
	"github.com/neovasili/spotistats/internal/metrics"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/spotify"
)

// Dependencies are built once per container and reused across warm invocations. A failed
// build is deliberately NOT cached, so a transient SSM or DynamoDB problem is retried on the
// next invocation rather than poisoning the container for its whole lifetime.
var (
	depsMu     sync.Mutex
	cachedDeps *config.Deps
)

// Response is returned to the caller and appears in the invocation log.
type Response struct {
	Fetched          int    `json:"fetched"`
	Inserted         int    `json:"inserted"`
	Duplicates       int    `json:"duplicates"`
	DeltasApplied    int    `json:"deltasApplied"`
	ArtistsWritten   int    `json:"artistsWritten"`
	TracksWritten    int    `json:"tracksWritten"`
	AlbumsWritten    int    `json:"albumsWritten"`
	Tombstoned       int    `json:"tombstoned"`
	GenresDegraded   bool   `json:"genresDegraded"`
	Saturated        bool   `json:"saturated"`
	GapRecorded      bool   `json:"gapRecorded"`
	CursorAdvancedTo string `json:"cursorAdvancedTo,omitempty"`
	DurationMs       int64  `json:"durationMs"`
}

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context, _ json.RawMessage) (Response, error) {
	start := time.Now()
	em := metrics.New()
	// Metrics are flushed on every path, including failure: a run that failed is exactly
	// when the alarm needs the data point.
	defer func() {
		if err := em.Flush(); err != nil {
			slog.WarnContext(ctx, "capture: flush metrics", "err", err)
		}
	}()

	em.Put(metrics.CaptureRun, metrics.UnitCount, 1)

	res, err := run(ctx, em)

	resp := Response{
		Fetched:        res.Fetched,
		Inserted:       res.Inserted,
		Duplicates:     res.Duplicates,
		DeltasApplied:  res.DeltasApplied,
		ArtistsWritten: res.ArtistsWritten,
		TracksWritten:  res.TracksWritten,
		AlbumsWritten:  res.AlbumsWritten,
		Tombstoned:     res.Tombstoned,
		GenresDegraded: res.GenresDegraded,
		Saturated:      res.Saturated,
		GapRecorded:    res.GapRecorded,
		DurationMs:     time.Since(start).Milliseconds(),
	}
	if !res.CursorAdvancedTo.IsZero() {
		resp.CursorAdvancedTo = model.FormatTS(res.CursorAdvancedTo)
	}

	publishMetrics(em, res, err)

	if err != nil {
		return resp, err
	}
	return resp, nil
}

// publishMetrics maps a run outcome onto the metrics the CloudWatch alarms threshold on.
//
// It is a separate function because this mapping IS the alarm behaviour: if GapRecorded
// stopped emitting PlaysGapDetected, the system would keep losing plays with nobody
// notified, and nothing else in the code would look wrong.
func publishMetrics(em metrics.Emitter, res ingest.Result, err error) {
	em.Put(metrics.PlaysIngested, metrics.UnitCount, float64(res.Inserted))
	em.Put(metrics.PlaysDuplicate, metrics.UnitCount, float64(res.Duplicates))
	em.Put(metrics.PlaysGapDetected, metrics.UnitCount, metrics.Bool(res.GapRecorded))
	em.Put(metrics.GenresDegraded, metrics.UnitCount, metrics.Bool(res.GenresDegraded))
	em.Put(metrics.CaptureFailed, metrics.UnitCount, metrics.Bool(err != nil))

	// A revoked refresh token cannot be fixed by retrying and needs a human to re-run the
	// interactive authorisation flow, so it gets its own metric rather than being folded
	// into a generic "capture failed".
	if err != nil && errors.Is(err, spotify.ErrRefreshTokenInvalid) {
		em.Put(metrics.TokenRefreshFailed, metrics.UnitCount, 1)
	}
}

func run(ctx context.Context, em metrics.Emitter) (ingest.Result, error) {
	cfg := config.Load()

	deps, err := getDeps(ctx, cfg, em)
	if err != nil {
		return ingest.Result{}, err
	}

	log := deps.Logger
	if rc, ok := lambdacontext.FromContext(ctx); ok {
		log = log.With("requestId", rc.AwsRequestID)
	}

	// If Spotify issued a replacement refresh token that could not be persisted, retry the
	// write before the container is frozen. The previous token is probably already invalid,
	// so losing the new one means every future run fails to authenticate.
	defer func() {
		pending, ok := deps.TokenSource.PendingRotation()
		if !ok {
			return
		}
		retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if perr := deps.TokenStore.Put(retryCtx, pending); perr != nil {
			em.Put(metrics.TokenRefreshFailed, metrics.UnitCount, 1)
			log.ErrorContext(ctx, "capture: a rotated refresh token could not be persisted; "+
				"future runs will fail to authenticate until `spotistats auth login` is re-run",
				"err", perr)
			return
		}
		deps.TokenSource.ClearPendingRotation()
		log.WarnContext(ctx, "capture: rotated refresh token persisted on retry")
	}()

	capturer, err := ingest.New(ingest.Config{
		Spotify: deps.Spotify,
		Store:   deps.Store,
		Limit:   cfg.CaptureLimit,
		Logger:  log,
	})
	if err != nil {
		return ingest.Result{}, err
	}

	res, err := capturer.Run(ctx)
	log.InfoContext(ctx, "capture: run complete", res.LogAttrs()...)
	if err != nil {
		return res, fmt.Errorf("capture: %w", err)
	}
	return res, nil
}

// getDeps builds or reuses the dependency set.
func getDeps(ctx context.Context, cfg config.Config, em metrics.Emitter) (*config.Deps, error) {
	depsMu.Lock()
	defer depsMu.Unlock()

	if cachedDeps != nil {
		return cachedDeps, nil
	}

	d, err := config.Build(ctx, cfg, config.BuildOptions{
		NeedStore:   true,
		NeedSpotify: true,
		// Verified once per container. A changed timezone would silently split history
		// across two calendars, so it must fail loudly on startup.
		VerifyStoreConfig: true,
		OnRotationError: func(_ context.Context, e error) {
			em.Put(metrics.TokenRefreshFailed, metrics.UnitCount, 1)
			slog.Error("capture: refresh token rotation could not be persisted", "err", e)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("capture: build dependencies: %w", err)
	}

	cachedDeps = d
	return d, nil
}
