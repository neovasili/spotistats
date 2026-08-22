package model

// Dim is an aggregation dimension: what a set of counters is grouped by.
type Dim string

const (
	// DimTotal is the ungrouped total. Its rows have no entity, so their sort key is
	// the literal TotalEntityID.
	DimTotal Dim = "TOTAL"
	DimTrack Dim = "TRACK"
	// DimArtist double-counts plays with multiple artists: a play credited to two
	// artists contributes one play to each. Artist totals therefore sum to MORE than
	// DimTotal, by design.
	DimArtist Dim = "ARTIST"
	DimAlbum  Dim = "ALBUM"
	// DimGenre is derived from the artists on a play, deduplicated across them, since
	// Spotify has no per-track genre. Many artists carry no genres at all, so genre
	// totals sum to LESS than DimTotal and the difference is the unclassified
	// remainder -- see AggregateDeltas.
	DimGenre Dim = "GENRE"
)

// AllDims lists every dimension in the canonical order used by AggregateDeltas.
func AllDims() []Dim { return []Dim{DimTotal, DimTrack, DimAlbum, DimArtist, DimGenre} }

// Valid reports whether d is a known dimension.
func (d Dim) Valid() bool {
	switch d {
	case DimTotal, DimTrack, DimArtist, DimAlbum, DimGenre:
		return true
	}
	return false
}

func (d Dim) String() string { return string(d) }
