package api

import (
	"context"
	"net/http"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// StatsResponse answers "how much did I listen to X during P".
type StatsResponse struct {
	Dim  string `json:"dim"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// ArtistName and AlbumName give a bare title the context it needs to identify anything.
	ArtistName string `json:"artistName,omitempty"`
	AlbumName  string `json:"albumName,omitempty"`
	// ImageURL is the large asset, for the drill-down header.
	ImageURL string  `json:"imageUrl,omitempty"`
	ThumbURL string  `json:"thumbUrl,omitempty"`
	Period   string  `json:"period,omitempty"`
	From     string  `json:"from,omitempty"`
	To       string  `json:"to,omitempty"`
	Metrics  Metrics `json:"metrics"`
	First    *string `json:"firstPlayedAt"`
	Last     *string `json:"lastPlayedAt"`
	// Buckets is the number of period rows summed. One for a single period; more for a range.
	Buckets int `json:"buckets"`
}

// handleStats is the canonical query from docs/SPECS.md 5.3 -- "minutes listening to Within
// Temptation during 2025" -- and for a single period it is a single GetItem.
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r, "dim", "id", "period", "from", "to", "metric")
	dim := p.dim("dim", true)
	id := p.required("id")

	rangeMode := p.has("from") || p.has("to")
	if rangeMode && p.has("period") {
		p.fail(badRequest(CodeInvalidParameter,
			"use either period or from/to, not both"))
	}

	var periods []model.Period
	var single model.Period
	if rangeMode {
		from := p.requiredPeriod("from")
		to := p.requiredPeriod("to")
		if err := p.err(); err != nil {
			return err
		}
		// Sum at the coarsest granularity the range is expressed in, so a month range sums
		// month rows rather than fanning out to days.
		gran := from.Granularity()
		if to.Granularity() > gran {
			gran = to.Granularity()
		}
		var err error
		periods, err = h.periodsBetween(from, to, gran)
		if err != nil {
			return err
		}
	} else {
		single = p.period("period")
		if err := p.err(); err != nil {
			return err
		}
		periods = []model.Period{single}
	}
	if err := p.err(); err != nil {
		return err
	}

	ctx := r.Context()
	keys := make([]model.AggKey, 0, len(periods))
	for _, period := range periods {
		keys = append(keys, model.AggKey{Dim: dim, Period: period, EntityID: id})
	}

	found, err := h.store.BatchGetAggregates(ctx, keys)
	if err != nil {
		return err
	}

	// Sum the buckets. A period with no row contributes nothing, which is why an absent key is
	// not an error.
	var total model.Aggregate
	total.Key = keys[0]
	for _, k := range keys {
		a, ok := found[k]
		if !ok {
			continue
		}
		total.Plays += a.Plays
		total.PlaysExact += a.PlaysExact
		total.MsPlayed += a.MsPlayed
		total.MsPlayedExact += a.MsPlayedExact
		if !a.FirstPlayedAt.IsZero() &&
			(total.FirstPlayedAt.IsZero() || a.FirstPlayedAt.Before(total.FirstPlayedAt)) {
			total.FirstPlayedAt = a.FirstPlayedAt
		}
		if a.LastPlayedAt.After(total.LastPlayedAt) {
			total.LastPlayedAt = a.LastPlayedAt
		}
	}

	out := StatsResponse{
		Dim:     string(dim),
		ID:      id,
		Metrics: metricsOf(total),
		First:   tsPtr(total.FirstPlayedAt),
		Last:    tsPtr(total.LastPlayedAt),
		Buckets: len(periods),
	}
	if rangeMode {
		out.From, out.To = string(periods[0]), string(periods[len(periods)-1])
	} else {
		out.Period = string(single)
	}
	l := h.displayLabel(ctx, dim, id)
	out.Name, out.ArtistName, out.AlbumName = l.Name, l.ArtistName, l.AlbumName
	out.ImageURL, out.ThumbURL = l.ImageURL, l.ThumbURL

	writeJSON(w, r, h.log, out)
	return nil
}

// displayLabel resolves an entity's name and surrounding context, empty when unknown.
//
// A missing label is never an error: genres are their own name, and a dimension row may simply
// not have been enriched yet. Failing the whole request over a label would be wrong.
func (h *Handler) displayLabel(ctx context.Context, dim model.Dim, id string) store.Label {
	labels, _ := h.store.ResolveLabels(ctx, dim, []string{id})
	return labels[id]
}
