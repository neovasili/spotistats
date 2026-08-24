package rollup

import (
	"context"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// genreCache resolves artist genres, caching across a run.
//
// A reconcile replays every play in the window, and the same handful of artists recur
// constantly, so an uncached lookup would dominate the run. Misses are cached too: an artist
// with no row must not be re-requested on every one of its plays.
//
// # The genres are MusicBrainz's, not Spotify's
//
// They used to come from ARTIST#/META.genres, which is Spotify's own tagging. Spotify removed
// that field in February 2026, and as of this change EVERY artist row in production carries an
// empty genre list -- verified, all 229 of them -- so the genre charts had nothing to draw and
// said so.
//
// MusicBrainz genres now feed them instead, read from the ARTIST#/EXTERNAL rows that §4.5
// enrichment writes. Note what this is NOT: it is not a merge of two taxonomies. There is no
// second taxonomy left to merge with, which is exactly why the rule against merging them (see
// model.ArtistProfile.MBGenres) does not apply here. If Spotify ever restores its field, that
// question comes back and this decision must be revisited rather than extended.
//
// Coverage is materially lower than Spotify's was, because MusicBrainz resolves through the
// Spotify URL relationship and a name-keyed artist has no Spotify ID to relate. The reconcile
// already measures this per play as CoverageRow.MsWithGenre, and the dashboard states the
// figure rather than implying the chart is complete.
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
			// A name-keyed artist has no Spotify ID, so no MusicBrainz link and no EXTERNAL
			// row can exist for it. Caching the miss without asking saves a round trip on the
			// 3,190 such artists a full pass over the imported history walks.
			if model.IsNameKey(id) {
				c.byID[id] = nil
				continue
			}
			missing = append(missing, id)
		}
	}

	var err error
	if len(missing) > 0 {
		found, ferr := c.store.GetArtistProfiles(ctx, missing)
		if ferr != nil {
			err = ferr
		}
		for _, id := range missing {
			// Cache the absence as well, so an unenriched artist is looked up once per run
			// rather than once per play.
			c.byID[id] = found[id].MBGenres
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

// trackCache resolves track dimension rows, caching across a run.
//
// It exists so artist and album attribution can be resolved LATE, at reconcile time, instead of
// being baked into each play row at import time. The GDPR export supplies no artist or album ID,
// and resolving thirteen thousand tracks through the API is a weeks-long job under a
// development-mode quota — so a reconcile reads the track row as it stands now, and picks up
// real Spotify IDs the moment enrichment writes them, with no reimport of 400,000 plays.
//
// Misses are cached: an unresolved track must not be re-read on every one of its plays.
type trackCache struct {
	store *store.Store
	byID  map[string]model.Track
}

func newTrackCache(s *store.Store) *trackCache {
	return &trackCache{store: s, byID: map[string]model.Track{}}
}

// Warm batch-loads the given track IDs.
//
// Without this the cache issues one round trip per distinct track, and a full-history pass
// touches thirteen thousand of them: about seven minutes of pure latency, inside a job that
// runs under a fifteen-minute Lambda timeout. Batched, it is a hundred and thirty requests.
func (c *trackCache) Warm(ctx context.Context, ids []string) error {
	const batch = 100 // DynamoDB's BatchGetItem key limit, which store.GetTracks chunks by
	pending := make([]string, 0, batch)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		got, err := c.store.GetTracks(ctx, pending)
		if err != nil {
			return err
		}
		for _, id := range pending {
			// Misses are cached too: an unresolved track must not be re-requested for every
			// one of its plays.
			c.byID[id] = got[id]
		}
		pending = pending[:0]
		return nil
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := c.byID[id]; ok {
			continue
		}
		pending = append(pending, id)
		if len(pending) >= batch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// Loaded exposes the rows resolved so far, for building the canonicaliser index.
func (c *trackCache) Loaded() map[string]model.Track { return c.byID }

// For returns the track row for id, zero-valued when there is none.
func (c *trackCache) For(ctx context.Context, id string) (model.Track, error) {
	if id == "" {
		return model.Track{}, nil
	}
	if t, ok := c.byID[id]; ok {
		return t, nil
	}
	got, err := c.store.GetTracks(ctx, []string{id})
	if err != nil {
		// Cache nothing on error: a transient failure must not become a permanent
		// "unattributed" verdict for the rest of the run.
		return model.Track{}, err
	}
	t := got[id]
	c.byID[id] = t
	return t, nil
}
