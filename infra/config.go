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

	// SSMPrefix is where the Spotify credentials live. These parameters are created BY HAND
	// (docs/PREREQUISITES.md step 5) and only referenced here: putting a secret in a
	// CloudFormation template would put it in the template's plaintext history.
	SSMPrefix string
}

const (
	defaultTableName        = "spotistats"
	defaultTimezone         = "Europe/Madrid"
	defaultSSMPrefix        = "/spotistats/spotify"
	defaultCaptureRateHours = 2
	defaultBudgetUSD        = 10
	// defaultLambdaAssetDir is relative to the repository root, where `cdk` is invoked.
	defaultLambdaAssetDir = "bin/lambda"
	// defaultRegion is us-east-1 because CloudFront requires its ACM certificate there, and
	// the whole stack lives in one region to avoid cross-region certificate plumbing.
	defaultRegion = "us-east-1"
)

func stackConfigFromContext(app awscdk.App) (StackConfig, error) {
	c := StackConfig{
		Account: ctxString(app, "account", os.Getenv("CDK_DEFAULT_ACCOUNT")),
		// Deliberately NOT inheriting CDK_DEFAULT_REGION. The region is a design constraint
		// -- CloudFront requires its ACM certificate in us-east-1 and the stack lives in one
		// region to avoid cross-region plumbing -- so it must not depend on whichever region
		// the operator's shell happens to be configured for. Override explicitly with
		// `-c region=...` if that ever changes.
		Region:           ctxString(app, "region", defaultRegion),
		TableName:        ctxString(app, "tableName", defaultTableName),
		Timezone:         ctxString(app, "timezone", defaultTimezone),
		AlarmEmail:       ctxString(app, "alarmEmail", ""),
		SSMPrefix:        ctxString(app, "ssmPrefix", defaultSSMPrefix),
		LambdaAssetDir:   ctxString(app, "lambdaAssetDir", defaultLambdaAssetDir),
		MonthlyBudgetUSD: ctxFloat(app, "monthlyBudgetUsd", defaultBudgetUSD),
		CaptureRateHours: ctxFloat(app, "captureRateHours", defaultCaptureRateHours),
	}
	if c.CaptureRateHours <= 0 {
		return c, fmt.Errorf("captureRateHours must be positive, got %v", c.CaptureRateHours)
	}
	if c.TableName == "" {
		return c, fmt.Errorf("tableName must not be empty")
	}
	// A missing asset directory otherwise panics deep inside jsii with no useful message.
	// Point the operator at the build step instead.
	for _, fn := range []string{"capture"} {
		path := filepath.Join(c.LambdaAssetDir, fn, "bootstrap")
		if _, err := os.Stat(path); err != nil {
			return c, fmt.Errorf("Lambda binary %s not found: run `make lambdas` first (%w)", path, err)
		}
	}
	return c, nil
}

// env returns the CDK environment. When no account is known the stack is environment-
// agnostic, which still synthesises a deployable template.
func (c StackConfig) env() *awscdk.Environment {
	if c.Account == "" {
		return nil
	}
	return &awscdk.Environment{
		Account: jsii.String(c.Account),
		Region:  jsii.String(c.Region),
	}
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
