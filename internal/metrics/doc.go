// Package metrics emits CloudWatch metrics from Lambda using the Embedded Metric Format.
//
// EMF is a JSON structure written to stdout that CloudWatch Logs parses into metrics. It is
// used rather than PutMetricData because it needs no API call on the hot path, costs
// nothing beyond the log line, and cannot fail the invocation -- a metric is never worth
// failing a capture run over.
//
// Outside Lambda the emitter is a no-op, so the CLI shares the same code path without
// producing stray log noise.
package metrics
