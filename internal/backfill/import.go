package backfill

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// batchSize is how many plays are buffered before a write. DynamoDB caps BatchWriteItem at 25,
// which store.PutPlaysBatch chunks internally; buffering more here just amortises the call.
const batchSize = 500

// ImportStats reports what an import pass did, including everything it declined to import.
//
// Skipped counts are reported per reason rather than as one total: "dropped 30,000 records" is
// alarming, whereas "29,000 below 30 seconds, 3 podcasts" is a description of the corpus.
type ImportStats struct {
	FilesRead    int
	RecordsRead  int
	PlaysWritten int
	Skipped      map[SkipReason]int

	// FirstPlayedAt and LastPlayedAt bound what was actually imported, which is what the
	// source-precedence check needs.
	FirstPlayedAt time.Time
	LastPlayedAt  time.Time

	// PlaceholdersWritten counts dimension rows created for entities the export names but
	// cannot identify.
	PlaceholdersWritten int

	// UnknownTracks are track IDs with no dimension row at import time. Their plays still
	// count towards TOTAL, but they will render as raw IDs until enrichment resolves them.
	UnknownTracks int
}

func (s ImportStats) TotalSkipped() int {
	n := 0
	for _, v := range s.Skipped {
		n += v
	}
	return n
}

// Importer writes export records as play rows.
type Importer struct {
	store *store.Store
	log   *slog.Logger
	minMs int64
	dry   bool
}

// ImportConfig configures an Importer.
type ImportConfig struct {
	Store *store.Store
	Log   *slog.Logger
	// MinMs is the shortest listening stretch that counts as a play. See Record.Filter for why
	// this is a duration threshold and not reason_end == "trackdone".
	MinMs int64
	// DryRun parses, filters and reports without writing anything.
	DryRun bool
}

func NewImporter(cfg ImportConfig) *Importer {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Importer{store: cfg.Store, log: log, minMs: cfg.MinMs, dry: cfg.DryRun}
}

// ScanTracks reads every file and returns the distinct track IDs that pass the filter.
func (im *Importer) ScanTracks(files []string) ([]string, ImportStats, error) {
	return Scan(files, im.minMs)
}

// Scan reads every file and returns the distinct track IDs that pass the filter, in first-seen
// order, together with a description of the corpus.
//
// It is a package function, not a method, because it touches no backend: a --dry-run must be
// able to describe the export with no table, no credentials and no network, since inspecting
// the corpus is exactly what you do BEFORE deciding to commit to an import.
//
// The set must be built with the SAME filter the import will use, or enrichment would resolve
// thousands of tracks whose only plays are then discarded.
func Scan(files []string, minMs int64) ([]string, ImportStats, error) {
	stats := ImportStats{Skipped: map[SkipReason]int{}}
	seen := map[string]bool{}
	var ids []string

	for _, f := range files {
		stats.FilesRead++
		err := ReadFile(f, func(r Record) error {
			stats.RecordsRead++
			if reason := r.Filter(minMs); reason != "" {
				stats.Skipped[reason]++
				return nil
			}
			id := r.TrackID()
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
			ts, _ := r.PlayedAt()
			if stats.FirstPlayedAt.IsZero() || ts.Before(stats.FirstPlayedAt) {
				stats.FirstPlayedAt = ts
			}
			if ts.After(stats.LastPlayedAt) {
				stats.LastPlayedAt = ts
			}
			return nil
		})
		if err != nil {
			return nil, stats, err
		}
	}
	return ids, stats, nil
}

// Import writes the plays. progress is called per file so a long run is visibly alive.
func (im *Importer) Import(
	ctx context.Context, files []string, progress func(file string, n, of int),
) (ImportStats, error) {
	stats := ImportStats{Skipped: map[SkipReason]int{}}

	// Track dimension rows are read once up front rather than per play: the same few thousand
	// tracks recur across four hundred thousand records.
	known, err := im.knownTracks(ctx, files)
	if err != nil {
		return stats, err
	}

	buf := make([]model.Play, 0, batchSize)
	flush := func() error {
		if len(buf) == 0 || im.dry {
			stats.PlaysWritten += len(buf)
			buf = buf[:0]
			return nil
		}
		n, err := im.store.PutPlaysBatch(ctx, buf)
		stats.PlaysWritten += n
		buf = buf[:0]
		return err
	}

	// Dimension rows for everything the export names but cannot identify. Without these the
	// name-keyed aggregates would have nowhere to read a display name from, and the dashboard
	// would show "nm:within temptation" instead of "Within Temptation".
	//
	// Written from the ORIGINAL casing the export supplied, not the folded key.
	placeholders := newPlaceholderWriter(im.store, im.dry)

	unknown := map[string]bool{}
	for i, f := range files {
		if progress != nil {
			progress(f, i+1, len(files))
		}
		err := ReadFile(f, func(r Record) error {
			stats.RecordsRead++
			if reason := r.Filter(im.minMs); reason != "" {
				stats.Skipped[reason]++
				return nil
			}
			id := r.TrackID()
			ts, err := r.PlayedAt()
			if err != nil {
				stats.Skipped[SkipBadTS]++
				return nil
			}

			// Attribution is deliberately NOT baked in here. The export gives no artist or
			// album ID, and resolving 13,000 tracks is a weeks-long job under a
			// development-mode quota, so identity is resolved LATE -- at reconcile time, from
			// the track's dimension row if it exists and from these names otherwise
			// (model.FactsForTrack). Writing it into the row instead would mean reimporting
			// 400,000 plays every time enrichment made progress.
			t := model.Track{ID: id}
			if kt, ok := known[id]; ok && kt.Name != "" {
				t = kt
			} else {
				unknown[id] = true
			}

			ext := r.ExportFields()
			ext.TrackName, ext.ArtistName, ext.AlbumName = r.TrackName, r.ArtistName, r.AlbumName
			p, err := model.NewExportPlay(ts, r.MsPlayed, t, ext)
			if err != nil {
				return fmt.Errorf("backfill: build play for %s at %s: %w", id, r.TS, err)
			}
			if err := placeholders.record(ctx, r, t); err != nil {
				return err
			}
			if stats.FirstPlayedAt.IsZero() || ts.Before(stats.FirstPlayedAt) {
				stats.FirstPlayedAt = ts
			}
			if ts.After(stats.LastPlayedAt) {
				stats.LastPlayedAt = ts
			}

			buf = append(buf, p)
			if len(buf) >= batchSize {
				return flush()
			}
			return nil
		})
		if err != nil {
			return stats, err
		}
		stats.FilesRead++
	}
	if err := flush(); err != nil {
		return stats, err
	}
	if err := placeholders.finish(ctx); err != nil {
		return stats, err
	}
	stats.UnknownTracks = len(unknown)
	stats.PlaceholdersWritten = placeholders.written
	return stats, nil
}

// placeholderWriter creates dimension rows for entities the export names but cannot identify.
//
// Deduplicated in-process and written once each: 400,000 plays reduce to a few thousand
// artists, albums and tracks. A resolved track is skipped entirely -- its real row already
// exists and must not be overwritten by a placeholder.
type placeholderWriter struct {
	store   *store.Store
	dry     bool
	seen    map[string]bool
	written int
}

func newPlaceholderWriter(st *store.Store, dry bool) *placeholderWriter {
	return &placeholderWriter{store: st, dry: dry, seen: map[string]bool{}}
}

func (w *placeholderWriter) record(ctx context.Context, r Record, resolved model.Track) error {
	// The artist row, keyed by name.
	if k := model.NameKey(r.ArtistName); k != "" && !w.seen[k] {
		w.seen[k] = true
		if err := w.put(ctx, func() error {
			return w.store.PutArtistName(ctx, k, r.ArtistName)
		}); err != nil {
			return fmt.Errorf("backfill: placeholder artist %q: %w", r.ArtistName, err)
		}
	}
	// The album row, keyed by artist and name together: album titles repeat heavily across
	// artists, and "Greatest Hits" alone would merge dozens of unrelated records.
	if ak := albumNameKey(r); ak != "" && !w.seen[ak] {
		w.seen[ak] = true
		if err := w.put(ctx, func() error {
			return w.store.PutAlbum(ctx, model.Album{
				ID: ak, Name: r.AlbumName,
				ArtistIDs: []string{model.NameKey(r.ArtistName)},
			})
		}); err != nil {
			return fmt.Errorf("backfill: placeholder album %q: %w", r.AlbumName, err)
		}
	}
	// A track row only when the real one is absent. This carries the export's track name so
	// the dashboard reads properly straight away, and name-keyed attribution so it is
	// recognisably unresolved -- see Enricher.unresolved.
	id := r.TrackID()
	if id != "" && resolved.Name == "" && !w.seen[id] {
		w.seen[id] = true
		if err := w.put(ctx, func() error {
			return w.store.PutTrack(ctx, model.Track{
				ID: id, Name: r.TrackName,
				AlbumID:   albumNameKey(r),
				ArtistIDs: nameKeys(r.ArtistName),
			})
		}); err != nil {
			return fmt.Errorf("backfill: placeholder track %q: %w", r.TrackName, err)
		}
	}
	return nil
}

func (w *placeholderWriter) put(_ context.Context, fn func() error) error {
	if w.dry {
		w.written++
		return nil
	}
	if err := fn(); err != nil {
		return err
	}
	w.written++
	return nil
}

func (w *placeholderWriter) finish(_ context.Context) error { return nil }

func albumNameKey(r Record) string {
	if r.AlbumName == "" {
		return ""
	}
	return model.NameKey(r.ArtistName + " - " + r.AlbumName)
}

func nameKeys(artistName string) []string {
	if k := model.NameKey(artistName); k != "" {
		return []string{k}
	}
	return nil
}

// knownTracks reads the dimension rows for every track the import will touch.
func (im *Importer) knownTracks(ctx context.Context, files []string) (map[string]model.Track, error) {
	ids, _, err := im.ScanTracks(files)
	if err != nil {
		return nil, err
	}
	out, err := im.store.GetTracks(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("backfill: read track rows: %w", err)
	}
	return out, nil
}

// SupersededAPIPlays lists API-sourced plays inside the export's coverage window.
//
// # Why the window and not the month
//
// docs/SPECS.md 4.2 originally made this month-granular: any month claimed by an export
// INGEST marker had its api rows deleted. That is destructive here. The export ends
// 2026-08-21 and capture began 2026-08-22, so the two share the month of August while
// overlapping on not a single play -- and a month rule would delete every captured play of
// the days after the export ends, which exist nowhere else.
//
// The window is therefore [first, last] of what was actually imported. Inside it the export is
// authoritative, because its ms_played is exact where the API's is the track's full duration.
// Outside it the API is the only source and must be left alone.
func SupersededAPIPlays(
	ctx context.Context, st *store.Store, from, to time.Time,
) ([]model.Play, error) {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return nil, nil
	}
	var out []model.Play
	for p, err := range st.Plays(ctx, from, to, store.PlayFilter{}) {
		if err != nil {
			return nil, fmt.Errorf("backfill: scan for superseded plays: %w", err)
		}
		if p.Source == model.SourceAPI {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PlayedAt.Before(out[j].PlayedAt) })
	return out, nil
}
