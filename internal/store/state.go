package store

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/neovasili/spotistats/internal/model"
)

// GetPollCursor reads the capture cursor. A missing cursor is not an error -- it is the
// first run -- so it returns a zero PollCursor, which the capture job treats as "fetch the
// most recent page with no `after`".
func (s *Store) GetPollCursor(ctx context.Context) (model.PollCursor, error) {
	const op = "GetPollCursor"
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            key(PKState, SKPollCursor),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return model.PollCursor{}, classify(op, PKState, SKPollCursor, err)
	}
	if out.Item == nil {
		return model.PollCursor{}, nil
	}

	var item pollCursorItem
	if err := attributevalue.UnmarshalMap(out.Item, &item); err != nil {
		return model.PollCursor{}, fmt.Errorf("store: unmarshal poll cursor: %w", err)
	}

	var c model.PollCursor
	c.LastStatus = item.LastStatus
	if item.LastPlayedAt != "" {
		if c.LastPlayedAt, err = model.ParseTS(item.LastPlayedAt); err != nil {
			return model.PollCursor{}, err
		}
	}
	if item.LastRunAt != "" {
		if c.LastRunAt, err = model.ParseTS(item.LastRunAt); err != nil {
			return model.PollCursor{}, err
		}
	}
	return c, nil
}

// PutPollCursor advances the capture cursor.
//
// The capture job must call this LAST, after every play is written. Failing earlier means
// the next run re-reads the same window, which is harmless because ingestion is
// idempotent; advancing it early would skip plays permanently.
func (s *Store) PutPollCursor(ctx context.Context, c model.PollCursor) error {
	const op = "PutPollCursor"
	item := pollCursorItem{
		PK: PKState, SK: SKPollCursor, Type: itemTypeState,
		LastStatus: c.LastStatus,
	}
	if !c.LastPlayedAt.IsZero() {
		item.LastPlayedAt = model.FormatTS(c.LastPlayedAt)
	}
	if !c.LastRunAt.IsZero() {
		item.LastRunAt = model.FormatTS(c.LastRunAt)
	}

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("store: marshal poll cursor: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	})
	return classify(op, PKState, SKPollCursor, err)
}

// PutGapMarker records a capture run whose page came back full, meaning listening may have
// outrun the polling window and plays may have been lost irrecoverably.
//
// These are kept rather than merely logged because they are the evidence for shortening the
// capture interval, and because they mark stretches of history that are known-incomplete.
func (s *Store) PutGapMarker(ctx context.Context, g model.GapMarker) error {
	const op = "PutGapMarker"
	sk := GapSK(g.DetectedAt)
	item := gapItem{
		PK: PKState, SK: sk, Type: itemTypeState,
		DetectedAt:    model.FormatTS(g.DetectedAt),
		ItemsReturned: g.ItemsReturned,
		Limit:         g.Limit,
	}
	if !g.WindowStart.IsZero() {
		item.WindowStart = model.FormatTS(g.WindowStart)
	}
	if !g.WindowEnd.IsZero() {
		item.WindowEnd = model.FormatTS(g.WindowEnd)
	}

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("store: marshal gap marker: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	})
	return classify(op, PKState, sk, err)
}

// GapMarkers iterates every recorded gap, oldest first.
func (s *Store) GapMarkers(ctx context.Context) iter.Seq2[model.GapMarker, error] {
	return func(yield func(model.GapMarker, error) bool) {
		var start map[string]ddbtypes.AttributeValue
		for {
			out, err := s.db.Query(ctx, &dynamodb.QueryInput{
				TableName:              aws.String(s.table),
				KeyConditionExpression: aws.String("#pk = :pk AND begins_with(#sk, :prefix)"),
				ExpressionAttributeNames: map[string]string{
					"#pk": AttrPK,
					"#sk": AttrSK,
				},
				ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
					":pk":     &ddbtypes.AttributeValueMemberS{Value: PKState},
					":prefix": &ddbtypes.AttributeValueMemberS{Value: prefixGap},
				},
				ExclusiveStartKey: start,
			})
			if err != nil {
				yield(model.GapMarker{}, classify("GapMarkers", PKState, "", err))
				return
			}
			for _, raw := range out.Items {
				var item gapItem
				if err := attributevalue.UnmarshalMap(raw, &item); err != nil {
					if !yield(model.GapMarker{}, fmt.Errorf("store: unmarshal gap marker: %w", err)) {
						return
					}
					continue
				}
				g := model.GapMarker{ItemsReturned: item.ItemsReturned, Limit: item.Limit}
				g.DetectedAt, _ = model.ParseTS(item.DetectedAt)
				if item.WindowStart != "" {
					g.WindowStart, _ = model.ParseTS(item.WindowStart)
				}
				if item.WindowEnd != "" {
					g.WindowEnd, _ = model.ParseTS(item.WindowEnd)
				}
				if !yield(g, nil) {
					return
				}
			}
			if len(out.LastEvaluatedKey) == 0 {
				return
			}
			start = out.LastEvaluatedKey
		}
	}
}

// GetIngestMarker reads which source is authoritative for a month, returning ErrNotFound
// when the month has never been claimed.
func (s *Store) GetIngestMarker(ctx context.Context, month model.Period) (model.IngestMarker, error) {
	const op = "GetIngestMarker"
	sk := IngestSK(month)
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       key(PKState, sk),
	})
	if err != nil {
		return model.IngestMarker{}, classify(op, PKState, sk, err)
	}
	if out.Item == nil {
		return model.IngestMarker{}, &Error{Op: op, PK: PKState, SK: sk, Err: ErrNotFound}
	}

	var item ingestItem
	if err := attributevalue.UnmarshalMap(out.Item, &item); err != nil {
		return model.IngestMarker{}, fmt.Errorf("store: unmarshal ingest marker: %w", err)
	}
	m := model.IngestMarker{
		Month:     model.Period(item.Month),
		Source:    model.Source(item.Source),
		PlayCount: item.PlayCount,
	}
	if item.ImportedAt != "" {
		if m.ImportedAt, err = model.ParseTS(item.ImportedAt); err != nil {
			return model.IngestMarker{}, err
		}
	}
	return m, nil
}

// PutIngestMarker claims a month for a source, conditional on it not already being claimed.
// It returns ErrAlreadyExists if it is, so an importer must decide explicitly whether to
// supersede rather than silently overwriting a claim.
func (s *Store) PutIngestMarker(ctx context.Context, m model.IngestMarker) error {
	const op = "PutIngestMarker"
	if !m.Month.Valid() || m.Month.Granularity() != model.GranularityMonth {
		return fmt.Errorf("store: ingest marker needs a YYYY-MM month, got %q", m.Month)
	}
	if !m.Source.Valid() {
		return fmt.Errorf("store: ingest marker has an unknown source %q", m.Source)
	}

	sk := IngestSK(m.Month)
	importedAt := m.ImportedAt
	if importedAt.IsZero() {
		importedAt = s.now()
	}
	av, err := attributevalue.MarshalMap(ingestItem{
		PK: PKState, SK: sk, Type: itemTypeState,
		Month:      string(m.Month),
		Source:     string(m.Source),
		ImportedAt: model.FormatTS(importedAt),
		PlayCount:  m.PlayCount,
	})
	if err != nil {
		return fmt.Errorf("store: marshal ingest marker: %w", err)
	}

	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                aws.String(s.table),
		Item:                     av,
		ConditionExpression:      aws.String("attribute_not_exists(#pk)"),
		ExpressionAttributeNames: map[string]string{"#pk": AttrPK},
	})
	return classify(op, PKState, sk, err)
}

// ReplaceIngestMarker claims a month unconditionally, superseding any existing claim. The
// export importer uses it after deleting the api-sourced rows it is replacing.
func (s *Store) ReplaceIngestMarker(ctx context.Context, m model.IngestMarker) error {
	const op = "ReplaceIngestMarker"
	if err := s.PutIngestMarker(ctx, m); err != nil && !errors.Is(err, ErrAlreadyExists) {
		return err
	} else if err == nil {
		return nil
	}

	sk := IngestSK(m.Month)
	importedAt := m.ImportedAt
	if importedAt.IsZero() {
		importedAt = s.now()
	}
	av, merr := attributevalue.MarshalMap(ingestItem{
		PK: PKState, SK: sk, Type: itemTypeState,
		Month:      string(m.Month),
		Source:     string(m.Source),
		ImportedAt: model.FormatTS(importedAt),
		PlayCount:  m.PlayCount,
	})
	if merr != nil {
		return fmt.Errorf("store: marshal ingest marker: %w", merr)
	}
	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	})
	return classify(op, PKState, sk, err)
}

// PutCoverage records the facts that only a full-history pass can establish.
func (s *Store) PutCoverage(ctx context.Context, c model.CoverageRow) error {
	const op = "PutCoverage"
	item := coverageItem{
		PK: PKState, SK: SKCoverage, Type: itemTypeState,
		TotalPlays: c.TotalPlays, TotalMs: c.TotalMs,
		PlaysWithGenre: c.PlaysWithGenre, MsWithGenre: c.MsWithGenre,
		PlaysWithArtist: c.PlaysWithArtist, MsWithArtist: c.MsWithArtist,
		ComputedAt: model.FormatTS(nonZero(c.ComputedAt, s.now())),
	}
	if !c.FirstPlayedAt.IsZero() {
		item.FirstPlayedAt = model.FormatTS(c.FirstPlayedAt)
	}
	if !c.LastPlayedAt.IsZero() {
		item.LastPlayedAt = model.FormatTS(c.LastPlayedAt)
	}

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("store: marshal coverage: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	})
	return classify(op, PKState, SKCoverage, err)
}

// GetCoverage reads the coverage row, returning ErrNotFound when no full pass has run yet.
func (s *Store) GetCoverage(ctx context.Context) (model.CoverageRow, error) {
	const op = "GetCoverage"
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       key(PKState, SKCoverage),
	})
	if err != nil {
		return model.CoverageRow{}, classify(op, PKState, SKCoverage, err)
	}
	if out.Item == nil {
		return model.CoverageRow{}, &Error{Op: op, PK: PKState, SK: SKCoverage, Err: ErrNotFound}
	}

	var item coverageItem
	if err := attributevalue.UnmarshalMap(out.Item, &item); err != nil {
		return model.CoverageRow{}, fmt.Errorf("store: unmarshal coverage: %w", err)
	}
	c := model.CoverageRow{
		TotalPlays: item.TotalPlays, TotalMs: item.TotalMs,
		PlaysWithGenre: item.PlaysWithGenre, MsWithGenre: item.MsWithGenre,
		PlaysWithArtist: item.PlaysWithArtist, MsWithArtist: item.MsWithArtist,
	}
	for _, f := range []struct {
		raw string
		dst *time.Time
	}{
		{item.FirstPlayedAt, &c.FirstPlayedAt},
		{item.LastPlayedAt, &c.LastPlayedAt},
		{item.ComputedAt, &c.ComputedAt},
	} {
		if f.raw == "" {
			continue
		}
		t, perr := model.ParseTS(f.raw)
		if perr != nil {
			return model.CoverageRow{}, perr
		}
		*f.dst = t
	}
	return c, nil
}

func nonZero(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}

// PutMBIDOverride records a hand-checked Spotify-artist-to-MBID mapping.
//
// The resolver consults overrides FIRST, so this also corrects an artist MusicBrainz has linked
// to the wrong entity — not only one it has not linked at all.
func (s *Store) PutMBIDOverride(ctx context.Context, spotifyID, mbid string) error {
	const op = "PutMBIDOverride"
	if spotifyID == "" || mbid == "" {
		return &Error{Op: op, Err: errors.New("store: PutMBIDOverride requires both ids")}
	}
	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]ddbtypes.AttributeValue{
			AttrPK:       &ddbtypes.AttributeValueMemberS{Value: ArtistPK(spotifyID)},
			AttrSK:       &ddbtypes.AttributeValueMemberS{Value: SKMBIDOverride},
			"type":       &ddbtypes.AttributeValueMemberS{Value: "mbidOverride"},
			"id":         &ddbtypes.AttributeValueMemberS{Value: spotifyID},
			"mbid":       &ddbtypes.AttributeValueMemberS{Value: mbid},
			"recordedAt": &ddbtypes.AttributeValueMemberS{Value: model.FormatTS(s.now())},
		},
	})
	return classify(op, ArtistPK(spotifyID), SKMBIDOverride, err)
}

// DeleteMBIDOverride removes an override, so a correction can itself be corrected.
func (s *Store) DeleteMBIDOverride(ctx context.Context, spotifyID string) error {
	const op = "DeleteMBIDOverride"
	_, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key:       key(ArtistPK(spotifyID), SKMBIDOverride),
	})
	return classify(op, ArtistPK(spotifyID), SKMBIDOverride, err)
}

// GetMBIDOverrides reads the overrides for a set of Spotify artist IDs.
//
// Batched because the enricher consults it for every artist in its work list, and a read per
// artist would double the round trips of a job whose whole design is about minimising them.
func (s *Store) GetMBIDOverrides(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	err := s.batchGetBySK(ctx, "GetMBIDOverrides", ids, SKMBIDOverride,
		func(raw map[string]ddbtypes.AttributeValue) error {
			id, mbid := stringAttr(raw, "id"), stringAttr(raw, "mbid")
			if id != "" && mbid != "" {
				out[id] = mbid
			}
			return nil
		})
	return out, err
}

// PutExternalEnrichCursor checkpoints how far external enrichment has walked its work list.
//
// A sibling of the Spotify enrichment cursor, not a shared one: the two jobs traverse the same
// artist list at very different rates against different rate limits, and sharing a cursor would
// make each resume from wherever the other happened to stop.
func (s *Store) PutExternalEnrichCursor(ctx context.Context, artistID string) error {
	const op = "PutExternalEnrichCursor"
	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]ddbtypes.AttributeValue{
			AttrPK:      &ddbtypes.AttributeValueMemberS{Value: PKState},
			AttrSK:      &ddbtypes.AttributeValueMemberS{Value: SKExternalEnrichCursor},
			"type":      &ddbtypes.AttributeValueMemberS{Value: "cursor"},
			"artistId":  &ddbtypes.AttributeValueMemberS{Value: artistID},
			"updatedAt": &ddbtypes.AttributeValueMemberS{Value: model.FormatTS(s.now())},
		},
	})
	return classify(op, PKState, SKExternalEnrichCursor, err)
}

// GetExternalEnrichCursor reads the last artist external enrichment completed, or "" if none.
func (s *Store) GetExternalEnrichCursor(ctx context.Context) (string, error) {
	const op = "GetExternalEnrichCursor"
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       key(PKState, SKExternalEnrichCursor),
	})
	if err != nil {
		return "", classify(op, PKState, SKExternalEnrichCursor, err)
	}
	// No cursor is not an error: a first run has none.
	return stringAttr(out.Item, "artistId"), nil
}

// ErrEnrichLockHeld means another external-enrichment run is already in progress.
var ErrEnrichLockHeld = errors.New("store: an external enrichment run is already in progress")

// SKExternalEnrichLock is the single-flight lock for external enrichment.
const SKExternalEnrichLock = "EXTERNAL_ENRICH_LOCK"

// AcquireEnrichLock takes the external-enrichment lock, or returns ErrEnrichLockHeld.
//
// # Why a lock and not just reserved concurrency
//
// Both upstream rate limits are per-IP, so two overlapping runs double the real request rate and
// MusicBrainz answers 503 to everything from that IP. `ReservedConcurrentExecutions: 1` is the
// obvious guard and IS still set where the account allows it — but it cannot be relied on:
// AWS rejects ANY reservation on an account whose total concurrency limit is 10, because it
// requires 10 to remain unreserved. This account is one of those, so the deploy failed with
// "decreases account's UnreservedConcurrentExecution below its minimum value of [10]".
//
// A conditional write does not depend on an account quota. It also covers a case reserved
// concurrency never did: a manual `aws lambda invoke` landing during the scheduled run.
//
// The lock EXPIRES rather than being released only on success. A run killed mid-flight — Lambda
// timeout, OOM, a deploy — would otherwise hold it forever and silently stop all enrichment,
// which is a worse failure than the overlap it prevents.
func (s *Store) AcquireEnrichLock(ctx context.Context, ttl time.Duration) error {
	const op = "AcquireEnrichLock"
	now := s.now()
	expires := now.Add(ttl)

	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]ddbtypes.AttributeValue{
			AttrPK:       &ddbtypes.AttributeValueMemberS{Value: PKState},
			AttrSK:       &ddbtypes.AttributeValueMemberS{Value: SKExternalEnrichLock},
			"type":       &ddbtypes.AttributeValueMemberS{Value: "lock"},
			"acquiredAt": &ddbtypes.AttributeValueMemberS{Value: model.FormatTS(now)},
			"expiresAt":  &ddbtypes.AttributeValueMemberS{Value: model.FormatTS(expires)},
		},
		// Take it if nobody holds it, or if the holder's lease has lapsed. String comparison is
		// valid because model.TimestampFormat is fixed-width and therefore lexically ordered --
		// the same property the play sort keys depend on.
		ConditionExpression: aws.String("attribute_not_exists(expiresAt) OR expiresAt < :now"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":now": &ddbtypes.AttributeValueMemberS{Value: model.FormatTS(now)},
		},
	})
	if err != nil {
		if classified := classify(op, PKState, SKExternalEnrichLock, err); errors.Is(classified, ErrAlreadyExists) {
			return ErrEnrichLockHeld
		}
		return classify(op, PKState, SKExternalEnrichLock, err)
	}
	return nil
}

// ReleaseEnrichLock drops the lock so a later run need not wait for the lease to lapse.
//
// Best-effort by design: the lock expires anyway, so a failure here costs latency rather than
// correctness and must never fail the run that just did the work.
func (s *Store) ReleaseEnrichLock(ctx context.Context) error {
	const op = "ReleaseEnrichLock"
	_, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key:       key(PKState, SKExternalEnrichLock),
	})
	return classify(op, PKState, SKExternalEnrichLock, err)
}
