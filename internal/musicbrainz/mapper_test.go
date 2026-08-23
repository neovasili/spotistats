package musicbrainz_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/neovasili/spotistats/internal/httpx/httpxtest"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/musicbrainz"
	"github.com/neovasili/spotistats/internal/musicbrainz/musicbrainztest"
)

// profileFrom fetches and maps, so the tests exercise the decode and the mapping together
// against goldens captured from the real service.
func profileFrom(t *testing.T, body, spotifyID string) model.ArtistProfile {
	t.Helper()
	srv := musicbrainztest.New(t, musicbrainztest.ServeJSON(body))
	c, err := musicbrainz.New(musicbrainz.Config{
		UserAgent: testUA, BaseURL: srv.URL,
		Clock: httpxtest.NewFakeClock(time.Unix(0, 0).UTC()),
	})
	if err != nil {
		t.Fatal(err)
	}
	a, found, err := c.Artist(context.Background(), "mbid")
	if err != nil || !found {
		t.Fatalf("Artist: found=%v err=%v", found, err)
	}
	return musicbrainz.ToProfile(spotifyID, a)
}

func TestToProfileMapsTheStructuredFacts(t *testing.T) {
	p := profileFrom(t, fixture(t, "artist_group.json"), "3hE8S8ohRErocpkY7uJW4a")

	if p.ArtistID != "3hE8S8ohRErocpkY7uJW4a" {
		t.Errorf("ArtistID = %q", p.ArtistID)
	}
	if p.MBID != "eace2373-31c8-4aba-9a5c-7bce22dd140a" {
		t.Errorf("MBID = %q", p.MBID)
	}
	if p.ArtistType != "Group" || p.Country != "NL" {
		t.Errorf("type/country = %q/%q, want Group/NL", p.ArtistType, p.Country)
	}
	// Country and city of origin are different facts: a band can be Dutch and have formed in
	// Waddinxveen, and the profile shows both.
	if p.AreaName != "Netherlands" || p.BeginAreaName != "Waddinxveen" {
		t.Errorf("area/begin-area = %q/%q", p.AreaName, p.BeginAreaName)
	}
	if p.ResolvedVia != model.ResolvedViaLink {
		t.Errorf("ResolvedVia = %q, want %q", p.ResolvedVia, model.ResolvedViaLink)
	}
	if p.Sources.Facts != model.SourceMusicBrainz {
		t.Errorf("Sources.Facts = %q", p.Sources.Facts)
	}
	// Prose and imagery are TheAudioDB's half and must be left empty for it to fill.
	if p.Sources.Prose != "" || p.Sources.Images != "" || p.Biography != "" {
		t.Errorf("MusicBrainz mapping populated TheAudioDB's half: %+v", p.Sources)
	}
}

// TestVariablePrecisionDatesAreVerbatim: this artist's real life-span begin is "1995-04".
// Parsing it into a time.Time would invent a day of the month the data does not claim, which is
// the same reason model.Album keeps ReleaseDate as a string.
func TestVariablePrecisionDatesAreVerbatim(t *testing.T) {
	p := profileFrom(t, fixture(t, "artist_group.json"), "x")
	if p.BeganAt != "1995-04" {
		t.Errorf("BeganAt = %q, want the verbatim %q", p.BeganAt, "1995-04")
	}
	if p.BeganPrecision != "month" {
		t.Errorf("BeganPrecision = %q, want month", p.BeganPrecision)
	}
	if p.Ended {
		t.Error("Ended = true for an active band")
	}
}

func TestDatePrecisionAcrossAllThreeForms(t *testing.T) {
	cases := map[string]struct{ date, precision string }{
		"year":  {"2008", "year"},
		"month": {"2008-04", "month"},
		"day":   {"2008-04-17", "day"},
		"empty": {"", ""},
	}
	for name, tc := range cases {
		body := `{"id":"m","name":"A","type":"Group","life-span":{"begin":"` + tc.date + `"}}`
		p := profileFrom(t, body, "x")
		if p.BeganAt != tc.date || p.BeganPrecision != tc.precision {
			t.Errorf("%s: got %q/%q, want %q/%q",
				name, p.BeganAt, p.BeganPrecision, tc.date, tc.precision)
		}
	}
}

// TestMembersAreFilteredByDirection is the rule that is silently wrong when you get it wrong.
//
// "member of band" appears on BOTH ends of the relationship. On a Group, direction "backward"
// points at the people; on a Person, "forward" points at the bands they were in. Filtering on
// type alone stores bands as members of people — rendering "Sharon den Adel's members: Within
// Temptation", confidently wrong and invisible unless someone opens a solo artist's profile.
func TestMembersAreFilteredByDirection(t *testing.T) {
	t.Run("a group yields its people", func(t *testing.T) {
		p := profileFrom(t, fixture(t, "artist_group.json"), "x")
		if len(p.Members) == 0 {
			t.Fatal("no members mapped for a Group")
		}
		names := make([]string, 0, len(p.Members))
		for _, m := range p.Members {
			names = append(names, m.Name)
		}
		if !contains(names, "Sharon den Adel") {
			t.Errorf("members = %v, want the band's vocalist among them", names)
		}
	})

	t.Run("a person yields nothing from backward relations", func(t *testing.T) {
		// The same relationship as above, seen from the other end: a Person whose "member of
		// band" relation points FORWARD at a group. Filtering on type alone would store
		// "Within Temptation" as a member of the person.
		body := `{"id":"m","name":"Sharon den Adel","type":"Person","relations":[
			{"type":"member of band","direction":"forward",
			 "artist":{"id":"g","name":"Within Temptation"}}]}`
		p := profileFrom(t, body, "x")
		if len(p.Members) != 1 || p.Members[0].Name != "Within Temptation" {
			t.Errorf("members = %+v; a Person's forward relations are the bands they were in",
				p.Members)
		}
	})

	t.Run("a group ignores forward relations", func(t *testing.T) {
		body := `{"id":"m","name":"A Band","type":"Group","relations":[
			{"type":"member of band","direction":"forward","artist":{"id":"o","name":"Wrong"}},
			{"type":"member of band","direction":"backward","artist":{"id":"p","name":"Right"}}]}`
		p := profileFrom(t, body, "x")
		if len(p.Members) != 1 || p.Members[0].Name != "Right" {
			t.Errorf("members = %+v, want only the backward relation", p.Members)
		}
	})

	t.Run("an unknown entity type is treated as an ensemble", func(t *testing.T) {
		// A new MusicBrainz type is far likelier to be an ensemble than a person, and the
		// alternative is silently dropping its members.
		body := `{"id":"m","name":"An Orchestra","type":"Orchestra","relations":[
			{"type":"member of band","direction":"backward","artist":{"id":"p","name":"Player"}}]}`
		p := profileFrom(t, body, "x")
		if len(p.Members) != 1 {
			t.Errorf("members = %+v for an Orchestra", p.Members)
		}
	})

	t.Run("other relation types are ignored", func(t *testing.T) {
		body := `{"id":"m","name":"A","type":"Group","relations":[
			{"type":"free streaming","direction":"backward","artist":{"id":"z","name":"Nope"}},
			{"type":"collaboration","direction":"backward","artist":{"id":"y","name":"Also no"}}]}`
		p := profileFrom(t, body, "x")
		if len(p.Members) != 0 {
			t.Errorf("members = %+v, want none", p.Members)
		}
	})
}

func TestMemberInstrumentsAndTenure(t *testing.T) {
	body := `{"id":"m","name":"A","type":"Group","relations":[
		{"type":"member of band","direction":"backward","attributes":["lead vocals","guitar"],
		 "begin":"1996","end":"2001","ended":true,
		 "artist":{"id":"p","name":"Someone"}}]}`
	p := profileFrom(t, body, "x")
	want := model.Member{
		Name: "Someone", MBID: "p",
		Instruments: []string{"lead vocals", "guitar"},
		Begin:       "1996", End: "2001", Ended: true,
	}
	if diff := cmp.Diff(want, p.Members[0]); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

// Genres are vote-counted and kept in MusicBrainz's own order, DISPLAY ONLY: they must never
// reach AGG#GENRE, because merging two taxonomies double-counts one artist under both.
func TestGenresAreMappedForDisplayOnly(t *testing.T) {
	p := profileFrom(t, fixture(t, "artist_group.json"), "x")
	if len(p.MBGenres) == 0 {
		t.Fatal("no genres mapped")
	}
	if !contains(p.MBGenres, "symphonic metal") {
		t.Errorf("genres = %v", p.MBGenres)
	}
}

func TestAbsentOptionalBlocksStayEmpty(t *testing.T) {
	// A sparse artist: no area, no life-span, no genres, no relations. Every one of those is
	// a pointer or slice in the wire struct, so the mapper must not dereference blindly.
	p := profileFrom(t, `{"id":"m","name":"Minimal","type":"Group"}`, "x")
	if p.AreaName != "" || p.BeginAreaName != "" || p.BeganAt != "" ||
		len(p.MBGenres) != 0 || len(p.Members) != 0 {
		t.Errorf("invented data for a sparse artist: %+v", p)
	}
	if p.MBID != "m" {
		t.Errorf("MBID = %q", p.MBID)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
