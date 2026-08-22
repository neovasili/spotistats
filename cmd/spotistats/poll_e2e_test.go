package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"testing"

	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
	"github.com/neovasili/spotistats/internal/store/storetest"
)

// fakeSpotifyServer stands in for accounts.spotify.com and api.spotify.com, so the CLI's
// real code path can be exercised without a browser, a live token, or network access.
type fakeSpotifyServer struct {
	*httptest.Server
	tokenCalls       int
	recentCalls      int
	artistCalls      int
	batchArtistCalls int
}

func newFakeSpotify(t *testing.T, plays []fakePlay) *fakeSpotifyServer {
	t.Helper()
	f := &fakeSpotifyServer{}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/token", func(w http.ResponseWriter, _ *http.Request) {
		f.tokenCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fake-access","token_type":"Bearer",` +
			`"expires_in":3600,"scope":"user-read-recently-played user-top-read"}`))
	})

	mux.HandleFunc("/v1/me/player/recently-played", func(w http.ResponseWriter, r *http.Request) {
		f.recentCalls++
		if got := r.Header.Get("Authorization"); got != "Bearer fake-access" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		items := make([]map[string]any, 0, len(plays))
		// Spotify returns newest first; the client normalises.
		for i := len(plays) - 1; i >= 0; i-- {
			items = append(items, plays[i].item())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":   items,
			"limit":   50,
			"cursors": map[string]string{"after": "1", "before": "2"},
		})
	})

	// Single-item route: Spotify removed GET /v1/artists (the batch multi-get) for
	// Development Mode apps in the February 2026 change, so the client addresses one artist
	// per request. The batch path is registered below and must never be called.
	mux.HandleFunc("/v1/artists/", func(w http.ResponseWriter, r *http.Request) {
		f.artistCalls++
		if r.URL.Query().Has("market") {
			http.Error(w, "market must never be sent: it triggers relinking", http.StatusBadRequest)
			return
		}
		id := path.Base(r.URL.Path)
		if id != "ar1" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "name": "Within Temptation",
			"genres": []string{"symphonic metal", "gothic metal"},
			// popularity is deprecated and followers is always null as of February 2026;
			// both are sent here only to prove the client tolerates them.
			"popularity": 62,
			"followers":  map[string]any{"total": 2500000},
			"images":     []map[string]any{{"url": "https://i.scdn.co/image/ar1", "height": 640, "width": 640}},
		})
	})

	// The removed batch endpoint. Spotify answers 403 here; failing loudly means a
	// reintroduced multi-get shows up as a test failure rather than as a silent production
	// outage where every artist renders as a raw ID.
	mux.HandleFunc("/v1/artists", func(w http.ResponseWriter, _ *http.Request) {
		f.batchArtistCalls++
		http.Error(w, "Forbidden", http.StatusForbidden)
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

type fakePlay struct {
	trackID  string
	playedAt string
	duration int64
}

func (p fakePlay) item() map[string]any {
	return map[string]any{
		"played_at": p.playedAt,
		"track": map[string]any{
			"id": p.trackID, "name": "Track " + p.trackID,
			"duration_ms": p.duration, "explicit": false, "popularity": 50,
			"uri":          "spotify:track:" + p.trackID,
			"external_ids": map[string]any{"isrc": "NLA320400123"},
			"album": map[string]any{
				"id": "al1", "name": "The Album",
				"release_date": "2014-10-24", "release_date_precision": "day",
				"total_tracks": 12,
				"images":       []map[string]any{{"url": "https://i.scdn.co/image/al1", "height": 640, "width": 640}},
				"artists":      []map[string]any{{"id": "ar1", "name": "Within Temptation"}},
			},
			"artists": []map[string]any{{"id": "ar1", "name": "Within Temptation"}},
		},
	}
}

// TestPollEndToEnd drives the real runPoll code path -- config resolution, token refresh,
// API client, capture pipeline, DynamoDB writes -- against a fake Spotify and DynamoDB
// Local. This is milestone 3's exit criterion, minus the browser step that only a human can
// perform.
func TestPollEndToEnd(t *testing.T) {
	ddb := storetest.RequireDynamoDB(t)
	table := storetest.CreateTable(t, ddb)

	fake := newFakeSpotify(t, []fakePlay{
		{trackID: "t1", playedAt: "2025-03-14T10:00:00.000Z", duration: 210_000},
		{trackID: "t2", playedAt: "2025-03-14T11:00:00.000Z", duration: 240_000},
	})

	tokenFile := filepath.Join(t.TempDir(), "refresh_token.json")
	if err := config.NewFileTokenStore(tokenFile).Put(context.Background(), "seed-refresh-token"); err != nil {
		t.Fatal(err)
	}

	t.Setenv(config.EnvTableName, table)
	t.Setenv(config.EnvDDBEndpoint, ddbEndpoint(t))
	t.Setenv(config.EnvTokenFile, tokenFile)
	t.Setenv(config.EnvClientID, "test-client-id")
	t.Setenv(config.EnvClientSecret, "test-client-secret")
	t.Setenv(config.EnvSpotifyBaseURL, fake.URL+"/v1")
	t.Setenv(config.EnvTokenURL, fake.URL+"/api/token")
	t.Setenv(config.EnvTimezone, storetest.DefaultTimezone)
	t.Setenv(config.EnvLogLevel, "error")

	// --- first run ingests everything ---
	if err := runPoll(context.Background(), nil); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if fake.tokenCalls != 1 {
		t.Errorf("token calls = %d, want 1", fake.tokenCalls)
	}
	if fake.batchArtistCalls != 0 {
		t.Errorf("batch GET /v1/artists called %d time(s); it is removed for dev-mode apps "+
			"and returns 403", fake.batchArtistCalls)
	}
	if fake.artistCalls != 1 {
		t.Errorf("artist calls = %d, want 1 (genres must be resolved before recording)", fake.artistCalls)
	}

	cfg := config.Load()
	deps, err := config.Build(context.Background(), cfg, config.BuildOptions{NeedStore: true})
	if err != nil {
		t.Fatal(err)
	}
	st := deps.Store
	ctx := context.Background()

	total, err := st.GetAggregate(ctx, model.AggKey{
		Dim: model.DimTotal, Period: model.PeriodAll, EntityID: model.TotalEntityID,
	})
	if err != nil {
		t.Fatalf("no total aggregate after poll: %v", err)
	}
	if total.Plays != 2 {
		t.Errorf("total plays = %d, want 2", total.Plays)
	}
	if want := int64(450_000); total.MsPlayed != want {
		t.Errorf("total msPlayed = %d, want %d (sum of track durations)", total.MsPlayed, want)
	}
	// API-sourced plays carry no real duration, so nothing is exact.
	if total.MsPlayedExact != 0 || total.PlaysExact != 0 {
		t.Errorf("exact counters = (%d, %d), want zero for api-sourced plays",
			total.PlaysExact, total.MsPlayedExact)
	}
	if r := total.EstimatedRatio(); r != 1 {
		t.Errorf("EstimatedRatio = %v, want 1", r)
	}

	// Genres resolved from the artist call, deduplicated, attributed to both plays.
	for _, g := range []string{"symphonic metal", "gothic metal"} {
		agg, err := st.GetAggregate(ctx, model.AggKey{
			Dim: model.DimGenre, Period: model.PeriodAll, EntityID: g,
		})
		if err != nil {
			t.Errorf("genre %q missing: %v", g, err)
			continue
		}
		if agg.Plays != 2 {
			t.Errorf("genre %q plays = %d, want 2", g, agg.Plays)
		}
	}

	// The canonical query.
	artist, err := st.GetAggregate(ctx, model.AggKey{
		Dim: model.DimArtist, Period: "2025", EntityID: "ar1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if artist.Plays != 2 {
		t.Errorf("artist 2025 plays = %d, want 2", artist.Plays)
	}

	// Metadata came from the payload, not a second API call.
	if tr, err := st.GetTrack(ctx, "t1"); err != nil || tr.Name != "Track t1" {
		t.Errorf("track metadata = %+v, %v", tr, err)
	}
	if al, err := st.GetAlbum(ctx, "al1"); err != nil || al.ReleaseDatePrecision != "day" {
		t.Errorf("album metadata = %+v, %v", al, err)
	}

	cursor, err := st.GetPollCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := model.FormatTS(cursor.LastPlayedAt); got != "2025-03-14T11:00:00.000Z" {
		t.Errorf("cursor = %s, want the newest play", got)
	}

	// --- second run: same window, nothing double-counted ---
	if err := runPoll(context.Background(), nil); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	after, err := st.GetAggregate(ctx, model.AggKey{
		Dim: model.DimTotal, Period: model.PeriodAll, EntityID: model.TotalEntityID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.Plays != 2 {
		t.Errorf("total plays after replay = %d, want 2 -- the run must be idempotent", after.Plays)
	}
	// A fresh artist must not be re-fetched.
	if fake.artistCalls != 1 {
		t.Errorf("artist calls = %d after two runs, want 1", fake.artistCalls)
	}
}

// A missing token must produce an actionable message, not a stack trace.
func TestPollWithoutStoredToken(t *testing.T) {
	ddb := storetest.RequireDynamoDB(t)
	table := storetest.CreateTable(t, ddb)

	t.Setenv(config.EnvTableName, table)
	t.Setenv(config.EnvDDBEndpoint, ddbEndpoint(t))
	t.Setenv(config.EnvTokenFile, filepath.Join(t.TempDir(), "absent.json"))
	t.Setenv(config.EnvClientID, "id")
	t.Setenv(config.EnvClientSecret, "secret")
	t.Setenv(config.EnvSpotifyBaseURL, "http://127.0.0.1:1/v1")
	t.Setenv(config.EnvTokenURL, "http://127.0.0.1:1/api/token")
	t.Setenv(config.EnvLogLevel, "error")

	err := runPoll(context.Background(), nil)
	if err == nil {
		t.Fatal("poll succeeded with no stored refresh token")
	}
	if !contains(err.Error(), "auth login") {
		t.Errorf("error = %v, want it to point at `auth login`", err)
	}
}

func TestPollDryRunWritesNothing(t *testing.T) {
	ddb := storetest.RequireDynamoDB(t)
	table := storetest.CreateTable(t, ddb)

	fake := newFakeSpotify(t, []fakePlay{
		{trackID: "t1", playedAt: "2025-03-14T10:00:00.000Z", duration: 210_000},
	})
	tokenFile := filepath.Join(t.TempDir(), "refresh_token.json")
	if err := config.NewFileTokenStore(tokenFile).Put(context.Background(), "seed"); err != nil {
		t.Fatal(err)
	}

	t.Setenv(config.EnvTableName, table)
	t.Setenv(config.EnvDDBEndpoint, ddbEndpoint(t))
	t.Setenv(config.EnvTokenFile, tokenFile)
	t.Setenv(config.EnvClientID, "id")
	t.Setenv(config.EnvClientSecret, "secret")
	t.Setenv(config.EnvSpotifyBaseURL, fake.URL+"/v1")
	t.Setenv(config.EnvTokenURL, fake.URL+"/api/token")
	t.Setenv(config.EnvLogLevel, "error")

	if err := runPoll(context.Background(), []string{"-dry-run"}); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	cfg := config.Load()
	deps, err := config.Build(context.Background(), cfg, config.BuildOptions{NeedStore: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Store.GetAggregate(context.Background(), model.AggKey{
		Dim: model.DimTotal, Period: model.PeriodAll, EntityID: model.TotalEntityID,
	}); err == nil {
		t.Error("dry run wrote an aggregate")
	} else if !isNotFound(err) {
		t.Errorf("unexpected error: %v", err)
	}
	cursor, err := deps.Store.GetPollCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.LastPlayedAt.IsZero() {
		t.Error("dry run advanced the cursor")
	}
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// ddbEndpoint returns the endpoint of the shared DynamoDB Local container, so the CLI can
// be pointed at the same instance the harness created.
func ddbEndpoint(t *testing.T) string {
	t.Helper()
	return storetest.Endpoint(t)
}
