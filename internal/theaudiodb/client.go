// Package theaudiodb reads artist prose and artwork from TheAudioDB.
//
// # What it is for, and what it is deliberately not for
//
// MusicBrainz holds no prose and no artist images at all, so biography, thumbnails, logos,
// banners and fanart come from here. Its STRUCTURED fields are read only to be ignored:
// intFormedYear disagrees with MusicBrainz and sometimes with its own biography, strCountry is
// free text where MusicBrainz gives an ISO code plus a city, and intMembers is a count with no
// names. This is a fan-curated artwork database with metadata attached, and it is excellent at
// the artwork.
//
// # There is no search
//
// TheAudioDB offers search.php?s={name}, and it is tempting for the artists MusicBrainz has not
// linked. It is not implemented here, and that absence is the point: an unexported, untested
// gap is the cheapest way to guarantee a future contributor cannot reach for a fuzzy name match
// that would attach the wrong biography to a real band.
package theaudiodb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/neovasili/spotistats/internal/httpx"
)

// DefaultBaseURL is TheAudioDB's JSON API root.
const DefaultBaseURL = "https://www.theaudiodb.com/api/v1/json"

// TestAPIKey is TheAudioDB's public test key.
//
// It works and is rate-limited hard. Named rather than inlined so a deployment accidentally
// running on it is greppable instead of mysterious.
const TestAPIKey = "123"

// The free key allows 30 requests per minute and answers 429 above it. Expressed as a rolling
// window so a burst does not pay for requests it never made.
const (
	requestsPerWindow = 30
	window            = time.Minute
)

const maxResponseBytes = 8 << 20

// Config configures a Client. APIKey is required.
type Config struct {
	APIKey  string
	BaseURL string
	Doer    httpx.Doer
	Retry   httpx.RetryPolicy
	Clock   httpx.Clock
	Logger  *slog.Logger
	// UserAgent is not enforced by TheAudioDB, but sending a contact string is basic
	// etiquette towards a free service and costs nothing.
	UserAgent string
}

// Client is a TheAudioDB client.
type Client struct {
	baseURL   *url.URL
	apiKey    string
	userAgent string
	retrier   *httpx.Retrier
	log       *slog.Logger
}

// ErrAPIKeyRequired means no key was configured.
//
// A missing key does not fail loudly at TheAudioDB -- the path simply 404s or returns an empty
// result -- so it is caught at construction instead, where the cause is obvious.
var ErrAPIKeyRequired = fmt.Errorf("theaudiodb: an API key is required")

// New builds a Client.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, ErrAPIKeyRequired
	}
	raw := cfg.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("theaudiodb: parse base URL %q: %w", raw, err)
	}
	doer := cfg.Doer
	if doer == nil {
		doer = &http.Client{Timeout: 20 * time.Second}
	}
	clock := cfg.Clock
	if clock == nil {
		clock = httpx.SystemClock()
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
		apiKey:    cfg.APIKey,
		userAgent: ua,
		retrier: httpx.NewRetrier(httpx.RetrierConfig{
			Doer:    doer,
			Policy:  cfg.Retry,
			Clock:   clock,
			Limiter: httpx.NewWindowLimiter(requestsPerWindow, window, clock),
			Log:     log,
		}),
		log: log,
	}, nil
}

// ArtistByMBID looks an artist up by MusicBrainz ID.
//
// TheAudioDB indexes by MBID directly, which is what makes the two-hop join exact: no name is
// ever compared. found=false means the MBID is not in their database, which is common and not
// an error.
func (c *Client) ArtistByMBID(ctx context.Context, mbid string) (Artist, bool, error) {
	if mbid == "" {
		return Artist{}, false, nil
	}
	q := url.Values{}
	q.Set("i", mbid)

	body, err := c.get(ctx, "artist-mb.php", q)
	if err != nil {
		if httpx.NotFound(err) {
			return Artist{}, false, nil
		}
		return Artist{}, false, fmt.Errorf("theaudiodb: artist %s: %w", mbid, err)
	}

	var resp artistResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Artist{}, false, fmt.Errorf("theaudiodb: decode artist %s: %w", mbid, err)
	}
	// An unknown MBID returns {"artists": null} with HTTP 200, so absence has to be detected
	// from the body rather than the status.
	if len(resp.Artists) == 0 || resp.Artists[0].ID == "" {
		return Artist{}, false, nil
	}
	return resp.Artists[0], true, nil
}

// get issues a request through the limiter and retrier and returns the body.
//
// The API key is a PATH segment, not a query parameter, so it is escaped as one -- and it must
// never reach a log line or an error message.
func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	base := *c.baseURL
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	rel, err := url.Parse(url.PathEscape(c.apiKey) + "/" + strings.TrimPrefix(path, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse path %q: %w", path, err)
	}
	u := base.ResolveReference(rel)
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	target := u.String()

	resp, err := c.retrier.Do(ctx, func() (*http.Request, error) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("User-Agent", c.userAgent)
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		// The URL carries the API key, so any error mentioning it is redacted before it can
		// reach a log. httpx.APIError includes the request path.
		return nil, redactKey(err, c.apiKey)
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
}

// redactKey replaces the API key in an error message with a placeholder.
//
// The key is a path segment, so it appears in every request URL and therefore in every
// transport error. Logging one would leak a credential into CloudWatch, where it long outlives
// the request.
func redactKey(err error, key string) error {
	if err == nil || key == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, key) {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(msg, key, "REDACTED"))
}
