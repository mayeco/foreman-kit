package proc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noRegressionFixture is an umbrella whose child `backend` has one test gate and a regression whose
// regression.when matches the untracked file it leaves in the child, so a plain run adds the regression.
func noRegressionFixture(t *testing.T) (root string, runGates func(args ...string) (int, string, string)) {
	t.Helper()
	isolateGitConfig(t)
	t.Setenv("FOREMANKIT_SET", "")
	root, st := setupProcTestWorkspace(t)
	cfg := "base_branch: main\ngates:\n  test: \"echo gate_test_ran\"\nregression:\n  commands:\n    full: \"echo regression_full_ran\"\n  when:\n    - \"config/**\"\n"
	child := gatesRepo(t, root, "backend", cfg)
	if err := os.MkdirAll(filepath.Join(child, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "config", "app.rb"), []byte("X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, func(args ...string) (int, string, string) {
		return runGatesCLI(t, root, st, args...)
	}
}

// TestGatesNoRegressionSkipsTheRegressionWhenItMatches: --no-regression runs the gates and leaves out
// the regression that regression.when would add, saying so in one info line; without it, the same
// change runs the regression after the gates.
func TestGatesNoRegressionSkipsTheRegressionWhenItMatches(t *testing.T) {
	_, run := noRegressionFixture(t)

	rc, out, errOut := run("--repo", "backend", "--no-regression")
	if rc != 0 || !strings.Contains(out, "gate_test_ran") || !strings.Contains(out, "gates: PASS (1 executed; repos: backend)") {
		t.Fatalf("--no-regression: rc=%d, want 0 with only the test gate\nout:\n%s\nerr:\n%s", rc, out, errOut)
	}
	if strings.Contains(out, "regression_full_ran") || strings.Contains(out, "regression:full") {
		t.Fatalf("--no-regression ran the regression:\n%s", out)
	}
	want := "---- [info] backend: config/app.rb matches regression.when, but --no-regression skips the regression: the dispatcher runs it after the build"
	if got := strings.Count(errOut, want); got != 1 {
		t.Fatalf("want the skip line once, got %d:\n%s", got, errOut)
	}
	if strings.Contains(errOut, "the regression runs after the gates") {
		t.Fatalf("--no-regression still says the regression runs:\n%s", errOut)
	}

	rc, out, errOut = run("--repo", "backend")
	if rc != 0 || !strings.Contains(out, "regression_full_ran") || !strings.Contains(errOut, "config/app.rb matches regression.when: the regression runs after the gates") {
		t.Fatalf("without --no-regression the regression must run: rc=%d\nout:\n%s\nerr:\n%s", rc, out, errOut)
	}
}

// TestGatesRegressionAndNoRegressionAreAUsageError: the two flags contradict each other; the command
// exits 2 naming both, and nothing runs.
func TestGatesRegressionAndNoRegressionAreAUsageError(t *testing.T) {
	_, run := noRegressionFixture(t)
	for _, args := range [][]string{{"--regression", "--no-regression"}, {"--no-regression", "--all", "--regression"}} {
		rc, out, errOut := run(args...)
		if rc != 2 {
			t.Fatalf("%v: rc=%d, want 2\nout:\n%s\nerr:\n%s", args, rc, out, errOut)
		}
		if !strings.Contains(errOut, "--regression") || !strings.Contains(errOut, "--no-regression") {
			t.Fatalf("%v: stderr does not name both flags:\n%s", args, errOut)
		}
		if strings.Contains(out, "gate_test_ran") || strings.Contains(out, "regression_full_ran") || strings.Contains(out, "gates: ") {
			t.Fatalf("%v: something ran:\n%s", args, out)
		}
	}
}

// TestGatesHelpNamesNoRegression: the usage line documents the flag.
func TestGatesHelpNamesNoRegression(t *testing.T) {
	var o, e strings.Builder
	if rc := RunGatesCLI(t.Context(), t.TempDir(), nil, []string{"--help"}, &o, &e); rc != 0 || !strings.Contains(o.String(), "[--regression | --no-regression]") {
		t.Fatalf("rc=%d, help:\n%s", rc, o.String())
	}
}
