package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// Namespace is the CloudWatch namespace every Spotistats metric lands in.
const Namespace = "Spotistats"

// Metric names. These are the names the CloudWatch alarms in infra/ reference, so the two
// must agree; they are constants for exactly that reason.
const (
	// PlaysGapDetected is 1 when a capture page came back full, meaning listening may have
	// outrun the polling interval and plays may be unrecoverable.
	PlaysGapDetected = "PlaysGapDetected"
	// TokenRefreshFailed is 1 when the refresh token was rejected, or when a rotated token
	// could not be persisted. Both need a human.
	TokenRefreshFailed = "TokenRefreshFailed"
	// GenresDegraded is 1 when plays were recorded without complete genre attribution.
	GenresDegraded = "GenresDegraded"
	// PlaysIngested counts newly inserted plays.
	PlaysIngested = "PlaysIngested"
	// PlaysDuplicate counts plays that were already stored.
	PlaysDuplicate = "PlaysDuplicate"
	// CaptureRun is 1 per completed run, so an alarm can detect the absence of runs.
	CaptureRun = "CaptureRun"
	// CaptureFailed is 1 when a run ended in error.
	CaptureFailed = "CaptureFailed"
	// AggregateDrift is the number of aggregate rows the reconciler had to correct. Non-zero
	// means a capture run died between writing a play and applying its aggregates; the system
	// self-heals, but a persistently non-zero value means something fails repeatedly.
	AggregateDrift = "AggregateDrift"
	// RollupRun is 1 per completed nightly run, so an alarm can detect the absence of runs.
	RollupRun = "RollupRun"
	// RollupFailed is 1 when a nightly run ended in error.
	RollupFailed = "RollupFailed"
)

// Unit values used by the metrics above.
const (
	UnitCount        = "Count"
	UnitMilliseconds = "Milliseconds"
)

// Emitter writes metrics.
type Emitter interface {
	// Put records a single metric value.
	Put(name, unit string, value float64)
	// Flush writes everything recorded so far as one EMF document.
	Flush() error
}

// New returns an EMF emitter when running in Lambda, and a no-op otherwise.
//
// AWS_LAMBDA_FUNCTION_NAME is the documented signal for "this process is a Lambda
// invocation"; the CLI shares this code path and must not emit EMF into a developer's
// terminal.
func New() Emitter {
	if os.Getenv("AWS_LAMBDA_FUNCTION_NAME") == "" {
		return Discard()
	}
	return NewEMF(os.Stdout)
}

// Discard returns an emitter that records nothing.
func Discard() Emitter { return discard{} }

type discard struct{}

func (discard) Put(string, string, float64) {}
func (discard) Flush() error                { return nil }

// NewEMF returns an emitter writing Embedded Metric Format documents to w.
func NewEMF(w io.Writer) Emitter {
	return &emf{w: w, values: map[string]float64{}}
}

type emf struct {
	w          io.Writer
	definition []metricDefinition
	values     map[string]float64
}

type metricDefinition struct {
	Name string `json:"Name"`
	Unit string `json:"Unit"`
}

func (e *emf) Put(name, unit string, value float64) {
	if _, seen := e.values[name]; !seen {
		e.definition = append(e.definition, metricDefinition{Name: name, Unit: unit})
	}
	e.values[name] = value
}

// Flush writes one EMF document containing every recorded metric.
//
// Errors are returned but callers should log rather than fail on them: losing a metric is
// never worth failing the work the metric describes.
func (e *emf) Flush() error {
	if len(e.definition) == 0 {
		return nil
	}
	doc := map[string]any{
		"_aws": map[string]any{
			"Timestamp": time.Now().UnixMilli(),
			"CloudWatchMetrics": []map[string]any{{
				"Namespace": Namespace,
				// No dimensions: this is a single-user, single-environment system, so every
				// dimension would have exactly one value and would only make the alarms
				// harder to write.
				"Dimensions": [][]string{{}},
				"Metrics":    e.definition,
			}},
		},
	}
	for name, v := range e.values {
		doc[name] = v
	}

	b, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("metrics: marshal EMF document: %w", err)
	}
	if _, err := fmt.Fprintf(e.w, "%s\n", b); err != nil {
		return fmt.Errorf("metrics: write EMF document: %w", err)
	}

	e.definition = nil
	e.values = map[string]float64{}
	return nil
}

// Bool converts a flag to the 1/0 a CloudWatch alarm can threshold on.
func Bool(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
