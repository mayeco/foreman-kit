package orchestrator

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

func TestDispatcherAndMergerRefuseOldGitAtStart(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "umbrella")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	settingstest.WriteFiles(t, root, nil)
	settingstest.Give(t, settings.BaseBranch, "master")
	settingstest.Give(t, settings.EvaluatorEnabled, "false")
	writeBacklogJSON(t, root, map[string]any{
		"change_name": "queued-item",
		"priority":    "high",
		"description": "Should stay queued when git is too old",
	})

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

	st, err := store.OpenAt(filepath.Join(tmp, "state.db"), "ws-old-git", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var dispLog bytes.Buffer
	disp, err := NewDispatcher(DispatcherOptions{
		Root:  root,
		Store: st,
		Out:   &dispLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp.Run(ctx); err == nil {
		t.Fatal("expected Dispatcher.Run to fail on git 2.16.6")
	} else {
		for _, want := range []string{"2.16.6", gitx.MinVersion, fakeGit} {
			if !strings.Contains(err.Error(), want) || !strings.Contains(dispLog.String(), want) {
				t.Fatalf("expected Dispatcher.Run error (%v) and log (%q) to contain %q", err, dispLog.String(), want)
			}
		}
	}

	dispLog.Reset()
	if err := disp.Serve(ctx); err == nil {
		t.Fatal("expected Dispatcher.Serve to fail on git 2.16.6")
	} else {
		for _, want := range []string{"2.16.6", gitx.MinVersion, fakeGit} {
			if !strings.Contains(err.Error(), want) || !strings.Contains(dispLog.String(), want) {
				t.Fatalf("expected Dispatcher.Serve error (%v) and log (%q) to contain %q", err, dispLog.String(), want)
			}
		}
	}

	// Verify no worktree was created and the item remains queued (not blocked, not ready).
	if _, statErr := os.Stat(filepath.Join(root, ".worktrees")); !os.IsNotExist(statErr) {
		t.Fatalf("expected no .worktrees directory to be created, stat err=%v", statErr)
	}
	blocked, _ := st.ListQuarantine(ctx, "")
	if len(blocked) != 0 {
		t.Fatalf("expected item not to be quarantined, got %+v", blocked)
	}
	ready, _ := st.ListReadyQueue(ctx)
	if len(ready) != 0 {
		t.Fatalf("expected ready queue to be empty, got %+v", ready)
	}

	var mergerLog bytes.Buffer
	merger, err := NewMerger(MergerOptions{
		Root:  root,
		Store: st,
		Out:   &mergerLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := merger.Run(ctx); err == nil {
		t.Fatal("expected Merger.Run to fail on git 2.16.6")
	} else {
		for _, want := range []string{"2.16.6", gitx.MinVersion, fakeGit} {
			if !strings.Contains(err.Error(), want) || !strings.Contains(mergerLog.String(), want) {
				t.Fatalf("expected Merger.Run error (%v) and log (%q) to contain %q", err, mergerLog.String(), want)
			}
		}
	}

	mergerLog.Reset()
	if err := merger.Serve(ctx); err == nil {
		t.Fatal("expected Merger.Serve to fail on git 2.16.6")
	} else {
		for _, want := range []string{"2.16.6", gitx.MinVersion, fakeGit} {
			if !strings.Contains(err.Error(), want) || !strings.Contains(mergerLog.String(), want) {
				t.Fatalf("expected Merger.Serve error (%v) and log (%q) to contain %q", err, mergerLog.String(), want)
			}
		}
	}
}

func TestDispatcherChildPreparationRebuildAndGitlinkKeepingUsePortableGitCommands(t *testing.T) {
	sb := newMCFSandbox(t, "verify")
	ctx := context.Background()

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}

	// Install a fake git 2.20.1 wrapper that logs every invocation and fails immediately if any caller
	// runs `switch`, `restore`, `--show-current`, or `worktree repair`.
	binDir := filepath.Join(sb.tmp, "git220-bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitLog := filepath.Join(sb.tmp, "git220.log")
	wrapper := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + gitLog + "'\n" +
		"if [ \"$1\" = \"--version\" ]; then\n" +
		"  echo 'git version 2.20.1'\n" +
		"  exit 0\n" +
		"fi\n" +
		"prev=''\n" +
		"for arg in \"$@\"; do\n" +
		"  case \"$arg\" in\n" +
		"    switch|restore|--show-current)\n" +
		"      echo \"fatal: forbidden git <2.22 argument: $arg\" >&2\n" +
		"      exit 129\n" +
		"      ;;\n" +
		"    repair)\n" +
		"      if [ \"$prev\" = \"worktree\" ]; then\n" +
		"        echo \"fatal: worktree repair is not available on git 2.20\" >&2\n" +
		"        exit 129\n" +
		"      fi\n" +
		"      ;;\n" +
		"  esac\n" +
		"  prev=\"$arg\"\n" +
		"done\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// 1. Dispatch and integrate `add-a` touching repos/core (exercises child preparation:
	// `checkout --quiet --detach <target>` and `checkout --quiet -b <branch>`, plus `commitChildren`
	// and `verifyBuild` branch check via `gitx.CurrentBranch`).
	sb.runCoreDispatch(t, "add-a", map[string]string{"conflict.txt": "from-add-a\n"})
	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-a")
	mcfGit(t, sb.umbrella, "fetch", "-q", "origin", "master")
	integrated := mcfGit(t, sb.umbrella, "rev-parse", "origin/master:repos/core")

	// 2. Simulate a pointer rollback on repos/core, then dispatch `add-m` touching only repos/mobile.
	// This exercises `refreshPhase` moving repos/core to the pre-rollback commit and
	// `unmoveUntouchedChildren` putting repos/core back with `checkout --quiet -B <branch> <commit>`.
	mate := filepath.Join(sb.tmp, "mate-umbrella")
	mcfGit(t, sb.tmp, "clone", "-q", "-b", "master", sb.umbrellaBare, mate)
	mcfGit(t, mate, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "-q", "repos/core")
	first := mcfGit(t, filepath.Join(mate, "repos", "core"), "rev-list", "--max-parents=0", "HEAD")
	mcfGit(t, filepath.Join(mate, "repos", "core"), "checkout", "-q", first)
	mustWriteFile(t, filepath.Join(mate, "MEMORY.md"), "# Memory\n\nRollback note.\n")
	mcfGit(t, mate, "commit", "-qam", "docs: rollback note")
	mcfGit(t, mate, "push", "-q", "origin", "master")

	sb.runCoreDispatch(t, "add-m", map[string]string{"repos/mobile/m.txt": "m\n"})
	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-m")
	mcfGit(t, sb.umbrella, "fetch", "-q", "origin", "master")
	if got := mcfGit(t, sb.umbrella, "rev-parse", "origin/master:repos/core"); got != first {
		t.Fatalf("expected untouched repos/core to keep rolled-back gitlink %s, got %s", first, got)
	}

	// 3. Exercise `keepLeftovers` and `rebuildChild` (when a child branch conflicts with integrated work)
	// on git 2.20.1.
	disp, err := NewDispatcher(DispatcherOptions{
		Root:       sb.umbrella,
		Store:      sb.st,
		MaxWorkers: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	change := "rebuild-portable"
	branch := "feat/spec-" + change
	wt := filepath.Join(sb.umbrella, ".worktrees", "worker-"+change)
	if err := disp.provisionWorktree(ctx, wt, change, branch); err != nil {
		t.Fatalf("provisionWorktree: %v", err)
	}
	mustWriteFile(t, filepath.Join(wt, "openspec", "changes", change, "tasks.md"), "# Tasks\n\n- [x] 1.1 Update conflict.txt\n")
	if err := disp.commitUmbrella(ctx, wt, "docs(openspec): plan "+change, []string{"openspec/changes/" + change}); err != nil {
		t.Fatalf("commitUmbrella plan: %v", err)
	}

	// Prepare child branches on `first`, then create a conflicting commit on `branch` in repos/core
	// before rebasing onto `integrated`.
	childCore := filepath.Join(wt, "repos", "core")
	mcfGit(t, childCore, "checkout", "--quiet", "-B", branch, first)
	mustWriteFile(t, filepath.Join(childCore, "leftover.txt"), "leftover work\n")
	if err := disp.keepLeftovers(ctx, wt, change, branch); err != nil {
		t.Fatalf("keepLeftovers: %v", err)
	}
	if logOut := mcfGit(t, childCore, "log", "-1", "--format=%s"); !strings.Contains(logOut, "keep edits found uncommitted") {
		t.Fatalf("expected keepLeftovers to commit child work on git 2.20.1, got log %q", logOut)
	}

	mustWriteFile(t, filepath.Join(childCore, "conflict.txt"), "conflicting-worker-line\n")
	mcfGit(t, childCore, "add", "conflict.txt")
	mcfGit(t, childCore, "commit", "-qm", "feat: conflicting worker change")

	// Advance origin/master's repos/core gitlink back to `integrated` so refreshPhase rebases onto `integrated`
	// and hits a conflict, triggering `rebuildChild`.
	mcfGit(t, mate, "fetch", "-q", "origin", "master")
	mcfGit(t, mate, "reset", "-q", "--hard", "origin/master")
	mcfGit(t, filepath.Join(mate, "repos", "core"), "checkout", "-q", integrated)
	mcfGit(t, mate, "add", "repos/core")
	mcfGit(t, mate, "commit", "-qm", "chore: advance core pointer")
	mcfGit(t, mate, "push", "-q", "origin", "master")

	retried := map[string]any{}
	feedback, err := disp.refreshPhase(ctx, BacklogItem{ChangeName: change}, wt, branch, 1, false, &retried)
	if err != nil {
		t.Fatalf("refreshPhase rebuild: %v", err)
	}
	if !strings.Contains(feedback, branch+"-conflict-1") {
		t.Fatalf("expected rebuild feedback naming kept branch %s-conflict-1, got %q", branch, feedback)
	}
	if cur, ok := gitx.CurrentBranch(ctx, childCore); !ok || cur != branch {
		t.Fatalf("expected childCore to be on %s after rebuild, got (%q, %v)", branch, cur, ok)
	}

	// 4. Also exercise stale worktree detection on git 2.20.1: RepairWorktree is skipped without
	// running `git worktree repair`.
	staleWT := filepath.Join(sb.umbrella, ".worktrees", "worker-stale-wt")
	if err := os.MkdirAll(staleWT, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(staleWT, ".git"), "gitdir: /nonexistent/worktrees/worker-stale-wt\n")
	_ = disp.provisionWorktree(ctx, staleWT, "stale-wt", "feat/spec-stale-wt")

	// Verify all logged commands were portable and included the four checkout forms.
	loggedBytes, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatal(err)
	}
	logged := string(loggedBytes)
	for _, forbidden := range []string{"switch", "restore", "--show-current", "worktree repair"} {
		for _, line := range strings.Split(logged, "\n") {
			fields := strings.Fields(line)
			for _, f := range fields {
				if f == forbidden {
					t.Fatalf("forbidden git argument %q found in command %q", forbidden, line)
				}
			}
			if forbidden == "worktree repair" && strings.Contains(line, "worktree repair") {
				t.Fatalf("worktree repair should be skipped on git 2.20.1, found %q", line)
			}
		}
	}
	for _, wantForm := range []string{
		"checkout --quiet --detach ",
		"checkout --quiet -b ",
		"checkout --quiet -B ",
		"checkout --quiet feat/spec-",
		"symbolic-ref --quiet --short HEAD",
	} {
		if !strings.Contains(logged, wantForm) {
			t.Fatalf("expected git command log to contain %q, log:\n%s", wantForm, logged)
		}
	}
}
