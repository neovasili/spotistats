package store

import (
	"fmt"
	"time"

	"github.com/neovasili/spotistats/internal/model"
)

// Item type discriminators. Stored on every row so a scan (reconciliation, export) can
// tell rows apart without reverse-engineering key prefixes.
const (
	itemTypePlay          = "play"
	itemTypeTrack         = "track"
	itemTypeArtist        = "artist"
	itemTypeArtistProfile = "artistProfile"
	itemTypeAlbum         = "album"
	itemTypeAggregate     = "aggregate"
	itemTypeTop           = "top"
	itemTypeHist          = "hist"
	itemTypeState         = "state"
)

// ---------------------------------------------------------------------------
// plays
// ---------------------------------------------------------------------------

// playItem is the stored form of a play event.
//
// playedAt is deliberately NOT stored as its own attribute: it is already the leading
// component of both SK and GSI1SK, and key attributes are always projected into a GSI, so
// duplicating it would add bytes to the highest-volume row type for nothing.
type playItem struct {
	PK     string `dynamodbav:"PK"`
	SK     string `dynamodbav:"SK"`
	GSI1PK string `dynamodbav:"GSI1PK"`
	GSI1SK string `dynamodbav:"GSI1SK"`
	Type   string `dynamodbav:"type"`

	TrackID   string   `dynamodbav:"trackId"`
	ArtistIDs []string `dynamodbav:"artistIds,omitempty"`
	AlbumID   string   `dynamodbav:"albumId,omitempty"`

	MsPlayed    int64 `dynamodbav:"msPlayed"`
	MsEstimated bool  `dynamodbav:"msEstimated"`

	Source string `dynamodbav:"source"`

	// Export-only fields; absent on api-sourced rows.
	Platform    string `dynamodbav:"platform,omitempty"`
	Country     string `dynamodbav:"country,omitempty"`
	ReasonStart string `dynamodbav:"reasonStart,omitempty"`
	ReasonEnd   string `dynamodbav:"reasonEnd,omitempty"`
	Shuffle     bool   `dynamodbav:"shuffle,omitempty"`
	Skipped     bool   `dynamodbav:"skipped,omitempty"`
	Offline     bool   `dynamodbav:"offline,omitempty"`

	// The names the export supplied. Kept because they are the only durable record of what an
	// old play was: the export gives no artist or album ID, so these are what identity falls
	// back to until the track is resolved (model.FactsForTrack), and keeping them is what makes
	// that eventual resolution a reconcile rather than a reimport of 400,000 rows.
	TrackName  string `dynamodbav:"trackName,omitempty"`
	ArtistName string `dynamodbav:"artistName,omitempty"`
	AlbumName  string `dynamodbav:"albumName,omitempty"`
}

func newPlayItem(p model.Play) playItem {
	return playItem{
		PK:     PlayPartition(p.PlayedAt),
		SK:     PlaySK(p.PlayedAt, p.TrackID),
		GSI1PK: TrackGSI1PK(p.TrackID),
		GSI1SK: model.FormatTS(p.PlayedAt),
		Type:   itemTypePlay,

		TrackID:   p.TrackID,
		ArtistIDs: p.ArtistIDs,
		AlbumID:   p.AlbumID,

		MsPlayed:    p.MsPlayed,
		MsEstimated: p.MsEstimated,
		Source:      string(p.Source),

		Platform:    p.Export.Platform,
		Country:     p.Export.Country,
		ReasonStart: p.Export.ReasonStart,
		ReasonEnd:   p.Export.ReasonEnd,
		Shuffle:     p.Export.Shuffle,
		Skipped:     p.Export.Skipped,
		Offline:     p.Export.Offline,
		TrackName:   p.Export.TrackName,
		ArtistName:  p.Export.ArtistName,
		AlbumName:   p.Export.AlbumName,
	}
}

// toModel reconstructs a play. It prefers GSI1SK for the timestamp because a GSI1 query
// projects that but not necessarily the base-table SK layout, and falls back to parsing SK.
func (i playItem) toModel() (model.Play, error) {
	var playedAt time.Time
	var err error

	switch {
	case i.GSI1SK != "":
		playedAt, err = model.ParseTS(i.GSI1SK)
	case i.SK != "":
		playedAt, _, err = ParsePlaySK(i.SK)
	default:
		return model.Play{}, fmt.Errorf("store: play item has neither SK nor GSI1SK")
	}
	if err != nil {
		return model.Play{}, err
	}

	return model.Play{
		PlayedAt:    playedAt,
		TrackID:     i.TrackID,
		ArtistIDs:   i.ArtistIDs,
		AlbumID:     i.AlbumID,
		MsPlayed:    i.MsPlayed,
		Source:      model.Source(i.Source),
		MsEstimated: i.MsEstimated,
		Export: model.ExportFields{
			Platform:    i.Platform,
			Country:     i.Country,
			ReasonStart: i.ReasonStart,
			ReasonEnd:   i.ReasonEnd,
			Shuffle:     i.Shuffle,
			Skipped:     i.Skipped,
			Offline:     i.Offline,
			TrackName:   i.TrackName,
			ArtistName:  i.ArtistName,
			AlbumName:   i.AlbumName,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// aggregates
// ---------------------------------------------------------------------------

type aggregateItem struct {
	PK   string `dynamodbav:"PK"`
	SK   string `dynamodbav:"SK"`
	Type string `dynamodbav:"type"`

	// Dim, Period and EntityID are denormalised copies of what the keys already encode.
	// They exist so a Query over an AGG partition yields usable rows without every reader
	// having to parse keys, and so a reconciliation scan can group without ParseAggKey.
	Dim      string `dynamodbav:"dim"`
	Period   string `dynamodbav:"period"`
	EntityID string `dynamodbav:"entityId"`

	Plays      int64 `dynamodbav:"plays"`
	PlaysExact int64 `dynamodbav:"playsExact"`
	// MsPlayedExact is a SUBSET of MsPlayed, not a parallel total: an estimated play
	// contributes to MsPlayed only. So estimatedRatio = 1 - exact/total.
	MsPlayed      int64 `dynamodbav:"msPlayed"`
	MsPlayedExact int64 `dynamodbav:"msPlayedExact"`

	FirstPlayedAt string `dynamodbav:"firstPlayedAt,omitempty"`
	LastPlayedAt  string `dynamodbav:"lastPlayedAt,omitempty"`
}

func newAggregateItem(a model.Aggregate) aggregateItem {
	it := aggregateItem{
		PK:            a.Key.PK(),
		SK:            a.Key.SK(),
		Type:          itemTypeAggregate,
		Dim:           string(a.Key.Dim),
		Period:        string(a.Key.Period),
		EntityID:      a.Key.EntityID,
		Plays:         a.Plays,
		PlaysExact:    a.PlaysExact,
		MsPlayed:      a.MsPlayed,
		MsPlayedExact: a.MsPlayedExact,
	}
	if !a.FirstPlayedAt.IsZero() {
		it.FirstPlayedAt = model.FormatTS(a.FirstPlayedAt)
	}
	if !a.LastPlayedAt.IsZero() {
		it.LastPlayedAt = model.FormatTS(a.LastPlayedAt)
	}
	return it
}

func (i aggregateItem) toModel() (model.Aggregate, error) {
	key, err := model.ParseAggKey(i.PK, i.SK)
	if err != nil {
		return model.Aggregate{}, err
	}
	a := model.Aggregate{
		Key:           key,
		Plays:         i.Plays,
		PlaysExact:    i.PlaysExact,
		MsPlayed:      i.MsPlayed,
		MsPlayedExact: i.MsPlayedExact,
	}
	if i.FirstPlayedAt != "" {
		if a.FirstPlayedAt, err = model.ParseTS(i.FirstPlayedAt); err != nil {
			return model.Aggregate{}, err
		}
	}
	if i.LastPlayedAt != "" {
		if a.LastPlayedAt, err = model.ParseTS(i.LastPlayedAt); err != nil {
			return model.Aggregate{}, err
		}
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// dimensions
// ---------------------------------------------------------------------------

type trackItem struct {
	PK   string `dynamodbav:"PK"`
	SK   string `dynamodbav:"SK"`
	Type string `dynamodbav:"type"`

	ID         string   `dynamodbav:"id"`
	Name       string   `dynamodbav:"name,omitempty"`
	DurationMs int64    `dynamodbav:"durationMs,omitempty"`
	AlbumID    string   `dynamodbav:"albumId,omitempty"`
	ArtistIDs  []string `dynamodbav:"artistIds,omitempty"`
	Popularity int      `dynamodbav:"popularity,omitempty"`
	Explicit   bool     `dynamodbav:"explicit,omitempty"`
	ISRC       string   `dynamodbav:"isrc,omitempty"`
	URI        string   `dynamodbav:"uri,omitempty"`

	RefreshedAt string `dynamodbav:"refreshedAt,omitempty"`
	// Missing marks a tombstone for an ID Spotify will never resolve, so enrichment stops
	// re-requesting it every run.
	Missing bool `dynamodbav:"missing,omitempty"`
}

func newTrackItem(t model.Track, now time.Time) trackItem {
	return trackItem{
		PK: TrackPK(t.ID), SK: SKMeta, Type: itemTypeTrack,
		ID: t.ID, Name: t.Name, DurationMs: t.DurationMs, AlbumID: t.AlbumID,
		ArtistIDs: t.ArtistIDs, Popularity: t.Popularity, Explicit: t.Explicit,
		ISRC: t.ISRC, URI: t.URI,
		RefreshedAt: model.FormatTS(now), Missing: t.Missing,
	}
}

func (i trackItem) toModel() (model.Track, error) {
	t := model.Track{
		ID: i.ID, Name: i.Name, DurationMs: i.DurationMs, AlbumID: i.AlbumID,
		ArtistIDs: i.ArtistIDs, Popularity: i.Popularity, Explicit: i.Explicit,
		ISRC: i.ISRC, URI: i.URI, Missing: i.Missing,
	}
	if i.RefreshedAt != "" {
		var err error
		if t.RefreshedAt, err = model.ParseTS(i.RefreshedAt); err != nil {
			return model.Track{}, err
		}
	}
	return t, nil
}

type artistItem struct {
	PK   string `dynamodbav:"PK"`
	SK   string `dynamodbav:"SK"`
	Type string `dynamodbav:"type"`

	ID         string   `dynamodbav:"id"`
	Name       string   `dynamodbav:"name,omitempty"`
	Genres     []string `dynamodbav:"genres,omitempty"`
	Popularity int      `dynamodbav:"popularity,omitempty"`
	Followers  int64    `dynamodbav:"followers,omitempty"`
	ImageURL   string   `dynamodbav:"imageUrl,omitempty"`
	ThumbURL   string   `dynamodbav:"thumbUrl,omitempty"`

	RefreshedAt string `dynamodbav:"refreshedAt,omitempty"`
	EnrichedAt  string `dynamodbav:"enrichedAt,omitempty"`
	Missing     bool   `dynamodbav:"missing,omitempty"`
}

func newArtistItem(a model.Artist, now time.Time) artistItem {
	return artistItem{
		PK: ArtistPK(a.ID), SK: SKMeta, Type: itemTypeArtist,
		ID: a.ID, Name: a.Name, Genres: a.Genres, Popularity: a.Popularity,
		Followers: a.Followers, ImageURL: a.ImageURL, ThumbURL: a.ThumbURL,
		// PutArtist is only ever called with a full GET /v1/artists object (or a tombstone),
		// so writing it IS the enrichment. Name-only stubs go through PutArtistName, which
		// deliberately leaves enrichedAt alone.
		RefreshedAt: model.FormatTS(now), EnrichedAt: model.FormatTS(now),
		Missing: a.Missing,
	}
}

func (i artistItem) toModel() (model.Artist, error) {
	a := model.Artist{
		ID: i.ID, Name: i.Name, Genres: i.Genres, Popularity: i.Popularity,
		Followers: i.Followers, ImageURL: i.ImageURL, ThumbURL: i.ThumbURL,
		Missing: i.Missing,
	}
	if i.RefreshedAt != "" {
		var err error
		if a.RefreshedAt, err = model.ParseTS(i.RefreshedAt); err != nil {
			return model.Artist{}, err
		}
	}
	if i.EnrichedAt != "" {
		var err error
		if a.EnrichedAt, err = model.ParseTS(i.EnrichedAt); err != nil {
			return model.Artist{}, err
		}
	}
	return a, nil
}

type albumItem struct {
	PK   string `dynamodbav:"PK"`
	SK   string `dynamodbav:"SK"`
	Type string `dynamodbav:"type"`

	ID                   string   `dynamodbav:"id"`
	Name                 string   `dynamodbav:"name,omitempty"`
	ReleaseDate          string   `dynamodbav:"releaseDate,omitempty"`
	ReleaseDatePrecision string   `dynamodbav:"releaseDatePrecision,omitempty"`
	ImageURL             string   `dynamodbav:"imageUrl,omitempty"`
	ThumbURL             string   `dynamodbav:"thumbUrl,omitempty"`
	TotalTracks          int      `dynamodbav:"totalTracks,omitempty"`
	ArtistIDs            []string `dynamodbav:"artistIds,omitempty"`

	RefreshedAt string `dynamodbav:"refreshedAt,omitempty"`
	Missing     bool   `dynamodbav:"missing,omitempty"`
}

func newAlbumItem(a model.Album, now time.Time) albumItem {
	return albumItem{
		PK: AlbumPK(a.ID), SK: SKMeta, Type: itemTypeAlbum,
		ID: a.ID, Name: a.Name, ReleaseDate: a.ReleaseDate,
		ReleaseDatePrecision: a.ReleaseDatePrecision,
		ImageURL:             a.ImageURL, ThumbURL: a.ThumbURL,
		TotalTracks: a.TotalTracks, ArtistIDs: a.ArtistIDs,
		RefreshedAt: model.FormatTS(now), Missing: a.Missing,
	}
}

func (i albumItem) toModel() (model.Album, error) {
	a := model.Album{
		ID: i.ID, Name: i.Name, ReleaseDate: i.ReleaseDate,
		ReleaseDatePrecision: i.ReleaseDatePrecision,
		ImageURL:             i.ImageURL, ThumbURL: i.ThumbURL,
		TotalTracks: i.TotalTracks, ArtistIDs: i.ArtistIDs, Missing: i.Missing,
	}
	if i.RefreshedAt != "" {
		var err error
		if a.RefreshedAt, err = model.ParseTS(i.RefreshedAt); err != nil {
			return model.Album{}, err
		}
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// state
// ---------------------------------------------------------------------------

type pollCursorItem struct {
	PK   string `dynamodbav:"PK"`
	SK   string `dynamodbav:"SK"`
	Type string `dynamodbav:"type"`

	LastPlayedAt string `dynamodbav:"lastPlayedAt,omitempty"`
	LastRunAt    string `dynamodbav:"lastRunAt,omitempty"`
	LastStatus   string `dynamodbav:"lastStatus,omitempty"`
}

type gapItem struct {
	PK   string `dynamodbav:"PK"`
	SK   string `dynamodbav:"SK"`
	Type string `dynamodbav:"type"`

	DetectedAt    string `dynamodbav:"detectedAt"`
	WindowStart   string `dynamodbav:"windowStart,omitempty"`
	WindowEnd     string `dynamodbav:"windowEnd,omitempty"`
	ItemsReturned int    `dynamodbav:"itemsReturned"`
	Limit         int    `dynamodbav:"limit"`
}

type ingestItem struct {
	PK   string `dynamodbav:"PK"`
	SK   string `dynamodbav:"SK"`
	Type string `dynamodbav:"type"`

	Month      string `dynamodbav:"month"`
	Source     string `dynamodbav:"source"`
	ImportedAt string `dynamodbav:"importedAt"`
	PlayCount  int64  `dynamodbav:"playCount"`
}

type configItem struct {
	PK   string `dynamodbav:"PK"`
	SK   string `dynamodbav:"SK"`
	Type string `dynamodbav:"type"`

	Timezone      string `dynamodbav:"timezone"`
	SchemaVersion int    `dynamodbav:"schemaVersion"`
	WrittenAt     string `dynamodbav:"writtenAt"`
}

type coverageItem struct {
	PK   string `dynamodbav:"PK"`
	SK   string `dynamodbav:"SK"`
	Type string `dynamodbav:"type"`

	FirstPlayedAt   string `dynamodbav:"firstPlayedAt,omitempty"`
	LastPlayedAt    string `dynamodbav:"lastPlayedAt,omitempty"`
	TotalPlays      int64  `dynamodbav:"totalPlays"`
	TotalMs         int64  `dynamodbav:"totalMs"`
	PlaysWithGenre  int64  `dynamodbav:"playsWithGenre"`
	MsWithGenre     int64  `dynamodbav:"msWithGenre"`
	PlaysWithArtist int64  `dynamodbav:"playsWithArtist"`
	MsWithArtist    int64  `dynamodbav:"msWithArtist"`
	ComputedAt      string `dynamodbav:"computedAt"`
}

// artistProfileItem is the ARTIST#{id} / EXTERNAL row.
//
// Every field is tagged explicitly and omitempty throughout: a partially-resolved profile is
// the normal case, not an error, so an absent block must be absent from the item rather than
// stored as a zero value the reader has to distinguish from a real one.
type artistProfileItem struct {
	PK   string `dynamodbav:"PK"`
	SK   string `dynamodbav:"SK"`
	Type string `dynamodbav:"type"`

	ID string `dynamodbav:"id"`

	MBID          string   `dynamodbav:"mbid,omitempty"`
	MBResolvedVia string   `dynamodbav:"mbResolvedVia,omitempty"`
	MBGenres      []string `dynamodbav:"mbGenres,omitempty"`

	ArtistType     string `dynamodbav:"artistType,omitempty"`
	Country        string `dynamodbav:"country,omitempty"`
	AreaName       string `dynamodbav:"areaName,omitempty"`
	BeginAreaName  string `dynamodbav:"beginAreaName,omitempty"`
	BeganAt        string `dynamodbav:"beganAt,omitempty"`
	BeganPrecision string `dynamodbav:"beganPrecision,omitempty"`
	EndedAt        string `dynamodbav:"endedAt,omitempty"`
	EndedPrecision string `dynamodbav:"endedPrecision,omitempty"`
	Ended          bool   `dynamodbav:"ended,omitempty"`

	Members []memberItem `dynamodbav:"members,omitempty"`

	AudioDBID     string `dynamodbav:"audiodbId,omitempty"`
	Biography     string `dynamodbav:"biography,omitempty"`
	BiographyLang string `dynamodbav:"biographyLang,omitempty"`

	Images imagesItem `dynamodbav:"images,omitempty"`

	SourceFacts  string `dynamodbav:"sourceFacts,omitempty"`
	SourceProse  string `dynamodbav:"sourceProse,omitempty"`
	SourceImages string `dynamodbav:"sourceImages,omitempty"`

	// RefreshedAt is set even on an unresolved artist. An empty mbid WITH a refreshedAt is a
	// tombstone: MusicBrainz has never linked this artist, so asking again tonight spends a
	// request on a known answer. Like the other negative caches it expires, because it can be
	// wrong -- an editor may add the link tomorrow.
	RefreshedAt string `dynamodbav:"refreshedAt,omitempty"`
}

type memberItem struct {
	Name        string   `dynamodbav:"name"`
	MBID        string   `dynamodbav:"mbid,omitempty"`
	Instruments []string `dynamodbav:"instruments,omitempty"`
	Begin       string   `dynamodbav:"begin,omitempty"`
	End         string   `dynamodbav:"end,omitempty"`
	Ended       bool     `dynamodbav:"ended,omitempty"`
}

type imagesItem struct {
	Thumb     string   `dynamodbav:"thumb,omitempty"`
	Logo      string   `dynamodbav:"logo,omitempty"`
	Cutout    string   `dynamodbav:"cutout,omitempty"`
	ClearArt  string   `dynamodbav:"clearart,omitempty"`
	WideThumb string   `dynamodbav:"wideThumb,omitempty"`
	Banner    string   `dynamodbav:"banner,omitempty"`
	Fanart    []string `dynamodbav:"fanart,omitempty"`
}

func newArtistProfileItem(p model.ArtistProfile, now time.Time) artistProfileItem {
	refreshed := p.RefreshedAt
	if refreshed.IsZero() {
		refreshed = now
	}
	item := artistProfileItem{
		PK: ArtistPK(p.ArtistID), SK: SKExternal, Type: itemTypeArtistProfile,
		ID: p.ArtistID,

		MBID: p.MBID, MBResolvedVia: p.ResolvedVia, MBGenres: p.MBGenres,

		ArtistType: p.ArtistType, Country: p.Country,
		AreaName: p.AreaName, BeginAreaName: p.BeginAreaName,
		BeganAt: p.BeganAt, BeganPrecision: p.BeganPrecision,
		EndedAt: p.EndedAt, EndedPrecision: p.EndedPrecision, Ended: p.Ended,

		AudioDBID: p.AudioDBID, Biography: p.Biography, BiographyLang: p.BiographyLang,

		Images: imagesItem{
			Thumb: p.Images.Thumb, Logo: p.Images.Logo, Cutout: p.Images.Cutout,
			ClearArt: p.Images.ClearArt, WideThumb: p.Images.WideThumb,
			Banner: p.Images.Banner, Fanart: p.Images.Fanart,
		},

		SourceFacts: p.Sources.Facts, SourceProse: p.Sources.Prose,
		SourceImages: p.Sources.Images,

		RefreshedAt: model.FormatTS(refreshed),
	}
	for _, m := range p.Members {
		item.Members = append(item.Members, memberItem{
			Name: m.Name, MBID: m.MBID, Instruments: m.Instruments,
			Begin: m.Begin, End: m.End, Ended: m.Ended,
		})
	}
	return item
}

func (i artistProfileItem) toModel() (model.ArtistProfile, error) {
	p := model.ArtistProfile{
		ArtistID: i.ID,

		MBID: i.MBID, ResolvedVia: i.MBResolvedVia, MBGenres: i.MBGenres,

		ArtistType: i.ArtistType, Country: i.Country,
		AreaName: i.AreaName, BeginAreaName: i.BeginAreaName,
		BeganAt: i.BeganAt, BeganPrecision: i.BeganPrecision,
		EndedAt: i.EndedAt, EndedPrecision: i.EndedPrecision, Ended: i.Ended,

		AudioDBID: i.AudioDBID, Biography: i.Biography, BiographyLang: i.BiographyLang,

		Images: model.ArtistImages{
			Thumb: i.Images.Thumb, Logo: i.Images.Logo, Cutout: i.Images.Cutout,
			ClearArt: i.Images.ClearArt, WideThumb: i.Images.WideThumb,
			Banner: i.Images.Banner, Fanart: i.Images.Fanart,
		},

		Sources: model.ProfileSources{
			Facts: i.SourceFacts, Prose: i.SourceProse, Images: i.SourceImages,
		},
	}
	for _, m := range i.Members {
		p.Members = append(p.Members, model.Member{
			Name: m.Name, MBID: m.MBID, Instruments: m.Instruments,
			Begin: m.Begin, End: m.End, Ended: m.Ended,
		})
	}
	if i.RefreshedAt != "" {
		var err error
		if p.RefreshedAt, err = model.ParseTS(i.RefreshedAt); err != nil {
			return model.ArtistProfile{}, err
		}
	}
	return p, nil
}
