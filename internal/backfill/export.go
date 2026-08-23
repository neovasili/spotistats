// Package backfill imports Spotify's "Extended Streaming History" GDPR export.
//
// The export is the only source of deep history: the Web API's recently-played endpoint
// retains roughly 50 plays and cannot page backwards, so everything before the first capture
// run exists only here.
//
// # What the export is, and how it differs from the API
//
// Each record is one listening stretch with an EXACT ms_played, which is the export's great
// advantage: API-sourced plays have to assume the track's full duration and therefore
// over-count skips (docs/SPECS.md 2.2). Export plays are stored with msEstimated=false and
// contribute to the exact totals.
//
// What it does NOT carry is entity identity beyond the track: there is a spotify_track_uri but
// no artist or album ID, only their names as free text. Since every aggregate is keyed by
// Spotify ID, the importer must resolve those IDs from the API before the plays are usable for
// anything but track and total figures. That resolution is the slow part; see enrich.go.
package backfill

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/neovasili/spotistats/internal/model"
)

// Record is one entry of a Streaming_History_*.json file.
//
// Only the fields Spotistats uses are declared; the export also carries ip_addr and
// incognito_mode, which are deliberately not read, let alone stored.
type Record struct {
	TS         string `json:"ts"`
	MsPlayed   int64  `json:"ms_played"`
	Platform   string `json:"platform"`
	Country    string `json:"conn_country"`
	TrackName  string `json:"master_metadata_track_name"`
	ArtistName string `json:"master_metadata_album_artist_name"`
	AlbumName  string `json:"master_metadata_album_album_name"`
	TrackURI   string `json:"spotify_track_uri"`

	// EpisodeURI and AudiobookURI identify non-music rows. Podcasts are out of scope
	// (docs/SPECS.md 2.1) and are skipped rather than counted as music.
	EpisodeURI   string `json:"spotify_episode_uri"`
	AudiobookURI string `json:"audiobook_uri"`

	ReasonStart string `json:"reason_start"`
	ReasonEnd   string `json:"reason_end"`
	Shuffle     bool   `json:"shuffle"`
	Skipped     bool   `json:"skipped"`
	Offline     bool   `json:"offline"`
}

// trackURIPrefix is the only URI form that identifies a track.
const trackURIPrefix = "spotify:track:"

// TrackID extracts the bare Spotify ID, or "" when the record does not identify a track.
//
// A local file or a podcast episode has no track URI, and those rows can never be aggregated:
// there is no entity to attribute them to.
func (r Record) TrackID() string {
	if !strings.HasPrefix(r.TrackURI, trackURIPrefix) {
		return ""
	}
	return strings.TrimPrefix(r.TrackURI, trackURIPrefix)
}

// PlayedAt parses the record timestamp.
//
// The export's `ts` is the moment the stream ENDED, whereas nothing documents the same for the
// API's played_at. Both are used as-is rather than one being shifted by ms_played: the two
// sources then attribute a play to the same period by the same rule, which matters more at a
// month boundary than a few minutes of nominal accuracy does.
func (r Record) PlayedAt() (time.Time, error) {
	// Export timestamps are "2009-10-31T18:29:21Z": second precision, always UTC.
	//
	// The repo bans time.RFC3339 because a second-precision layout used as a DynamoDB sort key
	// makes distinct millisecond instants collide. That hazard is absent here -- this parses an
	// input string into a time.Time, and every key derived from it is formatted through
	// model.FormatTS. The layouts are therefore spelled out locally, leaving the ban to protect
	// the places that genuinely need it.
	const exportLayout = "2006-01-02T15:04:05Z"
	if t, err := time.Parse(exportLayout, r.TS); err == nil {
		return t.UTC(), nil
	}
	// Tolerate a fractional-second form in case a future export changes precision.
	const fractional = "2006-01-02T15:04:05.999Z07:00"
	if t, err := time.Parse(fractional, r.TS); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("backfill: parse ts %q: want %q", r.TS, exportLayout)
}

// ExportFields projects the provenance fields worth keeping on the play row.
func (r Record) ExportFields() model.ExportFields {
	return model.ExportFields{
		Platform:    r.Platform,
		Country:     r.Country,
		ReasonStart: r.ReasonStart,
		ReasonEnd:   r.ReasonEnd,
		Shuffle:     r.Shuffle,
		Skipped:     r.Skipped,
		Offline:     r.Offline,
	}
}

// SkipReason explains why a record is not importable, or "" when it is.
type SkipReason string

const (
	SkipNoTrackURI SkipReason = "no track URI (local file or unavailable)"
	SkipPodcast    SkipReason = "podcast episode"
	SkipAudiobook  SkipReason = "audiobook"
	SkipTooShort   SkipReason = "below the minimum play duration"
	SkipBadTS      SkipReason = "unparseable timestamp"
)

// Filter decides whether a record counts as a play.
//
// # Why minMs, and why not reason_end == "trackdone"
//
// The obvious idea is to match the API exactly. recently-played records a track when it is
// played to COMPLETION -- not when it passes 30 seconds -- so `reason_end == "trackdone"` is
// the faithful equivalent, and an earlier version of docs/SPECS.md 4.2 asserted the API used a
// ~30s threshold, which is simply wrong.
//
// Measured against this corpus, matching the API exactly would discard 1,440 hours -- sixty
// days -- of genuinely attended listening, because a track played for four minutes and then
// skipped ends with reason_end="fwdbtn" and would vanish entirely. Seventeen years of real
// history is not worth sacrificing for comparability with the single day the API era covers.
//
// A duration threshold keeps 99.8% of attended time while dropping the sub-30-second noise of
// skips and mis-taps. The resulting asymmetry is real and disclosed: in the API era a play is a
// completion, in the export era it is a listening stretch of at least minMs.
func (r Record) Filter(minMs int64) SkipReason {
	switch {
	case r.EpisodeURI != "":
		return SkipPodcast
	case r.AudiobookURI != "":
		return SkipAudiobook
	case r.TrackID() == "":
		return SkipNoTrackURI
	case r.MsPlayed < minMs || r.MsPlayed <= 0:
		// The <= 0 arm is not redundant: minMs may be 0, and a zero-duration row is never a
		// play. model.Play.Validate would reject it anyway, loudly.
		return SkipTooShort
	}
	if _, err := r.PlayedAt(); err != nil {
		return SkipBadTS
	}
	return ""
}

// Files lists the streaming-history JSON files under dir, in a stable order.
//
// Both Audio and Video files are included. The Video ones are NOT podcasts: they are music
// tracks played with a video stream (reason_start="switched-to-video"), carrying an ordinary
// spotify_track_uri, and excluding them would silently drop real listening. The read-me PDF and
// any other JSON the export ships are ignored by the name prefix.
func Files(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "Streaming_History_*.json"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("backfill: no Streaming_History_*.json files in %s", dir)
	}
	sort.Strings(matches)
	return matches, nil
}

// ReadFile streams one export file, calling fn for each record.
//
// Streaming rather than unmarshalling the whole array keeps memory bounded by one record: the
// files run to 12 MB each and a full export is hundreds of megabytes, and this same code path
// is what a much larger export would need.
func ReadFile(path string, fn func(Record) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("backfill: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	dec := json.NewDecoder(f)
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("backfill: read %s: %w", path, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("backfill: %s: expected a JSON array, got %v", path, tok)
	}
	for dec.More() {
		var r Record
		if err := dec.Decode(&r); err != nil {
			return fmt.Errorf("backfill: decode a record in %s: %w", path, err)
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("backfill: trailing content in %s: %w", path, err)
	}
	return nil
}
