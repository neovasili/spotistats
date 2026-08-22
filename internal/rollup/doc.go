// Package rollup implements the nightly job: reconciling drifted aggregates, materialising
// leaderboards and histograms, and rendering the static snapshots the dashboard reads.
//
// # Why reconciliation exists
//
// Raw play rows are the source of truth; aggregates are a write-time cache. A capture run
// writes a play with a conditional PutItem and then applies its aggregate deltas as a separate
// UpdateItem. If the process dies between those two steps the counters are left low. That is
// rare, but "rare and permanent" is not acceptable for the only copy of someone's listening
// history, so it is repaired nightly.
//
// # Why the window is not enough on its own
//
// docs/SPECS.md 4.3 says to "recompute aggregates from raw play rows for the trailing 45 days
// and compare to stored counters". That cannot work as stated: AGG#TRACK#ALL covers every play
// ever recorded, so no windowed read can recompute it, and comparing a window's worth of plays
// against an all-time counter would report enormous phantom drift and then "correct" the
// counter to the window's value -- destroying the history it was meant to protect.
//
// What this package does instead:
//
//  1. Recompute the FINEST granularity that the window fully determines -- the month rows for
//     every month the window touches, read in full, plus the TOTAL day rows.
//  2. For each row, compute correction = recomputed - stored.
//  3. Apply that correction to the corresponding year and all-time rows with an atomic ADD.
//
// Step 3 is what makes it correct: a delta is meaningful against a counter of any span, whereas
// an absolute value is not. The hierarchy is repaired without reading history, and the cost is
// bounded by the window rather than by the size of the dataset.
//
// Drift originating outside the window is not caught by this path; ReconcileAll exists for
// that and is a manual operation, because a full pass rewrites every aggregate row and costs
// real money on a large dataset.
package rollup
