package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/neovasili/spotistats/internal/httpx"
)

// DefaultBaseURL is the Spotify Web API root.
const DefaultBaseURL = "https://api.spotify.com/v1"

// maxResponseBytes caps a decoded response. A 50-item recently-played page with full
// track objects is well under 1 MB; this only exists so a pathological response cannot
// exhaust a Lambda's memory.
const maxResponseBytes = 16 << 20

// Config configures a Client. Only TokenSource is required.
type Config struct {
	TokenSource TokenSource
	BaseURL     string
	Doer        Doer
	Retry       RetryPolicy
	Clock       Clock
	// Limiter is optional. Nil means unthrottled, which is right for the capture job
	// (about three calls per run); the history backfill should supply one.
	Limiter   Limiter
	UserAgent string
	Logger    *slog.Logger
}

// Client is a Spotify Web API client.
type Client struct {
	baseURL   *url.URL
	tokens    TokenSource
	retrier   *httpx.Retrier
	userAgent string
	log       *slog.Logger
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	if cfg.TokenSource == nil {
		return nil, errors.New("spotify: a TokenSource is required")
	}
	raw := cfg.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("spotify: parse base URL %q: %w", raw, err)
	}
	doer := cfg.Doer
	if doer == nil {
		doer = &http.Client{Timeout: 15 * time.Second}
	}
	clock := cfg.Clock
	if clock == nil {
		clock = SystemClock()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = "spotistats/1.0"
	}

	return &Client{
		baseURL:   base,
		tokens:    cfg.TokenSource,
		retrier:   httpx.NewRetrier(httpx.RetrierConfig{Doer: doer, Policy: cfg.Retry, Clock: clock, Limiter: cfg.Limiter, Log: log}),
		userAgent: ua,
		log:       log,
	}, nil
}

// invalidator is the optional half of TokenSource that lets the client force a refresh
// after a 401. Keeping it out of TokenSource means test doubles can be one-liners.
type invalidator interface{ Invalidate() }

// get issues an authenticated GET against path (relative to the base URL) and decodes the
// JSON body into out.
//
// A 401 is handled here rather than in the retry loop: the remedy is a token refresh, not
// a wait. The cached token is invalidated and the request is retried exactly once, since a
// second 401 with a freshly minted token means the grant itself is gone.
//
// The authorisation layer sits OUTSIDE the retrier so a 429 encountered on the post-401
// retry still gets proper backoff.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	endpoint, err := c.resolve(path, q)
	if err != nil {
		return err
	}

	for attempt := 0; attempt < 2; attempt++ {
		token, terr := c.tokens.Token(ctx)
		if terr != nil {
			return terr
		}

		resp, rerr := c.retrier.Do(ctx, func() (*http.Request, error) {
			r, herr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if herr != nil {
				return nil, herr
			}
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Accept", "application/json")
			r.Header.Set("User-Agent", c.userAgent)
			return r, nil
		})

		if rerr != nil {
			var api *APIError
			if attempt == 0 && errors.As(rerr, &api) && api.StatusCode == http.StatusUnauthorized {
				c.log.InfoContext(ctx, "spotify: 401, forcing a token refresh and retrying once",
					"path", path)
				if inv, ok := c.tokens.(invalidator); ok {
					inv.Invalidate()
				}
				continue
			}
			return rerr
		}

		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()

		if out == nil {
			return nil
		}
		if derr := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); derr != nil {
			return fmt.Errorf("spotify: decode %s: %w", path, derr)
		}
		return nil
	}

	return errors.New("spotify: unreachable: authorisation retry loop exhausted")
}

// resolve joins path onto the base URL and attaches the query.
func (c *Client) resolve(path string, q url.Values) (string, error) {
	rel, err := url.Parse(strings.TrimPrefix(path, "/"))
	if err != nil {
		return "", fmt.Errorf("spotify: parse path %q: %w", path, err)
	}
	// Ensure the base path is treated as a directory so ResolveReference appends.
	base := *c.baseURL
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	u := base.ResolveReference(rel)
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}
