package api

import (
	"net/http"

	"github.com/neovasili/spotistats/internal/model"
)

// TimelinePoint is one bucket of a series.
type TimelinePoint struct {
	Period  string  `json:"period"`
	Metrics Metrics `json:"metrics"`
}

// TimelineResponse is a per-bucket series suitable for charting.
type TimelineResponse struct {
	Dim  string `json:"dim"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	// ArtistName and AlbumName give a bare title the context it needs to identify anything.
	ArtistName string          `json:"artistName,omitempty"`
	AlbumName  string          `json:"albumName,omitempty"`
	Bucket     string          `json:"bucket"`
	From       string          `json:"from"`
	To         string          `json:"to"`
	Points     []TimelinePoint `json:"points"`
	Total      Metrics         `json:"total"`
}

// handleTimeline returns a dense series: every bucket in the range appears, including those
// with no listening.
//
// Density matters for charting. A line chart fed only non-zero points draws a straight line
// across a month of silence, which misrepresents the data; the client should not have to
// reconstruct the missing buckets itself.
func (h *Handler) handleTimeline(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r, "dim", "id", "from", "to", "bucket", "metric")
	bucket := p.bucket()
	from := p.requiredPeriod("from")
	to := p.requiredPeriod("to")

	// dim is optional: without it the series is the overall total, which is what the
	// dashboard's activity chart needs.
	dim := model.DimTotal
	id := model.TotalEntityID
	if p.has("dim") {
		dim = p.dim("dim", true)
		id = p.required("id")
	} else if p.has("id") {
		p.fail(badRequest(CodeInvalidParameter, "id requires dim"))
	}
	if err := p.err(); err != nil {
		return err
	}

	// Day granularity exists only for TOTAL rows: per-entity-per-day aggregates would
	// multiply write volume by the number of distinct entities for no query anyone makes.
	if bucket == model.GranularityDay && dim != model.DimTotal {
		return badRequest(CodeInvalidParameter,
			"bucket=day is only available for the overall total; omit dim, or use "+
				"bucket=month for a single %s", dim)
	}

	periods, err := h.periodsBetween(from, to, bucket)
	if err != nil {
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

	out := TimelineResponse{
		Dim: string(dim), Bucket: bucketName(bucket),
		From: string(periods[0]), To: string(periods[len(periods)-1]),
		Points: make([]TimelinePoint, 0, len(periods)),
	}
	if dim != model.DimTotal {
		out.ID = id
		l := h.displayLabel(ctx, dim, id)
		out.Name, out.ArtistName, out.AlbumName = l.Name, l.ArtistName, l.AlbumName
	}

	var total model.Aggregate
	for i, period := range periods {
		a := found[keys[i]]
		out.Points = append(out.Points, TimelinePoint{
			Period:  string(period),
			Metrics: metricsOf(a),
		})
		total.Plays += a.Plays
		total.PlaysExact += a.PlaysExact
		total.MsPlayed += a.MsPlayed
		total.MsPlayedExact += a.MsPlayedExact
	}
	out.Total = metricsOf(total)

	writeJSON(w, r, h.log, out)
	return nil
}

func bucketName(g model.Granularity) string {
	switch g {
	case model.GranularityDay:
		return "day"
	case model.GranularityYear:
		return "year"
	default:
		return "month"
	}
}
