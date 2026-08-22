// Package api implements the read-only query API described in docs/SPECS.md 6.
//
// It is a plain http.Handler. The query Lambda wraps it with an API Gateway adapter and
// `spotistats serve` binds it to a local port, so both run byte-identical handler code --
// which is what makes the offline frontend loop (§7.4) a faithful stand-in for production
// rather than an approximation.
//
// Two conventions are load-bearing:
//
// Validation is strict. An unrecognised query parameter is a 400, not something to ignore,
// because silently dropping `?perido=2025` would return all-time figures that look plausible
// and are wrong.
//
// Every response carrying a duration also carries the exact subtotal and the estimated
// ratio (§6.4). API-sourced plays have no real duration -- the endpoint does not expose one
// -- so their contribution is the full track length. Making the split part of the response
// shape means a client cannot present an estimate as a measurement by accident.
package api
