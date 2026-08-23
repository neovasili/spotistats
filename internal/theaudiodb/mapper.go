package theaudiodb

import (
	"strings"

	"github.com/neovasili/spotistats/internal/model"
)

// DefaultLanguage is the biography language kept when none is configured.
const DefaultLanguage = "en"

// Merge folds TheAudioDB's prose and imagery into a profile whose structured facts already came
// from MusicBrainz.
//
// It MERGES rather than returning a fresh profile because the two sources fill different halves
// and either can fail independently: a profile that resolved on MusicBrainz and then 429'd here
// is a normal, partially-populated row, and it must keep the facts it already has.
//
// Nothing structured is overwritten. TheAudioDB's intFormedYear, strCountry and intMembers are
// read only to be ignored — see the dto for the measured reasons — so this touches only the
// biography, the images and their provenance.
func Merge(p model.ArtistProfile, a Artist, lang string) model.ArtistProfile {
	p.AudioDBID = a.ID

	if bio, got := biographyFor(a, lang); bio != "" {
		p.Biography = bio
		p.BiographyLang = got
		p.Sources.Prose = model.SourceTheAudioDB
	}

	imgs := imagesOf(a)
	if imgs.Any() {
		p.Images = imgs
		p.Sources.Images = model.SourceTheAudioDB
	}
	return p
}

// biographyFor selects one language, returning the text and the language actually used.
//
// # The trap
//
// TheAudioDB's ENGLISH biography lives in the unsuffixed `strBiography`. `strBiographyEN`
// exists and is EMPTY. A selector built the obvious way — "strBiography" + upper(lang) —
// therefore returns nothing for English, which is the default and by far the most common case.
//
// Non-English requests fall back to English rather than returning nothing: a biography in the
// wrong language is far more useful than a blank panel, and BiographyLang records which one was
// stored so the UI can say so.
func biographyFor(a Artist, lang string) (text, used string) {
	l := strings.ToLower(strings.TrimSpace(lang))
	if l == "" {
		l = DefaultLanguage
	}
	// English is the unsuffixed field, not the EN one.
	if l == "en" {
		return a.Biography, "en"
	}
	if translated := translationFor(a, l); translated != "" {
		return translated, l
	}
	if a.Biography != "" {
		return a.Biography, "en"
	}
	return "", ""
}

// translationFor returns the biography in a specific language, or "".
//
// A map rather than reflection over field tags: fifteen explicit cases are greppable, and the
// set changes only when TheAudioDB adds a language.
func translationFor(a Artist, lang string) string {
	switch lang {
	case "de":
		return a.BiographyDE
	case "es":
		return a.BiographyES
	case "fr":
		return a.BiographyFR
	case "it":
		return a.BiographyIT
	case "pt":
		return a.BiographyPT
	case "nl":
		return a.BiographyNL
	case "sv", "se":
		return a.BiographySE
	case "no":
		return a.BiographyNO
	case "ru":
		return a.BiographyRU
	case "pl":
		return a.BiographyPL
	case "ja", "jp":
		return a.BiographyJP
	case "zh", "cn":
		return a.BiographyCN
	case "hu":
		return a.BiographyHU
	}
	return ""
}

// imagesOf collects the artwork URLs, dropping empties.
//
// Absent art is absent rather than an empty string, so no consumer has to special-case ""
// before deciding whether to render an <img>.
func imagesOf(a Artist) model.ArtistImages {
	imgs := model.ArtistImages{
		Thumb:     strings.TrimSpace(a.Thumb),
		Logo:      strings.TrimSpace(a.Logo),
		Cutout:    strings.TrimSpace(a.Cutout),
		ClearArt:  strings.TrimSpace(a.ClearArt),
		WideThumb: strings.TrimSpace(a.WideThumb),
		Banner:    strings.TrimSpace(a.Banner),
	}
	for _, f := range []string{a.Fanart, a.Fanart2, a.Fanart3, a.Fanart4} {
		if s := strings.TrimSpace(f); s != "" {
			imgs.Fanart = append(imgs.Fanart, s)
		}
	}
	return imgs
}
