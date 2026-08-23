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

	Popularity int
	Followers  int64

	// ImageURL is the widest asset; ThumbURL a small one for list rows. Two fields rather than
	// the whole array: the UI has exactly two jobs, thumbnail and hero, and storing five URLs
	// to serve two is a schema paying rent for nothing.
	//
	// Both are a refreshable CACHE, never an identifier -- no row keys off them, and the
	// staleness check rewrites them from a fresh response. See docs/SPECS.md 2.7.
	ImageURL string
	ThumbURL string

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

	// ImageURL is the widest cover; ThumbURL a small one for list rows. See Artist for why
	// these are two fields and why they are a cache rather than an identifier.
	ImageURL string
	ThumbURL string

	TotalTracks int
	ArtistIDs   []string
	RefreshedAt time.Time
	Missing     bool
}

// ArtistProfile is the external enrichment for one artist: structured facts from MusicBrainz,
// prose and imagery from TheAudioDB.
//
// # Why the split is not a preference
//
// Neither source is a superset of the other, and the division is what the data forces rather
// than a matter of taste. MusicBrainz holds no prose and no artist images at all. TheAudioDB
// returns `intMembers` as a bare COUNT with no names, a free-text country ("Waddinxveen,
// Netherlands" where MusicBrainz gives `NL` plus the city), and a formation year that
// disagrees with MusicBrainz and sometimes with its own biography.
//
// So MusicBrainz wins every structured fact and TheAudioDB wins prose and imagery, and the
// Sources block records which source each half actually came from — because a partially
// resolved profile is normal, not an error.
type ArtistProfile struct {
	// ArtistID is the Spotify artist ID this profile belongs to. Nothing keys off MBID.
	ArtistID string

	// MBID is the MusicBrainz identifier, empty when the artist could not be resolved.
	//
	// An empty MBID with a set RefreshedAt is a TOMBSTONE: an artist MusicBrainz has never
	// linked will not resolve tomorrow either, and re-asking nightly spends a request on a
	// known answer. It expires like the other negative caches, because it can be wrong.
	MBID string

	// ResolvedVia records HOW the MBID was found: "link" for the URL relationship a
	// MusicBrainz editor asserted, "override" for a manual `spotistats mbid set`.
	//
	// There is deliberately no "search" value. A fuzzy name match attaches the wrong
	// biography, members and country to a real band, and nothing downstream can detect it —
	// the profile renders a confident, wrong answer. An unresolved artist is a visible gap the
	// reader can interpret; a mis-resolved one is a lie they cannot see.
	ResolvedVia string

	// MBGenres are MusicBrainz's vote-counted genre tags. DISPLAY ONLY.
	//
	// They must never reach AGG#GENRE: MusicBrainz and Spotify are different taxonomies, so
	// merging them double-counts one artist under two vocabularies, and genre rows already
	// legitimately over-sum the total so nothing downstream would flag it. Adopting them for
	// real costs a schema-version bump and a full recompute, not a config flag.
	MBGenres []string

	// ArtistType is "Group", "Person", "Orchestra", "Choir" or "Character".
	ArtistType string

	// Country is an ISO 3166-1 code. AreaName and BeginAreaName are the country and the city
	// of origin respectively, which are different facts: a band can be Dutch and have formed
	// in Waddinxveen.
	Country       string
	AreaName      string
	BeginAreaName string

	// BeganAt is MusicBrainz's life-span begin, stored VERBATIM because its precision varies:
	// "2008", "2008-04" or "2008-04-17". BeganPrecision says which ("year", "month", "day").
	// Parsing it into a time.Time would invent a day the data does not claim — the same reason
	// Album keeps ReleaseDate as a string.
	BeganAt        string
	BeganPrecision string
	EndedAt        string
	EndedPrecision string
	Ended          bool

	Members []Member

	// AudioDBID, Biography and BiographyLang come from TheAudioDB.
	//
	// One language is stored, not fifteen. TheAudioDB returns strBiography plus fourteen
	// translations; persisting all of them multiplies the item ~15x for content nothing
	// renders. BiographyLang records which one was kept.
	AudioDBID     string
	Biography     string
	BiographyLang string

	Images ArtistImages

	// Sources records provenance per block, so a profile that resolved on MusicBrainz and
	// 429'd on TheAudioDB is a normal partially-populated row rather than an error.
	Sources ProfileSources

	RefreshedAt time.Time
}

// Resolved reports whether the artist has a MusicBrainz identity.
func (p ArtistProfile) Resolved() bool { return p.MBID != "" }

// Member is one person's membership of a group, or one group a person belonged to.
type Member struct {
	Name string
	MBID string
	// Instruments are the attributes MusicBrainz records on the membership relationship.
	Instruments []string
	// Begin and End are verbatim, variable-precision dates for the same reason as BeganAt.
	Begin string
	End   string
	// Ended distinguishes a former member from a current one whose start date is unknown.
	Ended bool
}

// ArtistImages are TheAudioDB's artwork URLs. Empty strings are dropped at mapping time, so an
// absent image is absent rather than an empty URL every consumer must special-case.
type ArtistImages struct {
	Thumb     string
	Logo      string
	Cutout    string
	ClearArt  string
	WideThumb string
	Banner    string
	// Fanart holds however many of the four fanart slots were populated.
	Fanart []string
}

// Any reports whether there is at least one image to render.
func (i ArtistImages) Any() bool {
	return i.Thumb != "" || i.Logo != "" || i.Cutout != "" || i.ClearArt != "" ||
		i.WideThumb != "" || i.Banner != "" || len(i.Fanart) > 0
}

// ProfileSources names the origin of each block of a profile.
type ProfileSources struct {
	// Facts is "musicbrainz" or empty. Prose and Images are "theaudiodb" or empty.
	Facts  string
	Prose  string
	Images string
}

// Source names, kept as constants so provenance strings cannot drift between the two mappers
// and the attribution the UI renders.
const (
	SourceMusicBrainz = "musicbrainz"
	SourceTheAudioDB  = "theaudiodb"

	// ResolvedViaLink is an MBID found through the Spotify URL relationship a MusicBrainz
	// editor asserted. ResolvedViaOverride is a manual correction.
	ResolvedViaLink     = "link"
	ResolvedViaOverride = "override"
)
