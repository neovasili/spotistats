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

// synthBoth builds both stacks in-memory and returns their templates. No AWS credentials, no
// network.
func synthBoth(t *testing.T, cfg StackConfig) (regional, global assertions.Template) {
	t.Helper()
	if cfg.LambdaAssetDir == "" {
		cfg.LambdaAssetDir = fakeAssetDir(t)
	}
	app := awscdk.NewApp(&awscdk.AppProps{
		// An explicit outdir keeps the assertion runs out of the real cdk.out.
		Outdir: jsii.String(t.TempDir()),
	})

	g := NewGlobalStack(app, "TestGlobalStack", &GlobalStackProps{
		StackProps: awscdk.StackProps{
			Env:                   cfg.globalEnv(),
			CrossRegionReferences: jsii.Bool(true),
		},
		Config: cfg,
	})
	r := NewSpotistatsStack(app, "TestStack", &SpotistatsStackProps{
		StackProps: awscdk.StackProps{
			Env:                   cfg.env(),
			CrossRegionReferences: jsii.Bool(true),
		},
		Config:      cfg,
		Certificate: g.Certificate,
	})
	return assertions.Template_FromStack(r.Stack, nil), assertions.Template_FromStack(g.Stack, nil)
}

// synth returns the regional template, which is what most assertions are about.
func synth(t *testing.T, cfg StackConfig) assertions.Template {
	t.Helper()
	regional, _ := synthBoth(t, cfg)
	return regional
}

// synthGlobal returns the us-east-1 template.
func synthGlobal(t *testing.T, cfg StackConfig) assertions.Template {
	t.Helper()
	_, global := synthBoth(t, cfg)
	return global
}

// fakeAssetDir creates a directory tree shaped like `make lambdas` output, so the stack can
// be synthesised without building the real Lambda binaries. These tests assert the shape of
// the template, not the contents of the executable.
func fakeAssetDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, fn := range lambdaFunctions {
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
		Account: "111122223333", Region: "eu-west-1",
		TableName: "spotistats-test", Timezone: "Europe/Madrid",
		AlarmEmail: "ops@example.com", SSMPrefix: "/spotistats/spotify",
		MonthlyBudgetUSD: 10, CaptureRateHours: 2,
		// A delegated subdomain zone: the domain IS the zone, so the alias record belongs at
		// its apex.
		DomainName:     "spotistats.neovasili.com",
		HostedZoneName: "spotistats.neovasili.com",
		HostedZoneID:   "Z08622643JXD4FF65E2XP",
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
		"Timeout":       120,
	})
}

// TestNoReservedConcurrencyByDefault guards a real deployment failure.
//
// AWS requires at least 10 unreserved concurrency to remain available account-wide, so on the
// new-account default quota of 10 ANY reservation is rejected -- the first deploy failed with
// exactly that. Reservations are therefore opt-in, and must stay so, or the stack becomes
// undeployable on a fresh account.
func TestNoReservedConcurrencyByDefault(t *testing.T) {
	tpl := synth(t, testConfig())
	fns := tpl.FindResources(jsii.String("AWS::Lambda::Function"), nil)

	checked := 0
	for id, res := range *fns {
		props := (*res)["Properties"].(map[string]any)
		name, _ := props["FunctionName"].(string)
		if name != "spotistats-capture" && name != "spotistats-query" {
			continue
		}
		checked++
		if v, present := props["ReservedConcurrentExecutions"]; present {
			t.Errorf("%s reserves %v concurrency by default; on an account with the default "+
				"quota of 10 this makes the stack undeployable", id, v)
		}
	}
	if checked != 2 {
		t.Fatalf("checked %d application functions, want 2", checked)
	}
}

// Once the account quota is raised, the reservations must actually take effect.
func TestReservedConcurrencyAppliesWhenConfigured(t *testing.T) {
	cfg := testConfig()
	cfg.CaptureReservedConcurrency = 1
	cfg.QueryReservedConcurrency = 10
	tpl := synth(t, cfg)

	want := map[string]float64{"spotistats-capture": 1, "spotistats-query": 10}
	seen := map[string]float64{}
	for _, res := range *tpl.FindResources(jsii.String("AWS::Lambda::Function"), nil) {
		props := (*res)["Properties"].(map[string]any)
		name, _ := props["FunctionName"].(string)
		if _, wanted := want[name]; !wanted {
			continue
		}
		v, present := props["ReservedConcurrentExecutions"]
		if !present {
			t.Errorf("%s has no reservation despite being configured", name)
			continue
		}
		seen[name] = v.(float64)
	}
	for name, w := range want {
		if seen[name] != w {
			t.Errorf("%s reserved = %v, want %v", name, seen[name], w)
		}
	}
}

// SPOTISTATS_REGION must NOT be set: the Lambda runtime always provides AWS_REGION, and
// pinning a region here could disagree with where the function actually runs.
func TestCaptureFunctionEnvironment(t *testing.T) {
	tpl := synth(t, testConfig())
	fns := tpl.FindResources(jsii.String("AWS::Lambda::Function"), nil)

	checked := false
	for id, res := range *fns {
		props := (*res)["Properties"].(map[string]any)
		// Skip CDK-managed helper functions (the bucket's auto-delete custom resource); only
		// the application's own functions are configured here.
		if name, _ := props["FunctionName"].(string); name != "spotistats-capture" {
			continue
		}
		checked = true
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
	if !checked {
		t.Fatal("no capture function found")
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

// The CAPTURE schedule needs no retry: the next scheduled run re-reads the same window and
// ingestion is idempotent, so retrying would only burn the scarce Spotify rate limit. The
// rollup schedule deliberately differs -- see TestRollupSchedule.
func TestCaptureScheduleDoesNotRetry(t *testing.T) {
	tpl := synth(t, testConfig())
	rules := tpl.FindResources(jsii.String("AWS::Events::Rule"), nil)

	checked := false
	for id, res := range *rules {
		props := (*res)["Properties"].(map[string]any)
		// Only the rate-based capture rule; the rollup uses a cron expression.
		expr, _ := props["ScheduleExpression"].(string)
		if indexOf(expr, "rate(") < 0 {
			continue
		}
		checked = true
		for _, tg := range props["Targets"].([]any) {
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
	if !checked {
		t.Fatal("no rate-based capture schedule found")
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
	regional, global := synthBoth(t, cfg)

	regional.ResourceCountIs(jsii.String("AWS::SNS::Topic"), jsii.Number(1))
	regional.ResourceCountIs(jsii.String("AWS::SNS::Subscription"), jsii.Number(0))
	// The budget needs a destination, so it is skipped rather than created uselessly.
	global.ResourceCountIs(jsii.String("AWS::Budgets::Budget"), jsii.Number(0))
}

func TestAlarmTopicSubscribedWithEmail(t *testing.T) {
	regional, global := synthBoth(t, testConfig())
	regional.ResourceCountIs(jsii.String("AWS::SNS::Subscription"), jsii.Number(1))
	regional.HasResourceProperties(jsii.String("AWS::SNS::Subscription"), map[string]any{
		"Protocol": "email",
		"Endpoint": "ops@example.com",
	})
	global.ResourceCountIs(jsii.String("AWS::Budgets::Budget"), jsii.Number(1))
}

// The public API is unauthenticated and there is no WAF by design, so the budget is the last
// line of defence against runaway cost.
func TestBudgetThreshold(t *testing.T) {
	tpl := synthGlobal(t, testConfig())
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
			for _, a := range actionsOf(stmt) {
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
			for _, a := range actionsOf(st.(map[string]any)) {
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
	if defaultRegion == CertRegion {
		t.Error("the deployment region and the certificate region are the same; the two-stack " +
			"split exists precisely because they differ")
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

// ---------------------------------------------------------------------------
// public surface
// ---------------------------------------------------------------------------

// TestQueryFunctionIsReadOnly is the security property that matters most in this stack: the
// query Lambda is the only component reachable from the internet, so it must be incapable of
// mutating anything (docs/SPECS.md 10.1).
func TestQueryFunctionIsReadOnly(t *testing.T) {
	tpl := synth(t, testConfig())
	policies := tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil)

	forbidden := map[string]bool{
		"dynamodb:PutItem": true, "dynamodb:UpdateItem": true, "dynamodb:DeleteItem": true,
		"dynamodb:BatchWriteItem": true, "dynamodb:Scan": true,
		"ssm:PutParameter": true, "ssm:GetParameter": true,
	}

	checked := false
	for id, res := range *policies {
		if !contains(id, "Query") {
			continue
		}
		checked = true
		props := (*res)["Properties"].(map[string]any)
		doc := props["PolicyDocument"].(map[string]any)
		for _, st := range doc["Statement"].([]any) {
			for _, a := range actionsOf(st.(map[string]any)) {
				if forbidden[a] {
					t.Errorf("%s grants %q to the internet-facing query Lambda", id, a)
				}
			}
		}
	}
	if !checked {
		t.Fatal("found no IAM policy for the query Lambda")
	}
}

// TestSpaFallbackIsNotDistributionWide guards a real spec defect.
//
// docs/SPECS.md 9.1 originally specified CloudFront custom error responses for SPA routing,
// "scoped to the S3 behaviours only". That is not implementable: CustomErrorResponses is a
// distribution-level setting, so a 404 from the API would also be rewritten to index.html --
// precisely the confusing failure the spec was trying to prevent. A viewer-request function
// is the correct mechanism, and it must be attached to the site behaviour and nowhere else.
func TestSpaFallbackIsNotDistributionWide(t *testing.T) {
	tpl := synth(t, testConfig())
	dists := tpl.FindResources(jsii.String("AWS::CloudFront::Distribution"), nil)
	if len(*dists) != 1 {
		t.Fatalf("distributions = %d, want 1", len(*dists))
	}

	for id, res := range *dists {
		cfg := (*res)["Properties"].(map[string]any)["DistributionConfig"].(map[string]any)

		// No distribution-wide error rewriting.
		if ce, present := cfg["CustomErrorResponses"]; present {
			t.Errorf("%s sets CustomErrorResponses (%v); an API 404 would be rewritten to "+
				"index.html", id, ce)
		}

		// The site behaviour carries the routing function.
		def := cfg["DefaultCacheBehavior"].(map[string]any)
		fns, _ := def["FunctionAssociations"].([]any)
		if len(fns) != 1 {
			t.Errorf("default behaviour has %d function associations, want 1 for SPA routing",
				len(fns))
		}

		// No other behaviour does, least of all /api/*.
		for _, b := range cfg["CacheBehaviors"].([]any) {
			bh := b.(map[string]any)
			path, _ := bh["PathPattern"].(string)
			assoc, _ := bh["FunctionAssociations"].([]any)
			if len(assoc) != 0 {
				t.Errorf("behaviour %q has a function association; SPA routing must apply to "+
					"the site only", path)
			}
		}
	}
}

func TestDistributionRoutesApiSeparately(t *testing.T) {
	tpl := synth(t, testConfig())
	dists := tpl.FindResources(jsii.String("AWS::CloudFront::Distribution"), nil)

	for _, res := range *dists {
		cfg := (*res)["Properties"].(map[string]any)["DistributionConfig"].(map[string]any)

		paths := map[string]map[string]any{}
		for _, b := range cfg["CacheBehaviors"].([]any) {
			bh := b.(map[string]any)
			paths[bh["PathPattern"].(string)] = bh
		}
		for _, want := range []string{"/api/*", "/data/*", "/assets/*"} {
			if _, ok := paths[want]; !ok {
				t.Errorf("no behaviour for %q", want)
			}
		}

		// The API must not share the site's origin, or /api/* would be served from S3.
		apiOrigin := paths["/api/*"]["TargetOriginId"]
		siteOrigin := cfg["DefaultCacheBehavior"].(map[string]any)["TargetOriginId"]
		if apiOrigin == siteOrigin {
			t.Error("/api/* and the site share an origin")
		}

		// Everything is HTTPS-only and compressed.
		for path, bh := range paths {
			if got := bh["ViewerProtocolPolicy"]; got != "redirect-to-https" {
				t.Errorf("%s ViewerProtocolPolicy = %v", path, got)
			}
			if got := bh["Compress"]; got != true {
				t.Errorf("%s Compress = %v", path, got)
			}
		}
	}
}

// The bucket must be unreachable except through CloudFront.
func TestWebBucketIsPrivate(t *testing.T) {
	tpl := synth(t, testConfig())
	tpl.HasResourceProperties(jsii.String("AWS::S3::Bucket"), map[string]any{
		"PublicAccessBlockConfiguration": map[string]any{
			"BlockPublicAcls":       true,
			"BlockPublicPolicy":     true,
			"IgnorePublicAcls":      true,
			"RestrictPublicBuckets": true,
		},
		"VersioningConfiguration": map[string]any{"Status": "Enabled"},
	})

	// Access is granted to CloudFront via OAC, not to a principal that could be anything else.
	policies := tpl.FindResources(jsii.String("AWS::S3::BucketPolicy"), nil)
	if len(*policies) == 0 {
		t.Fatal("no bucket policy; CloudFront would not be able to read the origin")
	}
	for id, res := range *policies {
		doc := (*res)["Properties"].(map[string]any)["PolicyDocument"].(map[string]any)

		var sawCloudFront bool
		for _, st := range doc["Statement"].([]any) {
			stmt := st.(map[string]any)
			effect, _ := stmt["Effect"].(string)

			raw, err := json.Marshal(stmt)
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)

			// A wildcard principal on a Deny is fine and desirable -- that is the EnforceSSL
			// rule, which denies every non-TLS request. On an Allow it would be a hole.
			if effect == "Allow" && (indexOf(body, `"Principal":"*"`) >= 0 || indexOf(body, `"AWS":"*"`) >= 0) {
				t.Errorf("%s has an Allow statement with a wildcard principal: %s", id, body)
			}

			if indexOf(body, "cloudfront.amazonaws.com") < 0 {
				continue
			}
			sawCloudFront = true

			// Without a SourceArn condition pinning the grant to THIS distribution, any
			// CloudFront distribution in any AWS account could read the bucket. That is the
			// classic OAC misconfiguration.
			cond, ok := stmt["Condition"].(map[string]any)
			if !ok {
				t.Errorf("%s grants CloudFront access with no condition; any distribution in "+
					"any account could read the bucket", id)
				continue
			}
			condRaw, err := json.Marshal(cond)
			if err != nil {
				t.Fatal(err)
			}
			if indexOf(string(condRaw), "AWS:SourceArn") < 0 {
				t.Errorf("%s CloudFront grant is not scoped by AWS:SourceArn: %s", id, condRaw)
			}
			if indexOf(string(condRaw), "distribution/") < 0 {
				t.Errorf("%s SourceArn does not name a specific distribution: %s", id, condRaw)
			}
		}
		if !sawCloudFront {
			t.Errorf("%s does not grant CloudFront read access; the origin would be unreadable", id)
		}
	}
}

// Album art comes from i.scdn.co; omitting it from the CSP breaks every image on the site.
func TestSecurityHeadersAllowSpotifyImages(t *testing.T) {
	tpl := synth(t, testConfig())
	policies := tpl.FindResources(jsii.String("AWS::CloudFront::ResponseHeadersPolicy"), nil)
	if len(*policies) != 1 {
		t.Fatalf("response headers policies = %d, want 1", len(*policies))
	}

	for _, res := range *policies {
		cfg := (*res)["Properties"].(map[string]any)["ResponseHeadersPolicyConfig"].(map[string]any)
		sec := cfg["SecurityHeadersConfig"].(map[string]any)

		csp := sec["ContentSecurityPolicy"].(map[string]any)["ContentSecurityPolicy"].(string)
		if indexOf(csp, "https://i.scdn.co") < 0 {
			t.Error("CSP omits i.scdn.co; every album cover and artist image would be blocked")
		}
		if indexOf(csp, "frame-ancestors 'none'") < 0 {
			t.Error("CSP does not forbid framing")
		}

		hsts := sec["StrictTransportSecurity"].(map[string]any)
		if hsts["IncludeSubdomains"] != true || hsts["Preload"] != true {
			t.Errorf("HSTS = %v, want includeSubdomains and preload", hsts)
		}
		if sec["ContentTypeOptions"] == nil {
			t.Error("X-Content-Type-Options is not set")
		}
		if got := sec["FrameOptions"].(map[string]any)["FrameOption"]; got != "DENY" {
			t.Errorf("X-Frame-Options = %v, want DENY", got)
		}
	}
}

// No CORS anywhere: production is same-origin behind CloudFront and local development uses a
// Vite proxy (docs/SPECS.md 7.4). A CORS header appearing here means the design drifted.
func TestNoCorsConfigurationExists(t *testing.T) {
	tpl := synth(t, testConfig())
	raw, err := json.Marshal(tpl.ToJSON())
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, forbidden := range []string{"CorsConfiguration", "AccessControlAllowOrigin", "access-control-allow-origin"} {
		if indexOf(body, forbidden) >= 0 {
			t.Errorf("template contains %q; the system is same-origin by design", forbidden)
		}
	}
}

// The public API is unauthenticated with no WAF, so stage throttling is the first line of
// defence (docs/SPECS.md 10.3).
func TestApiIsThrottled(t *testing.T) {
	tpl := synth(t, testConfig())
	tpl.HasResourceProperties(jsii.String("AWS::ApiGatewayV2::Stage"), map[string]any{
		"DefaultRouteSettings": map[string]any{
			"ThrottlingRateLimit":    20,
			"ThrottlingBurstLimit":   40,
			"DetailedMetricsEnabled": true,
		},
	})
}

// Without a domain the stack must still synthesise: the subdomain decision is open, and
// blocking the deploy on it would block the frontend loop too.
func TestSynthesisesWithoutADomain(t *testing.T) {
	cfg := testConfig()
	cfg.DomainName = ""
	cfg.HostedZoneID = ""
	cfg.HostedZoneName = ""
	regional, global := synthBoth(t, cfg)

	// No certificate is issued, and the distribution has no aliases.
	global.ResourceCountIs(jsii.String("AWS::CertificateManager::Certificate"), jsii.Number(0))
	regional.ResourceCountIs(jsii.String("AWS::Route53::RecordSet"), jsii.Number(0))

	tpl := regional
	dists := tpl.FindResources(jsii.String("AWS::CloudFront::Distribution"), nil)
	for id, res := range *dists {
		cfgBlock := (*res)["Properties"].(map[string]any)["DistributionConfig"].(map[string]any)
		if aliases, present := cfgBlock["Aliases"]; present {
			t.Errorf("%s has aliases %v without a certificate", id, aliases)
		}
	}
}

// A record inside a PARENT zone carries the full subdomain name.
func TestDomainInsideParentZone(t *testing.T) {
	cfg := testConfig()
	cfg.DomainName = "stats.example.com"
	cfg.HostedZoneID = "Z0123456789ABCDEFGHIJ"
	cfg.HostedZoneName = "example.com"
	regional, global := synthBoth(t, cfg)

	global.ResourceCountIs(jsii.String("AWS::CertificateManager::Certificate"), jsii.Number(1))
	// An A and an AAAA record, so the site resolves over IPv6 too.
	regional.ResourceCountIs(jsii.String("AWS::Route53::RecordSet"), jsii.Number(2))

	for id, res := range *regional.FindResources(jsii.String("AWS::Route53::RecordSet"), nil) {
		name := (*res)["Properties"].(map[string]any)["Name"].(string)
		if name != "stats.example.com." {
			t.Errorf("%s Name = %q, want stats.example.com.", id, name)
		}
	}
}

// TestDelegatedZoneRecordIsAtTheApex is the case this deployment actually uses.
//
// When the domain IS the hosted zone -- a delegated subdomain zone -- the record belongs at
// the apex and RecordName must be omitted. Passing the full name would make CDK append the
// zone again, producing spotistats.neovasili.com.spotistats.neovasili.com.
func TestDelegatedZoneRecordIsAtTheApex(t *testing.T) {
	tpl := synth(t, testConfig())
	records := tpl.FindResources(jsii.String("AWS::Route53::RecordSet"), nil)
	if len(*records) != 2 {
		t.Fatalf("records = %d, want an A and an AAAA", len(*records))
	}

	types := map[string]bool{}
	for id, res := range *records {
		props := (*res)["Properties"].(map[string]any)
		name := props["Name"].(string)
		if name != "spotistats.neovasili.com." {
			t.Errorf("%s Name = %q, want the zone apex spotistats.neovasili.com. "+
				"(a doubled suffix means RecordName was set for a delegated zone)", id, name)
		}
		if indexOf(name, "spotistats.neovasili.com.spotistats") >= 0 {
			t.Errorf("%s has a doubled zone suffix: %q", id, name)
		}
		types[props["Type"].(string)] = true
		if got := props["HostedZoneId"]; got != "Z08622643JXD4FF65E2XP" {
			t.Errorf("%s HostedZoneId = %v", id, got)
		}
	}
	if !types["A"] || !types["AAAA"] {
		t.Errorf("record types = %v, want both A and AAAA", types)
	}
}

// Half a hosted-zone configuration would silently fall back to manual DNS, so it is refused.
func TestPartialHostedZoneConfigIsRejected(t *testing.T) {
	app := awscdk.NewApp(&awscdk.AppProps{
		Outdir: jsii.String(t.TempDir()),
		Context: &map[string]interface{}{
			"lambdaAssetDir": fakeAssetDir(t),
			"domainName":     "stats.example.com",
			"hostedZoneId":   "Z0123456789ABCDEFGHIJ",
		},
	})
	if _, err := stackConfigFromContext(app); err == nil {
		t.Error("accepted a hosted zone ID with no zone name")
	}
}

func TestHostedZoneWithoutDomainIsRejected(t *testing.T) {
	app := awscdk.NewApp(&awscdk.AppProps{
		Outdir: jsii.String(t.TempDir()),
		Context: &map[string]interface{}{
			"lambdaAssetDir": fakeAssetDir(t),
			"hostedZoneId":   "Z0123456789ABCDEFGHIJ",
			"hostedZoneName": "example.com",
		},
	})
	if _, err := stackConfigFromContext(app); err == nil {
		t.Error("accepted a hosted zone with no domain name")
	}
}

// make deploy-web reads these outputs; without them it cannot find the bucket or invalidate.
func TestWebOutputsExist(t *testing.T) {
	tpl := synth(t, testConfig())
	raw, err := json.Marshal(tpl.ToJSON())
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, key := range []string{"WebBucketName", "DistributionId", "SiteUrl", "ApiUrl"} {
		if indexOf(body, key) < 0 {
			t.Errorf("no %q output; `make deploy-web` depends on it", key)
		}
	}
}

func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }

// ---------------------------------------------------------------------------
// rollup
// ---------------------------------------------------------------------------

// TestRollupCannotOverwriteTheFrontend: the snapshots and the frontend bundle share one bucket,
// so a bug in the rollup must not be able to replace the site with a JSON file.
func TestRollupCannotOverwriteTheFrontend(t *testing.T) {
	tpl := synth(t, testConfig())
	policies := tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil)

	checked := false
	for id, res := range *policies {
		if !contains(id, "Rollup") {
			continue
		}
		checked = true
		doc := (*res)["Properties"].(map[string]any)["PolicyDocument"].(map[string]any)
		for _, st := range doc["Statement"].([]any) {
			stmt := st.(map[string]any)
			raw, err := json.Marshal(stmt)
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			if indexOf(body, "s3:") < 0 {
				continue
			}
			// The only S3 action, and only under the data prefix.
			if indexOf(body, "s3:PutObject") < 0 {
				t.Errorf("%s grants an unexpected S3 action: %s", id, body)
			}
			if indexOf(body, "/data/*") < 0 {
				t.Errorf("%s S3 grant is not scoped to the data prefix: %s", id, body)
			}
			for _, forbidden := range []string{"s3:DeleteObject", "s3:*", "s3:GetObject"} {
				if indexOf(body, `"`+forbidden+`"`) >= 0 {
					t.Errorf("%s grants %s", id, forbidden)
				}
			}
		}
	}
	if !checked {
		t.Fatal("found no IAM policy for the rollup")
	}
}

// Unlike capture, the rollup never refreshes a token, so it has no reason to write one.
func TestRollupCannotWriteTheRefreshToken(t *testing.T) {
	tpl := synth(t, testConfig())
	for id, res := range *tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil) {
		if !contains(id, "Rollup") {
			continue
		}
		doc := (*res)["Properties"].(map[string]any)["PolicyDocument"].(map[string]any)
		for _, st := range doc["Statement"].([]any) {
			for _, a := range actionsOf(st.(map[string]any)) {
				if a == "ssm:PutParameter" {
					t.Errorf("%s grants ssm:PutParameter to the rollup", id)
				}
			}
		}
	}
}

func TestRollupSchedule(t *testing.T) {
	tpl := synth(t, testConfig())
	tpl.HasResourceProperties(jsii.String("AWS::Events::Rule"), map[string]any{
		"ScheduleExpression": "cron(15 3 * * ? *)",
		"State":              "ENABLED",
	})

	// The rollup is idempotent, so one retry is worth it -- unlike capture, where the next
	// scheduled run covers the same window anyway.
	rules := tpl.FindResources(jsii.String("AWS::Events::Rule"), nil)
	found := false
	for _, res := range *rules {
		props := (*res)["Properties"].(map[string]any)
		if props["ScheduleExpression"] != "cron(15 3 * * ? *)" {
			continue
		}
		found = true
		for _, tg := range props["Targets"].([]any) {
			rp := tg.(map[string]any)["RetryPolicy"].(map[string]any)
			if rp["MaximumRetryAttempts"] != float64(1) {
				t.Errorf("rollup retries = %v, want 1", rp["MaximumRetryAttempts"])
			}
		}
	}
	if !found {
		t.Fatal("no nightly rollup rule")
	}
}

// A reconcile streams every play in its window, so it needs the full timeout.
func TestRollupHasHeadroom(t *testing.T) {
	tpl := synth(t, testConfig())
	tpl.HasResourceProperties(jsii.String("AWS::Lambda::Function"), map[string]any{
		"FunctionName": "spotistats-rollup",
		"Timeout":      900,
		"MemorySize":   1024,
	})
}

// Every metric the rollup emits must have an alarm, or the drift signal goes unwatched.
func TestRollupAlarmsExist(t *testing.T) {
	tpl := synth(t, testConfig())
	names := map[string]bool{}
	for _, res := range *tpl.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil) {
		props := (*res)["Properties"].(map[string]any)
		if n, ok := props["AlarmName"].(string); ok {
			names[n] = true
		}
	}
	for _, want := range []string{
		"spotistats-RollupFailed",
		"spotistats-RollupStale",
		"spotistats-AggregateDrift",
	} {
		if !names[want] {
			t.Errorf("no %s alarm", want)
		}
	}
}

// actionsOf normalises a statement's Action, which CloudFormation renders as a bare string when
// there is only one and as a list otherwise. Asserting one shape panics on the other.
func actionsOf(stmt map[string]any) []string {
	switch v := stmt["Action"].(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, a := range v {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
