package store

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/neovasili/spotistats/internal/model"
)

// LeaderboardEntry is one ranked row of a materialised leaderboard. Display fields are
// denormalised into it so rendering the dashboard needs no dimension lookups at all.
type LeaderboardEntry struct {
	ID       string `dynamodbav:"id"`
	Name     string `dynamodbav:"name"`
	Plays    int64  `dynamodbav:"plays"`
	MsPlayed int64  `dynamodbav:"msPlayed"`
	ImageURL string `dynamodbav:"imageUrl,omitempty"`
}

// Leaderboard is a precomputed top-N list for one dimension and period.
//
// It exists so a dashboard widget is a single GetItem rather than a partition query plus an
// in-Lambda sort: DynamoDB cannot order by a non-key attribute, so ranking by listening
// time otherwise means reading every entity in the period.
type Leaderboard struct {
	Dim        model.Dim
	Period     model.Period
	Metric     string // "plays" or "ms" -- which measure the ordering reflects
	Entries    []LeaderboardEntry
	ComputedAt string
}

type leaderboardItem struct {
	PK   string `dynamodbav:"PK"`
	SK   string `dynamodbav:"SK"`
	Type string `dynamodbav:"type"`

	Dim        string             `dynamodbav:"dim"`
	Period     string             `dynamodbav:"period"`
	Metric     string             `dynamodbav:"metric"`
	Entries    []LeaderboardEntry `dynamodbav:"entries"`
	ComputedAt string             `dynamodbav:"computedAt"`
}

// PutLeaderboard writes a materialised leaderboard.
func (s *Store) PutLeaderboard(ctx context.Context, l Leaderboard) error {
	const op = "PutLeaderboard"
	pk := TopPK(l.Dim, l.Period)
	computedAt := l.ComputedAt
	if computedAt == "" {
		computedAt = model.FormatTS(s.now())
	}
	av, err := attributevalue.MarshalMap(leaderboardItem{
		PK: pk, SK: SKTopVersion, Type: itemTypeTop,
		Dim: string(l.Dim), Period: string(l.Period), Metric: l.Metric,
		Entries: l.Entries, ComputedAt: computedAt,
	})
	if err != nil {
		return fmt.Errorf("store: marshal leaderboard: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	})
	return classify(op, pk, SKTopVersion, err)
}

// GetLeaderboard reads a materialised leaderboard, returning ErrNotFound when it has not
// been computed yet.
func (s *Store) GetLeaderboard(ctx context.Context, dim model.Dim, period model.Period) (Leaderboard, error) {
	const op = "GetLeaderboard"
	pk := TopPK(dim, period)
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       key(pk, SKTopVersion),
	})
	if err != nil {
		return Leaderboard{}, classify(op, pk, SKTopVersion, err)
	}
	if out.Item == nil {
		return Leaderboard{}, &Error{Op: op, PK: pk, SK: SKTopVersion, Err: ErrNotFound}
	}
	var item leaderboardItem
	if err := attributevalue.UnmarshalMap(out.Item, &item); err != nil {
		return Leaderboard{}, fmt.Errorf("store: unmarshal leaderboard: %w", err)
	}
	return Leaderboard{
		Dim: model.Dim(item.Dim), Period: model.Period(item.Period),
		Metric: item.Metric, Entries: item.Entries, ComputedAt: item.ComputedAt,
	}, nil
}
