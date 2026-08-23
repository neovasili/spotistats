package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
	"github.com/neovasili/spotistats/internal/store/storetest"
)

// TestPutPlaysBatchDedupesByKey guards a failure that stopped the real backfill two files in.
//
// BatchWriteItem rejects the WHOLE batch if two items share a key, and Spotify's export
// genuinely repeats (ts, trackId) pairs where its history files overlap. One duplicated row
// must not fail the 24 good writes beside it.
func TestPutPlaysBatchDedupesByKey(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	p1 := storetest.APIPlay(t, "2015-03-14T10:00:00.000Z", "t1")
	p2 := storetest.APIPlay(t, "2015-03-14T11:00:00.000Z", "t2")

	// p1 twice, as the overlapping export files produce.
	n, err := s.PutPlaysBatch(ctx, []model.Play{p1, p2, p1})
	if err != nil {
		t.Fatalf("a duplicated key must be collapsed, not fail the batch: %v", err)
	}
	if n != 2 {
		t.Errorf("sent = %d, want 2 after dedup", n)
	}

	got := 0
	for _, err := range s.Plays(ctx,
		mustTS(t, "2015-03-14T00:00:00.000Z"),
		mustTS(t, "2015-03-15T00:00:00.000Z"), store.PlayFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		got++
	}
	if got != 2 {
		t.Errorf("stored plays = %d, want 2", got)
	}
}

// A batch larger than DynamoDB's 25-item limit must still be chunked correctly around dedup.
func TestPutPlaysBatchChunksBeyondTwentyFive(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	plays := make([]model.Play, 0, 60)
	for i := range 60 {
		ts := fmt.Sprintf("2015-04-%02dT10:00:00.000Z", 1+i%28)
		plays = append(plays, storetest.APIPlay(t, ts, fmt.Sprintf("t%02d", i)))
	}
	n, err := s.PutPlaysBatch(ctx, plays)
	if err != nil {
		t.Fatal(err)
	}
	if n != 60 {
		t.Errorf("sent = %d, want 60", n)
	}
}
