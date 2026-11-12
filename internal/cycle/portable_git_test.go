package cycle

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

func TestStatusUsesCurrentBranchAndSkipsOriginComparisonWithoutBranch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(tmp, "umbrella")
	childSrc := filepath.Join(tmp, "child-src")
	for _, d := range []string{root, childSrc} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	runGit := func(dir string, args ...string) {
		t.Helper()
		if out, err := gitx.RunGit(ctx, dir, args...); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}

	runGit(childSrc, "init", "--quiet")
	runGit(childSrc, "checkout", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(childSrc, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(childSrc, "add", "app.go")
	runGit(childSrc, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "init child")

	runGit(root, "init", "--quiet")
	runGit(root, "checkout", "--quiet", "-b", "master")
	settingstest.WriteFiles(t, root, nil)
	runGit(root, "-c", "protocol.file.allow=always", "submodule", "add", "--quiet", childSrc, "repos/app")
	runGit(root, "add", "-A")
	runGit(root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "init umbrella")

	childDir := filepath.Join(root, "repos", "app")
	runGit(childDir, "checkout", "--quiet", "-b", "feat/child")

	// Install a fake git 2.20.1 wrapper that logs commands and rejects `branch --show-current`.
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitLog := filepath.Join(tmp, "git.log")
	wrapper := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + gitLog + "'\n" +
		"if [ \"$1\" = \"--version\" ]; then\n" +
		"  echo 'git version 2.20.1'\n" +
		"  exit 0\n" +
		"fi\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = \"--show-current\" ]; then\n" +
		"    echo \"error: unknown option \\`show-current'\" >&2\n" +
		"    exit 129\n" +
		"  fi\n" +
		"done\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	st, err := store.OpenAt(filepath.Join(tmp, "state.db"), "ws-status-portable", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var out bytes.Buffer
	if err := Status(ctx, root, st, &out); err != nil {
		t.Fatalf("Status: %v", err)
	}
	text := out.String()
	if strings.Contains(text, "show-current") || strings.Contains(text, "unknown option") {
		t.Fatalf("Status printed git error output:\n%s", text)
	}
	if !strings.Contains(text, "  root                   master ") {
		t.Fatalf("expected root row to show master, got:\n%s", text)
	}
	if !strings.Contains(text, "  repos/app              feat/child ") {
		t.Fatalf("expected repos/app row to show feat/child, got:\n%s", text)
	}

	// Detach both root and child so CurrentBranch yields no branch; verify the branch column is empty
	// and Status does not compare HEAD..origin/<branch>.
	runGit(root, "checkout", "--quiet", "--detach", "HEAD")
	runGit(childDir, "checkout", "--quiet", "--detach", "HEAD")
	_ = os.WriteFile(gitLog, nil, 0o644)
	out.Reset()
	if err := Status(ctx, root, st, &out); err != nil {
		t.Fatalf("Status on detached HEAD: %v", err)
	}
	detachedText := out.String()
	// 22 chars for "root" + 1 space + 24 spaces for empty branch + 1 space before short SHA
	if !strings.Contains(detachedText, "  root                                            ") {
		t.Fatalf("expected empty branch column for detached root, got:\n%s", detachedText)
	}
	if !strings.Contains(detachedText, "  repos/app                                       ") {
		t.Fatalf("expected empty branch column for detached repos/app, got:\n%s", detachedText)
	}
	loggedBytes, _ := os.ReadFile(gitLog)
	if strings.Contains(string(loggedBytes), "HEAD..origin/") {
		t.Fatalf("expected no HEAD..origin/<branch> comparison when there is no branch, git log:\n%s", string(loggedBytes))
	}
}

func TestRunCycleRefusesOldGitAtStart(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "umbrella")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	settingstest.WriteFiles(t, root, nil)
	settingstest.Give(t, settings.BaseBranch, "master")

	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeGit := filepath.Join(binDir, "git")
	script := "#!/bin/sh\necho 'git version 2.16.6'\nexit 0\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	st, err := store.OpenAt(filepath.Join(tmp, "state.db"), "ws-cycle-old-git", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var out bytes.Buffer
	err = RunCycle(ctx, root, st, CycleOptions{Stamp: "20261112T120000Z"}, &out)
	if err == nil {
		t.Fatal("expected RunCycle to fail when git is older than minimum")
	}
	for _, want := range []string{"2.16.6", gitx.MinVersion, fakeGit} {
		if !strings.Contains(err.Error(), want) || !strings.Contains(out.String(), want) {
			t.Fatalf("expected error (%v) and log (%q) to contain %q", err, out.String(), want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(root, ".worktrees")); !os.IsNotExist(statErr) {
		t.Fatalf("expected no .worktrees to be created, stat err=%v", statErr)
	}
}
