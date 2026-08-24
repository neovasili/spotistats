package api

import (
	"errors"
	"net/http"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// ProfileResponse is the artist profile: external facts, prose and artwork, plus the listening
// figures Spotistats owns.
//
// Every block carries its source, because a partially-resolved profile is normal rather than an
// error and the UI must be able to credit each half honestly.
type ProfileResponse struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`

	// MBID is empty for an artist MusicBrainz has no Spotify link for. ResolvedVia is "link"
	// for the relationship an editor asserted, or "override" for a manual correction. There is
	// no "search": a fuzzy name match would attach the wrong biography to a real band and
	// nothing downstream could detect it.
	MBID        string `json:"mbid,omitempty"`
	ResolvedVia string `json:"resolvedVia,omitempty"`

	ArtistType     string `json:"artistType,omitempty"`
	Country        string `json:"country,omitempty"`
	AreaName       string `json:"areaName,omitempty"`
	BeginAreaName  string `json:"beginAreaName,omitempty"`
	BeganAt        string `json:"beganAt,omitempty"`
	BeganPrecision string `json:"beganPrecision,omitempty"`
	EndedAt        string `json:"endedAt,omitempty"`
	Ended          bool   `json:"ended,omitempty"`

	// MBGenres are MusicBrainz's, and are labelled as such in the UI. They are deliberately
	// NOT merged with Spotify's: the two are different taxonomies, so a merged list would
	// imply an agreement that does not exist.
	MBGenres []string `json:"mbGenres,omitempty"`

	Members []ProfileMember `json:"members,omitempty"`

	Biography     string `json:"biography,omitempty"`
	BiographyLang string `json:"biographyLang,omitempty"`

	Images ProfileImages `json:"images"`

	// Listening is what Spotistats itself knows, which is the one block always present.
	Listening ProfileListening `json:"listening"`

	Sources     ProfileSources `json:"sources"`
	RefreshedAt string         `json:"refreshedAt,omitempty"`
}

type ProfileMember struct {
	Name        string   `json:"name"`
	MBID        string   `json:"mbid,omitempty"`
	Instruments []string `json:"instruments,omitempty"`
	Begin       string   `json:"begin,omitempty"`
	End         string   `json:"end,omitempty"`
	Ended       bool     `json:"ended,omitempty"`
}

type ProfileImages struct {
	// Thumb and Spotify are both artist portraits from different sources; the UI prefers
	// TheAudioDB's, which is usually a better crop, and falls back to Spotify's.
	Thumb     string   `json:"thumb,omitempty"`
	Spotify   string   `json:"spotify,omitempty"`
	Logo      string   `json:"logo,omitempty"`
	Cutout    string   `json:"cutout,omitempty"`
	ClearArt  string   `json:"clearart,omitempty"`
	WideThumb string   `json:"wideThumb,omitempty"`
	Banner    string   `json:"banner,omitempty"`
	Fanart    []string `json:"fanart,omitempty"`
}

type ProfileListening struct {
	Metrics Metrics `json:"metrics"`
	First   *string `json:"firstPlayedAt,omitempty"`
	Last    *string `json:"lastPlayedAt,omitempty"`
	// SpotifyGenres are Spotify's own tags, kept separate from MBGenres.
	SpotifyGenres []string `json:"spotifyGenres,omitempty"`

	// TopAlbums and TopTracks are this artist's own records and songs, ranked by listening time.
	//
	// Precomputed by the nightly rollup, because nothing indexes them: aggregate rows are keyed
	// by the entity itself, so "the albums of this artist" is answerable only by streaming plays.
	// Absent until a full pass has run.
	TopAlbums []ProfileTopItem `json:"topAlbums,omitempty"`
	TopTracks []ProfileTopItem `json:"topTracks,omitempty"`
	// TopItemsAt dates those two lists, which are a nightly snapshot rather than live figures.
	TopItemsAt string `json:"topItemsAt,omitempty"`
}

// ProfileTopItem is one entry in an artist's top albums or top tracks.
type ProfileTopItem struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Context is the album a track came from. Empty for an album entry, whose context is the
	// artist whose page it is on.
	Context  string `json:"context,omitempty"`
	ThumbURL string `json:"thumbUrl,omitempty"`
	Plays    int64  `json:"plays"`
	MsPlayed int64  `json:"msPlayed"`
}

type ProfileSources struct {
	Facts  string `json:"facts,omitempty"`
	Prose  string `json:"prose,omitempty"`
	Images string `json:"images,omitempty"`
}

// handleProfile serves GET /artists/{id}/profile.
//
// An artist with no EXTERNAL row returns 404, not an empty object, so the client can tell
// "never enriched" from "enriched and found nothing" — which are different facts and want
// different words on screen.
func (h *Handler) handleProfile(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if id == "" {
		return badRequest(CodeMissingParameter, "an artist id is required")
	}
	ctx := r.Context()

	// A missing EXTERNAL row no longer ends the request.
	//
	// It used to 404, on the reasoning that "never enriched" and "enriched and found nothing"
	// are different facts wanting different words. That reasoning still holds, and the response
	// still distinguishes them -- an unenriched artist has no `refreshedAt`, a tombstoned one
	// does. What changed is that the page now carries this artist's top albums and tracks, which
	// are YOUR listening and owe nothing to enrichment. 404ing would hide them precisely for the
	// artists where they are the only content there is.
	//
	// So the 404 moves to its honest meaning: we know nothing about this artist at all.
	profile, err := h.store.GetArtistProfile(ctx, id)
	enriched := true
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		enriched = false
		profile = model.ArtistProfile{ArtistID: id}
	}

	out := ProfileResponse{
		ID:             id,
		MBID:           profile.MBID,
		ResolvedVia:    profile.ResolvedVia,
		ArtistType:     profile.ArtistType,
		Country:        profile.Country,
		AreaName:       profile.AreaName,
		BeginAreaName:  profile.BeginAreaName,
		BeganAt:        profile.BeganAt,
		BeganPrecision: profile.BeganPrecision,
		EndedAt:        profile.EndedAt,
		Ended:          profile.Ended,
		MBGenres:       profile.MBGenres,
		Biography:      profile.Biography,
		BiographyLang:  profile.BiographyLang,
		Images: ProfileImages{
			Thumb: profile.Images.Thumb, Logo: profile.Images.Logo,
			Cutout: profile.Images.Cutout, ClearArt: profile.Images.ClearArt,
			WideThumb: profile.Images.WideThumb, Banner: profile.Images.Banner,
			Fanart: profile.Images.Fanart,
		},
		Sources: ProfileSources{
			Facts: profile.Sources.Facts, Prose: profile.Sources.Prose,
			Images: profile.Sources.Images,
		},
	}
	if !profile.RefreshedAt.IsZero() {
		out.RefreshedAt = model.FormatTS(profile.RefreshedAt)
	}
	for _, m := range profile.Members {
		out.Members = append(out.Members, ProfileMember{
			Name: m.Name, MBID: m.MBID, Instruments: m.Instruments,
			Begin: m.Begin, End: m.End, Ended: m.Ended,
		})
	}

	// The name, Spotify portrait and Spotify genres come from the META row, which exists
	// independently. An artist can have a profile and no META row, or the reverse.
	if labels, lerr := h.store.ResolveLabels(ctx, model.DimArtist, []string{id}); lerr == nil {
		out.Name = labels[id].Name
		out.Images.Spotify = labels[id].ImageURL
	}
	if artists, aerr := h.store.GetArtists(ctx, []string{id}); aerr == nil {
		out.Listening.SpotifyGenres = artists[id].Genres
	}

	// The listening figures are the block Spotistats owns, and the reason an unresolved artist
	// still gets a usable page rather than a broken one.
	if agg, gerr := h.store.GetAggregate(ctx, model.AggKey{
		Dim: model.DimArtist, Period: model.PeriodAll, EntityID: id,
	}); gerr == nil {
		out.Listening.Metrics = metricsOf(agg)
		out.Listening.First = tsPtr(agg.FirstPlayedAt)
		out.Listening.Last = tsPtr(agg.LastPlayedAt)
	}

	// One more item read, and the reason it is precomputed: an artist's own albums and tracks
	// are not indexed anywhere, so the alternative is streaming plays inside a request handler.
	// A missing row is not an error -- it means no full rollup has run since this artist was
	// first played -- so the lists are simply absent and the page omits the sections.
	if tops, terr := h.store.GetArtistTopItems(ctx, id); terr == nil {
		out.Listening.TopAlbums = profileTopItems(tops.Albums)
		out.Listening.TopTracks = profileTopItems(tops.Tracks)
		if !tops.ComputedAt.IsZero() {
			out.Listening.TopItemsAt = model.FormatTS(tops.ComputedAt)
		}
	}

	// Nothing at all: no enrichment, no name, no listening. That is a 404 -- the id addresses
	// no artist this dataset has ever seen, which is different from an artist we simply know
	// little about.
	if !enriched && out.Name == "" && out.Listening.Metrics.Plays == 0 &&
		len(out.Listening.TopAlbums) == 0 && len(out.Listening.TopTracks) == 0 {
		return notFound("no artist with that id appears in this listening history")
	}

	writeJSON(w, r, h.log, out)
	return nil
}

func profileTopItems(in []model.TopItem) []ProfileTopItem {
	if len(in) == 0 {
		return nil
	}
	out := make([]ProfileTopItem, 0, len(in))
	for _, i := range in {
		out = append(out, ProfileTopItem{
			ID: i.ID, Name: i.Name, Context: i.Context, ThumbURL: i.ThumbURL,
			Plays: i.Plays, MsPlayed: i.MsPlayed,
		})
	}
	return out
}
