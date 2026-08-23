// Package theaudiodbtest provides a recording HTTP fake for TheAudioDB client.
package theaudiodbtest

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Server records every request URL and serves a scripted handler.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*url.URL
}

// New starts a server with the given handler.
func New(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *Server {
	t.Helper()
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		u := *r.URL
		s.requests = append(s.requests, &u)
		s.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// URLs returns every request URL seen, in order. The API key is a PATH segment, so these are
// also what a test checks to prove the key is placed correctly and never in a query.
func (s *Server) URLs() []*url.URL {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*url.URL(nil), s.requests...)
}

// Calls returns how many requests were made.
func (s *Server) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// ServeJSON returns a handler that writes a fixed body.
func ServeJSON(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// Fixture reads a golden file from testdata.
func Fixture(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}
