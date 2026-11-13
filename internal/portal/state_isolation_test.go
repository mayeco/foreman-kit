package portal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// isolatedTestHome holds the temporary FOREMANKIT_HOME directory created by TestMain.
var isolatedTestHome string

var isolatedWorkspaceEnvVars = []string{
	"FOREMANKIT_APP_ROOT",
	"FOREMANKIT_MAIN_ROOT",
	"FOREMANKIT_DB_PATH",
	"FOREMANKIT_STATE_DIR",
}

// TestMain keeps the runtime files of the workspaces these tests create (logs, scratch directories,
// PAUSE) out of the developer's own ~/.foreman-kit/state and clears any inherited workspace anchors
// so scratch applications never share a runtime directory with the outer umbrella or each other.
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
	isolatedTestHome = home
	return m.Run()
}

type isolatedRunnerFunc func() int

func (f isolatedRunnerFunc) Run() int { return f() }

func TestIsolatedStateIgnoresInheritedWorkspaceEnv(t *testing.T) {
	for _, key := range isolatedWorkspaceEnvVars {
		if got := os.Getenv(key); got != "" {
			t.Fatalf("%s = %q, want empty", key, got)
		}
	}
	if isolatedTestHome == "" {
		t.Fatal("isolatedTestHome was not recorded by TestMain")
	}
	if got := os.Getenv("FOREMANKIT_HOME"); got != isolatedTestHome {
		t.Fatalf("FOREMANKIT_HOME = %q, want %q", got, isolatedTestHome)
	}
	if info, err := os.Stat(isolatedTestHome); err != nil || !info.IsDir() {
		t.Fatalf("isolatedTestHome %q is not an existing directory: %v", isolatedTestHome, err)
	}

	rootA := t.TempDir()
	rootB := t.TempDir()
	idA, _ := store.WorkspaceID(rootA)
	idB, _ := store.WorkspaceID(rootB)
	if idA == idB {
		t.Fatalf("WorkspaceID(%q) == WorkspaceID(%q) == %q, want distinct workspace IDs", rootA, rootB, idA)
	}
	rdA := store.RuntimeDir(rootA)
	rdB := store.RuntimeDir(rootB)
	if rdA == rdB {
		t.Fatalf("RuntimeDir(%q) == RuntimeDir(%q) == %q, want distinct runtime directories", rootA, rootB, rdA)
	}
	wantPrefix := isolatedTestHome + string(filepath.Separator)
	if !strings.HasPrefix(rdA, wantPrefix) {
		t.Fatalf("RuntimeDir(%q) = %q, want prefix %q", rootA, rdA, wantPrefix)
	}
	if !strings.HasPrefix(rdB, wantPrefix) {
		t.Fatalf("RuntimeDir(%q) = %q, want prefix %q", rootB, rdB, wantPrefix)
	}

	origHome := isolatedTestHome
	callerHome := t.TempDir()
	for _, key := range isolatedWorkspaceEnvVars {
		t.Setenv(key, callerHome)
	}
	t.Setenv("FOREMANKIT_HOME", callerHome)
	t.Cleanup(func() {
		isolatedTestHome = origHome
	})

	var innerHome string
	code := runWithIsolatedState(isolatedRunnerFunc(func() int {
		for _, key := range isolatedWorkspaceEnvVars {
			if got := os.Getenv(key); got != "" {
				t.Errorf("inside runWithIsolatedState: %s = %q, want empty", key, got)
				return 1
			}
		}
		innerHome = os.Getenv("FOREMANKIT_HOME")
		if innerHome == "" || innerHome == callerHome {
			t.Errorf("inside runWithIsolatedState: FOREMANKIT_HOME = %q, want new temporary directory != %q", innerHome, callerHome)
			return 1
		}
		if _, err := os.Stat(innerHome); err != nil {
			t.Errorf("inside runWithIsolatedState: stat %q: %v", innerHome, err)
			return 1
		}
		return 0
	}))
	if code != 0 {
		t.Fatalf("runWithIsolatedState returned %d, want 0", code)
	}
	if _, err := os.Stat(innerHome); !os.IsNotExist(err) {
		t.Fatalf("temporary FOREMANKIT_HOME %q still exists after runWithIsolatedState finished (err=%v)", innerHome, err)
	}
}
