package api_test

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/neovasili/spotistats/internal/api"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
	"github.com/neovasili/spotistats/internal/store/storetest"
)

// newGenreAPI seeds the corpus with what a genre filter actually reads.
//
// Two things the standard harness deliberately does not provide, and both matter:
//
//   - ARTIST#/EXTERNAL rows, via SeedArtistRows. The standard harness puts genres on
//     ARTIST#/META, which is Spotify's field — empty for every artist in production since
//     February 2026. A fixture seeding only META would exercise a path production no longer
//     has: green tests, dead filter.
//   - Track and album rows carrying ARTIST CREDITS. Nothing tags a track with a genre, so the
//     filter reaches genres through the credits; a track row without them can never match.
//
// t6 is the interesting one: ar2 is gothic-only and ar3 is dutch-only, so t6 carries both tags
// while neither of its artists carries both alone.
func newGenreAPI(t *testing.T) *api.Handler {
	t.Helper()
	st := storetest.NewStore(t)
	ctx := context.Background()

	for _, p := range storetest.Corpus(t) {
		if _, err := st.RecordPlay(ctx, p, storetest.GenresFor(p)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// One extra play so the collaboration case has an entity to be about.
	collab := storetest.APIPlay(t, "2026-02-11T12:00:00.000Z", "t6",
		storetest.WithArtists("ar2", "ar3"), storetest.WithDuration(200_000))
	if _, err := st.RecordPlay(ctx, collab, storetest.GenresFor(collab)); err != nil {
		t.Fatalf("seed collab: %v", err)
	}

	storetest.SeedArtistRows(t, st)

	credits := map[string][]string{
		"t1": {"ar1"},
		"t2": {"ar1", "ar2"},
		"t3": {"ar3"},
		"t4": {"ar4"},
		"t5": {"ar4"},
		"t6": {"ar2", "ar3"},
	}
	for id, artists := range credits {
		if err := st.PutTrack(ctx, model.Track{
			ID: id, Name: "Track " + id, DurationMs: 200_000, ArtistIDs: artists,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutAlbum(ctx, model.Album{
		ID: "al1", Name: "The Album", ArtistIDs: []string{"ar1"},
	}); err != nil {
		t.Fatal(err)
	}
	return newAPIOver(t, st)
}

// listGenres returns the IDs a genre-filtered query yields, in response order.
func listGenres(t *testing.T, h *api.Handler, dim, genres, match string) api.ListResponse {
	t.Helper()
	path := "/list?dim=" + dim + "&period=ALL&limit=100&genres=" + url.QueryEscape(genres)
	if match != "" {
		path += "&genreMatch=" + match
	}
	var out api.ListResponse
	getOK(t, h, path, &out)
	return out
}

func ids(r api.ListResponse) []string {
	out := make([]string, 0, len(r.Items))
	for _, i := range r.Items {
		out = append(out, i.ID)
	}
	slices.Sort(out)
	return out
}

func TestGenreFilterKeepsArtistsCarryingTheTag(t *testing.T) {
	h := newGenreAPI(t)
	got := ids(listGenres(t, h, "artist", "gothic metal", ""))
	// ar1 (symphonic + gothic) and ar2 (gothic). Not ar3 (dutch) and not ar4 (none).
	if want := []string{"ar1", "ar2"}; !slices.Equal(got, want) {
		t.Errorf("gothic metal artists = %v, want %v", got, want)
	}
}

func TestGenreFilterAnyIsTheUnion(t *testing.T) {
	h := newGenreAPI(t)
	got := ids(listGenres(t, h, "artist", "gothic metal,dutch metal", "any"))
	if want := []string{"ar1", "ar2", "ar3"}; !slices.Equal(got, want) {
		t.Errorf("any(gothic, dutch) = %v, want %v", got, want)
	}
	// "any" is the default, so omitting genreMatch must give the identical answer.
	if def := ids(listGenres(t, h, "artist", "gothic metal,dutch metal", "")); !slices.Equal(def, got) {
		t.Errorf("default match = %v, want the same as any = %v", def, got)
	}
}

func TestGenreFilterAllIsTheIntersection(t *testing.T) {
	h := newGenreAPI(t)
	// Only ar1 carries both.
	if got, want := ids(listGenres(t, h, "artist", "symphonic metal,gothic metal", "all")),
		[]string{"ar1"}; !slices.Equal(got, want) {
		t.Errorf("all(symphonic, gothic) = %v, want %v", got, want)
	}
	// No single artist is both gothic and dutch, so the intersection is empty -- and an empty
	// result is a legitimate answer, not an error.
	if got := ids(listGenres(t, h, "artist", "gothic metal,dutch metal", "all")); len(got) != 0 {
		t.Errorf("all(gothic, dutch) = %v, want nothing", got)
	}
}

// An artist with no genres is absent, not silently kept.
//
// Keeping it would make a filtered list read as complete when 16% of listening time carries no
// genre at all -- the single most misleading thing this filter could do.
func TestGenreFilterExcludesUnknownGenres(t *testing.T) {
	h := newGenreAPI(t)
	for _, sel := range []string{"gothic metal", "dutch metal", "symphonic metal"} {
		for _, id := range ids(listGenres(t, h, "artist", sel, "any")) {
			if id == "ar4" {
				t.Errorf("%q kept ar4, which has no genres at all", sel)
			}
		}
	}
}

// Genres label artists, so a track matches through its credits.
func TestGenreFilterReachesTracksThroughTheirArtists(t *testing.T) {
	h := newGenreAPI(t)
	got := ids(listGenres(t, h, "track", "symphonic metal", "any"))
	// t1 (ar1) and t2 (ar1 + ar2). Not t3/t6 (no symphonic artist), not t4/t5 (ar4, no genres).
	if want := []string{"t1", "t2"}; !slices.Equal(got, want) {
		t.Errorf("symphonic metal tracks = %v, want %v", got, want)
	}
}

// A collaboration carries the UNION of its artists' tags, not the intersection.
//
// t6 is ar2 (gothic only) with ar3 (dutch only): the recording really does carry both labels
// even though neither artist does alone. Testing each artist separately would drop it.
func TestGenreFilterUnionsACollaborationsArtists(t *testing.T) {
	h := newGenreAPI(t)
	got := ids(listGenres(t, h, "track", "gothic metal,dutch metal", "all"))
	if want := []string{"t6"}; !slices.Equal(got, want) {
		t.Errorf("all(gothic, dutch) tracks = %v, want %v", got, want)
	}
}

func TestGenreFilterNormalisesTheSelection(t *testing.T) {
	h := newGenreAPI(t)
	base := ids(listGenres(t, h, "artist", "gothic metal", ""))
	// Case, padding and repeated whitespace are the same tag, and a duplicate is not a second
	// filter. model.NormalizeGenre is the shared rule with the aggregation side.
	for _, raw := range []string{"Gothic Metal", "  gothic   metal ", "gothic metal,gothic metal"} {
		if got := ids(listGenres(t, h, "artist", raw, "")); !slices.Equal(got, base) {
			t.Errorf("%q = %v, want the same as %q = %v", raw, got, "gothic metal", base)
		}
	}
}

// The totals describe the whole filtered set, which is the entire reason they are computed
// server-side: a client can only sum the rows it has been sent.
func TestListTotalsCoverEveryMatchNotJustThePage(t *testing.T) {
	h := newGenreAPI(t)
	full := listGenres(t, h, "artist", "gothic metal,dutch metal", "any")
	if full.Total != 3 {
		t.Fatalf("total = %d, want 3", full.Total)
	}

	var page api.ListResponse
	getOK(t, h, "/list?dim=artist&period=ALL&limit=1&genres="+
		url.QueryEscape("gothic metal,dutch metal"), &page)
	if len(page.Items) != 1 {
		t.Fatalf("page returned %d items, want 1", len(page.Items))
	}
	if page.Totals != full.Totals {
		t.Errorf("one-row page totals = %+v, want the whole set's %+v", page.Totals, full.Totals)
	}
	// And they must be the sum of the rows, not of something else.
	var sum int64
	for _, i := range full.Items {
		sum += i.Metrics.MsPlayed
	}
	if page.Totals.MsPlayed != sum {
		t.Errorf("totals.msPlayed = %d, want the row sum %d", page.Totals.MsPlayed, sum)
	}
}

// Unfiltered totals must equal the dimension's own sum, so the strip is trustworthy before a
// filter is ever applied.
func TestListTotalsWithoutAFilterSumTheWholeDimension(t *testing.T) {
	h := newGenreAPI(t)
	var out api.ListResponse
	getOK(t, h, "/list?dim=track&period=ALL&limit=2", &out)

	var all api.ListResponse
	getOK(t, h, "/list?dim=track&period=ALL&limit=200", &all)
	var sum int64
	var plays int64
	for _, i := range all.Items {
		sum += i.Metrics.MsPlayed
		plays += i.Metrics.Plays
	}
	if out.Totals.MsPlayed != sum || out.Totals.Plays != plays {
		t.Errorf("totals = %d ms / %d plays, want %d / %d",
			out.Totals.MsPlayed, out.Totals.Plays, sum, plays)
	}
}

// A cursor issued for one genre selection must not paginate another.
func TestListCursorIsScopedToTheGenreFilter(t *testing.T) {
	h := newGenreAPI(t)
	var first api.ListResponse
	getOK(t, h, "/list?dim=track&period=ALL&limit=1", &first)
	if first.NextCursor == "" {
		t.Skip("corpus too small to paginate")
	}
	_, code, _ := getErr(t, h, "/list?dim=track&period=ALL&limit=1&genres="+
		url.QueryEscape("gothic metal")+"&cursor="+first.NextCursor)
	if code != api.CodeInvalidCursor {
		t.Errorf("code = %q, want %q", code, api.CodeInvalidCursor)
	}
}

// Selection ORDER must not change the pagination identity: "rock,metal" and "metal,rock" are one
// query, and a cursor from either has to work on the other.
func TestGenreFilterOrderDoesNotChangeTheCursor(t *testing.T) {
	h := newGenreAPI(t)
	var a api.ListResponse
	getOK(t, h, "/list?dim=artist&period=ALL&limit=1&genres="+
		url.QueryEscape("gothic metal,dutch metal"), &a)
	if a.NextCursor == "" {
		t.Skip("selection too small to paginate")
	}
	var b api.ListResponse
	getOK(t, h, "/list?dim=artist&period=ALL&limit=1&genres="+
		url.QueryEscape("dutch metal,gothic metal")+"&cursor="+a.NextCursor, &b)
	if len(b.Items) == 0 {
		t.Error("a cursor from one ordering was refused by the other; the fingerprint is order-sensitive")
	}
}

func TestGenreFilterValidation(t *testing.T) {
	h := newGenreAPI(t)
	// Thirty DISTINCT tags. The cap counts distinct values, applied after normalisation and
	// dedup, so a URL repeating one tag thirty times is one filter rather than thirty.
	many := make([]string, 0, 30)
	for i := range 30 {
		many = append(many, "genre"+strconv.Itoa(i))
	}
	for _, tc := range []struct{ path, wantCode string }{
		{"/list?dim=track&genreMatch=either", api.CodeInvalidParameter},
		{"/list?dim=track&genres=" + url.QueryEscape(strings.Join(many, ",")), api.CodeInvalidParameter},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, code, _ := getErr(t, h, tc.path)
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// Repetition is not a filter count: the cap must not reject a URL that names one tag many times.
func TestGenreFilterCapCountsDistinctTags(t *testing.T) {
	h := newGenreAPI(t)
	var out api.ListResponse
	getOK(t, h, "/list?dim=artist&period=ALL&limit=100&genres="+
		url.QueryEscape(strings.TrimSuffix(strings.Repeat("gothic metal,", 30), ",")), &out)
	if out.Total != 2 {
		t.Errorf("total = %d, want the 2 gothic-metal artists", out.Total)
	}
}

// An empty or whitespace-only selection is no filter, not an error.
func TestGenreFilterEmptySelectionIsNotAFilter(t *testing.T) {
	h := newGenreAPI(t)
	var unfiltered api.ListResponse
	getOK(t, h, "/list?dim=track&period=ALL&limit=100", &unfiltered)
	for _, raw := range []string{"", " ", ",,", "  ,  "} {
		var out api.ListResponse
		getOK(t, h, "/list?dim=track&period=ALL&limit=100&genres="+url.QueryEscape(raw), &out)
		if out.Total != unfiltered.Total {
			t.Errorf("genres=%q gave %d items, want the unfiltered %d",
				raw, out.Total, unfiltered.Total)
		}
	}
}

// The response must say that a genre-filtered track list is "tracks by artists tagged X".
//
// It is a different question from "tracks tagged X", and substituting one for the other silently
// is exactly the kind of thing this codebase states rather than hides.
func TestGenreFilterCaveatNamesTheSubstitution(t *testing.T) {
	h := newGenreAPI(t)
	tracks := listGenres(t, h, "track", "gothic metal", "")
	if !strings.Contains(tracks.Caveat, "tracks by artists") {
		t.Errorf("track caveat does not name the artist substitution: %q", tracks.Caveat)
	}
	if !strings.Contains(tracks.Caveat, "gothic metal") {
		t.Errorf("caveat does not name the selection: %q", tracks.Caveat)
	}
	// The dimension's own caveat must survive alongside it.
	artists := listGenres(t, h, "artist", "gothic metal", "")
	if !strings.Contains(artists.Caveat, "several artists is credited to each") {
		t.Errorf("the dimension caveat was dropped when a genre filter was added: %q", artists.Caveat)
	}
	// And with no filter there must be no genre wording at all.
	var plain api.ListResponse
	getOK(t, h, "/list?dim=track&period=ALL", &plain)
	if strings.Contains(plain.Caveat, "tagged with") {
		t.Errorf("unfiltered list carries a genre caveat: %q", plain.Caveat)
	}
}

// ResolveLabels must return every credited artist, not only the primary one -- the filter reads
// this, and a primary-only list would drop collaborations.
func TestResolveLabelsCarriesEveryCreditedArtist(t *testing.T) {
	st := storetest.NewStore(t)
	ctx := context.Background()
	if err := st.PutTrack(ctx, model.Track{
		ID: "t1", Name: "Track t1", ArtistIDs: []string{"ar1", "ar2"},
	}); err != nil {
		t.Fatal(err)
	}
	labels, err := st.ResolveLabels(ctx, model.DimTrack, []string{"t1"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := labels["t1"].ArtistIDs, []string{"ar1", "ar2"}; !slices.Equal(got, want) {
		t.Errorf("ArtistIDs = %v, want %v", got, want)
	}

	// For the artist dimension the label carries the artist's own ID, so a caller can treat
	// every dimension identically.
	if err := st.PutArtist(ctx, model.Artist{ID: "ar1", Name: "Artist ar1"}); err != nil {
		t.Fatal(err)
	}
	al, err := st.ResolveLabels(ctx, model.DimArtist, []string{"ar1"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := al["ar1"].ArtistIDs, []string{"ar1"}; !slices.Equal(got, want) {
		t.Errorf("artist label ArtistIDs = %v, want %v", got, want)
	}
}

var _ = store.Label{}
