package api

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// maxPartitionItems bounds a ranking read. A personal library is a few thousand entities per
// dimension, so this is generous; it exists so a pathological dataset degrades visibly via
// the Truncated flag rather than by timing the Lambda out.
const maxPartitionItems = 50_000

// Entry is one ranked or listed entity.
type Entry struct {
	Rank    int     `json:"rank"`
	ID      string  `json:"id"`
	Name    string  `json:"name,omitempty"`
	Metrics Metrics `json:"metrics"`
	First   *string `json:"firstPlayedAt,omitempty"`
	Last    *string `json:"lastPlayedAt,omitempty"`
}

// TopResponse is a ranked leaderboard.
type TopResponse struct {
	Dim    string  `json:"dim"`
	Period string  `json:"period"`
	Metric string  `json:"metric"`
	Total  Metrics `json:"total"`
	Items  []Entry `json:"items"`
	// Source records whether the ranking came from a precomputed leaderboard or was computed
	// on the fly, so a slow response has a visible explanation.
	Source string `json:"source"`
	// Truncated means the partition exceeded maxPartitionItems and the ranking may be
	// incomplete. Reported rather than silently dropped.
	Truncated bool `json:"truncated,omitempty"`
	// Caveat is present for dimensions whose totals cannot be compared to the overall total.
	Caveat string `json:"caveat,omitempty"`
}

func (h *Handler) handleTop(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r, "dim", "period", "limit", "metric")
	dim := p.dim("dim", true)
	period := p.period("period")
	limit := p.limit()
	m := p.metric(metricMs)
	if err := p.err(); err != nil {
		return err
	}
	ctx := r.Context()

	out := TopResponse{
		Dim: string(dim), Period: string(period), Metric: string(m),
		Caveat: dimensionCaveat(dim),
		Items:  []Entry{},
	}

	// Prefer the materialised leaderboard the nightly rollup writes: a single GetItem instead
	// of reading and sorting a whole partition. It is only usable when it ranks by the same
	// metric the caller asked for.
	if lb, err := h.store.GetLeaderboard(ctx, dim, period); err == nil && lb.Metric == string(m) {
		out.Source = "materialised"
		for i, e := range lb.Entries {
			if i >= limit {
				break
			}
			out.Items = append(out.Items, Entry{
				Rank: i + 1, ID: e.ID, Name: e.Name,
				Metrics: Metrics{Plays: e.Plays, MsPlayed: e.MsPlayed},
			})
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	if out.Source == "" {
		out.Source = "computed"
		aggs, truncated, err := h.readPartition(ctx, dim, period)
		if err != nil {
			return err
		}
		out.Truncated = truncated
		sortAggregates(aggs, m, true)
		if len(aggs) > limit {
			aggs = aggs[:limit]
		}
		out.Items = h.toEntries(ctx, dim, aggs, 0)
	}

	total, err := h.store.GetAggregateOrZero(ctx, model.AggKey{
		Dim: model.DimTotal, Period: period, EntityID: model.TotalEntityID,
	})
	if err != nil {
		return err
	}
	out.Total = metricsOf(total)

	writeJSON(w, r, h.log, out)
	return nil
}

// ListResponse is a paginated, sorted view of every entity in a dimension.
type ListResponse struct {
	Dim        string  `json:"dim"`
	Period     string  `json:"period"`
	Sort       string  `json:"sort"`
	Order      string  `json:"order"`
	Items      []Entry `json:"items"`
	NextCursor string  `json:"nextCursor,omitempty"`
	Total      int     `json:"total"`
	Truncated  bool    `json:"truncated,omitempty"`
	Caveat     string  `json:"caveat,omitempty"`
}

// handleList is the Explorer's backing endpoint.
//
// Ranking by listening time cannot be done by DynamoDB -- it orders by key, and the measure is
// an attribute -- so the partition is read and sorted here. That is why this endpoint's cursor
// encodes a position in the computed ordering rather than a LastEvaluatedKey; see cursor.go.
// Edge caching (§6.2) means the work happens at most once an hour per distinct query.
func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r, "dim", "period", "sort", "order", "limit", "cursor", "q")
	dim := p.dim("dim", true)
	period := p.period("period")
	limit := p.limit()
	desc := p.descending()
	query := p.str("q", "")

	sortBy := metric(p.str("sort", string(metricMs)))
	switch sortBy {
	case metricPlays, metricMs, metricMsExact, "name":
	default:
		p.fail(badRequest(CodeInvalidParameter,
			"sort must be plays, ms, msExact or name; got %q", sortBy))
	}
	if err := p.err(); err != nil {
		return err
	}

	ctx := r.Context()
	fp := fingerprint(string(dim), string(period), string(sortBy), boolStr(desc), query)
	offset, err := parseOffsetCursor(p.str("cursor", ""), fp)
	if err != nil {
		return err
	}

	aggs, truncated, err := h.readPartition(ctx, dim, period)
	if err != nil {
		return err
	}

	// Names are needed both for the optional text filter and for sorting by name, so resolve
	// them before either.
	names := h.resolveNames(ctx, dim, aggs)

	if query != "" {
		aggs = filterByName(aggs, names, query)
	}
	if sortBy == "name" {
		sortByName(aggs, names, desc)
	} else {
		sortAggregates(aggs, sortBy, desc)
	}

	out := ListResponse{
		Dim: string(dim), Period: string(period),
		Sort: string(sortBy), Order: orderStr(desc),
		Total: len(aggs), Truncated: truncated,
		Caveat: dimensionCaveat(dim),
		Items:  []Entry{},
	}

	if offset > len(aggs) {
		offset = len(aggs)
	}
	end := min(offset+limit, len(aggs))
	page := aggs[offset:end]
	out.Items = h.entriesWithNames(dim, page, names, offset)

	if end < len(aggs) {
		next, cerr := offsetCursor(fp, end)
		if cerr != nil {
			return cerr
		}
		out.NextCursor = next
	}

	writeJSON(w, r, h.log, out)
	return nil
}

// readPartition reads every aggregate row for a dimension and period.
func (h *Handler) readPartition(
	ctx context.Context, dim model.Dim, period model.Period,
) ([]model.Aggregate, bool, error) {
	var out []model.Aggregate
	for a, err := range h.store.QueryAggregates(ctx, dim, period, "") {
		if err != nil {
			return nil, false, err
		}
		// TOTAL rows share the year partition with day rows; entity dimensions never do, but
		// guard anyway so a stray row cannot appear as an entity.
		if a.Key.Dim != dim {
			continue
		}
		out = append(out, a)
		if len(out) >= maxPartitionItems {
			return out, true, nil
		}
	}
	return out, false, nil
}

// sortAggregates orders by a measure, breaking ties on entity ID so pagination is stable.
//
// A non-deterministic tie-break would let an entity appear on two consecutive pages or on
// neither, which looks like data loss to the client.
func sortAggregates(aggs []model.Aggregate, m metric, desc bool) {
	sort.SliceStable(aggs, func(i, j int) bool {
		vi, vj := m.value(aggs[i]), m.value(aggs[j])
		if vi != vj {
			if desc {
				return vi > vj
			}
			return vi < vj
		}
		return aggs[i].Key.EntityID < aggs[j].Key.EntityID
	})
}

func sortByName(aggs []model.Aggregate, names map[string]string, desc bool) {
	sort.SliceStable(aggs, func(i, j int) bool {
		ni := lowerASCII(names[aggs[i].Key.EntityID])
		nj := lowerASCII(names[aggs[j].Key.EntityID])
		if ni != nj {
			if desc {
				return ni > nj
			}
			return ni < nj
		}
		return aggs[i].Key.EntityID < aggs[j].Key.EntityID
	})
}

func filterByName(aggs []model.Aggregate, names map[string]string, query string) []model.Aggregate {
	q := lowerASCII(query)
	out := aggs[:0]
	for _, a := range aggs {
		if containsFold(names[a.Key.EntityID], q) || containsFold(a.Key.EntityID, q) {
			out = append(out, a)
		}
	}
	return out
}

// resolveNames batch-reads display names for a set of aggregates.
func (h *Handler) resolveNames(
	ctx context.Context, dim model.Dim, aggs []model.Aggregate,
) map[string]string {
	names := make(map[string]string, len(aggs))

	// A genre aggregate is keyed by the genre string, so it is its own name.
	if dim == model.DimGenre {
		for _, a := range aggs {
			names[a.Key.EntityID] = a.Key.EntityID
		}
		return names
	}

	ids := make([]string, 0, len(aggs))
	for _, a := range aggs {
		ids = append(ids, a.Key.EntityID)
	}

	switch dim {
	case model.DimTrack:
		if m, err := h.store.GetTracks(ctx, ids); err == nil {
			for id, t := range m {
				names[id] = t.Name
			}
		}
	case model.DimArtist:
		if m, err := h.store.GetArtists(ctx, ids); err == nil {
			for id, a := range m {
				names[id] = a.Name
			}
		}
	case model.DimAlbum:
		if m, err := h.store.GetAlbums(ctx, ids); err == nil {
			for id, a := range m {
				names[id] = a.Name
			}
		}
	}
	// A missing name is not an error: the dimension row may not be enriched yet. The client
	// falls back to the ID.
	return names
}

func (h *Handler) toEntries(
	ctx context.Context, dim model.Dim, aggs []model.Aggregate, offset int,
) []Entry {
	return h.entriesWithNames(dim, aggs, h.resolveNames(ctx, dim, aggs), offset)
}

func (h *Handler) entriesWithNames(
	_ model.Dim, aggs []model.Aggregate, names map[string]string, offset int,
) []Entry {
	out := make([]Entry, 0, len(aggs))
	for i, a := range aggs {
		out = append(out, Entry{
			Rank:    offset + i + 1,
			ID:      a.Key.EntityID,
			Name:    names[a.Key.EntityID],
			Metrics: metricsOf(a),
			First:   tsPtr(a.FirstPlayedAt),
			Last:    tsPtr(a.LastPlayedAt),
		})
	}
	return out
}

// dimensionCaveat explains, in the response, why a dimension's totals do not reconcile with
// the overall total. Stating it here rather than only in the docs means a client cannot build
// a part-to-whole chart on data that has no whole.
func dimensionCaveat(dim model.Dim) string {
	switch dim {
	case model.DimArtist:
		return "A play with several artists is credited to each, so artist totals sum to " +
			"more than the overall total."
	case model.DimGenre:
		return "Genres are a many-to-many labelling: a play counts under every genre its " +
			"artists carry, and plays by artists with no genres count under none. Genre " +
			"totals therefore do not sum to the overall total in either direction, and must " +
			"not be drawn as a part-to-whole chart."
	case model.DimAlbum:
		return "Plays of tracks with no album are excluded, so album totals sum to less " +
			"than the overall total."
	default:
		return ""
	}
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func orderStr(desc bool) string {
	if desc {
		return "desc"
	}
	return "asc"
}

// lowerASCII lowercases ASCII letters only. Genre and track names are compared for filtering
// and ordering, where a full Unicode fold would be more correct but also locale-dependent;
// ASCII folding is predictable and sufficient for a substring filter.
func lowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func containsFold(haystack, needleLower string) bool {
	h := lowerASCII(haystack)
	for i := 0; i+len(needleLower) <= len(h); i++ {
		if h[i:i+len(needleLower)] == needleLower {
			return true
		}
	}
	return false
}
