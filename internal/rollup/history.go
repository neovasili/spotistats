package rollup

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/neovasili/spotistats/internal/model"
)

// PeriodValue is one point of a per-period series, e.g. one year of listening.
type PeriodValue struct {
	Period   string `json:"period"`
	Plays    int64  `json:"plays"`
	MsPlayed int64  `json:"msPlayed"`
}

// Records are the all-time extremes: facts rather than measures, and the only figures on the
// dashboard that describe a single moment.
type Records struct {
	// BusiestDay is the day with the most listening time in the whole history.
	BusiestDay DayValue `json:"busiestDay"`
	// LongestStreak is the longest run of consecutive days with at least one play, ALL TIME.
	//
	// It replaces a figure that was quietly wrong: the streaks were computed from the calendar
	// window alone, so "longest 150d" meant "longest within the last 24 months" while reading
	// as an all-time record. Widening that window from 12 to 24 months silently changed what
	// the number meant without changing its label.
	LongestStreak int `json:"longestStreak"`
	// LongestStreakEnd dates the streak, so the record is placeable rather than abstract.
	LongestStreakEnd string `json:"longestStreakEnd,omitempty"`
}

// history is every day row in the stored history, oldest first.
//
// One read powers four separate things -- the per-year series, the year-to-date comparison, the
// busiest day and the true longest streak -- which is why it is fetched once and passed around
// rather than each feature querying for itself.
//
// Cost is one Query per calendar year the history spans (a day row lives in its YEAR's partition
// by design, see model.AggKey), so about eighteen queries and six thousand items against a job
// that already streams four hundred thousand plays.
type history struct {
	days []DayValue
	// byYear is indexed by "yyyy".
	byYear map[string]*PeriodValue
}

// loadHistory reads every day row from the first year with plays through the current one.
func (r *Rollup) loadHistory(ctx context.Context, from, to time.Time) (*history, error) {
	h := &history{byYear: map[string]*PeriodValue{}}
	if from.IsZero() {
		return h, nil
	}

	for y := from.Year(); y <= to.Year(); y++ {
		year := model.Period(fmt.Sprintf("%04d", y))
		// The day rows only: begins_with excludes the year total, whose sort key is "ALL".
		for agg, err := range r.store.QueryAggregates(ctx, model.DimTotal, year, string(year)+"-") {
			if err != nil {
				return nil, fmt.Errorf("rollup: read %s day rows: %w", year, err)
			}
			h.days = append(h.days, DayValue{
				Date: string(agg.Key.Period), Plays: agg.Plays, MsPlayed: agg.MsPlayed,
			})
		}
	}

	// Chronological, because both the streak walk and the year-to-date cut depend on order and
	// DynamoDB only guarantees it within a partition.
	slices.SortFunc(h.days, func(a, b DayValue) int {
		switch {
		case a.Date < b.Date:
			return -1
		case a.Date > b.Date:
			return 1
		default:
			return 0
		}
	})

	for _, d := range h.days {
		if len(d.Date) < 4 {
			continue
		}
		y := d.Date[:4]
		pv, ok := h.byYear[y]
		if !ok {
			pv = &PeriodValue{Period: y}
			h.byYear[y] = pv
		}
		pv.Plays += d.Plays
		pv.MsPlayed += d.MsPlayed
	}
	return h, nil
}

// Series returns one entry per year the history spans, oldest first.
//
// Years with no listening are INCLUDED as zeroes. A gap is a fact about the archive -- a year
// away from Spotify, or a year of the export that never arrived -- and omitting it would draw a
// continuous line through a discontinuity.
func (h *history) Series(from, to time.Time) []PeriodValue {
	if from.IsZero() {
		return nil
	}
	out := make([]PeriodValue, 0, to.Year()-from.Year()+1)
	for y := from.Year(); y <= to.Year(); y++ {
		key := fmt.Sprintf("%04d", y)
		if pv, ok := h.byYear[key]; ok {
			out = append(out, *pv)
			continue
		}
		out = append(out, PeriodValue{Period: key})
	}
	return out
}

// YearToDate sums a year's listening up to the same month and day as `on`.
//
// Comparing a full previous year against a partial current one would flatter or damn the current
// year purely by the date, so the cut is on the calendar position rather than the total.
func (h *history) YearToDate(year int, on time.Time) Metrics {
	var m Metrics
	prefix := fmt.Sprintf("%04d-", year)
	cutoff := on.Format("01-02")
	for _, d := range h.days {
		if len(d.Date) != 10 || d.Date[:5] != prefix {
			continue
		}
		if d.Date[5:] > cutoff {
			continue
		}
		m.Plays += d.Plays
		m.MsPlayed += d.MsPlayed
	}
	return m
}

// Between sums the day rows falling in the inclusive date range [from, to], both "yyyy-mm-dd".
//
// A linear scan of the whole history rather than a binary search: six thousand string compares
// against a job that streams four hundred thousand plays is not where the time goes, and the
// days are already in memory for four other features.
//
// Absent days contribute nothing, which is correct rather than merely convenient -- a day row
// exists only for a day with plays, so a silent day is a real zero and not missing data.
func (h *history) Between(from, to string) Metrics {
	var m Metrics
	if from == "" || to == "" || from > to {
		return m
	}
	for _, d := range h.days {
		if d.Date < from || d.Date > to {
			continue
		}
		m.Plays += d.Plays
		m.MsPlayed += d.MsPlayed
	}
	return m
}

// Records computes the all-time extremes.
//
// The streak walk compares DATES, not adjacent slice entries. That distinction is the whole
// difficulty of counting streaks over a sparse series: a day row exists only for a day with
// plays, so every entry here has Plays > 0 and a "reset when Plays == 0" test can never fire --
// it would report the total number of active days as one unbroken streak. The dense-series code
// this replaced got it right by materialising every day including the empty ones; comparing
// dates gets it right without six thousand placeholder entries.
func (h *history) Records() Records {
	var rec Records
	run := 0
	var prev time.Time
	for _, d := range h.days {
		if d.MsPlayed > rec.BusiestDay.MsPlayed {
			rec.BusiestDay = d
		}
		day, err := parseDay(d.Date)
		if err != nil {
			continue
		}
		if run > 0 && !day.Equal(prev.AddDate(0, 0, 1)) {
			run = 0
		}
		run++
		prev = day
		if run > rec.LongestStreak {
			rec.LongestStreak = run
			rec.LongestStreakEnd = d.Date
		}
	}
	return rec
}

// CurrentStreak counts back from TODAY, not from the newest row.
//
// The anchor matters. Walking back from the last day that happens to have plays reports the
// length of the last run whenever it happened -- a streak that ended in 2022 would still be
// presented as current. The dense series this replaced could not make that mistake, because it
// ran to today and the trailing days were explicit zeroes.
//
// A missing today does not break the streak: the day is not over, and telling someone their
// forty-day run ended at 00:01 would be wrong by up to twenty-four hours. A gap of two days or
// more does break it.
//
// today is the local calendar day, supplied by the caller because model.Calendar owns the zone.
func (h *history) CurrentStreak(today string) int {
	anchor, err := parseDay(today)
	if err != nil || len(h.days) == 0 {
		return 0
	}

	newest, err := parseDay(h.days[len(h.days)-1].Date)
	if err != nil {
		return 0
	}
	// Anything older than yesterday means the streak is already over.
	if newest.Before(anchor.AddDate(0, 0, -1)) {
		return 0
	}

	streak := 0
	prev := newest
	for i := len(h.days) - 1; i >= 0; i-- {
		day, derr := parseDay(h.days[i].Date)
		if derr != nil {
			continue
		}
		if streak > 0 && !day.Equal(prev.AddDate(0, 0, -1)) {
			break
		}
		streak++
		prev = day
	}
	return streak
}

// parseDay reads a "yyyy-mm-dd" period key.
//
// Parsed in UTC deliberately: these are date LABELS already resolved in the configured zone by
// model.Calendar, so re-interpreting them in a zone with DST could shift one by a day.
func parseDay(date string) (time.Time, error) {
	return time.Parse("2006-01-02", date)
}

// topArtistByPeriod returns the single most-played artist of each period in the series.
//
// Named for the period rather than the year because it serves both: the per-year card passes
// eighteen years, and the artist-of-the-month figure passes one month. Any period with an
// AGG#ARTIST partition works, which is all-time, years and months -- but NOT weeks or days, for
// which see topArtistBetween.
//
// Not materialised anywhere: leaderboards cover ALL, the current year, the previous year and
// three recent months, so every earlier year has to be read from its own aggregate partition.
// Those partitions are small -- a few hundred to a couple of thousand artist rows per year --
// which is why this is affordable at eighteen queries.
//
// Labels come from store.ResolveLabels in one batch rather than per period, so a name and its
// artwork cost one round trip for the whole series.
func (r *Rollup) topArtistByPeriod(ctx context.Context, series []PeriodValue) ([]YearEntry, error) {
	type best struct {
		id  string
		agg model.Aggregate
	}
	found := make(map[string]best, len(series))
	var ids []string

	for _, pv := range series {
		if pv.MsPlayed == 0 {
			continue
		}
		var top best
		for agg, err := range r.store.QueryAggregates(ctx, model.DimArtist, model.Period(pv.Period), "") {
			if err != nil {
				return nil, fmt.Errorf("rollup: read %s artists: %w", pv.Period, err)
			}
			if agg.MsPlayed > top.agg.MsPlayed {
				top = best{id: agg.Key.EntityID, agg: agg}
			}
		}
		if top.id == "" {
			continue
		}
		found[pv.Period] = top
		ids = append(ids, top.id)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	labels, err := r.store.ResolveLabels(ctx, model.DimArtist, ids)
	if err != nil {
		// Cosmetic: the ranking is already decided, so a label failure costs names, not truth.
		r.log.WarnContext(ctx, "rollup: could not resolve labels for the per-year artists",
			"err", err)
	}

	out := make([]YearEntry, 0, len(found))
	for _, pv := range series {
		b, ok := found[pv.Period]
		if !ok {
			continue
		}
		l := labels[b.id]
		name := l.Name
		if name == "" {
			name = b.id
		}
		out = append(out, YearEntry{
			Period: pv.Period,
			Entry: Entry{
				Rank: 1, ID: b.id, Name: name,
				Plays: b.agg.Plays, MsPlayed: b.agg.MsPlayed,
				ImageURL: l.ImageURL, ThumbURL: l.ThumbURL,
			},
		})
	}
	return out, nil
}

// YearEntry pairs a year with the entity that defined it.
type YearEntry struct {
	Period string `json:"period"`
	Entry  Entry  `json:"entry"`
}
