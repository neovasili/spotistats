package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/neovasili/spotistats/internal/model"
)

// DynamoAPI is the subset of the DynamoDB client this package uses.
//
// Narrowing it to these seven calls is what lets the chunking and UnprocessedKeys retry
// logic be tested with a hand-written fake and no container. *dynamodb.Client satisfies it.
type DynamoAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	BatchGetItem(context.Context, *dynamodb.BatchGetItemInput, ...func(*dynamodb.Options)) (*dynamodb.BatchGetItemOutput, error)
	BatchWriteItem(context.Context, *dynamodb.BatchWriteItemInput, ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
	// Scan is used by exactly one caller, ScanAggregateKeys, which the full reconcile needs to
	// find aggregate rows no play supports any more. Nothing else should scan.
	Scan(context.Context, *dynamodb.ScanInput, ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
}

// Config configures a Store.
type Config struct {
	Client    DynamoAPI
	TableName string

	// Calendar supplies the zone every aggregate period key is derived in. Its name is
	// persisted by VerifyConfig so a later change is detected rather than silently
	// splitting history across two calendars.
	Calendar model.Calendar

	// Now is a plain function rather than an interface because the store never sleeps --
	// it only needs to stamp refreshedAt and lastRunAt. Defaults to time.Now.
	Now func() time.Time

	Logger *slog.Logger
}

// Store is the DynamoDB-backed repository.
type Store struct {
	db    DynamoAPI
	table string
	cal   model.Calendar
	now   func() time.Time
	log   *slog.Logger
}

// New validates cfg and returns a Store.
func New(cfg Config) (*Store, error) {
	if cfg.Client == nil {
		return nil, errors.New("store: a DynamoDB client is required")
	}
	if cfg.TableName == "" {
		return nil, errors.New("store: a table name is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Store{
		db:    cfg.Client,
		table: cfg.TableName,
		cal:   cfg.Calendar,
		now:   now,
		log:   log,
	}, nil
}

// TableName reports the table this store writes to.
func (s *Store) TableName() string { return s.table }

// Calendar reports the calendar period keys are derived in.
func (s *Store) Calendar() model.Calendar { return s.cal }

// key builds a primary key map.
func key(pk, sk string) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		AttrPK: &ddbtypes.AttributeValueMemberS{Value: pk},
		AttrSK: &ddbtypes.AttributeValueMemberS{Value: sk},
	}
}

// VerifyConfig reconciles this process's configuration with what the table records.
//
// On an empty table it writes the config row. On a populated one it fails with
// ErrConfigMismatch if the timezone or schema version differs.
//
// This exists because the timezone is a runtime setting. Changing it silently would mean
// new plays are filed under period keys derived from a different calendar than the
// existing history -- an inconsistency no query could detect and no reconcile could
// repair, since the raw plays carry no record of which zone was configured when they were
// aggregated. Turning that into a startup failure is the whole point.
func (s *Store) VerifyConfig(ctx context.Context) error {
	const op = "VerifyConfig"

	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            key(PKState, SKConfig),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return classify(op, PKState, SKConfig, err)
	}

	want := configItem{
		PK: PKState, SK: SKConfig, Type: itemTypeState,
		Timezone:      s.cal.Name(),
		SchemaVersion: model.SchemaVersion,
		WrittenAt:     model.FormatTS(s.now()),
	}

	if out.Item == nil {
		av, merr := attributevalue.MarshalMap(want)
		if merr != nil {
			return fmt.Errorf("store: marshal config: %w", merr)
		}
		// Conditional so two processes racing on a fresh table cannot both claim it; the
		// loser re-reads on its next call.
		_, perr := s.db.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:           aws.String(s.table),
			Item:                av,
			ConditionExpression: aws.String("attribute_not_exists(#pk)"),
			ExpressionAttributeNames: map[string]string{
				"#pk": AttrPK,
			},
		})
		if perr != nil {
			cerr := classify(op, PKState, SKConfig, perr)
			if errors.Is(cerr, ErrAlreadyExists) {
				// Another process wrote it first; validate against theirs.
				return s.VerifyConfig(ctx)
			}
			return cerr
		}
		s.log.InfoContext(ctx, "store: initialised configuration row",
			"timezone", want.Timezone, "schemaVersion", want.SchemaVersion)
		return nil
	}

	var got configItem
	if err := attributevalue.UnmarshalMap(out.Item, &got); err != nil {
		return fmt.Errorf("store: unmarshal config: %w", err)
	}

	if got.Timezone != want.Timezone {
		return fmt.Errorf("%w: table was written with timezone %q, this process is configured for %q",
			ErrConfigMismatch, got.Timezone, want.Timezone)
	}
	if got.SchemaVersion != want.SchemaVersion {
		return fmt.Errorf("%w: table uses schema version %d, this build writes version %d",
			ErrConfigMismatch, got.SchemaVersion, want.SchemaVersion)
	}
	return nil
}
