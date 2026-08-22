// Package config resolves runtime configuration and builds the AWS-backed dependencies
// the rest of the application needs.
//
// It is the only package that reads the environment, and the only one that knows the
// refresh token lives in SSM Parameter Store. internal/spotify defines a
// RefreshTokenStore interface precisely so it can stay AWS-free; the implementations live
// here.
package config
