package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Cursors are opaque base64 tokens. There are two kinds, and the distinction matters enough
// to state, because docs/SPECS.md 6.2 originally specified only "base64 of the DynamoDB
// LastEvaluatedKey" and that is not sufficient for either endpoint that paginates.
//
//	sortKeyCursor carries the last sort key returned, so the next page resumes strictly after
//	              it. Used by /plays. This is O(n) over a full walk and exact even when two
//	              plays share a millisecond -- which a LastEvaluatedKey would also give, but a
//	              LastEvaluatedKey is scoped to a single partition query and /plays spans
//	              several UTC partitions, so it cannot be handed back to the client directly.
//	offsetCursor  carries a position in a COMPUTED ordering. Required by /list, which ranks by
//	              listening time: DynamoDB orders by key, the measure is an attribute, so the
//	              ranking is produced in the handler and no DynamoDB key means "resume from
//	              rank 50".
//
// Both are opaque to the client, so the distinction never reaches the contract. Both carry a
// fingerprint of the query they belong to: replaying a cursor against different parameters is
// a 400 rather than a silently wrong page.

type cursorKind string

const (
	cursorKindSortKey cursorKind = "s"
	cursorKindOffset  cursorKind = "o"
)

type cursorPayload struct {
	Kind cursorKind `json:"k"`
	// Fingerprint identifies the query this cursor was issued for.
	Fingerprint string `json:"f"`
	// SortKey is set for cursorKindSortKey.
	SortKey string `json:"sk,omitempty"`
	// Offset is set for cursorKindOffset.
	Offset int `json:"off,omitempty"`
}

// fingerprint hashes the parameters that define a result set. Changing any of them
// invalidates outstanding cursors.
func fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func encodeCursor(p cursorPayload) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("api: encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeCursor(raw, wantFingerprint string, want cursorKind) (cursorPayload, error) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cursorPayload{}, badRequest(CodeInvalidCursor, "cursor is not valid base64")
	}
	var p cursorPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return cursorPayload{}, badRequest(CodeInvalidCursor, "cursor is malformed")
	}
	if p.Kind != want {
		return cursorPayload{}, badRequest(CodeInvalidCursor, "cursor is not valid for this endpoint")
	}
	if p.Fingerprint != wantFingerprint {
		// Reusing a cursor against changed parameters would return a page from a different
		// result set, which looks like data corruption to the caller.
		return cursorPayload{}, badRequest(CodeInvalidCursor,
			"cursor belongs to a different query; restart pagination without a cursor")
	}
	return p, nil
}

// sortKeyCursor encodes the last sort key returned, for exact resume-after pagination.
func sortKeyCursor(fp, sk string) (string, error) {
	if sk == "" {
		return "", nil
	}
	return encodeCursor(cursorPayload{Kind: cursorKindSortKey, Fingerprint: fp, SortKey: sk})
}

// parseSortKeyCursor returns the sort key to resume after, or "" for a first page.
func parseSortKeyCursor(raw, fp string) (string, error) {
	if raw == "" {
		return "", nil
	}
	p, err := decodeCursor(raw, fp, cursorKindSortKey)
	if err != nil {
		return "", err
	}
	return p.SortKey, nil
}

func offsetCursor(fp string, offset int) (string, error) {
	return encodeCursor(cursorPayload{Kind: cursorKindOffset, Fingerprint: fp, Offset: offset})
}

func parseOffsetCursor(raw, fp string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	p, err := decodeCursor(raw, fp, cursorKindOffset)
	if err != nil {
		return 0, err
	}
	if p.Offset < 0 {
		return 0, badRequest(CodeInvalidCursor, "cursor offset is negative")
	}
	return p.Offset, nil
}
