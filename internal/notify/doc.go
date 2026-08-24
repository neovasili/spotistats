// Package notify turns the messages that land on the alarm SNS topic into Slack posts.
//
// # Why this exists at all
//
// Slack was asked for. The reason it is a good answer is a history worth recording accurately,
// because the tempting version of it is wrong.
//
// The alarm topic went a long time with NO subscriber at all: alarmEmail was unset, and both the
// email subscription and the budget skipped themselves in silence, so ten alarms existed, three
// were firing, and the console showed a monitored system that could notify nobody. That was a
// real defect, and it is the one this package is shaped by.
//
// It was NOT still broken when this replaced it. The subscription was created on 2026-08-23,
// confirmed, and delivered a real alarm email the following morning. Nobody reading this later
// should conclude that SNS email is unusable: it worked.
//
// What remains true is the shape of the risk. An SNS email subscription has a two-step
// activation and the intermediate state is invisible -- the console lists the subscription
// either way, and nothing distinguishes "will deliver" from "waiting on a click". A webhook has
// no such state. That is a smaller claim than "email does not work", and it is the honest one.
//
// # The parsing contract
//
// Two unrelated message shapes arrive on the same topic:
//
//   - CloudWatch alarm state changes, whose Message is a JSON document.
//   - AWS Budgets notifications, whose Message is plain English prose.
//
// So parsing is best-effort by design, and the fallback is to post the raw text rather than to
// drop it. A notifier that silently discards what it cannot parse recreates exactly the failure
// it was built to remove: the operator believes they are covered, and they are not. An ugly
// message that arrives beats a tidy one that does not.
package notify
