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
//	cdk deploy -c timezone=Europe/Madrid -c monthlyBudgetUsd=10
//
// Everything has a usable default so `cdk synth` works with no arguments and no
// credentials, which is what lets the template be verified in CI.
type StackConfig struct {
	Account string
	Region  string

	TableName string
	Timezone  string

	// MonthlyBudgetUSD is the AWS Budgets threshold. Zero disables the budget.
	MonthlyBudgetUSD float64

	// CaptureRateMinutes is how often the capture Lambda runs.
	//
	// The binding constraint is plays per POLLING WINDOW, not per day: recently-played returns
	// at most 50 items and cannot page back into history, so anything beyond 50 in a single
	// window is lost permanently. An earlier version of this comment reasoned in plays/day
	// ("2 hours = 600/day of headroom"), which is the wrong quantity -- 60 tracks in one busy
	// evening loses 10 of them however quiet the rest of the day was. A gap was in fact
	// recorded in production at a 2-hour interval.
	//
	// 30 minutes needs a sustained track every 36 seconds to saturate, which is unreachable.
	// The cost of the extra runs is a few hundred no-op invocations a month.
	CaptureRateMinutes float64

	// EnrichReservedConcurrency reserves Lambda concurrency for the enrich job. Zero skips it,
	// which is required on an account whose total limit is 10 -- AWS rejects any reservation
	// there. Single-flight is enforced by a store lock regardless; see store.AcquireEnrichLock.
	EnrichReservedConcurrency float64

	// MusicBrainzContact is the contact detail in the User-Agent MusicBrainz requires. Empty
	// makes the enrich Lambda fail at cold start, which is deliberate: an anonymous agent is
	// throttled far harder as a class, so failing loudly beats working badly.
	MusicBrainzContact string

	// BiographyLanguage is the single biography language stored.
	BiographyLanguage string

	// GitHubRepo is "owner/repo". Empty disables the CI/CD role entirely: nobody deploying
	// from a laptop should acquire an IAM role they did not ask for.
	GitHubRepo string

	// GitHubRepoImmutable is the same repository written with GitHub's immutable identifiers,
	// "owner@ownerID/repo@repoID" -- e.g. "neovasili@6529592/spotistats@1342671978".
	//
	// GitHub has begun minting OIDC tokens whose `sub` names the repository by ID rather than
	// by name, so a trust policy that only lists the mutable "owner/repo" form is rejected with
	// "Not authorized to perform sts:AssumeRoleWithWebIdentity". Both forms are listed, because
	// which one arrives is GitHub's choice and not ours; the IDs are public data.
	//
	// Find it with: gh api repos/OWNER/REPO/actions/oidc/customization/sub --jq .sub_claim_prefix
	// or read it straight off a denied AssumeRoleWithWebIdentity event in CloudTrail.
	//
	// Empty simply omits the second form.
	GitHubRepoImmutable string

	// GitHubDeployRefs are the `sub` claim suffixes allowed to assume the deploy role, e.g.
	// "ref:refs/heads/main". This is the ONLY thing scoping the role to this repository --
	// the audience check proves a token came from GitHub, not from whose repository.
	GitHubDeployRefs []string

	// GitHubOIDCProviderArn references an existing account-global provider. Empty creates one,
	// which fails with EntityAlreadyExists if the account already has it from another project.
	GitHubOIDCProviderArn string

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
	// every CaptureRateMinutes with a 120s timeout and no EventBridge retry. The query function
	// is bounded by the API Gateway stage throttle (20 rps, 40 burst) and by the budget alarm,
	// which docs/SPECS.md 10.3 already names as the controls that matter given there is no WAF.
	CaptureReservedConcurrency float64
	QueryReservedConcurrency   float64
	RollupReservedConcurrency  float64

	// SSMPrefix is where the Spotify credentials live. These parameters are created BY HAND
	// (docs/PREREQUISITES.md step 5) and only referenced here: putting a secret in a
	// CloudFormation template would put it in the template's plaintext history.
	SSMPrefix string
}

// lambdaFunctions must match the LAMBDAS variable in the Makefile.
var lambdaFunctions = []string{"capture", "query", "rollup", "enrich", "notify", "resolve"}

const (
	defaultTableName          = "spotistats"
	defaultTimezone           = "Europe/Madrid"
	defaultSSMPrefix          = "/spotistats/spotify"
	defaultCaptureRateMinutes = 30.0
	defaultBudgetUSD          = 10
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
		Region:     ctxString(app, "region", defaultRegion),
		TableName:  ctxString(app, "tableName", defaultTableName),
		Timezone:   ctxString(app, "timezone", defaultTimezone),
		SSMPrefix:  ctxString(app, "ssmPrefix", defaultSSMPrefix),
		DomainName: ctxString(app, "domainName", ""),
		// The zone ID is account-specific, so unlike domainName/hostedZoneName it is NOT in
		// cdk.json. It arrives from the environment (or `-c hostedZoneId=`), the same way the
		// account does. A missing value trips the both-or-neither check below rather than
		// silently synthesising a stack with no alias records.
		HostedZoneID:               ctxString(app, "hostedZoneId", envOr("SPOTISTATS_HOSTED_ZONE_ID", "")),
		HostedZoneName:             ctxString(app, "hostedZoneName", ""),
		LambdaAssetDir:             ctxString(app, "lambdaAssetDir", defaultLambdaAssetDir),
		CaptureReservedConcurrency: ctxFloat(app, "captureReservedConcurrency", 0),
		QueryReservedConcurrency:   ctxFloat(app, "queryReservedConcurrency", 0),
		RollupReservedConcurrency:  ctxFloat(app, "rollupReservedConcurrency", 0),
		MonthlyBudgetUSD:           ctxFloat(app, "monthlyBudgetUsd", defaultBudgetUSD),
		CaptureRateMinutes:         ctxFloat(app, "captureRateMinutes", defaultCaptureRateMinutes),
		EnrichReservedConcurrency:  ctxFloat(app, "enrichReservedConcurrency", 0),
		MusicBrainzContact:         ctxString(app, "musicbrainzContact", ""),
		BiographyLanguage:          ctxString(app, "biographyLanguage", "en"),
		GitHubRepo:                 ctxString(app, "githubRepo", ""),
		GitHubRepoImmutable:        ctxString(app, "githubRepoImmutable", ""),
		GitHubOIDCProviderArn:      ctxString(app, "githubOidcProviderArn", ""),
		GitHubDeployRefs:           ctxStrings(app, "githubDeployRefs", defaultGitHubDeployRefs),
	}
	if c.CaptureRateMinutes <= 0 {
		return c, fmt.Errorf("captureRateMinutes must be positive, got %v", c.CaptureRateMinutes)
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

// defaultGitHubDeployRefs restricts the deploy role to main and to the production environment.
// Deliberately not a wildcard: a role assumable from any branch is assumable from a branch
// opened by a fork's pull request.
//
// BOTH entries are required, because the two are alternatives rather than an intersection.
// GitHub swaps the `sub` claim depending on the job: a job that declares `environment:` gets
// `repo:owner/repo:environment:NAME`, and only a job WITHOUT one gets the
// `repo:owner/repo:ref:refs/heads/main` form. deploy.yml declares `environment: production`,
// so the ref entry alone rejected every real deploy with "Not authorized to perform
// sts:AssumeRoleWithWebIdentity" -- which is what happens when no sub pattern matches.
//
// Listing both keeps the boundary tight. `environment:production` is not weaker than the ref:
// the environment is defined in this repository, so no fork can select it, and an environment
// with required reviewers is a stronger gate than a branch name. The ref entry stays for a
// deploy job that runs without an environment.
var defaultGitHubDeployRefs = []string{"ref:refs/heads/main", "environment:production"}

// ctxStrings reads a JSON array of strings from CDK context.
func ctxStrings(scope constructs.IConstruct, key string, def []string) []string {
	v := scope.Node().TryGetContext(jsii.String(key))
	raw, ok := v.([]interface{})
	if !ok || len(raw) == 0 {
		return def
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
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
