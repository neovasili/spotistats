package main

import (
	"path/filepath"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	awsevents "github.com/aws/aws-cdk-go/awscdk/v2/awsevents"
	awseventstargets "github.com/aws/aws-cdk-go/awscdk/v2/awseventstargets"
	awsiam "github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	awslambda "github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	awslogs "github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/jsii-runtime-go"
	"github.com/neovasili/spotistats/internal/store"
)

// newEnrichFunction is the external-enrichment job: MusicBrainz facts plus TheAudioDB prose and
// artwork for every artist ever played.
func (s *SpotistatsStack) newEnrichFunction(stack awscdk.Stack, cfg StackConfig) awslambda.Function {
	logGroup := awslogs.NewLogGroup(stack, jsii.String("EnrichLogs"), &awslogs.LogGroupProps{
		LogGroupName:  jsii.String("/aws/lambda/spotistats-enrich"),
		Retention:     awslogs.RetentionDays_TWO_WEEKS,
		RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
	})

	fn := awslambda.NewFunction(stack, jsii.String("Enrich"), &awslambda.FunctionProps{
		FunctionName: jsii.String("spotistats-enrich"),
		Description:  jsii.String("Daily MusicBrainz + TheAudioDB artist enrichment"),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String(filepath.Join(cfg.LambdaAssetDir, "enrich")), nil),
		// Modest: the job is entirely I/O-bound on two rate-limited upstreams, so more memory
		// buys nothing. It holds one artist's profile at a time.
		MemorySize: jsii.Number(512),
		// Five minutes, not fifteen. At MusicBrainz's 1 req/s a longer run would not finish the
		// library anyway, and the job is resumable -- it checkpoints per artist, so being cut
		// off costs nothing but the next run picks up where this one stopped. A short timeout
		// also bounds how long a wedged upstream can hold the single concurrency slot.
		Timeout:  awscdk.Duration_Minutes(jsii.Number(5)),
		LogGroup: logGroup,
		Environment: &map[string]*string{
			"SPOTISTATS_TABLE_NAME":          jsii.String(cfg.TableName),
			"SPOTISTATS_TIMEZONE":            jsii.String(cfg.Timezone),
			"SPOTISTATS_SSM_PREFIX":          jsii.String(cfg.SSMPrefix),
			"SPOTISTATS_MUSICBRAINZ_CONTACT": jsii.String(cfg.MusicBrainzContact),
			"SPOTISTATS_BIOGRAPHY_LANGUAGE":  jsii.String(cfg.BiographyLanguage),
			"SPOTISTATS_LOG_LEVEL":           jsii.String("info"),
		},
	})

	// Reserved concurrency 1, when the account can grant it.
	//
	// Both upstream rate limits are per-IP, so two overlapping runs double the real rate and
	// MusicBrainz answers 503 to EVERYTHING from that IP -- including the other run.
	//
	// It is OPT-IN because AWS rejects any reservation on an account whose total concurrency
	// limit is 10: it requires 10 to remain unreserved, so even a reservation of 1 fails with
	// "decreases account's UnreservedConcurrentExecution below its minimum value of [10]".
	// This deployment is on such an account.
	//
	// Single-flight therefore does NOT depend on this. store.AcquireEnrichLock enforces it with
	// a conditional write, which needs no quota and additionally covers a case a reservation
	// never did: a manual `aws lambda invoke` landing during the scheduled run.
	if cfg.EnrichReservedConcurrency > 0 {
		if cfnFn, ok := fn.Node().DefaultChild().(awslambda.CfnFunction); ok {
			cfnFn.SetReservedConcurrentExecutions(jsii.Number(cfg.EnrichReservedConcurrency))
		}
	}

	// Reads the artist work list and writes EXTERNAL rows. No DeleteItem on the table at
	// large: this job never removes an artist, and an unresolvable one is recorded as a
	// tombstone rather than deleted.
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

	// DeleteItem, but ONLY in the STATE partition.
	//
	// store.ReleaseEnrichLock deletes STATE/EXTERNAL_ENRICH_LOCK, and omitting this action
	// was a real defect: every run ended with "not authorized to perform dynamodb:DeleteItem",
	// the lock was never released, and it took the full 15-minute TTL to expire -- so a retry
	// after a failed run found the lock held and exited having done nothing.
	//
	// LeadingKeys confines it to the partition holding cursors, markers and locks. The job
	// still cannot delete a play, an aggregate or an artist, which is what the restriction is
	// actually for.
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("dynamodb:DeleteItem"),
		Resources: jsii.Strings(*s.Table.TableArn()),
		Conditions: &map[string]interface{}{
			"ForAllValues:StringEquals": map[string]interface{}{
				"dynamodb:LeadingKeys": []string{store.PKState},
			},
		},
	}))

	// TheAudioDB key only. Scoped to the one parameter rather than the prefix: this job has no
	// business reading the Spotify refresh token, and a wildcard here would let it.
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Effect:  awsiam.Effect_ALLOW,
		Actions: jsii.Strings("ssm:GetParameter"),
		Resources: jsii.Strings(
			"arn:aws:ssm:" + cfg.Region + ":" + *stack.Account() +
				":parameter" + cfg.SSMPrefix + "/theaudiodb_key",
		),
	}))

	return fn
}

// scheduleEnrich runs the job daily, an hour after the rollup.
func (s *SpotistatsStack) scheduleEnrich(stack awscdk.Stack) {
	rule := awsevents.NewRule(stack, jsii.String("EnrichSchedule"), &awsevents.RuleProps{
		RuleName: jsii.String("spotistats-enrich-schedule"),
		Description: jsii.String("Daily external artist enrichment at 04:15 UTC, an hour " +
			"after the rollup so the two never overlap"),
		// An hour after the rollup's 03:15. Not merely tidy: the rollup rewrites the artist
		// aggregates this job reads as its work list, and overlapping them would mean enriching
		// against a partially rewritten list.
		Schedule: awsevents.Schedule_Cron(&awsevents.CronOptions{
			Minute: jsii.String("15"),
			Hour:   jsii.String("4"),
		}),
	})
	rule.AddTarget(awseventstargets.NewLambdaFunction(s.Enrich, &awseventstargets.LambdaFunctionProps{
		// No retry. The job is resumable and daily, so a failed run costs a day of latency on
		// data that changes on a scale of years -- whereas a retry against a 503ing MusicBrainz
		// spends the rate limit making the problem worse.
		RetryAttempts: jsii.Number(0),
	}))
}
