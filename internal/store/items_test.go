package store

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/go-cmp/cmp"
	"github.com/neovasili/spotistats/internal/model"
)

// marshalRoundTrip pushes an item through the real attributevalue marshaller, which is
// where a mistyped dynamodbav tag would show up.
func marshalRoundTrip[T any](t *testing.T, in T) (map[string]types.AttributeValue, T) {
	t.Helper()
	av, err := attributevalue.MarshalMap(in)
	if err != nil {
		t.Fatalf("MarshalMap: %v", err)
	}
	var out T
	if err := attributevalue.UnmarshalMap(av, &out); err != nil {
		t.Fatalf("UnmarshalMap: %v", err)
	}
	return av, out
}

func requireAttrs(t *testing.T, av map[string]types.AttributeValue, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, ok := av[n]; !ok {
			t.Errorf("marshalled item is missing attribute %q", n)
		}
	}
}

func TestPlayItemRoundTrip(t *testing.T) {
	at := mustTS(t, "2025-03-14T21:04:33.123Z")
	tr := model.Track{
		ID: "t1", DurationMs: 231000, AlbumID: "al1", ArtistIDs: []string{"ar1", "ar2"},
	}

	t.Run("api play", func(t *testing.T) {
		p, err := model.NewAPIPlay(at, tr)
		if err != nil {
			t.Fatal(err)
		}
		item := newPlayItem(p)

		if item.PK != "PLAY#2025-03" {
			t.Errorf("PK = %q", item.PK)
		}
		if item.SK != "2025-03-14T21:04:33.123Z#t1" {
			t.Errorf("SK = %q", item.SK)
		}
		if item.GSI1PK != "TRACK#t1" || item.GSI1SK != "2025-03-14T21:04:33.123Z" {
			t.Errorf("GSI1 keys = (%q, %q)", item.GSI1PK, item.GSI1SK)
		}
		if item.Source != "api" || !item.MsEstimated {
			t.Errorf("fidelity = (%q, %v), want (api, true)", item.Source, item.MsEstimated)
		}

		av, _ := marshalRoundTrip(t, item)
		requireAttrs(t, av, "PK", "SK", "GSI1PK", "GSI1SK", "type", "trackId", "msPlayed", "source", "msEstimated")

		back, err := item.toModel()
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(p, back); diff != "" {
			t.Errorf("play round trip (-want +got):\n%s", diff)
		}
	})

	t.Run("export play keeps its extra fields", func(t *testing.T) {
		ext := model.ExportFields{
			Platform: "ios", Country: "ES", ReasonStart: "clickrow",
			ReasonEnd: "trackdone", Shuffle: true, Skipped: false, Offline: true,
		}
		p, err := model.NewExportPlay(at, 187433, tr, ext)
		if err != nil {
			t.Fatal(err)
		}
		item := newPlayItem(p)
		if item.Source != "export" || item.MsEstimated {
			t.Errorf("fidelity = (%q, %v), want (export, false)", item.Source, item.MsEstimated)
		}
		_, decoded := marshalRoundTrip(t, item)
		back, err := decoded.toModel()
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(p, back); diff != "" {
			t.Errorf("play round trip (-want +got):\n%s", diff)
		}
	})
}

// A GSI1 query projects only the attributes in GSI1ProjectedAttributes plus the keys, so
// toModel must still reconstruct a usable play from that subset.
func TestPlayItemFromGSI1Projection(t *testing.T) {
	at := mustTS(t, "2025-03-14T21:04:33.123Z")
	projected := playItem{
		GSI1PK:      "TRACK#t1",
		GSI1SK:      "2025-03-14T21:04:33.123Z",
		TrackID:     "t1",
		MsPlayed:    231000,
		Source:      "api",
		MsEstimated: true,
		// albumId and artistIds are NOT projected, so they arrive empty.
	}
	got, err := projected.toModel()
	if err != nil {
		t.Fatal(err)
	}
	if !got.PlayedAt.Equal(at) {
		t.Errorf("PlayedAt = %v, want %v", got.PlayedAt, at)
	}
	if got.AlbumID != "" || got.ArtistIDs != nil {
		t.Errorf("unprojected attributes came back non-zero: album=%q artists=%v",
			got.AlbumID, got.ArtistIDs)
	}
}

func TestPlayItemToModelRequiresATimestamp(t *testing.T) {
	if _, err := (playItem{TrackID: "t1"}).toModel(); err == nil {
		t.Error("toModel accepted an item with neither SK nor GSI1SK")
	}
	if _, err := (playItem{SK: "garbage", TrackID: "t1"}).toModel(); err == nil {
		t.Error("toModel accepted a malformed SK")
	}
}

func TestAggregateItemRoundTrip(t *testing.T) {
	first := mustTS(t, "2025-03-01T10:00:00.000Z")
	last := mustTS(t, "2025-03-14T21:04:33.123Z")

	tests := []struct {
		name string
		agg  model.Aggregate
	}{
		{
			name: "artist year",
			agg: model.Aggregate{
				Key:           model.AggKey{Dim: model.DimArtist, Period: "2025", EntityID: "ar1"},
				Plays:         42,
				PlaysExact:    30,
				MsPlayed:      8_420_000,
				MsPlayedExact: 6_110_000,
				FirstPlayedAt: first,
				LastPlayedAt:  last,
			},
		},
		{
			// The folded key layout must survive a round trip.
			name: "total day row",
			agg: model.Aggregate{
				Key:           model.AggKey{Dim: model.DimTotal, Period: "2025-03-14", EntityID: model.TotalEntityID},
				Plays:         7,
				PlaysExact:    7,
				MsPlayed:      1_000_000,
				MsPlayedExact: 1_000_000,
				FirstPlayedAt: last,
				LastPlayedAt:  last,
			},
		},
		{
			name: "genre all time with no bounds set",
			agg: model.Aggregate{
				Key:      model.AggKey{Dim: model.DimGenre, Period: model.PeriodAll, EntityID: "gothic metal"},
				Plays:    1,
				MsPlayed: 100,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			item := newAggregateItem(tc.agg)
			if item.PK != tc.agg.Key.PK() || item.SK != tc.agg.Key.SK() {
				t.Errorf("keys = (%q, %q), want (%q, %q)", item.PK, item.SK,
					tc.agg.Key.PK(), tc.agg.Key.SK())
			}
			av, decoded := marshalRoundTrip(t, item)
			requireAttrs(t, av, "PK", "SK", "dim", "period", "entityId",
				"plays", "playsExact", "msPlayed", "msPlayedExact")

			back, err := decoded.toModel()
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.agg, back); diff != "" {
				t.Errorf("aggregate round trip (-want +got):\n%s", diff)
			}
		})
	}
}

// The denormalised dim/period/entityId attributes must agree with the keys, or a
// reconciliation scan that groups by them would disagree with one that parses keys.
func TestAggregateItemDenormalisedFieldsMatchKeys(t *testing.T) {
	for _, key := range []model.AggKey{
		{Dim: model.DimTotal, Period: model.PeriodAll, EntityID: model.TotalEntityID},
		{Dim: model.DimTotal, Period: "2025-03-14", EntityID: model.TotalEntityID},
		{Dim: model.DimTrack, Period: "2025-03", EntityID: "t1"},
		{Dim: model.DimGenre, Period: "2025", EntityID: "symphonic metal"},
	} {
		item := newAggregateItem(model.Aggregate{Key: key})
		parsed, err := model.ParseAggKey(item.PK, item.SK)
		if err != nil {
			t.Fatalf("ParseAggKey(%q,%q): %v", item.PK, item.SK, err)
		}
		if string(parsed.Dim) != item.Dim {
			t.Errorf("%s: dim attribute %q disagrees with the key %q", key, item.Dim, parsed.Dim)
		}
		if string(parsed.Period) != item.Period {
			t.Errorf("%s: period attribute %q disagrees with the key %q", key, item.Period, parsed.Period)
		}
		if parsed.EntityID != item.EntityID {
			t.Errorf("%s: entityId attribute %q disagrees with the key %q", key, item.EntityID, parsed.EntityID)
		}
	}
}

func TestDimensionItemRoundTrips(t *testing.T) {
	now := mustTS(t, "2025-03-14T21:04:33.123Z")

	t.Run("track", func(t *testing.T) {
		want := model.Track{
			ID: "t1", Name: "Ice Queen", DurationMs: 314000, AlbumID: "al1",
			ArtistIDs: []string{"ar1"}, Popularity: 62, Explicit: false,
			ISRC: "NLA320400123", URI: "spotify:track:t1", RefreshedAt: now,
		}
		item := newTrackItem(want, now)
		if item.PK != "TRACK#t1" || item.SK != SKMeta {
			t.Errorf("keys = (%q, %q)", item.PK, item.SK)
		}
		_, decoded := marshalRoundTrip(t, item)
		got, err := decoded.toModel()
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
	})

	t.Run("artist keeps genres verbatim", func(t *testing.T) {
		// Normalisation happens at aggregation time, so Spotify's own casing is preserved
		// here for display.
		want := model.Artist{
			ID: "ar1", Name: "Within Temptation",
			Genres:     []string{"symphonic metal", "gothic metal"},
			Popularity: 62, Followers: 2_500_000,
			ImageURL: "https://i.scdn.co/image/ar1", RefreshedAt: now,
			// newArtistItem stamps enrichedAt: it is only ever given a full artist object.
			EnrichedAt: now,
		}
		_, decoded := marshalRoundTrip(t, newArtistItem(want, now))
		got, err := decoded.toModel()
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
	})

	t.Run("album preserves release date precision", func(t *testing.T) {
		want := model.Album{
			ID: "al2", Name: "Mother Earth", ReleaseDate: "1998",
			ReleaseDatePrecision: "year", TotalTracks: 10,
			ArtistIDs: []string{"ar1"}, RefreshedAt: now,
		}
		_, decoded := marshalRoundTrip(t, newAlbumItem(want, now))
		got, err := decoded.toModel()
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
	})

	t.Run("tombstones survive", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			run  func(t *testing.T)
		}{
			{"track", func(t *testing.T) {
				_, d := marshalRoundTrip(t, newTrackItem(model.Track{ID: "gone", Missing: true}, now))
				got, err := d.toModel()
				if err != nil || !got.Missing {
					t.Errorf("track tombstone lost: %+v %v", got, err)
				}
			}},
			{"artist", func(t *testing.T) {
				_, d := marshalRoundTrip(t, newArtistItem(model.Artist{ID: "gone", Missing: true}, now))
				got, err := d.toModel()
				if err != nil || !got.Missing {
					t.Errorf("artist tombstone lost: %+v %v", got, err)
				}
			}},
			{"album", func(t *testing.T) {
				_, d := marshalRoundTrip(t, newAlbumItem(model.Album{ID: "gone", Missing: true}, now))
				got, err := d.toModel()
				if err != nil || !got.Missing {
					t.Errorf("album tombstone lost: %+v %v", got, err)
				}
			}},
		} {
			t.Run(tc.name, tc.run)
		}
	})
}

func TestDimensionItemsStampRefreshedAt(t *testing.T) {
	now := mustTS(t, "2025-03-14T21:04:33.123Z")
	// RefreshedAt drives the 30-day staleness check, so it must come from the store's
	// clock rather than whatever the caller happened to leave on the struct.
	stale := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	item := newTrackItem(model.Track{ID: "t1", RefreshedAt: stale}, now)
	if item.RefreshedAt != model.FormatTS(now) {
		t.Errorf("RefreshedAt = %q, want the store clock's %q", item.RefreshedAt, model.FormatTS(now))
	}
}

func TestStateItemsMarshal(t *testing.T) {
	now := mustTS(t, "2025-03-14T21:04:33.123Z")

	t.Run("poll cursor", func(t *testing.T) {
		item := pollCursorItem{
			PK: PKState, SK: SKPollCursor, Type: itemTypeState,
			LastPlayedAt: model.FormatTS(now), LastRunAt: model.FormatTS(now), LastStatus: "ok",
		}
		av, decoded := marshalRoundTrip(t, item)
		requireAttrs(t, av, "PK", "SK", "lastPlayedAt", "lastRunAt", "lastStatus")
		if decoded != item {
			t.Errorf("round trip = %+v", decoded)
		}
	})

	t.Run("gap marker", func(t *testing.T) {
		item := gapItem{
			PK: PKState, SK: GapSK(now), Type: itemTypeState,
			DetectedAt: model.FormatTS(now), ItemsReturned: 50, Limit: 50,
		}
		av, decoded := marshalRoundTrip(t, item)
		requireAttrs(t, av, "PK", "SK", "detectedAt", "itemsReturned", "limit")
		if decoded != item {
			t.Errorf("round trip = %+v", decoded)
		}
	})

	t.Run("ingest marker", func(t *testing.T) {
		item := ingestItem{
			PK: PKState, SK: IngestSK("2025-03"), Type: itemTypeState,
			Month: "2025-03", Source: string(model.SourceExport),
			ImportedAt: model.FormatTS(now), PlayCount: 1234,
		}
		av, decoded := marshalRoundTrip(t, item)
		requireAttrs(t, av, "PK", "SK", "month", "source", "importedAt", "playCount")
		if decoded != item {
			t.Errorf("round trip = %+v", decoded)
		}
	})

	t.Run("config", func(t *testing.T) {
		item := configItem{
			PK: PKState, SK: SKConfig, Type: itemTypeState,
			Timezone: "Europe/Madrid", SchemaVersion: model.SchemaVersion,
			WrittenAt: model.FormatTS(now),
		}
		av, decoded := marshalRoundTrip(t, item)
		requireAttrs(t, av, "PK", "SK", "timezone", "schemaVersion", "writtenAt")
		if decoded != item {
			t.Errorf("round trip = %+v", decoded)
		}
	})
}

func TestSchemaIsSelfConsistent(t *testing.T) {
	if Schema.PartitionKey != AttrPK || Schema.SortKey != AttrSK {
		t.Errorf("Schema keys = (%q, %q)", Schema.PartitionKey, Schema.SortKey)
	}
	if len(Schema.Indexes) != 1 {
		t.Fatalf("indexes = %d, want 1", len(Schema.Indexes))
	}
	gsi := Schema.Indexes[0]
	if gsi.Name != IndexGSI1 || gsi.PartitionKey != AttrGSI1PK || gsi.SortKey != AttrGSI1SK {
		t.Errorf("GSI1 = %+v", gsi)
	}
	if len(gsi.Projected) == 0 {
		t.Error("GSI1 must project msPlayed and source, else access pattern 6 needs a base-table fetch")
	}
}
