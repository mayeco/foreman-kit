package proc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// gatesSelectionFixture is an umbrella whose child `backend` has one test gate: it touches a marker
// file and exits with exitCode, so a test sees both whether the gate ran and what it said.
func gatesSelectionFixture(t *testing.T, exitCode int) (root string, st *store.Store, child, marker string) {
	t.Helper()
	isolateGitConfig(t)
	t.Setenv("FOREMANKIT_SET", "")
	root, st = setupProcTestWorkspace(t)
	marker = filepath.Join(t.TempDir(), "gate-ran")
	cfg := fmt.Sprintf("base_branch: main\ngates:\n  test: \"touch '%s'; exit %d\"\n", marker, exitCode)
	child = gatesRepo(t, root, "backend", cfg)
	return root, st, child, marker
}

func runGatesCLI(t *testing.T, root string, st *store.Store, args ...string) (rc int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	rc = RunGatesCLIWith(context.Background(), root, st, args, &o, &e, nil)
	return rc, o.String(), e.String()
}

func fileThere(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// `--repo` takes the name of a child and its path, the two forms the other commands take
// (`foreman-kit exec repos/<name>`): the path form used to look for repos/repos/<name>, find no
// repo.config.yaml, and pass with nothing executed.
func TestGatesRepoAcceptsNameAndPathForms(t *testing.T) {
	root, st, _, marker := gatesSelectionFixture(t, 1)
	for _, value := range []string{"backend", "repos/backend", "repos/backend/"} {
		_ = os.Remove(marker)
		rc, out, errOut := runGatesCLI(t, root, st, "--repo", value)
		if rc != 1 || !strings.Contains(out, "---- [FAIL] backend:test") || !strings.Contains(out, "gates: FAIL (backend:test)") {
			t.Fatalf("--repo %s: rc=%d, want 1 with the failing gate of backend\nout:\n%s\nerr:\n%s", value, rc, out, errOut)
		}
		if !fileThere(marker) {
			t.Fatalf("--repo %s: the gate of backend did not run", value)
		}
	}
}

func TestGatesRepoNameStillPasses(t *testing.T) {
	root, st, _, marker := gatesSelectionFixture(t, 0)
	rc, out, errOut := runGatesCLI(t, root, st, "--repo", "repos/backend")
	if rc != 0 || !strings.Contains(out, "gates: PASS (1 executed; repos: backend)") {
		t.Fatalf("rc=%d, want 0 with one gate executed\nout:\n%s\nerr:\n%s", rc, out, errOut)
	}
	if !fileThere(marker) {
		t.Fatal("the gate did not run")
	}
}

// An unknown repository is refused with exit 2 naming the value, and nothing runs.
func TestGatesRepoRefusesWhatNamesNoChild(t *testing.T) {
	root, st, _, marker := gatesSelectionFixture(t, 0)
	for _, value := range []string{"chidl", "repos/chidl", "repos/repos/backend", "./repos/backend", "../backend", "repos/backend/x", ""} {
		rc, out, errOut := runGatesCLI(t, root, st, "--repo", value)
		if rc != 2 {
			t.Fatalf("--repo %q: rc=%d, want 2\nout:\n%s\nerr:\n%s", value, rc, out, errOut)
		}
		if !strings.Contains(errOut, fmt.Sprintf("%q", value)) || !strings.Contains(errOut, "backend") {
			t.Fatalf("--repo %q: stderr does not name the value and the known repositories:\n%s", value, errOut)
		}
		if strings.Contains(out, "gates: PASS") || strings.Contains(out, "[gate]") || fileThere(marker) {
			t.Fatalf("--repo %q: a gate ran or the run passed\nout:\n%s", value, out)
		}
	}
}

// A base that is not a commit is refused with exit 2 naming the value, and nothing runs. A diff
// against it used to fail unseen, so a committed change read as no change and nothing was selected.
func TestGatesBaseRefusesWhatIsNotACommit(t *testing.T) {
	root, st, _, marker := gatesSelectionFixture(t, 1)
	for _, value := range []string{"origin/no-such-ref", "HEAD~1x", "-x", "HEAD:.gitmodules"} {
		rc, out, errOut := runGatesCLI(t, root, st, "--base", value, "--repo", "backend")
		if rc != 2 {
			t.Fatalf("--base %s: rc=%d, want 2\nout:\n%s\nerr:\n%s", value, rc, out, errOut)
		}
		if !strings.Contains(errOut, fmt.Sprintf("%q", value)) {
			t.Fatalf("--base %s: stderr does not name the value:\n%s", value, errOut)
		}
		if strings.Contains(out, "gates: PASS") || strings.Contains(out, "[gate]") || fileThere(marker) {
			t.Fatalf("--base %s: a gate ran or the run passed\nout:\n%s", value, out)
		}
		// Without --repo the same base selects from the diff, which is the path that passed with nothing.
		if rc, out, _ := runGatesCLI(t, root, st, "--base", value); rc != 2 || strings.Contains(out, "gates: PASS") {
			t.Fatalf("--base %s without --repo: rc=%d\n%s", value, rc, out)
		}
	}
}

// With a base that resolves, the child whose gitlink moved since it is selected and its failing gate
// fails the run; the same umbrella checked against HEAD has nothing to select.
func TestGatesBaseSelectsTheChildItMoved(t *testing.T) {
	root, st, child, marker := gatesSelectionFixture(t, 1)
	first := gitIn(t, child, "rev-parse", "HEAD")
	gitIn(t, root, "update-index", "--add", "--cacheinfo", "160000,"+first+",repos/backend")
	gitIn(t, root, "commit", "-q", "-m", "record backend")
	gitIn(t, child, "commit", "-q", "--allow-empty", "-m", "backend: second")
	gitIn(t, root, "update-index", "--add", "--cacheinfo", "160000,"+gitIn(t, child, "rev-parse", "HEAD")+",repos/backend")
	gitIn(t, root, "commit", "-q", "-m", "move backend")

	rc, out, _ := runGatesCLI(t, root, st)
	if rc != 0 || !strings.Contains(out, "gates: PASS (0 executed; repos: none)") || fileThere(marker) {
		t.Fatalf("against HEAD nothing is selected: rc=%d\n%s", rc, out)
	}
	rc, out, errOut := runGatesCLI(t, root, st, "--base", "HEAD~1")
	if rc != 1 || !strings.Contains(out, "---- [FAIL] backend:test") || !fileThere(marker) {
		t.Fatalf("--base HEAD~1: rc=%d, want 1 with the failing gate of backend\nout:\n%s\nerr:\n%s", rc, out, errOut)
	}
}

// A child whose diff against the commit the base records for it fails is reported and fails the run:
// its gates never run against an empty list of changed files.
func TestGatesChildDiffFailureIsNotNothingChanged(t *testing.T) {
	root, st, _, marker := gatesSelectionFixture(t, 0)
	gitIn(t, root, "update-index", "--add", "--cacheinfo", "160000,1111111111111111111111111111111111111111,repos/backend")
	gitIn(t, root, "commit", "-q", "-m", "record a commit the child does not have")

	rc, out, errOut := runGatesCLI(t, root, st, "--repo", "backend")
	if rc != 1 || !strings.Contains(out, "---- [FAIL] repos/backend: cannot diff repos/backend against 1111111111111111111111111111111111111111") || !strings.Contains(out, "gates: FAIL (backend:diff)") {
		t.Fatalf("rc=%d, want 1 with the diff failure named\nout:\n%s\nerr:\n%s", rc, out, errOut)
	}
	if fileThere(marker) {
		t.Fatal("the gates of backend ran with an empty list of changed files")
	}
}
