package main

import (
	"fmt"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	awsiam "github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/jsii-runtime-go"
)

// githubOIDCURL is GitHub's OIDC token issuer.
const githubOIDCURL = "https://token.actions.githubusercontent.com"

// githubOIDCAudience is what AWS requires in the token's `aud` claim.
const githubOIDCAudience = "sts.amazonaws.com"

// globalStackName is the us-east-1 stack, whose outputs the deploy reads for the certificate.
const globalStackName = "SpotistatsGlobalStack"

// cdkBootstrapQualifier is CDK v2's default bootstrap qualifier. The deploy role is allowed to
// assume the bootstrap roles it names; if the account was bootstrapped with a custom qualifier
// this must match it or `cdk deploy` fails with an access error that names no role.
const cdkBootstrapQualifier = "hnb659fds"

// addGitHubDeployRole creates the role GitHub Actions assumes to deploy, via OIDC.
//
// # Why OIDC rather than an access key
//
// A long-lived AWS key in GitHub secrets is a credential that cannot be rotated by the thing
// that holds it, cannot be scoped to a branch, and survives the repository being cloned. OIDC
// issues a token per workflow run, scoped by the trust policy below.
//
// # Least privilege: assume the bootstrap roles, do not replace them
//
// `cdk deploy` already works by assuming the roles CDK bootstrap created, which hold the broad
// CloudFormation permissions. So this role needs almost none of its own: permission to assume
// those, plus the handful of direct calls the deploy targets make outside CloudFormation
// (syncing the web bundle, invalidating the CDN, invoking the rollup to re-render). Granting it
// administrator access "so deploys always work" would make a compromised workflow run equal to
// an account takeover.
func addGitHubDeployRole(stack awscdk.Stack, cfg StackConfig, s *SpotistatsStack) {
	if cfg.GitHubRepo == "" {
		// Opt-in. Nobody deploying from a laptop should get an IAM role they did not ask for,
		// and synth must work without it.
		return
	}

	provider := githubProvider(stack, cfg)

	// The sub claim is what scopes this to one repository and one branch. Without it ANY
	// GitHub Actions workflow in the world could assume the role -- the audience check alone
	// proves only that the token came from GitHub, not from whose repository.
	//
	// The default is applied HERE rather than trusted from the caller. Config loaded from CDK
	// context fills it in, but a hand-built StackConfig does not, and an empty list produces a
	// condition that scopes the role to nothing -- which is the one failure mode that must not
	// be possible to reach by omission.
	refs := cfg.GitHubDeployRefs
	if len(refs) == 0 {
		refs = defaultGitHubDeployRefs
	}
	// Both spellings of this repository, because GitHub decides which one it puts in the token:
	// the mutable "owner/repo" and, where configured, the immutable "owner@id/repo@id". Listing
	// both is not a widening -- each is an exact string, and neither can name another repo.
	//
	// NOT done with a wildcard. `repo:neovasili*/spotistats*:...` would match both forms in one
	// pattern, and would also match a repository called spotistats-anything owned by an account
	// called neovasili-anything, which anyone can create.
	repos := []string{cfg.GitHubRepo}
	if cfg.GitHubRepoImmutable != "" {
		repos = append(repos, cfg.GitHubRepoImmutable)
	}
	subs := make([]interface{}, 0, len(repos)*len(refs))
	for _, repo := range repos {
		for _, ref := range refs {
			subs = append(subs, jsii.String(fmt.Sprintf("repo:%s:%s", repo, ref)))
		}
	}

	role := awsiam.NewRole(stack, jsii.String("GitHubDeployRole"), &awsiam.RoleProps{
		RoleName:           jsii.String("spotistats-github-deploy"),
		Description:        jsii.String("Assumed by GitHub Actions via OIDC to deploy Spotistats"),
		MaxSessionDuration: awscdk.Duration_Hours(jsii.Number(1)),
		AssumedBy: awsiam.NewWebIdentityPrincipal(provider.OpenIdConnectProviderArn(),
			&map[string]interface{}{
				"StringEquals": map[string]interface{}{
					githubClaim("aud"): jsii.String(githubOIDCAudience),
				},
				"StringLike": map[string]interface{}{
					githubClaim("sub"): subs,
				},
			}),
	})

	// 1. cdk deploy: assume the bootstrap roles, which hold the actual deploy permissions.
	//    Both regions, because the certificate lives in a separate us-east-1 stack.
	bootstrapRoles := []string{}
	for _, region := range []string{cfg.Region, CertRegion} {
		for _, kind := range []string{"deploy", "file-publishing", "image-publishing", "lookup"} {
			bootstrapRoles = append(bootstrapRoles, fmt.Sprintf(
				"arn:aws:iam::%s:role/cdk-%s-%s-role-%s-%s",
				*stack.Account(), cdkBootstrapQualifier, kind, *stack.Account(), region))
		}
	}
	role.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String("AssumeCdkBootstrapRoles"),
		Actions:   jsii.Strings("sts:AssumeRole"),
		Resources: jsii.Strings(bootstrapRoles...),
	}))

	// 2. Reading stack outputs, which every deploy target does to find the bucket and
	//    distribution rather than having them hardcoded.
	role.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:     jsii.String("ReadStackOutputs"),
		Actions: jsii.Strings("cloudformation:DescribeStacks"),
		Resources: jsii.Strings(
			fmt.Sprintf("arn:aws:cloudformation:%s:%s:stack/%s/*",
				cfg.Region, *stack.Account(), *stack.StackName()),
			fmt.Sprintf("arn:aws:cloudformation:%s:%s:stack/%s/*",
				CertRegion, *stack.Account(), globalStackName),
		),
	}))

	// 3. Publishing the frontend bundle. Scoped to this bucket: a wildcard here would let a
	//    workflow run write to every bucket in the account.
	s.WebBucket.GrantReadWrite(role, nil)
	s.WebBucket.GrantDelete(role, nil)

	// 4. Invalidating the CDN after publishing. CloudFront invalidation cannot be scoped to a
	//    distribution ARN in IAM -- the action does not support resource-level permissions --
	//    so this is a wildcard by necessity, not by choice. It can only purge caches.
	role.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String("InvalidateCdn"),
		Actions:   jsii.Strings("cloudfront:CreateInvalidation", "cloudfront:GetInvalidation"),
		Resources: jsii.Strings("*"),
	}))

	// 5. Re-rendering the snapshots after a deploy, and updating function code directly for the
	//    fast path that skips CloudFormation.
	role.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:     jsii.String("PublishAndInvokeFunctions"),
		Actions: jsii.Strings("lambda:InvokeFunction", "lambda:UpdateFunctionCode", "lambda:GetFunction"),
		Resources: jsii.Strings(
			*s.Capture.FunctionArn(), *s.Query.FunctionArn(), *s.Rollup.FunctionArn(),
		),
	}))

	awscdk.NewCfnOutput(stack, jsii.String("GitHubDeployRoleArnOutput"), &awscdk.CfnOutputProps{
		Key:         jsii.String("GitHubDeployRoleArn"),
		Value:       role.RoleArn(),
		Description: jsii.String("Set as AWS_DEPLOY_ROLE_ARN in the GitHub repository variables"),
	})
}

// githubProvider returns the account's GitHub OIDC provider, creating it only when asked to.
//
// The provider is ACCOUNT-GLOBAL and there can be exactly one per issuer URL, so a second stack
// -- or an account that already has one from another project -- makes creation fail with
// EntityAlreadyExists. Referencing an existing one is therefore the default, and creation is
// explicit.
func githubProvider(stack awscdk.Stack, cfg StackConfig) awsiam.IOpenIdConnectProvider {
	if cfg.GitHubOIDCProviderArn != "" {
		return awsiam.OpenIdConnectProvider_FromOpenIdConnectProviderArn(
			stack, jsii.String("GitHubOIDC"), jsii.String(cfg.GitHubOIDCProviderArn))
	}
	return awsiam.NewOpenIdConnectProvider(stack, jsii.String("GitHubOIDC"),
		&awsiam.OpenIdConnectProviderProps{
			Url:       jsii.String(githubOIDCURL),
			ClientIds: jsii.Strings(githubOIDCAudience),
			// Thumbprints are accepted but IGNORED for this issuer: since July 2023 AWS
			// validates GitHub's JWKS endpoint against its own trusted root CAs. Pinning one
			// here would be a value that looks load-bearing and is not, and that breaks
			// confusingly if GitHub rotates its certificate.
		})
}

// githubClaim builds a condition key for a claim in GitHub's OIDC token.
func githubClaim(name string) string {
	return strings.TrimPrefix(githubOIDCURL, "https://") + ":" + name
}
