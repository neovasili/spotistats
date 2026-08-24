package notify_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neovasili/spotistats/internal/httpx"
	"github.com/neovasili/spotistats/internal/httpx/httpxtest"
	"github.com/neovasili/spotistats/internal/notify"
)

// fixedNow anchors the fake clock. Its value is irrelevant -- only that it never advances on
// its own, so a retry test cannot take real wall-clock time.
var fixedNow = time.Date(2026, 8, 23, 4, 15, 0, 0, time.UTC)

const fakeWebhook = "https://hooks.slack.com/services/T00000000/B00000000/SUPERSECRETTOKEN123"

func TestNewRequiresAWebhook(t *testing.T) {
	for _, url := range []string{"", "   "} {
		if _, err := notify.New(notify.Config{Webhook: url}); !errors.Is(err, notify.ErrNoWebhook) {
			t.Errorf("webhook %q: err = %v, want ErrNoWebhook", url, err)
		}
	}
}

func TestPostSendsTheRenderedPayload(t *testing.T) {
	var gotBody, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotType = string(b), r.Header.Get("Content-Type")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c, err := notify.New(notify.Config{Webhook: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Post(context.Background(), notify.Render(notify.Parse("", alarmJSON), "production")); err != nil {
		t.Fatal(err)
	}
	if gotType != "application/json" {
		t.Errorf("content-type = %q", gotType)
	}
	if !strings.Contains(gotBody, "spotistats-CaptureStale") {
		t.Errorf("body = %s", gotBody)
	}
}

// A non-2xx must be an error. SNS retries a failed Lambda invocation, so returning the error is
// what makes delivery durable -- swallowing it puts this notifier in exactly the position the
// unconfirmed email subscription was in: appearing to work, reaching nobody.
func TestPostFailsLoudlyOnRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid_payload"))
	}))
	defer srv.Close()

	c, _ := notify.New(notify.Config{Webhook: srv.URL})
	if err := c.Post(context.Background(), notify.SlackMessage{Text: "hi"}); err == nil {
		t.Fatal("a 400 from Slack was reported as success")
	}
}

// THE test for this file. The webhook URL is a bearer credential: anyone holding it can post to
// the channel. httpx puts the request URL in its error strings, which is correct for every
// other client in this repo and catastrophic here, because the error lands in a CloudWatch Logs
// group.
func TestPostNeverLeaksTheWebhook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	// The real Slack shape, so the token-stripping path is exercised too, with a Doer that
	// always fails so the error carries the URL.
	for name, cfg := range map[string]notify.Config{
		"http error": {
			Webhook: fakeWebhook,
			Doer: httpxtest.NewScriptedDoer(
				httpxtest.Step{Status: http.StatusForbidden, Body: "invalid_token"},
			),
			Retry: httpx.RetryPolicy{MaxAttempts: 1},
			Clock: httpxtest.NewFakeClock(fixedNow),
		},
		"transport error": {
			Webhook: fakeWebhook,
			Doer:    httpxtest.NewScriptedDoer(httpxtest.Step{Err: errors.New("dial " + fakeWebhook + ": refused")}),
			Retry:   httpx.RetryPolicy{MaxAttempts: 1},
			Clock:   httpxtest.NewFakeClock(fixedNow),
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := notify.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			err = c.Post(context.Background(), notify.SlackMessage{Text: "hi"})
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), "SUPERSECRETTOKEN123") {
				t.Errorf("the webhook token leaked into an error that will be logged:\n%s", err)
			}
			if strings.Contains(err.Error(), fakeWebhook) {
				t.Errorf("the full webhook URL leaked:\n%s", err)
			}
			// Proves the assertions above are not passing vacuously. httpx sets
			// APIError.Path from req.URL.Path, and for a Slack webhook the PATH is the
			// credential -- so the secret really is in the raw error and something must
			// have removed it.
			if !strings.Contains(err.Error(), "REDACTED") {
				t.Errorf("nothing was redacted, so this test would not catch a leak:\n%s", err)
			}
		})
	}
}

// A 429 or 5xx from Slack is retried rather than dropped: Slack rate-limits incoming webhooks
// at roughly one message per second, and an alarm storm will hit it.
func TestPostRetriesTransientFailures(t *testing.T) {
	doer := httpxtest.NewScriptedDoer(
		httpxtest.Step{Status: http.StatusTooManyRequests, Body: "rate limited"},
		httpxtest.Step{Status: http.StatusServiceUnavailable, Body: "nope"},
		httpxtest.Step{Status: http.StatusOK, Body: "ok"},
	)
	c, err := notify.New(notify.Config{
		Webhook: fakeWebhook,
		Doer:    doer,
		Retry:   httpx.RetryPolicy{MaxAttempts: 5, BaseDelay: 1, MaxDelay: 2},
		Clock:   httpxtest.NewFakeClock(fixedNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Post(context.Background(), notify.SlackMessage{Text: "hi"}); err != nil {
		t.Fatalf("gave up on a transient failure: %v", err)
	}
}
