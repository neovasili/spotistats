package theaudiodb_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/neovasili/spotistats/internal/httpx/httpxtest"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/theaudiodb"
	"github.com/neovasili/spotistats/internal/theaudiodb/theaudiodbtest"
)

// mergeFrom fetches and merges, so the decode and the mapping are exercised together against a
// golden captured from the real service.
func mergeFrom(t *testing.T, body, lang string) model.ArtistProfile {
	t.Helper()
	srv := theaudiodbtest.New(t, theaudiodbtest.ServeJSON(body))
	c, err := theaudiodb.New(theaudiodb.Config{
		APIKey: testKey, BaseURL: srv.URL,
		Clock: httpxtest.NewFakeClock(time.Unix(0, 0).UTC()),
	})
	if err != nil {
		t.Fatal(err)
	}
	a, found, err := c.ArtistByMBID(context.Background(), "m")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	return theaudiodb.Merge(model.ArtistProfile{}, a, lang)
}

// TestEnglishBiographyComesFromTheUnsuffixedField is the trap in this API.
//
// TheAudioDB's English text lives in `strBiography`. `strBiographyEN` EXISTS AND IS EMPTY. A
// selector built the obvious way -- "strBiography" + upper(lang) -- returns nothing for
// English, which is the default and by far the most common case. The golden pins the real
// shape: strBiographyEN is present and blank.
func TestEnglishBiographyComesFromTheUnsuffixedField(t *testing.T) {
	p := mergeFrom(t, fixture(t, "artist.json"), "en")
	if p.Biography == "" {
		t.Fatal("no English biography; strBiographyEN was probably read instead of strBiography")
	}
	if p.BiographyLang != "en" {
		t.Errorf("BiographyLang = %q, want en", p.BiographyLang)
	}
}

func TestDefaultLanguageIsEnglish(t *testing.T) {
	// An unset language must not mean "no biography".
	p := mergeFrom(t, fixture(t, "artist.json"), "")
	if p.Biography == "" || p.BiographyLang != "en" {
		t.Errorf("lang=%q bio=%d chars", p.BiographyLang, len(p.Biography))
	}
}

func TestTranslationIsSelectedWhenAvailable(t *testing.T) {
	p := mergeFrom(t, fixture(t, "artist.json"), "de")
	if p.BiographyLang != "de" {
		t.Fatalf("BiographyLang = %q, want de", p.BiographyLang)
	}
	if p.Biography == "" {
		t.Error("no German biography despite strBiographyDE being populated")
	}
}

// A missing translation falls back to English rather than returning nothing: a biography in the
// wrong language is far more useful than a blank panel, and BiographyLang records what was
// actually stored so the UI can say so.
func TestMissingTranslationFallsBackToEnglish(t *testing.T) {
	// strBiographyES is present but empty in the golden.
	p := mergeFrom(t, fixture(t, "artist.json"), "es")
	if p.BiographyLang != "en" {
		t.Errorf("BiographyLang = %q, want the en fallback", p.BiographyLang)
	}
	if p.Biography == "" {
		t.Error("fell back to nothing instead of English")
	}
}

func TestNoBiographyAtAllLeavesProseUnclaimed(t *testing.T) {
	p := mergeFrom(t, `{"artists":[{"idArtist":"1","strArtist":"A"}]}`, "en")
	if p.Biography != "" || p.BiographyLang != "" {
		t.Errorf("invented a biography: %q/%q", p.Biography, p.BiographyLang)
	}
	// Provenance must not claim a source that supplied nothing.
	if p.Sources.Prose != "" {
		t.Errorf("Sources.Prose = %q for an artist with no biography", p.Sources.Prose)
	}
}

// Absent art must be ABSENT, not an empty string, so no consumer has to special-case "" before
// deciding whether to render an <img>.
func TestImagesDropEmptyURLs(t *testing.T) {
	body := `{"artists":[{"idArtist":"1","strArtist":"A",
		"strArtistThumb":"https://r2.theaudiodb.com/thumb.jpg",
		"strArtistLogo":"","strArtistCutout":null,
		"strArtistFanart":"https://r2.theaudiodb.com/f1.jpg",
		"strArtistFanart2":"","strArtistFanart3":"https://r2.theaudiodb.com/f3.jpg"}]}`
	p := mergeFrom(t, body, "en")

	want := model.ArtistImages{
		Thumb: "https://r2.theaudiodb.com/thumb.jpg",
		Fanart: []string{
			"https://r2.theaudiodb.com/f1.jpg",
			"https://r2.theaudiodb.com/f3.jpg",
		},
	}
	if diff := cmp.Diff(want, p.Images); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	// Fanart slots are not positional: an empty slot 2 must not leave a gap that shifts slot 3.
	if len(p.Images.Fanart) != 2 {
		t.Errorf("fanart = %v, want the two populated slots compacted", p.Images.Fanart)
	}
}

func TestNoImagesLeavesImagesUnclaimed(t *testing.T) {
	p := mergeFrom(t, `{"artists":[{"idArtist":"1","strArtist":"A"}]}`, "en")
	if p.Images.Any() {
		t.Errorf("Any() = true for an artist with no artwork: %+v", p.Images)
	}
	if p.Sources.Images != "" {
		t.Errorf("Sources.Images = %q with no images", p.Sources.Images)
	}
}

func TestAllImageKindsAreMapped(t *testing.T) {
	p := mergeFrom(t, fixture(t, "artist.json"), "en")
	// The golden is a real artist with every slot populated, so a field the mapper forgot
	// shows up as an empty string here.
	for name, got := range map[string]string{
		"thumb": p.Images.Thumb, "logo": p.Images.Logo, "cutout": p.Images.Cutout,
		"clearart": p.Images.ClearArt, "wideThumb": p.Images.WideThumb,
		"banner": p.Images.Banner,
	} {
		if got == "" {
			t.Errorf("%s was not mapped", name)
		}
	}
	if len(p.Images.Fanart) != 4 {
		t.Errorf("fanart = %d slots, want 4", len(p.Images.Fanart))
	}
}
