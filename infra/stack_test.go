package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"

	"github.com/neovasili/spotistats/internal/metrics"
	"github.com/neovasili/spotistats/internal/store"
)

// synth builds the stack in-memory and returns its template. No AWS credentials, no network.
func synth(t *testing.T, cfg StackConfig) assertions.Template {
	t.Helper()
	if cfg.LambdaAssetDir == "" {
		cfg.LambdaAssetDir = fakeAssetDir(t)
	}
	app := awscdk.NewApp(&awscdk.AppProps{
		// An explicit outdir keeps the assertion runs out of the real cdk.out.
		Outdir: jsii.String(t.TempDir()),
	})
	stack := NewSpotistatsStack(app, "TestStack", &SpotistatsStackProps{Config: cfg})
	return assertions.Template_FromStack(stack.Stack, nil)
}

// fakeAssetDir creates a directory tree shaped like `make lambdas` output, so the stack can
// be synthesised without building the real Lambda binaries. These tests assert the shape of
// the template, not the contents of the executable.
func fakeAssetDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, fn := range []string{"capture"} {
		dir := filepath.Join(root, fn)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bootstrap"), []byte("#!/bin/true\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func testConfig() StackConfig {
	return StackConfig{
		Account: "111122223333", Region: "us-east-1",
		TableName: "spotistats-test", Timezone: "Europe/Madrid",
		AlarmEmail: "ops@example.com", SSMPrefix: "/spotistats/spotify",
		MonthlyBudgetUSD: 10, CaptureRateHours: 2,
	}
}

// TestTableSchemaParity is the reason infra/ lives in the same Go module as the application.
//
// The integration tests create their tables from store.Schema. If the CDK stack drifted from
// it, every test could pass against a shape production does not have -- a GSI1 projection
// missing an attribute, for instance, silently returns zero values rather than an error. This
// test turns that drift into a failed build.
func TestTableSchemaParity(t *testing.T) {
	tpl := synth(t, testConfig())

	tables := tpl.FindResources(jsii.String("AWS::DynamoDB::Table"), nil)
	if len(*tables) != 1 {
		t.Fatalf("found %d tables, want exactly 1", len(*tables))
	}

	var props struct {
		KeySchema []struct {
			AttributeName string `json:"AttributeName"`
			KeyType       string `json:"KeyType"`
		} `json:"KeySchema"`
		AttributeDefinitions []struct {
			AttributeName string `json:"AttributeName"`
			AttributeType string `json:"AttributeType"`
		} `json:"AttributeDefinitions"`
		GlobalSecondaryIndexes []struct {
			IndexName string `json:"IndexName"`
			KeySchema []struct {
				AttributeName string `json:"AttributeName"`
				KeyType       string `json:"KeyType"`
			} `json:"KeySchema"`
			Projection struct {
				ProjectionType   string   `json:"ProjectionType"`
				NonKeyAttributes []string `json:"NonKeyAttributes"`
			} `json:"Projection"`
		} `json:"GlobalSecondaryIndexes"`
	}
	for _, res := range *tables {
		m := *res
		raw, err := json.Marshal(m["Properties"])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &props); err != nil {
			t.Fatal(err)
		}
	}

	// Primary key.
	if len(props.KeySchema) != 2 {
		t.Fatalf("key schema has %d elements, want 2", len(props.KeySchema))
	}
	if got, want := props.KeySchema[0].AttributeName, store.Schema.PartitionKey; got != want {
		t.Errorf("partition key = %q, want store.Schema.PartitionKey (%q)", got, want)
	}
	if props.KeySchema[0].KeyType != "HASH" {
		t.Errorf("first key type = %q, want HASH", props.KeySchema[0].KeyType)
	}
	if got, want := props.KeySchema[1].AttributeName, store.Schema.SortKey; got != want {
		t.Errorf("sort key = %q, want store.Schema.SortKey (%q)", got, want)
	}
	if props.KeySchema[1].KeyType != "RANGE" {
		t.Errorf("second key type = %q, want RANGE", props.KeySchema[1].KeyType)
	}

	// Every key attribute must be declared, and as a string.
	declared := map[string]string{}
	for _, a := range props.AttributeDefinitions {
		declared[a.AttributeName] = a.AttributeType
	}
	wantAttrs := []string{store.Schema.PartitionKey, store.Schema.SortKey}
	for _, idx := range store.Schema.Indexes {
		wantAttrs = append(wantAttrs, idx.PartitionKey, idx.SortKey)
	}
	for _, name := range wantAttrs {
		typ, ok := declared[name]
		if !ok {
			t.Errorf("attribute %q is used as a key but not declared", name)
			continue
		}
		if typ != "S" {
			t.Errorf("attribute %q type = %q, want S -- every Spotistats key is a string", name, typ)
		}
	}

	// Indexes.
	if len(props.GlobalSecondaryIndexes) != len(store.Schema.Indexes) {
		t.Fatalf("template has %d GSIs, store.Schema declares %d",
			len(props.GlobalSecondaryIndexes), len(store.Schema.Indexes))
	}
	byName := map[string]int{}
	for i, g := range props.GlobalSecondaryIndexes {
		byName[g.IndexName] = i
	}
	for _, want := range store.Schema.Indexes {
		i, ok := byName[want.Name]
		if !ok {
			t.Errorf("index %q is declared in store.Schema but absent from the template", want.Name)
			continue
		}
		got := props.GlobalSecondaryIndexes[i]
		if len(got.KeySchema) != 2 {
			t.Errorf("index %q key schema has %d elements, want 2", want.Name, len(got.KeySchema))
			continue
		}
		if got.KeySchema[0].AttributeName != want.PartitionKey {
			t.Errorf("index %q partition key = %q, want %q",
				want.Name, got.KeySchema[0].AttributeName, want.PartitionKey)
		}
		if got.KeySchema[1].AttributeName != want.SortKey {
			t.Errorf("index %q sort key = %q, want %q",
				want.Name, got.KeySchema[1].AttributeName, want.SortKey)
		}

		// The projection is the subtle part: reading an attribute outside it yields a zero
		// value rather than an error, so a mismatch is silent in production.
		if len(want.Projected) == 0 {
			if got.Projection.ProjectionType != "KEYS_ONLY" {
				t.Errorf("index %q projection = %q, want KEYS_ONLY",
					want.Name, got.Projection.ProjectionType)
			}
			continue
		}
		if got.Projection.ProjectionType != "INCLUDE" {
			t.Errorf("index %q projection = %q, want INCLUDE",
				want.Name, got.Projection.ProjectionType)
		}
		gotSet := map[string]bool{}
		for _, a := range got.Projection.NonKeyAttributes {
			gotSet[a] = true
		}
		for _, a := range want.Projected {
			if !gotSet[a] {
				t.Errorf("index %q does not project %q; reads of it would silently return zero",
					want.Name, a)
			}
			delete(gotSet, a)
		}
		for extra := range gotSet {
			t.Errorf("index %q projects %q, which store.Schema does not declare", want.Name, extra)
		}
	}
}

// Years of listening history are irreplaceable -- the API cannot re-serve them -- so a
// `cdk destroy` must not be able to delete the table.
func TestTableIsRetainedAndRecoverable(t *testing.T) {
	tpl := synth(t, testConfig())
	tables := tpl.FindResources(jsii.String("AWS::DynamoDB::Table"), nil)

	for id, res := range *tables {
		m := *res
		if got := m["DeletionPolicy"]; got != "Retain" {
			t.Errorf("%s DeletionPolicy = %v, want Retain", id, got)
		}
		if got := m["UpdateReplacePolicy"]; got != "Retain" {
			t.Errorf("%s UpdateReplacePolicy = %v, want Retain", id, got)
		}
		props := m["Properties"].(map[string]any)
		pitr, ok := props["PointInTimeRecoverySpecification"].(map[string]any)
		if !ok || pitr["PointInTimeRecoveryEnabled"] != true {
			t.Errorf("%s has no point-in-time recovery; a bad backfill would be unrecoverable", id)
		}
		if props["BillingMode"] != "PAY_PER_REQUEST" {
			t.Errorf("%s BillingMode = %v, want PAY_PER_REQUEST", id, props["BillingMode"])
		}
	}
}

func TestCaptureFunctionRuntime(t *testing.T) {
	tpl := synth(t, testConfig())
	tpl.HasResourceProperties(jsii.String("AWS::Lambda::Function"), map[string]any{
		// A Go binary named bootstrap on the custom runtime; arm64 is cheaper per ms.
		"Runtime":       "provided.al2023",
		"Handler":       "bootstrap",
		"Architectures": []any{"arm64"},
		// One concurrent run: two racing captures would both be correct but would waste the
		// scarce development-mode rate limit.
		"ReservedConcurrentExecutions": 1,
		"Timeout":                      120,
	})
}

// SPOTISTATS_REGION must NOT be set: the Lambda runtime always provides AWS_REGION, and
// pinning a region here could disagree with where the function actually runs.
func TestCaptureFunctionEnvironment(t *testing.T) {
	tpl := synth(t, testConfig())
	fns := tpl.FindResources(jsii.String("AWS::Lambda::Function"), nil)

	for id, res := range *fns {
		props := (*res)["Properties"].(map[string]any)
		envBlock, ok := props["Environment"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no Environment block", id)
		}
		vars := envBlock["Variables"].(map[string]any)

		if vars["SPOTISTATS_TABLE_NAME"] != "spotistats-test" {
			t.Errorf("table name = %v", vars["SPOTISTATS_TABLE_NAME"])
		}
		if vars["SPOTISTATS_TIMEZONE"] != "Europe/Madrid" {
			t.Errorf("timezone = %v", vars["SPOTISTATS_TIMEZONE"])
		}
		if _, present := vars["SPOTISTATS_REGION"]; present {
			t.Error("SPOTISTATS_REGION is set; it must be inherited from the runtime's AWS_REGION")
		}
		// The secret itself must never reach the template.
		for k, v := range vars {
			if s, ok := v.(string); ok && (s == "") {
				continue
			}
			if k == "SPOTISTATS_CLIENT_SECRET" || k == "SPOTISTATS_CLIENT_ID" {
				t.Errorf("%s is baked into the template; credentials must be read from SSM", k)
			}
		}
	}
}

// The capture schedule must not be nightly: recently-played retains ~50 plays and cannot
// page back, so a nightly run silently loses data on any heavy day.
func TestCaptureScheduleIsSubDaily(t *testing.T) {
	tpl := synth(t, testConfig())
	tpl.HasResourceProperties(jsii.String("AWS::Events::Rule"), map[string]any{
		"ScheduleExpression": "rate(2 hours)",
		"State":              "ENABLED",
	})

	// And a custom rate flows through.
	cfg := testConfig()
	cfg.CaptureRateHours = 1
	synth(t, cfg).HasResourceProperties(jsii.String("AWS::Events::Rule"), map[string]any{
		"ScheduleExpression": "rate(1 hour)",
	})
}

// A failed run needs no retry: the next scheduled run re-reads the same window and ingestion
// is idempotent. Retrying would only burn rate limit.
func TestScheduleDoesNotRetry(t *testing.T) {
	tpl := synth(t, testConfig())
	rules := tpl.FindResources(jsii.String("AWS::Events::Rule"), nil)
	for id, res := range *rules {
		props := (*res)["Properties"].(map[string]any)
		targets := props["Targets"].([]any)
		for _, tg := range targets {
			rp, ok := tg.(map[string]any)["RetryPolicy"].(map[string]any)
			if !ok {
				t.Errorf("%s target has no RetryPolicy", id)
				continue
			}
			if rp["MaximumRetryAttempts"] != float64(0) {
				t.Errorf("%s MaximumRetryAttempts = %v, want 0", id, rp["MaximumRetryAttempts"])
			}
		}
	}
}

// Every alarm the code can emit a metric for must exist, with a unique name, and each must
// carry a description an operator can act on.
func TestAlarmsCoverEveryMetric(t *testing.T) {
	tpl := synth(t, testConfig())
	alarms := tpl.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)

	names := map[string]bool{}
	byMetric := map[string]map[string]any{}
	for id, res := range *alarms {
		props := (*res)["Properties"].(map[string]any)
		name, _ := props["AlarmName"].(string)
		if names[name] {
			t.Errorf("alarm name %q is used more than once (resource %s)", name, id)
		}
		names[name] = true

		if desc, _ := props["AlarmDescription"].(string); len(desc) < 20 {
			t.Errorf("alarm %q has no actionable description", name)
		}
		if props["AlarmActions"] == nil {
			t.Errorf("alarm %q has no action, so nobody is notified", name)
		}
		if m, ok := props["MetricName"].(string); ok {
			byMetric[m] = props
		}
	}

	for _, want := range []string{
		metrics.CaptureRun,
		metrics.PlaysGapDetected,
		metrics.TokenRefreshFailed,
		metrics.GenresDegraded,
	} {
		if _, ok := byMetric[want]; !ok {
			t.Errorf("no alarm watches the %q metric", want)
		}
	}
	// Lambda's own error metric.
	if _, ok := byMetric["Errors"]; !ok {
		t.Error("no alarm watches the Lambda Errors metric")
	}

	// CaptureStale alarms on the ABSENCE of runs, so missing data must breach and the
	// comparison must be inverted. Getting either wrong makes it never fire.
	stale := byMetric[metrics.CaptureRun]
	if stale["TreatMissingData"] != "breaching" {
		t.Errorf("CaptureStale TreatMissingData = %v, want breaching -- no data IS the failure",
			stale["TreatMissingData"])
	}
	if stale["ComparisonOperator"] != "LessThanThreshold" {
		t.Errorf("CaptureStale operator = %v, want LessThanThreshold", stale["ComparisonOperator"])
	}
	// The others must not breach on missing data, or they would fire whenever nothing happened.
	for _, m := range []string{metrics.PlaysGapDetected, metrics.TokenRefreshFailed, metrics.GenresDegraded} {
		if got := byMetric[m]["TreatMissingData"]; got != "notBreaching" {
			t.Errorf("%s TreatMissingData = %v, want notBreaching", m, got)
		}
	}
}

func TestAlarmsUseTheApplicationNamespace(t *testing.T) {
	tpl := synth(t, testConfig())
	alarms := tpl.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	for _, res := range *alarms {
		props := (*res)["Properties"].(map[string]any)
		ns, ok := props["Namespace"].(string)
		if !ok {
			continue // Lambda's own metrics use AWS/Lambda.
		}
		if ns != metrics.Namespace && ns != "AWS/Lambda" {
			t.Errorf("alarm namespace = %q, want %q or AWS/Lambda", ns, metrics.Namespace)
		}
	}
}

// Synth must work before the operator has chosen an alarm address, so the topic is created
// but left unsubscribed.
func TestAlarmTopicUnsubscribedWithoutEmail(t *testing.T) {
	cfg := testConfig()
	cfg.AlarmEmail = ""
	tpl := synth(t, cfg)

	tpl.ResourceCountIs(jsii.String("AWS::SNS::Topic"), jsii.Number(1))
	tpl.ResourceCountIs(jsii.String("AWS::SNS::Subscription"), jsii.Number(0))
	// The budget needs a destination, so it is skipped too rather than created uselessly.
	tpl.ResourceCountIs(jsii.String("AWS::Budgets::Budget"), jsii.Number(0))
}

func TestAlarmTopicSubscribedWithEmail(t *testing.T) {
	tpl := synth(t, testConfig())
	tpl.ResourceCountIs(jsii.String("AWS::SNS::Subscription"), jsii.Number(1))
	tpl.HasResourceProperties(jsii.String("AWS::SNS::Subscription"), map[string]any{
		"Protocol": "email",
		"Endpoint": "ops@example.com",
	})
	tpl.ResourceCountIs(jsii.String("AWS::Budgets::Budget"), jsii.Number(1))
}

// The public API is unauthenticated and there is no WAF by design, so the budget is the last
// line of defence against runaway cost.
func TestBudgetThreshold(t *testing.T) {
	tpl := synth(t, testConfig())
	tpl.HasResourceProperties(jsii.String("AWS::Budgets::Budget"), map[string]any{
		"Budget": map[string]any{
			"BudgetType":  "COST",
			"TimeUnit":    "MONTHLY",
			"BudgetLimit": map[string]any{"Amount": 10, "Unit": "USD"},
		},
	})
}

// The Spotify credentials must never appear in the template: CloudFormation retains template
// bodies, so a secret placed there is effectively published.
func TestNoSecretsInTemplate(t *testing.T) {
	tpl := synth(t, testConfig())
	raw, err := json.Marshal(tpl.ToJSON())
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, forbidden := range []string{"client_secret\":\"", "refresh_token\":\"", "AQC", "BQD"} {
		if idx := indexOf(body, forbidden); idx >= 0 {
			t.Errorf("template appears to contain a secret near %q", forbidden)
		}
	}
	// It must reference the SSM path instead.
	if indexOf(body, "/spotistats/spotify") < 0 {
		t.Error("template does not reference the SSM credential path")
	}
}

// The capture role must not be able to Scan or Delete: it never does either, and Scan on the
// play table would be expensive enough to matter.
func TestCaptureRoleIsLeastPrivilege(t *testing.T) {
	tpl := synth(t, testConfig())
	policies := tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil)

	for id, res := range *policies {
		props := (*res)["Properties"].(map[string]any)
		doc := props["PolicyDocument"].(map[string]any)
		for _, st := range doc["Statement"].([]any) {
			stmt := st.(map[string]any)
			actions, _ := stmt["Action"].([]any)
			for _, a := range actions {
				switch a {
				case "dynamodb:Scan":
					t.Errorf("%s grants dynamodb:Scan, which capture never performs", id)
				case "dynamodb:DeleteItem":
					t.Errorf("%s grants dynamodb:DeleteItem, which capture never performs", id)
				case "ssm:*", "dynamodb:*":
					t.Errorf("%s grants the wildcard action %v", id, a)
				}
			}
		}
	}
}

// The refresh token is WRITTEN as well as read: Spotify may rotate it on any refresh, and
// losing the replacement means no future run can authenticate.
func TestCaptureCanWriteTheRefreshToken(t *testing.T) {
	tpl := synth(t, testConfig())
	policies := tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil)

	found := false
	for _, res := range *policies {
		props := (*res)["Properties"].(map[string]any)
		doc := props["PolicyDocument"].(map[string]any)
		for _, st := range doc["Statement"].([]any) {
			for _, a := range st.(map[string]any)["Action"].([]any) {
				if a == "ssm:PutParameter" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("no ssm:PutParameter grant; a rotated refresh token could never be persisted")
	}
}

func TestLogGroupHasRetention(t *testing.T) {
	tpl := synth(t, testConfig())
	// Unbounded log retention is a slow cost leak; the durable record lives in DynamoDB.
	tpl.HasResourceProperties(jsii.String("AWS::Logs::LogGroup"), map[string]any{
		"RetentionInDays": 14,
	})
}

func TestStackConfigValidation(t *testing.T) {
	app := awscdk.NewApp(&awscdk.AppProps{
		Outdir:  jsii.String(t.TempDir()),
		Context: &map[string]interface{}{"lambdaAssetDir": fakeAssetDir(t)},
	})
	cfg, err := stackConfigFromContext(app)
	if err != nil {
		t.Fatalf("defaults should be valid: %v", err)
	}
	// The region is a design constraint (CloudFront needs its certificate in us-east-1), so
	// it must not be inherited from the operator's shell.
	if cfg.Region != defaultRegion {
		t.Errorf("default region = %q, want %q", cfg.Region, defaultRegion)
	}
	if cfg.CaptureRateHours != defaultCaptureRateHours {
		t.Errorf("default capture rate = %v, want %v", cfg.CaptureRateHours, defaultCaptureRateHours)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
