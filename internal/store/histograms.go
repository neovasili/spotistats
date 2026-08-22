package store

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/neovasili/spotistats/internal/model"
)

// HistogramKind selects which listening-rhythm histogram a row holds.
type HistogramKind string

const (
	// HistogramHour buckets by hour of day, 0..23.
	HistogramHour HistogramKind = SKHistHour
	// HistogramWeekday buckets by weekday, 0 (Sunday) .. 6.
	HistogramWeekday HistogramKind = SKHistWeekday
)

// Histogram counts plays and listening time per bucket.
//
// Buckets are computed in the listener's LOCAL timezone. An hour-of-day chart in UTC would
// be meaningless -- it would show someone in Madrid going to sleep an hour early in winter.
type Histogram struct {
	Period   model.Period
	Kind     HistogramKind
	Plays    map[int]int64
	MsPlayed map[int]int64
}

type histogramItem struct {
	PK   string `dynamodbav:"PK"`
	SK   string `dynamodbav:"SK"`
	Type string `dynamodbav:"type"`

	Period   string           `dynamodbav:"period"`
	Kind     string           `dynamodbav:"kind"`
	Plays    map[string]int64 `dynamodbav:"plays"`
	MsPlayed map[string]int64 `dynamodbav:"msPlayed"`
}

// PutHistogram writes a histogram row, replacing any previous value. The nightly rollup
// recomputes these wholesale rather than incrementing, so absolute writes are correct here.
func (s *Store) PutHistogram(ctx context.Context, h Histogram) error {
	const op = "PutHistogram"
	pk := HistPK(h.Period)
	sk := string(h.Kind)

	item := histogramItem{
		PK: pk, SK: sk, Type: itemTypeHist,
		Period:   string(h.Period),
		Kind:     string(h.Kind),
		Plays:    make(map[string]int64, len(h.Plays)),
		MsPlayed: make(map[string]int64, len(h.MsPlayed)),
	}
	for b, v := range h.Plays {
		item.Plays[fmt.Sprintf("%d", b)] = v
	}
	for b, v := range h.MsPlayed {
		item.MsPlayed[fmt.Sprintf("%d", b)] = v
	}

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("store: marshal histogram: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	})
	return classify(op, pk, sk, err)
}

// GetHistogram reads a histogram row, returning ErrNotFound when absent.
func (s *Store) GetHistogram(ctx context.Context, period model.Period, kind HistogramKind) (Histogram, error) {
	const op = "GetHistogram"
	pk := HistPK(period)
	sk := string(kind)

	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       key(pk, sk),
	})
	if err != nil {
		return Histogram{}, classify(op, pk, sk, err)
	}
	if out.Item == nil {
		return Histogram{}, &Error{Op: op, PK: pk, SK: sk, Err: ErrNotFound}
	}

	var item histogramItem
	if err := attributevalue.UnmarshalMap(out.Item, &item); err != nil {
		return Histogram{}, fmt.Errorf("store: unmarshal histogram: %w", err)
	}
	h := Histogram{
		Period:   model.Period(item.Period),
		Kind:     HistogramKind(item.Kind),
		Plays:    make(map[int]int64, len(item.Plays)),
		MsPlayed: make(map[int]int64, len(item.MsPlayed)),
	}
	for b, v := range item.Plays {
		var n int
		if _, err := fmt.Sscanf(b, "%d", &n); err == nil {
			h.Plays[n] = v
		}
	}
	for b, v := range item.MsPlayed {
		var n int
		if _, err := fmt.Sscanf(b, "%d", &n); err == nil {
			h.MsPlayed[n] = v
		}
	}
	return h, nil
}
