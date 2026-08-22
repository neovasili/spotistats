package api_test

import (
	"os"
	"testing"

	"github.com/neovasili/spotistats/internal/store/storetest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Shutdown()
	os.Exit(code)
}
