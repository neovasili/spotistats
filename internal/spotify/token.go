package spotify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/neovasili/spotistats/internal/httpx"
)

// DefaultTokenURL is Spotify's OAuth2 token endpoint.
const DefaultTokenURL = "https://accounts.spotify.com/api/token"

// defaultTokenSkew refreshes slightly before expiry to absorb clock drift and the
// round-trip of the request the token is about to be used for.
const defaultTokenSkew = 60 * time.Second

// defaultTokenTTL is used when the token response omits expires_in. Spotify always
// sends 3600, but a missing value must not mean "never expires".
const defaultTokenTTL = time.Hour

// Credentials are the app's client ID and secret from the Spotify developer dashboard.
type Credentials struct {
	ClientID     string
	ClientSecret string
}

// basicAuth renders the credentials for the Authorization header.
func (c Credentials) basicAuth() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.ClientID+":"+c.ClientSecret))
}

// RefreshTokenStore persists the long-lived refresh token across process restarts.
//
// Production backs this with an SSM SecureString parameter, but this package must not
// know that -- hence the interface. Tests use an in-memory implementation, which is what
// keeps the whole client testable with no AWS involvement.
type RefreshTokenStore interface {
	Get(ctx context.Context) (string, error)
	// Put is called only when Spotify issues a NEW refresh token. It must be durable
	// before it returns.
	Put(ctx context.Context, refreshToken string) error
}

// TokenSource yields a currently-valid access token.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// TokenSourceConfig configures a RefreshingTokenSource.
type TokenSourceConfig struct {
	Credentials Credentials
	Store       RefreshTokenStore
	Doer        Doer
	Clock       Clock
	Retry       RetryPolicy
	TokenURL    string

	// Skew is how long before nominal expiry a token is considered stale.
	Skew time.Duration

	// OnRotationError fires when Spotify issued a new refresh token but Store.Put
	// failed. Wire it to the TokenRefreshFailed alarm: this is the one failure in the
	// token flow that needs a human, because the previous token is probably already
	// invalid and the new one exists only in memory.
	OnRotationError func(context.Context, error)

	Logger *slog.Logger
}

// RefreshingTokenSource exchanges a stored refresh token for access tokens, caching each
// one until shortly before it expires.
//
// Access tokens live one hour. Refresh tokens do not expire but can be revoked, and
// Spotify MAY issue a replacement on any refresh -- handling that rotation durably is
// the whole reason this is hand-rolled rather than golang.org/x/oauth2, whose TokenSource
// offers no hook to persist a rotated refresh token.
type RefreshingTokenSource struct {
	creds    Credentials
	store    RefreshTokenStore
	retrier  *httpx.Retrier
	clock    Clock
	tokenURL string
	skew     time.Duration
	onRotErr func(context.Context, error)
	log      *slog.Logger

	// mu guards the cached token and is held across the refresh HTTP call. That
	// serialises concurrent expiries into a single request -- batch metadata enrichment
	// runs parallel calls, and without this each would refresh independently.
	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time

	// pendingRotation holds a rotated refresh token that Store.Put failed to persist.
	pendingRotation string
}

var _ TokenSource = (*RefreshingTokenSource)(nil)

// NewRefreshingTokenSource validates cfg and returns a token source.
func NewRefreshingTokenSource(cfg TokenSourceConfig) (*RefreshingTokenSource, error) {
	if cfg.Credentials.ClientID == "" || cfg.Credentials.ClientSecret == "" {
		return nil, errors.New("spotify: client ID and secret are required")
	}
	if cfg.Store == nil {
		return nil, errors.New("spotify: a RefreshTokenStore is required")
	}
	clock := cfg.Clock
	if clock == nil {
		clock = SystemClock()
	}
	doer := cfg.Doer
	if doer == nil {
		doer = &http.Client{Timeout: 15 * time.Second}
	}
	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = DefaultTokenURL
	}
	skew := cfg.Skew
	if skew <= 0 {
		skew = defaultTokenSkew
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return &RefreshingTokenSource{
		creds:    cfg.Credentials,
		store:    cfg.Store,
		retrier:  httpx.NewRetrier(httpx.RetrierConfig{Doer: doer, Policy: cfg.Retry, Clock: clock, Log: log}),
		clock:    clock,
		tokenURL: tokenURL,
		skew:     skew,
		onRotErr: cfg.OnRotationError,
		log:      log,
	}, nil
}

// Token returns a valid access token, refreshing if the cached one is missing or within
// Skew of expiry.
func (ts *RefreshingTokenSource) Token(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.accessToken != "" && ts.clock.Now().Add(ts.skew).Before(ts.expiresAt) {
		return ts.accessToken, nil
	}
	return ts.refreshLocked(ctx)
}

// Invalidate discards the cached access token so the next Token call refreshes. The
// client calls this on a 401, which can happen before nominal expiry if the token was
// revoked server-side.
func (ts *RefreshingTokenSource) Invalidate() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.accessToken = ""
	ts.expiresAt = time.Time{}
}

// PendingRotation returns a rotated refresh token that could not be persisted, so a
// caller can retry the write before the process exits (typically in a defer).
//
// Never log the returned value.
func (ts *RefreshingTokenSource) PendingRotation() (string, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.pendingRotation, ts.pendingRotation != ""
}

// ClearPendingRotation marks a previously failed rotation as persisted.
func (ts *RefreshingTokenSource) ClearPendingRotation() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.pendingRotation = ""
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// refreshLocked performs the refresh. ts.mu must be held.
func (ts *RefreshingTokenSource) refreshLocked(ctx context.Context) (string, error) {
	// A rotation we failed to persist is still the newest token we have; preferring the
	// stored value here would send an already-invalidated one.
	current := ts.pendingRotation
	if current == "" {
		got, err := ts.store.Get(ctx)
		if err != nil {
			return "", fmt.Errorf("spotify: read refresh token: %w", err)
		}
		current = got
	}
	if current == "" {
		return "", fmt.Errorf("%w: no refresh token stored", ErrRefreshTokenInvalid)
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {current},
	}
	body := form.Encode()

	newReq := func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.tokenURL, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", ts.creds.basicAuth())
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r, nil
	}

	resp, err := ts.retrier.Do(ctx, newReq)
	if err != nil {
		return "", asAuthError(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, httpx.MaxErrorBodyBytes)).Decode(&tr); err != nil {
		return "", fmt.Errorf("spotify: decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("spotify: token response contained no access_token")
	}

	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}
	ts.accessToken = tr.AccessToken
	ts.expiresAt = ts.clock.Now().Add(ttl)

	// Rotation. Spotify may or may not issue a replacement refresh token.
	if tr.RefreshToken != "" && tr.RefreshToken != current {
		if perr := ts.store.Put(ctx, tr.RefreshToken); perr != nil {
			// The dangerous path: Spotify has probably already invalidated the previous
			// token, so the replacement exists only in this process's memory.
			//
			// The access token is still returned successfully. Failing the call here
			// would GUARANTEE the loss rather than merely risk it, and it would force
			// every call site to errors.Is-and-continue -- where the one that forgets
			// turns a recoverable warning into an outage. Instead: cache it, alarm, and
			// expose it via PendingRotation so the caller can retry the write.
			ts.pendingRotation = tr.RefreshToken
			wrapped := fmt.Errorf("%w: %w", ErrRotationPersistFailed, perr)
			ts.log.ErrorContext(ctx, "spotify: rotated refresh token could not be persisted; "+
				"retry via PendingRotation before exit or the next run will fail to authenticate",
				"err", perr)
			if ts.onRotErr != nil {
				ts.onRotErr(ctx, wrapped)
			}
		} else {
			ts.pendingRotation = ""
			ts.log.InfoContext(ctx, "spotify: refresh token rotated and persisted")
		}
	}

	return ts.accessToken, nil
}

// asAuthError converts a token-endpoint failure into an *AuthError when the body carries
// the OAuth2 error shape, so callers can test for a revoked token with errors.Is.
func asAuthError(err error) error {
	var api *APIError
	if !errors.As(err, &api) {
		return err
	}
	var wire struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	ae := &AuthError{StatusCode: api.StatusCode}
	if jerr := json.Unmarshal(api.Body, &wire); jerr == nil {
		ae.Code = wire.Error
		ae.Description = wire.Description
	}
	if ae.Code == "" && api.StatusCode == http.StatusBadRequest {
		// A 400 with an unrecognised body is still a client-side problem; treat it as
		// terminal rather than retrying a request that cannot succeed.
		ae.Code = "invalid_request"
	}
	return ae
}
