// Package musicbrainztest provides a recording HTTP fake for the MusicBrainz client.
//
// It mirrors the pattern in the Spotify client's tests: a real httptest.Server, so header
// handling, query encoding and JSON decoding all go through net/http rather than a fake that
// might disagree with it.
package musicbrainztest

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Server records every request it received and serves a scripted handler.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*url.URL
	agents   []string
}

// New starts a server with the given handler.
func New(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *Server {
	t.Helper()
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		u := *r.URL
		s.requests = append(s.requests, &u)
		s.agents = append(s.agents, r.Header.Get("User-Agent"))
		s.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// URLs returns every request URL seen, in order.
func (s *Server) URLs() []*url.URL {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*url.URL(nil), s.requests...)
}

// UserAgents returns the User-Agent of every request. MusicBrainz throttles by agent, so a
// test that cares about etiquette needs to see them.
func (s *Server) UserAgents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.agents...)
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
//
// The goldens in this package were captured from real MusicBrainz responses rather than
// hand-written, so the shapes the client decodes are the shapes the service actually sends.
func Fixture(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}
