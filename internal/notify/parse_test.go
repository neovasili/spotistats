package notify_test

import (
	"strings"
	"testing"

	"github.com/neovasili/spotistats/internal/notify"
)

// A real CloudWatch alarm state-change message, kept verbatim so a schema change shows up here
// rather than in production.
const alarmJSON = `{
  "AlarmName": "spotistats-CaptureStale",
  "AlarmDescription": "No capture run completed recently. The schedule may be broken.",
  "AWSAccountId": "401547103722",
  "AlarmConfigurationUpdatedTimestamp": "2026-08-01T10:00:00.000+0000",
  "NewStateValue": "ALARM",
  "NewStateReason": "Threshold Crossed: 1 datapoint [0.0] was less than the threshold [1.0].",
  "StateChangeTime": "2026-08-23T04:15:30.123+0000",
  "Region": "EU (Ireland)",
  "AlarmArn": "arn:aws:cloudwatch:eu-west-1:401547103722:alarm:spotistats-CaptureStale",
  "OldStateValue": "OK",
  "OKActions": [],
  "AlarmActions": ["arn:aws:sns:eu-west-1:401547103722:SpotistatsStack-Alarms"],
  "Trigger": {
    "MetricName": "CaptureRun",
    "Namespace": "Spotistats",
    "StatisticType": "Statistic",
    "Statistic": "SUM",
    "Period": 90,
    "EvaluationPeriods": 1,
    "ComparisonOperator": "LessThanThreshold",
    "Threshold": 1.0,
    "TreatMissingData": "breaching"
  }
}`

func TestParseAlarm(t *testing.T) {
	n := notify.Parse("ALARM: \"spotistats-CaptureStale\" in EU (Ireland)", alarmJSON)

	if n.Kind != "alarm" {
		t.Fatalf("kind = %q, want alarm", n.Kind)
	}
	if n.Title != "spotistats-CaptureStale" {
		t.Errorf("title = %q", n.Title)
	}
	if n.State != notify.StateAlarm {
		t.Errorf("state = %q", n.State)
	}
	if !strings.Contains(n.Description, "capture run") {
		t.Errorf("description = %q", n.Description)
	}
	if !strings.Contains(n.Reason, "Threshold Crossed") {
		t.Errorf("reason = %q", n.Reason)
	}
	if n.MetricName != "CaptureRun" || n.Namespace != "Spotistats" {
		t.Errorf("trigger = %q/%q", n.Namespace, n.MetricName)
	}
	// The Region field is a DISPLAY NAME, useless in a URL, and the code comes from the ARN.
	// Both are kept: one to show, one to link with.
	if n.Region != "EU (Ireland)" {
		t.Errorf("region = %q, want the display name preserved", n.Region)
	}
	if n.RegionCode != "eu-west-1" {
		t.Errorf("regionCode = %q, want the code from the ARN", n.RegionCode)
	}
	if n.At.IsZero() {
		t.Error("StateChangeTime did not parse; its +0000 offset is not RFC 3339")
	}
	if got := n.At.UTC().Format("2006-01-02T15:04:05"); got != "2026-08-23T04:15:30" {
		t.Errorf("at = %s", got)
	}
}

func TestParseRecovery(t *testing.T) {
	n := notify.Parse("OK: ...", strings.Replace(alarmJSON, `"NewStateValue": "ALARM"`, `"NewStateValue": "OK"`, 1))
	if n.State != notify.StateOK {
		t.Errorf("state = %q, want OK", n.State)
	}
}

// A budget notification is English prose, not JSON. It must survive as a raw message rather
// than being dropped -- the whole point of the fallback.
func TestParseBudget(t *testing.T) {
	const prose = "AWS Budget Notification August 23, 2026\n" +
		"AWS Account 401547103722\n\n" +
		"Dear AWS Customer,\n\nYou requested that we alert you when the ACTUAL Cost " +
		"associated with your spotistats-monthly budget exceeded 80% of your budget."

	n := notify.Parse("AWS Budgets: spotistats-monthly has exceeded 80%", prose)

	if n.Kind != "budget" {
		t.Errorf("kind = %q, want budget", n.Kind)
	}
	if n.State != notify.StateUnknown {
		t.Errorf("state = %q; a budget has no CloudWatch state", n.State)
	}
	if n.Raw != prose {
		t.Error("the prose body was not preserved verbatim")
	}
	if n.Title == "" {
		t.Error("no title; the SNS subject is the only one available on this path")
	}
}

// The case that decides whether this package is trustworthy: something arrives that matches
// neither shape. It must still be delivered.
func TestParseNeverDropsAnUnknownMessage(t *testing.T) {
	for _, tc := range []struct{ name, subject, message string }{
		{"empty", "", ""},
		{"not json", "Something happened", "totally unstructured text"},
		{"truncated json", "", `{"AlarmName": "x"`},
		{"json array", "", `[1,2,3]`},
		{"json but not an alarm", "", `{"hello":"world"}`},
		{"json null", "", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := notify.Parse(tc.subject, tc.message)
			if n.Raw != tc.message {
				t.Errorf("raw = %q, want the message verbatim", n.Raw)
			}
			if n.Title == "" {
				t.Error("title is empty; a message with no title renders as a blank post")
			}
		})
	}
}

// Valid JSON with no AlarmName must NOT be treated as an alarm: it would render as an alarm
// with every field blank, which reads as a broken notifier rather than as an odd message.
func TestParseRejectsJSONThatIsNotAnAlarm(t *testing.T) {
	n := notify.Parse("test", `{"hello":"world"}`)
	if n.Kind == "alarm" {
		t.Errorf("kind = alarm for a non-alarm JSON payload")
	}
}

func TestParseUnknownStateValue(t *testing.T) {
	n := notify.Parse("", strings.Replace(alarmJSON, `"NewStateValue": "ALARM"`, `"NewStateValue": "WAT"`, 1))
	// Still an alarm -- AlarmName is present -- but the state is not invented.
	if n.Kind != "alarm" {
		t.Errorf("kind = %q", n.Kind)
	}
	if n.State != notify.StateUnknown {
		t.Errorf("state = %q, want unknown for an unrecognised value", n.State)
	}
}

func TestParseMissingStateChangeTime(t *testing.T) {
	n := notify.Parse("", strings.Replace(alarmJSON, `"StateChangeTime": "2026-08-23T04:15:30.123+0000"`, `"StateChangeTime": ""`, 1))
	if !n.At.IsZero() {
		t.Error("a missing timestamp must stay zero rather than becoming the epoch or now")
	}
}

// Real alarm payloads DO omit AlarmArn -- the widely-copied reference capture has no such field.
// Without an ARN there is no region code, and the display name must not be pressed into service
// as one.
func TestParseAlarmWithoutAnARN(t *testing.T) {
	body := strings.Replace(alarmJSON,
		`"AlarmArn": "arn:aws:cloudwatch:eu-west-1:401547103722:alarm:spotistats-CaptureStale",`, "", 1)
	n := notify.Parse("", body)

	if n.Kind != "alarm" {
		t.Fatalf("kind = %q", n.Kind)
	}
	if n.RegionCode != "" {
		t.Errorf("regionCode = %q, want empty when there is no ARN to read it from", n.RegionCode)
	}
	// The display name still shows in the footer; it just cannot become a URL.
	if n.Region != "EU (Ireland)" {
		t.Errorf("region = %q", n.Region)
	}
}

// A composite alarm carries AlarmRule and TriggeringChildren instead of Trigger; a log alarm
// carries LogGroups and QueryString. Neither must break parsing -- they simply have no metric.
func TestParseAlarmVariantsWithoutATrigger(t *testing.T) {
	for name, body := range map[string]string{
		"composite": `{"AlarmName":"spotistats-Composite","NewStateValue":"ALARM",
			"NewStateReason":"child in ALARM","AlarmRule":"ALARM(a) OR ALARM(b)",
			"TriggeringChildren":[{"Arn":"arn:aws:cloudwatch:eu-west-1:1:alarm:a"}],
			"AlarmArn":"arn:aws:cloudwatch:eu-west-1:1:alarm:spotistats-Composite"}`,
		"log": `{"AlarmName":"spotistats-Log","NewStateValue":"ALARM","NewStateReason":"matched",
			"LogGroups":["/aws/lambda/x"],"QueryString":"fields @message",
			"AlarmArn":"arn:aws:cloudwatch:eu-west-1:1:alarm:spotistats-Log"}`,
	} {
		t.Run(name, func(t *testing.T) {
			n := notify.Parse("", body)
			if n.Kind != "alarm" {
				t.Errorf("kind = %q, want alarm", n.Kind)
			}
			if n.State != notify.StateAlarm {
				t.Errorf("state = %q", n.State)
			}
			// No metric, and that is fine -- it is simply not shown.
			if n.MetricName != "" {
				t.Errorf("metricName = %q, want empty", n.MetricName)
			}
			if n.RegionCode != "eu-west-1" {
				t.Errorf("regionCode = %q", n.RegionCode)
			}
		})
	}
}

// A malformed ARN must not yield a region code. This is the path that would otherwise produce a
// console link to nowhere, which reads as the alarm having been deleted.
func TestRegionCodeRejectsRubbish(t *testing.T) {
	for name, arn := range map[string]string{
		"display name in the slot": "arn:aws:cloudwatch:EU (Ireland):1:alarm:x",
		"truncated":                "arn:aws:cloudwatch",
		"empty":                    "",
		"not an arn":               "eu-west-1:whatever:x:y",
		"uppercase":                "arn:aws:cloudwatch:EU-WEST-1:1:alarm:x",
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(alarmJSON,
				"arn:aws:cloudwatch:eu-west-1:401547103722:alarm:spotistats-CaptureStale", arn, 1)
			if got := notify.Parse("", body).RegionCode; got != "" {
				t.Errorf("regionCode = %q, want empty", got)
			}
		})
	}
}
