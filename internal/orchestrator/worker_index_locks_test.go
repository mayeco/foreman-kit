package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// leftWorker leaves the worker worktree of change as a build killed after its agent wrote handoff.md
// does: the worktree exists on feat/spec-<change>, openspec/changes/<change>/handoff.md is uncommitted,
// and the child repos/core is checked out (its git directory lives under the worker's administrative
// directory). It returns the worktree.
func (sb *mcfSandbox) leftWorker(t *testing.T, change string) string {
	t.Helper()
	wt := filepath.Join(sb.umbrella, ".worktrees", "worker-"+change)
	mcfGit(t, sb.umbrella, "worktree", "add", "-b", "feat/spec-"+change, wt, "master")
	mcfGit(t, wt, "submodule", "update", "--init", "--", "repos/core")
	mustWriteFile(t, filepath.Join(wt, "openspec", "changes", change, "handoff.md"), "# Handoff\n\nNO_ENTRY\n")
	return wt
}

// plantIndexLock leaves an index.lock in the git directory of dir, as a git killed mid-write does.
func plantIndexLock(t *testing.T, dir string) string {
	t.Helper()
	lock := filepath.Join(mcfGit(t, dir, "rev-parse", "--absolute-git-dir"), "index.lock")
	mustWriteFile(t, lock, "")
	return lock
}

func requireNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s is still there", path)
	}
}

func requireFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s is gone: %v", path, err)
	}
}

// requireResumed asserts that change was dispatched past the leftovers: it is not quarantined and the
// workspace is not paused.
func (sb *mcfSandbox) requireResumed(t *testing.T, change string) {
	t.Helper()
	ctx := context.Background()
	if q, err := sb.st.GetQuarantine(ctx, change); err != nil || q != nil {
		t.Fatalf("%s is blocked (%+v, err %v)", change, q, err)
	}
	if ws, err := sb.st.GetWorkspaceState(ctx); err != nil || ws.Paused {
		t.Fatalf("the workspace is paused (%+v, err %v)", ws, err)
	}
}

// TestResumedChangeSurvivesStaleIndexLockInWorker: a git killed by the dispatcher's SIGKILL left
// index.lock in the worker's git directory, and "git add -A -- openspec/changes/<change>" of the next
// run failed on it. The error was an orchestrator_error, which paused the workspace, and the change
// stayed blocked until a person deleted the file. The worker's lease is held and no git of the change
// is alive, so the dispatcher removes the lock whatever its age.
func TestResumedChangeSurvivesStaleIndexLockInWorker(t *testing.T) {
	sb := newMCFSandbox(t, "")
	wt := sb.leftWorker(t, "add-a")
	lock := plantIndexLock(t, wt)

	sb.runCoreDispatch(t, "add-a", map[string]string{"a.txt": "a\n"})

	requireNoFile(t, lock)
	sb.requireResumed(t, "add-a")
	if got := mcfGit(t, wt, "log", "-1", "--format=%s"); !strings.Contains(got, "add-a") {
		t.Fatalf("the change did not progress past its leftovers; last commit %q", got)
	}
	if got := mcfGit(t, wt, "ls-tree", "-r", "--name-only", "HEAD", "openspec/changes/add-a/handoff.md"); strings.TrimSpace(got) == "" {
		t.Fatal("the uncommitted handoff.md was not kept")
	}
	evs := sb.events(t, "change.index_lock_removed")
	if len(evs) != 1 || evs[0].Change != "add-a" || !strings.Contains(fmt.Sprint(evs[0].Payload["paths"]), "index.lock") {
		t.Fatalf("want one change.index_lock_removed event of add-a naming the lock, got %+v", evs)
	}
}

// TestResumedChangeRemovesIndexLockOfWorkerChild: the lock of a child repository's git directory is
// removed too, and so is the lock of a fresh file (no age is needed).
func TestResumedChangeRemovesIndexLockOfWorkerChild(t *testing.T) {
	sb := newMCFSandbox(t, "")
	wt := sb.leftWorker(t, "add-a")
	childLock := plantIndexLock(t, filepath.Join(wt, "repos", "core"))
	wtLock := plantIndexLock(t, wt)
	if filepath.Dir(childLock) == filepath.Dir(wtLock) {
		t.Fatalf("the child shares the worker's git directory: %s", childLock)
	}

	sb.runCoreDispatch(t, "add-a", map[string]string{"a.txt": "a\n"})

	requireNoFile(t, childLock)
	requireNoFile(t, wtLock)
	sb.requireResumed(t, "add-a")
}

// TestWorkerIndexLockRemovalLeavesOtherWorktreesAlone: only the git directories of the change being
// started are cleaned: another worker's worktree and the main checkout keep their locks.
func TestWorkerIndexLockRemovalLeavesOtherWorktreesAlone(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.leftWorker(t, "add-a")
	other := sb.leftWorker(t, "add-z")
	otherLock := plantIndexLock(t, other)
	otherChildLock := plantIndexLock(t, filepath.Join(other, "repos", "core"))
	mainLock := plantIndexLock(t, sb.umbrella)
	mainChildLock := plantIndexLock(t, filepath.Join(sb.umbrella, "repos", "core"))
	wtLock := plantIndexLock(t, filepath.Join(sb.umbrella, ".worktrees", "worker-add-a"))

	sb.runCoreDispatch(t, "add-a", map[string]string{"a.txt": "a\n"})

	requireNoFile(t, wtLock)
	requireFile(t, otherLock)
	requireFile(t, otherChildLock)
	requireFile(t, mainLock)
	requireFile(t, mainChildLock)
	sb.requireResumed(t, "add-a")
}

// TestWorkerIndexLockRemovalOfAChildWithItsOwnGitDirectory: a child checked out with a git directory
// of its own (outside the worker's administrative directory) loses its lock, unless that git
// directory is the main checkout's.
func TestWorkerIndexLockRemovalOfAChildWithItsOwnGitDirectory(t *testing.T) {
	sb := newMCFSandbox(t, "")
	wt := filepath.Join(sb.umbrella, ".worktrees", "worker-add-a")
	mcfGit(t, sb.umbrella, "worktree", "add", "-b", "feat/spec-add-a", wt, "master")
	child := filepath.Join(wt, "repos", "core")
	mcfGit(t, sb.tmp, "clone", "-q", "-b", "main", sb.childBare["core"], child)
	ownLock := plantIndexLock(t, child)
	if strings.HasPrefix(ownLock, mcfGit(t, wt, "rev-parse", "--absolute-git-dir")) {
		t.Fatalf("the child's git directory is inside the worker's: %s", ownLock)
	}
	mainGitDir := mcfGit(t, filepath.Join(sb.umbrella, "repos", "core"), "rev-parse", "--absolute-git-dir")

	disp, err := NewDispatcher(DispatcherOptions{Root: sb.umbrella, Store: sb.st, MaxWorkers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer disp.Close()
	ctx := context.Background()

	disp.removeWorkerIndexLocks(ctx, wt, "add-a", []string{"repos/core"})
	requireNoFile(t, ownLock)

	// A child whose gitfile points at the main checkout's git directory is never cleaned.
	mainLock := filepath.Join(mainGitDir, "index.lock")
	mustWriteFile(t, mainLock, "")
	if err := os.RemoveAll(child); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(child, ".git"), "gitdir: "+mainGitDir+"\n")
	disp.removeWorkerIndexLocks(ctx, wt, "add-a", []string{"repos/core"})
	requireFile(t, mainLock)
	if evs, _ := sb.st.QueryEvents(ctx, store.EventFilter{Event: "change.index_lock_removed"}); len(evs) != 1 {
		t.Fatalf("want exactly one change.index_lock_removed event, got %+v", evs)
	}
}
