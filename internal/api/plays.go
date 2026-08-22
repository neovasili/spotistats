package api

import (
	"net/http"
	"time"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// Play is one listening event as the API reports it.
type Play struct {
	PlayedAt string   `json:"playedAt"`
	TrackID  string   `json:"trackId"`
	Name     string   `json:"name,omitempty"`
	AlbumID  string   `json:"albumId,omitempty"`
	Artists  []string `json:"artistIds,omitempty"`
	MsPlayed int64    `json:"msPlayed"`
	// Estimated is true when msPlayed is the track's full length rather than a measurement,
	// which is the case for everything captured from the recently-played endpoint.
	Estimated bool   `json:"estimated"`
	Source    string `json:"source"`
}

// PlaysResponse is a page of raw play events, oldest first.
type PlaysResponse struct {
	Items      []Play `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
	// Partial is set when the page came from the GSI, whose INCLUDE projection omits the
	// album and artist attributes. Stating it prevents a client treating the absence as
	// "this track has no artists".
	Partial bool `json:"partial,omitempty"`
}

func (h *Handler) handlePlays(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r, "trackId", "from", "to", "limit", "cursor")
	trackID := p.str("trackId", "")
	limit := p.limit()
	from, hasFrom := p.timestamp("from")
	to, hasTo := p.timestamp("to")
	if err := p.err(); err != nil {
		return err
	}

	if trackID == "" && !hasFrom && !hasTo {
		return badRequest(CodeMissingParameter,
			"give trackId, or a from/to range, or both; an unbounded scan of every play is "+
				"not offered")
	}

	// Default the range generously when only a track is named: the point of that query is a
	// track's whole history.
	if !hasFrom {
		from = time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC) // Spotify launched in 2008
	}
	if !hasTo {
		to = h.now().Add(24 * time.Hour)
	}
	if !from.Before(to) {
		return badRequest(CodeInvalidRange, "from must be before to")
	}

	ctx := r.Context()
	out := PlaysResponse{Items: []Play{}}

	// The two paths differ in more than convenience. Querying by track uses GSI1, whose
	// INCLUDE projection omits albumId and artistIds, so those come back empty -- hence
	// Partial. The range path reads the base table and is complete.
	//
	// Pagination resumes strictly after the last sort key returned, so a full walk costs O(n)
	// reads rather than re-reading a growing prefix on every page.
	fp := fingerprint(trackID, model.FormatTS(from), model.FormatTS(to))
	afterSK, err := parseSortKeyCursor(p.str("cursor", ""), fp)
	if err != nil {
		return err
	}

	var seq func(func(model.Play, error) bool)
	if trackID != "" {
		seq = h.store.PlaysOfTrackAfter(ctx, trackID, from, to, afterSK)
		out.Partial = true
	} else {
		seq = h.store.Plays(ctx, from, to, store.PlayFilter{AfterSK: afterSK})
	}

	// The sort key of the last item, which becomes the next cursor. For the base table it is
	// "{timestamp}#{trackID}"; for GSI1 it is the timestamp alone.
	lastSK := ""
	more := false
	for pl, err := range seq {
		if err != nil {
			return err
		}
		if len(out.Items) == limit {
			more = true
			break
		}
		out.Items = append(out.Items, Play{
			PlayedAt:  model.FormatTS(pl.PlayedAt),
			TrackID:   pl.TrackID,
			AlbumID:   pl.AlbumID,
			Artists:   pl.ArtistIDs,
			MsPlayed:  pl.MsPlayed,
			Estimated: pl.MsEstimated,
			Source:    string(pl.Source),
		})
		if trackID != "" {
			lastSK = model.FormatTS(pl.PlayedAt)
		} else {
			lastSK = store.PlaySK(pl.PlayedAt, pl.TrackID)
		}
	}

	if more {
		next, cerr := sortKeyCursor(fp, lastSK)
		if cerr != nil {
			return cerr
		}
		out.NextCursor = next
	}

	// Names are a single batch read for the page, not one per play.
	if len(out.Items) > 0 {
		ids := make([]string, 0, len(out.Items))
		for _, it := range out.Items {
			ids = append(ids, it.TrackID)
		}
		if tracks, terr := h.store.GetTracks(ctx, ids); terr == nil {
			for i := range out.Items {
				out.Items[i].Name = tracks[out.Items[i].TrackID].Name
			}
		}
	}

	writeJSON(w, r, h.log, out)
	return nil
}
