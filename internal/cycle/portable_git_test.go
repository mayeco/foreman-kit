package cycle

import (
	"bytes"
	"context"
	"io"
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

	var cliOut, cliErr bytes.Buffer
	if rc := RunCycleCLI(ctx, root, st, CycleOptions{Stamp: "20261112T120001Z"}, nil, &cliOut, &cliErr); rc == 0 {
		t.Fatalf("expected RunCycleCLI to return non-zero on git 2.16.6")
	} else {
		combined := cliOut.String() + cliErr.String()
		for _, want := range []string{"2.16.6", gitx.MinVersion, fakeGit} {
			if !strings.Contains(combined, want) {
				t.Fatalf("expected RunCycleCLI output %q to contain %q", combined, want)
			}
		}
	}
}

func TestStartDaemonRefusesOldAndUnreadableGitBeforeLockAndPortal(t *testing.T) {
	ctx := context.Background()
	origPath := os.Getenv("PATH")

	for _, tc := range []struct {
		name      string
		script    string
		wantParts []string
	}{
		{
			name:      "2.16.6 below minimum",
			script:    "#!/bin/sh\nprintf 'git version 2.16.6\\n'\nexit 0\n",
			wantParts: []string{"2.16.6", gitx.MinVersion},
		},
		{
			name:      "unreadable version",
			script:    "#!/bin/sh\nprintf 'not a version line\\n'\nexit 0\n",
			wantParts: []string{"cannot be read", gitx.MinVersion},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if err := os.WriteFile(fakeGit, []byte(tc.script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+origPath)

			st, err := store.OpenAt(filepath.Join(tmp, "state.db"), "ws-daemon-old-git", root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })

			for _, detach := range []bool{true, false} {
				called := false
				var out, errOut bytes.Buffer
				rc := StartDaemon(ctx, root, st, DaemonOptions{
					Detach: detach,
					Web:    true,
					WebSet: true,
					Started: func(string, string) {
						called = true
					},
					StartWebFunc: func(context.Context, string, *store.Store, string, int, bool) (string, func(context.Context) error, error) {
						called = true
						return "http://127.0.0.1:4567", func(context.Context) error { return nil }, nil
					},
					ServeMergerFunc: func(context.Context, string, io.Writer) error {
						called = true
						return nil
					},
					ServeDispatcherFunc: func(context.Context, string, io.Writer) error {
						called = true
						return nil
					},
				}, &out, &errOut)
				if rc == 0 {
					t.Fatalf("detach=%v: expected StartDaemon to exit non-zero, got 0 (stdout=%q stderr=%q)", detach, out.String(), errOut.String())
				}
				if called {
					t.Fatalf("detach=%v: expected StartDaemon to fail before starting portal or calling Started", detach)
				}
				if dl, _ := st.GetLock(ctx, "daemon"); dl != nil {
					t.Fatalf("detach=%v: expected no daemon lock to be acquired, got %+v", detach, dl)
				}
				for _, want := range append(tc.wantParts, fakeGit) {
					if !strings.Contains(errOut.String(), want) {
						t.Fatalf("detach=%v: expected stderr %q to contain %q", detach, errOut.String(), want)
					}
				}
			}
		})
	}
}
