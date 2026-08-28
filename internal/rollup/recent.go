package rollup

import (
	"context"
	"fmt"
	"time"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// PartialPeriod is a period still in progress, paired with the SAME SPAN of the one before it.
//
// The comparison is the whole point. A month total on the 3rd is a small number and a month total
// on the 30th is a large one, and neither says anything until it is measured against the same
// stretch of the previous month. Comparing a partial period against a whole one would flatter or
// damn it purely by the date.
//
// CurrentYear predates this type and keeps its own shape with a `previousYearToDate` field. Not
// folded in: renaming a published JSON field would break every deployed frontend during the
// window between shipping the bundle and the next rollup writing a new snapshot.
type PartialPeriod struct {
	// Period is the machine label -- "2026-08" for a month, the Monday's date for a week.
	Period string `json:"period"`
	// Elapsed counts the days covered so far, including today. It is what makes the figure
	// interpretable: "5h this week" means something different on a Tuesday and on a Sunday.
	Elapsed        int     `json:"elapsed"`
	Metrics        Metrics `json:"metrics"`
	PreviousToDate Metrics `json:"previousToDate"`
}

// isoDate is the day-row date format, which is also the format the calendar's Day period uses.
const isoDate = "2006-01-02"

// monthToDate describes the current calendar month so far, against the same stretch of the last.
//
// A free function taking the calendar rather than a Rollup method: it needs nothing else off the
// Rollup, and the month clamp below is exactly the kind of arithmetic that wants a unit test
// rather than a seeded table and a fixed clock.
//
// The previous month's end is CLAMPED to its own length. On 31 March the current stretch is 31
// days and February has 28, so an unclamped cut would read into March and compare the month
// against part of itself.
func monthToDate(cal model.Calendar, hist *history, now time.Time) PartialPeriod {
	local := now.In(cal.Location())
	start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, cal.Location())
	elapsed := local.Day()

	prevStart := start.AddDate(0, -1, 0)
	prevLast := prevStart.AddDate(0, 1, -1) // the last day of the previous month
	prevEnd := prevStart.AddDate(0, 0, elapsed-1)
	if prevEnd.After(prevLast) {
		prevEnd = prevLast
	}

	return PartialPeriod{
		Period:         string(cal.Month(now)),
		Elapsed:        elapsed,
		Metrics:        hist.Between(start.Format(isoDate), local.Format(isoDate)),
		PreviousToDate: hist.Between(prevStart.Format(isoDate), prevEnd.Format(isoDate)),
	}
}

// weekToDate describes the current week so far, against the same weekdays of the last.
//
// Weeks start on MONDAY: ISO 8601, and the convention wherever this archive was recorded. Note
// this is a different question from the weekday histogram's bucket 0, which is Sunday because it
// mirrors Go's time.Weekday numbering -- an index into an array, not a claim about when a week
// begins.
func weekToDate(cal model.Calendar, hist *history, now time.Time) PartialPeriod {
	local := now.In(cal.Location())
	// Go numbers Sunday 0 through Saturday 6, so this is days since the most recent Monday.
	sinceMonday := (int(local.Weekday()) + 6) % 7
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, cal.Location()).
		AddDate(0, 0, -sinceMonday)

	// A week is always seven days, so the previous span needs no clamping -- unlike the month.
	prevStart := start.AddDate(0, 0, -7)
	prevEnd := local.AddDate(0, 0, -7)

	return PartialPeriod{
		Period:         start.Format(isoDate),
		Elapsed:        sinceMonday + 1,
		Metrics:        hist.Between(start.Format(isoDate), local.Format(isoDate)),
		PreviousToDate: hist.Between(prevStart.Format(isoDate), prevEnd.Format(isoDate)),
	}
}

// topArtistBetween finds the most-played artist in a window by reading the PLAYS themselves.
//
// Aggregates cannot answer this. Artist rows exist at all-time, year and month granularity and
// nothing finer -- per-entity-per-day rows would multiply write volume by the number of distinct
// entities (see model.AggKey.Validate) -- so there is no "artist of the week" row to read and no
// combination of stored rows that adds up to one.
//
// Reading the plays is affordable precisely because the window is small: a week is a few hundred
// items in one or two monthly partitions, and the distinct tracks behind them are batch-loaded in
// a single round trip. This would be the wrong approach for a year and is the only one for a week.
//
// A play counts towards EVERY artist credited on its track, which is what the aggregate fan-out
// does. Counting only the primary artist would make the weekly winner incomparable with the
// monthly one directly beside it on the page.
func (r *Rollup) topArtistBetween(ctx context.Context, from, to time.Time) (*Entry, error) {
	var buffered []model.Play
	var trackIDs []string
	seen := map[string]bool{}

	for p, err := range r.store.Plays(ctx, from, to, store.PlayFilter{}) {
		if err != nil {
			return nil, fmt.Errorf("rollup: read plays for the top-artist window: %w", err)
		}
		buffered = append(buffered, p)
		if p.TrackID != "" && !seen[p.TrackID] {
			seen[p.TrackID] = true
			trackIDs = append(trackIDs, p.TrackID)
		}
	}
	if len(buffered) == 0 {
		return nil, nil
	}

	tracks := newTrackCache(r.store)
	if err := tracks.Warm(ctx, trackIDs); err != nil {
		r.log.WarnContext(ctx, "rollup: batch track prefetch failed for the top-artist window; "+
			"falling back to per-track lookups", "err", err)
	}
	canon, err := newCanonicaliser(ctx, r.store, tracks.Loaded())
	if err != nil {
		return nil, fmt.Errorf("rollup: build canonical ID index: %w", err)
	}

	type tally struct {
		plays int64
		ms    int64
	}
	byArtist := map[string]*tally{}
	for _, p := range buffered {
		track, terr := tracks.For(ctx, p.TrackID)
		if terr != nil {
			r.log.WarnContext(ctx, "rollup: track lookup failed in the top-artist window",
				"trackId", p.TrackID, "err", terr)
		}
		facts := canon.Apply(model.FactsForTrack(p, track, nil))
		for _, id := range facts.ArtistIDs {
			t, ok := byArtist[id]
			if !ok {
				t = &tally{}
				byArtist[id] = t
			}
			t.plays++
			t.ms += p.MsPlayed
		}
	}

	// Ranked by listening time, like every other board on the page. The ID breaks ties so the
	// winner does not depend on Go's map iteration order.
	var topID string
	var top tally
	for id, t := range byArtist {
		if t.ms > top.ms || (t.ms == top.ms && topID != "" && id < topID) {
			topID, top = id, *t
		}
	}
	if topID == "" {
		return nil, nil
	}

	name := topID
	var imageURL, thumbURL string
	if labels, lerr := r.store.ResolveLabels(ctx, model.DimArtist, []string{topID}); lerr != nil {
		// Cosmetic: the winner is already decided, so this costs a name, not the truth.
		r.log.WarnContext(ctx, "rollup: could not resolve the top artist's label", "err", lerr)
	} else if l, ok := labels[topID]; ok && l.Name != "" {
		name, imageURL, thumbURL = l.Name, l.ImageURL, l.ThumbURL
	}

	return &Entry{
		Rank: 1, ID: topID, Name: name,
		Plays: top.plays, MsPlayed: top.ms,
		ImageURL: imageURL, ThumbURL: thumbURL,
	}, nil
}
