package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/spotify"
	"github.com/neovasili/spotistats/internal/store"
)

// SpotifyAPI is the slice of the Spotify client the pipeline uses. *spotify.Client satisfies
// it; tests supply a scripted fake.
type SpotifyAPI interface {
	RecentlyPlayed(context.Context, spotify.RecentlyPlayedOptions) (spotify.RecentlyPlayedPage, error)
	Artists(context.Context, []string) ([]model.Artist, []string, error)
}

// Config configures a Capturer.
type Config struct {
	Spotify SpotifyAPI
	Store   *store.Store

	// Limit is the page size, clamped by the client to at most 50.
	Limit int

	// Now supplies the run timestamp. Defaults to time.Now.
	Now func() time.Time

	Logger *slog.Logger
}

// Capturer runs one capture pass.
type Capturer struct {
	api   SpotifyAPI
	store *store.Store
	limit int
	now   func() time.Time
	log   *slog.Logger
}

// New validates cfg and returns a Capturer.
func New(cfg Config) (*Capturer, error) {
	if cfg.Spotify == nil {
		return nil, errors.New("ingest: a Spotify client is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("ingest: a store is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	limit := cfg.Limit
	if limit <= 0 || limit > spotify.MaxRecentlyPlayedLimit {
		limit = spotify.MaxRecentlyPlayedLimit
	}
	return &Capturer{api: cfg.Spotify, store: cfg.Store, limit: limit, now: now, log: log}, nil
}

// Result summarises one capture run. The cursor fields exist to settle, with real data,
// whether Spotify's `after` returns the oldest or the newest matching items -- which decides
// whether a saturated page means plays were at risk or definitely lost.
type Result struct {
	RequestedAfter time.Time
	Fetched        int
	Inserted       int
	Duplicates     int
	DeltasApplied  int

	ArtistsFetched int
	ArtistsWritten int
	TracksWritten  int
	AlbumsWritten  int
	Tombstoned     int

	// GenresDegraded reports that artist resolution failed, so some plays were recorded
	// with incomplete genre attribution. Recoverable: the nightly reconcile recomputes
	// genre aggregates from the raw plays once the artist rows exist.
	GenresDegraded bool

	Saturated   bool
	GapRecorded bool

	OldestPlayedAt   time.Time
	NewestPlayedAt   time.Time
	EchoedAfter      time.Time
	EchoedBefore     time.Time
	HasNext          bool
	CursorAdvancedTo time.Time
}

// LogAttrs renders the result for structured logging.
func (r Result) LogAttrs() []any {
	return []any{
		"fetched", r.Fetched,
		"inserted", r.Inserted,
		"duplicates", r.Duplicates,
		"deltasApplied", r.DeltasApplied,
		"artistsFetched", r.ArtistsFetched,
		"artistsWritten", r.ArtistsWritten,
		"tracksWritten", r.TracksWritten,
		"albumsWritten", r.AlbumsWritten,
		"tombstoned", r.Tombstoned,
		"genresDegraded", r.GenresDegraded,
		"saturated", r.Saturated,
		"gapRecorded", r.GapRecorded,
		"requestedAfter", tsOrEmpty(r.RequestedAfter),
		"oldestPlayedAt", tsOrEmpty(r.OldestPlayedAt),
		"newestPlayedAt", tsOrEmpty(r.NewestPlayedAt),
		"echoedAfter", tsOrEmpty(r.EchoedAfter),
		"echoedBefore", tsOrEmpty(r.EchoedBefore),
		"hasNext", r.HasNext,
		"cursorAdvancedTo", tsOrEmpty(r.CursorAdvancedTo),
	}
}

func tsOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return model.FormatTS(t)
}

// Run performs one capture pass. See the package doc for why the order is what it is.
func (c *Capturer) Run(ctx context.Context) (Result, error) {
	var res Result
	runAt := c.now()

	// 1. Where did the last run get to?
	cursor, err := c.store.GetPollCursor(ctx)
	if err != nil {
		return res, fmt.Errorf("ingest: read poll cursor: %w", err)
	}
	res.RequestedAfter = cursor.LastPlayedAt

	// 2. Fetch one page. A zero cursor means "the most recent page", which is the first-run
	// case.
	page, err := c.api.RecentlyPlayed(ctx, spotify.RecentlyPlayedOptions{
		Limit: c.limit,
		After: cursor.LastPlayedAt,
	})
	if err != nil {
		return res, fmt.Errorf("ingest: fetch recently played: %w", err)
	}

	res.Fetched = len(page.Plays)
	res.Saturated = page.Saturated
	res.OldestPlayedAt = page.OldestPlayedAt
	res.NewestPlayedAt = page.NewestPlayedAt
	res.EchoedAfter = page.NextAfter
	res.EchoedBefore = page.NextBefore
	res.HasNext = page.HasNext

	// 3. Gap detection. A full page means listening may have outrun the polling interval,
	// and the endpoint cannot page back into history, so anything missed is gone.
	if page.Saturated {
		gap := model.GapMarker{
			DetectedAt:    runAt,
			WindowStart:   cursor.LastPlayedAt,
			WindowEnd:     page.NewestPlayedAt,
			ItemsReturned: len(page.Plays),
			Limit:         c.limit,
		}
		if err := c.store.PutGapMarker(ctx, gap); err != nil {
			return res, fmt.Errorf("ingest: record gap marker: %w", err)
		}
		res.GapRecorded = true
		c.log.WarnContext(ctx, "ingest: capture window saturated; plays may have been lost",
			"itemsReturned", len(page.Plays), "limit", c.limit,
			"windowStart", tsOrEmpty(cursor.LastPlayedAt),
			"windowEnd", tsOrEmpty(page.NewestPlayedAt))
	}

	if len(page.Plays) == 0 {
		c.log.InfoContext(ctx, "ingest: nothing new", "requestedAfter", tsOrEmpty(cursor.LastPlayedAt))
		return res, c.finish(ctx, &res, cursor, runAt, page)
	}

	// 4-5. Resolve artist genres BEFORE recording, so a first-ever artist still gets genre
	// attribution. See the package doc.
	genresByArtist, artistStats, err := c.resolveArtistGenres(ctx, page)
	res.ArtistsFetched = artistStats.fetched
	res.ArtistsWritten = artistStats.written
	res.Tombstoned += artistStats.tombstoned
	if err != nil {
		// Deliberately not fatal. Failing here would leave the plays unrecorded, and the
		// endpoint retains only ~50, so the window can roll and lose them permanently.
		// Recording with incomplete genres is recoverable: the nightly reconcile
		// recomputes genre aggregates from the raw plays. Choose the recoverable failure.
		res.GenresDegraded = true
		c.log.ErrorContext(ctx, "ingest: artist genre resolution failed; recording plays with "+
			"incomplete genre attribution, which the nightly reconcile will repair", "err", err)
	}

	// 6. Record plays, oldest first, so a partial failure still leaves the cursor behind
	// the last successful write.
	for _, p := range page.Plays {
		genres := genresFor(p, genresByArtist)
		out, rerr := c.store.RecordPlay(ctx, p, genres)
		if rerr != nil {
			return res, fmt.Errorf("ingest: record play %s at %s: %w",
				p.TrackID, model.FormatTS(p.PlayedAt), rerr)
		}
		if out.Inserted {
			res.Inserted++
			res.DeltasApplied += out.DeltasApplied
		} else {
			res.Duplicates++
		}
	}

	// 7. Write track and album metadata straight from the payload -- no extra API calls,
	// because recently-played embeds both.
	tracks, albums, terr := c.writeEmbeddedMetadata(ctx, page)
	res.TracksWritten = tracks
	res.AlbumsWritten = albums
	if terr != nil {
		// Metadata is display-only and never feeds an aggregate, so a failure here must not
		// discard a successful ingest. The staleness check picks it up next run.
		c.log.WarnContext(ctx, "ingest: writing embedded metadata failed; display names may "+
			"be missing until the next run", "err", terr)
	}

	// 8. Advance the cursor LAST.
	return res, c.finish(ctx, &res, cursor, runAt, page)
}

// finish advances the poll cursor. Nothing after this point may fail in a way that would
// make the cursor a lie.
func (c *Capturer) finish(
	ctx context.Context, res *Result, prev model.PollCursor,
	runAt time.Time, page spotify.RecentlyPlayedPage,
) error {
	next := prev
	next.LastRunAt = runAt
	next.LastStatus = "ok"
	if res.GenresDegraded {
		next.LastStatus = "ok-degraded-genres"
	}

	// Only ever move forward. Spotify's ordering guarantees are unstated, so a page whose
	// newest item predates the stored cursor must not rewind it.
	if !page.NewestPlayedAt.IsZero() && page.NewestPlayedAt.After(prev.LastPlayedAt) {
		next.LastPlayedAt = page.NewestPlayedAt
	}

	if err := c.store.PutPollCursor(ctx, next); err != nil {
		return fmt.Errorf("ingest: advance poll cursor: %w", err)
	}
	res.CursorAdvancedTo = next.LastPlayedAt
	return nil
}

type enrichStats struct {
	fetched    int
	written    int
	tombstoned int
}

// resolveArtistGenres returns genres keyed by artist ID for every artist on the page,
// fetching and persisting any artist that is unknown or stale.
//
// Artists are the only entity needing an API call during capture: Spotify exposes genres on
// the full artist object and nowhere else -- not on the track, and not on the simplified
// artist objects embedded in one.
func (c *Capturer) resolveArtistGenres(
	ctx context.Context, page spotify.RecentlyPlayedPage,
) (map[string][]string, enrichStats, error) {
	var stats enrichStats

	ids := artistIDsOn(page)
	if len(ids) == 0 {
		return map[string][]string{}, stats, nil
	}

	existing, err := c.store.GetArtists(ctx, ids)
	if err != nil {
		return map[string][]string{}, stats, fmt.Errorf("read artists: %w", err)
	}

	genres := make(map[string][]string, len(ids))
	var needed []string
	for _, id := range ids {
		a, ok := existing[id]
		switch {
		case !ok:
			needed = append(needed, id)
		case a.Missing:
			// A tombstoned artist can never resolve; do not ask again.
		case c.store.IsStale(a.RefreshedAt):
			needed = append(needed, id)
			// Use the stale genres meanwhile rather than dropping attribution.
			genres[id] = a.Genres
		default:
			genres[id] = a.Genres
		}
	}

	if len(needed) == 0 {
		return genres, stats, nil
	}

	fetched, missing, err := c.api.Artists(ctx, needed)
	if err != nil {
		// Return what is already known so the caller can proceed in a degraded mode.
		return genres, stats, fmt.Errorf("fetch %d artists: %w", len(needed), err)
	}
	stats.fetched = len(fetched)

	for _, a := range fetched {
		if err := c.store.PutArtist(ctx, a); err != nil {
			return genres, stats, fmt.Errorf("write artist %s: %w", a.ID, err)
		}
		stats.written++
		genres[a.ID] = a.Genres
	}

	// Tombstone anything Spotify returned null for, or the enrichment pass would re-request
	// the same dead IDs on every run forever.
	for _, id := range missing {
		if err := c.store.PutMissing(ctx, model.DimArtist, id); err != nil {
			return genres, stats, fmt.Errorf("tombstone artist %s: %w", id, err)
		}
		stats.tombstoned++
		c.log.InfoContext(ctx, "ingest: artist unresolvable, tombstoned", "artistId", id)
	}

	return genres, stats, nil
}

// writeEmbeddedMetadata persists the track and album objects the recently-played payload
// already carried, for anything unknown or stale. It costs no API calls.
func (c *Capturer) writeEmbeddedMetadata(
	ctx context.Context, page spotify.RecentlyPlayedPage,
) (tracks, albums int, err error) {
	trackIDs := make([]string, 0, len(page.Tracks))
	for id := range page.Tracks {
		trackIDs = append(trackIDs, id)
	}
	known, err := c.store.GetTracks(ctx, trackIDs)
	if err != nil {
		return 0, 0, fmt.Errorf("read tracks: %w", err)
	}
	for id, t := range page.Tracks {
		if cur, ok := known[id]; ok && !cur.Missing && !c.store.IsStale(cur.RefreshedAt) {
			continue
		}
		if err := c.store.PutTrack(ctx, t); err != nil {
			return tracks, albums, fmt.Errorf("write track %s: %w", id, err)
		}
		tracks++
	}

	albumIDs := make([]string, 0, len(page.Albums))
	for id := range page.Albums {
		albumIDs = append(albumIDs, id)
	}
	knownAlbums, err := c.store.GetAlbums(ctx, albumIDs)
	if err != nil {
		return tracks, albums, fmt.Errorf("read albums: %w", err)
	}
	for id, a := range page.Albums {
		if cur, ok := knownAlbums[id]; ok && !cur.Missing && !c.store.IsStale(cur.RefreshedAt) {
			continue
		}
		if err := c.store.PutAlbum(ctx, a); err != nil {
			return tracks, albums, fmt.Errorf("write album %s: %w", id, err)
		}
		albums++
	}

	return tracks, albums, nil
}

// artistIDsOn returns every distinct artist ID on the page, in first-seen order.
func artistIDsOn(page spotify.RecentlyPlayedPage) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, p := range page.Plays {
		for _, id := range p.ArtistIDs {
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// genresFor concatenates the genres of a play's artists. Deduplication and normalisation
// happen in model.FactsFor, which store.RecordPlay calls.
func genresFor(p model.Play, byArtist map[string][]string) []string {
	var out []string
	for _, id := range p.ArtistIDs {
		out = append(out, byArtist[id]...)
	}
	return out
}
