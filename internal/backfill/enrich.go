package backfill

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/spotify"
	"github.com/neovasili/spotistats/internal/store"
)

// SpotifyAPI is the slice of the Spotify client the enricher needs.
//
// TrackDetail rather than Track: one request must yield the album and artist names too, or the
// pass would need three requests per track for data that already arrived with the first.
type SpotifyAPI interface {
	TrackDetail(ctx context.Context, id string) (spotify.TrackDetail, bool, error)
}

// EnrichStats reports what one enrich pass did.
type EnrichStats struct {
	UniqueTracks int
	AlreadyKnown int
	Fetched      int
	Unresolvable int
	TracksWritten,
	AlbumsWritten,
	ArtistsWritten int
	// Remaining is how many tracks were still unresolved when the pass stopped, whether
	// because of the deadline, a --limit, or a hard API failure. Non-zero means "run it again".
	Remaining int
}

// Enricher resolves the artist and album identity the export omits.
//
// # Why this phase has to exist
//
// Every aggregate is keyed by Spotify ID (docs/SPECS.md 5.2). The export gives a
// spotify_track_uri but names artists and albums only as free text, so without resolution the
// import could produce TRACK and TOTAL figures and nothing else -- no top artists, no top
// albums, across seventeen years. Inventing IDs from the names is not an option: it would fork
// the ID space, so the same artist would exist twice, once from the export and once from
// capture, and no query would ever join them.
//
// One GET /v1/tracks/{id} answers all of it at once. The full track object embeds its album
// (with ID, name and images) and its artists (with IDs and names), so a single request per
// unique track populates all three dimensions -- 14k requests rather than 14k + albums +
// artists. The batch endpoints that would have made this cheap were removed in February 2026
// (docs/SPECS.md 2.3).
//
// # Resumability
//
// There is no cursor row. The dimension rows ARE the cursor: a pass begins by reading which
// track IDs already exist and skips them. That makes an interrupted run resumable by simply
// running it again, with no state to go stale, and it means a partial failure costs only the
// requests not yet made.
type Enricher struct {
	store *store.Store
	api   SpotifyAPI
	log   *slog.Logger
}

func NewEnricher(st *store.Store, api SpotifyAPI, log *slog.Logger) *Enricher {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Enricher{store: st, api: api, log: log}
}

// Enrich resolves up to limit unresolved tracks (limit <= 0 means all of them).
//
// progress is called periodically so a caller can report a live count; an hour-long pass with
// no output is indistinguishable from a hang.
func (e *Enricher) Enrich(
	ctx context.Context, trackIDs []string, limit int, progress func(done, total int),
) (EnrichStats, error) {
	stats := EnrichStats{UniqueTracks: len(trackIDs)}

	todo, err := e.unresolved(ctx, trackIDs)
	if err != nil {
		return stats, err
	}
	stats.AlreadyKnown = len(trackIDs) - len(todo)
	if limit > 0 && len(todo) > limit {
		stats.Remaining = len(todo) - limit
		todo = todo[:limit]
	}

	seenAlbum := map[string]bool{}
	seenArtist := map[string]bool{}

	for i, id := range todo {
		if err := ctx.Err(); err != nil {
			// Deadline or interrupt: report what is left rather than pretending completion.
			stats.Remaining += len(todo) - i
			return stats, err
		}

		detail, found, err := e.api.TrackDetail(ctx, id)
		if err != nil {
			stats.Remaining += len(todo) - i
			return stats, fmt.Errorf("backfill: resolve track %s: %w", id, err)
		}
		stats.Fetched++
		if !found {
			// A track removed from the catalogue can never resolve. Tombstone it so a rerun
			// does not ask again; its plays still count towards TOTAL, and towards TRACK under
			// the raw ID.
			if err := e.store.PutMissing(ctx, model.DimTrack, id); err != nil {
				return stats, fmt.Errorf("backfill: tombstone track %s: %w", id, err)
			}
			stats.Unresolvable++
			continue
		}

		if err := e.store.PutTrack(ctx, detail.Track); err != nil {
			return stats, fmt.Errorf("backfill: write track %s: %w", id, err)
		}
		stats.TracksWritten++

		// The album and artists arrive embedded in the same response, so persisting them costs
		// nothing beyond the write. Deduplicated in-process because a few thousand albums back
		// fourteen thousand tracks.
		if al := detail.Album; al.ID != "" && !seenAlbum[al.ID] {
			seenAlbum[al.ID] = true
			if err := e.store.PutAlbum(ctx, al); err != nil {
				return stats, fmt.Errorf("backfill: write album %s: %w", al.ID, err)
			}
			stats.AlbumsWritten++
		}
		for _, ar := range detail.Artists {
			if ar.ID == "" || seenArtist[ar.ID] {
				continue
			}
			seenArtist[ar.ID] = true
			// A name-only stub: the embedded artist object has no genres, and PutArtistName
			// deliberately leaves enrichedAt unset so a later pass can still fill them in.
			// Since Spotify removed genres entirely (docs/SPECS.md 2.3) nothing is lost today.
			if err := e.store.PutArtistName(ctx, ar.ID, ar.Name); err != nil {
				return stats, fmt.Errorf("backfill: write artist %s: %w", ar.ID, err)
			}
			stats.ArtistsWritten++
		}

		if progress != nil && (i+1)%100 == 0 {
			progress(i+1, len(todo))
		}
	}
	if progress != nil {
		progress(len(todo), len(todo))
	}
	return stats, nil
}

// unresolved returns the track IDs with no usable dimension row yet, preserving input order so
// a resumed run makes progress in the same direction.
func (e *Enricher) unresolved(ctx context.Context, ids []string) ([]string, error) {
	known, err := e.store.GetTracks(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("backfill: read known tracks: %w", err)
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		t, ok := known[id]
		switch {
		case !ok:
			out = append(out, id)
		case t.Missing:
			// Tombstoned: asking again would burn a request on a permanently dead ID.
		case t.Name == "":
			// A row with no name is not usable for display; treat it as unresolved.
			out = append(out, id)
		case hasNameKeyedAttribution(t):
			// A placeholder written by the importer: it has a display name from the export but
			// only name-derived artist and album keys. Treating it as resolved would leave the
			// track permanently on fallback identity, which is exactly the state enrichment
			// exists to escape.
			out = append(out, id)
		}
	}
	return out, nil
}

// hasNameKeyedAttribution reports whether a track's artist or album identity is name-derived
// rather than a real Spotify ID.
func hasNameKeyedAttribution(t model.Track) bool {
	if model.IsNameKey(t.AlbumID) {
		return true
	}
	for _, id := range t.ArtistIDs {
		if model.IsNameKey(id) {
			return true
		}
	}
	// No attribution at all also counts: nothing to upgrade from, everything to gain.
	return t.AlbumID == "" && len(t.ArtistIDs) == 0
}

// EstimateEnrichDuration is a rough wall-clock estimate for the enrich phase, used only to tell
// the operator what they are committing to before an hour-long pass starts.
func EstimateEnrichDuration(tracks int, perRequest time.Duration) time.Duration {
	if perRequest <= 0 {
		perRequest = 250 * time.Millisecond
	}
	return time.Duration(tracks) * perRequest
}
