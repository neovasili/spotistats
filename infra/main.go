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

	NewSpotistatsStack(app, "SpotistatsStack", &SpotistatsStackProps{
		StackProps: awscdk.StackProps{
			Env:         cfg.env(),
			Description: jsii.String("Spotistats: personal Spotify listening statistics"),
		},
		Config: cfg,
	})

	app.Synth(nil)
}
