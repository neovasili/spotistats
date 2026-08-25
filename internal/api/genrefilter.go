package api

import (
	"context"
	"slices"
	"strings"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// genreMatch is how a multi-genre selection combines.
type genreMatch string

const (
	// matchAny keeps an entity carrying at least one of the selected genres.
	matchAny genreMatch = "any"
	// matchAll keeps only entities carrying every selected genre.
	matchAll genreMatch = "all"
)

// maxGenreFilters bounds a single request's selection.
//
// The whole vocabulary is 350 tags, and a selection past a handful stops being a filter: under
// "any" it converges on the unfiltered list, and under "all" it is empty long before this. The
// cap exists so a hand-written URL cannot turn one request into an unbounded string comparison
// across every entity in the partition.
const maxGenreFilters = 25

// genreFilter is a parsed genre selection.
type genreFilter struct {
	// Normalised through model.NormalizeGenre, deduplicated, sorted. Sorted so the value takes
	// part in the response cursor's fingerprint identically however the client ordered it --
	// otherwise "rock,metal" and "metal,rock" would be two different paginations of one list.
	genres []string
	match  genreMatch
}

func (f genreFilter) active() bool { return len(f.genres) > 0 }

// fingerprint contributes the filter to the pagination fingerprint.
func (f genreFilter) fingerprint() string {
	if !f.active() {
		return ""
	}
	return string(f.match) + ":" + strings.Join(f.genres, ",")
}

// genreFilterParams parses `genres` and `genreMatch`.
//
// Comma-separated rather than a repeated parameter: every genre in the vocabulary was checked
// and none contains a comma, so the simpler encoding is unambiguous here, and it keeps the
// shareable URL readable (`genres=power+metal,symphonic+metal`).
func (p *params) genreFilter() genreFilter {
	raw := p.str("genres", "")
	match := genreMatch(strings.ToLower(p.str("genreMatch", string(matchAny))))
	switch match {
	case matchAny, matchAll:
	default:
		p.fail(badRequest(CodeInvalidParameter,
			"genreMatch must be any or all; got %q", match))
	}

	if raw == "" {
		return genreFilter{match: match}
	}

	seen := make(map[string]struct{})
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		g := model.NormalizeGenre(part)
		if g == "" {
			continue
		}
		if _, dup := seen[g]; dup {
			continue
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	if len(out) > maxGenreFilters {
		p.fail(badRequest(CodeInvalidParameter,
			"genres accepts at most %d values; got %d", maxGenreFilters, len(out)))
		return genreFilter{match: match}
	}
	slices.Sort(out)
	return genreFilter{genres: out, match: match}
}

// filterByGenre keeps the aggregates whose entity carries the selected genres.
//
// # Genres belong to artists, not to tracks
//
// Nothing in the data tags a track or an album with a genre: MusicBrainz labels the ARTIST, and
// the genre leaderboards are built by attributing each play to every genre its artists carry
// (see internal/rollup/genres.go). So a track matches when one of its credited artists matches,
// and that is the only mapping available rather than a choice between several. The response
// caveat says so, because a reader filtering tracks by "power metal" is really asking for tracks
// by power-metal artists, and those are not the same question.
//
// # Cost
//
// One batch pass over the distinct artists in the result set -- about 3,400 for this archive, so
// 35 BatchGetItem calls -- and only when a filter is active. The labels have already been
// resolved by this point, which is what makes the credit lists free; without them this would be
// a second full read of every track row.
//
// An entity whose artists have no EXTERNAL row matches nothing. That is correct rather than
// unfortunate: it has no known genre, and silently keeping it would make a filtered list read as
// complete when 16% of listening time carries no genre at all.
func (h *Handler) filterByGenre(
	ctx context.Context,
	aggs []model.Aggregate,
	labels map[string]store.Label,
	f genreFilter,
) ([]model.Aggregate, error) {
	if !f.active() {
		return aggs, nil
	}

	// Distinct artists across the whole result set, in one pass.
	ids := make([]string, 0, len(aggs))
	seen := make(map[string]struct{}, len(aggs))
	for _, a := range aggs {
		for _, id := range labels[a.Key.EntityID].ArtistIDs {
			// A name-keyed artist has no Spotify ID, so no MusicBrainz link and no EXTERNAL
			// row can exist. Skipping them here saves the round trips rather than asking for
			// rows that cannot be there.
			if id == "" || model.IsNameKey(id) {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}

	profiles, err := h.store.GetArtistProfiles(ctx, ids)
	if err != nil {
		return nil, err
	}

	// Normalised once per artist, not once per comparison.
	byArtist := make(map[string]map[string]struct{}, len(profiles))
	for id, p := range profiles {
		set := make(map[string]struct{}, len(p.MBGenres))
		for _, g := range p.MBGenres {
			if n := model.NormalizeGenre(g); n != "" {
				set[n] = struct{}{}
			}
		}
		byArtist[id] = set
	}

	out := make([]model.Aggregate, 0, len(aggs))
	for _, a := range aggs {
		if matchesGenres(labels[a.Key.EntityID].ArtistIDs, byArtist, f) {
			out = append(out, a)
		}
	}
	return out, nil
}

// matchesGenres tests one entity's credited artists against the filter.
//
// The union of every credited artist's genres is what is tested, not each artist separately. A
// collaboration between a power-metal act and a symphonic-metal one satisfies "all: power metal,
// symphonic metal" — the track really does carry both labels, even though neither artist does
// alone.
func matchesGenres(
	artistIDs []string,
	byArtist map[string]map[string]struct{},
	f genreFilter,
) bool {
	union := make(map[string]struct{})
	for _, id := range artistIDs {
		for g := range byArtist[id] {
			union[g] = struct{}{}
		}
	}
	if len(union) == 0 {
		return false
	}

	for _, want := range f.genres {
		_, ok := union[want]
		switch {
		case ok && f.match == matchAny:
			return true
		case !ok && f.match == matchAll:
			return false
		}
	}
	// Ran the whole selection without an early exit: under "all" everything matched, under
	// "any" nothing did.
	return f.match == matchAll
}
