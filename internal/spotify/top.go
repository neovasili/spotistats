package spotify

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/neovasili/spotistats/internal/model"
)

// TimeRange selects the window Spotify computes its own top-items rankings over.
//
// These are Spotify's rankings, not Spotistats'. They are stored alongside the computed
// leaderboards and will disagree with them -- Spotify's weighting is undocumented and its
// windows are approximate -- so the UI must label which is which.
type TimeRange string

const (
	// TimeRangeShort is approximately the last 4 weeks.
	TimeRangeShort TimeRange = "short_term"
	// TimeRangeMedium is approximately the last 6 months. This is Spotify's default.
	TimeRangeMedium TimeRange = "medium_term"
	// TimeRangeLong is calculated from roughly the last year of data.
	TimeRangeLong TimeRange = "long_term"
)

// AllTimeRanges lists every window, for the nightly refresh that stores all three.
func AllTimeRanges() []TimeRange {
	return []TimeRange{TimeRangeShort, TimeRangeMedium, TimeRangeLong}
}

// Valid reports whether tr is a known window.
func (tr TimeRange) Valid() bool {
	switch tr {
	case TimeRangeShort, TimeRangeMedium, TimeRangeLong:
		return true
	}
	return false
}

// MaxTopItemsLimit is the per-request maximum for the top-items endpoints.
const MaxTopItemsLimit = 50

func topQuery(tr TimeRange, limit, offset int) (url.Values, error) {
	if !tr.Valid() {
		return nil, fmt.Errorf("spotify: unknown time range %q", tr)
	}
	if limit <= 0 || limit > MaxTopItemsLimit {
		limit = MaxTopItemsLimit
	}
	if offset < 0 {
		offset = 0
	}
	return url.Values{
		"time_range": {string(tr)},
		"limit":      {strconv.Itoa(limit)},
		"offset":     {strconv.Itoa(offset)},
	}, nil
}

// TopArtists returns the user's top artists for the given window. Requires the
// user-top-read scope.
func (c *Client) TopArtists(ctx context.Context, tr TimeRange, limit, offset int) ([]model.Artist, error) {
	q, err := topQuery(tr, limit, offset)
	if err != nil {
		return nil, err
	}
	var wire dtoTopArtists
	if err := c.get(ctx, "me/top/artists", q, &wire); err != nil {
		return nil, err
	}
	out := make([]model.Artist, 0, len(wire.Items))
	for i := range wire.Items {
		out = append(out, wire.Items[i].toModel())
	}
	return out, nil
}

// TopTracks returns the user's top tracks for the given window. Requires the
// user-top-read scope.
func (c *Client) TopTracks(ctx context.Context, tr TimeRange, limit, offset int) ([]model.Track, error) {
	q, err := topQuery(tr, limit, offset)
	if err != nil {
		return nil, err
	}
	var wire dtoTopTracks
	if err := c.get(ctx, "me/top/tracks", q, &wire); err != nil {
		return nil, err
	}
	out := make([]model.Track, 0, len(wire.Items))
	for i := range wire.Items {
		out = append(out, wire.Items[i].toModel())
	}
	return out, nil
}
