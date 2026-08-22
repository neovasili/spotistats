package spotify

import (
	"context"
	"net/url"
	"strings"

	"github.com/neovasili/spotistats/internal/model"
)

// Multi-get maximums, which differ per resource. Albums allowing only 20 while tracks and
// artists allow 50 is the entire reason batching is configured per resource rather than
// with one shared constant -- getting it wrong means silent 400s on every album lookup.
const (
	MaxTrackIDs  = 50
	MaxArtistIDs = 50
	MaxAlbumIDs  = 20
)

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

// idsQuery builds the query for a multi-get.
//
// It deliberately does NOT set `market`. Passing a market triggers Spotify's track
// relinking, which returns a DIFFERENT track ID from the one requested -- silently
// forking the ID space so the same song accumulates statistics under two identities.
func idsQuery(ids []string) url.Values {
	return url.Values{"ids": {strings.Join(ids, ",")}}
}

// Tracks resolves track metadata.
//
// The second return value lists IDs Spotify returned null for: removed, relinked or
// invalid. Callers MUST record these (as tombstone rows) or the enrichment pass will
// re-request the same dead IDs on every run, forever.
func (c *Client) Tracks(ctx context.Context, ids []string) ([]model.Track, []string, error) {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return nil, nil, nil
	}
	found := make([]model.Track, 0, len(ids))
	var missing []string

	for _, batch := range Chunk(ids, MaxTrackIDs) {
		var wire dtoTracksResponse
		if err := c.get(ctx, "tracks", idsQuery(batch), &wire); err != nil {
			return nil, nil, err
		}
		for i, t := range wire.Tracks {
			if t == nil || t.ID == "" {
				missing = append(missing, batch[min(i, len(batch)-1)])
				continue
			}
			found = append(found, t.toModel())
		}
		// A short response means trailing entries were dropped rather than nulled.
		for i := len(wire.Tracks); i < len(batch); i++ {
			missing = append(missing, batch[i])
		}
	}
	return found, missing, nil
}

// Artists resolves artist metadata, including genres -- which exist only on this endpoint,
// never on the simplified artist objects embedded in tracks.
func (c *Client) Artists(ctx context.Context, ids []string) ([]model.Artist, []string, error) {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return nil, nil, nil
	}
	found := make([]model.Artist, 0, len(ids))
	var missing []string

	for _, batch := range Chunk(ids, MaxArtistIDs) {
		var wire dtoArtistsResponse
		if err := c.get(ctx, "artists", idsQuery(batch), &wire); err != nil {
			return nil, nil, err
		}
		for i, a := range wire.Artists {
			if a == nil || a.ID == "" {
				missing = append(missing, batch[min(i, len(batch)-1)])
				continue
			}
			found = append(found, a.toModel())
		}
		for i := len(wire.Artists); i < len(batch); i++ {
			missing = append(missing, batch[i])
		}
	}
	return found, missing, nil
}

// Albums resolves album metadata. Note the batch ceiling is 20, not 50.
func (c *Client) Albums(ctx context.Context, ids []string) ([]model.Album, []string, error) {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return nil, nil, nil
	}
	found := make([]model.Album, 0, len(ids))
	var missing []string

	for _, batch := range Chunk(ids, MaxAlbumIDs) {
		var wire dtoAlbumsResponse
		if err := c.get(ctx, "albums", idsQuery(batch), &wire); err != nil {
			return nil, nil, err
		}
		for i, a := range wire.Albums {
			if a == nil || a.ID == "" {
				missing = append(missing, batch[min(i, len(batch)-1)])
				continue
			}
			found = append(found, a.toModel())
		}
		for i := len(wire.Albums); i < len(batch); i++ {
			missing = append(missing, batch[i])
		}
	}
	return found, missing, nil
}
