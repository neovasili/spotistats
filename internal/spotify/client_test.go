package spotify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/spotify/spotifytest"
)

// ---------------------------------------------------------------------------
// test server
// ---------------------------------------------------------------------------

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// recordingServer serves fixtures and records every request URL it saw. It uses a real
// httptest.Server so header handling, query encoding and JSON decoding all go through
// net/http rather than a fake.
type recordingServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*url.URL
	auths    []string
	handler  func(w http.ResponseWriter, r *http.Request)
}

func newRecordingServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *recordingServer {
	t.Helper()
	rs := &recordingServer{handler: handler}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		u := *r.URL
		rs.requests = append(rs.requests, &u)
		rs.auths = append(rs.auths, r.Header.Get("Authorization"))
		rs.mu.Unlock()
		rs.handler(w, r)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *recordingServer) URLs() []*url.URL {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]*url.URL(nil), rs.requests...)
}

func (rs *recordingServer) Auths() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.auths...)
}

func (rs *recordingServer) Calls() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.requests)
}

// serveJSON returns a handler that always serves body.
func serveJSON(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func newTestClient(t *testing.T, srv *recordingServer, ts TokenSource) *Client {
	t.Helper()
	if ts == nil {
		ts = spotifytest.StaticTokenSource("access-token")
	}
	p := DefaultRetryPolicy()
	p.Rand = spotifytest.FixedRand(1.0)
	c, err := New(Config{
		TokenSource: ts,
		BaseURL:     srv.URL + "/v1",
		Retry:       p,
		Clock:       spotifytest.NewFakeClock(epoch),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------------
// client basics
// ---------------------------------------------------------------------------

func TestNewClientValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New accepted a config with no TokenSource")
	}
	c, err := New(Config{TokenSource: spotifytest.StaticTokenSource("t")})
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL.String() != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, DefaultBaseURL)
	}
}

func TestClientResolvePreservesBasePath(t *testing.T) {
	c, err := New(Config{TokenSource: spotifytest.StaticTokenSource("t"), BaseURL: "https://api.spotify.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.resolve("me/player/recently-played", url.Values{"limit": {"50"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://api.spotify.com/v1/me/player/recently-played?limit=50"
	if got != want {
		t.Errorf("resolve = %q, want %q", got, want)
	}
}

func TestClientSendsBearerAndUserAgent(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_empty.json")))
	c := newTestClient(t, srv, nil)

	if _, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{}); err != nil {
		t.Fatal(err)
	}
	auths := srv.Auths()
	if len(auths) != 1 || auths[0] != "Bearer access-token" {
		t.Errorf("Authorization = %v, want a single Bearer header", auths)
	}
}

// invalidatingTokenSource counts refreshes and rotates the token each time, so a test can
// prove the client obtained a NEW token after a 401 rather than reusing the stale one.
type invalidatingTokenSource struct {
	mu            sync.Mutex
	n             int
	invalidations int
}

func (s *invalidatingTokenSource) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == 0 {
		s.n = 1
	}
	return fmt.Sprintf("token-%d", s.n), nil
}

func (s *invalidatingTokenSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidations++
	s.n++
}

func (s *invalidatingTokenSource) Invalidations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidations
}

// TestClient401ForcesOneRefreshAndRetries: a 401 is an authorisation problem, so the
// remedy is a new token, not a wait. It must be retried exactly once.
func TestClient401ForcesOneRefreshAndRetries(t *testing.T) {
	var n int
	var mu sync.Mutex
	srv := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"status":401,"message":"The access token expired"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixture(t, "recently_played_empty.json")))
	})

	ts := &invalidatingTokenSource{}
	c := newTestClient(t, srv, ts)

	if _, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{}); err != nil {
		t.Fatalf("RecentlyPlayed: %v", err)
	}
	if got := srv.Calls(); got != 2 {
		t.Errorf("HTTP calls = %d, want 2 (the 401 plus one retry)", got)
	}
	if got := ts.Invalidations(); got != 1 {
		t.Errorf("Invalidate calls = %d, want 1", got)
	}
	auths := srv.Auths()
	if len(auths) == 2 && auths[0] == auths[1] {
		t.Errorf("retry reused the stale token %q; it must fetch a fresh one", auths[0])
	}
}

// A second 401 with a freshly minted token means the grant is gone; do not loop.
func TestClientRepeated401GivesUp(t *testing.T) {
	srv := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"status":401,"message":"Invalid access token"}}`))
	})
	ts := &invalidatingTokenSource{}
	c := newTestClient(t, srv, ts)

	_, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{})
	if err == nil {
		t.Fatal("expected failure")
	}
	var api *APIError
	if !errors.As(err, &api) || api.StatusCode != 401 {
		t.Errorf("err = %v, want *APIError with 401", err)
	}
	if got := srv.Calls(); got != 2 {
		t.Errorf("HTTP calls = %d, want exactly 2 -- no unbounded refresh loop", got)
	}
}

// ---------------------------------------------------------------------------
// recently-played
// ---------------------------------------------------------------------------

// TestRecentlyPlayedNormalisesToOldestFirst is the invariant every caller depends on:
// Spotify returns newest-first, ingestion must advance a cursor forward.
func TestRecentlyPlayedNormalisesToOldestFirst(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_3.json")))
	c := newTestClient(t, srv, nil)

	page, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Plays) != 3 {
		t.Fatalf("plays = %d, want 3", len(page.Plays))
	}

	wantOrder := []string{"t1", "t2", "t3"} // the fixture lists t3, t2, t1
	got := make([]string, 0, 3)
	for _, p := range page.Plays {
		got = append(got, p.TrackID)
	}
	if diff := cmp.Diff(wantOrder, got); diff != "" {
		t.Errorf("play order (-want +got):\n%s", diff)
	}
	for i := 1; i < len(page.Plays); i++ {
		if !page.Plays[i-1].PlayedAt.Before(page.Plays[i].PlayedAt) {
			t.Errorf("plays not strictly ascending at %d", i)
		}
	}

	if got, want := model.FormatTS(page.OldestPlayedAt), "2025-03-14T21:04:33.123Z"; got != want {
		t.Errorf("OldestPlayedAt = %s, want %s", got, want)
	}
	if got, want := model.FormatTS(page.NewestPlayedAt), "2025-03-14T22:10:00.500Z"; got != want {
		t.Errorf("NewestPlayedAt = %s, want %s", got, want)
	}
}

// Every API play is estimated, with msPlayed taken from the embedded track duration --
// the endpoint carries no duration of its own.
func TestRecentlyPlayedPlaysAreEstimatedFromTrackDuration(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_3.json")))
	c := newTestClient(t, srv, nil)

	page, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range page.Plays {
		if p.Source != model.SourceAPI {
			t.Errorf("%s Source = %q, want api", p.TrackID, p.Source)
		}
		if !p.MsEstimated {
			t.Errorf("%s MsEstimated = false; the endpoint returns no duration", p.TrackID)
		}
		tr, ok := page.Tracks[p.TrackID]
		if !ok {
			t.Fatalf("track %s missing from the Tracks map", p.TrackID)
		}
		if tr.DurationMs == 0 {
			t.Errorf("track %s has no duration; the estimate would be meaningless", p.TrackID)
		}
		if p.MsPlayed != tr.DurationMs {
			t.Errorf("%s MsPlayed = %d, want the track duration %d", p.TrackID, p.MsPlayed, tr.DurationMs)
		}
	}
}

func TestRecentlyPlayedCarriesEmbeddedObjects(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_3.json")))
	c := newTestClient(t, srv, nil)

	page, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tracks) != 3 {
		t.Errorf("Tracks = %d, want 3", len(page.Tracks))
	}
	// All three fixture tracks share one album, so the map collapses to a single entry.
	if len(page.Albums) != 1 {
		t.Errorf("Albums = %d, want 1", len(page.Albums))
	}
	al, ok := page.Albums["al1"]
	if !ok {
		t.Fatal("album al1 missing")
	}
	if al.ReleaseDate != "2014-10-24" || al.ReleaseDatePrecision != "day" {
		t.Errorf("album release = (%q, %q)", al.ReleaseDate, al.ReleaseDatePrecision)
	}
	// Spotify orders images widest-first.
	if al.ImageURL != "https://i.scdn.co/image/big" {
		t.Errorf("ImageURL = %q, want the widest image", al.ImageURL)
	}
	// Multi-artist tracks keep every artist.
	if got := page.Tracks["t3"].ArtistIDs; len(got) != 2 {
		t.Errorf("t3 ArtistIDs = %v, want two", got)
	}
	if got := page.Tracks["t1"].ISRC; got != "ESAAA2500001" {
		t.Errorf("ISRC = %q", got)
	}
}

// TestRecentlyPlayedSaturationSignal is the gap detector: a full page means listening may
// have outrun the polling window and plays may be permanently lost.
func TestRecentlyPlayedSaturationSignal(t *testing.T) {
	t.Run("full page sets Saturated", func(t *testing.T) {
		srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_50.json")))
		c := newTestClient(t, srv, nil)
		page, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if !page.Saturated {
			t.Error("Saturated = false for a 50-item response at limit 50")
		}
	})

	t.Run("partial page does not", func(t *testing.T) {
		srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_3.json")))
		c := newTestClient(t, srv, nil)
		page, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if page.Saturated {
			t.Error("Saturated = true for 3 items at limit 50")
		}
	})

	// Saturation compares against the REQUESTED limit, not the literal 50, so the
	// condition is reproducible in a test without a 50-item fixture.
	t.Run("compares against the requested limit", func(t *testing.T) {
		srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_3.json")))
		c := newTestClient(t, srv, nil)
		page, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		if !page.Saturated {
			t.Error("Saturated = false for 3 items at limit 3")
		}
	})
}

func TestRecentlyPlayedCursors(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_3.json")))
	c := newTestClient(t, srv, nil)

	page, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasNext {
		t.Error("HasNext = false despite a next URL in the payload")
	}
	if want := model.FromUnixMillis(1741989000500); !page.NextAfter.Equal(want) {
		t.Errorf("NextAfter = %v, want %v", page.NextAfter, want)
	}
	if want := model.FromUnixMillis(1741986273123); !page.NextBefore.Equal(want) {
		t.Errorf("NextBefore = %v, want %v", page.NextBefore, want)
	}
}

// The cursor is a Unix MILLISECOND timestamp, not seconds and not RFC3339.
func TestRecentlyPlayedSendsAfterAsUnixMillis(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_empty.json")))
	c := newTestClient(t, srv, nil)

	after := model.FromUnixMillis(1741986273123)
	if _, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{Limit: 10, After: after}); err != nil {
		t.Fatal(err)
	}
	q := srv.URLs()[0].Query()
	if got := q.Get("after"); got != "1741986273123" {
		t.Errorf("after = %q, want 1741986273123", got)
	}
	if got := q.Get("limit"); got != "10" {
		t.Errorf("limit = %q, want 10", got)
	}
	if q.Has("before") {
		t.Error("before must not be sent alongside after")
	}
}

func TestRecentlyPlayedSendsBefore(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_empty.json")))
	c := newTestClient(t, srv, nil)

	before := model.FromUnixMillis(1741986273123)
	if _, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{Before: before}); err != nil {
		t.Fatal(err)
	}
	q := srv.URLs()[0].Query()
	if got := q.Get("before"); got != "1741986273123" {
		t.Errorf("before = %q", got)
	}
	if q.Has("after") {
		t.Error("after must not be sent alongside before")
	}
}

func TestRecentlyPlayedCursorConflict(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_empty.json")))
	c := newTestClient(t, srv, nil)

	now := model.FromUnixMillis(1741986273123)
	_, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{After: now, Before: now})
	if !errors.Is(err, ErrCursorConflict) {
		t.Errorf("err = %v, want ErrCursorConflict", err)
	}
	if srv.Calls() != 0 {
		t.Error("a conflicting cursor pair must be rejected before any HTTP call")
	}
}

func TestRecentlyPlayedLimitClamping(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{0, "50"}, {-1, "50"}, {1, "1"}, {50, "50"}, {51, "50"}, {1000, "50"},
	} {
		srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_empty.json")))
		c := newTestClient(t, srv, nil)
		if _, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{Limit: tc.in}); err != nil {
			t.Fatal(err)
		}
		if got := srv.URLs()[0].Query().Get("limit"); got != tc.want {
			t.Errorf("Limit %d sent limit=%q, want %q", tc.in, got, tc.want)
		}
	}
}

// Local files and unavailable tracks arrive with no ID and cannot be aggregated; they must
// be skipped rather than fail the whole page.
func TestRecentlyPlayedSkipsItemsWithoutTrackID(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_localfile.json")))
	c := newTestClient(t, srv, nil)

	page, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{})
	if err != nil {
		t.Fatalf("a local-file entry must not fail the page: %v", err)
	}
	if len(page.Plays) != 1 {
		t.Fatalf("plays = %d, want 1", len(page.Plays))
	}
	if page.Plays[0].TrackID != "t1" {
		t.Errorf("kept the wrong item: %q", page.Plays[0].TrackID)
	}
}

func TestRecentlyPlayedEmpty(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_empty.json")))
	c := newTestClient(t, srv, nil)

	page, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Plays) != 0 {
		t.Errorf("plays = %d, want 0", len(page.Plays))
	}
	if page.Saturated {
		t.Error("an empty page must not be Saturated")
	}
	if !page.OldestPlayedAt.IsZero() || !page.NewestPlayedAt.IsZero() {
		t.Error("bounds must stay zero for an empty page")
	}
}

// ---------------------------------------------------------------------------
// Chunk
// ---------------------------------------------------------------------------

func TestChunk(t *testing.T) {
	tests := []struct {
		name string
		in   []int
		n    int
		want [][]int
	}{
		{"empty", nil, 3, nil},
		{"single chunk", []int{1, 2}, 3, [][]int{{1, 2}}},
		{"exact multiple", []int{1, 2, 3, 4}, 2, [][]int{{1, 2}, {3, 4}}},
		{"remainder", []int{1, 2, 3, 4, 5}, 2, [][]int{{1, 2}, {3, 4}, {5}}},
		{"n of 1", []int{1, 2, 3}, 1, [][]int{{1}, {2}, {3}}},
		{"n exceeds len", []int{1}, 10, [][]int{{1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, Chunk(tc.in, tc.n)); diff != "" {
				t.Errorf("Chunk (-want +got):\n%s", diff)
			}
		})
	}
}

func TestChunkPanicsOnNonPositiveN(t *testing.T) {
	for _, n := range []int{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Chunk(_, %d) did not panic", n)
				}
			}()
			Chunk([]int{1}, n)
		}()
	}
}

// ---------------------------------------------------------------------------
// multi-get batching
// ---------------------------------------------------------------------------

func genIDs(prefix string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("%s%d", prefix, i))
	}
	return out
}

// TestMultiGetBatchSizes pins the per-resource asymmetry. Albums cap at 20 while tracks
// and artists cap at 50, so the same ID count produces a different number of requests --
// treating them uniformly would 400 on every album batch.
func TestMultiGetBatchSizes(t *testing.T) {
	tests := []struct {
		name         string
		fixtureFile  string
		ids          []string
		wantRequests int
		call         func(c *Client, ids []string) error
	}{
		{
			name: "101 tracks batch by 50", fixtureFile: "tracks_batch.json",
			ids: genIDs("t", 101), wantRequests: 3,
			call: func(c *Client, ids []string) error {
				_, _, err := c.Tracks(context.Background(), ids)
				return err
			},
		},
		{
			name: "101 artists batch by 50", fixtureFile: "artists.json",
			ids: genIDs("ar", 101), wantRequests: 3,
			call: func(c *Client, ids []string) error {
				_, _, err := c.Artists(context.Background(), ids)
				return err
			},
		},
		{
			name: "101 albums batch by 20", fixtureFile: "albums.json",
			ids: genIDs("al", 101), wantRequests: 6,
			call: func(c *Client, ids []string) error {
				_, _, err := c.Albums(context.Background(), ids)
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRecordingServer(t, serveJSON(fixture(t, tc.fixtureFile)))
			c := newTestClient(t, srv, nil)
			if err := tc.call(c, tc.ids); err != nil {
				t.Fatal(err)
			}
			if got := srv.Calls(); got != tc.wantRequests {
				t.Errorf("requests = %d, want %d", got, tc.wantRequests)
			}
		})
	}
}

// TestMultiGetNeverSendsMarket guards a silent data-corruption bug: with a market set,
// Spotify relinks tracks and returns a DIFFERENT ID than requested, so the same song would
// accumulate statistics under two identities.
func TestMultiGetNeverSendsMarket(t *testing.T) {
	calls := []struct {
		name    string
		fixture string
		fn      func(c *Client) error
	}{
		{"tracks", "tracks_batch.json", func(c *Client) error {
			_, _, err := c.Tracks(context.Background(), []string{"t1", "t2"})
			return err
		}},
		{"artists", "artists.json", func(c *Client) error {
			_, _, err := c.Artists(context.Background(), []string{"ar1", "ar2"})
			return err
		}},
		{"albums", "albums.json", func(c *Client) error {
			_, _, err := c.Albums(context.Background(), []string{"al1", "al2"})
			return err
		}},
		{"top artists", "top_artists.json", func(c *Client) error {
			_, err := c.TopArtists(context.Background(), TimeRangeLong, 20, 0)
			return err
		}},
		{"top tracks", "top_tracks.json", func(c *Client) error {
			_, err := c.TopTracks(context.Background(), TimeRangeShort, 20, 0)
			return err
		}},
		{"recently played", "recently_played_3.json", func(c *Client) error {
			_, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{})
			return err
		}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRecordingServer(t, serveJSON(fixture(t, tc.fixture)))
			c := newTestClient(t, srv, nil)
			if err := tc.fn(c); err != nil {
				t.Fatal(err)
			}
			for _, u := range srv.URLs() {
				if u.Query().Has("market") {
					t.Errorf("%s sent market=%q; relinking would fork the ID space",
						u.Path, u.Query().Get("market"))
				}
			}
		})
	}
}

func TestMultiGetSendsCommaSeparatedIDs(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "tracks_batch.json")))
	c := newTestClient(t, srv, nil)
	if _, _, err := c.Tracks(context.Background(), []string{"t1", "t2", "t3"}); err != nil {
		t.Fatal(err)
	}
	if got := srv.URLs()[0].Query().Get("ids"); got != "t1,t2,t3" {
		t.Errorf("ids = %q, want t1,t2,t3", got)
	}
}

func TestMultiGetEmptyInputMakesNoCall(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(`{}`))
	c := newTestClient(t, srv, nil)

	tr, missing, err := c.Tracks(context.Background(), nil)
	if err != nil || tr != nil || missing != nil {
		t.Errorf("Tracks(nil) = (%v, %v, %v), want all nil", tr, missing, err)
	}
	if _, _, err := c.Tracks(context.Background(), []string{"", ""}); err != nil {
		t.Fatal(err)
	}
	if srv.Calls() != 0 {
		t.Errorf("HTTP calls = %d, want 0 for empty input", srv.Calls())
	}
}

func TestMultiGetDedupesIDs(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "tracks_batch.json")))
	c := newTestClient(t, srv, nil)
	if _, _, err := c.Tracks(context.Background(), []string{"t1", "t1", "t2", "", "t2"}); err != nil {
		t.Fatal(err)
	}
	if got := srv.URLs()[0].Query().Get("ids"); got != "t1,t2" {
		t.Errorf("ids = %q, want t1,t2 with duplicates and blanks removed", got)
	}
}

// TestMultiGetReportsMissingIDs: Spotify returns positionally-aligned nulls for IDs it
// cannot resolve. Callers need them so enrichment can tombstone dead IDs instead of
// re-requesting them on every run forever.
func TestMultiGetReportsMissingIDs(t *testing.T) {
	t.Run("tracks", func(t *testing.T) {
		srv := newRecordingServer(t, serveJSON(fixture(t, "tracks_with_null.json")))
		c := newTestClient(t, srv, nil)

		found, missing, err := c.Tracks(context.Background(), []string{"t1", "gone", "t3"})
		if err != nil {
			t.Fatal(err)
		}
		if len(found) != 2 {
			t.Errorf("found = %d, want 2", len(found))
		}
		if diff := cmp.Diff([]string{"gone"}, missing); diff != "" {
			t.Errorf("missing (-want +got):\n%s", diff)
		}
	})

	t.Run("artists with a leading null", func(t *testing.T) {
		srv := newRecordingServer(t, serveJSON(fixture(t, "artists_with_null.json")))
		c := newTestClient(t, srv, nil)

		found, missing, err := c.Artists(context.Background(), []string{"gone", "ar2"})
		if err != nil {
			t.Fatal(err)
		}
		if len(found) != 1 || found[0].ID != "ar2" {
			t.Errorf("found = %+v, want just ar2", found)
		}
		if diff := cmp.Diff([]string{"gone"}, missing); diff != "" {
			t.Errorf("missing (-want +got):\n%s", diff)
		}
	})
}

func TestArtistsCarryGenres(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "artists.json")))
	c := newTestClient(t, srv, nil)

	found, _, err := c.Artists(context.Background(), []string{"ar1", "ar2"})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]model.Artist{}
	for _, a := range found {
		byID[a.ID] = a
	}
	wt := byID["ar1"]
	if diff := cmp.Diff([]string{"symphonic metal", "gothic metal", "dutch metal"}, wt.Genres); diff != "" {
		t.Errorf("genres (-want +got):\n%s", diff)
	}
	if wt.Followers != 2_500_000 {
		t.Errorf("Followers = %d", wt.Followers)
	}
	if wt.ImageURL != "https://i.scdn.co/image/ar1-big" {
		t.Errorf("ImageURL = %q, want the widest", wt.ImageURL)
	}
	// Most artists genuinely have no genres. That is why genre aggregates under-sum the
	// total rather than being buggy.
	if got := byID["ar2"].Genres; len(got) != 0 {
		t.Errorf("ar2 Genres = %v, want empty", got)
	}
}

func TestAlbumsPreserveReleaseDatePrecision(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "albums.json")))
	c := newTestClient(t, srv, nil)

	found, _, err := c.Albums(context.Background(), []string{"al1", "al2"})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]model.Album{}
	for _, a := range found {
		byID[a.ID] = a
	}
	if got := byID["al1"]; got.ReleaseDate != "2014-10-24" || got.ReleaseDatePrecision != "day" {
		t.Errorf("al1 = (%q, %q)", got.ReleaseDate, got.ReleaseDatePrecision)
	}
	// A year-only release must not be inflated into a false full date.
	if got := byID["al2"]; got.ReleaseDate != "1998" || got.ReleaseDatePrecision != "year" {
		t.Errorf("al2 = (%q, %q), want the year preserved verbatim", got.ReleaseDate, got.ReleaseDatePrecision)
	}
}

// ---------------------------------------------------------------------------
// top items
// ---------------------------------------------------------------------------

func TestTopItemsQuery(t *testing.T) {
	tests := []struct {
		tr                 TimeRange
		limit, offset      int
		wantLimit, wantOff string
	}{
		{TimeRangeShort, 20, 0, "20", "0"},
		{TimeRangeMedium, 0, 0, "50", "0"},
		{TimeRangeLong, 100, 10, "50", "10"},
		{TimeRangeLong, 5, -3, "5", "0"},
	}
	for _, tc := range tests {
		t.Run(string(tc.tr)+"/"+tc.wantLimit, func(t *testing.T) {
			srv := newRecordingServer(t, serveJSON(fixture(t, "top_artists.json")))
			c := newTestClient(t, srv, nil)
			if _, err := c.TopArtists(context.Background(), tc.tr, tc.limit, tc.offset); err != nil {
				t.Fatal(err)
			}
			q := srv.URLs()[0].Query()
			if got := q.Get("time_range"); got != string(tc.tr) {
				t.Errorf("time_range = %q, want %q", got, tc.tr)
			}
			if got := q.Get("limit"); got != tc.wantLimit {
				t.Errorf("limit = %q, want %q", got, tc.wantLimit)
			}
			if got := q.Get("offset"); got != tc.wantOff {
				t.Errorf("offset = %q, want %q", got, tc.wantOff)
			}
		})
	}
}

func TestTopItemsRejectsUnknownTimeRange(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "top_artists.json")))
	c := newTestClient(t, srv, nil)

	if _, err := c.TopArtists(context.Background(), TimeRange("all_time"), 20, 0); err == nil {
		t.Error("TopArtists accepted an unknown time range")
	}
	if _, err := c.TopTracks(context.Background(), TimeRange(""), 20, 0); err == nil {
		t.Error("TopTracks accepted an empty time range")
	}
	if srv.Calls() != 0 {
		t.Error("an invalid time range must be rejected before any HTTP call")
	}
}

func TestTopArtistsAndTracks(t *testing.T) {
	t.Run("artists", func(t *testing.T) {
		srv := newRecordingServer(t, serveJSON(fixture(t, "top_artists.json")))
		c := newTestClient(t, srv, nil)
		got, err := c.TopArtists(context.Background(), TimeRangeLong, 20, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "Within Temptation" {
			t.Errorf("artists = %+v", got)
		}
	})
	t.Run("tracks", func(t *testing.T) {
		srv := newRecordingServer(t, serveJSON(fixture(t, "top_tracks.json")))
		c := newTestClient(t, srv, nil)
		got, err := c.TopTracks(context.Background(), TimeRangeShort, 20, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("tracks = %d, want 2", len(got))
		}
		if got[0].ID != "t1" || got[0].DurationMs == 0 {
			t.Errorf("track[0] = %+v", got[0])
		}
	})
}

func TestAllTimeRangesAreValid(t *testing.T) {
	rs := AllTimeRanges()
	if len(rs) != 3 {
		t.Fatalf("AllTimeRanges = %v, want 3", rs)
	}
	for _, r := range rs {
		if !r.Valid() {
			t.Errorf("%q reported invalid", r)
		}
	}
	if TimeRange("nope").Valid() {
		t.Error("an unknown range reported valid")
	}
}

// ---------------------------------------------------------------------------
// error surfacing
// ---------------------------------------------------------------------------

func TestClientSurfacesAPIError(t *testing.T) {
	srv := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"status":403,"message":"Insufficient client scope"}}`))
	})
	c := newTestClient(t, srv, nil)

	_, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{})
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("err = %T, want *APIError", err)
	}
	if api.StatusCode != 403 {
		t.Errorf("StatusCode = %d", api.StatusCode)
	}
	if api.Message != "Insufficient client scope" {
		t.Errorf("Message = %q", api.Message)
	}
	if !strings.Contains(api.Path, "recently-played") {
		t.Errorf("Path = %q, want it to name the endpoint", api.Path)
	}
}

func TestClientSurfacesMalformedJSON(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(`{"items": [`))
	c := newTestClient(t, srv, nil)
	if _, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{}); err == nil {
		t.Error("expected a decode error")
	}
}

func TestClientTokenFailurePropagates(t *testing.T) {
	srv := newRecordingServer(t, serveJSON(fixture(t, "recently_played_empty.json")))
	want := errors.New("no token for you")
	c := newTestClient(t, srv, failingTokenSource{want})

	if _, err := c.RecentlyPlayed(context.Background(), RecentlyPlayedOptions{}); !errors.Is(err, want) {
		t.Errorf("err = %v, want the token error", err)
	}
	if srv.Calls() != 0 {
		t.Error("no HTTP call should be made without a token")
	}
}

type failingTokenSource struct{ err error }

func (f failingTokenSource) Token(context.Context) (string, error) { return "", f.err }

func TestClientRespectsContextCancellation(t *testing.T) {
	srv := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(http.StatusGatewayTimeout)
	})
	c := newTestClient(t, srv, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.RecentlyPlayed(ctx, RecentlyPlayedOptions{}); err == nil {
		t.Error("expected a context error")
	}
}
