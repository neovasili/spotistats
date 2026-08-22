// Package ingest implements the capture pipeline: turning the Spotify recently-played feed
// into stored plays and aggregates.
//
// It is shared by the `spotistats poll` CLI command and the scheduled capture Lambda, so
// both take exactly the same path through the ordering rules below.
//
// # Ordering
//
// The order of operations is the correctness contract, and two parts of it are subtle.
//
// The cursor advances LAST, after every write has succeeded. Failing earlier means the next
// run re-reads the same window, which is harmless because a play insert is conditional and
// its aggregate deltas are gated on that insert actually creating a row. The failure mode
// is therefore always "redo work", never "lose a play" or "double-count".
//
// Artist genres are resolved BEFORE plays are recorded. docs/SPECS.md 4.1 originally
// listed enrichment after the write, which is wrong: genres live on the artist object, so a
// brand-new artist would have no row yet, its plays would contribute zero genre deltas, and
// nothing would ever notice. Resolving first closes that hole.
//
// Tracks and albums are written from the recently-played payload itself, which embeds full
// track objects and simplified album objects. Only artists need a separate API call, and
// only because genres exist nowhere else.
package ingest
