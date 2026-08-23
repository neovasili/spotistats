package rollup

import (
	"context"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// canonicaliser collapses name-derived entity IDs onto real Spotify IDs where one is known.
//
// # The bug this exists to fix
//
// Identity is resolved per TRACK: an enriched track contributes its plays under real Spotify
// IDs, an unenriched one under name keys. For an artist whose catalogue is only partly enriched
// -- which is every artist, while enrichment drips through a rate-limited API -- that splits the
// history in two. It shipped exactly that way: "Disturbed" appeared twice on the dashboard, at
// 1,656h under `nm:disturbed` and 554h under `3TOqt5oJwL9BE2NG9MEwDa`, when the truth was the
// sum of the pair.
//
// So both paths are made to converge on the real ID whenever one exists for that name, and on
// the name key otherwise. Entities the API has never resolved keep their name keys, which is the
// point: they still aggregate correctly, just without a Spotify identity.
type canonicaliser struct {
	artists map[string]string // artist name key -> real Spotify ID
	albums  map[string]string // album name key  -> real Spotify ID
}

// newCanonicaliser builds the index from the resolved tracks a run has already loaded.
//
// Only artists and albums reachable from a resolved track can fork, so those are exactly the
// ones that need indexing -- no table scan required.
func newCanonicaliser(
	ctx context.Context, st *store.Store, tracks map[string]model.Track,
) (*canonicaliser, error) {
	c := &canonicaliser{artists: map[string]string{}, albums: map[string]string{}}

	artistIDs := map[string]bool{}
	albumIDs := map[string]bool{}
	for _, t := range tracks {
		for _, id := range t.ArtistIDs {
			if id != "" && !model.IsNameKey(id) {
				artistIDs[id] = true
			}
		}
		if t.AlbumID != "" && !model.IsNameKey(t.AlbumID) {
			albumIDs[t.AlbumID] = true
		}
	}
	if len(artistIDs) == 0 && len(albumIDs) == 0 {
		return c, nil
	}

	artists, err := st.GetArtists(ctx, keysOf(artistIDs))
	if err != nil {
		return nil, err
	}
	for _, a := range artists {
		if a.Name == "" || a.Missing {
			continue
		}
		// First writer wins, so the mapping is stable across runs regardless of map order.
		if k := model.NameKey(a.Name); k != "" {
			if _, seen := c.artists[k]; !seen {
				c.artists[k] = a.ID
			}
		}
	}

	albums, err := st.GetAlbums(ctx, keysOf(albumIDs))
	if err != nil {
		return nil, err
	}
	for _, al := range albums {
		if al.Name == "" || al.Missing {
			continue
		}
		// An album's fallback key folds in its artist, so reproducing it needs the artist's
		// NAME -- which is why the artist index is built first.
		artistName := ""
		if primary := store.PrimaryArtist(al.ArtistIDs); primary != "" {
			if a, ok := artists[primary]; ok {
				artistName = a.Name
			}
		}
		if k := model.AlbumNameKey(artistName, al.Name); k != "" {
			if _, seen := c.albums[k]; !seen {
				c.albums[k] = al.ID
			}
		}
	}
	return c, nil
}

// Apply rewrites any name-keyed artist or album in facts to its real Spotify ID when one is
// known, leaving genuinely unresolved entities on their name key.
func (c *canonicaliser) Apply(f model.PlayFacts) model.PlayFacts {
	if c == nil {
		return f
	}
	if len(f.ArtistIDs) > 0 {
		out := make([]string, 0, len(f.ArtistIDs))
		seen := make(map[string]bool, len(f.ArtistIDs))
		for _, id := range f.ArtistIDs {
			if real, ok := c.artists[id]; ok {
				id = real
			}
			// Converging two IDs onto one can create a duplicate within a single play, which
			// would double-count that play for the artist.
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		f.ArtistIDs = out
	}
	if real, ok := c.albums[f.AlbumID]; ok {
		f.AlbumID = real
	}
	return f
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
