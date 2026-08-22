package ingest_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/spotify"
	"github.com/neovasili/spotistats/internal/store/storetest"
)

// fakeSpotify is a scripted stand-in for the parts of the client the pipeline uses.
type fakeSpotify struct {
	mu sync.Mutex

	pages    []spotify.RecentlyPlayedPage
	pageErrs []error
	pageIdx  int
	// requestedAfter records the After cursor of each call, so a test can prove the stored
	// cursor is actually being used.
	requestedAfter []time.Time

	artists        map[string]model.Artist
	artistsMissing []string
	artistsErr     error
	artistCalls    [][]string
}

func (f *fakeSpotify) RecentlyPlayed(_ context.Context, opt spotify.RecentlyPlayedOptions) (spotify.RecentlyPlayedPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestedAfter = append(f.requestedAfter, opt.After)

	i := f.pageIdx
	f.pageIdx++
	if i < len(f.pageErrs) && f.pageErrs[i] != nil {
		return spotify.RecentlyPlayedPage{}, f.pageErrs[i]
	}
	if i >= len(f.pages) {
		return spotify.RecentlyPlayedPage{}, errors.New("fakeSpotify: no page scripted")
	}
	return f.pages[i], nil
}

func (f *fakeSpotify) Artists(_ context.Context, ids []string) ([]model.Artist, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.artistCalls = append(f.artistCalls, append([]string(nil), ids...))
	if f.artistsErr != nil {
		return nil, nil, f.artistsErr
	}
	// Anything absent from the catalogue is reported missing, mirroring the positional
	// nulls Spotify returns for IDs it cannot resolve.
	var out []model.Artist
	var missing []string
	for _, id := range ids {
		if a, ok := f.artists[id]; ok {
			out = append(out, a)
			continue
		}
		missing = append(missing, id)
	}
	missing = append(missing, f.artistsMissing...)
	return out, missing, nil
}

func (f *fakeSpotify) ArtistCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.artistCalls...)
}

func (f *fakeSpotify) RequestedAfter() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.requestedAfter...)
}

// pageOf assembles a RecentlyPlayedPage the way the real client would: plays sorted oldest
// first, with the embedded track and album maps populated.
func pageOf(t *testing.T, limit int, plays ...model.Play) spotify.RecentlyPlayedPage {
	t.Helper()
	page := spotify.RecentlyPlayedPage{
		Limit:     limit,
		Tracks:    map[string]model.Track{},
		Albums:    map[string]model.Album{},
		Saturated: len(plays) >= limit,
	}
	for _, p := range plays {
		page.Plays = append(page.Plays, p)
		page.Tracks[p.TrackID] = model.Track{
			ID: p.TrackID, Name: "Track " + p.TrackID, DurationMs: p.MsPlayed,
			AlbumID: p.AlbumID, ArtistIDs: p.ArtistIDs,
		}
		if p.AlbumID != "" {
			page.Albums[p.AlbumID] = model.Album{
				ID: p.AlbumID, Name: "Album " + p.AlbumID,
				ReleaseDate: "2014-10-24", ReleaseDatePrecision: "day",
			}
		}
	}
	if n := len(page.Plays); n > 0 {
		page.OldestPlayedAt = page.Plays[0].PlayedAt
		page.NewestPlayedAt = page.Plays[n-1].PlayedAt
		page.NextAfter = page.NewestPlayedAt
		page.NextBefore = page.OldestPlayedAt
	}
	return page
}

// artistsWithGenres builds the fake's artist catalogue from the shared corpus mapping.
func artistsWithGenres() map[string]model.Artist {
	out := map[string]model.Artist{}
	for id, genres := range storetest.Genres() {
		out[id] = model.Artist{ID: id, Name: "Artist " + id, Genres: genres}
	}
	return out
}
