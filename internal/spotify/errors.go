package spotify

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

var (
	// ErrRefreshTokenInvalid means Spotify rejected the stored refresh token, normally
	// because app access was revoked. It is terminal: retrying cannot help, and the fix
	// is to re-run the interactive authorisation flow (docs/PREREQUISITES.md step 6).
	ErrRefreshTokenInvalid = errors.New("spotify: refresh token invalid or revoked")

	// ErrRotationPersistFailed means Spotify issued a new refresh token but it could not
	// be written to durable storage. The old token is likely already invalid, so this
	// needs human attention before the process exits.
	ErrRotationPersistFailed = errors.New("spotify: rotated refresh token could not be persisted")

	// ErrCursorConflict means both After and Before were supplied. The recently-played
	// endpoint accepts at most one.
	ErrCursorConflict = errors.New("spotify: after and before are mutually exclusive")
)

// APIError is a non-2xx response from the Web API.
type APIError struct {
	StatusCode int
	Status     string
	Message    string // Spotify's error.message, when the body carried one
	Method     string
	Path       string
	RequestID  string

	// Body is the response body, truncated. It is retained because the accounts
	// endpoint uses a different error shape from the Web API
	// ({"error":"invalid_grant","error_description":"..."} rather than
	// {"error":{"status":N,"message":"..."}}), so the token flow needs to re-parse it.
	Body []byte
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("spotify: %s %s: %d %s", e.Method, e.Path, e.StatusCode, e.Status)
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.RequestID != "" {
		s += " (request-id " + e.RequestID + ")"
	}
	return s
}

// Retryable reports whether the status is one where retrying can plausibly succeed.
// 401 is excluded: it needs a token refresh, which is a different remedy handled by the
// authorisation layer rather than by waiting.
func (e *APIError) Retryable() bool {
	if e.StatusCode == http.StatusTooManyRequests {
		return true
	}
	switch e.StatusCode {
	case http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// RateLimitError is a 429. It is returned rather than slept through when the requested
// wait exceeds the policy's ceiling, so a short-lived process can end cleanly instead of
// blocking past its own deadline.
type RateLimitError struct {
	APIError
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%s: retry after %s", e.APIError.Error(), e.RetryAfter)
}

// AuthError is a failure from the accounts.spotify.com token endpoint, which uses a
// different error shape from the Web API.
type AuthError struct {
	StatusCode  int
	Code        string // OAuth2 error code, e.g. "invalid_grant"
	Description string
}

func (e *AuthError) Error() string {
	s := fmt.Sprintf("spotify: token endpoint: %d", e.StatusCode)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Description != "" {
		s += ": " + e.Description
	}
	return s
}

// Terminal reports whether retrying is pointless. These codes mean the credentials or
// the request itself are wrong, not that the service is busy.
func (e *AuthError) Terminal() bool {
	switch e.Code {
	case "invalid_grant", "invalid_client", "invalid_request", "unsupported_grant_type":
		return true
	}
	return false
}

// Unwrap exposes ErrRefreshTokenInvalid for terminal failures so callers can test with
// errors.Is rather than string matching.
func (e *AuthError) Unwrap() error {
	if e.Terminal() {
		return ErrRefreshTokenInvalid
	}
	return nil
}
