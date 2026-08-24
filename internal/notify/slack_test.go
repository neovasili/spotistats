package notify_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/neovasili/spotistats/internal/notify"
)

func TestRenderAlarm(t *testing.T) {
	msg := notify.Render(notify.Parse("", alarmJSON), "production")

	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments = %d", len(msg.Attachments))
	}
	att := msg.Attachments[0]

	// The push-notification line must carry the state on its own: the attachment is not
	// rendered in the popup or the channel list, so "Spotistats" alone tells you nothing.
	if !strings.Contains(msg.Text, "ALARM") || !strings.Contains(msg.Text, "spotistats-CaptureStale") {
		t.Errorf("summary = %q", msg.Text)
	}
	// The description this repo wrote comes FIRST; CloudWatch's arithmetic restatement second.
	descAt := strings.Index(att.Text, "capture run")
	reasonAt := strings.Index(att.Text, "Threshold Crossed")
	if descAt < 0 || reasonAt < 0 || descAt > reasonAt {
		t.Errorf("the actionable description must precede the reason:\n%s", att.Text)
	}
	if att.Color == "" {
		t.Error("no colour stripe")
	}
	if !strings.Contains(att.TitleURL, "eu-west-1") || !strings.Contains(att.TitleURL, "alarmsV2") {
		t.Errorf("console link = %q", att.TitleURL)
	}
	if att.TS == 0 {
		t.Error("no timestamp, so Slack will stamp the message with delivery time instead")
	}
	if !strings.Contains(att.Footer, "production") || !strings.Contains(att.Footer, "eu-west-1") {
		t.Errorf("footer = %q", att.Footer)
	}
}

// Colour is never the only signal. Every state must be named in the text as well, so the
// message survives colour blindness, a screen reader, and Slack's own email digest.
func TestRenderNeverRelesOnColourAlone(t *testing.T) {
	for state, want := range map[string]string{
		"ALARM":             "ALARM",
		"OK":                "RECOVERED",
		"INSUFFICIENT_DATA": "NO DATA",
	} {
		body := strings.Replace(alarmJSON, `"NewStateValue": "ALARM"`, `"NewStateValue": "`+state+`"`, 1)
		msg := notify.Render(notify.Parse("", body), "production")
		if !strings.Contains(msg.Text, want) {
			t.Errorf("state %s: summary %q does not name the state", state, msg.Text)
		}
		if msg.Attachments[0].Color == "" {
			t.Errorf("state %s: no colour", state)
		}
	}
}

func TestRenderDistinguishesStatesByColour(t *testing.T) {
	colourOf := func(state string) string {
		body := strings.Replace(alarmJSON, `"NewStateValue": "ALARM"`, `"NewStateValue": "`+state+`"`, 1)
		return notify.Render(notify.Parse("", body), "").Attachments[0].Color
	}
	alarm, ok := colourOf("ALARM"), colourOf("OK")
	if alarm == ok {
		t.Errorf("ALARM and OK share the colour %q, so the channel cannot be skimmed", alarm)
	}
}

func TestRenderBudget(t *testing.T) {
	const prose = "You requested that we alert you when the ACTUAL Cost associated with your " +
		"spotistats-monthly budget exceeded 80%."
	msg := notify.Render(notify.Parse("AWS Budgets: spotistats-monthly", prose), "production")

	if !strings.Contains(msg.Text, "BUDGET") {
		t.Errorf("summary = %q", msg.Text)
	}
	// The prose is posted verbatim. Extracting the amount would mean pattern-matching an
	// English sentence AWS can reword at will, and a silently-empty amount is worse than
	// the full sentence.
	if !strings.Contains(msg.Attachments[0].Text, "80%") {
		t.Errorf("body lost the prose: %q", msg.Attachments[0].Text)
	}
	// No console link: there is no alarm to link to, and a link built from a guessed region
	// lands on "not found", which reads as the alarm having been deleted.
	if msg.Attachments[0].TitleURL != "" {
		t.Errorf("budget message has a CloudWatch link: %q", msg.Attachments[0].TitleURL)
	}
}

func TestRenderRawMessageStillProducesAPost(t *testing.T) {
	msg := notify.Render(notify.Parse("", "unstructured"), "")
	if msg.Text == "" {
		t.Error("empty summary, so the Slack post would show as blank")
	}
	if msg.Attachments[0].Text != "unstructured" {
		t.Errorf("body = %q", msg.Attachments[0].Text)
	}
}

// Slack truncates a long attachment silently and mid-word. Cutting it here means the cut is
// visible, so nobody reads a half sentence as the whole message.
func TestRenderTruncatesLoudly(t *testing.T) {
	huge := strings.Repeat("x", 10_000)
	msg := notify.Render(notify.Parse("big", huge), "")
	body := msg.Attachments[0].Text
	if len(body) >= len(huge) {
		t.Error("body was not truncated")
	}
	if !strings.Contains(body, "truncated") {
		t.Error("truncation is silent; a message that just stops reads as a broken notifier")
	}
}

// A multi-byte body must not be cut mid-rune, which would emit invalid UTF-8 and render as a
// replacement character.
func TestRenderTruncatesOnRuneBoundaries(t *testing.T) {
	msg := notify.Render(notify.Parse("", strings.Repeat("é", 5000)), "")
	body := msg.Attachments[0].Text
	if !json.Valid(mustJSON(t, msg)) {
		t.Fatal("payload is not valid JSON")
	}
	for _, r := range body {
		if r == '�' {
			t.Fatal("truncation split a multi-byte rune")
		}
	}
}

// An alarm name with characters that need escaping must still produce a usable link.
func TestConsoleLinkEscapesTheAlarmName(t *testing.T) {
	body := strings.Replace(alarmJSON, `"AlarmName": "spotistats-CaptureStale"`,
		`"AlarmName": "spotistats Capture/Stale+1"`, 1)
	url := notify.Render(notify.Parse("", body), "").Attachments[0].TitleURL
	if strings.Contains(url, " ") {
		t.Errorf("unescaped space in %q", url)
	}
	// A literal "+" in a URL fragment is read back as a space by the console.
	if strings.Contains(url, "+") {
		t.Errorf("unescaped plus in %q", url)
	}
}

func TestRenderProducesValidJSON(t *testing.T) {
	if got := mustJSON(t, notify.Render(notify.Parse("", alarmJSON), "production")); !json.Valid(got) {
		t.Fatalf("invalid payload: %s", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An alarm with no ARN gets no console link. A URL built from "EU (Ireland)" cannot resolve, and
// a dead link reads as a deleted alarm.
func TestNoConsoleLinkWithoutARegionCode(t *testing.T) {
	body := strings.Replace(alarmJSON,
		`"AlarmArn": "arn:aws:cloudwatch:eu-west-1:401547103722:alarm:spotistats-CaptureStale",`, "", 1)
	att := notify.Render(notify.Parse("", body), "production").Attachments[0]

	if att.TitleURL != "" {
		t.Errorf("built a link with no region code: %q", att.TitleURL)
	}
	// The message is otherwise complete, and the display name still appears.
	if !strings.Contains(att.Footer, "EU (Ireland)") {
		t.Errorf("footer = %q, want the display-name region", att.Footer)
	}
	if att.Text == "" {
		t.Error("body is empty")
	}
}
