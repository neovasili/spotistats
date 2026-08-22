package rollup_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/neovasili/spotistats/internal/rollup"
)

// TestSnapshotsRenderWithoutSpotify is the regression test for a real production failure.
//
// The deployed dashboard served HTTP 403 because cmd/rollup resolved the Spotify credentials as a
// hard dependency before doing anything else. On a deployment where `spotistats auth login` had
// not yet been run those SSM parameters did not exist, so the run aborted before writing a single
// snapshot -- and with Origin Access Control a missing S3 key surfaces as 403, not 404.
//
// internal/rollup always treated Spotify as optional; the cmd layer defeated it. This asserts the
// package-level contract that the cmd layer must honour: no Spotify, full output anyway.
func TestSnapshotsRenderWithoutSpotify(t *testing.T) {
	st := seedCorpus(t)
	dir := t.TempDir()

	// Config with NO Spotify client at all, exactly as an unauthorised deployment produces.
	r := newRollup(t, st, func(c *rollup.Config) {
		c.Publisher = rollup.NewDirPublisher(dir)
		c.Spotify = nil
	})

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run failed without Spotify credentials; the reconcile and snapshots must not "+
			"depend on them: %v", err)
	}
	if !res.SkippedSpotify {
		t.Error("SkippedSpotify = false despite no Spotify client")
	}

	// Everything that does not need Spotify must still have happened.
	if res.SnapshotsWritten != 3 {
		t.Errorf("snapshots = %d, want all 3", res.SnapshotsWritten)
	}
	if res.LeaderboardsWritten == 0 {
		t.Error("no leaderboards written")
	}
	if res.HistogramsWritten == 0 {
		t.Error("no histograms written")
	}

	// And the dashboard the browser fetches must be present and complete.
	body, err := os.ReadFile(filepath.Join(dir, rollup.FileDashboard))
	if err != nil {
		t.Fatalf("dashboard.json absent — this is precisely the 403: %v", err)
	}
	var d rollup.Dashboard
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	if d.AllTime.Plays == 0 {
		t.Error("dashboard reports no plays")
	}
	if len(d.Top.Artists) == 0 {
		t.Error("dashboard has no top artists")
	}
	if d.Coverage.Approximate {
		t.Error("coverage should be exact after a full pass, with or without Spotify")
	}
}
