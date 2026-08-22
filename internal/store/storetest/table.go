package storetest

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/store"
)

// DefaultTimezone is the zone the harness configures, matching production.
const DefaultTimezone = "Europe/Madrid"

// FixedNow is the clock every harness-built store reports, so refreshedAt and lastRunAt
// stamps are deterministic.
var FixedNow = time.Date(2025, 3, 14, 21, 4, 33, 123_000_000, time.UTC)

var tableNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// CreateTable creates a table matching the production schema and registers cleanup.
//
// The definition is derived from store.Schema rather than written out here, so a test can
// never exercise a shape the CDK stack does not provision. Creating a fresh table per test
// costs about 20ms against in-memory DynamoDB Local and buys real isolation -- sharing one
// table with key prefixes would make any Query-based assertion unsafe.
func CreateTable(t *testing.T, c *dynamodb.Client) string {
	t.Helper()
	ctx := context.Background()

	name := tableNameUnsafe.ReplaceAllString(t.Name(), "_")
	if len(name) > 200 {
		name = name[:200]
	}
	name = "spotistats_" + name

	in := store.CreateTableInput(name)

	if _, err := c.CreateTable(ctx, in); err != nil {
		// A leftover table from an interrupted run is reusable; anything else is fatal.
		if !strings.Contains(err.Error(), "ResourceInUseException") {
			t.Fatalf("storetest: create table %s: %v", name, err)
		}
	}

	t.Cleanup(func() {
		_, err := c.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{
			TableName: aws.String(name),
		})
		if err != nil {
			t.Logf("storetest: delete table %s: %v", name, err)
		}
	})

	return name
}

// Option customises a harness-built Store.
type Option func(*store.Config)

// WithTimezone overrides the calendar zone.
func WithTimezone(tz string) Option {
	return func(c *store.Config) { c.Calendar = model.MustCalendar(tz) }
}

// WithNow overrides the store clock.
func WithNow(now time.Time) Option {
	return func(c *store.Config) { c.Now = func() time.Time { return now } }
}

// WithLogger attaches a logger, for debugging a single test.
func WithLogger(l *slog.Logger) Option {
	return func(c *store.Config) { c.Logger = l }
}

// NewStore is the one-liner most integration tests want: a container, a fresh table, and a
// configured Store with a fixed clock and the production timezone.
func NewStore(t *testing.T, opts ...Option) *store.Store {
	t.Helper()
	c := RequireDynamoDB(t)
	table := CreateTable(t, c)

	cfg := store.Config{
		Client:    c,
		TableName: table,
		Calendar:  model.MustCalendar(DefaultTimezone),
		Now:       func() time.Time { return FixedNow },
	}
	for _, o := range opts {
		o(&cfg)
	}

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("storetest: new store: %v", err)
	}
	return s
}

// NewStoreOnTable builds a second Store over an existing table, for tests that need two
// differently-configured processes against the same data (the VerifyConfig mismatch case).
func NewStoreOnTable(t *testing.T, c *dynamodb.Client, table string, opts ...Option) *store.Store {
	t.Helper()
	cfg := store.Config{
		Client:    c,
		TableName: table,
		Calendar:  model.MustCalendar(DefaultTimezone),
		Now:       func() time.Time { return FixedNow },
	}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("storetest: new store: %v", err)
	}
	return s
}
