// Command notify is the alarm fan-out Lambda: it subscribes to the alarm SNS topic and posts
// each message to Slack.
//
// It exists because an SNS email subscription needs the recipient to click a confirmation link,
// and production ran for weeks with that subscription in PendingConfirmation -- ten alarms
// configured, three of them firing, every notification going nowhere. A Slack incoming webhook
// has no handshake: the first post either works or fails loudly.
//
// It holds no AWS permissions beyond reading one SSM parameter and writing its own logs. In
// particular it cannot read the table or the Spotify refresh token: a function whose whole job
// is to forward text to a third party should not be able to reach anything worth exfiltrating.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/neovasili/spotistats/internal/config"
	"github.com/neovasili/spotistats/internal/notify"
)

// EnvEnvironment labels the deployment in the message footer.
const EnvEnvironment = "SPOTISTATS_ENVIRONMENT"

// client is built once per container, not per invocation: the webhook comes from SSM, and
// re-reading it on every alarm would turn an alarm storm into an SSM throttle.
var (
	once    sync.Once
	client  *notify.Client
	initErr error
)

func main() {
	lambda.Start(handle)
}

func handle(ctx context.Context, ev events.SNSEvent) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	c, err := notifier(ctx, log)
	if err != nil {
		// Fatal, and deliberately so. There is no useful degraded mode: the only job here is
		// delivery, and a success return would discard the alarm. Failing makes SNS retry and
		// eventually surfaces in the notifier's own error metric.
		return err
	}

	env := os.Getenv(EnvEnvironment)

	// Every record is attempted even if an earlier one fails, then the first error is
	// returned. Returning early would drop the remaining records: SNS retries the whole
	// invocation, but a batch that fails halfway would re-deliver the ones that succeeded and
	// still never deliver the ones after the failure if the same record keeps failing.
	var firstErr error
	for _, rec := range ev.Records {
		n := notify.Parse(rec.SNS.Subject, rec.SNS.Message)
		// The alarm NAME and state are safe to log; the message body is not logged at all.
		// A budget notification names the account and the spend, and there is no reason for
		// a second copy of it to sit in CloudWatch Logs.
		log.InfoContext(ctx, "notify: forwarding",
			"kind", n.Kind, "title", n.Title, "state", string(n.State))

		if err := c.Post(ctx, notify.Render(n, env)); err != nil {
			log.ErrorContext(ctx, "notify: post failed", "title", n.Title, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("notify %q: %w", n.Title, err)
			}
		}
	}
	return firstErr
}

// notifier builds the Slack client once, reading the webhook from SSM.
func notifier(ctx context.Context, log *slog.Logger) (*notify.Client, error) {
	once.Do(func() {
		cfg := config.Load()
		webhook, err := cfg.ResolveSlackWebhook(ctx)
		if err != nil {
			initErr = err
			return
		}
		if strings.TrimSpace(webhook) == "" {
			initErr = notify.ErrNoWebhook
			return
		}
		client, initErr = notify.New(notify.Config{Webhook: webhook, Logger: log})
	})
	if initErr != nil {
		// Reset so a container that started before the parameter existed can recover on its
		// next invocation instead of failing for its whole lifetime.
		once = sync.Once{}
		return nil, fmt.Errorf("notify: no Slack webhook available: %w", initErr)
	}
	if client == nil {
		return nil, errors.New("notify: client was not initialised")
	}
	return client, nil
}
