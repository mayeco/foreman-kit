package portal

import (
	"fmt"
	"os"
	"testing"
)

// TestMain keeps the runtime files of the workspaces these tests create (logs, scratch directories,
// PAUSE) out of the developer's own ~/.foreman-kit/state. store.RuntimeDir resolves that directory from
// the environment, whatever database a test opened, so every run of this package left a dozen
// directories there. A state directory the environment already names is respected.
func TestMain(m *testing.M) {
	os.Exit(runWithIsolatedState(m))
}

func runWithIsolatedState(m *testing.M) int {
	if os.Getenv("FOREMANKIT_STATE_DIR") == "" && os.Getenv("FOREMANKIT_HOME") == "" {
		home, err := os.MkdirTemp("", "foreman-test-home-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "cannot create a state directory for the tests:", err)
			return 1
		}
		defer os.RemoveAll(home)
		if err := os.Setenv("FOREMANKIT_HOME", home); err != nil {
			fmt.Fprintln(os.Stderr, "cannot point the tests at their state directory:", err)
			return 1
		}
	}
	return m.Run()
}
