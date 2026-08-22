package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/neovasili/spotistats/internal/spotify"
	"github.com/neovasili/spotistats/internal/store"
)

// Deps is the assembled dependency set. Building it in one place keeps cmd/ thin and means
// every entry point wires the same objects the same way.
type Deps struct {
	Config Config
	Logger *slog.Logger

	Store       *store.Store
	TokenStore  spotify.RefreshTokenStore
	TokenSource *spotify.RefreshingTokenSource
	Spotify     *spotify.Client
}

// BuildOptions narrows what Build constructs, so a command only pays for what it uses --
// `auth login` needs no DynamoDB, and `poll` needs everything.
type BuildOptions struct {
	// NeedStore constructs the DynamoDB-backed store.
	NeedStore bool
	// NeedSpotify constructs the token source and API client.
	NeedSpotify bool
	// VerifyStoreConfig runs the timezone/schema guard. Skip it for read-only commands
	// against a table that may not exist yet.
	VerifyStoreConfig bool
	// OnRotationError is forwarded to the token source.
	OnRotationError func(context.Context, error)
}

// Logger builds the application logger.
func (c Config) Logger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// Build assembles the requested dependencies.
func Build(ctx context.Context, c Config, opts BuildOptions) (*Deps, error) {
	d := &Deps{Config: c, Logger: c.Logger()}

	if opts.NeedStore {
		if err := c.Validate(); err != nil {
			return nil, err
		}
		ddb, err := c.DynamoClient(ctx)
		if err != nil {
			return nil, err
		}
		cal, err := c.Calendar()
		if err != nil {
			return nil, err
		}
		st, err := store.New(store.Config{
			Client:    ddb,
			TableName: c.TableName,
			Calendar:  cal,
			Logger:    d.Logger,
		})
		if err != nil {
			return nil, err
		}
		if opts.VerifyStoreConfig {
			if err := st.VerifyConfig(ctx); err != nil {
				return nil, err
			}
		}
		d.Store = st
	}

	if opts.NeedSpotify {
		creds, ts, err := c.buildTokenSource(ctx, d.Logger, opts.OnRotationError)
		if err != nil {
			return nil, err
		}
		d.TokenStore = ts.store
		d.TokenSource = ts.source

		client, err := spotify.New(spotify.Config{
			TokenSource: ts.source,
			BaseURL:     c.SpotifyBaseURL,
			Retry:       spotify.DefaultRetryPolicy(),
			Logger:      d.Logger,
			UserAgent:   "spotistats/1.0 (+https://github.com/neovasili/spotistats)",
		})
		if err != nil {
			return nil, err
		}
		d.Spotify = client
		_ = creds
	}

	return d, nil
}

type tokenBundle struct {
	store  spotify.RefreshTokenStore
	source *spotify.RefreshingTokenSource
}

func (c Config) buildTokenSource(
	ctx context.Context, log *slog.Logger, onRotErr func(context.Context, error),
) (spotify.Credentials, tokenBundle, error) {
	creds, err := c.ResolveCredentials(ctx)
	if err != nil {
		return spotify.Credentials{}, tokenBundle{}, err
	}
	ts, err := c.ResolveTokenStore(ctx)
	if err != nil {
		return spotify.Credentials{}, tokenBundle{}, err
	}

	source, err := spotify.NewRefreshingTokenSource(spotify.TokenSourceConfig{
		Credentials:     creds,
		Store:           ts,
		TokenURL:        c.TokenURL,
		Retry:           spotify.DefaultRetryPolicy(),
		Logger:          log,
		OnRotationError: onRotErr,
	})
	if err != nil {
		return spotify.Credentials{}, tokenBundle{}, err
	}
	return creds, tokenBundle{store: ts, source: source}, nil
}

// ResolveTokenStore returns the configured refresh-token store: a local file when
// TokenFile is set, otherwise SSM.
func (c Config) ResolveTokenStore(ctx context.Context) (spotify.RefreshTokenStore, error) {
	if c.UsesLocalTokenFile() {
		return NewFileTokenStore(c.TokenFile), nil
	}
	client, err := c.SSMClient(ctx)
	if err != nil {
		return nil, err
	}
	return NewSSMTokenStore(client, c.RefreshTokenParam()), nil
}

// ResolveCredentials returns the Spotify app credentials, preferring the environment and
// falling back to SSM.
func (c Config) ResolveCredentials(ctx context.Context) (spotify.Credentials, error) {
	if c.ClientID != "" && c.ClientSecret != "" {
		return spotify.Credentials{ClientID: c.ClientID, ClientSecret: c.ClientSecret}, nil
	}

	client, err := c.SSMClient(ctx)
	if err != nil {
		return spotify.Credentials{}, fmt.Errorf(
			"no %s/%s in the environment and SSM is unavailable: %w",
			EnvClientID, EnvClientSecret, err)
	}

	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	id := c.ClientID
	if id == "" {
		if id, err = readParam(readCtx, client, c.ClientIDParam()); err != nil {
			return spotify.Credentials{}, err
		}
	}
	secret := c.ClientSecret
	if secret == "" {
		if secret, err = readParam(readCtx, client, c.ClientSecretParam()); err != nil {
			return spotify.Credentials{}, err
		}
	}
	return spotify.Credentials{ClientID: id, ClientSecret: secret}, nil
}

func readParam(ctx context.Context, client SSMAPI, name string) (string, error) {
	s := NewSSMTokenStore(client, name)
	v, err := s.Get(ctx)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	return v, nil
}
