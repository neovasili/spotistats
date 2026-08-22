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
		return aws.Config{
			Region:      c.Region,
			Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
		}, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.Region))
	if err != nil {
		return aws.Config{}, fmt.Errorf("config: load AWS configuration: %w", err)
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
