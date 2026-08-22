package main

import (
	"context"
	"fmt"
	"time"

	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// runDoctor diagnoses why a leaderboard entry has no display name.
//
// It exists because the failure is silent by nature and spans two components: the rollup
// writes whatever names exist when it runs, and capture is what puts them there. A dashboard
// showing raw Spotify IDs therefore says nothing about which of three very different causes
// is responsible -- the row is absent, the row is a tombstone, or the row is present and the
// rollup simply predates it. Each has a different fix, so the command names which one it is
// rather than leaving it to be inferred from a scan.
func runDoctor(ctx context.Context, args []string) error {
	fs := newFlagSet("doctor", "doctor [flags]")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall deadline")
	if err := fs.Parse(args); err != nil {
		return err
	}

	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	deps, err := config.Build(runCtx, config.Load(), config.BuildOptions{
		NeedStore:         true,
		VerifyStoreConfig: true,
	})
	if err != nil {
		return err
	}
	st := deps.Store

	fmt.Println("Name resolution")
	anyProblem := false
	for _, dim := range []model.Dim{model.DimArtist, model.DimTrack, model.DimAlbum} {
		report, err := diagnoseDim(runCtx, st, dim)
		if err != nil {
			return err
		}
		fmt.Printf("\n  %s (top %d, all time)\n", dim, report.total)
		if report.total == 0 {
			fmt.Println("    no leaderboard -- run `spotistats rollup` first")
			continue
		}
		fmt.Printf("    named:      %d\n", report.named)
		fmt.Printf("    row absent: %d\n", report.absent)
		fmt.Printf("    tombstoned: %d\n", report.tombstoned)
		fmt.Printf("    row exists but unnamed: %d\n", report.unnamed)
		fmt.Printf("    stale in the snapshot: %d (row is named; the leaderboard is older)\n",
			report.staleBoard)
		for _, s := range report.samples {
			fmt.Printf("      e.g. %s -> %s\n", s.id, s.state)
		}
		if report.absent+report.tombstoned+report.unnamed+report.staleBoard > 0 {
			anyProblem = true
		}
	}

	if !anyProblem {
		fmt.Println("\nEvery leaderboard entry resolves to a name.")
		return nil
	}
	fmt.Print(`
What each state means

  stale in the snapshot  The dimension row HAS a name; the leaderboard was built before it
                         landed. Re-run ` + "`spotistats rollup`" + ` and republish.

  row absent             Capture never wrote the row. For artists this means the
                         GET /v1/artists call did not succeed, which also costs genre
                         attribution. Check ` + "`make logs-capture`" + ` for
                         "artist genre resolution failed" and re-run capture.

  tombstoned             Spotify returned null for the ID, so it was marked unresolvable.
                         Tombstones now expire, so a later capture will retry it.

  row exists but unnamed Spotify returned the object with an empty name -- unexpected;
                         worth reporting with the ID.
`)
	return nil
}

type nameSample struct{ id, state string }

type dimReport struct {
	total, named, absent, tombstoned, unnamed, staleBoard int
	samples                                               []nameSample
}

// diagnoseDim compares one all-time leaderboard against the dimension rows behind it.
func diagnoseDim(ctx context.Context, st *store.Store, dim model.Dim) (dimReport, error) {
	var r dimReport
	board, err := st.GetLeaderboard(ctx, dim, model.PeriodAll)
	if err != nil {
		return r, fmt.Errorf("read %s leaderboard: %w", dim, err)
	}
	r.total = len(board.Entries)
	if r.total == 0 {
		return r, nil
	}

	ids := make([]string, 0, r.total)
	for _, e := range board.Entries {
		ids = append(ids, e.ID)
	}
	rowName, err := lookupNames(ctx, st, dim, ids)
	if err != nil {
		return r, err
	}

	for _, e := range board.Entries {
		n, present := rowName[e.ID]
		// The rollup falls back to the ID when it finds no name, so an entry whose displayed
		// name IS its ID is exactly the unresolved case.
		boardResolved := e.Name != "" && e.Name != e.ID
		switch {
		case boardResolved:
			r.named++
			continue
		case !present:
			r.absent++
			r.note(&r.samples, e.ID, "no dimension row")
		case n == tombstone:
			r.tombstoned++
			r.note(&r.samples, e.ID, "tombstoned (Spotify returned null)")
		case n == "":
			r.unnamed++
			r.note(&r.samples, e.ID, "row present, name empty")
		default:
			r.staleBoard++
			r.note(&r.samples, e.ID, fmt.Sprintf("row says %q -- leaderboard is stale", n))
		}
	}
	return r, nil
}

// tombstone is an out-of-band marker; a real name can never equal it.
const tombstone = "\x00tombstone"

func (r *dimReport) note(dst *[]nameSample, id, state string) {
	if len(*dst) >= 3 {
		return
	}
	*dst = append(*dst, nameSample{id: id, state: state})
}

// lookupNames returns the stored name per ID, using the tombstone marker for rows flagged
// missing and omitting IDs with no row at all.
func lookupNames(
	ctx context.Context, st *store.Store, dim model.Dim, ids []string,
) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	switch dim {
	case model.DimArtist:
		rows, err := st.GetArtists(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("read artists: %w", err)
		}
		for id, a := range rows {
			out[id] = nameOrTombstone(a.Name, a.Missing)
		}
	case model.DimTrack:
		rows, err := st.GetTracks(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("read tracks: %w", err)
		}
		for id, t := range rows {
			out[id] = nameOrTombstone(t.Name, t.Missing)
		}
	case model.DimAlbum:
		rows, err := st.GetAlbums(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("read albums: %w", err)
		}
		for id, a := range rows {
			out[id] = nameOrTombstone(a.Name, a.Missing)
		}
	default:
		return nil, fmt.Errorf("doctor: dimension %s has no dimension rows", dim)
	}
	return out, nil
}

func nameOrTombstone(name string, missing bool) string {
	if missing {
		return tombstone
	}
	return name
}
