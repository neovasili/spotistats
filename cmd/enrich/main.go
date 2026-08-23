// Command enrich is the external-enrichment Lambda: MusicBrainz facts plus TheAudioDB prose
// and artwork for every artist ever played.
//
// It runs daily, an hour after the rollup so the two never overlap, and it is the one Lambda in
// this system with ReservedConcurrentExecutions: 1. Both upstream rate limits are per-IP, so a
// self-imposed limiter inside the process means nothing if two invocations overlap — two
// concurrent runs double the real rate and earn a 503 for everything.
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/enrich"
	"github.com/neovasili/spotistats/internal/metrics"
)

// Event optionally bounds one invocation, so a manual `aws lambda invoke` can enrich a single
// artist or cap the work without a redeploy.
type Event struct {
	Limit    int    `json:"limit"`
	Force    bool   `json:"force"`
	ArtistID string `json:"artistId"`
}

// Response is what the invocation returns, mirroring the CLI's report.
type Response struct {
	Candidates   int            `json:"candidates"`
	Skipped      int            `json:"skipped"`
	Resolved     int            `json:"resolved"`
	Unresolved   int            `json:"unresolved"`
	FactsWritten int            `json:"factsWritten"`
	ProseWritten int            `json:"proseWritten"`
	SourceErrors map[string]int `json:"sourceErrors,omitempty"`
	Remaining    int            `json:"remaining"`
	DurationMs   int64          `json:"durationMs"`
}

func main() {
	lambda.Start(handle)
}

func handle(ctx context.Context, ev Event) (Response, error) {
	cfg := config.Load()
	deps, err := config.Build(ctx, cfg, config.BuildOptions{
		NeedStore:         true,
		NeedExternal:      true,
		VerifyStoreConfig: true,
	})
	if err != nil {
		return Response{}, err
	}
	log := deps.Logger

	e, err := enrich.New(enrich.Config{
		Store:       deps.Store,
		MusicBrainz: deps.MusicBrainz,
		AudioDB:     deps.AudioDB,
		Language:    cfg.BiographyLanguage,
		Logger:      log,
	})
	if err != nil {
		return Response{}, err
	}

	// Leave headroom before the Lambda's own timeout so the run can checkpoint and report
	// rather than being killed mid-artist.
	runCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithDeadline(ctx, deadline.Add(-10*time.Second))
		defer cancel()
	}

	res, runErr := e.Run(runCtx, enrich.Options{
		Limit: ev.Limit, Force: ev.Force, ArtistID: ev.ArtistID,
	})

	// Metrics are emitted whether or not the run failed: a failed run's partial numbers are
	// exactly what an alarm needs to see.
	emit(ctx, log, res, runErr)

	if runErr != nil {
		log.ErrorContext(ctx, "enrich: run failed", "err", runErr)
		return Response{}, runErr
	}
	log.InfoContext(ctx, "enrich: run complete", res.LogAttrs()...)
	return Response{
		Candidates: res.Candidates, Skipped: res.Skipped,
		Resolved: res.Resolved, Unresolved: res.Unresolved,
		FactsWritten: res.FactsWritten, ProseWritten: res.ProseWritten,
		SourceErrors: res.SourceErrors, Remaining: res.Remaining,
		DurationMs: res.Duration.Milliseconds(),
	}, nil
}

// emit publishes EMF metrics.
//
// The unresolved RATIO is the one worth alarming on rather than the count: a count rises slowly
// as obscure artists accumulate, while a sudden ratio jump means an upstream shape change.
func emit(ctx context.Context, log *slog.Logger, res enrich.Result, runErr error) {
	em := metrics.New()
	em.Put(metrics.ExternalEnrichRun, metrics.UnitCount, 1)
	em.Put(metrics.ExternalArtistsResolved, metrics.UnitCount, float64(res.Resolved))
	em.Put(metrics.ExternalArtistsUnresolved, metrics.UnitCount, float64(res.Unresolved))
	em.Put(metrics.ExternalUnresolvedRatio, metrics.UnitCount, res.UnresolvedRatio())
	em.Put(metrics.ExternalSourceErrors, metrics.UnitCount, float64(sumErrors(res.SourceErrors)))
	em.Put(metrics.ExternalEnrichFailed, metrics.UnitCount, metrics.Bool(runErr != nil))
	if err := em.Flush(); err != nil {
		// A metrics failure must never fail the run: the work is already durable.
		log.WarnContext(ctx, "enrich: could not emit metrics", "err", err)
	}
}

func sumErrors(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
