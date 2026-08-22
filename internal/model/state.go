package model

import "time"

// SchemaVersion is the version of the DynamoDB item layout this build writes. It is
// persisted in the config row so a binary cannot silently write a layout the table was
// not built for.
const SchemaVersion = 1

// PollCursor tracks how far the capture job has consumed the recently-played feed.
// LastPlayedAt is fed back as the endpoint's `after` cursor on the next run.
type PollCursor struct {
	LastPlayedAt time.Time
	LastRunAt    time.Time
	LastStatus   string
}

// GapMarker records a capture run whose response filled the requested limit, meaning
// listening may have exceeded the polling window and plays may have been lost
// irrecoverably (the API retains only about 50). Its presence is the signal to increase
// capture frequency.
type GapMarker struct {
	DetectedAt    time.Time
	WindowStart   time.Time
	WindowEnd     time.Time
	ItemsReturned int
	Limit         int
}

// IngestMarker claims a calendar month for a source. The GDPR export is authoritative
// for any month it covers, so the importer uses these markers to decide which api-sourced
// rows to supersede.
type IngestMarker struct {
	Month      Period
	Source     Source
	ImportedAt time.Time
	PlayCount  int64
}

// ConfigRow is written once and then verified on every startup. A changed timezone
// would silently re-file every subsequently derived period key against history derived
// under the old zone, so a mismatch must be a loud failure rather than a drift.
type ConfigRow struct {
	Timezone      string
	SchemaVersion int
	WrittenAt     time.Time
}
