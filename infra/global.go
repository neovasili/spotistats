package main

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsbudgets"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscertificatemanager"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsroute53"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
)

// GlobalStackProps configures the us-east-1 stack.
type GlobalStackProps struct {
	awscdk.StackProps
	Config StackConfig
}

// GlobalStack holds the resources that cannot live in the deployment region.
//
// There are exactly two, and each for a different reason:
//
//   - The ACM certificate, because CloudFront accepts a certificate only from us-east-1. This
//     is a hard AWS constraint and the sole reason this deployment spans two regions.
//   - The AWS Budget, because billing is a global concern and conventionally provisioned in
//     us-east-1.
//
// Everything else -- including the CloudFront distribution itself, despite CloudFront being a
// global service -- stays in the regional stack. The distribution cannot move here: its Origin
// Access Control grant is a policy on the S3 bucket that names the distribution's ARN, while
// the distribution names the bucket's domain. Split across stacks those two references form a
// cycle, which CDK cannot resolve. Keeping the distribution with its origin means the only
// value crossing regions is the certificate ARN.
type GlobalStack struct {
	awscdk.Stack
	Certificate awscertificatemanager.ICertificate
}

// NewGlobalStack provisions the us-east-1 stack.
func NewGlobalStack(scope constructs.Construct, id string, props *GlobalStackProps) *GlobalStack {
	stack := awscdk.NewStack(scope, jsii.String(id), &props.StackProps)
	cfg := props.Config
	s := &GlobalStack{Stack: stack}

	if cfg.DomainName != "" {
		s.Certificate = newCertificate(stack, cfg)
		awscdk.NewCfnOutput(stack, jsii.String("CertificateArnOutput"), &awscdk.CfnOutputProps{
			Key:         jsii.String("CertificateArn"),
			Value:       s.Certificate.CertificateArn(),
			Description: jsii.String("Consumed by the regional stack's CloudFront distribution"),
		})
	}

	addBudget(stack, cfg)

	return s
}

// newCertificate issues the TLS certificate for the site.
//
// With a hosted zone the validation records are created and waited on automatically. Without
// one, CloudFormation blocks until the operator adds the CNAME that ACM displays -- which is
// why supplying the zone is worth it even though the certificate would eventually issue either
// way.
func newCertificate(stack awscdk.Stack, cfg StackConfig) awscertificatemanager.ICertificate {
	if cfg.HostedZoneID == "" {
		return awscertificatemanager.NewCertificate(stack, jsii.String("Certificate"),
			&awscertificatemanager.CertificateProps{
				DomainName: jsii.String(cfg.DomainName),
				Validation: awscertificatemanager.CertificateValidation_FromDns(nil),
			})
	}

	// fromHostedZoneAttributes rather than fromLookup: a lookup would need AWS credentials at
	// synth time, which would stop CI reviewing the template.
	zone := awsroute53.HostedZone_FromHostedZoneAttributes(stack, jsii.String("CertZone"),
		&awsroute53.HostedZoneAttributes{
			HostedZoneId: jsii.String(cfg.HostedZoneID),
			ZoneName:     jsii.String(cfg.HostedZoneName),
		})
	return awscertificatemanager.NewCertificate(stack, jsii.String("Certificate"),
		&awscertificatemanager.CertificateProps{
			DomainName: jsii.String(cfg.DomainName),
			Validation: awscertificatemanager.CertificateValidation_FromDns(zone),
		})
}

// addBudget is the backstop against runaway cost. The public API is unauthenticated and there
// is no WAF by design, so a spending alarm is the last line of defence.
func addBudget(stack awscdk.Stack, cfg StackConfig) {
	if cfg.MonthlyBudgetUSD <= 0 || cfg.AlarmEmail == "" {
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
						SubscriptionType: jsii.String("EMAIL"),
						Address:          jsii.String(cfg.AlarmEmail),
					},
				},
			},
		},
	})
}
