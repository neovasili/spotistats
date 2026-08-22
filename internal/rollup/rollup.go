package rollup

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/spotify"
	"github.com/neovasili/spotistats/internal/store"
)

// DefaultWindowDays is how far back a nightly reconcile reads.
//
// Wide enough to cover a long outage, narrow enough that the read stays cheap. Capture runs
// every two hours, so anything older than this has been reconciled several times already.
const DefaultWindowDays = 45

// SpotifyAPI is the slice of the Spotify client the rollup uses: Spotify's own top-items
// rankings, stored alongside the computed leaderboards.
type SpotifyAPI interface {
	TopArtists(ctx context.Context, tr spotify.TimeRange, limit, offset int) ([]model.Artist, error)
	TopTracks(ctx context.Context, tr spotify.TimeRange, limit, offset int) ([]model.Track, error)
}

// Publisher writes the rendered snapshots and invalidates the CDN.
//
// An interface so the local CLI can write to a directory while the Lambda writes to S3, and so
// the rendering logic is testable with no AWS at all.
type Publisher interface {
	// Publish writes one snapshot. name is a path relative to the data prefix.
	Publish(ctx context.Context, name string, body []byte) error
	// Invalidate purges the CDN paths. A no-op for a local publisher.
	Invalidate(ctx context.Context, paths []string) error
}

// Config configures a Rollup.
type Config struct {
	Store    *store.Store
	Calendar model.Calendar

	// Spotify is optional. Without it the Spotify-sourced top items are skipped; everything
	// else still runs, because those rankings are supplementary and their absence must not
	// stop the reconcile.
	Spotify SpotifyAPI

	// Publisher is optional. Without it snapshots are computed but not written, which is what
	// a reconcile-only run wants.
	Publisher Publisher

	// WindowDays overrides DefaultWindowDays.
	WindowDays int

	// LeaderboardSize is how many entries each materialised leaderboard holds.
	LeaderboardSize int

	Now    func() time.Time
	Logger *slog.Logger
}

// Rollup runs the nightly job.
type Rollup struct {
	store     *store.Store
	cal       model.Calendar
	spotify   SpotifyAPI
	publisher Publisher
	window    int
	topN      int
	now       func() time.Time
	log       *slog.Logger
}

// DefaultLeaderboardSize is enough for any dashboard widget plus a healthy tail.
const DefaultLeaderboardSize = 100

// New validates cfg and returns a Rollup.
func New(cfg Config) (*Rollup, error) {
	if cfg.Store == nil {
		return nil, errors.New("rollup: a store is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	window := cfg.WindowDays
	if window <= 0 {
		window = DefaultWindowDays
	}
	topN := cfg.LeaderboardSize
	if topN <= 0 {
		topN = DefaultLeaderboardSize
	}
	return &Rollup{
		store: cfg.Store, cal: cfg.Calendar, spotify: cfg.Spotify,
		publisher: cfg.Publisher, window: window, topN: topN, now: now, log: log,
	}, nil
}

// Result summarises a run.
type Result struct {
	// PlaysRead is how many raw plays the reconcile streamed.
	PlaysRead int
	// RowsChecked and RowsCorrected quantify drift. RowsCorrected > 0 means a capture run died
	// between writing a play and applying its aggregates.
	RowsChecked   int
	RowsCorrected int
	// PropagatedRows is how many year and all-time rows received a correction.
	PropagatedRows int

	LeaderboardsWritten int
	HistogramsWritten   int

	// UnresolvedNames is how many entities will render as raw IDs because their dimension row is
	// missing or unenriched. Non-zero means capture could not reach GET /v1/artists (or the
	// equivalent), which also costs genre attribution.
	UnresolvedNames int

	SnapshotsWritten  int
	TopItemsRefreshed bool

	// SkippedSpotify records that the Spotify top-items refresh was skipped or failed. Not an
	// error: those rankings are supplementary.
	SkippedSpotify bool

	Duration time.Duration
}

// LogAttrs renders the result for structured logging.
func (r Result) LogAttrs() []any {
	return []any{
		"playsRead", r.PlaysRead,
		"rowsChecked", r.RowsChecked,
		"rowsCorrected", r.RowsCorrected,
		"propagatedRows", r.PropagatedRows,
		"leaderboards", r.LeaderboardsWritten,
		"unresolvedNames", r.UnresolvedNames,
		"histograms", r.HistogramsWritten,
		"snapshots", r.SnapshotsWritten,
		"topItemsRefreshed", r.TopItemsRefreshed,
		"skippedSpotify", r.SkippedSpotify,
		"durationMs", r.Duration.Milliseconds(),
	}
}

// Run performs the nightly job in the order docs/SPECS.md 4.3 specifies: reconcile first, so
// everything computed afterwards is derived from corrected counters.
func (r *Rollup) Run(ctx context.Context) (Result, error) {
	start := r.now()
	var res Result

	rec, err := r.Reconcile(ctx, r.window)
	res.PlaysRead = rec.PlaysRead
	res.RowsChecked = rec.RowsChecked
	res.RowsCorrected = rec.RowsCorrected
	res.PropagatedRows = rec.PropagatedRows
	if err != nil {
		res.Duration = r.now().Sub(start)
		return res, err
	}

	boards, unresolved, err := r.RefreshLeaderboards(ctx)
	res.LeaderboardsWritten = boards
	res.UnresolvedNames = unresolved
	if err != nil {
		res.Duration = r.now().Sub(start)
		return res, err
	}

	hists, err := r.RefreshHistograms(ctx)
	res.HistogramsWritten = hists
	if err != nil {
		res.Duration = r.now().Sub(start)
		return res, err
	}

	// Spotify's own rankings are supplementary: a rate limit or an outage here must not fail a
	// run whose real work -- the reconcile -- already succeeded.
	if r.spotify != nil {
		if err := r.refreshSpotifyTopItems(ctx); err != nil {
			res.SkippedSpotify = true
			r.log.WarnContext(ctx, "rollup: Spotify top-items refresh failed; "+
				"the computed leaderboards are unaffected", "err", err)
		} else {
			res.TopItemsRefreshed = true
		}
	} else {
		res.SkippedSpotify = true
	}

	if r.publisher != nil {
		n, err := r.RenderSnapshots(ctx)
		res.SnapshotsWritten = n
		if err != nil {
			res.Duration = r.now().Sub(start)
			return res, err
		}
	}

	res.Duration = r.now().Sub(start)
	return res, nil
}
