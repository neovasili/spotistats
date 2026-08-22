package metrics

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestEMFDocumentShape(t *testing.T) {
	var buf bytes.Buffer
	e := NewEMF(&buf)

	e.Put(PlaysIngested, UnitCount, 7)
	e.Put(PlaysGapDetected, UnitCount, 1)
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("EMF output is not valid JSON: %v\n%s", err, buf.String())
	}

	// The metric values live at the top level, alongside the _aws envelope.
	if got := doc[PlaysIngested]; got != float64(7) {
		t.Errorf("%s = %v, want 7", PlaysIngested, got)
	}
	if got := doc[PlaysGapDetected]; got != float64(1) {
		t.Errorf("%s = %v, want 1", PlaysGapDetected, got)
	}

	aws, ok := doc["_aws"].(map[string]any)
	if !ok {
		t.Fatalf("_aws envelope missing: %v", doc)
	}
	if _, ok := aws["Timestamp"]; !ok {
		t.Error("_aws.Timestamp missing")
	}
	cw, ok := aws["CloudWatchMetrics"].([]any)
	if !ok || len(cw) != 1 {
		t.Fatalf("CloudWatchMetrics = %v", aws["CloudWatchMetrics"])
	}
	block := cw[0].(map[string]any)
	if block["Namespace"] != Namespace {
		t.Errorf("Namespace = %v, want %q", block["Namespace"], Namespace)
	}
	defs, ok := block["Metrics"].([]any)
	if !ok || len(defs) != 2 {
		t.Fatalf("Metrics definitions = %v, want 2", block["Metrics"])
	}
	// Definition order must follow first Put, so the document is deterministic.
	first := defs[0].(map[string]any)
	if first["Name"] != PlaysIngested {
		t.Errorf("first definition = %v, want %q (insertion order)", first["Name"], PlaysIngested)
	}
	if first["Unit"] != UnitCount {
		t.Errorf("unit = %v", first["Unit"])
	}
}

func TestEMFFlushResets(t *testing.T) {
	var buf bytes.Buffer
	e := NewEMF(&buf)

	e.Put(CaptureRun, UnitCount, 1)
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	firstLen := buf.Len()

	// A second flush with nothing recorded must write nothing at all.
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != firstLen {
		t.Errorf("empty flush wrote %d extra bytes", buf.Len()-firstLen)
	}
}

func TestEMFRepeatedPutOverwrites(t *testing.T) {
	var buf bytes.Buffer
	e := NewEMF(&buf)
	e.Put(PlaysIngested, UnitCount, 1)
	e.Put(PlaysIngested, UnitCount, 5)
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc[PlaysIngested]; got != float64(5) {
		t.Errorf("%s = %v, want the latest value 5", PlaysIngested, got)
	}
	// And it must not be declared twice.
	aws := doc["_aws"].(map[string]any)
	defs := aws["CloudWatchMetrics"].([]any)[0].(map[string]any)["Metrics"].([]any)
	if len(defs) != 1 {
		t.Errorf("definitions = %d, want 1", len(defs))
	}
}

// Outside Lambda the emitter must be silent, so the CLI shares this code path without
// printing EMF documents into a developer's terminal.
func TestNewIsNoOpOutsideLambda(t *testing.T) {
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "")
	if _, ok := New().(discard); !ok {
		t.Error("New() outside Lambda should discard")
	}
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "spotistats-capture")
	if _, ok := New().(discard); ok {
		t.Error("New() inside Lambda should emit")
	}
}

func TestDiscardIsSafe(t *testing.T) {
	d := Discard()
	d.Put("x", UnitCount, 1)
	if err := d.Flush(); err != nil {
		t.Errorf("Flush = %v", err)
	}
}

func TestBool(t *testing.T) {
	if Bool(true) != 1 || Bool(false) != 0 {
		t.Error("Bool must map to 1/0 for alarm thresholds")
	}
}

func TestEMFWritesToStdoutByDefault(t *testing.T) {
	// EMF is parsed out of the Lambda log stream, which is stdout.
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "fn")
	e, ok := New().(*emf)
	if !ok {
		t.Fatalf("New() = %T, want *emf", New())
	}
	if e.w != os.Stdout {
		t.Error("EMF must write to stdout to reach CloudWatch Logs")
	}
}
