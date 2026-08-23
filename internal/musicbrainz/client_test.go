package musicbrainz_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/neovasili/spotistats/internal/httpx"
	"github.com/neovasili/spotistats/internal/httpx/httpxtest"
	"github.com/neovasili/spotistats/internal/musicbrainz"
	"github.com/neovasili/spotistats/internal/musicbrainz/musicbrainztest"
)

const testUA = "spotistats-test/1.0 ( https://example.test )"

func fixture(t *testing.T, name string) string {
	t.Helper()
	return musicbrainztest.Fixture(t, "testdata", name)
}

func newClient(t *testing.T, srv *musicbrainztest.Server) *musicbrainz.Client {
	t.Helper()
	c, err := musicbrainz.New(musicbrainz.Config{
		UserAgent: testUA,
		BaseURL:   srv.URL,
		// A fake clock keeps the 1 req/s limiter from making the suite take a second per
		// request. It must ADVANCE on Sleep: a clock frozen at one instant makes the window
		// limiter wait forever, because time never passes and the window never clears.
		// The limiter's own behaviour is tested in internal/httpx.
		Clock: httpxtest.NewFakeClock(time.Unix(0, 0).UTC()),
	})
	if err != nil {
		t.Fatalf("musicbrainz.New: %v", err)
	}
	return c
}

// TestNewRequiresAContactUserAgent: MusicBrainz throttles anonymous and default library agents
// far harder as a class, so a client that silently sent a default would pass every test and be
// throttled into uselessness in production.
func TestNewRequiresAContactUserAgent(t *testing.T) {
	for _, ua := range []string{"", "spotistats/1.0", "Go-http-client/2.0"} {
		_, err := musicbrainz.New(musicbrainz.Config{UserAgent: ua})
		if err == nil {
			t.Errorf("New accepted User-Agent %q", ua)
			continue
		}
		if !errors.Is(err, httpx.ErrUserAgentRequired) {
			t.Errorf("New(%q) error does not wrap ErrUserAgentRequired: %v", ua, err)
		}
	}
}

func TestSendsTheContactUserAgent(t *testing.T) {
	srv := musicbrainztest.New(t, musicbrainztest.ServeJSON(fixture(t, "url_batch.json")))
	c := newClient(t, srv)
	if _, err := c.ResolveSpotifyArtists(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if got := srv.UserAgents(); len(got) == 0 || got[0] != testUA {
		t.Errorf("User-Agent = %v, want %q", got, testUA)
	}
}

// TestResolveBatchIsKeyedByResourceNotPosition guards a mis-resolution nothing downstream
// could detect.
//
// MusicBrainz does NOT preserve request order: the golden here was captured from a real
// request for [Within Temptation, Disturbed] and came back in the opposite order. Reading the
// response positionally would attach Disturbed's MBID to Within Temptation — a confident,
// wrong profile for a real band, which is exactly what this package refuses to risk.
func TestResolveBatchIsKeyedByResourceNotPosition(t *testing.T) {
	srv := musicbrainztest.New(t, musicbrainztest.ServeJSON(fixture(t, "url_batch.json")))
	c := newClient(t, srv)

	got, err := c.ResolveSpotifyArtists(context.Background(),
		[]string{"3hE8S8ohRErocpkY7uJW4a", "3TOqt5oJwL9BE2NG9MEwDa"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"3hE8S8ohRErocpkY7uJW4a": "eace2373-31c8-4aba-9a5c-7bce22dd140a",
		"3TOqt5oJwL9BE2NG9MEwDa": "4bb4e4e4-5f66-4509-98af-62dbb90c45c5",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

// TestResolveHandlesTheSingleResourceShape is the case a client written against the batch
// shape alone gets silently wrong.
//
// With exactly ONE `resource` parameter MusicBrainz returns the bare URL entity — relations at
// the top level, no `urls` wrapper. That is not an exotic input: it is the tail chunk of any
// artist count that is not a multiple of 100, and every single-artist run.
func TestResolveHandlesTheSingleResourceShape(t *testing.T) {
	srv := musicbrainztest.New(t, musicbrainztest.ServeJSON(fixture(t, "url_single.json")))
	c := newClient(t, srv)

	got, err := c.ResolveSpotifyArtists(context.Background(), []string{"3hE8S8ohRErocpkY7uJW4a"})
	if err != nil {
		t.Fatal(err)
	}
	if got["3hE8S8ohRErocpkY7uJW4a"] != "eace2373-31c8-4aba-9a5c-7bce22dd140a" {
		t.Errorf("single-resource lookup returned %v; the batch shape was assumed", got)
	}
}

func TestResolveBatchesAtOneHundred(t *testing.T) {
	srv := musicbrainztest.New(t, musicbrainztest.ServeJSON(`{"url-count":0,"urls":[]}`))
	c := newClient(t, srv)

	ids := make([]string, 250)
	for i := range ids {
		ids[i] = string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if _, err := c.ResolveSpotifyArtists(context.Background(), ids); err != nil {
		t.Fatal(err)
	}
	// 250 unique IDs at 100 per request. Batching is the difference between a 20-second job
	// and a 30-minute one at 1 req/s, so the ceiling is asserted rather than assumed.
	if got := srv.Calls(); got != 3 {
		t.Errorf("requests = %d, want 3 for 250 ids", got)
	}
	for _, u := range srv.URLs() {
		if n := len(u.Query()["resource"]); n > 100 {
			t.Errorf("a request carried %d resources, over the 100 ceiling", n)
		}
	}
}

func TestResolveDedupesAndSkipsBlanks(t *testing.T) {
	srv := musicbrainztest.New(t, musicbrainztest.ServeJSON(`{"url-count":0,"urls":[]}`))
	c := newClient(t, srv)
	if _, err := c.ResolveSpotifyArtists(context.Background(),
		[]string{"a", "a", "", "b", "b"}); err != nil {
		t.Fatal(err)
	}
	if got := len(srv.URLs()[0].Query()["resource"]); got != 2 {
		t.Errorf("sent %d resources, want 2 after dedup; a duplicate wastes a whole batch slot", got)
	}
}

// An artist with no asserted Spotify relationship is simply absent. There is no name-search
// fallback anywhere in this package, deliberately.
func TestUnlinkedArtistIsAbsentNotGuessed(t *testing.T) {
	srv := musicbrainztest.New(t, musicbrainztest.ServeJSON(
		`{"url-count":1,"urls":[{"resource":"https://open.spotify.com/artist/x","relations":[]}]}`))
	c := newClient(t, srv)
	got, err := c.ResolveSpotifyArtists(context.Background(), []string{"x", "y"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want nothing resolved", got)
	}
}

func TestArtistLookup(t *testing.T) {
	srv := musicbrainztest.New(t, musicbrainztest.ServeJSON(fixture(t, "artist_group.json")))
	c := newClient(t, srv)

	got, found, err := c.Artist(context.Background(), "eace2373-31c8-4aba-9a5c-7bce22dd140a")
	if err != nil || !found {
		t.Fatalf("Artist = %v, found=%v, err=%v", got, found, err)
	}
	u := srv.URLs()[0]
	if u.Query().Get("inc") != "genres+artist-rels" {
		t.Errorf("inc = %q; genres and relations are both needed in one call", u.Query().Get("inc"))
	}
}

// A 404 is an answer, not a failure: the caller tombstones the MBID rather than retrying it.
func TestArtistNotFoundIsNotAnError(t *testing.T) {
	srv := musicbrainztest.New(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	c := newClient(t, srv)
	_, found, err := c.Artist(context.Background(), "gone")
	if err != nil {
		t.Fatalf("a 404 must not be an error: %v", err)
	}
	if found {
		t.Error("found = true for a 404")
	}
}

// TestServiceUnavailableIsRetried: MusicBrainz answers 503 to roughly half of all requests
// even within its own rate limit, so 503 is backpressure to wait out, not a failure to report.
func TestServiceUnavailableIsRetried(t *testing.T) {
	var n int
	srv := musicbrainztest.New(t, func(w http.ResponseWriter, _ *http.Request) {
		n++
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"eace2373","name":"Within Temptation","type":"Group"}`))
	})
	c := newClient(t, srv)

	got, found, err := c.Artist(context.Background(), "eace2373")
	if err != nil {
		t.Fatalf("503 was surfaced instead of retried: %v", err)
	}
	if !found || got.Name != "Within Temptation" {
		t.Errorf("got %+v", got)
	}
	if n != 3 {
		t.Errorf("attempts = %d, want 3", n)
	}
}
