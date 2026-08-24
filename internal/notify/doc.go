// Package notify turns the messages that land on the alarm SNS topic into Slack posts.
//
// # Why this exists at all
//
// The topic previously had one email subscriber, and an SNS email subscription requires the
// recipient to click a confirmation link. Production shipped with that subscription in
// PendingConfirmation for weeks: ten alarms existed, three were firing, and every notification
// went nowhere. A Slack incoming webhook has no handshake — it either works on the first post
// or fails loudly — which removes the entire class of failure rather than fixing one instance
// of it.
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
