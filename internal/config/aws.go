package config

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// awsConfig builds an AWS SDK config.
//
// When DDBEndpoint is set the credential chain is bypassed entirely in favour of static
// throwaway credentials: DynamoDB Local validates a signature's shape but not its contents,
// and going through the real chain would fail on an expired SSO session or a missing
// profile for no benefit. That is what makes `poll` runnable with no AWS account at all.
func (c Config) awsConfig(ctx context.Context, local bool) (aws.Config, error) {
	if local {
		region := c.Region
		if region == "" {
			// DynamoDB Local validates a signature's shape but not its contents, so any region
			// works; a fixed one keeps signing deterministic.
			region = "local"
		}
		return aws.Config{
			Region:      region,
			Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
		}, nil
	}
	// WithRegion is applied only when a region was actually configured. Forcing an empty string
	// would override the SDK's own resolution and leave every call region-less.
	opts := []func(*awsconfig.LoadOptions) error{}
	if c.Region != "" {
		opts = append(opts, awsconfig.WithRegion(c.Region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("config: load AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		return aws.Config{}, fmt.Errorf(
			"config: no AWS region resolved. Set %s, or AWS_REGION, or configure a region on "+
				"the active profile", EnvRegion)
	}
	return cfg, nil
}

// DynamoClient builds a DynamoDB client, honouring DDBEndpoint for local runs.
func (c Config) DynamoClient(ctx context.Context) (*dynamodb.Client, error) {
	local := c.DDBEndpoint != ""
	cfg, err := c.awsConfig(ctx, local)
	if err != nil {
		return nil, err
	}
	return dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		if local {
			o.BaseEndpoint = aws.String(c.DDBEndpoint)
		}
	}), nil
}

// SSMClient builds an SSM client. It always uses the real credential chain: there is no
// local substitute for Parameter Store, which is exactly why a file-backed token store
// exists.
func (c Config) SSMClient(ctx context.Context) (*ssm.Client, error) {
	cfg, err := c.awsConfig(ctx, false)
	if err != nil {
		return nil, err
	}
	return ssm.NewFromConfig(cfg), nil
}
