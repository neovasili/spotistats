package model

import (
	"fmt"
	"time"
)

// TimestampFormat is the canonical layout for every timestamp Spotistats persists.
//
// The fixed three-digit fractional part is REQUIRED, not cosmetic. Play sort keys
// (PLAY# partitions) and the firstPlayedAt/lastPlayedAt attributes are compared
// LEXICALLY by DynamoDB, and Go's time.RFC3339Nano strips trailing zeros: the instant
// .120 formats as ".12Z", which then sorts AFTER ".123Z" and inverts chronological
// order. Verified empirically. A fixed width makes lexical order and chronological
// order identical.
//
// .golangci.yml forbids time.RFC3339 and time.RFC3339Nano repo-wide so this cannot be
// bypassed by accident.
const TimestampFormat = "2006-01-02T15:04:05.000Z07:00"

// Layouts accepted when reading foreign timestamps. Parsing is tolerant because
// neither source guarantees a fractional part; only formatting must be fixed-width.
const (
	// layoutSpotifyMillis matches recently_played.played_at, e.g. 2025-03-14T21:04:33.123Z.
	layoutSpotifyMillis = "2006-01-02T15:04:05.999Z07:00"
	// layoutSecondsZ matches the GDPR export ts field, e.g. 2025-03-14T21:04:33Z.
	layoutSecondsZ = "2006-01-02T15:04:05Z07:00"
	// layoutExportSpace matches the older export/basic-history form, e.g. 2025-03-14 21:04.
	layoutExportSpace  = "2006-01-02 15:04"
	layoutExportSpaceS = "2006-01-02 15:04:05"
)

// FormatTS renders t in TimestampFormat, normalised to UTC. Every timestamp written
// to DynamoDB goes through here.
func FormatTS(t time.Time) string {
	return t.UTC().Format(TimestampFormat)
}

// ParseTS parses a timestamp previously written by FormatTS. It is strict: use it for
// values that Spotistats itself produced, and ParseSpotifyTS / ParseExportTS for
// foreign input.
func ParseTS(s string) (time.Time, error) {
	t, err := time.Parse(TimestampFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("model: parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

// ParseSpotifyTS parses the played_at field of the recently-played endpoint, which
// carries milliseconds in practice but is not contractually guaranteed to.
func ParseSpotifyTS(s string) (time.Time, error) {
	return parseAny(s, "spotify played_at", layoutSpotifyMillis, layoutSecondsZ)
}

// ParseExportTS parses the ts field of the GDPR Extended Streaming History export,
// which uses second precision, plus the older space-separated forms.
func ParseExportTS(s string) (time.Time, error) {
	return parseAny(s, "export ts", layoutSecondsZ, layoutSpotifyMillis, layoutExportSpaceS, layoutExportSpace)
}

func parseAny(s, what string, layouts ...string) (time.Time, error) {
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("model: parse %s %q: no accepted layout matched", what, s)
}

// UnixMillis returns t as a Unix millisecond timestamp. The recently-played cursors
// (after / before) are specified in these units.
func UnixMillis(t time.Time) int64 { return t.UnixMilli() }

// FromUnixMillis converts a Unix millisecond timestamp to a UTC time.
func FromUnixMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
