package spotify

import "github.com/neovasili/spotistats/internal/model"

// Mapping from wire DTOs to domain types. Hand-written on purpose: the shapes are not
// identical (RefreshedAt, Missing, MsEstimated and Source exist on no wire format), and
// the domain types must stay constructible from the GDPR export too.

// widestImageURL returns the first image URL. Spotify orders images widest-first, so the
// first entry is the highest resolution available.
func widestImageURL(imgs []dtoImage) string {
	if len(imgs) == 0 {
		return ""
	}
	return imgs[0].URL
}

// thumbMinWidth is the smallest asset worth showing in a thumbnail slot. The largest thumbnail
// the UI draws is the ~160px Explorer drill-down inset, doubled for a 2x display.
const thumbMinWidth = 160

// thumbImageURL picks the NARROWEST image at least thumbMinWidth wide, falling back to the
// widest when the array offers nothing smaller.
//
// The choice has to happen here, at capture time, because it cannot be made later: Spotify
// documents no resizing parameter, and hand-editing an i.scdn.co path produces an unsupported
// URL that can 404 without warning (docs/SPECS.md 2.7). Once only the widest URL is stored, a
// hundred-row table has no option but to pull a hundred 640px covers to paint them at 28px.
//
// Sizes are deliberately read from `width` rather than assumed by position: the API documents
// only that the array is ordered widest-first, not what the sizes are.
func thumbImageURL(imgs []dtoImage) string {
	best := ""
	bestWidth := 0
	for _, img := range imgs {
		if img.URL == "" || img.Width < thumbMinWidth {
			continue
		}
		if best == "" || img.Width < bestWidth {
			best, bestWidth = img.URL, img.Width
		}
	}
	if best != "" {
		return best
	}
	// Nothing wide enough, or no widths reported at all: the widest is the only honest answer.
	return widestImageURL(imgs)
}

func artistIDs(as []dtoSimpleArtist) []string {
	if len(as) == 0 {
		return nil
	}
	out := make([]string, 0, len(as))
	for _, a := range as {
		if a.ID != "" {
			out = append(out, a.ID)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (d *dtoTrack) toModel() model.Track {
	return model.Track{
		ID:         d.ID,
		Name:       d.Name,
		DurationMs: d.DurationMs,
		AlbumID:    d.Album.ID,
		ArtistIDs:  artistIDs(d.Artists),
		Popularity: d.Popularity,
		Explicit:   d.Explicit,
		ISRC:       d.ExternalIDs.ISRC,
		URI:        d.URI,
	}
}

func (d *dtoArtist) toModel() model.Artist {
	// Genres are normalised at aggregation time rather than here, so the stored artist
	// row keeps Spotify's own strings verbatim.
	var genres []string
	if len(d.Genres) > 0 {
		genres = append(genres, d.Genres...)
	}
	return model.Artist{
		ID:         d.ID,
		Name:       d.Name,
		Genres:     genres,
		Popularity: d.Popularity,
		Followers:  d.Followers.Total,
		ImageURL:   widestImageURL(d.Images),
		ThumbURL:   thumbImageURL(d.Images),
	}
}

func (d *dtoSimpleAlbum) toModel() model.Album {
	return model.Album{
		ID:                   d.ID,
		Name:                 d.Name,
		ReleaseDate:          d.ReleaseDate,
		ReleaseDatePrecision: d.ReleaseDatePrecision,
		ImageURL:             widestImageURL(d.Images),
		ThumbURL:             thumbImageURL(d.Images),
		TotalTracks:          d.TotalTracks,
		ArtistIDs:            artistIDs(d.Artists),
	}
}

func (d *dtoAlbum) toModel() model.Album { return d.dtoSimpleAlbum.toModel() }
