package theaudiodb_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/neovasili/spotistats/internal/httpx/httpxtest"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/theaudiodb"
	"github.com/neovasili/spotistats/internal/theaudiodb/theaudiodbtest"
)

const testKey = "test-key-abc123"

func fixture(t *testing.T, name string) string {
	t.Helper()
	return theaudiodbtest.Fixture(t, "testdata", name)
}

func newClient(t *testing.T, srv *theaudiodbtest.Server) *theaudiodb.Client {
	t.Helper()
	c, err := theaudiodb.New(theaudiodb.Config{
		APIKey:  testKey,
		BaseURL: srv.URL,
		// Must ADVANCE on sleep, or the 30/min limiter waits forever.
		Clock: httpxtest.NewFakeClock(time.Unix(0, 0).UTC()),
	})
	if err != nil {
		t.Fatalf("theaudiodb.New: %v", err)
	}
	return c
}

// A missing key does not fail loudly at TheAudioDB -- the path just 404s or returns nothing --
// so it is caught at construction where the cause is obvious.
func TestNewRequiresAnAPIKey(t *testing.T) {
	for _, k := range []string{"", "   "} {
		if _, err := theaudiodb.New(theaudiodb.Config{APIKey: k}); !errors.Is(err, theaudiodb.ErrAPIKeyRequired) {
			t.Errorf("New(%q) = %v, want ErrAPIKeyRequired", k, err)
		}
	}
}

// The key is a PATH segment, not a query parameter. Getting that wrong returns an unhelpful
// 404 rather than an auth error.
func TestAPIKeyGoesInThePath(t *testing.T) {
	srv := theaudiodbtest.New(t, theaudiodbtest.ServeJSON(fixture(t, "artist.json")))
	c := newClient(t, srv)
	if _, _, err := c.ArtistByMBID(context.Background(), "mbid-1"); err != nil {
		t.Fatal(err)
	}
	u := srv.URLs()[0]
	if !strings.Contains(u.Path, testKey) {
		t.Errorf("path %q does not carry the key", u.Path)
	}
	if u.Query().Get("key") != "" || strings.Contains(u.RawQuery, testKey) {
		t.Errorf("the key leaked into the query string: %q", u.RawQuery)
	}
	if u.Query().Get("i") != "mbid-1" {
		t.Errorf("i = %q, want the mbid", u.Query().Get("i"))
	}
}

// TestErrorsRedactTheAPIKey: the key is in every request URL, so it is in every transport
// error. Logging one would leak a credential into CloudWatch, where it outlives the request.
func TestErrorsRedactTheAPIKey(t *testing.T) {
	srv := theaudiodbtest.New(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	c := newClient(t, srv)
	_, _, err := c.ArtistByMBID(context.Background(), "mbid-1")
	if err == nil {
		t.Fatal("want an error for a 400")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("the API key leaked into an error message: %v", err)
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Errorf("error does not show the redaction: %v", err)
	}
}

// An unknown MBID returns {"artists": null} with HTTP 200, so absence has to be read from the
// body rather than the status.
func TestUnknownMBIDIsNotFoundNotAnError(t *testing.T) {
	srv := theaudiodbtest.New(t, theaudiodbtest.ServeJSON(fixture(t, "artist_missing.json")))
	c := newClient(t, srv)
	_, found, err := c.ArtistByMBID(context.Background(), "nope")
	if err != nil {
		t.Fatalf("a null artists array must not be an error: %v", err)
	}
	if found {
		t.Error("found = true for a null result")
	}
}

func TestEmptyMBIDMakesNoRequest(t *testing.T) {
	srv := theaudiodbtest.New(t, theaudiodbtest.ServeJSON(`{}`))
	c := newClient(t, srv)
	if _, found, err := c.ArtistByMBID(context.Background(), ""); err != nil || found {
		t.Errorf("got found=%v err=%v", found, err)
	}
	if srv.Calls() != 0 {
		t.Errorf("made %d requests for an empty mbid", srv.Calls())
	}
}

// TestNoSearchMethodExists guards the no-fuzzy-match rule structurally.
//
// TheAudioDB offers search.php?s={name} and it is tempting for the artists MusicBrainz has not
// linked. Its absence here is deliberate: a fuzzy match attaches the wrong biography to a real
// band and nothing downstream can detect it. This asserts the client exposes no way to reach it.
func TestNoSearchMethodExists(t *testing.T) {
	// A compile-time check would be better, but the point is documented and greppable: the
	// only exported lookup is by MBID.
	srv := theaudiodbtest.New(t, theaudiodbtest.ServeJSON(fixture(t, "artist.json")))
	c := newClient(t, srv)
	if _, _, err := c.ArtistByMBID(context.Background(), "m"); err != nil {
		t.Fatal(err)
	}
	for _, u := range srv.URLs() {
		if strings.Contains(u.Path, "search") {
			t.Errorf("a request reached %q; name search must never be used", u.Path)
		}
	}
}

// TestRateLimitIsRetried: the free key answers 429 above 30 requests a minute.
func TestRateLimitIsRetried(t *testing.T) {
	var n int
	srv := theaudiodbtest.New(t, func(w http.ResponseWriter, _ *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artists":[{"idArtist":"1","strArtist":"A"}]}`))
	})
	c := newClient(t, srv)
	_, found, err := c.ArtistByMBID(context.Background(), "m")
	if err != nil {
		t.Fatalf("429 was surfaced instead of retried: %v", err)
	}
	if !found || n != 2 {
		t.Errorf("found=%v attempts=%d", found, n)
	}
}

// The structured fields are decoded but must never win over MusicBrainz's.
func TestMergeNeverOverwritesMusicBrainzFacts(t *testing.T) {
	srv := theaudiodbtest.New(t, theaudiodbtest.ServeJSON(fixture(t, "artist.json")))
	c := newClient(t, srv)
	a, found, err := c.ArtistByMBID(context.Background(), "m")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}

	// A profile as MusicBrainz left it. TheAudioDB says formed 1996 and country
	// "Waddinxveen, Netherlands"; MusicBrainz says 1995-04 and NL. MusicBrainz must win.
	before := model.ArtistProfile{
		MBID: "mb-1", ArtistType: "Group", Country: "NL",
		BeganAt: "1995-04", BeganPrecision: "month",
		AreaName: "Netherlands", BeginAreaName: "Waddinxveen",
		MBGenres: []string{"symphonic metal"},
		Members:  []model.Member{{Name: "Sharon den Adel"}},
		Sources:  model.ProfileSources{Facts: model.SourceMusicBrainz},
	}
	got := theaudiodb.Merge(before, a, "en")

	if got.Country != "NL" || got.BeganAt != "1995-04" {
		t.Errorf("TheAudioDB overwrote MusicBrainz facts: country=%q beganAt=%q",
			got.Country, got.BeganAt)
	}
	if len(got.Members) != 1 || len(got.MBGenres) != 1 {
		t.Errorf("structured blocks were disturbed: %+v", got)
	}
	if got.Sources.Facts != model.SourceMusicBrainz {
		t.Errorf("Sources.Facts = %q", got.Sources.Facts)
	}
	// And the halves it DOES own are filled with its own provenance.
	if got.Sources.Prose != model.SourceTheAudioDB || got.Biography == "" {
		t.Errorf("prose not merged: %+v", got.Sources)
	}
	if got.Sources.Images != model.SourceTheAudioDB || !got.Images.Any() {
		t.Errorf("images not merged: %+v", got.Images)
	}
	if got.AudioDBID == "" {
		t.Error("AudioDBID not recorded")
	}
}
