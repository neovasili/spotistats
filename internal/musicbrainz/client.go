// Package musicbrainz reads structured artist facts from the MusicBrainz web service.
//
// # What it is for
//
// Spotify's artist object carries a name, genres, popularity, followers and one photo, and
// nothing else. Formation date, origin, type and band members exist in MusicBrainz, and — the
// part that makes this worth building — they join to a Spotify artist EXACTLY, through a URL
// relationship a human editor asserted, never through a name match.
//
// # Rate limiting is the dominant design constraint
//
// MusicBrainz allows one request per second per IP and answers 503 to EVERYTHING from that IP
// once exceeded. In practice it answers 503 to roughly half of all requests anyway, well under
// the limit, so 503 is treated as backpressure to retry rather than a failure to report. A
// client without a limiter here does not degrade, it stops working.
package musicbrainz

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

// DefaultBaseURL is the MusicBrainz web service root.
const DefaultBaseURL = "https://musicbrainz.org/ws/2"

// requestsPerWindow and window implement the documented 1 req/s average.
//
// Expressed as a rolling window rather than a fixed sleep so a burst of cache hits does not
// pay for requests it never made, and so the limiter composes with the retrier's own backoff.
const (
	requestsPerWindow = 1
	window            = time.Second
)

// maxResourcesPerLookup is the URL-lookup batch ceiling.
//
// This is the single most valuable optimisation in the package: 2,000 artists resolve in 20
// requests rather than 2,000, which is the difference between a 20-second job and a 30-minute
// one at 1 req/s.
const maxResourcesPerLookup = 100

// maxResponseBytes caps a decoded response so a pathological body cannot exhaust a Lambda.
const maxResponseBytes = 8 << 20

// Config configures a Client. UserAgent is required.
type Config struct {
	// UserAgent must be descriptive and carry contact information. MusicBrainz throttles
	// anonymous and default library agents far harder as a class, so this is validated at
	// construction rather than defaulted — see httpx.RequireUserAgent.
	UserAgent string
	BaseURL   string
	Doer      httpx.Doer
	Retry     httpx.RetryPolicy
	Clock     httpx.Clock
	Logger    *slog.Logger
}

// Client is a MusicBrainz web service client.
type Client struct {
	baseURL   *url.URL
	userAgent string
	retrier   *httpx.Retrier
	log       *slog.Logger
}

// New builds a Client, refusing to construct without a contact string.
func New(cfg Config) (*Client, error) {
	if err := httpx.RequireUserAgent(cfg.UserAgent); err != nil {
		return nil, fmt.Errorf("musicbrainz: %w", err)
	}
	raw := cfg.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("musicbrainz: parse base URL %q: %w", raw, err)
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
	policy := cfg.Retry
	if policy.MaxAttempts == 0 {
		// More attempts than the shared default: MusicBrainz 503s about half the time even
		// within its own limit, so a five-attempt budget spends itself on normal backpressure.
		policy = httpx.DefaultRetryPolicy()
		policy.MaxAttempts = 8
	}
	return &Client{
		baseURL:   base,
		userAgent: cfg.UserAgent,
		retrier: httpx.NewRetrier(httpx.RetrierConfig{
			Doer:    doer,
			Policy:  policy,
			Clock:   clock,
			Limiter: httpx.NewWindowLimiter(requestsPerWindow, window, clock),
			Log:     log,
		}),
		log: log,
	}, nil
}

// spotifyArtistURL is the resource MusicBrainz indexes a Spotify artist under.
func spotifyArtistURL(spotifyID string) string {
	return "https://open.spotify.com/artist/" + spotifyID
}

// ResolveSpotifyArtists maps Spotify artist IDs to MusicBrainz IDs.
//
// IDs with no asserted relationship are simply absent from the result — there is no fuzzy
// fallback, deliberately. A name match attaches the wrong biography, members and country to a
// real band and nothing downstream can detect it; an absent artist is a visible gap.
func (c *Client) ResolveSpotifyArtists(
	ctx context.Context, spotifyIDs []string,
) (map[string]string, error) {
	out := make(map[string]string, len(spotifyIDs))
	unique := dedupe(spotifyIDs)
	if len(unique) == 0 {
		return out, nil
	}

	for start := 0; start < len(unique); start += maxResourcesPerLookup {
		end := min(start+maxResourcesPerLookup, len(unique))
		batch := unique[start:end]

		q := url.Values{}
		for _, id := range batch {
			q.Add("resource", spotifyArtistURL(id))
		}
		q.Set("inc", "artist-rels")
		q.Set("fmt", "json")

		body, err := c.get(ctx, "url", q)
		if err != nil {
			return out, fmt.Errorf("musicbrainz: resolve %d artist URL(s): %w", len(batch), err)
		}

		for _, u := range decodeURLLookup(body, len(batch)) {
			// Keyed by the resource the response reports, NEVER by position. MusicBrainz does
			// not preserve request order -- asking for [A, B] can return [B, A] -- so a
			// positional read silently attaches one artist's MBID to another. That is exactly
			// the mis-resolution this package refuses to risk.
			id := spotifyIDFromResource(u.Resource)
			if id == "" {
				continue
			}
			if mbid := artistMBIDFrom(u.Relations); mbid != "" {
				out[id] = mbid
			}
		}
	}
	return out, nil
}

// decodeURLLookup handles BOTH response shapes the URL endpoint returns.
//
// With two or more `resource` parameters the body is {"url-count", "url-offset", "urls": [...]}.
// With exactly ONE, MusicBrainz returns the bare URL entity — relations at the top level, no
// wrapper. A client written against only the batch shape decodes nothing for a batch of one,
// and a batch of one is not exotic: it is the tail chunk of any count that is not a multiple
// of 100, and it is what every single-artist run sends.
func decodeURLLookup(body []byte, batchSize int) []URLEntity {
	if batchSize == 1 {
		var single URLEntity
		if err := json.Unmarshal(body, &single); err != nil || single.Resource == "" {
			return nil
		}
		return []URLEntity{single}
	}
	var batch urlBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil
	}
	return batch.URLs
}

// artistMBIDFrom returns the MBID of the artist a "free streaming" relation points at.
func artistMBIDFrom(rels []Relation) string {
	for _, r := range rels {
		if r.Type == relFreeStreaming && r.Artist != nil && r.Artist.ID != "" {
			return r.Artist.ID
		}
	}
	return ""
}

// relFreeStreaming is the relationship type MusicBrainz records a Spotify page under.
const relFreeStreaming = "free streaming"

// spotifyIDFromResource extracts the Spotify ID from the resource URL, or "" if it is not one.
func spotifyIDFromResource(resource string) string {
	const prefix = "https://open.spotify.com/artist/"
	if !strings.HasPrefix(resource, prefix) {
		return ""
	}
	// Strip any query or trailing path a stored URL might carry.
	id := strings.TrimPrefix(resource, prefix)
	if i := strings.IndexAny(id, "?/#"); i >= 0 {
		id = id[:i]
	}
	return id
}

// Artist fetches one artist with its genres and relationships.
//
// Returns found=false for a 404 rather than an error: an MBID that no longer exists is an
// answer, not a failure, and the caller tombstones it.
func (c *Client) Artist(ctx context.Context, mbid string) (Artist, bool, error) {
	if mbid == "" {
		return Artist{}, false, nil
	}
	q := url.Values{}
	q.Set("inc", "genres+artist-rels")
	q.Set("fmt", "json")

	body, err := c.get(ctx, "artist/"+url.PathEscape(mbid), q)
	if err != nil {
		if httpx.NotFound(err) {
			return Artist{}, false, nil
		}
		return Artist{}, false, fmt.Errorf("musicbrainz: artist %s: %w", mbid, err)
	}
	var a Artist
	if err := json.Unmarshal(body, &a); err != nil {
		return Artist{}, false, fmt.Errorf("musicbrainz: decode artist %s: %w", mbid, err)
	}
	if a.ID == "" {
		return Artist{}, false, nil
	}
	return a, true, nil
}

// get issues a request through the limiter and retrier and returns the body.
func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	base := *c.baseURL
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	rel, err := url.Parse(strings.TrimPrefix(path, "/"))
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
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
}

// dedupe removes blanks and duplicates, preserving first-seen order so request composition is
// deterministic and assertable. A duplicate ID here is a whole wasted slot in a 100-item batch.
func dedupe(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
