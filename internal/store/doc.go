// Package store is the DynamoDB persistence layer for Spotistats.
//
// It owns a single table whose access patterns are enumerated in docs/SPECS.md 5.1. The
// design choices worth knowing before changing anything here:
//
// Raw play events are the source of truth; aggregates are a write-time cache. A play is
// inserted with a conditional write and its aggregate deltas are applied ONLY if the
// insert actually created a row, which makes ingestion idempotent. If a process dies
// between those two steps the counters drift, and the nightly reconcile repairs them from
// the raw plays.
//
// TransactWriteItems was considered and rejected for the play-insert-plus-aggregates
// write. It would fit inside the 100-item transaction limit and would remove drift
// entirely, but it doubles the write-capacity cost on the hottest path in the system.
// docs/SPECS.md 3.1 and 4.3 deliberately chose cheap writes plus nightly reconciliation
// instead. Do not relitigate this without re-reading the cost model in 11.
//
// PLAY# partitions are keyed by UTC month while every aggregate period key is derived in
// the listener's local zone. That asymmetry is intentional -- see PlayPartition.
package store
