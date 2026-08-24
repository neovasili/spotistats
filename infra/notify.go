package main

import (
	"path/filepath"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	awsbudgets "github.com/aws/aws-cdk-go/awscdk/v2/awsbudgets"
	awsiam "github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	awslambda "github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	awslogs "github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	awssns "github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	awssnssubscriptions "github.com/aws/aws-cdk-go/awscdk/v2/awssnssubscriptions"
	"github.com/aws/jsii-runtime-go"
)

// newNotifyFunction is the alarm fan-out: it subscribes to the alarm topic and posts each
// message to a Slack incoming webhook.
//
// # Why a Lambda rather than AWS Chatbot
//
// Chatbot would need no code, but it needs a manual OAuth in the AWS console to authorise the
// Slack workspace -- the workspace ID cannot be created by CloudFormation -- and its CDK
// construct defaults GuardrailPolicies to AdministratorAccess, which is a policy this repo
// refuses to attach anywhere (see cicd.go). A webhook is a single-channel credential that grants
// nothing in AWS, and the message formatting is ordinary Go with ordinary tests.
//
// # Why not email
//
// An SNS email subscription requires the recipient to click a confirmation link, and production
// ran for weeks with that subscription in PendingConfirmation: ten alarms configured, three of
// them firing, nobody notified. A webhook either works on the first post or fails loudly into
// this function's own error metric.
func (s *SpotistatsStack) newNotifyFunction(stack awscdk.Stack, cfg StackConfig) awslambda.Function {
	logGroup := awslogs.NewLogGroup(stack, jsii.String("NotifyLogs"), &awslogs.LogGroupProps{
		LogGroupName:  jsii.String("/aws/lambda/spotistats-notify"),
		Retention:     awslogs.RetentionDays_TWO_WEEKS,
		RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
	})

	fn := awslambda.NewFunction(stack, jsii.String("Notify"), &awslambda.FunctionProps{
		FunctionName: jsii.String("spotistats-notify"),
		Description:  jsii.String("Posts CloudWatch alarms and budget notifications to Slack"),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String(filepath.Join(cfg.LambdaAssetDir, "notify")), nil),
		// The smallest useful size: one SSM read and one HTTPS POST per invocation.
		MemorySize: jsii.Number(128),
		// Short, but not tight. It is one POST; a longer budget would only let a wedged Slack
		// hold the invocation open, and SNS retries anyway.
		Timeout:  awscdk.Duration_Seconds(jsii.Number(30)),
		LogGroup: logGroup,
		Environment: &map[string]*string{
			"SPOTISTATS_SSM_PREFIX":  jsii.String(cfg.SSMPrefix),
			"SPOTISTATS_ENVIRONMENT": jsii.String("production"),
			"SPOTISTATS_LOG_LEVEL":   jsii.String("info"),
		},
	})

	// The webhook parameter and NOTHING else.
	//
	// This function's whole job is to forward text to a third party, so it is the one in this
	// stack that should hold the least. No table access, and specifically not the Spotify
	// refresh token: a wildcard on the prefix would grant both, and a compromised notifier
	// would then be a compromised account. A test asserts the narrowness.
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Effect:  awsiam.Effect_ALLOW,
		Actions: jsii.Strings("ssm:GetParameter"),
		Resources: jsii.Strings(
			"arn:aws:ssm:" + cfg.Region + ":" + *stack.Account() +
				":parameter" + cfg.SSMPrefix + "/slack_webhook",
		),
	}))

	return fn
}

// subscribeNotifier points the alarm topic at the notifier.
//
// A dead-letter queue is deliberately omitted. SNS retries a failed Lambda invocation for up to
// 23 hours by default, and an alarm that could not be delivered for 23 hours is not a message
// worth replaying afterwards -- the state it described has changed. What matters is that the
// FAILURE is visible, which the NotifyFailed alarm covers.
func (s *SpotistatsStack) subscribeNotifier(fn awslambda.Function) {
	s.AlarmTopic.AddSubscription(awssnssubscriptions.NewLambdaSubscription(fn, nil))
}

// alarmTopicForBudget grants AWS Budgets permission to publish to the alarm topic.
//
// Budgets publishes as the service principal budgets.amazonaws.com, which an SNS topic does not
// allow by default, so without this the budget silently never notifies -- the same class of
// quiet failure as the unconfirmed email subscription. The source conditions keep the grant from
// being usable by any other account's budgets.
func grantBudgetPublish(topic awssns.Topic, stack awscdk.Stack) {
	topic.AddToResourcePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:        jsii.String("AllowBudgetsToPublish"),
		Effect:     awsiam.Effect_ALLOW,
		Principals: &[]awsiam.IPrincipal{awsiam.NewServicePrincipal(jsii.String("budgets.amazonaws.com"), nil)},
		Actions:    jsii.Strings("SNS:Publish"),
		Resources:  jsii.Strings(*topic.TopicArn()),
		Conditions: &map[string]interface{}{
			"StringEquals": map[string]interface{}{
				"aws:SourceAccount": *stack.Account(),
			},
			"ArnLike": map[string]interface{}{
				// Budget ARNs carry NO region -- hence the empty segment -- and AWS documents
				// the pattern as ":*" rather than ":budget/*". Being stricter than documented
				// would risk the condition not matching, and a grant that does not match means
				// the budget silently never notifies: the same quiet failure as the
				// unconfirmed email subscription.
				"aws:SourceArn": "arn:aws:budgets::" + *stack.Account() + ":*",
			},
		},
	}))
}

// addBudget is the backstop against runaway cost. The public API is unauthenticated and there is
// no WAF by design, so a spending alarm is the last line of defence.
//
// It lives here, in the REGIONAL stack, beside the topic it publishes to. It used to sit in the
// us-east-1 stack on the grounds that billing is global -- true, but only a convention, and the
// global stack deploys first, so a budget there naming a topic here would reference a resource
// that does not exist yet. AWS::Budgets::Budget is in the eu-west-1 CloudFormation resource
// specification with an identical definition, and budget ARNs carry no region.
//
// The subscriber is the alarm topic, not an email address. A budget email needs no confirmation
// step, so it would have worked -- but two notification channels means two places to check and
// one of them will rot. Slack is the channel.
func (s *SpotistatsStack) addBudget(stack awscdk.Stack, cfg StackConfig) {
	if cfg.MonthlyBudgetUSD <= 0 {
		// No warning here. A zero budget is a deliberate "I do not want one", and the alarm
		// topic already warns about anything that would leave notifications undelivered.
		return
	}
	awsbudgets.NewCfnBudget(stack, jsii.String("MonthlyBudget"), &awsbudgets.CfnBudgetProps{
		Budget: &awsbudgets.CfnBudget_BudgetDataProperty{
			BudgetName: jsii.String("spotistats-monthly"),
			BudgetType: jsii.String("COST"),
			TimeUnit:   jsii.String("MONTHLY"),
			BudgetLimit: &awsbudgets.CfnBudget_SpendProperty{
				Amount: jsii.Number(cfg.MonthlyBudgetUSD),
				Unit:   jsii.String("USD"),
			},
		},
		NotificationsWithSubscribers: &[]interface{}{
			&awsbudgets.CfnBudget_NotificationWithSubscribersProperty{
				Notification: &awsbudgets.CfnBudget_NotificationProperty{
					ComparisonOperator: jsii.String("GREATER_THAN"),
					NotificationType:   jsii.String("ACTUAL"),
					Threshold:          jsii.Number(80),
					ThresholdType:      jsii.String("PERCENTAGE"),
				},
				Subscribers: &[]interface{}{
					&awsbudgets.CfnBudget_SubscriberProperty{
						SubscriptionType: jsii.String("SNS"),
						Address:          s.AlarmTopic.TopicArn(),
					},
				},
			},
		},
	})
}
