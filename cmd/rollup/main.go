// Command rollup is the nightly Lambda: it reconciles drifted aggregates, materialises
// leaderboards and histograms, and renders the static snapshots the dashboard reads.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/metrics"
	"github.com/neovasili/spotistats/internal/rollup"
)

// Environment variables specific to this function.
const (
	envWebBucket      = "SPOTISTATS_WEB_BUCKET"
	envDistributionID = "SPOTISTATS_DISTRIBUTION_ID"
	envWindowDays     = "SPOTISTATS_RECONCILE_WINDOW_DAYS"
)

var (
	mu     sync.Mutex
	cached *rollup.Rollup
)

// Response appears in the invocation log.
type Response struct {
	PlaysRead      int   `json:"playsRead"`
	RowsChecked    int   `json:"rowsChecked"`
	RowsCorrected  int   `json:"rowsCorrected"`
	PropagatedRows int   `json:"propagatedRows"`
	Leaderboards   int   `json:"leaderboards"`
	Histograms     int   `json:"histograms"`
	Snapshots      int   `json:"snapshots"`
	TopItems       bool  `json:"topItemsRefreshed"`
	DurationMs     int64 `json:"durationMs"`
}

func main() { lambda.Start(handler) }

func handler(ctx context.Context, _ json.RawMessage) (Response, error) {
	em := metrics.New()
	defer func() {
		if err := em.Flush(); err != nil {
			slog.WarnContext(ctx, "rollup: flush metrics", "err", err)
		}
	}()

	res, err := run(ctx)

	// AggregateDrift is the signal that a capture run died between writing a play and applying
	// its aggregates. Non-zero is not an outage -- it is the system self-healing -- but a
	// persistently non-zero value means something is failing repeatedly.
	em.Put(metrics.AggregateDrift, metrics.UnitCount, float64(res.RowsCorrected))
	em.Put(metrics.RollupRun, metrics.UnitCount, 1)
	em.Put(metrics.RollupFailed, metrics.UnitCount, metrics.Bool(err != nil))

	resp := Response{
		PlaysRead: res.PlaysRead, RowsChecked: res.RowsChecked,
		RowsCorrected: res.RowsCorrected, PropagatedRows: res.PropagatedRows,
		Leaderboards: res.LeaderboardsWritten, Histograms: res.HistogramsWritten,
		Snapshots: res.SnapshotsWritten, TopItems: res.TopItemsRefreshed,
		DurationMs: res.Duration.Milliseconds(),
	}
	return resp, err
}

func run(ctx context.Context) (rollup.Result, error) {
	r, err := get(ctx)
	if err != nil {
		return rollup.Result{}, err
	}

	log := slog.Default()
	if rc, ok := lambdacontext.FromContext(ctx); ok {
		log = log.With("requestId", rc.AwsRequestID)
	}

	res, err := r.Run(ctx)
	log.InfoContext(ctx, "rollup: run complete", res.LogAttrs()...)
	return res, err
}

func get(ctx context.Context) (*rollup.Rollup, error) {
	mu.Lock()
	defer mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	cfg := config.Load()

	// The store is mandatory; Spotify is NOT.
	//
	// These were originally resolved in one call with NeedSpotify: true, which made the whole
	// run depend on the Spotify credentials being present in SSM. That defeated the intent
	// stated in internal/rollup -- "those rankings are supplementary and their absence must not
	// stop the reconcile" -- and in practice meant that on a deployment where `auth login` had
	// not yet been run, the rollup failed before writing a single snapshot and the dashboard
	// served a 403 forever.
	deps, err := config.Build(ctx, cfg, config.BuildOptions{
		NeedStore: true,
		// The rollup writes aggregates, so a timezone mismatch would corrupt them just as it
		// would for capture.
		VerifyStoreConfig: true,
	})
	if err != nil {
		return nil, fmt.Errorf("rollup: build dependencies: %w", err)
	}

	// Attempted separately, and allowed to fail: without it the Spotify-sourced top items are
	// skipped and everything else -- the reconcile, the leaderboards, the snapshots -- still runs.
	var spotifyAPI rollup.SpotifyAPI
	if sdeps, serr := config.Build(ctx, cfg, config.BuildOptions{NeedSpotify: true}); serr != nil {
		slog.WarnContext(ctx, "rollup: Spotify credentials unavailable; skipping the "+
			"Spotify top-items refresh. Run `spotistats auth login` to enable it.", "err", serr)
	} else {
		spotifyAPI = sdeps.Spotify
	}

	cal, err := cfg.Calendar()
	if err != nil {
		return nil, err
	}

	bucket := os.Getenv(envWebBucket)
	if bucket == "" {
		return nil, errors.New("rollup: " + envWebBucket + " is required")
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("rollup: load AWS configuration: %w", err)
	}
	publisher := rollup.NewS3Publisher(
		s3.NewFromConfig(awsCfg),
		// CloudFront is a global service; its API lives in us-east-1 regardless of where the
		// stack is deployed.
		cloudfront.NewFromConfig(awsCfg, func(o *cloudfront.Options) { o.Region = "us-east-1" }),
		bucket, os.Getenv(envDistributionID), time.Now,
	)

	window := 0
	if v := os.Getenv(envWindowDays); v != "" {
		if n, cerr := fmt.Sscanf(v, "%d", &window); cerr != nil || n != 1 {
			window = 0
		}
	}

	r, err := rollup.New(rollup.Config{
		Store:      deps.Store,
		Calendar:   cal,
		Spotify:    spotifyAPI,
		Publisher:  publisher,
		WindowDays: window,
		Logger:     deps.Logger,
	})
	if err != nil {
		return nil, err
	}
	cached = r
	return cached, nil
}
