package store

import (
	"errors"
	"fmt"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var (
	// ErrAlreadyExists means a conditional write found the item already present. For
	// plays this is the normal, expected outcome of re-processing an overlapping window.
	ErrAlreadyExists = errors.New("store: item already exists")

	// ErrNotFound means no item matched the key.
	ErrNotFound = errors.New("store: item not found")

	// ErrThrottled means DynamoDB rejected the request for capacity reasons.
	ErrThrottled = errors.New("store: request throttled")

	// ErrTooLarge means the item exceeded DynamoDB's 400 KB limit.
	ErrTooLarge = errors.New("store: item exceeds the size limit")

	// ErrConfigMismatch means the table was written by a process configured with a
	// different timezone or schema version. Continuing would derive period keys under one
	// calendar while history was derived under another.
	ErrConfigMismatch = errors.New("store: persisted configuration does not match this process")
)

// Error wraps a failure with the operation and key that produced it, so a log line
// identifies the item without the caller having to add context at every call site.
type Error struct {
	Op  string
	PK  string
	SK  string
	Err error
}

func (e *Error) Error() string {
	if e.SK == "" {
		return fmt.Sprintf("store: %s %s: %v", e.Op, e.PK, e.Err)
	}
	return fmt.Sprintf("store: %s %s/%s: %v", e.Op, e.PK, e.SK, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// classify converts an AWS SDK error into a package sentinel.
//
// This is the ONLY function in the repository that references an AWS SDK error type.
// Everything above it tests with errors.Is against the sentinels above, so the SDK stays
// an implementation detail.
func classify(op, pk, sk string, err error) error {
	if err == nil {
		return nil
	}

	// A conditional-check failure on attribute_not_exists(PK) means "no item with this
	// exact primary key". On a composite-key table that reads oddly -- the condition
	// names only PK -- but condition expressions are evaluated against the single item
	// addressed by the FULL key, so it is correct.
	var ccf *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		return &Error{Op: op, PK: pk, SK: sk, Err: ErrAlreadyExists}
	}

	var throughput *ddbtypes.ProvisionedThroughputExceededException
	if errors.As(err, &throughput) {
		return &Error{Op: op, PK: pk, SK: sk, Err: ErrThrottled}
	}
	var throttling *ddbtypes.RequestLimitExceeded
	if errors.As(err, &throttling) {
		return &Error{Op: op, PK: pk, SK: sk, Err: ErrThrottled}
	}
	var tooLarge *ddbtypes.ItemCollectionSizeLimitExceededException
	if errors.As(err, &tooLarge) {
		return &Error{Op: op, PK: pk, SK: sk, Err: ErrTooLarge}
	}

	return &Error{Op: op, PK: pk, SK: sk, Err: err}
}
