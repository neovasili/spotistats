package main

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	awsevents "github.com/aws/aws-cdk-go/awscdk/v2/awsevents"
	awseventstargets "github.com/aws/aws-cdk-go/awscdk/v2/awseventstargets"
	awsiam "github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	awslambda "github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	awslogs "github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/jsii-runtime-go"
	"github.com/neovasili/spotistats/internal/backfill"
)

// newResolveFunction is the track-identity job: it upgrades placeholder track rows to real
// Spotify identity, a bounded batch per night.
//
// # The quota is shared with capture, and that governs everything here
//
// The imported history carries no artist or album IDs, so every track starts as a placeholder
// whose identity is derived from the export's text (docs/SPECS.md 4.2.1). Upgrading one costs a
// single Spotify request, and there are twelve thousand of them.
//
// Capture spends the same quota, and capture is the one job whose failure is unrecoverable:
// recently-played is a rolling ~50-play window, so consecutive failures lose listening
// permanently. That is why this function is budgeted at a couple of hundred tracks rather than
// running until the API refuses — and why a 429 writes a cooldown row that stops the NEXT run
// from asking at all, rather than merely stopping this one from succeeding.
//
// The cost of that caution, stated plainly: the backlog takes about two months to drain. Nothing
// downstream is waiting on it — the plays are already stored, and each nightly reconcile moves
// whatever resolved that day onto real identity.
func (s *SpotistatsStack) newResolveFunction(stack awscdk.Stack, cfg StackConfig) awslambda.Function {
	logGroup := awslogs.NewLogGroup(stack, jsii.String("ResolveLogs"), &awslogs.LogGroupProps{
		LogGroupName:  jsii.String("/aws/lambda/spotistats-resolve"),
		Retention:     awslogs.RetentionDays_TWO_WEEKS,
		RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
	})

	fn := awslambda.NewFunction(stack, jsii.String("Resolve"), &awslambda.FunctionProps{
		FunctionName: jsii.String("spotistats-resolve"),
		Description:  jsii.String("Nightly: upgrade placeholder track rows to real Spotify identity"),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String(filepath.Join(cfg.LambdaAssetDir, "resolve")), nil),
		// It holds one track's metadata at a time and is entirely I/O-bound.
		MemorySize: jsii.Number(512),
		// Generous relative to the work: a 200-track batch at the client's own request rate is a
		// couple of minutes. The margin is for a slow upstream, not for a bigger batch — the
		// batch is bounded by the quota, not by the clock.
		Timeout:  awscdk.Duration_Minutes(jsii.Number(10)),
		LogGroup: logGroup,
		Environment: &map[string]*string{
			"SPOTISTATS_TABLE_NAME":    jsii.String(cfg.TableName),
			"SPOTISTATS_TIMEZONE":      jsii.String(cfg.Timezone),
			"SPOTISTATS_SSM_PREFIX":    jsii.String(cfg.SSMPrefix),
			"SPOTISTATS_RESOLVE_LIMIT": jsii.String(strconv.Itoa(backfill.DefaultResolveLimit)),
			"SPOTISTATS_LOG_LEVEL":     jsii.String("info"),
		},
	})

	// Reads the track work list and writes the dimension rows a resolution produces. DeleteItem
	// is withheld: this job only ever upgrades a row's identity, never removes one.
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"dynamodb:GetItem",
			"dynamodb:BatchGetItem",
			"dynamodb:PutItem",
			"dynamodb:Query",
		),
		Resources: jsii.Strings(*s.Table.TableArn()),
	}))

	// The Spotify credentials, including WRITING the refresh token: Spotify may rotate it on any
	// refresh, and dropping the replacement would break every future run of this job AND of
	// capture, since they share the one token.
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"ssm:GetParameter",
			"ssm:GetParameters",
			"ssm:PutParameter",
		),
		Resources: jsii.Strings(fmt.Sprintf("arn:aws:ssm:%s:%s:parameter%s/*",
			*stack.Region(), *stack.Account(), cfg.SSMPrefix)),
	}))
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("kms:Decrypt", "kms:Encrypt", "kms:GenerateDataKey"),
		Resources: jsii.Strings("*"),
		Conditions: &map[string]interface{}{
			"StringEquals": map[string]interface{}{
				"kms:ViaService": fmt.Sprintf("ssm.%s.amazonaws.com", *stack.Region()),
			},
		},
	}))

	return fn
}

// scheduleResolve runs the job every six hours.
//
// 05:15 UTC as the start, which is after the rollup (03:15) and the external enrichment (04:15),
// so the nightly jobs never overlap. Ordering matters beyond tidiness: the rollup rewrites the
// aggregates this job derives its work list from.
//
// # Why four attempts a day rather than one
//
// It was once a day, reasoning that the batch size keeps the quota safe and running more often
// would double the spend. The first half was right; the second was wrong, and the schedule missed
// its very first real window because of it.
//
// A 429 writes a cooldown carrying Spotify's own Retry-After, and observed values run 7.5 to 18
// hours -- so the window reopens at an arbitrary hour. A once-daily run at 05:15 that finds a
// cooldown lapsing at 08:05 does nothing and waits until the next morning: a whole day lost to
// three hours of bad luck. That is precisely what happened on the first night.
//
// Attempting more often costs NOTHING while a cooldown is active -- the run reads one item, spends
// no quota, and returns in about a millisecond. So the extra attempts buy a shorter wait for the
// next window without buying any risk, and the batch limit still governs how much is ever spent.
func (s *SpotistatsStack) scheduleResolve(stack awscdk.Stack) {
	rule := awsevents.NewRule(stack, jsii.String("ResolveSchedule"), &awsevents.RuleProps{
		RuleName: jsii.String("spotistats-resolve-schedule"),
		Description: jsii.String("Track-identity resolution every 6h from 05:15 UTC. A run " +
			"during a rate-limit cooldown spends no quota, so frequent attempts only shorten " +
			"the wait for the next window"),
		Schedule: awsevents.Schedule_Cron(&awsevents.CronOptions{
			Minute: jsii.String("15"),
			Hour:   jsii.String("5/6"),
		}),
	})
	rule.AddTarget(awseventstargets.NewLambdaFunction(s.Resolve, &awseventstargets.LambdaFunctionProps{
		// No retry. The next attempt is six hours away and costs nothing if the quota is still
		// spent, so retrying now would only risk taking quota capture needs.
		RetryAttempts: jsii.Number(0),
	}))
}
