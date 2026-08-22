// Command infra is the AWS CDK application that provisions Spotistats.
//
// It lives in the same Go module as the application on purpose: the DynamoDB table is built
// from store.Schema, the same declaration the integration tests create their tables from, so
// the deployed table cannot drift from the shape the tests exercise. TableSchemaParity in
// stack_test.go asserts that property, turning a drift into a failed build rather than a
// production incident.
//
// The stack performs no context lookups, so `cdk synth` produces a template with no AWS
// credentials and no network access.
package main
