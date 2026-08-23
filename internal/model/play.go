package model

import (
	"fmt"
	"time"
)

// Source records where a play was ingested from. The two sources differ in fidelity,
// which is why this is stored on every row rather than inferred.
type Source string

const (
	// SourceAPI is the recently-played endpoint. It reports THAT a track played at a
	// given time but never how long it played for, so msPlayed is an estimate.
	SourceAPI Source = "api"
	// SourceExport is the GDPR Extended Streaming History export, which carries exact
	// ms_played.
	SourceExport Source = "export"
)

// Valid reports whether s is a known source.
func (s Source) Valid() bool { return s == SourceAPI || s == SourceExport }

// DefaultMinMsPlayed is the threshold for counting a stream as a play. Spotify's own
// recently-played endpoint only reports tracks played for roughly 30 seconds, so the
// export importer applies the same floor -- otherwise API-era and export-era play
// counts would not be comparable.
const DefaultMinMsPlayed int64 = 30_000

// ExportFields are the extra columns the GDPR export provides and the API does not.
// All zero for SourceAPI plays.
type ExportFields struct {
	Platform    string
	Country     string
	ReasonStart string
	ReasonEnd   string
	Shuffle     bool
	Skipped     bool
	Offline     bool

	// TrackName, ArtistName and AlbumName are what the export actually supplies: names, as
	// free text, with no Spotify ID for the artist or album.
	//
	// They are stored on the play row rather than discarded because they are the ONLY durable
	// record of what a seventeen-year-old play was. Resolving 13,000 tracks through the API is
	// a weeks-long job under a development-mode quota, so identity has to be derivable without
	// it -- see NameKey and FactsForTrack. Keeping the names on the row is also what makes the
	// eventual upgrade to real Spotify IDs a reconcile rather than a reimport.
	TrackName  string
	ArtistName string
	AlbumName  string
}

// Play is one listening event: the immutable fact everything else is derived from.
//
// PlayedAt is always a UTC instant. The period keys a play contributes to are derived
// from it in the listener's local zone at aggregation time, never stored on the row.
type Play struct {
	PlayedAt  time.Time
	TrackID   string
	ArtistIDs []string
	AlbumID   string

	// MsPlayed is exact for SourceExport and an estimate for SourceAPI.
	MsPlayed int64

	Source Source

	// MsEstimated mirrors Source and must always agree with it: api implies estimated,
	// export implies exact. The constructors are the only way to build a Play, so an
	// inconsistent combination is unrepresentable in practice; Validate re-checks it
	// for rows read back out of storage.
	MsEstimated bool

	Export ExportFields
}

// NewAPIPlay builds a play from the recently-played endpoint.
//
// That endpoint returns no duration whatsoever, so msPlayed is taken as the track's
// FULL duration. This over-counts skipped tracks, and it is the reason the API exposes
// an estimated-vs-exact split (docs/SPECS.md 2.2, 6.4). This constructor is the only
// way to produce a Source=api play, so MsEstimated can never be false for one.
func NewAPIPlay(playedAt time.Time, t Track) (Play, error) {
	p := Play{
		PlayedAt:    playedAt.UTC(),
		TrackID:     t.ID,
		ArtistIDs:   dedupeStrings(t.ArtistIDs),
		AlbumID:     t.AlbumID,
		MsPlayed:    t.DurationMs,
		Source:      SourceAPI,
		MsEstimated: true,
	}
	if err := p.Validate(); err != nil {
		return Play{}, err
	}
	return p, nil
}

// NewExportPlay builds a play from a GDPR export record, which carries exact ms_played.
func NewExportPlay(playedAt time.Time, msPlayed int64, t Track, ext ExportFields) (Play, error) {
	p := Play{
		PlayedAt:    playedAt.UTC(),
		TrackID:     t.ID,
		ArtistIDs:   dedupeStrings(t.ArtistIDs),
		AlbumID:     t.AlbumID,
		MsPlayed:    msPlayed,
		Source:      SourceExport,
		MsEstimated: false,
		Export:      ext,
	}
	if err := p.Validate(); err != nil {
		return Play{}, err
	}
	return p, nil
}

// Validate enforces the invariants every stored play must satisfy.
//
// MsPlayed must be strictly positive and is a hard reject rather than a clamp: the
// recently-played payload always embeds track.duration_ms, so a zero here means a
// mapping bug, and silently contributing zero minutes forever is far worse than
// failing the run.
func (p Play) Validate() error {
	if p.PlayedAt.IsZero() {
		return fmt.Errorf("%w: playedAt is zero", ErrInvalidPlay)
	}
	if p.TrackID == "" {
		return fmt.Errorf("%w: trackID is empty", ErrInvalidPlay)
	}
	if !p.Source.Valid() {
		return fmt.Errorf("%w: unknown source %q", ErrInvalidPlay, p.Source)
	}
	if p.MsPlayed <= 0 {
		return fmt.Errorf("%w: msPlayed is %d, want > 0 (track %s at %s)",
			ErrInvalidPlay, p.MsPlayed, p.TrackID, FormatTS(p.PlayedAt))
	}
	if wantEstimated := p.Source == SourceAPI; p.MsEstimated != wantEstimated {
		return fmt.Errorf("%w: source %q requires msEstimated=%v, got %v",
			ErrInvalidPlay, p.Source, wantEstimated, p.MsEstimated)
	}
	return nil
}

// dedupeStrings returns s with empty values dropped and duplicates removed, preserving
// first-seen order. Order is preserved because artist order is meaningful (the first
// artist is the primary one) and because deterministic output keeps aggregate deltas
// byte-comparable in tests.
func dedupeStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
