package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// testAccount is the account every synthesised template is built against.
const testAccount = "111122223333"

// templateJSON renders a synthesised template for substring assertions.
//
// IAM trust policies nest conditions several levels deep and CloudFormation intrinsics wrap the
// values, so matching structurally is brittle in a way that hides the thing being asserted.
// These tests care whether a specific principal or ARN appears at all.
func templateJSON(t *testing.T, tpl assertions.Template) string {
	t.Helper()
	b, err := json.Marshal(tpl.ToJSON())
	if err != nil {
		t.Fatalf("marshal template: %v", err)
	}
	return string(b)
}

func synth(t *testing.T, cfg StackConfig) assertions.Template {
	t.Helper()
	regional, _ := synthBoth(t, cfg)
	return regional
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
		SSMPrefix:        "/spotistats/spotify",
		MonthlyBudgetUSD: 10, CaptureRateMinutes: 30,
		// A delegated subdomain zone: the domain IS the zone, so the alias record belongs at
		// its apex.
		DomainName:     "spotistats.neovasili.com",
		HostedZoneName: "spotistats.neovasili.com",
		HostedZoneID:   "Z0123456789ABCDEFGHIJ",
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
		"ScheduleExpression": "rate(30 minutes)",
		"State":              "ENABLED",
	})

	// And a custom rate flows through, in minutes.
	cfg := testConfig()
	cfg.CaptureRateMinutes = 15
	synth(t, cfg).HasResourceProperties(jsii.String("AWS::Events::Rule"), map[string]any{
		"ScheduleExpression": "rate(15 minutes)",
	})
}

// TestCaptureIntervalGuardsAgainstPageSaturation pins the reasoning behind the interval.
//
// recently-played returns at most 50 items and cannot page back, so the risk is plays per
// POLLING WINDOW, not per day -- a busy evening loses plays however quiet the rest of the day
// was, and a gap was in fact recorded in production at a 2-hour interval. The default must
// leave the 50-item page unreachable at any plausible listening rate.
func TestCaptureIntervalGuardsAgainstPageSaturation(t *testing.T) {
	const pageLimit = 50.0
	// The fastest plausible sustained rate: nothing but ~2-minute tracks, back to back.
	const busiestPlaysPerMinute = 0.5

	worstCase := defaultCaptureRateMinutes * busiestPlaysPerMinute
	if worstCase >= pageLimit {
		t.Errorf("a %g-minute interval allows up to %g plays per window against a %g-item page; "+
			"saturation would silently lose plays", defaultCaptureRateMinutes, worstCase, pageLimit)
	}
	// And the alarm window must not be so tight that one skipped run reads as an outage:
	// EventBridge does not retry, so a single transient failure is normal.
	if got := alarmWindowFor(defaultCaptureRateMinutes); got < 3*defaultCaptureRateMinutes {
		t.Errorf("alarm window %g is under 3 capture intervals", got)
	}
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
		// The external-enrichment ratio, which is the alarmable figure rather than the count.
		metrics.ExternalUnresolvedRatio,
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
	for _, m := range []string{
		metrics.PlaysGapDetected, metrics.TokenRefreshFailed, metrics.GenresDegraded,
		metrics.ExternalUnresolvedRatio,
	} {
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

// The topic is subscribed unconditionally now, because the subscriber is a Lambda in this same
// stack rather than an address someone has to supply. That is the point of the change: there is
// no configuration whose absence leaves the topic silently unsubscribed.
func TestAlarmTopicIsAlwaysSubscribed(t *testing.T) {
	cfg := testConfig()
	regional, _ := synthBoth(t, cfg)

	regional.ResourceCountIs(jsii.String("AWS::SNS::Topic"), jsii.Number(1))
	regional.ResourceCountIs(jsii.String("AWS::SNS::Subscription"), jsii.Number(1))
	regional.HasResourceProperties(jsii.String("AWS::SNS::Subscription"), map[string]any{
		"Protocol": "lambda",
	})
}

// No email subscription may exist anywhere: Slack is the channel, and a second one would be a
// second place to check that nobody checks. Not an indictment of SNS email, which worked -- see
// internal/notify's package comment.
func TestNoEmailSubscriptionAnywhere(t *testing.T) {
	regional, global := synthBoth(t, testConfig())
	for name, tpl := range map[string]assertions.Template{"regional": regional, "global": global} {
		subs := tpl.FindResources(jsii.String("AWS::SNS::Subscription"), map[string]any{
			"Properties": map[string]any{"Protocol": "email"},
		})
		if len(*subs) != 0 {
			t.Errorf("%s stack has an email subscription, which cannot deliver until a human "+
				"clicks a link: %v", name, *subs)
		}
	}
}

// The public API is unauthenticated and there is no WAF by design, so the budget is the last
// line of defence against runaway cost.
//
// It lives in the REGIONAL stack now, next to the topic it notifies. The global stack deploys
// first, so a budget there could not reference a topic here.
func TestBudgetThreshold(t *testing.T) {
	regional, global := synthBoth(t, testConfig())
	regional.HasResourceProperties(jsii.String("AWS::Budgets::Budget"), map[string]any{
		"Budget": map[string]any{
			"BudgetType":  "COST",
			"TimeUnit":    "MONTHLY",
			"BudgetLimit": map[string]any{"Amount": 10, "Unit": "USD"},
		},
	})
	global.ResourceCountIs(jsii.String("AWS::Budgets::Budget"), jsii.Number(0))
}

// The budget notifies through the alarm topic, so it reaches Slack like everything else rather
// than being a second channel nobody checks.
func TestBudgetNotifiesTheAlarmTopic(t *testing.T) {
	budgets := *synth(t, testConfig()).FindResources(jsii.String("AWS::Budgets::Budget"), nil)
	if len(budgets) != 1 {
		t.Fatalf("budgets = %d, want 1", len(budgets))
	}
	for _, res := range budgets {
		subs := subscriberTypes(res)
		if len(subs) == 0 {
			t.Fatal("the budget has no subscribers, so it can notify nobody")
		}
		for _, kind := range subs {
			if kind != "SNS" {
				t.Errorf("subscriber type = %q, want SNS: email would be a second channel", kind)
			}
		}
	}
}

// Budgets publishes as a service principal, which an SNS topic does not permit by default.
// Without this grant the budget silently never notifies -- the same class of quiet failure as
// the unconfirmed email subscription, and invisible in the console.
func TestTopicAllowsBudgetsToPublish(t *testing.T) {
	policies := *synth(t, testConfig()).FindResources(jsii.String("AWS::SNS::TopicPolicy"), nil)
	if len(policies) == 0 {
		t.Fatal("no topic policy, so budgets.amazonaws.com cannot publish")
	}
	if !strings.Contains(templateJSON(t, synth(t, testConfig())), "budgets.amazonaws.com") {
		t.Error("the topic policy does not name budgets.amazonaws.com")
	}
}

// subscriberTypes pulls SubscriptionType values out of a synthesised budget resource.
func subscriberTypes(res *map[string]any) []string {
	var out []string
	props, _ := (*res)["Properties"].(map[string]any)
	notifications, _ := props["NotificationsWithSubscribers"].([]any)
	for _, n := range notifications {
		nm, _ := n.(map[string]any)
		subs, _ := nm["Subscribers"].([]any)
		for _, sub := range subs {
			sm, _ := sub.(map[string]any)
			if kind, ok := sm["SubscriptionType"].(string); ok {
				out = append(out, kind)
			}
		}
	}
	return out
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
//
// Scoped to the CAPTURE policy. It used to scan every policy in the template, which made it fail
// the moment the enrich role legitimately gained a DeleteItem for its own lock -- the same
// over-broad pattern that made the Spotify-token test flag capture and rollup. A test that fires
// on another role's legitimate grant reports the wrong component and gets weakened rather than
// understood.
func TestCaptureRoleIsLeastPrivilege(t *testing.T) {
	tpl := synth(t, testConfig())
	policies := tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil)

	checked := 0
	for id, res := range *policies {
		if !strings.Contains(id, "Capture") {
			continue
		}
		checked++
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
	// Without this the test passes vacuously if the policy is ever renamed.
	if checked == 0 {
		t.Fatal("no capture policy found, so nothing was actually checked")
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
	if cfg.CaptureRateMinutes != defaultCaptureRateMinutes {
		t.Errorf("default capture rate = %v, want %v", cfg.CaptureRateMinutes, defaultCaptureRateMinutes)
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
		// Both image hosts, because the failure mode is identical and silent: a page that
		// renders with no pictures and only a console error.
		if indexOf(csp, "https://r2.theaudiodb.com") < 0 {
			t.Error("CSP omits r2.theaudiodb.com; every fanart, banner and logo on the artist " +
				"profile would be blocked")
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

// With no domain there is nothing for the us-east-1 stack to hold, so it must not be created at
// all. An empty stack is not merely pointless: CloudFormation requires a non-empty Resources
// section, so it would fail to deploy. This was masked for a while by the budget living there.
func TestNoGlobalStackWithoutADomain(t *testing.T) {
	app := awscdk.NewApp(&awscdk.AppProps{Outdir: jsii.String(t.TempDir())})
	cfg := testConfig()
	cfg.LambdaAssetDir = fakeAssetDir(t)
	cfg.DomainName, cfg.HostedZoneID, cfg.HostedZoneName = "", "", ""

	// Mirrors main(): the global stack is constructed only when there is a certificate for it.
	NewSpotistatsStack(app, "SpotistatsStack", &SpotistatsStackProps{
		StackProps: awscdk.StackProps{Env: cfg.env()},
		Config:     cfg,
	})
	assembly := app.Synth(nil)
	for _, st := range *assembly.Stacks() {
		if *st.StackName() != "SpotistatsStack" {
			t.Errorf("unexpected stack %q synthesised without a domain", *st.StackName())
		}
	}
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
		if got := props["HostedZoneId"]; got != "Z0123456789ABCDEFGHIJ" {
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

// TestAlarmsHaveSomewhereToGo guards the gap that really did ship: alarms existed, three were
// firing, and the SNS topic had no subscribers at all -- because alarmEmail was never set and
// both the subscription and the budget skipped themselves silently.
//
// An unsubscribed topic is worse than no alarms: the console shows a monitored stack. Note this
// was a MISSING subscriber, not a broken one; the email subscription delivered fine once it
// existed and was confirmed.
func TestAlarmsHaveSomewhereToGo(t *testing.T) {
	// The regression in one assertion: alarms exist, so a subscriber must too. This now holds
	// for EVERY configuration, because the subscriber is a Lambda in this stack rather than an
	// address someone has to remember to set.
	t.Run("alarms exist, so a subscriber must too", func(t *testing.T) {
		{
			cfg := testConfig()
			tpl := synth(t, cfg)
			alarms := *tpl.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
			subs := *tpl.FindResources(jsii.String("AWS::SNS::Subscription"), nil)
			if len(alarms) == 0 {
				t.Fatal("no alarms synthesised")
			}
			if len(subs) == 0 {
				t.Errorf("%d alarms but no SNS subscription: every one would fire into "+
					"the void", len(alarms))
			}
		}
	})

	t.Run("the subscriber is the notifier Lambda", func(t *testing.T) {
		tpl := synth(t, testConfig())
		tpl.HasResourceProperties(jsii.String("AWS::SNS::Subscription"), map[string]any{
			"Protocol": "lambda",
		})
		// And SNS must be permitted to invoke it, or the subscription exists and delivers
		// nothing -- which is precisely the failure being removed.
		tpl.HasResourceProperties(jsii.String("AWS::Lambda::Permission"), map[string]any{
			"Action":    "lambda:InvokeFunction",
			"Principal": "sns.amazonaws.com",
		})
	})

	t.Run("every alarm notifies on recovery too", func(t *testing.T) {
		// A channel that only ever says "broken" and never "fixed" cannot be used to tell a
		// live incident from a stale message.
		for name, res := range *synth(t, testConfig()).FindResources(
			jsii.String("AWS::CloudWatch::Alarm"), nil) {
			props, _ := (*res)["Properties"].(map[string]any)
			if actions, _ := props["OKActions"].([]any); len(actions) == 0 {
				t.Errorf("%s has no OKActions, so recovery is never announced", name)
			}
		}
	})
}

// The budget is one of the compensating controls for running without a WAF (docs/SPECS.md 10),
// so it must actually be created for a normal configuration.
func TestBudgetIsCreatedWhenConfigured(t *testing.T) {
	cfg := testConfig()
	// No email anywhere any more: the budget notifies the alarm topic.
	cfg.MonthlyBudgetUSD = 10
	synth(t, cfg).HasResourceProperties(jsii.String("AWS::Budgets::Budget"), map[string]any{
		"Budget": map[string]any{
			"BudgetName":  "spotistats-monthly",
			"BudgetType":  "COST",
			"TimeUnit":    "MONTHLY",
			"BudgetLimit": map[string]any{"Amount": 10, "Unit": "USD"},
		},
	})
}

// TestGitHubDeployRoleIsOptional: nobody deploying from a laptop should acquire an IAM role
// they did not ask for, and synth must work without any GitHub configuration.
func TestGitHubDeployRoleIsOptional(t *testing.T) {
	cfg := testConfig()
	cfg.GitHubRepo = ""
	roles := synth(t, cfg).FindResources(jsii.String("AWS::IAM::Role"), map[string]any{
		"Properties": map[string]any{"RoleName": "spotistats-github-deploy"},
	})
	if len(*roles) != 0 {
		t.Error("a deploy role was created without githubRepo being set")
	}
}

// TestGitHubDeployRoleIsScopedToTheRepo guards the one condition that makes OIDC safe.
//
// The audience check proves only that a token came from GitHub — not from WHOSE repository.
// Without the `sub` condition, any GitHub Actions workflow anywhere could assume this role and
// deploy to (or delete) this account. It is the whole security boundary.
func TestGitHubDeployRoleIsScopedToTheRepo(t *testing.T) {
	cfg := testConfig()
	cfg.GitHubRepo = "neovasili/spotistats"
	tpl := synth(t, cfg)

	roles := tpl.FindResources(jsii.String("AWS::IAM::Role"), map[string]any{
		"Properties": map[string]any{"RoleName": "spotistats-github-deploy"},
	})
	if len(*roles) != 1 {
		t.Fatalf("found %d deploy roles, want exactly 1", len(*roles))
	}

	body := templateJSON(t, tpl)
	for _, want := range []string{
		// Scoped to this repo AND this branch.
		"repo:neovasili/spotistats:ref:refs/heads/main",
		// The audience must be pinned too, or a token minted for another service would pass.
		"sts.amazonaws.com",
		"token.actions.githubusercontent.com:sub",
		"token.actions.githubusercontent.com:aud",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the trust policy is missing %q; the role may be assumable by any repository", want)
		}
	}
}

// TestGitHubDeployRoleIsNotAdministrator pins least privilege.
//
// `cdk deploy` works by assuming the roles CDK bootstrap created, which already hold the broad
// permissions — so this role needs almost none of its own. Attaching AdministratorAccess "so
// deploys always work" would make one compromised workflow run equal to an account takeover.
func TestGitHubDeployRoleIsNotAdministrator(t *testing.T) {
	cfg := testConfig()
	cfg.GitHubRepo = "neovasili/spotistats"
	body := templateJSON(t, synth(t, cfg))

	for _, forbidden := range []string{"AdministratorAccess", "PowerUserAccess", "IAMFullAccess"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the deploy role has %s attached", forbidden)
		}
	}
	// It must be able to assume the bootstrap roles, which is HOW it deploys.
	if !strings.Contains(body, "cdk-hnb659fds-deploy-role") {
		t.Error("the deploy role cannot assume the CDK bootstrap deploy role, so cdk deploy " +
			"would fail with an access error naming no role")
	}
	// And the certificate stack's region must be covered, or the global stack never deploys.
	if !strings.Contains(body, "cdk-hnb659fds-deploy-role-"+testAccount+"-us-east-1") {
		t.Error("no bootstrap role for us-east-1; the certificate stack could not be deployed")
	}
}

// A custom ref list must replace the default rather than adding to it, so narrowing works.
func TestGitHubDeployRefsAreConfigurable(t *testing.T) {
	cfg := testConfig()
	cfg.GitHubRepo = "neovasili/spotistats"
	cfg.GitHubDeployRefs = []string{"environment:production"}
	body := templateJSON(t, synth(t, cfg))

	if !strings.Contains(body, "repo:neovasili/spotistats:environment:production") {
		t.Error("the configured ref was not applied")
	}
	if strings.Contains(body, "refs/heads/main") {
		t.Error("the default ref survived alongside the configured one, widening the trust")
	}
}

// TestEnrichLambdaConcurrency covers a constraint that could not be met the obvious way.
//
// Both upstream rate limits are per-IP, so two overlapping runs double the real request rate and
// MusicBrainz answers 503 to EVERYTHING from that IP, including the other run.
// ReservedConcurrentExecutions: 1 is the natural guard, and it IS applied when configured.
//
// But it cannot be depended on: AWS rejects ANY reservation on an account whose total
// concurrency limit is 10, because it requires 10 to remain unreserved. This deployment is on
// such an account, and the deploy failed with "decreases account's
// UnreservedConcurrentExecution below its minimum value of [10]". So it is opt-in, and
// single-flight is enforced by store.AcquireEnrichLock, which needs no quota. See
// internal/enrich's TestSingleFlightLock.
func TestEnrichLambdaConcurrency(t *testing.T) {
	enrichProps := func(t *testing.T, cfg StackConfig) map[string]any {
		t.Helper()
		for _, res := range *synth(t, cfg).FindResources(jsii.String("AWS::Lambda::Function"), nil) {
			props := (*res)["Properties"].(map[string]any)
			if name, _ := props["FunctionName"].(string); name == "spotistats-enrich" {
				return props
			}
		}
		t.Fatal("no spotistats-enrich function found")
		return nil
	}

	t.Run("reserved when configured", func(t *testing.T) {
		cfg := testConfig()
		cfg.EnrichReservedConcurrency = 1
		if got := enrichProps(t, cfg)["ReservedConcurrentExecutions"]; fmt.Sprint(got) != "1" {
			t.Errorf("ReservedConcurrentExecutions = %v, want 1", got)
		}
	})

	t.Run("omitted by default, so a capped account can deploy", func(t *testing.T) {
		if _, ok := enrichProps(t, testConfig())["ReservedConcurrentExecutions"]; ok {
			t.Error("a reservation is set by default; the deploy fails outright on any " +
				"account whose total concurrency limit is 10")
		}
	})

	t.Run("five minutes, not fifteen", func(t *testing.T) {
		// The job checkpoints per artist and is resumable, and a short timeout bounds how long
		// a wedged upstream can hold the lock.
		if got := enrichProps(t, testConfig())["Timeout"]; got != float64(300) {
			t.Errorf("Timeout = %v, want 300s", got)
		}
	})
}

// The enrich job runs an hour after the rollup. Not merely tidy: the rollup rewrites the artist
// aggregates this job reads as its work list, and overlapping them would mean enriching against
// a partially rewritten list.
func TestEnrichRunsAfterTheRollup(t *testing.T) {
	tpl := synth(t, testConfig())
	rules := tpl.FindResources(jsii.String("AWS::Events::Rule"), nil)

	schedules := map[string]string{}
	for _, res := range *rules {
		props := (*res)["Properties"].(map[string]any)
		name, _ := props["Name"].(string)
		expr, _ := props["ScheduleExpression"].(string)
		schedules[name] = expr
	}
	if got := schedules["spotistats-enrich-schedule"]; got != "cron(15 4 * * ? *)" {
		t.Errorf("enrich schedule = %q, want 04:15 UTC (an hour after the rollup)", got)
	}
	if got := schedules["spotistats-rollup-schedule"]; got != "cron(15 3 * * ? *)" {
		t.Errorf("rollup schedule = %q; the enrich offset assumes 03:15", got)
	}
}

// The enrich job reads ONE SSM parameter, not the prefix. It has no business reading the Spotify
// refresh token, and a wildcard would grant it.
//
// Scoped to the enrich role's own policy rather than the whole template: capture legitimately
// holds a prefix wildcard (it ROTATES the refresh token) and so does the rollup (it calls
// Spotify for top items). Asserting no wildcard exists anywhere would fail on those and teach
// the next person to delete the assertion.
func TestEnrichLambdaCannotReadTheSpotifyToken(t *testing.T) {
	tpl := synth(t, testConfig())
	policies := tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil)

	const keyParam = "/theaudiodb_key"
	var found bool
	for id, res := range *policies {
		body, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if !strings.Contains(text, keyParam) {
			continue // not the enrich role's policy
		}
		found = true
		// This IS the enrich policy. It must not also carry a prefix wildcard.
		if strings.Contains(text, "parameter/spotistats/spotify/*") {
			t.Errorf("%s grants the enrich role a wildcard over the credential prefix, which "+
				"includes the Spotify refresh token", id)
		}
		// Nor write access to anything in SSM.
		if strings.Contains(text, "ssm:PutParameter") {
			t.Errorf("%s lets the enrich role write SSM parameters", id)
		}
	}
	if !found {
		t.Error("no policy grants the enrich role its own API key parameter")
	}
}

// TestNotifyLambdaHoldsAlmostNothing is the counterpart to the enrich test above.
//
// The notifier forwards text to a third party over the public internet, which makes it the
// worst function in this stack to over-permission: anything it can read, a compromised webhook
// endpoint can be fed. So it gets exactly one SSM parameter and nothing else -- no table, no
// credential prefix, no writes.
func TestNotifyLambdaHoldsAlmostNothing(t *testing.T) {
	tpl := synth(t, testConfig())

	const webhookParam = "/slack_webhook"
	var found bool
	for id, res := range *tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil) {
		body, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if !strings.Contains(text, webhookParam) {
			continue // not the notifier's policy
		}
		found = true

		// A wildcard here would hand it the Spotify refresh token.
		if strings.Contains(text, "parameter/spotistats/spotify/*") {
			t.Errorf("%s grants the notifier a wildcard over the credential prefix", id)
		}
		if strings.Contains(text, "/refresh_token") || strings.Contains(text, "/client_secret") {
			t.Errorf("%s grants the notifier a Spotify credential", id)
		}
		// The notifier never touches the table: it has no reason to read a play and no reason
		// to be able to delete one.
		if strings.Contains(text, "dynamodb:") {
			t.Errorf("%s grants the notifier DynamoDB access", id)
		}
		if strings.Contains(text, "ssm:PutParameter") {
			t.Errorf("%s lets the notifier write SSM parameters", id)
		}
	}
	if !found {
		t.Error("no policy grants the notifier its webhook parameter, so it cannot deliver anything")
	}
}

// The notifier must exist as a real function, on the same runtime and architecture as the rest.
func TestNotifyFunctionExists(t *testing.T) {
	synth(t, testConfig()).HasResourceProperties(jsii.String("AWS::Lambda::Function"), map[string]any{
		"FunctionName":  "spotistats-notify",
		"Runtime":       "provided.al2023",
		"Architectures": []any{"arm64"},
	})
}

// The webhook is a credential, so it must never reach the template. CloudFormation retains
// template history, and a template is readable by anyone with cloudformation:GetTemplate.
func TestSlackWebhookIsNotInTheTemplate(t *testing.T) {
	regional, global := synthBoth(t, testConfig())
	for name, tpl := range map[string]assertions.Template{"regional": regional, "global": global} {
		text := templateJSON(t, tpl)
		if strings.Contains(text, "hooks.slack.com") {
			t.Errorf("%s stack template contains a Slack webhook URL", name)
		}
	}
}

// A failure to deliver must itself be alarmed. It is NOT self-monitoring -- the alarm travels
// through the function that failed -- but it has to exist so the console and the CLI can answer
// "why has the channel gone quiet?".
func TestNotifierFailureIsAlarmed(t *testing.T) {
	synth(t, testConfig()).HasResourceProperties(jsii.String("AWS::CloudWatch::Alarm"), map[string]any{
		"AlarmName": "spotistats-NotifyFailed",
	})
}

// TestEnrichCanReleaseItsOwnLock guards a defect that reached production.
//
// store.ReleaseEnrichLock issues a DeleteItem on STATE/EXTERNAL_ENRICH_LOCK, but the enrich
// role was granted only GetItem/BatchGetItem/PutItem/Query -- with a comment asserting the job
// "never removes anything", which was simply wrong. Every run ended with AccessDenied on the
// release, the lock survived its full 15-minute TTL, and a retry after a failed run found the
// lock held and exited having done nothing.
//
// The grant must exist AND stay confined to the STATE partition: the job still has no business
// deleting a play, an aggregate or an artist.
func TestEnrichCanReleaseItsOwnLock(t *testing.T) {
	var found bool
	for id, res := range *synth(t, testConfig()).FindResources(jsii.String("AWS::IAM::Policy"), nil) {
		body, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		// The enrich policy is the one carrying its own API key parameter.
		if !strings.Contains(text, "/theaudiodb_key") {
			continue
		}
		if !strings.Contains(text, "dynamodb:DeleteItem") {
			t.Errorf("%s does not let the enrich role delete its own lock, so every run leaks "+
				"it for the full TTL and blocks the next one", id)
			continue
		}
		found = true
		// Confined to the STATE partition, or this is a licence to delete plays.
		if !strings.Contains(text, "dynamodb:LeadingKeys") {
			t.Errorf("%s grants DeleteItem with no LeadingKeys condition, so the enrich role "+
				"can delete any item in the table", id)
		}
		if !strings.Contains(text, `"STATE"`) {
			t.Errorf("%s does not confine DeleteItem to the STATE partition", id)
		}
	}
	if !found {
		t.Error("no policy grants the enrich role DeleteItem on its lock")
	}
}

// TestResolveLambdaIsQuotaSafe pins the properties that keep track resolution from taking
// capture down with it.
//
// The two jobs share one Spotify quota, and capture is the only one whose failure is
// unrecoverable: recently-played is a rolling ~50-play window, so consecutive failures lose
// listening permanently. Everything asserted here exists for that reason.
func TestResolveLambdaIsQuotaSafe(t *testing.T) {
	tpl := synth(t, testConfig())

	// A BOUNDED batch, set in the environment rather than left to run until the API refuses.
	fns := *tpl.FindResources(jsii.String("AWS::Lambda::Function"), map[string]any{
		"Properties": map[string]any{"FunctionName": "spotistats-resolve"},
	})
	if len(fns) != 1 {
		t.Fatalf("resolve functions = %d, want 1", len(fns))
	}
	for _, res := range fns {
		props := (*res)["Properties"].(map[string]any)
		env, _ := props["Environment"].(map[string]any)
		vars, _ := env["Variables"].(map[string]any)
		if vars["SPOTISTATS_RESOLVE_LIMIT"] == nil {
			t.Error("no batch limit configured; the job would run until Spotify refuses, " +
				"which is exactly what starves capture")
		}
	}

	// Once a day. Twice would double the quota spend for no benefit a two-month job can feel.
	rules := *tpl.FindResources(jsii.String("AWS::Events::Rule"), map[string]any{
		"Properties": map[string]any{"Name": "spotistats-resolve-schedule"},
	})
	if len(rules) != 1 {
		t.Fatalf("resolve schedules = %d, want 1", len(rules))
	}
	for _, res := range rules {
		props := (*res)["Properties"].(map[string]any)
		// Every six hours, not daily. A cooldown carries Spotify's own Retry-After and reopens
		// at an arbitrary hour, so a once-daily attempt can miss a window by hours and lose a
		// whole day -- which is what happened on the first night. Attempting during a cooldown
		// costs nothing.
		if got, _ := props["ScheduleExpression"].(string); got != "cron(15 5/6 * * ? *)" {
			t.Errorf("schedule = %q, want every 6h from 05:15", got)
		}
	}

	// A stalled backlog must be visible: a cooldown that never expires would otherwise freeze
	// the job silently for months.
	tpl.HasResourceProperties(jsii.String("AWS::CloudWatch::Alarm"), map[string]any{
		"AlarmName": "spotistats-ResolveStalled",
	})
}

// The resolve role must not be able to delete anything: it only ever upgrades a row's identity.
func TestResolveRoleCannotDelete(t *testing.T) {
	tpl := synth(t, testConfig())
	checked := 0
	for id, res := range *tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil) {
		if !strings.Contains(id, "Resolve") {
			continue
		}
		checked++
		body, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "dynamodb:DeleteItem") {
			t.Errorf("%s grants DeleteItem to the resolver, which never removes a row", id)
		}
	}
	if checked == 0 {
		t.Fatal("no resolve policy found, so nothing was checked")
	}
}

// TestSnapshotsAreRenderedBetweenNightlyRuns covers the dashboard's freshness.
//
// The snapshot is a static file, and it used to be written only by the nightly run -- so a play
// captured at 09:00 did not appear until 03:15 the following morning, and the footer's
// "last updated 05:16" was reporting that render rather than any staleness in the data.
func TestSnapshotsAreRenderedBetweenNightlyRuns(t *testing.T) {
	tpl := synth(t, testConfig())

	rules := *tpl.FindResources(jsii.String("AWS::Events::Rule"), map[string]any{
		"Properties": map[string]any{"Name": "spotistats-rollup-render-schedule"},
	})
	if len(rules) != 1 {
		t.Fatalf("render schedules = %d, want 1", len(rules))
	}
	for _, res := range rules {
		props := (*res)["Properties"].(map[string]any)
		if got, _ := props["ScheduleExpression"].(string); got != "cron(35 */2 * * ? *)" {
			t.Errorf("schedule = %q, want every two hours", got)
		}
		// It must ask for the CHEAP path. Without renderOnly it would reconcile and stream the
		// whole play history twelve times a day, which is where the read cost lives.
		body, err := json.Marshal(props["Targets"])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "renderOnly") {
			t.Error("the two-hourly rule does not request renderOnly, so it would run a full " +
				"reconcile every two hours")
		}
	}

	// Both rules drive the SAME function: identical code, config and IAM; only the work differs.
	fns := *tpl.FindResources(jsii.String("AWS::Lambda::Function"), map[string]any{
		"Properties": map[string]any{"FunctionName": "spotistats-rollup"},
	})
	if len(fns) != 1 {
		t.Errorf("rollup functions = %d, want 1 shared by both schedules", len(fns))
	}
}

// TestNightlyFullReconcileExists guards the gap that would have made coverage stall.
//
// The nightly run reconciles a 45-day window, and that window is where identity changes go to be
// forgotten: when resolution upgrades a 2011 track from a name key to a real Spotify artist, the
// nightly pass never reads that play, so the old row keeps its listening and the new one never
// receives it. Without this rule the resolver would work for two months while the dashboard kept
// reporting the figures from the last full pass.
func TestNightlyFullReconcileExists(t *testing.T) {
	tpl := synth(t, testConfig())

	rules := *tpl.FindResources(jsii.String("AWS::Events::Rule"), map[string]any{
		"Properties": map[string]any{"Name": "spotistats-rollup-full-schedule"},
	})
	if len(rules) != 1 {
		t.Fatalf("full-reconcile schedules = %d, want 1", len(rules))
	}
	for _, res := range rules {
		props := (*res)["Properties"].(map[string]any)
		// 01:15 nightly: BEFORE the 03:15 run, so the leaderboards and coverage that run
		// computes are built on freshly reconciled aggregates the same morning.
		//
		// Nightly, not weekly. The weekly cadence came from a local timing of ~10 minutes; the
		// deployed pass is 1m59s, so there is no reason to make the resolver's progress wait
		// until Sunday to become visible.
		if got, _ := props["ScheduleExpression"].(string); got != "cron(15 1 * * ? *)" {
			t.Errorf("schedule = %q, want nightly at 01:15", got)
		}
		body, err := json.Marshal(props["Targets"])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "reconcileAll") {
			t.Error("the weekly rule does not request reconcileAll, so it would run the " +
				"windowed pass and change nothing")
		}
	}

	// All three rollup schedules drive ONE function: identical code, config and IAM, differing
	// only in how much work the payload asks for.
	fns := *tpl.FindResources(jsii.String("AWS::Lambda::Function"), map[string]any{
		"Properties": map[string]any{"FunctionName": "spotistats-rollup"},
	})
	if len(fns) != 1 {
		t.Errorf("rollup functions = %d, want 1 shared by all three schedules", len(fns))
	}
}
