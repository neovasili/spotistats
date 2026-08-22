package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// Error codes. These are part of the API contract, so they are constants rather than
// formatted strings.
const (
	CodeInvalidPeriod    = "INVALID_PERIOD"
	CodeInvalidDimension = "INVALID_DIMENSION"
	CodeInvalidParameter = "INVALID_PARAMETER"
	CodeUnknownParameter = "UNKNOWN_PARAMETER"
	CodeMissingParameter = "MISSING_PARAMETER"
	CodeInvalidCursor    = "INVALID_CURSOR"
	CodeInvalidRange     = "INVALID_RANGE"
	CodeNotFound         = "NOT_FOUND"
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	CodeNotImplemented   = "NOT_IMPLEMENTED"
	CodeInternal         = "INTERNAL"
)

// apiError is a client-visible failure.
type apiError struct {
	status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func badRequest(code, format string, a ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, Code: code, Message: fmt.Sprintf(format, a...)}
}

func notFound(format string, a ...any) *apiError {
	return &apiError{status: http.StatusNotFound, Code: CodeNotFound, Message: fmt.Sprintf(format, a...)}
}

// errorEnvelope is the wire shape from docs/SPECS.md 6.2.
type errorEnvelope struct {
	Error *apiError `json:"error"`
}

// writeError renders err to the client.
//
// Only *apiError reaches the client verbatim. Anything else becomes a generic 500: an
// internal error message can leak a table name, a key layout or an AWS request ID, none of
// which a caller of a public endpoint needs.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	var out *apiError
	if !errors.As(err, &out) {
		log.ErrorContext(r.Context(), "api: unhandled error",
			"path", r.URL.Path, "query", r.URL.RawQuery, "err", err)
		out = &apiError{
			status:  http.StatusInternalServerError,
			Code:    CodeInternal,
			Message: "internal error",
		}
	}

	// Errors must never be cached: a 400 from a typo would otherwise be served from the edge
	// for an hour after the caller fixed it.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(out.status)

	if encErr := json.NewEncoder(w).Encode(errorEnvelope{Error: out}); encErr != nil {
		log.ErrorContext(r.Context(), "api: write error response", "err", encErr)
	}
}
