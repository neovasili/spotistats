package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
)

// StackConfig is the deployment-time configuration, supplied via CDK context:
//
//	cdk deploy -c alarmEmail=me@example.com -c timezone=Europe/Madrid
//
// Everything has a usable default so `cdk synth` works with no arguments and no
// credentials, which is what lets the template be verified in CI.
type StackConfig struct {
	Account string
	Region  string

	TableName string
	Timezone  string

	// AlarmEmail receives operational alarms. Empty means the SNS topic is still created but
	// left unsubscribed, so synth works before the address is decided.
	AlarmEmail string

	// MonthlyBudgetUSD is the AWS Budgets threshold. Zero disables the budget.
	MonthlyBudgetUSD float64

	// CaptureRateHours is how often the capture Lambda runs. Two hours gives roughly 600
	// plays/day of headroom against the endpoint's 50-item page; see docs/SPECS.md 2.1 for
	// why nightly is not an option.
	CaptureRateHours float64

	// LambdaAssetDir is the directory holding each function's `bootstrap` binary, one
	// subdirectory per function. Asset paths resolve relative to the CDK app process's
	// working directory, which is the repository root for `cdk synth` but the package
	// directory under `go test` -- hence configurable rather than a constant.
	LambdaAssetDir string

	// DomainName is the custom domain for the site. Empty means the distribution is reachable
	// only on its own *.cloudfront.net name, which is deliberate: it lets the stack deploy
	// before the subdomain has been chosen (docs/SPECS.md 14 decision 1).
	DomainName string

	// HostedZoneID and HostedZoneName enable automatic certificate validation and alias
	// records. Without them the operator adds the DNS records by hand
	// (docs/PREREQUISITES.md step 7 path C).
	HostedZoneID   string
	HostedZoneName string

	// CaptureReservedConcurrency and QueryReservedConcurrency bound each function's
	// concurrency. Zero means unreserved, which is the DEFAULT and is required on an account
	// whose concurrent-execution quota has not been raised.
	//
	// AWS insists at least 10 unreserved concurrency remain available account-wide, so on the
	// new-account default quota of 10 ANY reservation is rejected outright -- it is not a
	// matter of reserving less. Raise "Concurrent executions" in Service Quotas above roughly
	// 20 and then set these to re-enable the bounds.
	//
	// Nothing critical depends on them. Capture cannot overlap itself regardless: it runs
	// every CaptureRateHours with a 120s timeout and no EventBridge retry. The query function
	// is bounded by the API Gateway stage throttle (20 rps, 40 burst) and by the budget alarm,
	// which docs/SPECS.md 10.3 already names as the controls that matter given there is no WAF.
	CaptureReservedConcurrency float64
	QueryReservedConcurrency   float64

	// SSMPrefix is where the Spotify credentials live. These parameters are created BY HAND
	// (docs/PREREQUISITES.md step 5) and only referenced here: putting a secret in a
	// CloudFormation template would put it in the template's plaintext history.
	SSMPrefix string
}

// lambdaFunctions must match the LAMBDAS variable in the Makefile.
var lambdaFunctions = []string{"capture", "query"}

const (
	defaultTableName        = "spotistats"
	defaultTimezone         = "Europe/Madrid"
	defaultSSMPrefix        = "/spotistats/spotify"
	defaultCaptureRateHours = 2
	defaultBudgetUSD        = 10
	// defaultLambdaAssetDir is relative to the repository root, where `cdk` is invoked.
	defaultLambdaAssetDir = "bin/lambda"
	// defaultRegion is where the data and compute live.
	defaultRegion = "eu-west-1"

	// CertRegion is fixed. CloudFront only accepts an ACM certificate from us-east-1, which
	// is the sole reason this deployment spans two regions at all: the certificate (and the
	// billing budget, which is likewise global) live in a separate us-east-1 stack, and
	// everything else lives in defaultRegion.
	CertRegion = "us-east-1"
)

func stackConfigFromContext(app awscdk.App) (StackConfig, error) {
	c := StackConfig{
		Account: ctxString(app, "account", envOr("CDK_DEFAULT_ACCOUNT", SynthOnlyAccount)),
		// Deliberately NOT inheriting CDK_DEFAULT_REGION: the region is a deployment decision
		// recorded in cdk.json, not a property of whichever region the operator's shell
		// happens to be configured for. Override explicitly with `-c region=...`.
		Region:                     ctxString(app, "region", defaultRegion),
		TableName:                  ctxString(app, "tableName", defaultTableName),
		Timezone:                   ctxString(app, "timezone", defaultTimezone),
		AlarmEmail:                 ctxString(app, "alarmEmail", ""),
		SSMPrefix:                  ctxString(app, "ssmPrefix", defaultSSMPrefix),
		DomainName:                 ctxString(app, "domainName", ""),
		HostedZoneID:               ctxString(app, "hostedZoneId", ""),
		HostedZoneName:             ctxString(app, "hostedZoneName", ""),
		LambdaAssetDir:             ctxString(app, "lambdaAssetDir", defaultLambdaAssetDir),
		CaptureReservedConcurrency: ctxFloat(app, "captureReservedConcurrency", 0),
		QueryReservedConcurrency:   ctxFloat(app, "queryReservedConcurrency", 0),
		MonthlyBudgetUSD:           ctxFloat(app, "monthlyBudgetUsd", defaultBudgetUSD),
		CaptureRateHours:           ctxFloat(app, "captureRateHours", defaultCaptureRateHours),
	}
	if c.CaptureRateHours <= 0 {
		return c, fmt.Errorf("captureRateHours must be positive, got %v", c.CaptureRateHours)
	}
	if c.TableName == "" {
		return c, fmt.Errorf("tableName must not be empty")
	}
	// A missing asset directory otherwise panics deep inside jsii with no useful message.
	// Point the operator at the build step instead.
	for _, fn := range lambdaFunctions {
		path := filepath.Join(c.LambdaAssetDir, fn, "bootstrap")
		if _, err := os.Stat(path); err != nil {
			return c, fmt.Errorf("Lambda binary %s not found: run `make lambdas` first (%w)", path, err)
		}
	}
	// A hosted zone needs both halves to be usable; one alone is a misconfiguration that would
	// otherwise silently fall back to manual DNS.
	if (c.HostedZoneID == "") != (c.HostedZoneName == "") {
		return c, fmt.Errorf("hostedZoneId and hostedZoneName must be given together")
	}
	if (c.HostedZoneID != "") && c.DomainName == "" {
		return c, fmt.Errorf("hostedZoneId requires domainName")
	}
	return c, nil
}

// SynthOnlyAccount stands in when no account is known.
//
// A concrete account is required rather than an environment-agnostic stack, because CDK
// cannot resolve a cross-region reference without one -- and the certificate ARN crosses from
// us-east-1 into the regional stack. The placeholder keeps `cdk synth` working with no AWS
// credentials, which is what lets CI review the template; the resulting output is structurally
// valid but not deployable. The real account arrives via CDK_DEFAULT_ACCOUNT, which the CDK
// CLI populates from the active credentials, or via `-c account=`.
const SynthOnlyAccount = "000000000000"

// IsSynthOnly reports whether no real account was resolved.
func (c StackConfig) IsSynthOnly() bool { return c.Account == SynthOnlyAccount }

// env returns the environment for the regional stack.
func (c StackConfig) env() *awscdk.Environment {
	return &awscdk.Environment{
		Account: jsii.String(c.Account),
		Region:  jsii.String(c.Region),
	}
}

// globalEnv returns the environment for the us-east-1 stack holding the certificate and the
// budget.
func (c StackConfig) globalEnv() *awscdk.Environment {
	return &awscdk.Environment{
		Account: jsii.String(c.Account),
		Region:  jsii.String(CertRegion),
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func ctxString(scope constructs.IConstruct, key, def string) string {
	if v := scope.Node().TryGetContext(jsii.String(key)); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return def
}

func ctxFloat(scope constructs.IConstruct, key string, def float64) float64 {
	v := scope.Node().TryGetContext(jsii.String(key))
	if v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case string:
		var f float64
		if _, err := fmt.Sscanf(n, "%g", &f); err == nil {
			return f
		}
	}
	return def
}
