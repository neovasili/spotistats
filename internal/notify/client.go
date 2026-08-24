package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/neovasili/spotistats/internal/httpx"
)

// ErrNoWebhook means no webhook URL was configured, which is a deployment fault rather than a
// runtime one: the notifier exists precisely so alarms reach someone.
var ErrNoWebhook = errors.New("notify: no Slack webhook URL configured")

// Client posts to a Slack incoming webhook.
type Client struct {
	webhook string
	retrier *httpx.Retrier
	log     *slog.Logger
}

// Config configures a Client. Webhook is required.
type Config struct {
	// Webhook is the full incoming-webhook URL. It is a BEARER SECRET IN A URL: anyone with
	// it can post to the channel. It is never logged, and errors are redacted before they
	// leave this package -- the same rule TheAudioDB's path-segment key follows.
	Webhook string
	Doer    httpx.Doer
	Retry   httpx.RetryPolicy
	Clock   httpx.Clock
	Logger  *slog.Logger
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Webhook) == "" {
		return nil, ErrNoWebhook
	}
	doer := cfg.Doer
	if doer == nil {
		doer = http.DefaultClient
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{
		webhook: cfg.Webhook,
		retrier: httpx.NewRetrier(httpx.RetrierConfig{
			Doer: doer, Policy: cfg.Retry, Clock: cfg.Clock, Log: log,
		}),
		log: log,
	}, nil
}

// Post delivers one message.
//
// A non-2xx is an error, deliberately. SNS retries a failed Lambda invocation, so returning the
// error is what makes delivery durable; swallowing it would put this notifier back in the
// position the email subscription was in -- appearing to work and reaching nobody.
func (c *Client) Post(ctx context.Context, msg SlackMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("notify: encode payload: %w", err)
	}

	resp, err := c.retrier.Do(ctx, func() (*http.Request, error) {
		req, rerr := http.NewRequestWithContext(
			ctx, http.MethodPost, c.webhook, bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
	if err != nil {
		return c.redact(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return nil
}

// redact removes the webhook URL from an error before it can reach a log.
//
// httpx puts the request URL in its error strings, which is right for every other client in
// this repo and wrong for exactly this one: here the URL IS the credential. A CloudWatch Logs
// group that anyone with read access can grep is not where it should end up.
func (c *Client) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !strings.Contains(msg, c.webhook) {
		// The secret is the path, so a partial leak matters too: strip the token segment
		// even when the full URL is not present verbatim.
		if token := webhookToken(c.webhook); token != "" && strings.Contains(msg, token) {
			return fmt.Errorf("%s", strings.ReplaceAll(msg, token, "REDACTED"))
		}
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(msg, c.webhook, "https://hooks.slack.com/REDACTED"))
}

// webhookToken is the secret tail of a Slack webhook URL:
// https://hooks.slack.com/services/T000/B000/XXXXXXXX -> everything after /services/.
func webhookToken(webhook string) string {
	const marker = "/services/"
	i := strings.Index(webhook, marker)
	if i < 0 {
		return ""
	}
	return webhook[i+len(marker):]
}
