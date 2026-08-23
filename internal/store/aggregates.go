package store

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/neovasili/spotistats/internal/model"
)

// DynamoDB request limits.
const (
	maxBatchGetKeys    = 100
	maxBatchWriteItems = 25
	// maxUnprocessedRetries bounds the retry loop for partially-processed batches.
	maxUnprocessedRetries = 5
)

// ApplyDeltas applies aggregate contributions with atomic counters.
//
// Each delta becomes one UpdateItem using ADD for the counters, so concurrent updates to
// the same row compose correctly without read-modify-write.
//
// The firstPlayedAt and lastPlayedAt bounds are best-effort. DynamoDB has no MIN/MAX, so
// firstPlayedAt uses if_not_exists (correct as long as writes arrive roughly in order) and
// lastPlayedAt is set unconditionally (so an out-of-order write can move it backwards).
// The counters -- which is what every query actually reports -- are always exact, and the
// nightly reconcile recomputes the bounds from raw plays.
//
// Callers should MergeDeltas first: it collapses a batch into far fewer UpdateItem calls
// AND computes true minimum and maximum bounds in memory, which sidesteps the limitation
// above for the batch case.
func (s *Store) ApplyDeltas(ctx context.Context, deltas []model.AggDelta) error {
	const op = "ApplyDeltas"

	for _, d := range deltas {
		if err := d.Key.Validate(); err != nil {
			return err
		}
		pk, sk := d.Key.PK(), d.Key.SK()

		// DynamoDB rejects an ExpressionAttributeNames entry that no expression
		// references, so #first and #last are added below only when their SET clauses are.
		names := map[string]string{
			"#type":     "type",
			"#dim":      "dim",
			"#period":   "period",
			"#entityId": "entityId",
			"#plays":    "plays",
			"#playsX":   "playsExact",
			"#ms":       "msPlayed",
			"#msX":      "msPlayedExact",
		}
		values := map[string]ddbtypes.AttributeValue{
			":plays":  numberAV(d.Plays),
			":playsX": numberAV(d.PlaysExact),
			":ms":     numberAV(d.MsPlayed),
			":msX":    numberAV(d.MsPlayedExact),
			":type":   &ddbtypes.AttributeValueMemberS{Value: itemTypeAggregate},
			":dim":    &ddbtypes.AttributeValueMemberS{Value: string(d.Key.Dim)},
			":period": &ddbtypes.AttributeValueMemberS{Value: string(d.Key.Period)},
			":entity": &ddbtypes.AttributeValueMemberS{Value: d.Key.EntityID},
		}

		// The denormalised descriptors are written with if_not_exists so they are set once
		// on creation and never rewritten, keeping the hot path a pure counter update.
		sets := []string{
			"#type = if_not_exists(#type, :type)",
			"#dim = if_not_exists(#dim, :dim)",
			"#period = if_not_exists(#period, :period)",
			"#entityId = if_not_exists(#entityId, :entity)",
		}
		if !d.FirstPlayedAt.IsZero() {
			names["#first"] = "firstPlayedAt"
			values[":first"] = &ddbtypes.AttributeValueMemberS{Value: model.FormatTS(d.FirstPlayedAt)}
			sets = append(sets, "#first = if_not_exists(#first, :first)")
		}
		if !d.LastPlayedAt.IsZero() {
			names["#last"] = "lastPlayedAt"
			values[":last"] = &ddbtypes.AttributeValueMemberS{Value: model.FormatTS(d.LastPlayedAt)}
			sets = append(sets, "#last = :last")
		}

		expr := "SET " + strings.Join(sets, ", ") +
			" ADD #plays :plays, #playsX :playsX, #ms :ms, #msX :msX"

		if _, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String(s.table),
			Key:                       key(pk, sk),
			UpdateExpression:          aws.String(expr),
			ExpressionAttributeNames:  names,
			ExpressionAttributeValues: values,
		}); err != nil {
			return classify(op, pk, sk, err)
		}
	}
	return nil
}

// GetAggregate reads one aggregate row. It returns ErrNotFound when the row is absent,
// which for an aggregate means "no plays recorded", not an error condition -- callers
// generally want to treat it as a zero.
func (s *Store) GetAggregate(ctx context.Context, k model.AggKey) (model.Aggregate, error) {
	const op = "GetAggregate"
	if err := k.Validate(); err != nil {
		return model.Aggregate{}, err
	}
	pk, sk := k.PK(), k.SK()

	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       key(pk, sk),
	})
	if err != nil {
		return model.Aggregate{}, classify(op, pk, sk, err)
	}
	if out.Item == nil {
		return model.Aggregate{}, &Error{Op: op, PK: pk, SK: sk, Err: ErrNotFound}
	}

	var item aggregateItem
	if err := attributevalue.UnmarshalMap(out.Item, &item); err != nil {
		return model.Aggregate{}, fmt.Errorf("store: unmarshal aggregate: %w", err)
	}
	return item.toModel()
}

// GetAggregateOrZero is GetAggregate with ErrNotFound mapped to a zero-valued aggregate,
// which is what most read paths want.
func (s *Store) GetAggregateOrZero(ctx context.Context, k model.AggKey) (model.Aggregate, error) {
	a, err := s.GetAggregate(ctx, k)
	if errors.Is(err, ErrNotFound) {
		return model.Aggregate{Key: k}, nil
	}
	return a, err
}

// BatchGetAggregates reads many aggregate rows, chunking at DynamoDB's 100-key limit and
// retrying UnprocessedKeys with a bounded loop.
//
// This is how a non-calendar-aligned range is answered: sum the monthly rows the range
// spans in a single round trip rather than one GetItem per month.
//
// Keys absent from the table are simply absent from the result map.
func (s *Store) BatchGetAggregates(ctx context.Context, keys []model.AggKey) (map[model.AggKey]model.Aggregate, error) {
	const op = "BatchGetAggregates"
	out := make(map[model.AggKey]model.Aggregate, len(keys))
	if len(keys) == 0 {
		return out, nil
	}

	// Deduplicate: the same aggregate key can legitimately appear twice in a caller's
	// request list, and DynamoDB rejects a batch containing duplicate keys.
	seen := make(map[model.AggKey]struct{}, len(keys))
	unique := make([]model.AggKey, 0, len(keys))
	for _, k := range keys {
		if err := k.Validate(); err != nil {
			return nil, err
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		unique = append(unique, k)
	}

	for start := 0; start < len(unique); start += maxBatchGetKeys {
		end := min(start+maxBatchGetKeys, len(unique))
		chunk := unique[start:end]

		reqKeys := make([]map[string]ddbtypes.AttributeValue, 0, len(chunk))
		for _, k := range chunk {
			reqKeys = append(reqKeys, key(k.PK(), k.SK()))
		}

		pending := map[string]ddbtypes.KeysAndAttributes{
			s.table: {Keys: reqKeys},
		}

		for attempt := 0; len(pending) > 0; attempt++ {
			if attempt > maxUnprocessedRetries {
				return nil, &Error{Op: op, Err: fmt.Errorf(
					"%w: %d keys still unprocessed after %d attempts",
					ErrThrottled, len(pending[s.table].Keys), attempt)}
			}

			resp, err := s.db.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: pending})
			if err != nil {
				return nil, classify(op, "", "", err)
			}
			for _, raw := range resp.Responses[s.table] {
				var item aggregateItem
				if err := attributevalue.UnmarshalMap(raw, &item); err != nil {
					return nil, fmt.Errorf("store: unmarshal aggregate: %w", err)
				}
				agg, err := item.toModel()
				if err != nil {
					return nil, err
				}
				out[agg.Key] = agg
			}

			pending = nil
			if ks, ok := resp.UnprocessedKeys[s.table]; ok && len(ks.Keys) > 0 {
				pending = map[string]ddbtypes.KeysAndAttributes{s.table: ks}
			}
		}
	}
	return out, nil
}

// PutAggregate overwrites a row absolutely, discarding whatever counters were there.
//
// This is the reconcile and backfill path: the importer accumulates totals in memory and
// writes each row once, and the nightly reconcile replaces drifted counters with values
// recomputed from raw plays. Never use it on the ingestion path -- use ApplyDeltas.
func (s *Store) PutAggregate(ctx context.Context, a model.Aggregate) error {
	const op = "PutAggregate"
	if err := a.Key.Validate(); err != nil {
		return err
	}
	item := newAggregateItem(a)
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("store: marshal aggregate: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	})
	return classify(op, item.PK, item.SK, err)
}

// PutAggregates overwrites many rows via BatchWriteItem, chunking at 25 items and retrying
// UnprocessedItems.
func (s *Store) PutAggregates(ctx context.Context, aggs []model.Aggregate) error {
	const op = "PutAggregates"
	if len(aggs) == 0 {
		return nil
	}

	requests := make([]ddbtypes.WriteRequest, 0, len(aggs))
	for _, a := range aggs {
		if err := a.Key.Validate(); err != nil {
			return err
		}
		av, err := attributevalue.MarshalMap(newAggregateItem(a))
		if err != nil {
			return fmt.Errorf("store: marshal aggregate: %w", err)
		}
		requests = append(requests, ddbtypes.WriteRequest{
			PutRequest: &ddbtypes.PutRequest{Item: av},
		})
	}

	for start := 0; start < len(requests); start += maxBatchWriteItems {
		end := min(start+maxBatchWriteItems, len(requests))
		pending := map[string][]ddbtypes.WriteRequest{s.table: requests[start:end]}

		for attempt := 0; len(pending) > 0; attempt++ {
			if attempt > maxUnprocessedRetries {
				return &Error{Op: op, Err: fmt.Errorf(
					"%w: %d writes still unprocessed after %d attempts",
					ErrThrottled, len(pending[s.table]), attempt)}
			}
			resp, err := s.db.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: pending})
			if err != nil {
				return classify(op, "", "", err)
			}
			pending = nil
			if rs, ok := resp.UnprocessedItems[s.table]; ok && len(rs) > 0 {
				pending = map[string][]ddbtypes.WriteRequest{s.table: rs}
			}
		}
	}
	return nil
}

// QueryAggregates iterates one aggregate partition.
//
// skPrefix optionally narrows the sort key with begins_with. That is what makes the
// calendar heatmap a single query: DimTotal day rows live in their year's partition, so
// QueryAggregates(DimTotal, "2025", "2025-") returns that year's days and excludes the
// year total, whose sort key is "ALL".
func (s *Store) QueryAggregates(ctx context.Context, dim model.Dim, period model.Period, skPrefix string) iter.Seq2[model.Aggregate, error] {
	return func(yield func(model.Aggregate, error) bool) {
		pk := model.AggKey{Dim: dim, Period: period, EntityID: model.TotalEntityID}.PK()

		cond := "#pk = :pk"
		names := map[string]string{"#pk": AttrPK}
		values := map[string]ddbtypes.AttributeValue{
			":pk": &ddbtypes.AttributeValueMemberS{Value: pk},
		}
		if skPrefix != "" {
			cond += " AND begins_with(#sk, :skp)"
			names["#sk"] = AttrSK
			values[":skp"] = &ddbtypes.AttributeValueMemberS{Value: skPrefix}
		}

		var start map[string]ddbtypes.AttributeValue
		for {
			out, err := s.db.Query(ctx, &dynamodb.QueryInput{
				TableName:                 aws.String(s.table),
				KeyConditionExpression:    aws.String(cond),
				ExpressionAttributeNames:  names,
				ExpressionAttributeValues: values,
				ExclusiveStartKey:         start,
			})
			if err != nil {
				yield(model.Aggregate{}, classify("QueryAggregates", pk, "", err))
				return
			}
			for _, raw := range out.Items {
				var item aggregateItem
				if err := attributevalue.UnmarshalMap(raw, &item); err != nil {
					if !yield(model.Aggregate{}, fmt.Errorf("store: unmarshal aggregate: %w", err)) {
						return
					}
					continue
				}
				agg, err := item.toModel()
				if !yield(agg, err) {
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

func numberAV(n int64) ddbtypes.AttributeValue {
	return &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", n)}
}

// ScanAggregateKeys yields the key of every aggregate row in the table.
//
// It is a full table scan and exists for exactly one caller: the full reconcile, which needs to
// find rows that NO play supports any more. Nothing else should use it.
//
// Such orphans are not hypothetical. When artist identity converged from name keys onto real
// Spotify IDs, the reconcile rewrote every row it computed and left the superseded name-keyed
// rows in place -- so the dashboard listed the same artist twice, once under each key, with the
// history split between them.
func (s *Store) ScanAggregateKeys(ctx context.Context) iter.Seq2[model.AggKey, error] {
	return func(yield func(model.AggKey, error) bool) {
		var start map[string]ddbtypes.AttributeValue
		for {
			out, err := s.db.Scan(ctx, &dynamodb.ScanInput{
				TableName:        aws.String(s.table),
				FilterExpression: aws.String("#t = :t"),
				// type is a DynamoDB reserved keyword.
				ExpressionAttributeNames: map[string]string{"#t": "type"},
				ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
					":t": &ddbtypes.AttributeValueMemberS{Value: itemTypeAggregate},
				},
				ProjectionExpression: aws.String("PK,SK"),
				ExclusiveStartKey:    start,
			})
			if err != nil {
				yield(model.AggKey{}, classify("ScanAggregateKeys", "", "", err))
				return
			}
			for _, item := range out.Items {
				key, perr := model.ParseAggKey(stringAttr(item, AttrPK), stringAttr(item, AttrSK))
				if perr != nil {
					// A row whose key cannot be parsed is not something to delete on a guess.
					if !yield(model.AggKey{}, fmt.Errorf("store: ScanAggregateKeys: %w", perr)) {
						return
					}
					continue
				}
				if !yield(key, nil) {
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

// DeleteAggregates removes aggregate rows by key, in batches.
func (s *Store) DeleteAggregates(ctx context.Context, keys []model.AggKey) error {
	const op = "DeleteAggregates"
	if len(keys) == 0 {
		return nil
	}
	requests := make([]ddbtypes.WriteRequest, 0, len(keys))
	for _, k := range keys {
		requests = append(requests, ddbtypes.WriteRequest{
			DeleteRequest: &ddbtypes.DeleteRequest{Key: key(k.PK(), k.SK())},
		})
	}
	for start := 0; start < len(requests); start += maxBatchWriteItems {
		end := min(start+maxBatchWriteItems, len(requests))
		pending := map[string][]ddbtypes.WriteRequest{s.table: requests[start:end]}
		for attempt := 0; len(pending) > 0; attempt++ {
			if attempt > maxUnprocessedRetries {
				return &Error{Op: op, Err: fmt.Errorf(
					"%w: %d deletes still unprocessed after %d attempts",
					ErrThrottled, len(pending[s.table]), attempt)}
			}
			resp, err := s.db.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: pending})
			if err != nil {
				return classify(op, "", "", err)
			}
			pending = nil
			if rs, ok := resp.UnprocessedItems[s.table]; ok && len(rs) > 0 {
				pending = map[string][]ddbtypes.WriteRequest{s.table: rs}
			}
		}
	}
	return nil
}
