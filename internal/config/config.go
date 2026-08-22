package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/neovasili/spotistats/internal/model"
	"github.com/neovasili/spotistats/internal/spotify"
)

// Environment variables. Every knob the application has is here; nothing else in the
// codebase calls os.Getenv.
const (
	EnvRegion       = "SPOTISTATS_REGION"
	EnvTableName    = "SPOTISTATS_TABLE_NAME"
	EnvDDBEndpoint  = "SPOTISTATS_DDB_ENDPOINT"
	EnvSSMPrefix    = "SPOTISTATS_SSM_PREFIX"
	EnvTokenFile    = "SPOTISTATS_TOKEN_FILE"
	EnvTimezone     = "SPOTISTATS_TIMEZONE"
	EnvClientID     = "SPOTISTATS_CLIENT_ID"
	EnvClientSecret = "SPOTISTATS_CLIENT_SECRET"
	EnvRedirectURI  = "SPOTISTATS_REDIRECT_URI"
	EnvCaptureLimit = "SPOTISTATS_CAPTURE_LIMIT"
	EnvLogLevel     = "SPOTISTATS_LOG_LEVEL"

	// EnvSpotifyBaseURL and EnvTokenURL point the client at a stand-in for the Spotify API.
	// They exist so the CLI and the capture pipeline can be exercised end to end against a
	// local fake -- the real API is rate limited, needs a human to authorise, and is
	// unreachable from CI. Leave both unset in production.
	EnvSpotifyBaseURL = "SPOTISTATS_SPOTIFY_BASE_URL"
	EnvTokenURL       = "SPOTISTATS_TOKEN_URL"
)

// Defaults.
const (
	DefaultSSMPrefix = "/spotistats/spotify"
	DefaultTimezone  = "Europe/Madrid"

	// DefaultRedirectURI must match the value registered in the Spotify developer
	// dashboard byte for byte. Spotify forbids `localhost` outright and permits plain HTTP
	// only for an explicit loopback IP literal, so this is not interchangeable with
	// http://localhost:8888/callback.
	DefaultRedirectURI = "http://127.0.0.1:8888/callback"
)

// Config is the resolved runtime configuration.
type Config struct {
	// Region is the AWS region. EMPTY is meaningful: it means "let the AWS SDK resolve it",
	// from AWS_REGION, AWS_DEFAULT_REGION or the active profile.
	//
	// There is deliberately no hardcoded default. One used to exist, and when the deployment
	// moved to eu-west-1 it was left at us-east-1 -- so every Lambda, which relies on the
	// runtime-provided AWS_REGION, silently addressed a region with no table in it. A constant
	// here duplicates cdk.json and will drift from it again.
	Region      string
	TableName   string
	DDBEndpoint string // set to run against DynamoDB Local

	SSMPrefix string

	// TokenFile, when set, stores the refresh token in a local file instead of SSM. It
	// exists so the capture pipeline is fully runnable before an AWS account has been
	// chosen, and so a developer can iterate without touching production state.
	TokenFile string

	// ClientID and ClientSecret may be supplied directly; when empty they are read from
	// SSM under SSMPrefix.
	ClientID     string
	ClientSecret string

	Timezone     string
	RedirectURI  string
	CaptureLimit int
	LogLevel     string

	// SpotifyBaseURL and TokenURL override the API endpoints. Empty means the real Spotify.
	SpotifyBaseURL string
	TokenURL       string
}

// Load reads configuration from the environment and applies defaults.
func Load() Config {
	c := Config{
		// AWS_REGION is always set by the Lambda runtime, and locally the SDK falls back to the
		// active profile when this is empty. An explicit SPOTISTATS_REGION still wins.
		Region:         env(EnvRegion, os.Getenv("AWS_REGION")),
		TableName:      os.Getenv(EnvTableName),
		DDBEndpoint:    os.Getenv(EnvDDBEndpoint),
		SSMPrefix:      strings.TrimSuffix(env(EnvSSMPrefix, DefaultSSMPrefix), "/"),
		TokenFile:      os.Getenv(EnvTokenFile),
		ClientID:       os.Getenv(EnvClientID),
		ClientSecret:   os.Getenv(EnvClientSecret),
		Timezone:       env(EnvTimezone, DefaultTimezone),
		RedirectURI:    env(EnvRedirectURI, DefaultRedirectURI),
		LogLevel:       env(EnvLogLevel, "info"),
		SpotifyBaseURL: os.Getenv(EnvSpotifyBaseURL),
		TokenURL:       os.Getenv(EnvTokenURL),
	}
	if v := os.Getenv(EnvCaptureLimit); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.CaptureLimit = n
		}
	}
	if c.CaptureLimit <= 0 || c.CaptureLimit > spotify.MaxRecentlyPlayedLimit {
		c.CaptureLimit = spotify.MaxRecentlyPlayedLimit
	}
	return c
}

// Validate checks the configuration is usable for a command that touches storage.
func (c Config) Validate() error {
	var errs []error
	if c.TableName == "" {
		errs = append(errs, fmt.Errorf("%s is required", EnvTableName))
	}
	if _, err := model.NewCalendar(c.Timezone); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Calendar builds the calendar every aggregate period key is derived in.
func (c Config) Calendar() (model.Calendar, error) {
	return model.NewCalendar(c.Timezone)
}

// UsesLocalTokenFile reports whether the refresh token is file-backed rather than in SSM.
func (c Config) UsesLocalTokenFile() bool { return c.TokenFile != "" }

// SSM parameter names, derived from SSMPrefix.
func (c Config) ClientIDParam() string     { return c.SSMPrefix + "/client_id" }
func (c Config) ClientSecretParam() string { return c.SSMPrefix + "/client_secret" }
func (c Config) RefreshTokenParam() string { return c.SSMPrefix + "/refresh_token" }

// Redacted returns a copy safe to log: the secret is replaced, never truncated to a prefix
// (a prefix of a short secret is still most of it).
func (c Config) Redacted() Config {
	if c.ClientSecret != "" {
		c.ClientSecret = "[redacted]"
	}
	return c
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
