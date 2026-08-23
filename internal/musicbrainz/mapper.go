package musicbrainz

import (
	"strings"

	"github.com/neovasili/spotistats/internal/model"
)

// relMemberOfBand is the relationship type carrying band membership.
const relMemberOfBand = "member of band"

// ToProfile maps a MusicBrainz artist onto the facts half of a model.ArtistProfile.
//
// The prose and imagery half is left empty for TheAudioDB to fill; Sources.Facts records that
// this half came from here, so a profile that resolved on MusicBrainz and then failed on
// TheAudioDB is a normal partially-populated row rather than an error.
func ToProfile(spotifyID string, a dtoArtist) model.ArtistProfile {
	p := model.ArtistProfile{
		ArtistID:    spotifyID,
		MBID:        a.ID,
		ResolvedVia: model.ResolvedViaLink,
		ArtistType:  a.Type,
		Country:     a.Country,
		MBGenres:    genreNames(a.Genres),
		Members:     membersOf(a),
		Sources:     model.ProfileSources{Facts: model.SourceMusicBrainz},
	}
	if a.Area != nil {
		p.AreaName = a.Area.Name
	}
	if a.BeginArea != nil {
		p.BeginAreaName = a.BeginArea.Name
	}
	if a.LifeSpan != nil {
		// Verbatim plus a precision field. "1995-04" is a real MusicBrainz value; parsing it
		// into a time.Time would invent a day of the month the data does not claim.
		p.BeganAt, p.BeganPrecision = a.LifeSpan.Begin, datePrecision(a.LifeSpan.Begin)
		p.EndedAt, p.EndedPrecision = a.LifeSpan.End, datePrecision(a.LifeSpan.End)
		p.Ended = a.LifeSpan.Ended
	}
	return p
}

// datePrecision classifies a variable-precision MusicBrainz date.
//
// Mirrors the ReleaseDate/ReleaseDatePrecision pattern already on model.Album, so both places
// that face this problem answer it the same way.
func datePrecision(s string) string {
	switch strings.Count(s, "-") {
	case 0:
		if s == "" {
			return ""
		}
		return "year"
	case 1:
		return "month"
	default:
		return "day"
	}
}

// genreNames flattens the vote-counted genre list, keeping MusicBrainz's own ordering.
//
// The counts are dropped: they are useful for deciding whether a tag is credible, and this
// list is DISPLAY ONLY (it must never reach AGG#GENRE), so a number nothing renders is a
// number nobody keeps accurate.
func genreNames(gs []dtoGenre) []string {
	if len(gs) == 0 {
		return nil
	}
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		if g.Name != "" {
			out = append(out, g.Name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// membersOf extracts band membership, filtered by DIRECTION as well as type.
//
// This is the rule that is easy to get wrong and silently wrong when you do. A "member of band"
// relationship appears on both ends:
//
//   - On a Group, direction "backward" points at the PEOPLE in the band.
//   - On a Person, direction "forward" points at the BANDS they belonged to.
//
// Filtering on type alone therefore stores bands as members of people, which renders as
// "Sharon den Adel's members: Within Temptation" — confidently wrong, and invisible unless
// someone looks at a solo artist's profile.
func membersOf(a dtoArtist) []model.Member {
	want := memberDirectionFor(a.Type)
	var out []model.Member
	for _, r := range a.Relations {
		if r.Type != relMemberOfBand || r.Direction != want || r.Artist == nil {
			continue
		}
		out = append(out, model.Member{
			Name:        r.Artist.Name,
			MBID:        r.Artist.ID,
			Instruments: append([]string(nil), r.Attributes...),
			Begin:       r.Begin,
			End:         r.End,
			Ended:       r.Ended,
		})
	}
	return out
}

// memberDirectionFor returns the relationship direction that yields members for an entity type.
//
// Groups, orchestras and choirs have members; a Person has bands. Anything unrecognised is
// treated as a group, because a new MusicBrainz type is far more likely to be an ensemble than
// a person, and the alternative is silently dropping its members.
func memberDirectionFor(artistType string) string {
	if strings.EqualFold(artistType, "Person") {
		return "forward"
	}
	return "backward"
}
