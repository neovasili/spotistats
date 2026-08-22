package model

// The IANA time zone database is embedded in the binary.
//
// Spotistats derives every calendar period key in the listener's local timezone
// (see Calendar), so time.LoadLocation must work at runtime. The Lambda runtime
// provided.al2023 ships no /usr/share/zoneinfo, so without this import
// LoadLocation("Europe/Madrid") fails in production with "unknown time zone".
//
// The idiomatic home for a side-effect import is the program entry point, but the
// failure mode argues against it here: macOS and Linux dev machines DO have system
// tzdata, so a binary missing this import works perfectly in tests and locally and
// breaks only once deployed. Putting it next to Calendar makes it impossible to
// forget when a new cmd/ binary is added. Cost is roughly 800 KB of binary size,
// which is irrelevant on Lambda.
import _ "time/tzdata"
