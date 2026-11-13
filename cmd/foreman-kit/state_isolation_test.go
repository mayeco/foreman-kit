package main

import (
	"fmt"
	"os"
	"testing"
)

// isolatedWorkspaceEnvVars are the variables the pipeline gives an agent about the workspace it works
// in. A test that inherits them resolves its scratch application to that workspace instead (see
// store.AppRoot), so its dispatcher, merger or daemon would work on the real umbrella.
var isolatedWorkspaceEnvVars = []string{
	"FOREMANKIT_APP_ROOT",
	"FOREMANKIT_MAIN_ROOT",
	"FOREMANKIT_DB_PATH",
	"FOREMANKIT_STATE_DIR",
	"FOREMANKIT_WORKDIR",
	"FOREMANKIT_CHANGE",
	"FOREMANKIT_PHASE",
	"FOREMANKIT_ROLE",
	"FOREMANKIT_ENGINE",
	"FOREMANKIT_PROMPT_FILE",
}

// TestMain clears the workspace the tests were started from and keeps their runtime files out of the
// developer's own ~/.foreman-kit/state.
func TestMain(m *testing.M) {
	os.Exit(runWithIsolatedState(m))
}

func runWithIsolatedState(m interface{ Run() int }) int {
	for _, key := range isolatedWorkspaceEnvVars {
		if err := os.Unsetenv(key); err != nil {
			fmt.Fprintf(os.Stderr, "cannot clear %s for the tests: %v\n", key, err)
			return 1
		}
	}
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
	return m.Run()
}

func TestTestsDoNotInheritTheWorkspaceTheyWereStartedFrom(t *testing.T) {
	for _, key := range isolatedWorkspaceEnvVars {
		if got := os.Getenv(key); got != "" {
			t.Fatalf("%s = %q, want empty: a scratch application would resolve to that workspace", key, got)
		}
	}
	if os.Getenv("FOREMANKIT_HOME") == "" {
		t.Fatal("FOREMANKIT_HOME is not set to a temporary directory")
	}
}
