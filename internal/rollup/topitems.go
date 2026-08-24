package rollup

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// TopItemsPerArtist is how many albums and tracks an artist's page shows of each.
const TopItemsPerArtist = 5

// artistTopAccumulator collects, per artist, how much of each of their albums and tracks was
// played.
//
// This is precomputed because nothing indexes it. Aggregate rows are keyed by the entity itself,
// so "the albums of this artist" is answerable only by streaming plays — which the nightly pass
// already does for coverage, and which a per-request handler must never do.
//
// Memory is bounded by DISTINCT PAIRS, not by plays: about thirteen thousand tracks and seven
// thousand albums across three thousand artists, so tens of thousands of small entries for a
// pass that already buffers four hundred thousand plays.
type artistTopAccumulator struct {
	albums map[string]map[string]*model.TopItem
	tracks map[string]map[string]*model.TopItem
}

func newArtistTopAccumulator() *artistTopAccumulator {
	return &artistTopAccumulator{
		albums: map[string]map[string]*model.TopItem{},
		tracks: map[string]map[string]*model.TopItem{},
	}
}

// Add attributes one play to every artist credited on it.
//
// Credited to ALL of them, not just the first: a collaboration is genuinely part of each
// artist's listening, and picking one would silently erase the track from the other's page.
// The consequence is that summing these across artists exceeds the total, exactly as the ARTIST
// aggregate already does.
func (a *artistTopAccumulator) Add(facts model.PlayFacts, trackName, albumName string) {
	for _, artistID := range facts.ArtistIDs {
		if artistID == "" {
			continue
		}
		if facts.AlbumID != "" {
			bump(a.albums, artistID, facts.AlbumID, albumName, "", facts)
		}
		if facts.TrackID != "" {
			// A track's context is its album, which is what makes two identically titled songs
			// on a live and a studio record distinguishable in the list.
			bump(a.tracks, artistID, facts.TrackID, trackName, albumName, facts)
		}
	}
}

func bump(
	into map[string]map[string]*model.TopItem,
	artistID, entityID, name, context string,
	facts model.PlayFacts,
) {
	byEntity, ok := into[artistID]
	if !ok {
		byEntity = map[string]*model.TopItem{}
		into[artistID] = byEntity
	}
	item, ok := byEntity[entityID]
	if !ok {
		item = &model.TopItem{ID: entityID}
		byEntity[entityID] = item
	}
	// The name is taken from whichever play carried one: a track resolved partway through the
	// history has a name on its later plays and none on its earlier ones.
	if item.Name == "" {
		item.Name = name
	}
	if item.Context == "" {
		item.Context = context
	}
	item.Plays++
	item.MsPlayed += facts.MsPlayed
}

// artistIDs lists every artist the accumulator saw.
func (a *artistTopAccumulator) artistIDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]map[string]*model.TopItem{a.albums, a.tracks} {
		for id := range m {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	slices.Sort(out) // deterministic write order, so a re-run is comparable
	return out
}

// top returns the highest-ranked entries for one artist.
func top(byEntity map[string]*model.TopItem, n int) []model.TopItem {
	out := make([]model.TopItem, 0, len(byEntity))
	for _, i := range byEntity {
		out = append(out, *i)
	}
	slices.SortFunc(out, func(x, y model.TopItem) int {
		if x.MsPlayed != y.MsPlayed {
			return int(y.MsPlayed - x.MsPlayed) // descending
		}
		// Ties broken by ID, so the list does not reshuffle between nightly runs for entries
		// that are genuinely equal.
		return strings.Compare(x.ID, y.ID)
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// writeArtistTopItems resolves display names and artwork, then stores one row per artist.
//
// Names are written down rather than resolved at read time because an artist profile is one item
// read by design; resolving ten names per request would make it eleven.
func (r *Rollup) writeArtistTopItems(ctx context.Context, acc *artistTopAccumulator) (int, error) {
	ids := acc.artistIDs()
	if len(ids) == 0 {
		return 0, nil
	}

	// Every album and track that will appear anywhere, so labels are resolved in batches rather
	// than one lookup per list entry.
	albumIDs := map[string]bool{}
	trackIDs := map[string]bool{}
	shortlist := make(map[string][2][]model.TopItem, len(ids))
	for _, artistID := range ids {
		albums := top(acc.albums[artistID], TopItemsPerArtist)
		tracks := top(acc.tracks[artistID], TopItemsPerArtist)
		shortlist[artistID] = [2][]model.TopItem{albums, tracks}
		for _, x := range albums {
			albumIDs[x.ID] = true
		}
		for _, x := range tracks {
			trackIDs[x.ID] = true
		}
	}

	albumLabels, err := r.store.ResolveLabels(ctx, model.DimAlbum, sortedKeys(albumIDs))
	if err != nil {
		// Labels are cosmetic; the figures are not. A lookup failure must not lose the ranking.
		r.log.WarnContext(ctx, "rollup: could not resolve album labels for artist top items",
			"err", err)
	}
	trackLabels, lerr := r.store.ResolveLabels(ctx, model.DimTrack, sortedKeys(trackIDs))
	if lerr != nil {
		r.log.WarnContext(ctx, "rollup: could not resolve track labels for artist top items",
			"err", lerr)
	}

	// Batched, not one write per artist: three thousand sequential PutItems added nearly four
	// minutes to a job that runs under a fifteen-minute Lambda timeout.
	rows := make([]model.ArtistTopItems, 0, len(ids))
	now := r.now()
	for _, artistID := range ids {
		lists := shortlist[artistID]
		rows = append(rows, model.ArtistTopItems{
			ArtistID:   artistID,
			Albums:     applyLabels(lists[0], albumLabels),
			Tracks:     applyLabels(lists[1], trackLabels),
			ComputedAt: now,
		})
	}
	if err := r.store.PutArtistTopItemsBatch(ctx, rows); err != nil {
		return 0, fmt.Errorf("rollup: write artist top items: %w", err)
	}
	return len(rows), nil
}

// applyLabels fills in names and artwork from resolved dimension rows, keeping whatever the
// accumulator already had when a row is missing.
func applyLabels(items []model.TopItem, labels map[string]store.Label) []model.TopItem {
	out := make([]model.TopItem, 0, len(items))
	for _, i := range items {
		if l, ok := labels[i.ID]; ok {
			if l.Name != "" {
				i.Name = l.Name
			}
			if l.ThumbURL != "" {
				i.ThumbURL = l.ThumbURL
			} else if l.ImageURL != "" {
				i.ThumbURL = l.ImageURL
			}
		}
		out = append(out, i)
	}
	return out
}

// sortedKeys returns a map's keys in a deterministic order, so batched reads and the rows they
// produce are comparable between runs.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
