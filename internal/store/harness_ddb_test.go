package store_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/neovasili/spotistats/internal/store"
	"github.com/neovasili/spotistats/internal/store/storetest"
)

// TestHarnessCreatesProductionShapedTable proves the harness builds what store.Schema
// declares, so no integration test can pass against a shape production does not have.
func TestHarnessCreatesProductionShapedTable(t *testing.T) {
	c := storetest.RequireDynamoDB(t)
	table := storetest.CreateTable(t, c)

	out, err := c.DescribeTable(context.Background(), &dynamodb.DescribeTableInput{
		TableName: aws.String(table),
	})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	d := out.Table

	if len(d.KeySchema) != 2 {
		t.Fatalf("key schema = %d elements, want 2", len(d.KeySchema))
	}
	if got := aws.ToString(d.KeySchema[0].AttributeName); got != store.AttrPK {
		t.Errorf("hash key = %q, want %q", got, store.AttrPK)
	}
	if got := aws.ToString(d.KeySchema[1].AttributeName); got != store.AttrSK {
		t.Errorf("range key = %q, want %q", got, store.AttrSK)
	}

	if len(d.GlobalSecondaryIndexes) != 1 {
		t.Fatalf("GSIs = %d, want 1", len(d.GlobalSecondaryIndexes))
	}
	gsi := d.GlobalSecondaryIndexes[0]
	if got := aws.ToString(gsi.IndexName); got != store.IndexGSI1 {
		t.Errorf("index name = %q, want %q", got, store.IndexGSI1)
	}
	if got := aws.ToString(gsi.KeySchema[0].AttributeName); got != store.AttrGSI1PK {
		t.Errorf("GSI1 hash key = %q", got)
	}
	// The INCLUDE projection is load-bearing: reading an unprojected attribute from a
	// GSI1 query silently yields zero.
	if gsi.Projection == nil || len(gsi.Projection.NonKeyAttributes) == 0 {
		t.Fatal("GSI1 has no INCLUDE projection")
	}
	want := map[string]bool{}
	for _, a := range store.GSI1ProjectedAttributes {
		want[a] = true
	}
	for _, a := range gsi.Projection.NonKeyAttributes {
		delete(want, a)
	}
	if len(want) != 0 {
		t.Errorf("GSI1 projection is missing %v", want)
	}
}

// TestHarnessNeedsNoAWSCredentials documents the property that lets this suite run on a
// machine whose real AWS token is expired: the client is built from static throwaway
// credentials and an explicit endpoint, never from the ambient config chain.
func TestHarnessNeedsNoAWSCredentials(t *testing.T) {
	t.Setenv("AWS_PROFILE", "definitely-not-a-real-profile")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")

	s := storetest.NewStore(t)
	if err := s.VerifyConfig(context.Background()); err != nil {
		t.Fatalf("VerifyConfig with a bogus AWS_PROFILE: %v", err)
	}
}
