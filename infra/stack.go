package main

import (
	"fmt"
	"path/filepath"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsapigatewayv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscertificatemanager"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudfront"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatch"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatchactions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsevents"
	"github.com/aws/aws-cdk-go/awscdk/v2/awseventstargets"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awss3"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/neovasili/spotistats/internal/metrics"
	"github.com/neovasili/spotistats/internal/store"
)

// SpotistatsStackProps configures the regional stack.
type SpotistatsStackProps struct {
	awscdk.StackProps
	Config StackConfig

	// Certificate comes from the us-east-1 stack, because CloudFront accepts a certificate
	// only from that region. Nil means no custom domain, in which case the distribution is
	// reachable on its own *.cloudfront.net name.
	Certificate awscertificatemanager.ICertificate
}

// SpotistatsStack is the whole deployment.
type SpotistatsStack struct {
	awscdk.Stack
	Table        awsdynamodb.Table
	Capture      awslambda.Function
	Query        awslambda.Function
	Rollup       awslambda.Function
	Enrich       awslambda.Function
	Notify       awslambda.Function
	AlarmTopic   awssns.Topic
	certificate  awscertificatemanager.ICertificate
	WebBucket    awss3.Bucket
	HTTPAPI      awsapigatewayv2.HttpApi
	Distribution awscloudfront.Distribution
}

// NewSpotistatsStack provisions the stack.
func NewSpotistatsStack(scope constructs.Construct, id string, props *SpotistatsStackProps) *SpotistatsStack {
	stack := awscdk.NewStack(scope, jsii.String(id), &props.StackProps)
	cfg := props.Config
	s := &SpotistatsStack{Stack: stack, certificate: props.Certificate}

	s.Table = newTable(stack, cfg)
	s.AlarmTopic = newAlarmTopic(stack, cfg)
	// The notifier is created before the alarms so every alarm can be wired to a topic that
	// already has a subscriber. An alarm attached to an unsubscribed topic is the exact
	// failure this replaced.
	s.Notify = s.newNotifyFunction(stack, cfg)
	s.subscribeNotifier(s.Notify)
	s.Capture = s.newCaptureFunction(stack, cfg)
	s.scheduleCapture(stack, cfg)
	s.addWeb(stack, cfg)
	s.addAlarms(stack, cfg)
	s.addBudget(stack, cfg)

	// The CI/CD role last: it grants access to the bucket and functions created above, so it
	// has to see them.
	addGitHubDeployRole(stack, cfg, s)
	s.addOutputs(stack, cfg)

	return s
}

// newTable builds the DynamoDB table from store.Schema.
//
// Deriving it from the same declaration the integration tests create their tables from is
// what makes drift impossible: a test could otherwise pass against a shape production does
// not have. TestTableSchemaParity asserts the two stay aligned.
func newTable(stack awscdk.Stack, cfg StackConfig) awsdynamodb.Table {
	table := awsdynamodb.NewTable(stack, jsii.String("Table"), &awsdynamodb.TableProps{
		TableName: jsii.String(cfg.TableName),
		PartitionKey: &awsdynamodb.Attribute{
			Name: jsii.String(store.Schema.PartitionKey),
			Type: awsdynamodb.AttributeType_STRING,
		},
		SortKey: &awsdynamodb.Attribute{
			Name: jsii.String(store.Schema.SortKey),
			Type: awsdynamodb.AttributeType_STRING,
		},
		// On-demand: traffic is a handful of writes per hour with a large one-off backfill,
		// which is the exact shape provisioned capacity handles badly.
		BillingMode: awsdynamodb.BillingMode_PAY_PER_REQUEST,
		// Point-in-time recovery is the only defence against a bad backfill or a bug that
		// corrupts aggregates. The raw plays are irreplaceable -- the API cannot re-serve
		// history.
		PointInTimeRecoverySpecification: &awsdynamodb.PointInTimeRecoverySpecification{
			PointInTimeRecoveryEnabled: jsii.Bool(true),
		},
		// RETAIN: a `cdk destroy` must never be able to delete years of listening history.
		RemovalPolicy: awscdk.RemovalPolicy_RETAIN,
		Encryption:    awsdynamodb.TableEncryption_AWS_MANAGED,
	})

	for _, idx := range store.Schema.Indexes {
		projection := awsdynamodb.ProjectionType_KEYS_ONLY
		var nonKey *[]*string
		if len(idx.Projected) > 0 {
			projection = awsdynamodb.ProjectionType_INCLUDE
			nonKey = jsii.Strings(idx.Projected...)
		}
		table.AddGlobalSecondaryIndex(&awsdynamodb.GlobalSecondaryIndexProps{
			IndexName: jsii.String(idx.Name),
			PartitionKey: &awsdynamodb.Attribute{
				Name: jsii.String(idx.PartitionKey),
				Type: awsdynamodb.AttributeType_STRING,
			},
			SortKey: &awsdynamodb.Attribute{
				Name: jsii.String(idx.SortKey),
				Type: awsdynamodb.AttributeType_STRING,
			},
			ProjectionType:   projection,
			NonKeyAttributes: nonKey,
		})
	}

	return table
}

// newAlarmTopic creates the topic every alarm and the budget publish to.
//
// There is no email subscription. The subscriber is the notifier Lambda, created in this same
// stack, and that is the property worth having: no configuration value's absence can leave the
// topic silently unsubscribed. That WAS the defect here once -- alarmEmail unset meant no
// subscription and no budget, with no complaint from synth.
//
// The topic's subscriber is attached by subscribeNotifier rather than here, because the notifier
// needs the stack and config that only the SpotistatsStack method has.
func newAlarmTopic(stack awscdk.Stack, cfg StackConfig) awssns.Topic {
	topic := awssns.NewTopic(stack, jsii.String("Alarms"), &awssns.TopicProps{
		DisplayName: jsii.String("Spotistats alarms"),
	})

	// Budgets publishes as a service principal, which a topic does not allow by default. Grant
	// it here, next to the topic, so the budget cannot be moved without the grant coming along.
	grantBudgetPublish(topic, stack)

	// The webhook lives in SSM, so CDK cannot check it exists -- but it CAN check that someone
	// has said where it should be. An empty parameter name means the notifier will fail on its
	// first invocation, which is better than silence but still worth saying at synth time.
	if cfg.SSMPrefix == "" {
		awscdk.Annotations_Of(stack).AddWarning(jsii.String(
			"ssmPrefix is empty, so the notifier cannot resolve the Slack webhook parameter " +
				"and every alarm will fail to deliver."))
	}
	return topic
}

// newCaptureFunction builds the scheduled capture Lambda.
func (s *SpotistatsStack) newCaptureFunction(stack awscdk.Stack, cfg StackConfig) awslambda.Function {
	logGroup := awslogs.NewLogGroup(stack, jsii.String("CaptureLogs"), &awslogs.LogGroupProps{
		LogGroupName: jsii.String("/aws/lambda/spotistats-capture"),
		// Two weeks is plenty: the durable record of what happened is in DynamoDB, and logs
		// are for debugging a recent run.
		Retention:     awslogs.RetentionDays_TWO_WEEKS,
		RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
	})

	fn := awslambda.NewFunction(stack, jsii.String("Capture"), &awslambda.FunctionProps{
		FunctionName: jsii.String("spotistats-capture"),
		Description:  jsii.String("Ingests the Spotify recently-played feed"),
		// provided.al2023 with a Go binary named `bootstrap`. arm64 is cheaper per
		// millisecond than x86_64 and Go cross-compiles to it cleanly.
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String(filepath.Join(cfg.LambdaAssetDir, "capture")), nil),
		MemorySize:   jsii.Number(512),
		Timeout:      awscdk.Duration_Seconds(jsii.Number(120)),
		LogGroup:     logGroup,
		Environment: &map[string]*string{
			"SPOTISTATS_TABLE_NAME": jsii.String(cfg.TableName),
			"SPOTISTATS_TIMEZONE":   jsii.String(cfg.Timezone),
			"SPOTISTATS_SSM_PREFIX": jsii.String(cfg.SSMPrefix),
			// SPOTISTATS_REGION is deliberately NOT set: the Lambda runtime always provides
			// AWS_REGION, and config.Load prefers it. Pinning a region here could disagree
			// with where the function actually runs.
			"SPOTISTATS_LOG_LEVEL": jsii.String("info"),
		},
	})

	// Applied only when configured: on an account whose concurrency quota has not been raised,
	// any reservation is rejected. See StackConfig.CaptureReservedConcurrency.
	if cfg.CaptureReservedConcurrency > 0 {
		cfnFn, ok := fn.Node().DefaultChild().(awslambda.CfnFunction)
		if ok {
			cfnFn.SetReservedConcurrentExecutions(jsii.Number(cfg.CaptureReservedConcurrency))
		}
	}

	// Least privilege, enumerated rather than GrantReadWriteData: that helper also grants
	// Scan and DeleteItem, neither of which the capture path ever performs. A future change
	// that needs another action fails loudly at deploy time, which is the right failure mode.
	//
	// BatchGetItem is required and docs/SPECS.md 10.1 omits it -- the artist, track and album
	// lookups are batched.
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"dynamodb:GetItem",
			"dynamodb:BatchGetItem",
			"dynamodb:PutItem",
			"dynamodb:UpdateItem",
			"dynamodb:Query",
		),
		Resources: jsii.Strings(
			*s.Table.TableArn(),
			*s.Table.TableArn()+"/index/*",
		),
	}))

	// The Spotify credentials are read, and the refresh token is also WRITTEN: Spotify may
	// issue a replacement on any refresh, and losing it means no future run can authenticate.
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
	// SecureString parameters are encrypted with the account's default SSM key.
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

// alarmWindowFor returns the metric period for the capture alarms: three capture intervals,
// with a three-hour floor. See the call site for why it is not simply a multiple.
func alarmWindowFor(captureRateMinutes float64) float64 {
	const floorMinutes = 180.0
	if w := 3 * captureRateMinutes; w > floorMinutes {
		return w
	}
	return floorMinutes
}

func (s *SpotistatsStack) scheduleCapture(stack awscdk.Stack, cfg StackConfig) {
	rule := awsevents.NewRule(stack, jsii.String("CaptureSchedule"), &awsevents.RuleProps{
		RuleName: jsii.String("spotistats-capture-schedule"),
		Description: jsii.String(fmt.Sprintf(
			"Runs the capture Lambda every %g minutes. The limit is plays per POLLING WINDOW, "+
				"not per day: recently-played returns at most 50 items and cannot page back, "+
				"so anything past 50 in one window is lost permanently.", cfg.CaptureRateMinutes)),
		Schedule: awsevents.Schedule_Rate(
			awscdk.Duration_Minutes(jsii.Number(cfg.CaptureRateMinutes))),
	})
	rule.AddTarget(awseventstargets.NewLambdaFunction(s.Capture, &awseventstargets.LambdaFunctionProps{
		// A failed run needs no retry: the next scheduled run re-reads the same window, and
		// ingestion is idempotent.
		RetryAttempts: jsii.Number(0),
	}))
}

// addAlarms wires the operational alarms. Each one is actionable: it either needs a human or
// tells the operator to change the schedule.
func (s *SpotistatsStack) addAlarms(stack awscdk.Stack, cfg StackConfig) {
	action := awscloudwatchactions.NewSnsAction(s.AlarmTopic)

	customPeriod := func(name string, period awscdk.Duration) awscloudwatch.IMetric {
		return awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String(metrics.Namespace),
			MetricName: jsii.String(name),
			Statistic:  jsii.String("Sum"),
			Period:     period,
		})
	}
	// Alarm window: wide enough that a single missed run is not an incident.
	//
	// It is deliberately NOT just a multiple of the capture interval. EventBridge does not
	// retry, so one transient Spotify outage means one skipped run; at a 30-minute interval a
	// bare 2x window would page an hour after a blip that the next run already repaired. The
	// floor keeps the alarm meaning "capture has genuinely stopped".
	alarmWindow := alarmWindowFor(cfg.CaptureRateMinutes)
	custom := func(name string) awscloudwatch.IMetric {
		return customPeriod(name, awscdk.Duration_Minutes(jsii.Number(alarmWindow)))
	}

	type alarmSpec struct {
		id, desc  string
		metric    awscloudwatch.IMetric
		threshold float64
		periods   float64
		// comparison and missingBreaching differ for CaptureStale, which alarms on the
		// ABSENCE of runs rather than the presence of a bad signal. Making both part of the
		// spec avoids special-casing after construction, which previously produced two
		// alarms sharing one name.
		comparison       awscloudwatch.ComparisonOperator
		missingBreaching bool
	}

	atLeast := awscloudwatch.ComparisonOperator_GREATER_THAN_OR_EQUAL_TO_THRESHOLD
	fewerThan := awscloudwatch.ComparisonOperator_LESS_THAN_THRESHOLD

	specs := []alarmSpec{
		{
			id:   "CaptureFailed",
			desc: "The capture Lambda errored. Plays are not being ingested.",
			metric: s.Capture.MetricErrors(&awscloudwatch.MetricOptions{
				Statistic: jsii.String("Sum"),
				Period:    awscdk.Duration_Minutes(jsii.Number(alarmWindow)),
			}),
			threshold: 1, periods: 1, comparison: atLeast,
		},
		{
			id: "CaptureStale",
			desc: "No capture run completed recently. The schedule may be broken, and the " +
				"recently-played window can roll and lose plays permanently.",
			metric:    custom(metrics.CaptureRun),
			threshold: 1, periods: 1, comparison: fewerThan,
			// Absence of data IS the failure here, so missing data must breach.
			missingBreaching: true,
		},
		{
			id: "PlaysGapDetected",
			desc: "A capture page came back full, so listening outran the polling interval " +
				"and plays may already be unrecoverable. Shorten the schedule.",
			metric:    custom(metrics.PlaysGapDetected),
			threshold: 1, periods: 1, comparison: atLeast,
		},
		{
			id: "TokenRefreshFailed",
			desc: "The Spotify refresh token was rejected, or a rotated token could not be " +
				"persisted. NEEDS A HUMAN: re-run `spotistats auth login`.",
			metric:    custom(metrics.TokenRefreshFailed),
			threshold: 1, periods: 1, comparison: atLeast,
		},
		{
			id: "RollupFailed",
			desc: "The nightly rollup errored. Aggregate drift is not being repaired and the " +
				"dashboard snapshots are going stale.",
			metric: s.Rollup.MetricErrors(&awscloudwatch.MetricOptions{
				Statistic: jsii.String("Sum"),
				Period:    awscdk.Duration_Hours(jsii.Number(24)),
			}),
			threshold: 1, periods: 1, comparison: atLeast,
		},
		{
			id: "RollupStale",
			desc: "No nightly rollup completed in the last two days. Leaderboards and the " +
				"dashboard are frozen at their last good values.",
			metric:    customPeriod(metrics.RollupRun, awscdk.Duration_Hours(jsii.Number(48))),
			threshold: 1, periods: 1, comparison: fewerThan,
			missingBreaching: true,
		},
		{
			id: "AggregateDrift",
			desc: "The reconciler had to correct aggregate rows, meaning a capture run died " +
				"between writing a play and applying its aggregates. Self-healing, but a " +
				"persistently non-zero count means something is failing repeatedly.",
			metric:    customPeriod(metrics.AggregateDrift, awscdk.Duration_Hours(jsii.Number(24))),
			threshold: 1, periods: 3,
			comparison: atLeast,
		},
		{
			id: "ExternalEnrichFailed",
			desc: "The external enrichment job errored. Artist profiles are going stale; " +
				"nothing else is affected, since this job touches no play or aggregate.",
			metric: s.Enrich.MetricErrors(&awscloudwatch.MetricOptions{
				Statistic: jsii.String("Sum"),
				Period:    awscdk.Duration_Hours(jsii.Number(24)),
			}),
			threshold: 1, periods: 1, comparison: atLeast,
		},
		{
			id: "ExternalUnresolvedRatio",
			desc: "Most artists stopped resolving to a MusicBrainz ID. A gradual rise is " +
				"normal as obscure artists accumulate; a jump means an upstream response " +
				"shape changed and the resolver has silently stopped working.",
			// The RATIO, not the count. A count climbs legitimately as the library grows, so
			// alarming on it would either fire constantly or be set so high it never fires.
			// Two periods, because one bad night is a 503 storm rather than a broken parser.
			metric:    customPeriod(metrics.ExternalUnresolvedRatio, awscdk.Duration_Hours(jsii.Number(24))),
			threshold: 0.9, periods: 2, comparison: atLeast,
		},
		{
			id: "NotifyFailed",
			desc: "The Slack notifier errored, so some alarm did not reach the channel. " +
				"NOT self-monitoring: this alarm is delivered through the very function " +
				"that failed, so check it in the console if the channel has gone quiet.",
			metric: s.Notify.MetricErrors(&awscloudwatch.MetricOptions{
				Statistic: jsii.String("Sum"),
				Period:    awscdk.Duration_Minutes(jsii.Number(alarmWindow)),
			}),
			threshold: 1, periods: 1, comparison: atLeast,
		},
		{
			id: "GenresDegraded",
			desc: "Plays were recorded without complete genre attribution. Recoverable: the " +
				"nightly reconcile repairs it. Persistent firing means artist lookups are failing.",
			metric:    custom(metrics.GenresDegraded),
			threshold: 1, periods: 2, comparison: atLeast,
		},
	}

	for _, spec := range specs {
		missing := awscloudwatch.TreatMissingData_NOT_BREACHING
		if spec.missingBreaching {
			missing = awscloudwatch.TreatMissingData_BREACHING
		}
		alarm := awscloudwatch.NewAlarm(stack, jsii.String(spec.id+"Alarm"), &awscloudwatch.AlarmProps{
			AlarmName:          jsii.String("spotistats-" + spec.id),
			AlarmDescription:   jsii.String(spec.desc),
			Metric:             spec.metric,
			Threshold:          jsii.Number(spec.threshold),
			EvaluationPeriods:  jsii.Number(spec.periods),
			ComparisonOperator: spec.comparison,
			TreatMissingData:   missing,
		})
		alarm.AddAlarmAction(action)
		// Recovery is notified too. A channel that only ever says "broken" and never "fixed"
		// teaches the reader to ignore it, because there is no way to tell a live incident
		// from a stale message. The notifier renders OK in green and labels it RECOVERED.
		alarm.AddOkAction(action)
	}
}

func (s *SpotistatsStack) addOutputs(stack awscdk.Stack, cfg StackConfig) {
	awscdk.NewCfnOutput(stack, jsii.String("TableNameOutput"), &awscdk.CfnOutputProps{
		Key:         jsii.String("TableName"),
		Value:       s.Table.TableName(),
		Description: jsii.String("Set SPOTISTATS_TABLE_NAME to this for the CLI"),
	})
	awscdk.NewCfnOutput(stack, jsii.String("CaptureFunctionOutput"), &awscdk.CfnOutputProps{
		Key:         jsii.String("CaptureFunctionName"),
		Value:       s.Capture.FunctionName(),
		Description: jsii.String("Invoke manually with: aws lambda invoke --function-name <this> /dev/stdout"),
	})
	awscdk.NewCfnOutput(stack, jsii.String("AlarmTopicOutput"), &awscdk.CfnOutputProps{
		Key:         jsii.String("AlarmTopicArn"),
		Value:       s.AlarmTopic.TopicArn(),
		Description: jsii.String("Publish here to test the Slack channel: aws sns publish --topic-arn <this> --subject test --message hello"),
	})
	awscdk.NewCfnOutput(stack, jsii.String("SSMPrefixOutput"), &awscdk.CfnOutputProps{
		Key:         jsii.String("SSMPrefix"),
		Value:       jsii.String(cfg.SSMPrefix),
		Description: jsii.String("Spotify credentials are read from here; create them by hand"),
	})
}
