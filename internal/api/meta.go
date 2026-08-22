package api

import (
	"net/http"

	"github.com/neovasili/spotistats/internal/model"
)

// MetaResponse describes the dataset as a whole: what it covers, how fresh it is, and how
// much of it is estimated.
type MetaResponse struct {
	Metrics Metrics `json:"metrics"`

	// Coverage is the span of stored listening history.
	//
	// Approximate reports that the bounds come from write-time aggregate attributes, which
	// DynamoDB cannot maintain as a true min/max in a single request (docs/SPECS.md 5.2):
	// firstPlayedAt is set by whichever play was WRITTEN first and lastPlayedAt by whichever
	// was written last, so out-of-order ingestion -- a backfill, a replay -- leaves them
	// astray. The nightly reconcile recomputes both from the raw plays and clears the flag.
	// The play and duration counters are always exact regardless.
	Coverage struct {
		FirstPlayedAt *string `json:"firstPlayedAt"`
		LastPlayedAt  *string `json:"lastPlayedAt"`
		Approximate   bool    `json:"approximate"`
	} `json:"coverage"`

	// Capture reports the ingestion pipeline's state.
	Capture struct {
		LastRunAt    *string `json:"lastRunAt"`
		LastPlayedAt *string `json:"lastPlayedAt"`
		LastStatus   string  `json:"lastStatus,omitempty"`
		// Gaps counts recorded capture windows that came back full, meaning plays may have
		// been lost irrecoverably. Non-zero is worth surfacing in the UI.
		Gaps int `json:"gaps"`
	} `json:"capture"`

	// Timezone is the zone every period key is derived in. A client rendering a date must use
	// it, or its labels will disagree with the aggregates.
	Timezone string `json:"timezone"`

	// Notes are caveats a UI should surface. Encoding them in the response rather than the
	// docs means a client cannot present the data as more complete than it is.
	Notes []string `json:"notes"`
}

func (h *Handler) handleMeta(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r)
	if err := p.err(); err != nil {
		return err
	}
	ctx := r.Context()

	total, err := h.store.GetAggregateOrZero(ctx, model.AggKey{
		Dim: model.DimTotal, Period: model.PeriodAll, EntityID: model.TotalEntityID,
	})
	if err != nil {
		return err
	}

	var out MetaResponse
	out.Metrics = metricsOf(total)
	out.Timezone = h.cal.Name()

	cursor, err := h.store.GetPollCursor(ctx)
	if err != nil {
		return err
	}

	first, last := total.FirstPlayedAt, total.LastPlayedAt

	// The poll cursor only ever moves forward, so it is a reliable lower bound on the latest
	// play and can correct a lagging aggregate attribute.
	if cursor.LastPlayedAt.After(last) {
		last = cursor.LastPlayedAt
	}
	// Never report an inverted window. Presenting "coverage: 23 Jul to 11 Jul" as fact is
	// worse than admitting the bounds are approximate, and a client would have no way to tell
	// it was nonsense.
	approximate := false
	if !first.IsZero() && !last.IsZero() && first.After(last) {
		first, last = last, first
		approximate = true
	}
	if first.After(total.FirstPlayedAt) || last.After(total.LastPlayedAt) {
		approximate = true
	}

	out.Coverage.FirstPlayedAt = tsPtr(first)
	out.Coverage.LastPlayedAt = tsPtr(last)
	out.Coverage.Approximate = approximate
	out.Capture.LastRunAt = tsPtr(cursor.LastRunAt)
	out.Capture.LastPlayedAt = tsPtr(cursor.LastPlayedAt)
	out.Capture.LastStatus = cursor.LastStatus

	for _, err := range h.store.GapMarkers(ctx) {
		if err != nil {
			return err
		}
		out.Capture.Gaps++
	}

	out.Notes = []string{
		"Podcast episodes are not included: the Spotify recently-played endpoint does not " +
			"report them, so all figures are music only.",
	}
	if out.Metrics.EstimatedRatio > 0 {
		out.Notes = append(out.Notes,
			"Some listening time is estimated. The recently-played endpoint returns no "+
				"duration, so plays captured from it count the track's full length and "+
				"over-count skips. See estimatedRatio.")
	}
	if out.Capture.Gaps > 0 {
		out.Notes = append(out.Notes,
			"One or more capture windows came back full, so some plays may be missing "+
				"permanently: the endpoint retains only about 50 and cannot page back.")
	}
	if out.Coverage.Approximate {
		out.Notes = append(out.Notes,
			"The coverage window is approximate: DynamoDB cannot maintain a true minimum and "+
				"maximum in one request, so out-of-order ingestion leaves the bounds astray "+
				"until the nightly reconcile recomputes them. Play counts and durations are "+
				"unaffected and exact.")
	}
	if cursor.LastStatus == "ok-degraded-genres" {
		out.Notes = append(out.Notes,
			"The most recent capture could not resolve artist genres, so genre figures may "+
				"be incomplete until the next reconcile.")
	}

	writeJSON(w, r, h.log, out)
	return nil
}
