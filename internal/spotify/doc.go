// Package spotify is a minimal client for the parts of the Spotify Web API that
// Spotistats actually uses: recently-played, top items, and the track/artist/album
// multi-get endpoints.
//
// Deliberate constraints, all enforced by depguard (.golangci.yml):
//
//   - No AWS imports. The long-lived refresh token is persisted through the
//     RefreshTokenStore interface; only the cmd/ layer knows that production backs it
//     with SSM Parameter Store.
//   - No internal/store import. The package DAG is model <- spotify and model <- store.
//   - No os.Getenv. Every knob is an explicit Config field, which is what makes the
//     whole package testable without a network, a clock, or a filesystem.
//
// Wire DTOs are unexported and never leave the package; they are mapped to
// internal/model types. That boundary exists because a model.Play must also be
// constructible from a GDPR export record, which is a completely different format.
//
// Endpoints this package does NOT and cannot provide: audio features, audio analysis,
// recommendations, related artists, featured playlists, category playlists and 30-second
// preview URLs. Spotify restricted all of them to apps registered before 2024-11-27.
// See docs/SPECS.md 2.3.
package spotify
