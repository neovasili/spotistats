package spotify

// Wire types for the Spotify Web API. These are UNEXPORTED and never escape the package:
// callers receive internal/model types instead.
//
// The boundary is not ceremony. internal/model.Play must also be constructible from a
// GDPR export record, which has a completely different shape; if the domain type were a
// Spotify DTO, the history importer would have to fabricate fake API responses.
//
// Every field Spotistats does not use is deliberately omitted -- unmarshalling ignores
// unknown JSON keys, so the DTOs stay small.

type dtoImage struct {
	URL    string `json:"url"`
	Height int    `json:"height"`
	Width  int    `json:"width"`
}

type dtoFollowers struct {
	Total int64 `json:"total"`
}

type dtoExternalIDs struct {
	ISRC string `json:"isrc"`
}

// dtoSimpleArtist is the artist shape embedded in track and album objects. It carries no
// genres -- those exist only on the full artist object from GET /v1/artists, which is why
// genre rollups need a separate enrichment pass.
type dtoSimpleArtist struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URI  string `json:"uri"`
}

// dtoArtist is the full artist object from GET /v1/artists.
type dtoArtist struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Genres     []string     `json:"genres"`
	Popularity int          `json:"popularity"`
	Followers  dtoFollowers `json:"followers"`
	Images     []dtoImage   `json:"images"`
	URI        string       `json:"uri"`
}

// dtoSimpleAlbum is the album shape embedded in a track object.
type dtoSimpleAlbum struct {
	ID                   string            `json:"id"`
	Name                 string            `json:"name"`
	ReleaseDate          string            `json:"release_date"`
	ReleaseDatePrecision string            `json:"release_date_precision"`
	TotalTracks          int               `json:"total_tracks"`
	Images               []dtoImage        `json:"images"`
	Artists              []dtoSimpleArtist `json:"artists"`
	URI                  string            `json:"uri"`
}

// dtoAlbum is the full album object from GET /v1/albums.
type dtoAlbum struct {
	dtoSimpleAlbum
	Popularity int `json:"popularity"`
}

// dtoTrack is the full track object, as embedded in recently-played items and returned by
// GET /v1/tracks.
type dtoTrack struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	DurationMs  int64             `json:"duration_ms"`
	Explicit    bool              `json:"explicit"`
	Popularity  int               `json:"popularity"`
	URI         string            `json:"uri"`
	Album       dtoSimpleAlbum    `json:"album"`
	Artists     []dtoSimpleArtist `json:"artists"`
	ExternalIDs dtoExternalIDs    `json:"external_ids"`
}

type dtoCursors struct {
	After  string `json:"after"`
	Before string `json:"before"`
}

type dtoRecentlyPlayedItem struct {
	Track    dtoTrack `json:"track"`
	PlayedAt string   `json:"played_at"`
}

type dtoRecentlyPlayed struct {
	Items   []dtoRecentlyPlayedItem `json:"items"`
	Next    string                  `json:"next"`
	Cursors dtoCursors              `json:"cursors"`
	Limit   int                     `json:"limit"`
	Href    string                  `json:"href"`
}

// Multi-get envelopes. Entries are positionally aligned with the requested IDs and are
// NULL for anything Spotify cannot resolve, hence the pointer element types.
type dtoTracksResponse struct {
	Tracks []*dtoTrack `json:"tracks"`
}

type dtoArtistsResponse struct {
	Artists []*dtoArtist `json:"artists"`
}

type dtoAlbumsResponse struct {
	Albums []*dtoAlbum `json:"albums"`
}

type dtoTopArtists struct {
	Items  []dtoArtist `json:"items"`
	Total  int         `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

type dtoTopTracks struct {
	Items  []dtoTrack `json:"items"`
	Total  int        `json:"total"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
}
