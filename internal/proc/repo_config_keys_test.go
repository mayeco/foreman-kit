package proc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/store"
)

// gatesRepo makes repos/<name> of root a repository whose repo.config.yaml is cfg, listed in the
// .gitmodules of root.
func gatesRepo(t *testing.T, root, name, cfg string) string {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(root, "repos", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
	} {
		if _, err := gitx.RunGit(ctx, dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "repo.config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = gitx.RunGit(ctx, dir, "add", "-A")
	_, _ = gitx.RunGit(ctx, dir, "commit", "-m", "init "+name)
	modules := "[submodule \"" + name + "\"]\n\tpath = repos/" + name + "\n\turl = ./repos/" + name + "\n\tbranch = main\n"
	if err := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte(modules), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = gitx.RunGit(ctx, root, "add", ".gitmodules")
	_, _ = gitx.RunGit(ctx, root, "commit", "-m", "add "+name)
	return dir
}

func runGatesOf(t *testing.T, root string, st *store.Store, regression bool) (*GatesResult, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	res, err := RunGates(context.Background(), GatesOptions{Root: root, Repos: []string{"backend"}, Slot: 1, Regression: regression, Store: st, Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatalf("RunGates: %v\nout:\n%s\nerr:\n%s", err, out.String(), errOut.String())
	}
	return res, out.String() + errOut.String()
}

const formerRepoConfig = "base_branch: main\ngates:\n  setup: \"echo setup_ran\"\n  test: \"echo gate_test_ran\"\nregression:\n  full: \"echo regression_full_ran\"\nregression_when: []\n"

// TestRunGatesReadsNoKeyOfAnOlderKit: a repo.config.yaml written for an older foreman-kit is not
// read as one of today, where the same words say other things (gates.setup was a gate, an empty
// regression ran the gates). Its gates do not run, and the failure says what rewrites the file.
func TestRunGatesReadsNoKeyOfAnOlderKit(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	gatesRepo(t, root, "backend", formerRepoConfig)

	for _, regression := range []bool{false, true} {
		res, out := runGatesOf(t, root, st, regression)
		if res.ExitCode == 0 {
			t.Fatalf("regression=%v: the gates passed with a file of an older kit:\n%s", regression, out)
		}
		if !strings.Contains(out, "---- [FAIL] repos/backend: repo.config.yaml holds keys an older foreman-kit read (gates.setup, regression, regression_when): foreman-kit update rewrites it") {
			t.Fatalf("regression=%v: the failure does not say what rewrites the file:\n%s", regression, out)
		}
		for _, ran := range []string{"setup_ran", "gate_test_ran", "regression_full_ran"} {
			if strings.Contains(out, ran+"\n") {
				t.Fatalf("regression=%v: %s, a command of the former file, ran:\n%s", regression, ran, out)
			}
		}
	}
}

// TestRunGatesOfAWorktreeCutBeforeTheRename: the worktree of a change that was being built when
// foreman-kit update renamed the keys still holds the former file. Its gates run as the file of
// the main checkout says, where update rewrote it, so the change needs no rebuild.
func TestRunGatesOfAWorktreeCutBeforeTheRename(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	gatesRepo(t, root, "backend", "base_branch: main\nsetup: \"echo setup_of_main_ran\"\ngates:\n  test: \"echo gate_of_main_ran\"\n")

	worktree := filepath.Join(root, ".worktrees", "worker-add-search")
	child := filepath.Join(worktree, "repos", "backend")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "repo.config.yaml"), []byte(formerRepoConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	res, out := runGatesOf(t, worktree, st, false)
	if res.ExitCode != 0 || !strings.Contains(out, "setup_of_main_ran\n") || !strings.Contains(out, "gate_of_main_ran\n") {
		t.Fatalf("the worktree did not run the gates of the main checkout's file (exit %d):\n%s", res.ExitCode, out)
	}
	if strings.Contains(out, "gate_test_ran\n") || strings.Contains(out, "setup_ran\n") {
		t.Fatalf("a command of the former file ran:\n%s", out)
	}
}

// TestRegressionSweepRunsOnlyTheRegressionCommands: the sweep of the cycle (foreman-kit gates
// --regression) runs setup and then the commands under regression.commands, in file order. A
// repository that has none runs nothing, not even setup: its gates do not stand in for them, and
// the sweep says so.
func TestRegressionSweepRunsOnlyTheRegressionCommands(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	dir := gatesRepo(t, root, "backend", "base_branch: main\nsetup: \"echo setup_ran\"\ngates:\n  test: \"echo gate_test_ran\"\nregression:\n  commands:\n    slow: \"echo regression_slow_ran\"\n    full: \"echo regression_full_ran\"\n    later: \"{{REGRESSION_COMMAND}}\"\n")

	res, out := runGatesOf(t, root, st, true)
	if res.ExitCode != 0 || res.Passed != 3 {
		t.Fatalf("want setup and two regression commands to pass, got exit %d, %d passed:\n%s", res.ExitCode, res.Passed, out)
	}
	setup, slow, full := strings.Index(out, "setup_ran\n"), strings.Index(out, "regression_slow_ran\n"), strings.Index(out, "regression_full_ran\n")
	if setup < 0 || slow < setup || full < slow {
		t.Fatalf("want setup, then slow, then full:\n%s", out)
	}
	if strings.Contains(out, "gate_test_ran\n") {
		t.Fatalf("the sweep ran a gate:\n%s", out)
	}

	list, err := RegressionSweepGates([]byte("setup: bin/setup\ngates:\n  test: bin/test\nregression:\n  commands:\n    slow: bin/slow\n    full: bin/full\n"))
	if err != nil || len(list) != 3 || list[0].Name != "setup" || list[1] != (RepoGate{Name: "slow", Cmd: "bin/slow"}) || list[2] != (RepoGate{Name: "full", Cmd: "bin/full"}) {
		t.Fatalf("RegressionSweepGates = %+v (%v)", list, err)
	}

	// No regression command: nothing runs.
	for name, cfg := range map[string]string{
		"no regression section": "base_branch: main\nsetup: \"echo setup_ran\"\ngates:\n  test: \"echo gate_test_ran\"\n",
		"empty commands":        "base_branch: main\nsetup: \"echo setup_ran\"\ngates:\n  test: \"echo gate_test_ran\"\nregression:\n  commands: {}\n  when: [\"app/**\"]\n",
		"only a placeholder":    "base_branch: main\nsetup: \"echo setup_ran\"\ngates:\n  test: \"echo gate_test_ran\"\nregression:\n  commands:\n    test: \"{{REGRESSION_COMMAND}}\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, "repo.config.yaml"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out := runGatesOf(t, root, st, true)
		if res.ExitCode != 0 || res.Passed != 0 {
			t.Fatalf("%s: want nothing to run, got exit %d, %d passed:\n%s", name, res.ExitCode, res.Passed, out)
		}
		if strings.Contains(out, "setup_ran\n") || strings.Contains(out, "gate_test_ran\n") {
			t.Fatalf("%s: the sweep ran a command:\n%s", name, out)
		}
		if !strings.Contains(out, "---- [info] repos/backend: no regression command (regression.commands of its repo.config.yaml): the sweep runs nothing here") {
			t.Fatalf("%s: the sweep does not say that it ran nothing:\n%s", name, out)
		}
		if list, err := RegressionSweepGates([]byte(cfg)); err != nil || len(list) != 0 {
			t.Fatalf("%s: RegressionSweepGates = %+v (%v)", name, list, err)
		}
	}
}
