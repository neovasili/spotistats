package model

import "time"

// SchemaVersion is the version of the DynamoDB item layout this build writes. It is
// persisted in the config row so a binary cannot silently write a layout the table was
// not built for.
const SchemaVersion = 1

// PollCursor tracks how far the capture job has consumed the recently-played feed.
// LastPlayedAt is fed back as the endpoint's `after` cursor on the next run.
type PollCursor struct {
	LastPlayedAt time.Time
	LastRunAt    time.Time
	LastStatus   string
}

// GapMarker records a capture run whose response filled the requested limit, meaning
// listening may have exceeded the polling window and plays may have been lost
// irrecoverably (the API retains only about 50). Its presence is the signal to increase
// capture frequency.
type GapMarker struct {
	DetectedAt    time.Time
	WindowStart   time.Time
	WindowEnd     time.Time
	ItemsReturned int
	Limit         int
}

// IngestMarker claims a calendar month for a source. The GDPR export is authoritative
// for any month it covers, so the importer uses these markers to decide which api-sourced
// rows to supersede.
type IngestMarker struct {
	Month      Period
	Source     Source
	ImportedAt time.Time
	PlayCount  int64
}

// ConfigRow is written once and then verified on every startup. A changed timezone
// would silently re-file every subsequently derived period key against history derived
// under the old zone, so a mismatch must be a loud failure rather than a drift.
type ConfigRow struct {
	Timezone      string
	SchemaVersion int
	WrittenAt     time.Time
}

// CoverageRow records facts that can only be established by reading the whole play history, so
// that the dashboard need not re-derive them and need not guess.
//
// It exists because two figures are otherwise unobtainable or wrong:
//
//   - The all-time firstPlayedAt and lastPlayedAt on an aggregate row are best-effort: DynamoDB
//     cannot maintain a true min/max in one request, so they reflect WRITE order, not play
//     order. Out-of-order ingestion leaves them badly astray, and a windowed reconcile cannot
//     correct an all-time bound.
//   - Genre coverage cannot be derived from the genre aggregates. A play whose artists carry
//     three genres contributes to three rows, so summing them overstates coverage, and capping
//     the sum at the total silently reports 100% whenever the overcount exceeds the shortfall.
//     Only a per-play pass can tell how much listening carries any genre at all.
type CoverageRow struct {
	FirstPlayedAt time.Time
	LastPlayedAt  time.Time

	// TotalPlays and TotalMs are the exact figures from the pass that produced this row.
	TotalPlays int64
	TotalMs    int64

	// PlaysWithGenre and MsWithGenre count only plays whose artists carry at least one genre.
	PlaysWithGenre int64
	MsWithGenre    int64

	// PlaysWithArtist and MsWithArtist count plays that carry artist attribution at all.
	//
	// This is not a nicety. The GDPR export names artists as free text and gives no artist ID,
	// so a play imported before its track was resolved contributes to TOTAL but to no ARTIST or
	// ALBUM row. The resulting leaderboards are not merely incomplete, they are WRONG: measured
	// against the export, the top artist was missing from the top five entirely and the ones
	// shown read at a quarter of their true totals -- while looking entirely plausible.
	//
	// Recording the coverage is what lets the dashboard say "these rankings are incomplete"
	// instead of presenting a quarter of the truth as the whole of it.
	PlaysWithArtist int64
	MsWithArtist    int64

	ComputedAt time.Time
}

// ArtistCoverage is the exact share of listening time that carries artist attribution.
func (c CoverageRow) ArtistCoverage() float64 {
	if c.TotalMs <= 0 {
		return 0
	}
	return float64(c.MsWithArtist) / float64(c.TotalMs)
}

// GenreCoverage is the exact share of listening time that carries at least one genre.
func (c CoverageRow) GenreCoverage() float64 {
	if c.TotalMs <= 0 {
		return 0
	}
	return float64(c.MsWithGenre) / float64(c.TotalMs)
}
