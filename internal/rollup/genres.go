package rollup

import (
	"context"

	"github.com/neovasili/spotistats/internal/store"
)

// genreCache resolves artist genres, caching across a run.
//
// A reconcile replays every play in the window, and the same handful of artists recur
// constantly, so an uncached lookup would dominate the run. Misses are cached too: an artist
// with no row must not be re-requested on every one of its plays.
type genreCache struct {
	store *store.Store
	byID  map[string][]string
}

func newGenreCache(s *store.Store) *genreCache {
	return &genreCache{store: s, byID: map[string][]string{}}
}

// For returns the concatenated genres of the given artists. Deduplication and normalisation
// happen in model.FactsFor.
func (c *genreCache) For(ctx context.Context, artistIDs []string) ([]string, error) {
	var missing []string
	for _, id := range artistIDs {
		if _, ok := c.byID[id]; !ok {
			missing = append(missing, id)
		}
	}

	var err error
	if len(missing) > 0 {
		found, ferr := c.store.GetArtists(ctx, missing)
		if ferr != nil {
			err = ferr
		}
		for _, id := range missing {
			// Cache the absence as well, so an unenriched artist is looked up once per run
			// rather than once per play.
			c.byID[id] = found[id].Genres
		}
	}

	var out []string
	for _, id := range artistIDs {
		out = append(out, c.byID[id]...)
	}
	return out, err
}

// storePlayFilter reads every source. A reconcile must see api and export rows alike, since the
// aggregate it is repairing counts both.
func storePlayFilter() store.PlayFilter { return store.PlayFilter{} }
