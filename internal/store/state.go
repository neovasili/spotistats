package store

import (
	"context"
	"errors"
	"fmt"
	"iter"

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
