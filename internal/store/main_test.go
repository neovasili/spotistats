package store_test

import (
	"os"
	"testing"

	"github.com/neovasili/spotistats/internal/store/storetest"
)

// TestMain tears down the shared DynamoDB Local container after the package's tests.
//
// The harness disables testcontainers' Ryuk reaper, which cannot run under colima, so
// container lifecycle is explicit. See storetest.Shutdown.
func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Shutdown()
	os.Exit(code)
}
