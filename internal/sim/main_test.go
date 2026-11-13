package sim

import (
	"os"
	"testing"
)

// TestMain runs the package without the variables of the pipeline agent that may have started
// `go test` (a build's gates): a goroutine of a scenario that outlives it must not see them either.
func TestMain(m *testing.M) {
	for _, k := range inheritedAgentEnv {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
