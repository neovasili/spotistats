package spotify

import (
	"context"
	"net/url"

	"github.com/neovasili/spotistats/internal/model"
)

// The per-resource multi-get ceilings (tracks and artists 50, albums 20) that used to live
// here are deliberately gone: Spotify removed every batch multi-get for Development Mode apps
// in the February 2026 Web API change, so a batch size is no longer a meaningful quantity.
// Keeping the constants would have described a constraint that no longer exists.

// Chunk splits s into consecutive slices of at most n elements. It panics if n <= 0.
func Chunk[T any](s []T, n int) [][]T {
	if n <= 0 {
		panic("spotify: Chunk requires n > 0")
	}
	if len(s) == 0 {
		return nil
	}
	out := make([][]T, 0, (len(s)+n-1)/n)
	for i := 0; i < len(s); i += n {
		end := min(i+n, len(s))
		out = append(out, s[i:end])
	}
	return out
}

// dedupeIDs removes blanks and duplicates, preserving first-seen order so request
// composition is deterministic and assertable.
func dedupeIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// fanOut resolves ids one at a time through get, returning what succeeded, the ids the API
// reported as unresolvable, and the first error encountered.
//
// It exists because Spotify REMOVED every batch multi-get -- Get Several Artists, Tracks,
// Albums, Shows, Episodes -- for Development Mode apps in the February 2026 Web API change
// (migrated 2026-03-09). Calling GET /v1/artists now returns 403 Forbidden with no useful
// body, which is exactly how it presented in production: artist enrichment failed wholesale
// and every artist rendered as a raw Spotify ID. The single-item endpoints are unaffected.
//
// The cost is one request per entity instead of one per 50. Two things make that acceptable
// here: only entities that are new or unenriched are ever fetched, so the steady state is a
// handful per run, and callers cap how many they attempt per run.
//
// Partial progress is deliberately preserved: a failure part-way through returns everything
// resolved so far ALONGSIDE the error, so each run makes forward progress instead of
// discarding the work. The batch version was all-or-nothing, which is what turned one 403
// into zero artist rows.
func fanOut[T any](
	ctx context.Context, ids []string, get func(context.Context, string) (T, bool, error),
) ([]T, []string, error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	found := make([]T, 0, len(ids))
	var missing []string
	for _, id := range ids {
		v, ok, err := get(ctx, id)
		if err != nil {
			return found, missing, err
		}
		if !ok {
			missing = append(missing, id)
			continue
		}
		found = append(found, v)
	}
	return found, missing, nil
}

// Track resolves one track. GET /v1/tracks/{id} survived the batch-endpoint removal.
func (c *Client) Track(ctx context.Context, id string) (model.Track, bool, error) {
	var wire dtoTrack
	if err := c.get(ctx, "tracks/"+url.PathEscape(id), nil, &wire); err != nil {
		if notFound(err) {
			return model.Track{}, false, nil
		}
		return model.Track{}, false, err
	}
	if wire.ID == "" {
		return model.Track{}, false, nil
	}
	return wire.toModel(), true, nil
}

// Tracks resolves track metadata one request per ID; see fanOut.
//
// The second return value lists IDs Spotify could not resolve: removed, relinked or invalid.
// Callers MUST record these (as tombstone rows) or the enrichment pass will re-request the
// same dead IDs on every run, forever.
func (c *Client) Tracks(ctx context.Context, ids []string) ([]model.Track, []string, error) {
	return fanOut(ctx, dedupeIDs(ids), c.Track)
}

// Artist resolves one artist, including genres.
//
// Genres exist only on the artist object, never on the simplified artist embedded in a track.
// Spotify marks the field deprecated as of the February 2026 change but still returns it;
// `followers` is now always null and `popularity` is deprecated, so both are best-effort.
func (c *Client) Artist(ctx context.Context, id string) (model.Artist, bool, error) {
	var wire dtoArtist
	if err := c.get(ctx, "artists/"+url.PathEscape(id), nil, &wire); err != nil {
		if notFound(err) {
			return model.Artist{}, false, nil
		}
		return model.Artist{}, false, err
	}
	if wire.ID == "" {
		return model.Artist{}, false, nil
	}
	return wire.toModel(), true, nil
}

// Artists resolves artist metadata one request per ID; see fanOut.
func (c *Client) Artists(ctx context.Context, ids []string) ([]model.Artist, []string, error) {
	return fanOut(ctx, dedupeIDs(ids), c.Artist)
}

// Album resolves one album.
func (c *Client) Album(ctx context.Context, id string) (model.Album, bool, error) {
	var wire dtoAlbum
	if err := c.get(ctx, "albums/"+url.PathEscape(id), nil, &wire); err != nil {
		if notFound(err) {
			return model.Album{}, false, nil
		}
		return model.Album{}, false, err
	}
	if wire.ID == "" {
		return model.Album{}, false, nil
	}
	return wire.toModel(), true, nil
}

// Albums resolves album metadata one request per ID; see fanOut.
func (c *Client) Albums(ctx context.Context, ids []string) ([]model.Album, []string, error) {
	return fanOut(ctx, dedupeIDs(ids), c.Album)
}
