package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// APIError is a non-2xx HTTP response.
type APIError struct {
	StatusCode int
	Status     string
	Message    string // the API's error message, when the body carried one
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

// NotFound reports whether err is a 404 from the Web API.
//
// A single-item get answers "this ID does not exist" with 404, where the removed batch
// endpoints answered with a positional null. Both mean the same thing to a caller -- record a
// tombstone -- so this keeps that distinction inside the client rather than leaking an
// HTTP status into the ingest pipeline.
func NotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// ErrUserAgentRequired means a client was constructed without a contact string.
//
// This is a transport concern, not a caller's, because the requirement comes from the wire:
// MusicBrainz mandates a descriptive User-Agent with contact information and throttles
// anonymous or default library agents (`Python-urllib`, `Java`, blank) far harder as a class.
// A client that silently sent a default would work in testing and be rate-limited into
// uselessness in production.
var ErrUserAgentRequired = errors.New("httpx: a descriptive User-Agent with contact information is required")

// RequireUserAgent validates a User-Agent string, returning ErrUserAgentRequired when it is
// absent or plainly not a contact.
//
// The check is deliberately shallow: it rejects the empty and the obviously-default, and does
// not attempt to validate that a URL or address is reachable. A stricter rule would reject
// legitimate agents and teach people to work around it.
func RequireUserAgent(s string) error {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ErrUserAgentRequired
	}
	// A bare token with no contact detail is what the throttling class looks for.
	if !strings.ContainsAny(trimmed, "(@") && !strings.Contains(trimmed, "http") {
		return fmt.Errorf("%w: %q names no contact (want e.g. "+
			`"myapp/1.0 ( https://example.com )")`, ErrUserAgentRequired, trimmed)
	}
	for _, bad := range []string{"python-urllib", "java/", "go-http-client", "curl/", "libwww"} {
		if strings.Contains(strings.ToLower(trimmed), bad) {
			return fmt.Errorf("%w: %q is a default library agent, which is throttled as a class",
				ErrUserAgentRequired, trimmed)
		}
	}
	return nil
}
