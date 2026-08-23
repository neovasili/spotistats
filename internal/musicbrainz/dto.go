package musicbrainz

// Wire structs for the MusicBrainz web service.
//
// The artist-facing ones are EXPORTED, unlike the Spotify client's, because internal/enrich
// declares an interface over this client and an interface method cannot name an unexported
// type. They stay wire shapes rather than becoming a second domain model: ToProfile is the only
// thing that reads them. Only the fields Spotistats uses are declared,
// matching the internal/spotify/dto.go convention: an unused field is a field nobody keeps
// accurate.

// urlBatch is the response to a URL lookup with TWO OR MORE `resource` parameters.
//
// With exactly ONE resource MusicBrainz returns the bare URL entity instead -- see URLEntity and
// the decode in ResolveSpotifyArtists.
type urlBatch struct {
	Count  int         `json:"url-count"`
	Offset int         `json:"url-offset"`
	URLs   []URLEntity `json:"urls"`
}

// URLEntity is one URL entity and its relationships.
type URLEntity struct {
	ID        string     `json:"id"`
	Resource  string     `json:"resource"`
	Relations []Relation `json:"relations"`
}

// Relation is one relationship on an entity.
//
// Direction is load-bearing, not informational: on a Group, "member of band" with
// direction "backward" yields the group's MEMBERS, while "forward" on a Person yields the
// bands that person belonged to. Filtering on Type alone stores bands as members of people.
type Relation struct {
	Type      string  `json:"type"`
	Direction string  `json:"direction"`
	Artist    *Artist `json:"artist"`
	// Attributes carries instrument names on a membership relationship.
	Attributes []string `json:"attributes"`
	Begin      string   `json:"begin"`
	End        string   `json:"end"`
	Ended      bool     `json:"ended"`
}

// Artist is an artist entity.
type Artist struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	Country   string     `json:"country"`
	Area      *Area      `json:"area"`
	BeginArea *Area      `json:"begin-area"`
	LifeSpan  *LifeSpan  `json:"life-span"`
	Genres    []Genre    `json:"genres"`
	Relations []Relation `json:"relations"`
}

type Area struct {
	Name string `json:"name"`
}

// LifeSpan holds variable-precision dates: "2008", "2008-04" or "2008-04-17". They are
// kept as strings all the way to storage; parsing invents precision the data does not claim.
type LifeSpan struct {
	Begin string `json:"begin"`
	End   string `json:"end"`
	Ended bool   `json:"ended"`
}

// Genre is a vote-counted genre tag.
type Genre struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}
