package model

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// TotalEntityID is the sort key used by DimTotal rows, which have no entity. It is also
// the sort key of the year/month/all-time rows of every dimension under the key layout
// described on AggKey.
const TotalEntityID = "ALL"

const aggKeyPrefix = "AGG#"

// AggKey identifies one aggregate row: a dimension, a calendar period, and an entity.
type AggKey struct {
	Dim      Dim
	Period   Period
	EntityID string // TotalEntityID when Dim is DimTotal
}

// PK returns the DynamoDB partition key.
//
// The layout is AGG#{DIM}#{PERIOD} with the entity as the sort key, EXCEPT for
// DimTotal at day granularity, which is deliberately folded into its year's partition:
//
//	TOTAL + day    -> PK "AGG#TOTAL#{yyyy}",     SK "{yyyy-mm-dd}"
//	TOTAL + year   -> PK "AGG#TOTAL#{yyyy}",     SK "ALL"
//	TOTAL + month  -> PK "AGG#TOTAL#{yyyy-mm}",  SK "ALL"
//	TOTAL + all    -> PK "AGG#TOTAL#ALL",        SK "ALL"
//	anything else  -> PK "AGG#{DIM}#{PERIOD}",   SK "{entityID}"
//
// The exception exists so the calendar heatmap is a single Query over a year partition
// with begins_with(SK, "2025-") rather than 365 GetItems. Monthly totals live in their
// own partition, so that prefix matches day rows only. This function and SK are the only
// two places in the codebase that know about the exception.
func (k AggKey) PK() string {
	if k.Dim == DimTotal && k.Period.Granularity() == GranularityDay {
		return aggKeyPrefix + string(DimTotal) + "#" + string(k.Period)[:len(layoutYear)]
	}
	return aggKeyPrefix + string(k.Dim) + "#" + string(k.Period)
}

// SK returns the DynamoDB sort key. See PK for the layout.
func (k AggKey) SK() string {
	if k.Dim == DimTotal {
		if k.Period.Granularity() == GranularityDay {
			return string(k.Period)
		}
		return TotalEntityID
	}
	return k.EntityID
}

// ParseAggKey reverses PK and SK.
func ParseAggKey(pk, sk string) (AggKey, error) {
	rest, ok := strings.CutPrefix(pk, aggKeyPrefix)
	if !ok {
		return AggKey{}, fmt.Errorf("%w: partition key %q lacks the %q prefix", ErrInvalidAggKey, pk, aggKeyPrefix)
	}
	dimStr, periodStr, ok := strings.Cut(rest, "#")
	if !ok {
		return AggKey{}, fmt.Errorf("%w: partition key %q is not AGG#{DIM}#{PERIOD}", ErrInvalidAggKey, pk)
	}
	dim := Dim(dimStr)
	if !dim.Valid() {
		return AggKey{}, fmt.Errorf("%w: unknown dimension %q in %q", ErrInvalidAggKey, dimStr, pk)
	}
	period, err := ParsePeriod(periodStr)
	if err != nil {
		return AggKey{}, fmt.Errorf("%w: %w", ErrInvalidAggKey, err)
	}

	if dim == DimTotal {
		// A TOTAL partition holds its own period row (SK "ALL") plus that year's day
		// rows (SK "YYYY-MM-DD"), so the sort key alone says which this is.
		if sk == TotalEntityID {
			return AggKey{Dim: DimTotal, Period: period, EntityID: TotalEntityID}, nil
		}
		dayPeriod, derr := ParsePeriod(sk)
		if derr != nil || dayPeriod.Granularity() != GranularityDay {
			return AggKey{}, fmt.Errorf("%w: TOTAL sort key %q is neither %q nor a YYYY-MM-DD day",
				ErrInvalidAggKey, sk, TotalEntityID)
		}
		if period.Granularity() != GranularityYear {
			return AggKey{}, fmt.Errorf("%w: TOTAL day row %q must live in a year partition, got %q",
				ErrInvalidAggKey, sk, pk)
		}
		if !strings.HasPrefix(sk, string(period)) {
			return AggKey{}, fmt.Errorf("%w: TOTAL day row %q is not inside partition %q",
				ErrInvalidAggKey, sk, pk)
		}
		return AggKey{Dim: DimTotal, Period: dayPeriod, EntityID: TotalEntityID}, nil
	}

	if sk == "" {
		return AggKey{}, fmt.Errorf("%w: empty sort key for dimension %q", ErrInvalidAggKey, dim)
	}
	return AggKey{Dim: dim, Period: period, EntityID: sk}, nil
}

// Validate enforces the structural rules of an aggregate key.
func (k AggKey) Validate() error {
	if !k.Dim.Valid() {
		return fmt.Errorf("%w: unknown dimension %q", ErrInvalidAggKey, k.Dim)
	}
	if !k.Period.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidAggKey, k.Period)
	}
	if k.Dim == DimTotal {
		if k.EntityID != TotalEntityID {
			return fmt.Errorf("%w: DimTotal requires entityID %q, got %q",
				ErrInvalidAggKey, TotalEntityID, k.EntityID)
		}
		return nil
	}
	if k.EntityID == "" {
		return fmt.Errorf("%w: dimension %q requires a non-empty entityID", ErrInvalidAggKey, k.Dim)
	}
	// Day granularity is reserved for DimTotal: per-entity-per-day rows would multiply
	// write volume by the number of distinct entities for no query anyone makes.
	if k.Period.Granularity() == GranularityDay {
		return fmt.Errorf("%w: day granularity is only defined for DimTotal, got %q", ErrInvalidAggKey, k.Dim)
	}
	return nil
}

func (k AggKey) String() string { return k.PK() + " / " + k.SK() }

// Aggregate is a stored counter row.
type Aggregate struct {
	Key AggKey

	// Plays counts every play. PlaysExact counts only those whose duration is known
	// exactly, i.e. export-sourced. PlaysExact <= Plays always.
	Plays      int64
	PlaysExact int64

	// MsPlayed is the total listening time, mixing exact and estimated contributions.
	// MsPlayedExact is the subset contributed by export-sourced plays, so
	// MsPlayedExact <= MsPlayed always.
	MsPlayed      int64
	MsPlayedExact int64

	// FirstPlayedAt and LastPlayedAt are best-effort at write time -- DynamoDB has no
	// MIN/MAX, so out-of-order writes (re-runs, backfill) can leave LastPlayedAt behind
	// the true maximum. The nightly reconcile recomputes them from raw plays. The
	// counters above are always exact.
	FirstPlayedAt time.Time
	LastPlayedAt  time.Time
}

// EstimatedRatio is the fraction of MsPlayed that is estimated rather than measured,
// i.e. 1 - exact/total. Zero when there is no listening time recorded.
//
// The UI must surface an indicator whenever this is above zero: API-sourced plays have
// no duration, so their contribution is the full track length and over-counts skips.
func (a Aggregate) EstimatedRatio() float64 {
	if a.MsPlayed <= 0 {
		return 0
	}
	return 1 - float64(a.MsPlayedExact)/float64(a.MsPlayed)
}

// AggDelta is the contribution of one or more plays to a single aggregate row.
type AggDelta struct {
	Key           AggKey
	Plays         int64
	PlaysExact    int64
	MsPlayed      int64
	MsPlayedExact int64
	FirstPlayedAt time.Time
	LastPlayedAt  time.Time
}

// PlayFacts is everything AggregateDeltas needs about one play. It is deliberately
// self-contained: no lookups, no I/O.
//
// Genres are the caller's responsibility because Spotify exposes them on the ARTIST
// object, not on the track or the play, so resolving them needs the artist rows.
type PlayFacts struct {
	PlayedAt  time.Time
	TrackID   string
	AlbumID   string   // empty means no album deltas
	ArtistIDs []string // deduplicated, first-seen order
	Genres    []string // normalised, deduplicated union across the artists
	MsPlayed  int64
	Estimated bool
}

// FactsFor derives the aggregation inputs for p. genres is the concatenation of the
// Genres of every artist on the play; this function normalises and deduplicates them.
//
// Deduplicating across artists is required, not an optimisation: if two artists on one
// play both carry "gothic metal", counting it twice would let genre play counts exceed
// the ungrouped total and the genre-mix chart could not sum to a meaningful whole.
func FactsFor(p Play, genres []string) PlayFacts {
	return PlayFacts{
		PlayedAt:  p.PlayedAt,
		TrackID:   p.TrackID,
		AlbumID:   p.AlbumID,
		ArtistIDs: dedupeStrings(p.ArtistIDs),
		Genres:    normalizeGenres(genres),
		MsPlayed:  p.MsPlayed,
		Estimated: p.MsEstimated,
	}
}

// NormalizeGenre lowercases, trims and collapses internal whitespace. It deliberately
// does NOT slugify: the normalised form is used verbatim as the aggregate sort key and
// is displayed as-is by the UI, so "gothic metal" must stay readable.
func NormalizeGenre(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// normalizeGenres normalises, drops empties, deduplicates and sorts. Sorting makes
// AggregateDeltas byte-for-byte deterministic regardless of artist order.
func normalizeGenres(genres []string) []string {
	if len(genres) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(genres))
	out := make([]string, 0, len(genres))
	for _, g := range genres {
		n := NormalizeGenre(g)
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// AggregateDeltas returns the complete set of aggregate contributions for one play.
//
// It is PURE: no I/O, no clock, no randomness. The same inputs always produce a
// byte-identical result, which is what makes it exhaustively testable and what lets the
// backfill importer accumulate deltas in memory instead of issuing millions of writes.
//
// The result has exactly this many entries:
//
//	4 + 3 + 3*len(ArtistIDs) + 3*hasAlbum + 3*len(Genres)
//
// DimTotal gets four rows (all-time, year, month, day); every other dimension gets
// three (all-time, year, month). Day granularity is reserved for DimTotal -- see
// AggKey.Validate.
//
// Order is deterministic and part of the contract: TOTAL, TRACK, ALBUM, ARTIST, GENRE;
// within a dimension all-time, year, month, then day. Artists keep their first-seen
// order (the first artist is the primary one); genres are sorted.
//
// Period keys are derived in cal's location while the timestamps stay UTC. See Calendar.
func AggregateDeltas(f PlayFacts, cal Calendar) []AggDelta {
	year := cal.Year(f.PlayedAt)
	month := cal.Month(f.PlayedAt)
	day := cal.Day(f.PlayedAt)

	// The fidelity split lives here and nowhere else. An estimated play contributes to
	// the totals but not to the exact subtotals, which guarantees
	// MsPlayedExact <= MsPlayed and makes EstimatedRatio correct by construction.
	exactMs, exactPlays := f.MsPlayed, int64(1)
	if f.Estimated {
		exactMs, exactPlays = 0, 0
	}

	n := 4 + 3 + 3*len(f.ArtistIDs) + 3*len(f.Genres)
	if f.AlbumID != "" {
		n += 3
	}
	out := make([]AggDelta, 0, n)

	add := func(dim Dim, entityID string, periods ...Period) {
		for _, p := range periods {
			out = append(out, AggDelta{
				Key:           AggKey{Dim: dim, Period: p, EntityID: entityID},
				Plays:         1,
				PlaysExact:    exactPlays,
				MsPlayed:      f.MsPlayed,
				MsPlayedExact: exactMs,
				FirstPlayedAt: f.PlayedAt,
				LastPlayedAt:  f.PlayedAt,
			})
		}
	}

	add(DimTotal, TotalEntityID, PeriodAll, year, month, day)
	add(DimTrack, f.TrackID, PeriodAll, year, month)
	if f.AlbumID != "" {
		add(DimAlbum, f.AlbumID, PeriodAll, year, month)
	}
	for _, id := range f.ArtistIDs {
		add(DimArtist, id, PeriodAll, year, month)
	}
	for _, g := range f.Genres {
		add(DimGenre, g, PeriodAll, year, month)
	}

	return out
}

// MergeDeltas combines deltas that share a key: counters sum, FirstPlayedAt takes the
// minimum and LastPlayedAt the maximum. Key order follows first appearance in ds, so
// the output is deterministic.
//
// Callers should merge before writing. Beyond cutting the number of UpdateItem calls
// for a batch of plays, it computes true minimum and maximum bounds in memory, which is
// the practical mitigation for DynamoDB having no MIN/MAX.
func MergeDeltas(ds []AggDelta) []AggDelta {
	if len(ds) <= 1 {
		return ds
	}
	idx := make(map[AggKey]int, len(ds))
	out := make([]AggDelta, 0, len(ds))
	for _, d := range ds {
		i, ok := idx[d.Key]
		if !ok {
			idx[d.Key] = len(out)
			out = append(out, d)
			continue
		}
		m := &out[i]
		m.Plays += d.Plays
		m.PlaysExact += d.PlaysExact
		m.MsPlayed += d.MsPlayed
		m.MsPlayedExact += d.MsPlayedExact
		if !d.FirstPlayedAt.IsZero() && (m.FirstPlayedAt.IsZero() || d.FirstPlayedAt.Before(m.FirstPlayedAt)) {
			m.FirstPlayedAt = d.FirstPlayedAt
		}
		if d.LastPlayedAt.After(m.LastPlayedAt) {
			m.LastPlayedAt = d.LastPlayedAt
		}
	}
	return out
}

// NameKeyPrefix marks an entity identified by name rather than by Spotify ID.
//
// It is deliberately not a valid Spotify ID (those are 22 base62 characters), so a name-keyed
// row can never be mistaken for a real one, and a query can tell at a glance which entities are
// still awaiting resolution.
const NameKeyPrefix = "nm:"

// NameKey derives a stable entity ID from a display name, or "" for an empty name.
//
// This exists because the GDPR export names artists and albums as free text and supplies no ID,
// while every aggregate is keyed by ID. Without it, seventeen years of history could produce
// track and total figures and nothing else.
//
// Normalisation folds case, strips diacritics and collapses whitespace, because the export and
// the API disagree about accents on the same artist -- the export writes "Heroes Del Silencio"
// where the API returns "Héroes del Silencio", and two keys for one artist would split its
// history exactly as a forked ID space would.
//
// The residual risk is real and accepted: two genuinely different artists sharing a name merge
// into one row. For a personal library that is rare, and far less wrong than attributing 85% of
// plays to nobody.
func NameKey(name string) string {
	n := foldName(name)
	if n == "" {
		return ""
	}
	return NameKeyPrefix + n
}

// IsNameKey reports whether an entity ID is name-derived rather than a Spotify ID.
func IsNameKey(id string) bool { return strings.HasPrefix(id, NameKeyPrefix) }

// foldName lowercases, folds Latin diacritics to their base letter and collapses whitespace.
//
// The fold table is explicit because internal/model is stdlib-only by design (see
// .golangci.yml): it is the shared vocabulary of every component, so it must not drag in a
// Unicode normalisation dependency. Runes outside the table pass through untouched, which is
// correct -- non-Latin scripts have no base letter to fold to.
//
// Measured against the real export's 3,751 distinct artist names, this merges exactly three
// pairs, and all three are one artist spelled two ways: Ayo/Ayọ, JAY-Z/JAŸ-Z and
// Nino Bravo/Niño Bravo. Folding is therefore not a compromise here, it is a repair.
func foldName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if f, ok := latinFolds[r]; ok {
			b.WriteString(f)
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// latinFolds maps lowercase accented Latin letters to their unaccented base. It covers
// Latin-1 Supplement, the common parts of Latin Extended-A, and the dot-below forms that
// appear in Yoruba names.
var latinFolds = map[rune]string{
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'ā': "a", 'ă': "a", 'ą': "a",
	'æ': "ae",
	'ç': "c", 'ć': "c", 'ĉ': "c", 'ċ': "c", 'č': "c",
	'ď': "d", 'đ': "d", 'ð': "d",
	'è': "e", 'é': "e", 'ê': "e", 'ë': "e", 'ē': "e", 'ĕ': "e", 'ė': "e", 'ę': "e", 'ě': "e",
	'ĝ': "g", 'ğ': "g", 'ġ': "g", 'ģ': "g",
	'ĥ': "h", 'ħ': "h",
	'ì': "i", 'í': "i", 'î': "i", 'ï': "i", 'ĩ': "i", 'ī': "i", 'ĭ': "i", 'į': "i", 'ı': "i",
	'ĵ': "j",
	'ķ': "k",
	'ĺ': "l", 'ļ': "l", 'ľ': "l", 'ł': "l",
	'ñ': "n", 'ń': "n", 'ņ': "n", 'ň': "n",
	'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o", 'ø': "o", 'ō': "o", 'ŏ': "o", 'ő': "o",
	'ọ': "o",
	'œ': "oe",
	'ŕ': "r", 'ŗ': "r", 'ř': "r",
	'ś': "s", 'ŝ': "s", 'ş': "s", 'š': "s", 'ș': "s",
	'ţ': "t", 'ť': "t", 'ŧ': "t", 'ț': "t",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u", 'ũ': "u", 'ū': "u", 'ŭ': "u", 'ů': "u", 'ű': "u",
	'ų': "u",
	'ŵ': "w",
	'ý': "y", 'ÿ': "y", 'ŷ': "y",
	'ź': "z", 'ż': "z", 'ž': "z",
	'ß': "ss",
	'þ': "th",
}

// FactsForTrack derives the aggregation inputs for a play, resolving artist and album identity
// LATE -- from the track's dimension row if it has been enriched, and from the export's names
// otherwise.
//
// Late binding is the whole point. Attribution is not baked into the play row at import time,
// so when enrichment eventually resolves a track, a reconcile picks up the real Spotify IDs and
// upgrades seventeen years of aggregates without reimporting four hundred thousand rows.
//
// track is the enriched dimension row, zero-valued when unknown.
func FactsForTrack(p Play, track Track, genres []string) PlayFacts {
	f := FactsFor(p, genres)

	// Prefer real Spotify IDs wherever they exist: the track row first, then whatever the play
	// itself was written with.
	if len(track.ArtistIDs) > 0 {
		f.ArtistIDs = dedupeStrings(track.ArtistIDs)
	}
	if track.AlbumID != "" {
		f.AlbumID = track.AlbumID
	}

	// Fall back to the export's names only where no ID is available at all.
	if len(f.ArtistIDs) == 0 {
		if k := NameKey(p.Export.ArtistName); k != "" {
			f.ArtistIDs = []string{k}
		}
	}
	if f.AlbumID == "" && p.Export.AlbumName != "" {
		// An album key folds in the artist: album titles repeat heavily across artists, and
		// "Greatest Hits" alone would merge dozens of unrelated records into one row.
		f.AlbumID = AlbumNameKey(p.Export.ArtistName, p.Export.AlbumName)
	}
	return f
}

// AlbumNameKey derives an album's fallback identity from its artist and title together.
//
// Exported so the reconcile can compute the same key for a REAL album row and thereby recognise
// that a name-keyed album and a resolved one are the same record. Without that, an artist whose
// tracks are only partly enriched appears twice: once under its Spotify ID and once under its
// name, splitting its history.
func AlbumNameKey(artistName, albumName string) string {
	if albumName == "" {
		return ""
	}
	return NameKey(artistName + " - " + albumName)
}
