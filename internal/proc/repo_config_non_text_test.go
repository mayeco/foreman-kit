package proc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunGatesFailsOnACommandThatIsNotText: a gate or a regression command written as a list
// (test: [go, test, ./...]) is not skipped, which would let the run pass without it: the run fails
// as a config failure naming the key, in the gates and in the regression sweep. A string gate passes.
func TestRunGatesFailsOnACommandThatIsNotText(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	dir := gatesRepo(t, root, "backend", "base_branch: main\ngates:\n  lint: \"echo lint_ran\"\n  test: [go, test, ./...]\n")

	res, out := runGatesOf(t, root, st, false)
	if res.ExitCode == 0 || len(res.Failed) != 1 || res.Failed[0] != "backend:config" {
		t.Fatalf("a list-valued gate did not fail as a config failure (exit %d, failed %v):\n%s", res.ExitCode, res.Failed, out)
	}
	if !strings.Contains(out, "gates.test: must be a command (text), not a list") || strings.Contains(out, "gates: PASS") {
		t.Fatalf("the failure does not name gates.test:\n%s", out)
	}
	if strings.Contains(out, "lint_ran\n") {
		t.Fatalf("a gate ran with a file that holds a command that is not text:\n%s", out)
	}

	if err := os.WriteFile(filepath.Join(dir, "repo.config.yaml"), []byte("base_branch: main\ngates:\n  test: \"echo test_ran\"\nregression:\n  commands:\n    full: [go, test, ./...]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, regression := range []bool{false, true} {
		res, out = runGatesOf(t, root, st, regression)
		if res.ExitCode == 0 || len(res.Failed) != 1 || res.Failed[0] != "backend:config" || !strings.Contains(out, "regression.commands.full: must be a command (text), not a list") {
			t.Fatalf("regression=%v: a list-valued regression command did not fail as a config failure (exit %d, failed %v):\n%s", regression, res.ExitCode, res.Failed, out)
		}
	}
	if list, err := RegressionSweepGates([]byte("regression:\n  commands:\n    full: [go, test]\n")); err == nil || list != nil {
		t.Fatalf("RegressionSweepGates accepted a list-valued command: %+v (%v)", list, err)
	}

	// A string gate still runs and passes.
	if err := os.WriteFile(filepath.Join(dir, "repo.config.yaml"), []byte("base_branch: main\ngates:\n  lint: \"echo lint_ran\"\n  test: \"echo test_ran\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, out = runGatesOf(t, root, st, false)
	if res.ExitCode != 0 || res.Passed != 2 || !strings.Contains(out, "test_ran\n") {
		t.Fatalf("string gates did not pass (exit %d, %d passed):\n%s", res.ExitCode, res.Passed, out)
	}
}
