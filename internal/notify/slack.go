package notify

import (
	"fmt"
	"net/url"
	"strings"
)

// Slack's attachment colours, used for the state stripe down the left of a message.
//
// These are status colours, not series colours: they mean good / serious, and they are never
// reused to distinguish one alarm from another. The state is ALSO written out as text in the
// message, so a reader with any form of colour blindness, or one reading the notification
// through a screen reader or an email digest, loses nothing.
const (
	colourAlarm        = "#c0392b"
	colourOK           = "#1baf7a"
	colourInsufficient = "#d68910"
	colourNeutral      = "#77756e"
)

// maxRawChars bounds the verbatim body. Slack truncates a long attachment silently and mid-word;
// truncating here means the cut is visible and says so.
const maxRawChars = 2500

// SlackMessage is the webhook payload.
//
// The attachment form is used rather than Block Kit because incoming webhooks render the colour
// stripe only for attachments, and the stripe is what makes "is anything broken?" answerable by
// glancing at the channel.
type SlackMessage struct {
	Text        string            `json:"text"`
	Attachments []SlackAttachment `json:"attachments,omitempty"`
}

type SlackAttachment struct {
	Color    string       `json:"color,omitempty"`
	Title    string       `json:"title,omitempty"`
	TitleURL string       `json:"title_link,omitempty"`
	Text     string       `json:"text,omitempty"`
	Fields   []SlackField `json:"fields,omitempty"`
	Footer   string       `json:"footer,omitempty"`
	TS       int64        `json:"ts,omitempty"`
	// MarkdownIn tells Slack which fields to render as mrkdwn. Without it the alarm reason
	// arrives with literal asterisks in it.
	MarkdownIn []string `json:"mrkdwn_in,omitempty"`
}

type SlackField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// Render builds the webhook payload for one notification.
//
// env labels which deployment spoke -- there is only one today, but a message that cannot say
// where it came from is a message that has to be guessed about the first time there are two.
func Render(n Notification, env string) SlackMessage {
	att := SlackAttachment{
		Color:      colourFor(n.State),
		Title:      titleFor(n),
		Text:       bodyFor(n),
		MarkdownIn: []string{"text", "fields"},
	}
	if url := consoleURL(n); url != "" {
		att.TitleURL = url
	}
	if !n.At.IsZero() {
		att.TS = n.At.Unix()
	}

	var footer []string
	if env != "" {
		footer = append(footer, env)
	}
	// The CODE is preferred in the footer, because it is what an operator pastes into a CLI or
	// a --region flag; the display name is the fallback for payloads that carry no ARN.
	if n.RegionCode != "" {
		footer = append(footer, n.RegionCode)
	} else if n.Region != "" {
		footer = append(footer, n.Region)
	}
	if n.AccountID != "" {
		footer = append(footer, "account "+n.AccountID)
	}
	att.Footer = strings.Join(footer, " · ")

	// The top-level text is what appears in the Slack notification popup and the channel
	// list, where the attachment is not rendered at all. It has to carry the state on its
	// own or a push notification says only "Spotistats".
	return SlackMessage{
		Text:        summaryLine(n),
		Attachments: []SlackAttachment{att},
	}
}

// summaryLine is the push-notification line: state, then name, and nothing else.
func summaryLine(n Notification) string {
	switch n.State {
	case StateAlarm:
		return fmt.Sprintf("🔴 ALARM · %s", n.Title)
	case StateOK:
		return fmt.Sprintf("🟢 RECOVERED · %s", n.Title)
	case StateInsufficientData:
		return fmt.Sprintf("🟡 NO DATA · %s", n.Title)
	}
	if n.Kind == "budget" {
		return fmt.Sprintf("💰 BUDGET · %s", n.Title)
	}
	return n.Title
}

func titleFor(n Notification) string {
	if n.Kind == "alarm" {
		return n.Title
	}
	return ""
}

// bodyFor is the message body.
//
// For an alarm it is the DESCRIPTION first and the CloudWatch reason second. That order is
// deliberate: the description is the sentence this repo wrote about what the alarm means and
// what to do, while the reason is CloudWatch restating the arithmetic ("1 datapoint [1.0] was
// greater than the threshold [1.0]"). The actionable half goes where it is read.
func bodyFor(n Notification) string {
	if n.Kind != "alarm" {
		return truncate(strings.TrimSpace(n.Raw), maxRawChars)
	}
	var b strings.Builder
	if n.Description != "" {
		b.WriteString(n.Description)
	}
	if n.Reason != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("_" + n.Reason + "_")
	}
	return truncate(b.String(), maxRawChars)
}

func colourFor(s State) string {
	switch s {
	case StateAlarm:
		return colourAlarm
	case StateOK:
		return colourOK
	case StateInsufficientData:
		return colourInsufficient
	default:
		return colourNeutral
	}
}

// consoleURL deep links to the alarm.
//
// Only for alarms, and only when the region is known: a link built from a guessed region lands
// on "alarm not found", which is worse than no link because it reads as the alarm having been
// deleted.
func consoleURL(n Notification) string {
	// RegionCode, never Region: the latter is a display name, and "EU (Ireland)" in a hostname
	// produces a link that cannot resolve.
	if n.Kind != "alarm" || n.RegionCode == "" || n.Title == "" {
		return ""
	}
	return fmt.Sprintf(
		"https://%s.console.aws.amazon.com/cloudwatch/home?region=%s#alarmsV2:alarm/%s",
		n.RegionCode, n.RegionCode, urlPathEscape(n.Title),
	)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Cut on a rune boundary, and say that it was cut. A message that silently ends
	// mid-sentence reads as a broken notifier.
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n\n…truncated."
}

// urlPathEscape escapes an alarm name for a console fragment. url.PathEscape leaves "+" alone,
// which the console then reads as a space -- so it is encoded explicitly.
func urlPathEscape(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "+", "%2B")
}
