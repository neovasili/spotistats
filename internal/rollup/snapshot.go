package rollup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// Snapshot file names, relative to the data prefix. The dashboard fetches these from its own
// origin rather than the API (docs/SPECS.md 3.1): identical for every visitor and changing once
// a night, so the landing page costs no compute and survives a Lambda outage.
const (
	FileDashboard = "dashboard.json"
	FileCatalog   = "catalog.json"
	FileMeta      = "meta.json"
)

// Metrics mirrors the API's measure envelope. Duplicated rather than imported to keep this
// package free of internal/api: the snapshot is a wire format in its own right, and coupling the
// two would mean an API refactor silently changed the file the dashboard reads.
type Metrics struct {
	Plays          int64   `json:"plays"`
	PlaysExact     int64   `json:"playsExact"`
	MsPlayed       int64   `json:"msPlayed"`
	MsPlayedExact  int64   `json:"msPlayedExact"`
	EstimatedRatio float64 `json:"estimatedRatio"`
}

// Entry is one ranked entity.
type Entry struct {
	Rank     int    `json:"rank"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Plays    int64  `json:"plays"`
	MsPlayed int64  `json:"msPlayed"`
	// ImageURL is the large asset, ThumbURL the small one for list rows. Both omitted when
	// absent, which is common: an entity only has artwork once the API has resolved it, and
	// the renderer treats absent and failed-to-load as the same case.
	ImageURL string `json:"imageUrl,omitempty"`
	ThumbURL string `json:"thumbUrl,omitempty"`

	// ArtistName and AlbumName give a bare title the context it needs to identify anything.
	// Omitted when empty, so an artist entry carries neither and a track missing its album row
	// simply renders without the subtitle rather than with a blank one.
	ArtistName string `json:"artistName,omitempty"`
	AlbumName  string `json:"albumName,omitempty"`
}

// DayValue is one cell of the calendar heatmap.
type DayValue struct {
	Date     string `json:"date"`
	Plays    int64  `json:"plays"`
	MsPlayed int64  `json:"msPlayed"`
}

// BucketValue is one bar of a rhythm histogram.
type BucketValue struct {
	Bucket   int   `json:"bucket"`
	Plays    int64 `json:"plays"`
	MsPlayed int64 `json:"msPlayed"`
}

// Dashboard is the whole landing page in one document.
type Dashboard struct {
	GeneratedAt string `json:"generatedAt"`
	Timezone    string `json:"timezone"`

	Coverage struct {
		FirstPlayedAt *string `json:"firstPlayedAt"`
		LastPlayedAt  *string `json:"lastPlayedAt"`
		Approximate   bool    `json:"approximate"`
	} `json:"coverage"`

	AllTime     Metrics `json:"allTime"`
	CurrentYear struct {
		Period  string  `json:"period"`
		Metrics Metrics `json:"metrics"`
	} `json:"currentYear"`

	KPIs struct {
		DistinctTracks  int `json:"distinctTracks"`
		DistinctArtists int `json:"distinctArtists"`
		DistinctAlbums  int `json:"distinctAlbums"`
		DistinctGenres  int `json:"distinctGenres"`
		// CurrentStreak counts consecutive days with listening, ending today or yesterday.
		// Yesterday counts because today is not over.
		CurrentStreak int `json:"currentStreak"`
		LongestStreak int `json:"longestStreak"`
	} `json:"kpis"`

	Top struct {
		Artists []Entry `json:"artists"`
		Tracks  []Entry `json:"tracks"`
		Albums  []Entry `json:"albums"`
		Genres  []Entry `json:"genres"`
	} `json:"top"`

	TopThisYear struct {
		Artists []Entry `json:"artists"`
		Tracks  []Entry `json:"tracks"`
	} `json:"topThisYear"`

	// Calendar covers the trailing 12 months, densely: every day appears, including those with
	// no listening, so the heatmap does not have to reconstruct gaps.
	Calendar []DayValue `json:"calendar"`

	Rhythm struct {
		HourOfDay []BucketValue `json:"hourOfDay"`
		Weekday   []BucketValue `json:"weekday"`
	} `json:"rhythm"`

	// GenreCoverage is the EXACT share of listening time whose artists carry at least one genre,
	// counted per play by the nightly coverage pass.
	//
	// It cannot be derived from the genre aggregates: a play with three genres contributes to
	// three rows, so summing them overstates coverage, and capping the sum at the total reports
	// a confident 100% whenever the overcount exceeds the shortfall. Zero means no full pass has
	// run yet.
	//
	// It exists because genre figures cannot be drawn as a part-to-whole chart: genres are a
	// many-to-many labelling, so they neither sum to the total nor fall short of it in a
	// predictable direction (docs/SPECS.md 5.2). Reporting coverage separately is the honest
	// alternative to a fabricated "Other" slice.
	GenreCoverage float64 `json:"genreCoverage"`

	// ArtistCoverage is the EXACT share of listening time carrying artist attribution.
	//
	// Below 1.0 the artist and album rankings are not merely truncated, they are WRONG: an
	// unattributed play contributes to the total and to no artist, so every artist reads low by
	// a different amount and the ORDER changes. Measured after the history import, the true top
	// artist was absent from the top five while the ones shown read at a quarter of their real
	// totals -- and looked entirely plausible.
	//
	// The UI uses this to say so rather than presenting a quarter of the truth as all of it.
	ArtistCoverage float64 `json:"artistCoverage"`

	// GenresAvailable reports whether any genre data exists to chart. Genres come from
	// MusicBrainz enrichment (§4.5), not from Spotify, which removed its field in February
	// 2026. Derived from the aggregates, so it follows the data rather than a flag.
	GenresAvailable bool `json:"genresAvailable"`

	Notes []string `json:"notes"`
}

// Catalog is the client-side search index (docs/SPECS.md 6.3).
//
// Pairs rather than objects: a few thousand entities compress to a few hundred KB this way, and
// there is no search engine to pay for.
type Catalog struct {
	GeneratedAt string      `json:"generatedAt"`
	Artists     [][2]string `json:"artists"`
	Tracks      [][2]string `json:"tracks"`
	Albums      [][2]string `json:"albums"`
	Genres      []string    `json:"genres"`
}

// RenderSnapshots computes and publishes the static files.
func (r *Rollup) RenderSnapshots(ctx context.Context) (int, error) {
	dash, err := r.buildDashboard(ctx)
	if err != nil {
		return 0, err
	}
	cat, err := r.buildCatalog(ctx)
	if err != nil {
		return 0, err
	}

	written := 0
	for name, doc := range map[string]any{
		FileDashboard: dash,
		FileCatalog:   cat,
		// meta is a subset of the dashboard, published separately so a client can cheaply poll
		// for freshness without downloading the whole page.
		FileMeta: map[string]any{
			"generatedAt":     dash.GeneratedAt,
			"timezone":        dash.Timezone,
			"coverage":        dash.Coverage,
			"allTime":         dash.AllTime,
			"genreCoverage":   dash.GenreCoverage,
			"artistCoverage":  dash.ArtistCoverage,
			"notes":           dash.Notes,
			"genresAvailable": dash.GenresAvailable,
		},
	} {
		body, merr := json.Marshal(doc)
		if merr != nil {
			return written, fmt.Errorf("rollup: marshal %s: %w", name, merr)
		}
		if perr := r.publisher.Publish(ctx, name, body); perr != nil {
			return written, fmt.Errorf("rollup: publish %s: %w", name, perr)
		}
		written++
	}

	// A targeted invalidation: /data/* only. Invalidating /* would evict the whole site's
	// hashed assets, which are immutable and never need it.
	if err := r.publisher.Invalidate(ctx, []string{"/data/*"}); err != nil {
		// The files are already written; a failed invalidation only means the edge serves the
		// previous snapshot until its TTL expires. Not worth failing the run.
		r.log.WarnContext(ctx, "rollup: CDN invalidation failed; the edge will serve the "+
			"previous snapshot until its TTL expires", "err", err)
	}
	return written, nil
}

func (r *Rollup) buildDashboard(ctx context.Context) (Dashboard, error) {
	var d Dashboard
	now := r.now()
	d.GeneratedAt = model.FormatTS(now)
	d.Timezone = r.cal.Name()

	total, err := r.store.GetAggregateOrZero(ctx, model.AggKey{
		Dim: model.DimTotal, Period: model.PeriodAll, EntityID: model.TotalEntityID,
	})
	if err != nil {
		return d, err
	}
	d.AllTime = metricsOf(total)

	// Prefer the coverage row, which a full-history pass produced. The aggregate attributes are
	// set by WRITE order rather than play order, so on an out-of-order ingest they are simply
	// wrong -- and a windowed reconcile cannot fix an all-time bound. Falling back to them is
	// always marked approximate, because they are.
	first, last := total.FirstPlayedAt, total.LastPlayedAt
	approx := true
	if cov, cerr := r.store.GetCoverage(ctx); cerr == nil && !cov.FirstPlayedAt.IsZero() {
		first, last = cov.FirstPlayedAt, cov.LastPlayedAt
		approx = false
		d.GenreCoverage = round4(cov.GenreCoverage())
		d.ArtistCoverage = round4(cov.ArtistCoverage())
	}
	if !first.IsZero() && !last.IsZero() && first.After(last) {
		first, last = last, first
		approx = true
	}
	d.Coverage.FirstPlayedAt = tsPtr(first)
	d.Coverage.LastPlayedAt = tsPtr(last)
	d.Coverage.Approximate = approx

	year := r.cal.Year(now)
	yearAgg, err := r.store.GetAggregateOrZero(ctx, model.AggKey{
		Dim: model.DimTotal, Period: year, EntityID: model.TotalEntityID,
	})
	if err != nil {
		return d, err
	}
	d.CurrentYear.Period = string(year)
	d.CurrentYear.Metrics = metricsOf(yearAgg)

	// Distinct counts come from the all-time partitions: one query per dimension.
	counts := map[model.Dim]int{}
	for _, dim := range []model.Dim{model.DimTrack, model.DimArtist, model.DimAlbum, model.DimGenre} {
		n := 0
		for a, qerr := range r.store.QueryAggregates(ctx, dim, model.PeriodAll, "") {
			if qerr != nil {
				return d, qerr
			}
			if a.Key.Dim != dim {
				continue
			}
			n++
		}
		counts[dim] = n
	}
	d.KPIs.DistinctTracks = counts[model.DimTrack]
	d.KPIs.DistinctArtists = counts[model.DimArtist]
	d.KPIs.DistinctAlbums = counts[model.DimAlbum]
	d.KPIs.DistinctGenres = counts[model.DimGenre]

	// Leaderboards, read from the rows just materialised.
	if d.Top.Artists, err = r.topEntries(ctx, model.DimArtist, model.PeriodAll, 10); err != nil {
		return d, err
	}
	if d.Top.Tracks, err = r.topEntries(ctx, model.DimTrack, model.PeriodAll, 10); err != nil {
		return d, err
	}
	if d.Top.Albums, err = r.topEntries(ctx, model.DimAlbum, model.PeriodAll, 10); err != nil {
		return d, err
	}
	if d.Top.Genres, err = r.topEntries(ctx, model.DimGenre, model.PeriodAll, 12); err != nil {
		return d, err
	}
	if d.TopThisYear.Artists, err = r.topEntries(ctx, model.DimArtist, year, 10); err != nil {
		return d, err
	}
	if d.TopThisYear.Tracks, err = r.topEntries(ctx, model.DimTrack, year, 10); err != nil {
		return d, err
	}

	cal, streak, longest, err := r.buildCalendar(ctx, now)
	if err != nil {
		return d, err
	}
	d.Calendar = cal
	d.KPIs.CurrentStreak = streak
	d.KPIs.LongestStreak = longest

	if d.Rhythm.HourOfDay, err = r.rhythm(ctx, store.HistogramHour, 24); err != nil {
		return d, err
	}
	if d.Rhythm.Weekday, err = r.rhythm(ctx, store.HistogramWeekday, 7); err != nil {
		return d, err
	}

	d.Notes = []string{
		"Podcast episodes are not included: the Spotify recently-played endpoint does not " +
			"report them, so all figures are music only.",
	}
	if d.AllTime.EstimatedRatio > 0 {
		d.Notes = append(d.Notes,
			"Some listening time is estimated. The recently-played endpoint returns no "+
				"duration, so plays captured from it count the track's full length and "+
				"over-count skips.")
	}
	// Artist and album rankings are only trustworthy when nearly every play is attributed.
	// The threshold is deliberately strict: at 95% coverage the top few entries are usually
	// right, but the ORDER further down is not, and a ranking whose order is wrong is worse
	// than one that admits it.
	if d.ArtistCoverage > 0 && d.ArtistCoverage < 0.99 {
		d.Notes = append(d.Notes, fmt.Sprintf(
			"Artist and album figures are INCOMPLETE: only %.0f%% of listening time has artist "+
				"attribution. Imported history identifies artists by name, not by Spotify ID, "+
				"so a play counts towards the totals but towards no artist until its track is "+
				"resolved. Treat the artist and album rankings as provisional; the totals, "+
				"calendar and per-track figures are exact.", d.ArtistCoverage*100))
	}

	// Genres come from MusicBrainz (§4.5), not Spotify: Spotify removed its artist genres
	// field in February 2026 and every artist row carries an empty list, so this dimension
	// had no data at all until enrichment supplied another source.
	//
	// GenresAvailable stays DERIVED rather than hardcoded, so the chart appears and disappears
	// with the data instead of with a flag someone has to remember to flip.
	d.GenresAvailable = len(d.Top.Genres) > 0
	switch {
	case d.GenresAvailable:
		d.Notes = append(d.Notes,
			"A track can belong to several genres at once, so genre figures do not sum to the "+
				"total and must not be read as a part-to-whole breakdown.")
		// The same honesty rule the artist ranking follows, for the same reason. Measured on
		// the real corpus: at 56% coverage the SET of top genres is stable -- splitting the
		// artists into halves and ranking each independently reproduces 7 to 9 of the top ten
		// -- but the ORDER is not, with the two halves disagreeing on first place. So the
		// caveat speaks about order specifically rather than vaguely about completeness.
		if d.GenreCoverage > 0 && d.GenreCoverage < 0.99 {
			d.Notes = append(d.Notes, fmt.Sprintf(
				"Genre figures cover %.0f%% of listening time. Genres come from MusicBrainz, "+
					"which identifies an artist through their Spotify link, so an artist "+
					"imported by name carries none. Which genres appear is reliable; their "+
					"exact order is not.", d.GenreCoverage*100))
		}
	case d.AllTime.Plays > 0:
		d.Notes = append(d.Notes,
			"Genre data is unavailable. Spotify removed the artist genres field from the Web "+
				"API in February 2026, and no artist has been matched to MusicBrainz yet, so "+
				"there is no genre taxonomy to draw from.")
	}
	if approx {
		d.Notes = append(d.Notes,
			"The coverage window is approximate until the next reconcile.")
	}
	return d, nil
}

// topEntries reads a materialised leaderboard, tolerating its absence.
func (r *Rollup) topEntries(
	ctx context.Context, dim model.Dim, period model.Period, limit int,
) ([]Entry, error) {
	board, err := r.store.GetLeaderboard(ctx, dim, period)
	if err != nil {
		// Absent is fine: a period with no listening has no board. Returning an empty slice
		// rather than nil keeps the JSON as [] instead of null, which the frontend can map over.
		return []Entry{}, nil
	}
	out := make([]Entry, 0, limit)
	for i, e := range board.Entries {
		if i >= limit {
			break
		}
		name := e.Name
		if name == "" {
			name = e.ID
		}
		out = append(out, Entry{
			Rank: i + 1, ID: e.ID, Name: name,
			Plays: e.Plays, MsPlayed: e.MsPlayed,
			ImageURL: e.ImageURL, ThumbURL: e.ThumbURL,
			ArtistName: e.ArtistName, AlbumName: e.AlbumName,
		})
	}
	return out, nil
}

// buildCalendar returns a dense trailing-12-month series plus the streak figures.
func (r *Rollup) buildCalendar(
	ctx context.Context, now time.Time,
) (days []DayValue, current, longest int, err error) {
	// Day rows live in their year's partition, so the trailing 12 months needs at most two
	// queries rather than 365 GetItems.
	byDate := map[string]model.Aggregate{}
	years := []model.Period{r.cal.Year(now)}
	if prev := r.cal.Year(now.AddDate(-1, 0, 0)); prev != years[0] {
		years = append(years, prev)
	}
	for _, y := range years {
		for a, qerr := range r.store.QueryAggregates(ctx, model.DimTotal, y, string(y)+"-") {
			if qerr != nil {
				return nil, 0, 0, qerr
			}
			byDate[string(a.Key.Period)] = a
		}
	}

	start := now.AddDate(0, -12, 0)
	cursor := start
	for i := 0; cursor.Before(now.AddDate(0, 0, 1)) && i < 400; i++ {
		day := r.cal.Day(cursor)
		a := byDate[string(day)]
		days = append(days, DayValue{
			Date: string(day), Plays: a.Plays, MsPlayed: a.MsPlayed,
		})
		_, next, berr := r.cal.Bounds(day)
		if berr != nil {
			return nil, 0, 0, berr
		}
		cursor = next
	}

	// Longest streak over the whole window; current streak walking back from today. Today is
	// allowed to be empty without breaking the streak, since it is not over yet.
	run := 0
	for _, d := range days {
		if d.Plays > 0 {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	for i := len(days) - 1; i >= 0; i-- {
		if days[i].Plays > 0 {
			current++
			continue
		}
		// Skip a trailing empty today, then stop at the first real gap.
		if i == len(days)-1 {
			continue
		}
		break
	}
	return days, current, longest, nil
}

func (r *Rollup) rhythm(ctx context.Context, kind store.HistogramKind, buckets int) ([]BucketValue, error) {
	h, err := r.store.GetHistogram(ctx, model.PeriodAll, kind)
	out := make([]BucketValue, 0, buckets)
	if err != nil {
		// No histogram yet: emit dense zeroes so the chart renders an empty axis rather than
		// nothing at all.
		for i := 0; i < buckets; i++ {
			out = append(out, BucketValue{Bucket: i})
		}
		return out, nil
	}
	for i := 0; i < buckets; i++ {
		out = append(out, BucketValue{Bucket: i, Plays: h.Plays[i], MsPlayed: h.MsPlayed[i]})
	}
	return out, nil
}

func (r *Rollup) buildCatalog(ctx context.Context) (Catalog, error) {
	c := Catalog{
		GeneratedAt: model.FormatTS(r.now()),
		Artists:     [][2]string{}, Tracks: [][2]string{}, Albums: [][2]string{},
		Genres: []string{},
	}

	collect := func(dim model.Dim) ([]string, error) {
		var ids []string
		for a, err := range r.store.QueryAggregates(ctx, dim, model.PeriodAll, "") {
			if err != nil {
				return nil, err
			}
			if a.Key.Dim != dim {
				continue
			}
			ids = append(ids, a.Key.EntityID)
		}
		return ids, nil
	}

	artistIDs, err := collect(model.DimArtist)
	if err != nil {
		return c, err
	}
	artists, err := r.store.GetArtists(ctx, artistIDs)
	if err != nil {
		return c, err
	}
	for _, id := range artistIDs {
		c.Artists = append(c.Artists, [2]string{id, nameOr(artists[id].Name, id)})
	}

	trackIDs, err := collect(model.DimTrack)
	if err != nil {
		return c, err
	}
	tracks, err := r.store.GetTracks(ctx, trackIDs)
	if err != nil {
		return c, err
	}
	for _, id := range trackIDs {
		c.Tracks = append(c.Tracks, [2]string{id, nameOr(tracks[id].Name, id)})
	}

	albumIDs, err := collect(model.DimAlbum)
	if err != nil {
		return c, err
	}
	albums, err := r.store.GetAlbums(ctx, albumIDs)
	if err != nil {
		return c, err
	}
	for _, id := range albumIDs {
		c.Albums = append(c.Albums, [2]string{id, nameOr(albums[id].Name, id)})
	}

	genres, err := collect(model.DimGenre)
	if err != nil {
		return c, err
	}
	c.Genres = genres

	// Sorted by name so the client-side index is stable between runs and diffs cleanly.
	sort.Slice(c.Artists, func(i, j int) bool { return c.Artists[i][1] < c.Artists[j][1] })
	sort.Slice(c.Tracks, func(i, j int) bool { return c.Tracks[i][1] < c.Tracks[j][1] })
	sort.Slice(c.Albums, func(i, j int) bool { return c.Albums[i][1] < c.Albums[j][1] })
	sort.Strings(c.Genres)

	return c, nil
}

func metricsOf(a model.Aggregate) Metrics {
	return Metrics{
		Plays: a.Plays, PlaysExact: a.PlaysExact,
		MsPlayed: a.MsPlayed, MsPlayedExact: a.MsPlayedExact,
		EstimatedRatio: round4(a.EstimatedRatio()),
	}
}

func tsPtr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := model.FormatTS(t)
	return &s
}

func round4(f float64) float64 { return float64(int64(f*10000+0.5)) / 10000 }

func nameOr(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}
