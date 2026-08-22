// Package model holds the shared domain vocabulary for Spotistats: timestamps,
// calendar periods, plays, catalog entities, and the aggregate math.
//
// It imports only the standard library. That constraint is enforced by depguard
// (see .golangci.yml) and it is deliberate: model is the join point of two unrelated
// wire formats -- the Spotify recently-played API and the GDPR "Extended Streaming
// History" export -- and it is shared by the capture Lambda, the query Lambda, the
// nightly reconciler and the local CLI. Keeping it dependency-free means none of
// those drag in a Spotify client or the AWS SDK just to do period arithmetic.
//
// Spotify wire DTOs deliberately do NOT live here. They stay unexported inside
// internal/spotify and are mapped into these types, because a model.Play must be
// constructible from an export record without fabricating a fake API response.
//
// See docs/SPECS.md sections 2.2, 5.1 and 5.2 for the design this implements.
package model
