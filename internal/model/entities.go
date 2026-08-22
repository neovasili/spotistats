package model

import "time"

// Track is a Spotify track as Spotistats stores it. Field set is deliberately narrow:
// only what the dashboard, the explorer or the aggregate math actually needs.
//
// Audio features (danceability, energy, valence, tempo) are absent on purpose -- the
// Audio Features and Audio Analysis endpoints were restricted to pre-2024-11-27 apps
// and are permanently unavailable to this one. See docs/SPECS.md 2.3.
type Track struct {
	ID         string
	Name       string
	DurationMs int64
	AlbumID    string
	ArtistIDs  []string
	Popularity int
	Explicit   bool
	ISRC       string
	URI        string

	// RefreshedAt is when this row was last written from the API. The capture job
	// opportunistically refreshes dimension rows older than 30 days.
	RefreshedAt time.Time

	// Missing marks a tombstone: Spotify returned null for this ID, so it can never
	// resolve (removed, relinked or invalid). Without it, enrichment would re-request
	// the same dead IDs on every run forever.
	Missing bool
}

// Artist is a Spotify artist. Genres live here and nowhere else: Spotify has no
// per-track genre, so all genre rollups are derived from the artists on a play.
type Artist struct {
	ID   string
	Name string

	// Genres is Spotify's own tagging, lowercase and free-form. It is frequently
	// EMPTY -- most artists carry no genres at all, which is why genre aggregates
	// legitimately under-sum the total. See AggregateDeltas.
	Genres []string

	Popularity  int
	Followers   int64
	ImageURL    string
	RefreshedAt time.Time
	Missing     bool

	// EnrichedAt is when the FULL artist object was last fetched from GET /v1/artists.
	//
	// It is separate from RefreshedAt because an artist row has two independent sources. The
	// name arrives free, embedded in every recently-played track; genres exist only on the
	// full object. A row can therefore be freshly written and still have never been enriched,
	// and only EnrichedAt distinguishes "this artist has no genres" from "we never asked".
	// Zero means never enriched.
	EnrichedAt time.Time
}

// Album is a Spotify album.
type Album struct {
	ID   string
	Name string

	// ReleaseDate is Spotify's string form, which may be a year, a year-month or a
	// full date. ReleaseDatePrecision says which ("year", "month" or "day"), so it is
	// kept verbatim rather than parsed into a time.Time that would imply false precision.
	ReleaseDate          string
	ReleaseDatePrecision string

	ImageURL    string
	TotalTracks int
	ArtistIDs   []string
	RefreshedAt time.Time
	Missing     bool
}
