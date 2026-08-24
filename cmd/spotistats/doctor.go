package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
	"net/http"
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

	cfg := config.Load()
	deps, err := config.Build(runCtx, cfg, config.BuildOptions{
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

	// External enrichment is a separate concern with its own two upstreams, and both fail in
	// ways that are silent from the dashboard's point of view: no contact string means every
	// MusicBrainz request is throttled as an anonymous agent, and no key means profiles store
	// facts but never a biography or artwork.
	if err := reportIdentityResolution(runCtx, st); err != nil {
		return err
	}
	reportExternalReadiness(runCtx, cfg, deps)
	reportAlertingReadiness(runCtx, cfg)

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

// reportExternalReadiness checks the two external-enrichment prerequisites and their hosts.
func reportExternalReadiness(ctx context.Context, cfg config.Config, deps *config.Deps) {
	fmt.Println("\nExternal enrichment")

	if ua := cfg.MusicBrainzUserAgent(); ua == "" {
		fmt.Printf("    MusicBrainz contact: NOT SET (%s)\n", config.EnvMusicBrainzContact)
		fmt.Println("      Without it every request is throttled as an anonymous agent, which")
		fmt.Println("      MusicBrainz rate-limits far harder as a class.")
	} else {
		fmt.Printf("    MusicBrainz contact: %s\n", ua)
	}

	key, err := cfg.ResolveAudioDBKey(ctx)
	switch {
	case err != nil || key == "":
		fmt.Println("    TheAudioDB key:      NOT SET")
		fmt.Println("      Profiles will store facts but no biography and no artwork.")
	case key == theaudiodbTestKey:
		fmt.Println("    TheAudioDB key:      the PUBLIC TEST KEY (rate-limited hard)")
	default:
		fmt.Printf("    TheAudioDB key:      configured (%d chars)\n", len(key))
	}

	// Reachability, because a run that cannot reach either host fails the same way a
	// misconfigured one does and the two are worth telling apart.
	for name, url := range map[string]string{
		"musicbrainz.org": "https://musicbrainz.org/ws/2/artist/eace2373-31c8-4aba-9a5c-7bce22dd140a?fmt=json",
		"theaudiodb.com":  "https://www.theaudiodb.com/api/v1/json/" + firstNonEmpty(key, theaudiodbTestKey) + "/artist-mb.php?i=eace2373-31c8-4aba-9a5c-7bce22dd140a",
	} {
		fmt.Printf("    %-20s %s\n", name+":", probe(ctx, url))
	}
	_ = deps
}

// reportIdentityResolution reports how much listening time is attributed to NAME-KEYED
// entities rather than real Spotify IDs.
//
// This check exists because its absence hid a real defect for weeks. The imported history
// supplies no artist or album ID, so the importer writes placeholder track rows whose artistIds
// are name keys, and a later pass upgrades them from the API (docs/SPECS.md 4.2). That pass is
// slow and resumable, so a partly-finished state is normal -- but nothing reported it, and every
// surface that would have hinted at it looked healthy:
//
//   - `artistCoverage` read 1.0, because a name key IS attribution as far as it is concerned.
//   - Name resolution above read "named: 100", because a name-keyed artist has a perfectly good
//     display name -- it is the name that IS its identity.
//   - The leaderboards looked plausible, because the canonicaliser silently merges a name key
//     into a real ID whenever some other track supplies the mapping.
//
// The consequence is a SPLIT ranking: an artist with some tracks resolved and some not appears
// twice, and one of the two rows is invisible below the top N. That is exactly the failure the
// first backfill produced, and it is undetectable without this figure.
func reportIdentityResolution(ctx context.Context, st *store.Store) error {
	fmt.Println("\nIdentity resolution")

	for _, dim := range []model.Dim{model.DimArtist, model.DimAlbum} {
		var total, nameKeyed int64
		var rows, nameRows int
		type sample struct {
			id string
			ms int64
		}
		var unresolved []sample
		for agg, err := range st.QueryAggregates(ctx, dim, model.PeriodAll, "") {
			if err != nil {
				return fmt.Errorf("doctor: read %s aggregates: %w", dim, err)
			}
			rows++
			total += agg.MsPlayed
			if model.IsNameKey(agg.Key.EntityID) {
				nameRows++
				nameKeyed += agg.MsPlayed
				unresolved = append(unresolved, sample{agg.Key.EntityID, agg.MsPlayed})
			}
		}
		// The BIGGEST offenders, not the alphabetically first. Aggregates arrive in sort-key
		// order, so taking the first three listed "nm:!!!" and "nm:*nsync" at zero hours --
		// technically name-keyed, utterly uninformative. What an operator needs is which
		// artists are actually being misreported.
		slices.SortFunc(unresolved, func(a, b sample) int { return int(b.ms - a.ms) })
		worst := make([]string, 0, 3)
		for _, u := range unresolved[:min(3, len(unresolved))] {
			worst = append(worst, fmt.Sprintf("%s (%s)", u.id, formatHours(u.ms)))
		}
		if rows == 0 {
			fmt.Printf("    %-7s no aggregates -- run `spotistats rollup` first\n", dim)
			continue
		}
		pct := 0.0
		if total > 0 {
			pct = 100 * float64(nameKeyed) / float64(total)
		}
		fmt.Printf("    %-7s %d/%d rows are name-keyed, holding %.1f%% of attributed time\n",
			dim, nameRows, rows, pct)
		for _, w := range worst {
			fmt.Printf("              e.g. %s\n", w)
		}
	}
	fmt.Println("      A name-keyed row means its tracks have not been resolved to Spotify IDs,")
	fmt.Println("      so one artist can occupy two rows and neither shows its real total.")
	fmt.Println("      Backlog, spending no quota:  make resolve PROD=1 ARGS='-dry-run'")
	fmt.Println("      Resolve a batch (budgeted):  make resolve PROD=1")
	fmt.Println("      Then rewrite the aggregates: make rollup PROD=1 ARGS='-all'")
	return nil
}

// formatHours renders a duration the way the rest of the CLI does.
func formatHours(ms int64) string {
	return fmt.Sprintf("%.0f h", float64(ms)/float64(3_600_000))
}

// reportAlertingReadiness checks that alarms have somewhere to go.
//
// This is the check that would have caught the original defect: the SNS topic had no subscriber
// at all, because alarmEmail was unset and the subscription skipped itself in silence. Three
// alarms were firing into nothing and the console looked healthy.
//
// It reports the webhook's SHAPE, never its value: the URL is a bearer credential, and printing
// it here would put it in a terminal scrollback and any CI log that runs doctor.
func reportAlertingReadiness(ctx context.Context, cfg config.Config) {
	fmt.Println("\nAlerting")

	webhook, err := cfg.ResolveSlackWebhook(ctx)
	switch {
	case err != nil:
		fmt.Printf("    Slack webhook:       NOT SET (%s)\n", cfg.SlackWebhookParam())
		fmt.Println("      Every alarm will fail to deliver. Store one with:")
		fmt.Printf("        aws ssm put-parameter --name %s \\\n", cfg.SlackWebhookParam())
		fmt.Println("          --type SecureString --value 'https://hooks.slack.com/services/...'")
	case !strings.HasPrefix(webhook, "https://hooks.slack.com/"):
		// A webhook that is not a Slack webhook is almost always a copy-paste of the channel
		// URL or of the app's settings page, and it fails only when an alarm fires.
		fmt.Println("    Slack webhook:       SET, but it is not a hooks.slack.com URL")
		fmt.Println("      Incoming webhooks look like https://hooks.slack.com/services/T.../B.../...")
	default:
		fmt.Printf("    Slack webhook:       configured (%d chars)\n", len(webhook))
	}
	fmt.Println("      Verify delivery end to end with: make notify-test PROD=1")
}

// theaudiodbTestKey mirrors theaudiodb.TestAPIKey without importing the package for one string.
const theaudiodbTestKey = "123"

// probe reports whether a host answers, distinguishing "unreachable" from "throttling".
func probe(ctx context.Context, url string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "bad probe URL"
	}
	// MusicBrainz needs a real agent even to answer a probe.
	req.Header.Set("User-Agent", "spotistats-doctor/1.0 ( https://github.com/neovasili/spotistats )")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "unreachable: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == 200:
		return "ok"
	case resp.StatusCode == 503:
		// Normal for MusicBrainz roughly half the time, so it is not a failure.
		return "503 (backpressure, which the client retries)"
	default:
		return fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
