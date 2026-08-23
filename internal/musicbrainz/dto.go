package musicbrainz

// Wire structs for the MusicBrainz web service. Only the fields Spotistats uses are declared,
// matching the internal/spotify/dto.go convention: an unused field is a field nobody keeps
// accurate.

// dtoURLBatch is the response to a URL lookup with TWO OR MORE `resource` parameters.
//
// With exactly ONE resource MusicBrainz returns the bare URL entity instead -- see dtoURL and
// the decode in ResolveSpotifyArtists.
type dtoURLBatch struct {
	Count  int      `json:"url-count"`
	Offset int      `json:"url-offset"`
	URLs   []dtoURL `json:"urls"`
}

// dtoURL is one URL entity and its relationships.
type dtoURL struct {
	ID        string        `json:"id"`
	Resource  string        `json:"resource"`
	Relations []dtoRelation `json:"relations"`
}

// dtoRelation is one relationship on an entity.
//
// Direction is load-bearing, not informational: on a Group, "member of band" with
// direction "backward" yields the group's MEMBERS, while "forward" on a Person yields the
// bands that person belonged to. Filtering on Type alone stores bands as members of people.
type dtoRelation struct {
	Type      string     `json:"type"`
	Direction string     `json:"direction"`
	Artist    *dtoArtist `json:"artist"`
	// Attributes carries instrument names on a membership relationship.
	Attributes []string `json:"attributes"`
	Begin      string   `json:"begin"`
	End        string   `json:"end"`
	Ended      bool     `json:"ended"`
}

// dtoArtist is an artist entity.
type dtoArtist struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Type      string        `json:"type"`
	Country   string        `json:"country"`
	Area      *dtoArea      `json:"area"`
	BeginArea *dtoArea      `json:"begin-area"`
	LifeSpan  *dtoLifeSpan  `json:"life-span"`
	Genres    []dtoGenre    `json:"genres"`
	Relations []dtoRelation `json:"relations"`
}

type dtoArea struct {
	Name string `json:"name"`
}

// dtoLifeSpan holds variable-precision dates: "2008", "2008-04" or "2008-04-17". They are
// kept as strings all the way to storage; parsing invents precision the data does not claim.
type dtoLifeSpan struct {
	Begin string `json:"begin"`
	End   string `json:"end"`
	Ended bool   `json:"ended"`
}

// dtoGenre is a vote-counted genre tag.
type dtoGenre struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}
