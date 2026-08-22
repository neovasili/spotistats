package main

import (
	"fmt"
	"os"

	"github.com/aws/aws-cdk-go/awscdk/v2"
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

	// The certificate must be in us-east-1 for CloudFront, and billing is global, so both live
	// in their own stack. crossRegionReferences lets the regional stack consume the
	// certificate ARN: CDK passes it through an SSM parameter and a custom resource, which is
	// the supported mechanism for a value crossing regions.
	global := NewGlobalStack(app, "SpotistatsGlobalStack", &GlobalStackProps{
		StackProps: awscdk.StackProps{
			Env:                   cfg.globalEnv(),
			CrossRegionReferences: jsii.Bool(true),
			Description: jsii.String(
				"Spotistats global resources: the CloudFront certificate and the billing budget"),
		},
		Config: cfg,
	})

	regional := NewSpotistatsStack(app, "SpotistatsStack", &SpotistatsStackProps{
		StackProps: awscdk.StackProps{
			Env:                   cfg.env(),
			CrossRegionReferences: jsii.Bool(true),
			Description:           jsii.String("Spotistats: personal Spotify listening statistics"),
		},
		Config:      cfg,
		Certificate: global.Certificate,
	})

	// Explicit ordering: the certificate must exist and be validated before CloudFront can
	// reference it.
	regional.AddDependency(global.Stack, jsii.String("the distribution needs the certificate"))

	app.Synth(nil)
}
