package spotify

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/neovasili/spotistats/internal/model"
)

// MaxRecentlyPlayedLimit is the hard maximum the endpoint accepts.
//
// Spotify retains only around this many plays in total, which is why the capture job runs
// every two hours rather than nightly: 50 tracks is roughly three hours of listening, and
// anything dropped is unrecoverable -- the endpoint cannot page back into history.
const MaxRecentlyPlayedLimit = 50

// RecentlyPlayedOptions selects a page of the recently-played feed.
type RecentlyPlayedOptions struct {
	// Limit is clamped to [1, MaxRecentlyPlayedLimit]. Zero means the maximum.
	Limit int

	// After returns items strictly after this instant. Mutually exclusive with Before.
	After time.Time
	// Before returns items strictly before this instant. Mutually exclusive with After.
	Before time.Time
}

// RecentlyPlayedPage is one page of the recently-played feed.
type RecentlyPlayedPage struct {
	// Plays is normalised to OLDEST FIRST. Spotify returns newest-first, but ingestion
	// must process oldest-first so the stored cursor only ever moves forward; doing the
	// reversal once here removes a whole class of ordering bug from every caller.
	Plays []model.Play

	// Tracks and Albums carry the objects embedded in the same payload, keyed by ID.
	// Tracks is where the estimated msPlayed comes from (the endpoint has no duration
	// field, so the track's full length is used).
	//
	// Artists are NOT included: the embedded artist objects are the simplified shape and
	// carry no genres, so genre rollups still need a GET /v1/artists enrichment pass.
	Tracks map[string]model.Track
	Albums map[string]model.Album

	// Limit is the limit the server echoed back.
	Limit int

	// NextAfter and NextBefore are the cursors from the response, zero when absent.
	NextAfter  time.Time
	NextBefore time.Time
	// HasNext reports whether the response carried a next URL.
	HasNext bool

	// Saturated reports that the page came back completely full, meaning listening may
	// have exceeded the polling window and plays may have been lost. It compares against
	// the REQUESTED limit rather than the literal 50, so tests can reproduce the
	// condition with a small limit.
	//
	// This is the gap signal: it should raise an alarm and prompt a shorter capture
	// interval.
	Saturated bool

	// OldestPlayedAt and NewestPlayedAt bound the returned items. They are logged
	// alongside the cursors because Spotify does not document whether `after` returns the
	// oldest or the newest matching items -- which determines whether a saturated page
	// means plays were merely at risk or definitely lost. Until that is settled with real
	// data, this package deliberately provides no auto-paginating iterator.
	OldestPlayedAt time.Time
	NewestPlayedAt time.Time
}

// RecentlyPlayed fetches exactly one page of the current user's recently played tracks.
//
// Podcast episodes are never included -- the endpoint does not support them -- so all
// Spotistats figures are music-only. Requires the user-read-recently-played scope.
func (c *Client) RecentlyPlayed(ctx context.Context, opt RecentlyPlayedOptions) (RecentlyPlayedPage, error) {
	if !opt.After.IsZero() && !opt.Before.IsZero() {
		return RecentlyPlayedPage{}, ErrCursorConflict
	}

	limit := opt.Limit
	if limit <= 0 || limit > MaxRecentlyPlayedLimit {
		limit = MaxRecentlyPlayedLimit
	}

	q := url.Values{"limit": {strconv.Itoa(limit)}}
	switch {
	case !opt.After.IsZero():
		q.Set("after", strconv.FormatInt(model.UnixMillis(opt.After), 10))
	case !opt.Before.IsZero():
		q.Set("before", strconv.FormatInt(model.UnixMillis(opt.Before), 10))
	}

	var wire dtoRecentlyPlayed
	if err := c.get(ctx, "me/player/recently-played", q, &wire); err != nil {
		return RecentlyPlayedPage{}, err
	}

	page := RecentlyPlayedPage{
		Limit:     wire.Limit,
		HasNext:   wire.Next != "",
		Saturated: len(wire.Items) >= limit,
		Tracks:    make(map[string]model.Track, len(wire.Items)),
		Albums:    make(map[string]model.Album, len(wire.Items)),
	}
	if page.Limit == 0 {
		page.Limit = limit
	}
	page.NextAfter = cursorTime(wire.Cursors.After)
	page.NextBefore = cursorTime(wire.Cursors.Before)

	plays := make([]model.Play, 0, len(wire.Items))
	for i := range wire.Items {
		item := &wire.Items[i]
		if item.Track.ID == "" {
			// Local files and unavailable tracks have no ID and cannot be aggregated.
			c.log.WarnContext(ctx, "spotify: skipping recently-played item with no track ID",
				"playedAt", item.PlayedAt, "name", item.Track.Name)
			continue
		}
		playedAt, err := model.ParseSpotifyTS(item.PlayedAt)
		if err != nil {
			return RecentlyPlayedPage{}, fmt.Errorf("spotify: recently-played item %d: %w", i, err)
		}

		track := item.Track.toModel()
		page.Tracks[track.ID] = track
		if album := item.Track.Album.toModel(); album.ID != "" {
			page.Albums[album.ID] = album
		}

		play, err := model.NewAPIPlay(playedAt, track)
		if err != nil {
			return RecentlyPlayedPage{}, fmt.Errorf("spotify: recently-played item %d (track %s): %w",
				i, track.ID, err)
		}
		plays = append(plays, play)
	}

	sort.SliceStable(plays, func(i, j int) bool {
		return plays[i].PlayedAt.Before(plays[j].PlayedAt)
	})
	page.Plays = plays

	if n := len(plays); n > 0 {
		page.OldestPlayedAt = plays[0].PlayedAt
		page.NewestPlayedAt = plays[n-1].PlayedAt
	}

	return page, nil
}

// cursorTime parses a cursor value, which Spotify sends as Unix milliseconds in a string.
func cursorTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return model.FromUnixMillis(ms)
}
