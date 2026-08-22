package model

import (
	"errors"
	"testing"
	"time"
)

func testTrack() Track {
	return Track{
		ID:         "4uLU6hMCjMI75M1A2tKUQC",
		Name:       "Never Gonna Give You Up",
		DurationMs: 213_573,
		AlbumID:    "1DFixLWuPkv3KT3TnV35m3",
		ArtistIDs:  []string{"0gxyHStUsqpMadRV0Di1Qt"},
	}
}

// TestNewAPIPlayIsAlwaysEstimated is the type-level guard for the fidelity invariant:
// there is no way to construct an api-sourced play that claims exact duration.
func TestNewAPIPlayIsAlwaysEstimated(t *testing.T) {
	at := time.Date(2025, 3, 14, 21, 4, 33, 123_000_000, time.UTC)
	p, err := NewAPIPlay(at, testTrack())
	if err != nil {
		t.Fatalf("NewAPIPlay: %v", err)
	}
	if p.Source != SourceAPI {
		t.Errorf("Source = %q, want %q", p.Source, SourceAPI)
	}
	if !p.MsEstimated {
		t.Error("MsEstimated = false for an API play; the endpoint returns no duration")
	}
	// msPlayed falls back to the full track duration.
	if p.MsPlayed != testTrack().DurationMs {
		t.Errorf("MsPlayed = %d, want the track duration %d", p.MsPlayed, testTrack().DurationMs)
	}
	if !p.PlayedAt.Equal(at) {
		t.Errorf("PlayedAt = %v, want %v", p.PlayedAt, at)
	}
	if p.PlayedAt.Location() != time.UTC {
		t.Errorf("PlayedAt location = %v, want UTC", p.PlayedAt.Location())
	}
}

func TestNewAPIPlayNormalisesToUTC(t *testing.T) {
	madrid := MustCalendar("Europe/Madrid").Location()
	local := time.Date(2026, 1, 1, 0, 30, 0, 0, madrid)
	p, err := NewAPIPlay(local, testTrack())
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatTS(p.PlayedAt); got != "2025-12-31T23:30:00.000Z" {
		t.Errorf("PlayedAt = %s, want 2025-12-31T23:30:00.000Z", got)
	}
}

func TestNewExportPlayIsAlwaysExact(t *testing.T) {
	at := time.Date(2025, 3, 14, 21, 4, 33, 0, time.UTC)
	ext := ExportFields{Platform: "ios", Country: "ES", ReasonEnd: "trackdone", Shuffle: true}
	p, err := NewExportPlay(at, 187_433, testTrack(), ext)
	if err != nil {
		t.Fatalf("NewExportPlay: %v", err)
	}
	if p.Source != SourceExport {
		t.Errorf("Source = %q, want %q", p.Source, SourceExport)
	}
	if p.MsEstimated {
		t.Error("MsEstimated = true for an export play; the export carries exact ms_played")
	}
	if p.MsPlayed != 187_433 {
		t.Errorf("MsPlayed = %d, want the exact 187433", p.MsPlayed)
	}
	if p.Export != ext {
		t.Errorf("Export = %+v, want %+v", p.Export, ext)
	}
}

func TestNewPlayRejectsBadInput(t *testing.T) {
	at := time.Date(2025, 3, 14, 21, 4, 33, 0, time.UTC)

	t.Run("zero playedAt", func(t *testing.T) {
		if _, err := NewAPIPlay(time.Time{}, testTrack()); !errors.Is(err, ErrInvalidPlay) {
			t.Errorf("err = %v, want ErrInvalidPlay", err)
		}
	})
	t.Run("empty track ID", func(t *testing.T) {
		tr := testTrack()
		tr.ID = ""
		if _, err := NewAPIPlay(at, tr); !errors.Is(err, ErrInvalidPlay) {
			t.Errorf("err = %v, want ErrInvalidPlay", err)
		}
	})
	t.Run("zero duration is rejected not clamped", func(t *testing.T) {
		tr := testTrack()
		tr.DurationMs = 0
		if _, err := NewAPIPlay(at, tr); !errors.Is(err, ErrInvalidPlay) {
			t.Errorf("err = %v, want ErrInvalidPlay (a zero duration means a mapping bug)", err)
		}
	})
	t.Run("negative export duration", func(t *testing.T) {
		if _, err := NewExportPlay(at, -1, testTrack(), ExportFields{}); !errors.Is(err, ErrInvalidPlay) {
			t.Errorf("err = %v, want ErrInvalidPlay", err)
		}
	})
	t.Run("zero export duration", func(t *testing.T) {
		if _, err := NewExportPlay(at, 0, testTrack(), ExportFields{}); !errors.Is(err, ErrInvalidPlay) {
			t.Errorf("err = %v, want ErrInvalidPlay", err)
		}
	})
}

// TestValidateCatchesInconsistentSource covers rows read back out of storage, where the
// constructors were not involved and the two fields could disagree.
func TestValidateCatchesInconsistentSource(t *testing.T) {
	at := time.Date(2025, 3, 14, 21, 4, 33, 0, time.UTC)
	base := Play{PlayedAt: at, TrackID: "t1", MsPlayed: 1000}

	tests := []struct {
		name        string
		source      Source
		msEstimated bool
		wantErr     bool
	}{
		{"api + estimated is consistent", SourceAPI, true, false},
		{"export + exact is consistent", SourceExport, false, false},
		{"api + exact is impossible", SourceAPI, false, true},
		{"export + estimated is impossible", SourceExport, true, true},
		{"unknown source", Source("guess"), true, true},
		{"empty source", Source(""), false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			p.Source = tc.source
			p.MsEstimated = tc.msEstimated
			err := p.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidPlay) {
				t.Errorf("err %v does not wrap ErrInvalidPlay", err)
			}
		})
	}
}

func TestPlayDedupesArtistIDs(t *testing.T) {
	at := time.Date(2025, 3, 14, 21, 4, 33, 0, time.UTC)
	tr := testTrack()
	tr.ArtistIDs = []string{"a", "a", "b", "", "c", "b"}

	p, err := NewAPIPlay(at, tr)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c"}
	if len(p.ArtistIDs) != len(want) {
		t.Fatalf("ArtistIDs = %v, want %v", p.ArtistIDs, want)
	}
	for i := range want {
		if p.ArtistIDs[i] != want[i] {
			t.Errorf("ArtistIDs[%d] = %q, want %q (first-seen order must be preserved)",
				i, p.ArtistIDs[i], want[i])
		}
	}
}

func TestPlayEmptyArtistIDs(t *testing.T) {
	at := time.Date(2025, 3, 14, 21, 4, 33, 0, time.UTC)
	tr := testTrack()
	tr.ArtistIDs = []string{"", ""}
	p, err := NewAPIPlay(at, tr)
	if err != nil {
		t.Fatal(err)
	}
	if p.ArtistIDs != nil {
		t.Errorf("ArtistIDs = %v, want nil when every value is empty", p.ArtistIDs)
	}
}

func TestSourceValid(t *testing.T) {
	for _, tc := range []struct {
		s    Source
		want bool
	}{
		{SourceAPI, true}, {SourceExport, true}, {"", false}, {"API", false}, {"other", false},
	} {
		if got := tc.s.Valid(); got != tc.want {
			t.Errorf("Source(%q).Valid() = %v, want %v", tc.s, got, tc.want)
		}
	}
}
