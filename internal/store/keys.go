package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/neovasili/spotistats/internal/model"
)

// Key prefixes and fixed sort keys.
const (
	prefixPlay   = "PLAY#"
	prefixTrack  = "TRACK#"
	prefixArtist = "ARTIST#"
	prefixAlbum  = "ALBUM#"
	prefixTop    = "TOP#"
	prefixHist   = "HIST#"

	// PKState is the single partition holding cursors, markers and configuration.
	PKState = "STATE"

	// SKMeta is the sort key of every dimension row.
	SKMeta = "META"

	// SKTopVersion versions the materialised leaderboard payload, so its shape can change
	// without a migration: write V2 alongside V1 and switch readers over.
	SKTopVersion = "V1"

	SKPollCursor   = "POLL_CURSOR"
	SKEnrichCursor = "ENRICH_CURSOR"
	SKConfig       = "CONFIG"
	SKCoverage     = "COVERAGE"

	prefixIngest = "INGEST#"
	prefixGap    = "GAP#"

	SKHistHour    = "HOUR"
	SKHistWeekday = "DOW"

	playSKSeparator = "#"
)

// PlayPartition returns the partition key for a play at t.
//
// It is derived from the UTC month, NOT the listener's local month, even though every
// aggregate period key is local. The reasons:
//
//   - This is a storage partition, not a semantic period. Its only jobs are bounding
//     partition size and making a time-range scan a predictable number of partition reads.
//   - The timezone is a runtime setting (docs/SPECS.md 14.4 makes it changeable without a
//     redeploy). If partitions were local, changing it would leave every previously
//     written play in the wrong partition -- silently invisible to range queries, and
//     unrecoverable without a full table migration.
//
// The cost is one extra partition read: a local month spans two UTC months, so scanning
// local 2025-03 in Europe/Madrid reads UTC partitions 2025-02 and 2025-03. Sort keys are
// full UTC instants, so a range condition filters precisely inside each partition.
//
// PlayPartitionsBetween owns that fan-out so callers never have to think about it.
func PlayPartition(t time.Time) string {
	return prefixPlay + t.UTC().Format("2006-01")
}

// PlaySK returns the sort key for a play: its UTC instant followed by the track ID.
//
// Including the track ID keeps two tracks played in the same millisecond distinct, and
// makes the full primary key the natural idempotency key for ingestion.
func PlaySK(t time.Time, trackID string) string {
	return model.FormatTS(t) + playSKSeparator + trackID
}

// ParsePlaySK reverses PlaySK.
func ParsePlaySK(sk string) (time.Time, string, error) {
	ts, trackID, ok := strings.Cut(sk, playSKSeparator)
	if !ok {
		return time.Time{}, "", fmt.Errorf("store: play sort key %q is not {timestamp}#{trackID}", sk)
	}
	t, err := model.ParseTS(ts)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("store: play sort key %q: %w", sk, err)
	}
	if trackID == "" {
		return time.Time{}, "", fmt.Errorf("store: play sort key %q has an empty track ID", sk)
	}
	return t, trackID, nil
}

// PlayPartitionsBetween returns every play partition covering the half-open UTC range
// [from, to), oldest first. It returns nil when the range is empty or inverted.
//
// This is the fan-out that makes the UTC-partition choice invisible to callers: ask for a
// local calendar month and get back the two UTC partitions it touches.
func PlayPartitionsBetween(from, to time.Time) []string {
	if !from.Before(to) {
		return nil
	}
	from, to = from.UTC(), to.UTC()

	// Walk month by month from the first partition to the one containing the last instant
	// inside the range. `to` is exclusive, so a range ending exactly on a month boundary
	// must not pull in the following partition.
	cur := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)
	last := to.Add(-time.Nanosecond)
	end := time.Date(last.Year(), last.Month(), 1, 0, 0, 0, 0, time.UTC)

	var out []string
	for !cur.After(end) {
		out = append(out, prefixPlay+cur.Format("2006-01"))
		cur = cur.AddDate(0, 1, 0)
	}
	return out
}

// TrackPK, ArtistPK and AlbumPK build dimension partition keys.
func TrackPK(id string) string  { return prefixTrack + id }
func ArtistPK(id string) string { return prefixArtist + id }
func AlbumPK(id string) string  { return prefixAlbum + id }

// DimensionPK builds the dimension row key for any entity dimension.
func DimensionPK(dim model.Dim, id string) (string, error) {
	switch dim {
	case model.DimTrack:
		return TrackPK(id), nil
	case model.DimArtist:
		return ArtistPK(id), nil
	case model.DimAlbum:
		return AlbumPK(id), nil
	default:
		return "", fmt.Errorf("store: dimension %q has no metadata rows", dim)
	}
}

// TrackGSI1PK is the GSI1 partition key for a play, which groups every play of one track.
func TrackGSI1PK(trackID string) string { return prefixTrack + trackID }

// TopPK is the partition key of a materialised leaderboard.
func TopPK(dim model.Dim, period model.Period) string {
	return prefixTop + string(dim) + "#" + string(period)
}

// HistPK is the partition key of a listening-rhythm histogram.
func HistPK(period model.Period) string { return prefixHist + string(period) }

// IngestSK is the sort key of a month's ingest marker, which records the authoritative
// source for that month.
func IngestSK(month model.Period) string { return prefixIngest + string(month) }

// GapSK is the sort key of a gap marker. The timestamp keeps successive markers distinct
// and naturally ordered.
func GapSK(detectedAt time.Time) string { return prefixGap + model.FormatTS(detectedAt) }
