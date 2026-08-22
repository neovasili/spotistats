package model

import "errors"

var (
	// ErrUnboundedPeriod is returned by Calendar.Bounds for PeriodAll, which has no
	// start or end. Callers must handle all-time explicitly.
	ErrUnboundedPeriod = errors.New("model: period is unbounded")

	// ErrInvalidPlay is returned by Play.Validate and the play constructors.
	ErrInvalidPlay = errors.New("model: invalid play")

	// ErrInvalidAggKey is returned by AggKey.Validate and ParseAggKey.
	ErrInvalidAggKey = errors.New("model: invalid aggregate key")
)
