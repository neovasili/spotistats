package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/neovasili/spotistats/internal/api"
	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
	"github.com/neovasili/spotistats/internal/store/storetest"
)

// newAPI returns a handler over a table seeded with the shared corpus, so every endpoint test
// asserts against the same known dataset.
func newAPI(t *testing.T) *api.Handler {
	t.Helper()
	st := storetest.NewStore(t)
	ctx := context.Background()

	for _, p := range storetest.Corpus(t) {
		if _, err := st.RecordPlay(ctx, p, storetest.GenresFor(p)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Dimension rows so display names resolve.
	for id, genres := range storetest.Genres() {
		if err := st.PutArtist(ctx, model.Artist{ID: id, Name: "Artist " + id, Genres: genres}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"t1", "t2", "t3", "t4", "t5"} {
		if err := st.PutTrack(ctx, model.Track{ID: id, Name: "Track " + id, DurationMs: 200_000}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutAlbum(ctx, model.Album{ID: "al1", Name: "The Album"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPollCursor(ctx, model.PollCursor{
		LastPlayedAt: mustTS(t, "2026-02-10T13:00:00.000Z"),
		LastRunAt:    storetest.FixedNow,
		LastStatus:   "ok",
	}); err != nil {
		t.Fatal(err)
	}

	return newAPIOver(t, st)
}

func newAPIOver(t *testing.T, st *store.Store) *api.Handler {
	t.Helper()
	h, err := api.New(api.Config{
		Store:    st,
		Calendar: model.MustCalendar(storetest.DefaultTimezone),
		Now:      func() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func mustTS(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := model.ParseTS(s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// get issues a request and returns the recorder.
func get(t *testing.T, h *api.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, api.BasePath+path, nil))
	return rec
}

// getOK requires a 200 and decodes the body into v.
func getOK(t *testing.T, h *api.Handler, path string, v any) {
	t.Helper()
	rec := get(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200\nbody: %s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v\nbody: %s", path, err, rec.Body.String())
	}
}

// getErr requires a 4xx and returns the decoded error envelope.
func getErr(t *testing.T, h *api.Handler, path string) (int, string, string) {
	t.Helper()
	rec := get(t, h, path)
	if rec.Code < 400 {
		t.Fatalf("GET %s = %d, want a 4xx\nbody: %s", path, rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body is not the documented envelope: %v\nbody: %s", err, rec.Body.String())
	}
	if env.Error.Code == "" {
		t.Errorf("error envelope has no code: %s", rec.Body.String())
	}
	if env.Error.Message == "" {
		t.Errorf("error envelope has no message: %s", rec.Body.String())
	}
	return rec.Code, env.Error.Code, env.Error.Message
}

func httptestRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func newRequest(method, target string) *http.Request {
	return httptest.NewRequest(method, target, nil)
}
