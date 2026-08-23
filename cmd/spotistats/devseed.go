package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/model"
)

// runDevSeed writes a synthetic dataset to a local table.
//
// It exists so the frontend can be developed against realistic-looking data before the GDPR
// export arrives and before enough real listening has been captured. The shape matters more
// than the values: a plausible long tail, multi-artist tracks, artists with no genres, tracks
// with no album, and a diurnal play-time distribution, so charts exercise the same edge cases
// production will.
//
// Deterministic by default, so a screenshot is reproducible and a UI regression is visible.
func runDevSeed(ctx context.Context, args []string) error {
	fs := newFlagSet("dev-seed", "dev-seed [flags]")
	months := fs.Int("months", 14, "how many months of history to generate")
	plays := fs.Int("plays", 900, "roughly how many plays to generate")
	seed := fs.Uint64("seed", 20260322, "PRNG seed; the same seed gives the same dataset")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		return err
	}
	// Never against a real account: this writes fabricated listening history, which would be
	// indistinguishable from real data once mixed in.
	if cfg.DDBEndpoint == "" {
		return fmt.Errorf("dev-seed requires %s to be set; it writes synthetic data and must "+
			"never touch a real table", config.EnvDDBEndpoint)
	}

	deps, err := config.Build(ctx, cfg, config.BuildOptions{
		NeedStore: true, VerifyStoreConfig: true,
	})
	if err != nil {
		return err
	}
	cal, err := cfg.Calendar()
	if err != nil {
		return err
	}

	cat := buildCatalogue()
	rng := rand.New(rand.NewPCG(*seed, 0x5eed))

	heading("Seeding synthetic data")
	bullet("table:   %s at %s", cfg.TableName, cfg.DDBEndpoint)
	bullet("artists: %d, tracks: %d", len(cat.artists), len(cat.tracks))

	for _, a := range cat.artists {
		if err := deps.Store.PutArtist(ctx, a); err != nil {
			return err
		}
	}
	for _, al := range cat.albums {
		if err := deps.Store.PutAlbum(ctx, al); err != nil {
			return err
		}
	}
	for _, t := range cat.tracks {
		if err := deps.Store.PutTrack(ctx, t); err != nil {
			return err
		}
	}

	// Walk backwards from today so the newest data is "now" and the dashboard looks current.
	end := time.Now().UTC().Truncate(time.Hour)
	start := end.AddDate(0, -*months, 0)
	span := end.Sub(start)

	genresOf := cat.genresByArtist()
	inserted, dup := 0, 0
	var newest time.Time

	for i := 0; i < *plays; i++ {
		// Weight recent months more heavily: listening history is rarely uniform, and a
		// uniform distribution makes the activity chart look synthetic.
		frac := rng.Float64()
		frac = frac * frac
		at := end.Add(-time.Duration(frac * float64(span)))

		// A diurnal shape: most listening in waking hours, a little at night.
		at = at.Truncate(time.Hour).
			Add(time.Duration(diurnalHour(rng)) * time.Hour).
			Add(time.Duration(rng.IntN(60)) * time.Minute).
			Add(time.Duration(rng.IntN(60)) * time.Second)
		if at.After(end) {
			at = at.Add(-24 * time.Hour)
		}

		// Zipf-ish track choice so a long tail exists rather than a flat distribution.
		idx := int(float64(len(cat.tracks)) * rng.Float64() * rng.Float64())
		if idx >= len(cat.tracks) {
			idx = len(cat.tracks) - 1
		}
		tr := cat.tracks[idx]

		// Two thirds export-sourced (exact durations) so estimatedRatio is a realistic
		// fraction rather than 0 or 1.
		var p model.Play
		var perr error
		if rng.IntN(3) > 0 {
			ms := int64(float64(tr.DurationMs) * (0.55 + rng.Float64()*0.45))
			p, perr = model.NewExportPlay(at, ms, tr, model.ExportFields{
				Platform: []string{"ios", "android", "osx", "web"}[rng.IntN(4)],
				Country:  "ES", ReasonEnd: "trackdone",
			})
		} else {
			p, perr = model.NewAPIPlay(at, tr)
		}
		if perr != nil {
			return perr
		}

		var g []string
		for _, id := range tr.ArtistIDs {
			g = append(g, genresOf[id]...)
		}

		res, rerr := deps.Store.RecordPlay(ctx, p, g)
		if rerr != nil {
			return fmt.Errorf("record play: %w", rerr)
		}
		if res.Inserted {
			inserted++
			if at.After(newest) {
				newest = at
			}
		} else {
			dup++
		}
	}

	if err := deps.Store.PutPollCursor(ctx, model.PollCursor{
		LastPlayedAt: newest,
		LastRunAt:    time.Now().UTC(),
		LastStatus:   "ok",
	}); err != nil {
		return err
	}

	bullet("inserted %d plays (%d collided on the same second and were skipped)", inserted, dup)
	bullet("range:   %s to %s (local %s)",
		start.Format("2006-01-02"), end.Format("2006-01-02"), cal.Name())
	fmt.Printf("\nNext:  %s serve   then   cd web && VITE_API_TARGET=http://%s npm run dev\n",
		progName, DefaultServeAddr)
	return nil
}

// diurnalHour returns an hour of day weighted towards waking hours.
func diurnalHour(rng *rand.Rand) int {
	// Rough weights per hour, 00..23.
	weights := [24]int{2, 1, 1, 1, 1, 1, 2, 4, 7, 8, 8, 7, 6, 6, 7, 8, 9, 10, 9, 8, 7, 6, 5, 3}
	total := 0
	for _, w := range weights {
		total += w
	}
	n := rng.IntN(total)
	for h, w := range weights {
		if n < w {
			return h
		}
		n -= w
	}
	return 12
}

type catalogue struct {
	artists []model.Artist
	albums  []model.Album
	tracks  []model.Track
}

func (c catalogue) genresByArtist() map[string][]string {
	out := make(map[string][]string, len(c.artists))
	for _, a := range c.artists {
		out[a.ID] = a.Genres
	}
	return out
}

// buildCatalogue produces a fixed catalogue with deliberate awkwardness: shared genres, an
// artist with none, a track with no album, and multi-artist tracks. Those are the cases that
// break naive frontend code.
func buildCatalogue() catalogue {
	type artistSpec struct {
		id, name string
		genres   []string
	}
	specs := []artistSpec{
		{"ar-wt", "Within Temptation", []string{"symphonic metal", "gothic metal", "dutch metal"}},
		{"ar-nw", "Nightwish", []string{"symphonic metal", "power metal"}},
		{"ar-ep", "Epica", []string{"symphonic metal", "gothic metal"}},
		{"ar-ld", "Lacuna Coil", []string{"gothic metal"}},
		{"ar-op", "Opeth", []string{"progressive metal", "death metal"}},
		{"ar-tp", "Tool", []string{"progressive metal"}},
		{"ar-pj", "Portishead", []string{"trip hop"}},
		{"ar-bb", "Bonobo", []string{"downtempo", "trip hop"}},
		// No genres at all: the common case Spotify actually returns, and the one that makes
		// genre totals fall short of the overall total.
		{"ar-unk", "Unsigned Local Band", nil},
		{"ar-sol", "Session Soloist", nil},
	}

	var c catalogue
	for _, s := range specs {
		c.artists = append(c.artists, model.Artist{
			ID: s.id, Name: s.name, Genres: s.genres,
			Popularity: 30 + len(s.name), Followers: int64(1000 * (len(s.name) + 3)),
			ImageURL: "https://i.scdn.co/image/" + s.id,
		})
	}

	albumNames := []string{
		"The Silent Force", "Century Child", "Design Your Universe", "Comalies",
		"Blackwater Park", "Lateralus", "Dummy", "Black Sands", "Demo Tape",
	}
	for i, n := range albumNames {
		c.albums = append(c.albums, model.Album{
			ID: fmt.Sprintf("al-%02d", i), Name: n,
			ReleaseDate:          fmt.Sprintf("%d-%02d-%02d", 1994+i*3, 1+i%12, 1+i%28),
			ReleaseDatePrecision: "day",
			TotalTracks:          9 + i,
			ImageURL:             fmt.Sprintf("https://i.scdn.co/image/al-%02d", i),
			// Albums must carry their artist, or the seeded data silently fails to exercise
			// the album -> artist label lookup and a dashboard bug hides behind green tests.
			// Real albums get this from the simplified album object embedded in every play.
			ArtistIDs: []string{c.artists[i%len(c.artists)].ID},
		})
	}
	// A year-only release date, to exercise the precision field. Deliberately left with no
	// artist, so the "album whose artist row is missing" path is covered too.
	c.albums = append(c.albums, model.Album{
		ID: "al-year", Name: "Early Recordings", ReleaseDate: "1998",
		ReleaseDatePrecision: "year", TotalTracks: 6,
	})

	trackNames := []string{
		"Stand My Ground", "Angels", "Memories", "Ice Queen", "Mother Earth",
		"Nemo", "Wish I Had an Angel", "Ever Dream", "Cry for the Moon",
		"Unleashed", "Heaven's a Lie", "Swamped", "Blackwater Park", "Harvest",
		"Schism", "Parabola", "The Grudge", "Glory Box", "Roads", "Sour Times",
		"Kiara", "Kong", "Cirrus", "First Fires", "Untitled Demo",
		"Long Jam", "Sketch No. 3", "Interlude", "Reprise", "Hidden Track",
	}
	for i, n := range trackNames {
		primary := c.artists[i%len(c.artists)].ID
		artists := []string{primary}
		// Every fifth track is a collaboration, so artist totals exceed the overall total.
		if i%5 == 4 {
			artists = append(artists, c.artists[(i+3)%len(c.artists)].ID)
		}
		album := c.albums[i%len(c.albums)].ID
		// Two tracks have no album, so album totals fall short of the overall total.
		if i == 24 || i == 29 {
			album = ""
		}
		c.tracks = append(c.tracks, model.Track{
			ID: fmt.Sprintf("tr-%02d", i), Name: n,
			DurationMs: int64(150_000 + (i*37_000)%280_000),
			AlbumID:    album, ArtistIDs: artists,
			Popularity: 20 + (i*7)%70,
			ISRC:       fmt.Sprintf("NLA32%07d", i),
			URI:        fmt.Sprintf("spotify:track:tr-%02d", i),
		})
	}
	return c
}
