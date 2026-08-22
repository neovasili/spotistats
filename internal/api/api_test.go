package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/neovasili/spotistats/internal/api"
	"github.com/neovasili/spotistats/internal/store/storetest"
)

// ---------------------------------------------------------------------------
// conventions
// ---------------------------------------------------------------------------

// Strict validation is the point of docs/SPECS.md 6.2: `?perido=2025` silently ignored would
// return all-time figures that look plausible and are wrong.
func TestUnknownParameterIsRejected(t *testing.T) {
	h := newAPI(t)
	for _, path := range []string{
		"/meta?nope=1",
		"/stats?dim=artist&id=ar1&perido=2025",
		"/top?dim=artist&limit=5&sortby=ms",
		"/list?dim=track&pageSize=10",
		"/plays?trackId=t1&max=5",
		"/timeline?from=2025-12&to=2026-02&granularity=month",
	} {
		t.Run(path, func(t *testing.T) {
			status, code, msg := getErr(t, h, path)
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", status)
			}
			if code != api.CodeUnknownParameter {
				t.Errorf("code = %q, want %q", code, api.CodeUnknownParameter)
			}
			// The message must name the accepted set, or the caller cannot self-correct.
			if !strings.Contains(msg, "Accepted for this endpoint") {
				t.Errorf("message does not list valid parameters: %q", msg)
			}
		})
	}
}

func TestCacheHeaders(t *testing.T) {
	h := newAPI(t)
	rec := get(t, h, "/meta")
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60, s-maxage=3600" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	// CloudFront compresses, so the response varies by encoding.
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q", got)
	}
}

// A cached 400 would be served from the edge for an hour after the caller fixed the typo.
func TestErrorsAreNotCached(t *testing.T) {
	h := newAPI(t)
	rec := get(t, h, "/stats?dim=nonsense&id=x")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestUnknownEndpointUsesTheErrorEnvelope(t *testing.T) {
	h := newAPI(t)
	status, code, _ := getErr(t, h, "/nope")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if code != api.CodeNotFound {
		t.Errorf("code = %q", code)
	}
}

// A write verb on a read-only API must not be silently treated as a read.
func TestNonGetIsRejected(t *testing.T) {
	h := newAPI(t)
	rec := httptestRecorder()
	req := newRequest(http.MethodPost, api.BasePath+"/meta")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /meta = %d, want 405", rec.Code)
	}
}

func TestHealth(t *testing.T) {
	h := newAPI(t)
	rec := get(t, h, "/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("health must not be cached, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// /meta
// ---------------------------------------------------------------------------

func TestMeta(t *testing.T) {
	h := newAPI(t)
	var out api.MetaResponse
	getOK(t, h, "/meta", &out)

	if out.Metrics.Plays != 10 {
		t.Errorf("plays = %d, want the corpus size 10", out.Metrics.Plays)
	}
	if out.Timezone != "Europe/Madrid" {
		t.Errorf("timezone = %q", out.Timezone)
	}
	if out.Coverage.FirstPlayedAt == nil || out.Coverage.LastPlayedAt == nil {
		t.Fatal("coverage window is not reported")
	}
	if *out.Coverage.FirstPlayedAt != "2025-12-15T10:00:00.000Z" {
		t.Errorf("first = %q", *out.Coverage.FirstPlayedAt)
	}
	if out.Capture.LastRunAt == nil {
		t.Error("capture.lastRunAt is not reported")
	}

	// The corpus mixes api and export sources, so the ratio must be strictly between 0 and 1
	// and the caveat must be surfaced.
	if out.Metrics.EstimatedRatio <= 0 || out.Metrics.EstimatedRatio >= 1 {
		t.Errorf("estimatedRatio = %v, want strictly between 0 and 1", out.Metrics.EstimatedRatio)
	}
	var hasEstimateNote, hasPodcastNote bool
	for _, n := range out.Notes {
		if strings.Contains(n, "estimated") {
			hasEstimateNote = true
		}
		if strings.Contains(n, "Podcast") {
			hasPodcastNote = true
		}
	}
	if !hasEstimateNote {
		t.Error("no note explaining the estimated listening time")
	}
	if !hasPodcastNote {
		t.Error("no note that podcasts are excluded")
	}
}

// ---------------------------------------------------------------------------
// /stats -- the canonical query
// ---------------------------------------------------------------------------

// TestStatsCanonicalQuery is docs/SPECS.md 5.3: minutes for one artist in one year.
func TestStatsCanonicalQuery(t *testing.T) {
	h := newAPI(t)
	var out api.StatsResponse
	getOK(t, h, "/stats?dim=artist&id=ar1&period=2026", &out)

	// Local 2026 plays crediting ar1: t2 export 120000 and t1 export 205000.
	if out.Metrics.Plays != 2 {
		t.Errorf("plays = %d, want 2", out.Metrics.Plays)
	}
	if out.Metrics.MsPlayed != 325_000 {
		t.Errorf("msPlayed = %d, want 325000", out.Metrics.MsPlayed)
	}
	// Both are export-sourced, so the figure is exact and the ratio is zero.
	if out.Metrics.MsPlayedExact != out.Metrics.MsPlayed {
		t.Errorf("msPlayedExact = %d, want it to equal msPlayed", out.Metrics.MsPlayedExact)
	}
	if out.Metrics.EstimatedRatio != 0 {
		t.Errorf("estimatedRatio = %v, want 0 for all-exact data", out.Metrics.EstimatedRatio)
	}
	if out.Name != "Artist ar1" {
		t.Errorf("name = %q, want the resolved display name", out.Name)
	}
	if out.Buckets != 1 {
		t.Errorf("buckets = %d, want 1 for a single period", out.Buckets)
	}
}

// Period keys are local, so a play at 23:30Z on New Year's Eve belongs to the next year.
func TestStatsPeriodKeysAreLocal(t *testing.T) {
	h := newAPI(t)
	var y2026, y2025 api.StatsResponse
	getOK(t, h, "/stats?dim=artist&id=ar3&period=2026", &y2026)
	getOK(t, h, "/stats?dim=artist&id=ar3&period=2025", &y2025)

	// ar3's plays: 2025-12-31T23:30Z (local 2026), and two on 2026-02-10.
	if y2026.Metrics.Plays != 3 {
		t.Errorf("2026 plays = %d, want 3", y2026.Metrics.Plays)
	}
	if y2025.Metrics.Plays != 0 {
		t.Errorf("2025 plays = %d, want 0 -- the New Year's Eve play is local 2026",
			y2025.Metrics.Plays)
	}
}

// An entity with no plays in a period is a legitimate zero, not a 404.
func TestStatsAbsentEntityIsZero(t *testing.T) {
	h := newAPI(t)
	var out api.StatsResponse
	getOK(t, h, "/stats?dim=artist&id=does-not-exist&period=2026", &out)
	if out.Metrics.Plays != 0 || out.Metrics.MsPlayed != 0 {
		t.Errorf("metrics = %+v, want zero", out.Metrics)
	}
	if out.First != nil || out.Last != nil {
		t.Error("bounds must be null when there are no plays")
	}
}

func TestStatsRangeSumsBuckets(t *testing.T) {
	h := newAPI(t)
	var ranged, y api.StatsResponse
	getOK(t, h, "/stats?dim=artist&id=ar1&from=2026-01&to=2026-12", &ranged)
	getOK(t, h, "/stats?dim=artist&id=ar1&period=2026", &y)

	// Summing every month of 2026 must equal the year row.
	if ranged.Metrics.Plays != y.Metrics.Plays {
		t.Errorf("range plays = %d, year plays = %d", ranged.Metrics.Plays, y.Metrics.Plays)
	}
	if ranged.Metrics.MsPlayed != y.Metrics.MsPlayed {
		t.Errorf("range ms = %d, year ms = %d", ranged.Metrics.MsPlayed, y.Metrics.MsPlayed)
	}
	if ranged.Buckets != 12 {
		t.Errorf("buckets = %d, want 12", ranged.Buckets)
	}
}

func TestStatsValidation(t *testing.T) {
	h := newAPI(t)
	tests := []struct{ path, wantCode string }{
		{"/stats?id=ar1&period=2025", api.CodeMissingParameter},
		{"/stats?dim=artist&period=2025", api.CodeMissingParameter},
		{"/stats?dim=total&id=x", api.CodeInvalidDimension},
		{"/stats?dim=nope&id=x", api.CodeInvalidDimension},
		{"/stats?dim=artist&id=ar1&period=2025-3", api.CodeInvalidPeriod},
		{"/stats?dim=artist&id=ar1&period=2025&from=2025-01", api.CodeInvalidParameter},
		{"/stats?dim=artist&id=ar1&from=2026-01", api.CodeMissingParameter},
		{"/stats?dim=artist&id=ar1&from=2026-12&to=2026-01", api.CodeInvalidRange},
		{"/stats?dim=artist&id=ar1&from=ALL&to=2026-01", api.CodeInvalidPeriod},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			_, code, _ := getErr(t, h, tc.path)
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// /top
// ---------------------------------------------------------------------------

func TestTopRanksByMetric(t *testing.T) {
	h := newAPI(t)
	var byMs, byPlays api.TopResponse
	getOK(t, h, "/top?dim=artist&period=ALL&metric=ms&limit=10", &byMs)
	getOK(t, h, "/top?dim=artist&period=ALL&metric=plays&limit=10", &byPlays)

	if len(byMs.Items) == 0 {
		t.Fatal("no items")
	}
	// Ranks start at 1 and descend by the chosen measure.
	for i, it := range byMs.Items {
		if it.Rank != i+1 {
			t.Errorf("item %d rank = %d", i, it.Rank)
		}
		if i > 0 && byMs.Items[i-1].Metrics.MsPlayed < it.Metrics.MsPlayed {
			t.Errorf("not descending by ms at %d", i)
		}
	}
	for i := 1; i < len(byPlays.Items); i++ {
		if byPlays.Items[i-1].Metrics.Plays < byPlays.Items[i].Metrics.Plays {
			t.Errorf("not descending by plays at %d", i)
		}
	}
	// No leaderboard has been rendered yet, so the ranking is computed on the fly and says so.
	if byMs.Source != "computed" {
		t.Errorf("source = %q, want computed", byMs.Source)
	}
	if byMs.Items[0].Name == "" {
		t.Error("display names are not resolved")
	}
}

func TestTopRespectsLimit(t *testing.T) {
	h := newAPI(t)
	var out api.TopResponse
	getOK(t, h, "/top?dim=track&period=ALL&limit=2", &out)
	if len(out.Items) != 2 {
		t.Errorf("items = %d, want 2", len(out.Items))
	}
}

// The caveats are part of the response because a client must not build a part-to-whole chart
// on data that has no whole.
func TestTopCarriesDimensionCaveats(t *testing.T) {
	h := newAPI(t)
	for dim, want := range map[string]string{
		"artist": "sum to more",
		"genre":  "many-to-many",
		"album":  "sum to less",
	} {
		var out api.TopResponse
		getOK(t, h, "/top?dim="+dim+"&period=ALL", &out)
		if !strings.Contains(out.Caveat, want) {
			t.Errorf("%s caveat = %q, want it to mention %q", dim, out.Caveat, want)
		}
	}
	// Tracks reconcile exactly, so they need no caveat.
	var tracks api.TopResponse
	getOK(t, h, "/top?dim=track&period=ALL", &tracks)
	if tracks.Caveat != "" {
		t.Errorf("track caveat = %q, want none", tracks.Caveat)
	}
}

// Genre totals exceeding the overall total is correct, not a bug, so the API must not clamp it.
func TestTopGenreSumMayExceedTotal(t *testing.T) {
	h := newAPI(t)
	var out api.TopResponse
	getOK(t, h, "/top?dim=genre&period=ALL&metric=plays&limit=500", &out)

	var sum int64
	for _, it := range out.Items {
		sum += it.Metrics.Plays
	}
	if sum <= out.Total.Plays {
		t.Errorf("genre plays sum to %d against a total of %d; the corpus has multi-genre "+
			"plays so it should exceed the total", sum, out.Total.Plays)
	}
}

// ---------------------------------------------------------------------------
// /list
// ---------------------------------------------------------------------------

func TestListPaginationCoversEveryItemExactlyOnce(t *testing.T) {
	h := newAPI(t)

	var first api.ListResponse
	getOK(t, h, "/list?dim=track&period=ALL&limit=2", &first)
	if first.Total == 0 {
		t.Fatal("no items")
	}
	if first.NextCursor == "" {
		t.Fatal("expected more pages")
	}

	seen := map[string]int{}
	page := first
	pages := 0
	for {
		pages++
		if pages > 20 {
			t.Fatal("pagination did not terminate")
		}
		for _, it := range page.Items {
			seen[it.ID]++
		}
		next := page.NextCursor
		if next == "" {
			break
		}
		page = api.ListResponse{}
		getOK(t, h, "/list?dim=track&period=ALL&limit=2&cursor="+next, &page)
		if len(page.Items) == 0 {
			t.Fatal("a page returned no items but pagination had not finished")
		}
	}

	if len(seen) != first.Total {
		t.Errorf("saw %d distinct items across pages, total says %d", len(seen), first.Total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s appeared %d times across pages", id, n)
		}
	}
}

// A cursor from one query must not silently return a page of another.
func TestListCursorIsQueryScoped(t *testing.T) {
	h := newAPI(t)
	var first api.ListResponse
	getOK(t, h, "/list?dim=track&period=ALL&limit=2", &first)
	if first.NextCursor == "" {
		t.Skip("corpus too small to paginate")
	}

	// Same cursor, different sort: must be refused rather than honoured.
	_, code, _ := getErr(t, h, "/list?dim=track&period=ALL&limit=2&sort=plays&cursor="+first.NextCursor)
	if code != api.CodeInvalidCursor {
		t.Errorf("code = %q, want %q", code, api.CodeInvalidCursor)
	}
	// Same cursor, different dimension: likewise.
	_, code, _ = getErr(t, h, "/list?dim=artist&period=ALL&limit=2&cursor="+first.NextCursor)
	if code != api.CodeInvalidCursor {
		t.Errorf("code = %q, want %q", code, api.CodeInvalidCursor)
	}
}

func TestListSortAndOrder(t *testing.T) {
	h := newAPI(t)
	var desc, asc api.ListResponse
	getOK(t, h, "/list?dim=track&period=ALL&sort=plays&order=desc&limit=500", &desc)
	getOK(t, h, "/list?dim=track&period=ALL&sort=plays&order=asc&limit=500", &asc)

	for i := 1; i < len(desc.Items); i++ {
		if desc.Items[i-1].Metrics.Plays < desc.Items[i].Metrics.Plays {
			t.Errorf("desc not descending at %d", i)
		}
	}
	for i := 1; i < len(asc.Items); i++ {
		if asc.Items[i-1].Metrics.Plays > asc.Items[i].Metrics.Plays {
			t.Errorf("asc not ascending at %d", i)
		}
	}
	if len(desc.Items) != len(asc.Items) {
		t.Errorf("order changed the item count: %d vs %d", len(desc.Items), len(asc.Items))
	}
}

func TestListSortByName(t *testing.T) {
	h := newAPI(t)
	var out api.ListResponse
	getOK(t, h, "/list?dim=track&period=ALL&sort=name&order=asc&limit=500", &out)
	for i := 1; i < len(out.Items); i++ {
		if strings.ToLower(out.Items[i-1].Name) > strings.ToLower(out.Items[i].Name) {
			t.Errorf("names not ascending at %d: %q then %q",
				i, out.Items[i-1].Name, out.Items[i].Name)
		}
	}
}

func TestListTextFilter(t *testing.T) {
	h := newAPI(t)
	var all, filtered api.ListResponse
	getOK(t, h, "/list?dim=track&period=ALL&limit=500", &all)
	getOK(t, h, "/list?dim=track&period=ALL&limit=500&q=t1", &filtered)

	if filtered.Total >= all.Total {
		t.Errorf("filter did not narrow: %d vs %d", filtered.Total, all.Total)
	}
	for _, it := range filtered.Items {
		if !strings.Contains(strings.ToLower(it.Name+it.ID), "t1") {
			t.Errorf("%q does not match the filter", it.Name)
		}
	}
}

func TestListValidation(t *testing.T) {
	h := newAPI(t)
	for _, tc := range []struct{ path, wantCode string }{
		{"/list?dim=track&sort=nonsense", api.CodeInvalidParameter},
		{"/list?dim=track&order=sideways", api.CodeInvalidParameter},
		{"/list?dim=track&limit=0", api.CodeInvalidParameter},
		{"/list?dim=track&limit=abc", api.CodeInvalidParameter},
		{"/list?dim=track&cursor=not-base64!!", api.CodeInvalidCursor},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, code, _ := getErr(t, h, tc.path)
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// A limit above the cap is clamped rather than refused.
func TestListLimitIsClamped(t *testing.T) {
	h := newAPI(t)
	var out api.ListResponse
	getOK(t, h, "/list?dim=track&period=ALL&limit=100000", &out)
	if len(out.Items) > api.MaxLimit {
		t.Errorf("returned %d items, above the cap of %d", len(out.Items), api.MaxLimit)
	}
}

// ---------------------------------------------------------------------------
// /plays
// ---------------------------------------------------------------------------

func TestPlaysByTrack(t *testing.T) {
	h := newAPI(t)
	var out api.PlaysResponse
	getOK(t, h, "/plays?trackId=t1&limit=10", &out)

	if len(out.Items) != 3 {
		t.Fatalf("t1 plays = %d, want 3", len(out.Items))
	}
	for i := 1; i < len(out.Items); i++ {
		if out.Items[i-1].PlayedAt > out.Items[i].PlayedAt {
			t.Errorf("plays not ascending at %d", i)
		}
	}
	// GSI1's INCLUDE projection omits albumId and artistIds, so the response says so rather
	// than letting a client read the absence as "this track has no artists".
	if !out.Partial {
		t.Error("partial = false for a GSI-backed read whose projection omits attributes")
	}
	if out.Items[0].Name == "" {
		t.Error("track name not resolved")
	}
	// The estimated flag must survive to the client.
	var sawEstimated, sawExact bool
	for _, it := range out.Items {
		if it.Estimated {
			sawEstimated = true
		} else {
			sawExact = true
		}
	}
	if !sawEstimated || !sawExact {
		t.Errorf("t1 has both api and export plays; got estimated=%v exact=%v",
			sawEstimated, sawExact)
	}
}

func TestPlaysByRangeIsComplete(t *testing.T) {
	h := newAPI(t)
	var out api.PlaysResponse
	getOK(t, h, "/plays?from=2026-01-01&to=2026-03-01&limit=100", &out)

	if len(out.Items) == 0 {
		t.Fatal("no plays in range")
	}
	if out.Partial {
		t.Error("a base-table read must not be marked partial")
	}
	// The base table has every attribute.
	var sawArtists bool
	for _, it := range out.Items {
		if len(it.Artists) > 0 {
			sawArtists = true
		}
	}
	if !sawArtists {
		t.Error("no play carried artist IDs; the base-table read should be complete")
	}
}

// Exact resume-after pagination: every play exactly once, no duplicates or gaps.
func TestPlaysPaginationIsExact(t *testing.T) {
	h := newAPI(t)

	var all api.PlaysResponse
	getOK(t, h, "/plays?from=2025-01-01&to=2027-01-01&limit=500", &all)
	if len(all.Items) != 10 {
		t.Fatalf("corpus plays = %d, want 10", len(all.Items))
	}

	var seen []string
	page := api.PlaysResponse{}
	path := "/plays?from=2025-01-01&to=2027-01-01&limit=3"
	for i := 0; ; i++ {
		if i > 20 {
			t.Fatal("pagination did not terminate")
		}
		page = api.PlaysResponse{}
		getOK(t, h, path, &page)
		for _, it := range page.Items {
			seen = append(seen, it.PlayedAt+"#"+it.TrackID)
		}
		if page.NextCursor == "" {
			break
		}
		path = "/plays?from=2025-01-01&to=2027-01-01&limit=3&cursor=" + page.NextCursor
	}

	var want []string
	for _, it := range all.Items {
		want = append(want, it.PlayedAt+"#"+it.TrackID)
	}
	if diff := cmp.Diff(want, seen); diff != "" {
		t.Errorf("paginated walk differs from a single read (-want +got):\n%s", diff)
	}
}

func TestPlaysValidation(t *testing.T) {
	h := newAPI(t)
	// An unbounded scan of every play is deliberately not offered.
	_, code, msg := getErr(t, h, "/plays")
	if code != api.CodeMissingParameter {
		t.Errorf("code = %q, want %q", code, api.CodeMissingParameter)
	}
	if !strings.Contains(msg, "trackId") {
		t.Errorf("message should say what is needed: %q", msg)
	}

	_, code, _ = getErr(t, h, "/plays?from=2026-03-01&to=2026-01-01")
	if code != api.CodeInvalidRange {
		t.Errorf("code = %q, want %q", code, api.CodeInvalidRange)
	}
	_, code, _ = getErr(t, h, "/plays?from=not-a-date&to=2026-01-01")
	if code != api.CodeInvalidParameter {
		t.Errorf("code = %q, want %q", code, api.CodeInvalidParameter)
	}
}

// ---------------------------------------------------------------------------
// /timeline
// ---------------------------------------------------------------------------

// The series must be dense: a line chart fed only non-zero points draws a straight line
// across a month of silence.
func TestTimelineIsDense(t *testing.T) {
	h := newAPI(t)
	var out api.TimelineResponse
	getOK(t, h, "/timeline?from=2025-11&to=2026-03&bucket=month", &out)

	want := []string{"2025-11", "2025-12", "2026-01", "2026-02", "2026-03"}
	var got []string
	for _, p := range out.Points {
		got = append(got, p.Period)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("buckets (-want +got):\n%s", diff)
	}
	// 2025-11 has no listening and must still appear, with zeroes.
	if out.Points[0].Metrics.Plays != 0 {
		t.Errorf("2025-11 plays = %d, want 0", out.Points[0].Metrics.Plays)
	}
	if out.Points[1].Metrics.Plays == 0 {
		t.Error("2025-12 should have plays")
	}
}

func TestTimelineTotalMatchesSumOfPoints(t *testing.T) {
	h := newAPI(t)
	var out api.TimelineResponse
	getOK(t, h, "/timeline?from=2025-01&to=2026-12&bucket=month", &out)

	var plays, ms int64
	for _, p := range out.Points {
		plays += p.Metrics.Plays
		ms += p.Metrics.MsPlayed
	}
	if plays != out.Total.Plays {
		t.Errorf("points sum to %d plays, total says %d", plays, out.Total.Plays)
	}
	if ms != out.Total.MsPlayed {
		t.Errorf("points sum to %d ms, total says %d", ms, out.Total.MsPlayed)
	}
	// Every corpus play falls in this range.
	if out.Total.Plays != 10 {
		t.Errorf("total plays = %d, want 10", out.Total.Plays)
	}
}

func TestTimelineForOneEntity(t *testing.T) {
	h := newAPI(t)
	var out api.TimelineResponse
	getOK(t, h, "/timeline?dim=artist&id=ar1&from=2025-12&to=2026-02&bucket=month", &out)
	if out.Name != "Artist ar1" {
		t.Errorf("name = %q", out.Name)
	}
	if len(out.Points) != 3 {
		t.Errorf("points = %d, want 3", len(out.Points))
	}
}

// Day granularity exists only for the overall total; per-entity-per-day rows are not written.
func TestTimelineDayBucketRejectedForEntities(t *testing.T) {
	h := newAPI(t)
	_, code, msg := getErr(t, h, "/timeline?dim=artist&id=ar1&from=2026-01-01&to=2026-01-31&bucket=day")
	if code != api.CodeInvalidParameter {
		t.Errorf("code = %q", code)
	}
	if !strings.Contains(msg, "overall total") {
		t.Errorf("message should explain the restriction: %q", msg)
	}

	// The same request without dim is fine.
	var out api.TimelineResponse
	getOK(t, h, "/timeline?from=2026-01-01&to=2026-01-31&bucket=day", &out)
	if len(out.Points) != 31 {
		t.Errorf("points = %d, want 31 days", len(out.Points))
	}
}

func TestTimelineValidation(t *testing.T) {
	h := newAPI(t)
	for _, tc := range []struct{ path, wantCode string }{
		{"/timeline?to=2026-01", api.CodeMissingParameter},
		{"/timeline?from=2026-01", api.CodeMissingParameter},
		{"/timeline?from=2026-01&to=2026-02&bucket=hour", api.CodeInvalidParameter},
		{"/timeline?from=ALL&to=2026-02", api.CodeInvalidPeriod},
		{"/timeline?id=ar1&from=2026-01&to=2026-02", api.CodeInvalidParameter},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, code, _ := getErr(t, h, tc.path)
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// A range wide enough to exhaust memory must be refused rather than attempted.
func TestTimelineRejectsAbsurdRange(t *testing.T) {
	h := newAPI(t)
	_, code, _ := getErr(t, h, "/timeline?from=1970-01-01&to=2026-12-31&bucket=day")
	if code != api.CodeInvalidRange {
		t.Errorf("code = %q, want %q", code, api.CodeInvalidRange)
	}
}

// TestMetaNeverReportsAnInvertedWindow guards a real defect found by running the API over
// out-of-order data: the write-time aggregate bounds are best-effort (docs/SPECS.md 5.2), so
// a backfill or a replay can leave firstPlayedAt later than lastPlayedAt. Presenting that as
// fact would render as "coverage: 23 Jul to 11 Jul" with no way for a client to tell it was
// nonsense.
func TestMetaNeverReportsAnInvertedWindow(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()

	// Write plays newest-first, which is what makes the write-time bounds disagree with reality.
	for _, instant := range []string{
		"2026-02-10T13:00:00.000Z",
		"2026-01-05T09:00:00.000Z",
		"2025-12-15T10:00:00.000Z",
	} {
		p := storetest.APIPlay(t, instant, "t1", storetest.WithArtists("ar1"))
		if _, err := st.RecordPlay(ctx, p, nil); err != nil {
			t.Fatal(err)
		}
	}

	h := newAPIOver(t, st)
	var out api.MetaResponse
	getOK(t, h, "/meta", &out)

	if out.Coverage.FirstPlayedAt == nil || out.Coverage.LastPlayedAt == nil {
		t.Fatal("coverage not reported")
	}
	if *out.Coverage.FirstPlayedAt > *out.Coverage.LastPlayedAt {
		t.Errorf("inverted window reported: first=%s last=%s",
			*out.Coverage.FirstPlayedAt, *out.Coverage.LastPlayedAt)
	}
	if !out.Coverage.Approximate {
		t.Error("approximate = false despite the bounds having been corrected")
	}
	var explained bool
	for _, n := range out.Notes {
		if strings.Contains(n, "coverage window is approximate") {
			explained = true
		}
	}
	if !explained {
		t.Error("no note explains why the window is approximate")
	}
	// The counters are unaffected by the ordering and must stay exact.
	if out.Metrics.Plays != 3 {
		t.Errorf("plays = %d, want 3", out.Metrics.Plays)
	}
}
