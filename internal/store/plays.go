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

// RecordResult reports what RecordPlay did.
type RecordResult struct {
	// Inserted is false when the play was already stored, in which case no aggregate
	// deltas were applied.
	Inserted bool
	// DeltasApplied is the number of aggregate rows updated. Zero when Inserted is false.
	DeltasApplied int
}

// PutPlay writes a play, conditional on it not already existing.
//
// It returns (false, nil) -- a bool, with a NIL error -- when the play was already stored.
// That is the normal outcome of re-reading an overlapping capture window, so it is not an
// error condition. Returning a bool rather than only a sentinel makes the "skip the
// aggregates" branch impossible to overlook at a call site.
func (s *Store) PutPlay(ctx context.Context, p model.Play) (bool, error) {
	const op = "PutPlay"
	if err := p.Validate(); err != nil {
		return false, err
	}

	item := newPlayItem(p)
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return false, fmt.Errorf("store: marshal play: %w", err)
	}

	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                aws.String(s.table),
		Item:                     av,
		ConditionExpression:      aws.String("attribute_not_exists(#pk)"),
		ExpressionAttributeNames: map[string]string{"#pk": AttrPK},
	})
	if err != nil {
		cerr := classify(op, item.PK, item.SK, err)
		if errors.Is(cerr, ErrAlreadyExists) {
			return false, nil
		}
		return false, cerr
	}
	return true, nil
}

// RecordPlay is the ingestion primitive: it writes the play and, only if that actually
// inserted a row, applies its aggregate deltas.
//
// The ordering invariant lives here rather than at every call site. The failure mode is
// therefore always "redo work" and never "double-count": a crash between the insert and
// the deltas leaves the counters low, which the nightly reconcile repairs from the raw
// plays.
//
// genres is the concatenated Genres of every artist on the play; FactsFor normalises and
// deduplicates them.
func (s *Store) RecordPlay(ctx context.Context, p model.Play, genres []string) (RecordResult, error) {
	inserted, err := s.PutPlay(ctx, p)
	if err != nil || !inserted {
		return RecordResult{}, err
	}

	deltas := model.AggregateDeltas(model.FactsFor(p, genres), s.cal)
	if err := s.ApplyDeltas(ctx, deltas); err != nil {
		return RecordResult{Inserted: true}, err
	}
	return RecordResult{Inserted: true, DeltasApplied: len(deltas)}, nil
}

// DeletePlay removes a play row. It does NOT adjust aggregates: the only caller is the
// export importer superseding api-sourced rows for a month it is about to recompute from
// scratch, so adjusting counters here would double-correct.
func (s *Store) DeletePlay(ctx context.Context, playedAt time.Time, trackID string) error {
	const op = "DeletePlay"
	pk, sk := PlayPartition(playedAt), PlaySK(playedAt, trackID)
	_, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key:       key(pk, sk),
	})
	return classify(op, pk, sk, err)
}

// PlayFilter narrows a play scan.
type PlayFilter struct {
	// Source restricts results to one ingest source. Empty means all.
	Source model.Source

	// AfterSK resumes strictly after a previously returned sort key, for exact pagination.
	//
	// It exists so the API can paginate in O(n) rather than re-reading and discarding a
	// growing prefix on every page. Resuming from "the last timestamp seen, plus one
	// millisecond" would be simpler but wrong: two plays can share a millisecond (the GDPR
	// export makes that reachable) and one of them would be silently skipped. Carrying the
	// full sort key is exact.
	AfterSK string
}

// exclusiveLowerBound returns the smallest sort key strictly greater than sk.
//
// A DynamoDB KeyConditionExpression allows exactly one condition per sort key, so an
// exclusive lower bound cannot be expressed as `> :after AND < :hi`; it has to be folded into
// the BETWEEN. Appending a NUL byte does that: for any real key k, sk < sk+"\x00" <= k
// whenever k > sk, because no Spotistats sort key contains a NUL.
func exclusiveLowerBound(sk string) string { return sk + "\x00" }

// Plays iterates every play in the half-open range [from, to), oldest first.
//
// A local calendar month spans two UTC partitions (see PlayPartition), and this method
// owns that fan-out so no caller has to know about it. Sort keys are full UTC instants, so
// the range condition filters precisely inside each partition and nothing outside [from,
// to) is returned.
//
// It is an iterator rather than a paginated call because its consumers -- the reconciler
// and the CLI exporter -- stream over the whole range.
func (s *Store) Plays(ctx context.Context, from, to time.Time, f PlayFilter) iter.Seq2[model.Play, error] {
	return func(yield func(model.Play, error) bool) {
		partitions := PlayPartitionsBetween(from, to)
		if len(partitions) == 0 {
			return
		}

		lo := PlaySK(from, "")
		// The sort key is "{timestamp}#{trackID}". Using the bare formatted instant as the
		// exclusive upper bound is correct: every real key at exactly `to` has a "#..."
		// suffix and so sorts strictly after it.
		hi := model.FormatTS(to)

		// When resuming, partitions wholly before the cursor hold nothing new.
		resumePartition := ""
		if f.AfterSK != "" {
			if ts, _, err := ParsePlaySK(f.AfterSK); err == nil {
				resumePartition = PlayPartition(ts)
			}
		}

		for _, pk := range partitions {
			partitionLo := lo
			if resumePartition != "" {
				if pk < resumePartition {
					continue
				}
				if pk == resumePartition {
					partitionLo = exclusiveLowerBound(f.AfterSK)
				}
			}
			var start map[string]ddbtypes.AttributeValue
			for {
				in := &dynamodb.QueryInput{
					TableName:              aws.String(s.table),
					KeyConditionExpression: aws.String("#pk = :pk AND #sk BETWEEN :lo AND :hi"),
					ExpressionAttributeNames: map[string]string{
						"#pk": AttrPK,
						"#sk": AttrSK,
					},
					ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
						":pk": &ddbtypes.AttributeValueMemberS{Value: pk},
						":lo": &ddbtypes.AttributeValueMemberS{Value: partitionLo},
						":hi": &ddbtypes.AttributeValueMemberS{Value: hi},
					},
					ExclusiveStartKey: start,
				}
				if f.Source != "" {
					in.FilterExpression = aws.String("#src = :src")
					in.ExpressionAttributeNames["#src"] = "source"
					in.ExpressionAttributeValues[":src"] = &ddbtypes.AttributeValueMemberS{Value: string(f.Source)}
				}

				out, err := s.db.Query(ctx, in)
				if err != nil {
					yield(model.Play{}, classify("Plays", pk, "", err))
					return
				}
				for _, raw := range out.Items {
					var item playItem
					if err := attributevalue.UnmarshalMap(raw, &item); err != nil {
						if !yield(model.Play{}, fmt.Errorf("store: unmarshal play: %w", err)) {
							return
						}
						continue
					}
					p, err := item.toModel()
					if !yield(p, err) {
						return
					}
				}
				if len(out.LastEvaluatedKey) == 0 {
					break
				}
				start = out.LastEvaluatedKey
			}
		}
	}
}

// PlaysOfTrack iterates every play of one track in [from, to), oldest first, via GSI1.
//
// IMPORTANT: GSI1 uses an INCLUDE projection (see GSI1ProjectedAttributes), so the returned
// plays carry only the projected attributes. AlbumID and ArtistIDs come back EMPTY. Fetch
// the base-table row if you need them.
func (s *Store) PlaysOfTrack(ctx context.Context, trackID string, from, to time.Time) iter.Seq2[model.Play, error] {
	return s.PlaysOfTrackAfter(ctx, trackID, from, to, "")
}

// PlaysOfTrackAfter is PlaysOfTrack resuming strictly after a previously returned GSI1 sort
// key, which is the play's timestamp. See PlayFilter.AfterSK for why the full key is carried
// rather than a timestamp plus one millisecond.
func (s *Store) PlaysOfTrackAfter(
	ctx context.Context, trackID string, from, to time.Time, afterSK string,
) iter.Seq2[model.Play, error] {
	return func(yield func(model.Play, error) bool) {
		pk := TrackGSI1PK(trackID)
		if !from.Before(to) {
			return
		}
		// A KeyConditionExpression permits exactly ONE condition per key, so the range
		// must be a single BETWEEN rather than a >= paired with a <.
		//
		// BETWEEN is inclusive at both ends, and GSI1SK is the bare formatted instant with
		// no trailing discriminator, so the exclusive upper bound of [from, to) is spelled
		// as the last representable instant below `to`. TimestampFormat has exactly
		// millisecond precision, so stepping back one millisecond is exact rather than an
		// approximation. (The base table differs: its sort key carries a "#trackID"
		// suffix, so FormatTS(to) is already strictly below every real key at `to`.)
		lo := model.FormatTS(from)
		if afterSK != "" {
			lo = exclusiveLowerBound(afterSK)
		}
		hi := model.FormatTS(to.Add(-time.Millisecond))

		var start map[string]ddbtypes.AttributeValue
		for {
			out, err := s.db.Query(ctx, &dynamodb.QueryInput{
				TableName:              aws.String(s.table),
				IndexName:              aws.String(IndexGSI1),
				KeyConditionExpression: aws.String("#pk = :pk AND #sk BETWEEN :lo AND :hi"),
				ExpressionAttributeNames: map[string]string{
					"#pk": AttrGSI1PK,
					"#sk": AttrGSI1SK,
				},
				ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
					":pk": &ddbtypes.AttributeValueMemberS{Value: pk},
					":lo": &ddbtypes.AttributeValueMemberS{Value: lo},
					":hi": &ddbtypes.AttributeValueMemberS{Value: hi},
				},
				ExclusiveStartKey: start,
			})
			if err != nil {
				yield(model.Play{}, classify("PlaysOfTrack", pk, "", err))
				return
			}
			for _, raw := range out.Items {
				var item playItem
				if err := attributevalue.UnmarshalMap(raw, &item); err != nil {
					if !yield(model.Play{}, fmt.Errorf("store: unmarshal play: %w", err)) {
						return
					}
					continue
				}
				p, err := item.toModel()
				if !yield(p, err) {
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
