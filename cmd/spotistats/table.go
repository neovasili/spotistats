package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/store"
)

// runInitTable creates the DynamoDB table.
//
// In production the table is provisioned by CDK; this exists for local development against
// DynamoDB Local, where there is no CloudFormation. It builds the request from
// store.CreateTableInput -- the same declaration the CDK stack and the test harness use --
// so a locally created table cannot differ from the deployed one.
func runInitTable(ctx context.Context, args []string) error {
	fs := newFlagSet("init-table", "init-table [flags]")
	wait := fs.Duration("wait", 30*time.Second, "how long to wait for the table to become ACTIVE")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		return err
	}

	// Refuse to touch a real account by accident: this is a local-development helper, and in
	// production the table is CDK's to own.
	if cfg.DDBEndpoint == "" {
		return fmt.Errorf("init-table requires %s to be set (it is a local-development "+
			"helper). In AWS the table is provisioned by CDK: run `make deploy`",
			config.EnvDDBEndpoint)
	}

	client, err := cfg.DynamoClient(ctx)
	if err != nil {
		return err
	}

	heading("Creating table %s", cfg.TableName)
	bullet("endpoint: %s", cfg.DDBEndpoint)

	if _, err := client.CreateTable(ctx, store.CreateTableInput(cfg.TableName)); err != nil {
		var inUse *ddbtypes.ResourceInUseException
		if errors.As(err, &inUse) {
			bullet("already exists, nothing to do")
			return nil
		}
		return fmt.Errorf("create table %s: %w", cfg.TableName, err)
	}

	waiter := dynamodb.NewTableExistsWaiter(client)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(cfg.TableName),
	}, *wait); err != nil {
		return fmt.Errorf("wait for table %s: %w", cfg.TableName, err)
	}

	desc, err := client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(cfg.TableName),
	})
	if err != nil {
		return err
	}
	bullet("created, status %s", desc.Table.TableStatus)
	for _, idx := range desc.Table.GlobalSecondaryIndexes {
		bullet("index %s: %s", aws.ToString(idx.IndexName), idx.Projection.ProjectionType)
	}
	fmt.Printf("\nNext:  %s auth login  &&  %s poll\n", progName, progName)
	return nil
}
