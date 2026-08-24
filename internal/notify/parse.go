package notify

import (
	"encoding/json"
	"strings"
	"time"
)

// State is an alarm's CloudWatch state, or Unknown for a message that is not an alarm.
type State string

const (
	StateAlarm            State = "ALARM"
	StateOK               State = "OK"
	StateInsufficientData State = "INSUFFICIENT_DATA"
	// StateUnknown covers anything that is not a CloudWatch alarm state change -- a budget
	// notification, or a hand-published test message.
	StateUnknown State = ""
)

// Notification is one message, normalised enough to render.
//
// Raw is always populated, even when nothing else could be parsed, because the fallback path
// posts it verbatim.
type Notification struct {
	// Kind distinguishes what was recognised: "alarm", "budget" or "raw".
	Kind string

	Title       string
	State       State
	Reason      string
	Description string

	// Region is CloudWatch's own field, which is a DISPLAY NAME ("EU (Ireland)", and in some
	// payloads "EU - Ireland") rather than an identifier. It is fine to show and useless in a
	// URL.
	Region string
	// RegionCode is the real region ("eu-west-1"), taken from the alarm ARN. It is empty when
	// the payload carries no ARN -- which real payloads do omit -- and the console link is
	// skipped in that case rather than built out of the display name.
	RegionCode string

	AccountID string
	At        time.Time

	// MetricName and Namespace come from the alarm's Trigger block, and are what make the
	// console deep link possible.
	MetricName string
	Namespace  string

	Raw string
}

// cloudWatchAlarm mirrors the fields of a CloudWatch alarm SNS message that are worth showing.
//
// It is deliberately a partial view. The real payload carries the full metric definition,
// dimensions, statistics and thresholds; decoding all of it would couple this package to a
// schema AWS extends without notice, for fields no reader of a Slack message wants.
type cloudWatchAlarm struct {
	AlarmName        string `json:"AlarmName"`
	AlarmDescription string `json:"AlarmDescription"`
	NewStateValue    string `json:"NewStateValue"`
	NewStateReason   string `json:"NewStateReason"`
	StateChangeTime  string `json:"StateChangeTime"`
	Region           string `json:"Region"`
	AWSAccountID     string `json:"AWSAccountId"`
	AlarmARN         string `json:"AlarmArn"`
	Trigger          struct {
		MetricName string `json:"MetricName"`
		Namespace  string `json:"Namespace"`
	} `json:"Trigger"`
}

// Parse normalises one SNS message. It never fails: the worst case is Kind "raw" carrying the
// message verbatim, which is still delivered.
//
// subject is the SNS Subject, which CloudWatch sets to a human summary and Budgets sets to the
// budget's alert name. It is the only title available on the raw path.
func Parse(subject, message string) Notification {
	n := Notification{Kind: "raw", Title: strings.TrimSpace(subject), Raw: message}

	var alarm cloudWatchAlarm
	// A budget notification is prose, so Unmarshal fails and we fall through. AlarmName is
	// checked as well because valid JSON that is not an alarm -- a hand-published test
	// message, say -- would otherwise render as an alarm with every field empty.
	if err := json.Unmarshal([]byte(message), &alarm); err == nil && alarm.AlarmName != "" {
		n.Kind = "alarm"
		n.Title = alarm.AlarmName
		n.State = normaliseState(alarm.NewStateValue)
		n.Reason = alarm.NewStateReason
		n.Description = alarm.AlarmDescription
		n.Region = alarm.Region
		n.AccountID = alarm.AWSAccountID
		n.MetricName = alarm.Trigger.MetricName
		n.Namespace = alarm.Trigger.Namespace
		n.At = parseTime(alarm.StateChangeTime)
		// Only the ARN carries the region CODE. Keep the display name for the footer either
		// way; RegionCode staying empty is what suppresses the console link.
		n.RegionCode = regionFromARN(alarm.AlarmARN)
		return n
	}

	// AWS Budgets prose. Recognised only to label it; the body is posted as-is, because
	// pattern-matching amounts out of an English sentence AWS is free to reword would
	// silently start reporting nothing.
	if looksLikeBudget(subject, message) {
		n.Kind = "budget"
		if n.Title == "" {
			n.Title = "AWS Budgets"
		}
	}
	if n.Title == "" {
		n.Title = "Notification"
	}
	return n
}

func normaliseState(v string) State {
	switch State(strings.ToUpper(strings.TrimSpace(v))) {
	case StateAlarm:
		return StateAlarm
	case StateOK:
		return StateOK
	case StateInsufficientData:
		return StateInsufficientData
	default:
		return StateUnknown
	}
}

// parseTime reads CloudWatch's StateChangeTime. Its format is ISO 8601 with microseconds and a
// literal "+0000" offset, which is not RFC 3339, so the layouts are tried in order. A zero time
// means "not stated" and the renderer omits it -- better than a wrong timestamp.
func parseTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999-0700",
		"2006-01-02T15:04:05.999Z0700",
		"2006-01-02T15:04:05.999999-0700",
		"2006-01-02T15:04:05-0700",
		// RFC 3339 with a colon in the offset, spelled out rather than using
		// time.RFC3339Nano: that constant is banned repo-wide because it strips trailing
		// zeros, which inverts the lexical ordering of the timestamps used as sort keys.
		// Parsing is not affected, but the ban is deliberately blunt so nobody has to
		// remember which uses are safe.
		"2006-01-02T15:04:05.999999999Z07:00",
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// regionFromARN extracts the region from an ARN: arn:aws:cloudwatch:eu-west-1:123:alarm:name.
//
// It validates the shape rather than just splitting, because a malformed ARN that yields
// "EU (Ireland)" or "" would go on to build a console URL that 404s -- and a link to a
// non-existent alarm reads as the alarm having been deleted, which is worse than no link.
func regionFromARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 4 || parts[0] != "arn" {
		return ""
	}
	code := parts[3]
	// Region codes are lowercase letters, digits and hyphens. Anything else is a display name
	// or a truncated ARN.
	if code == "" {
		return ""
	}
	for _, r := range code {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return ""
		}
	}
	return code
}

func looksLikeBudget(subject, message string) bool {
	hay := strings.ToLower(subject + " " + message)
	return strings.Contains(hay, "budget")
}
