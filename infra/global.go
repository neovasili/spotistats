package main

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
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
// There is exactly ONE: the ACM certificate, because CloudFront accepts a certificate only from
// us-east-1. That hard AWS constraint is the sole reason this deployment spans two regions.
//
// The AWS Budget used to live here too, on the reasoning that billing is global and is
// "conventionally" provisioned in us-east-1. It moved to the regional stack when the budget
// started notifying through the alarm SNS topic: a convention is not worth a cross-region
// reference, and this stack deploys FIRST -- so a budget here referencing a topic there would
// name a resource that does not exist yet. AWS::Budgets::Budget is present in the eu-west-1
// CloudFormation resource specification, identical to the us-east-1 definition, and budget ARNs
// carry no region at all.
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
