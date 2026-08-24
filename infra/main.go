package main

import (
	"fmt"
	"os"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	awscertificatemanager "github.com/aws/aws-cdk-go/awscdk/v2/awscertificatemanager"
	"github.com/aws/jsii-runtime-go"
)

func main() {
	defer jsii.Close()

	app := awscdk.NewApp(nil)

	cfg, err := stackConfigFromContext(app)
	if err != nil {
		fmt.Fprintf(os.Stderr, "infra: %v\n", err)
		os.Exit(1)
	}

	// The us-east-1 stack exists for exactly one resource: the CloudFront certificate, which
	// ACM will only issue there. crossRegionReferences lets the regional stack consume its ARN
	// -- CDK passes it through an SSM parameter and a custom resource, which is the supported
	// mechanism for a value crossing regions.
	//
	// With no domain there is no certificate, and therefore nothing for that stack to hold. It
	// is skipped entirely rather than synthesised empty: CloudFormation requires a non-empty
	// Resources section, so an empty stack is not merely pointless but undeployable. This used
	// to be masked by the budget living there; the budget has since moved next to the SNS
	// topic it notifies.
	var global *GlobalStack
	if cfg.DomainName != "" {
		global = NewGlobalStack(app, "SpotistatsGlobalStack", &GlobalStackProps{
			StackProps: awscdk.StackProps{
				Env:                   cfg.globalEnv(),
				CrossRegionReferences: jsii.Bool(true),
				Description: jsii.String(
					"Spotistats global resources: the CloudFront certificate"),
			},
			Config: cfg,
		})
	}

	var certificate awscertificatemanager.ICertificate
	if global != nil {
		certificate = global.Certificate
	}

	regional := NewSpotistatsStack(app, "SpotistatsStack", &SpotistatsStackProps{
		StackProps: awscdk.StackProps{
			Env:                   cfg.env(),
			CrossRegionReferences: jsii.Bool(true),
			Description:           jsii.String("Spotistats: personal Spotify listening statistics"),
		},
		Config:      cfg,
		Certificate: certificate,
	})

	// Explicit ordering: the certificate must exist and be validated before CloudFront can
	// reference it.
	if global != nil {
		regional.AddDependency(global.Stack, jsii.String("the distribution needs the certificate"))
	}

	app.Synth(nil)
}
