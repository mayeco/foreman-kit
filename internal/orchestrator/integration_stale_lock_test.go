package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// integrationStaleLockPlant leaves an index.lock, last touched at when, in the git directory of the
// integration worktree (child == "") or of one of its child repositories, as a git that was killed
// mid-operation does (a crash, a power loss, the SIGKILL that follows an ignored SIGTERM).
func (sb *mcfSandbox) integrationStaleLockPlant(t *testing.T, child string, when time.Time) string {
	t.Helper()
	dir := filepath.Join(sb.umbrella, ".worktrees", "_integration", filepath.FromSlash(child))
	lock := filepath.Join(mcfGit(t, dir, "rev-parse", "--absolute-git-dir"), "index.lock")
	mustWriteFile(t, lock, "")
	if err := os.Chtimes(lock, when, when); err != nil {
		t.Fatal(err)
	}
	return lock
}

// TestMergerIntegratesDespiteStaleIndexLocksInItsWorktree: every command the merger runs in its
// integration worktree failed with "Unable to create index.lock: File exists" until a person deleted the
// file by hand, and each ready change was blocked as an orchestrator error (pausing the workspace). Only
// the merger, under its lock, runs git there: a lock no git has touched for a long time is a leftover.
func TestMergerIntegratesDespiteStaleIndexLocksInItsWorktree(t *testing.T) {
	sb := newMCFSandbox(t, "")
	ctx := context.Background()

	// A first integration creates the integration worktree and its child checkouts.
	sb.runCoreDispatch(t, "add-a", map[string]string{"a.txt": "a\n"})
	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-a")

	longAgo := time.Now().Add(-time.Hour)
	locks := []string{
		sb.integrationStaleLockPlant(t, "", longAgo),
		sb.integrationStaleLockPlant(t, "repos/core", longAgo),
	}

	sb.runCoreDispatch(t, "add-b", map[string]string{"b.txt": "b\n"})
	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-b")
	for _, lock := range locks {
		if _, err := os.Stat(lock); err == nil {
			t.Fatalf("the stale %s is still there", lock)
		}
	}
	if ws, err := sb.st.GetWorkspaceState(ctx); err != nil || ws.Paused {
		t.Fatalf("the workspace is paused (%+v, err %v)", ws, err)
	}
}

// TestMergerLeavesAFreshIndexLockAlone: a lock touched a moment ago may belong to a git that is still
// running (one a stopped merger left behind): it is never removed.
func TestMergerLeavesAFreshIndexLockAlone(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.runCoreDispatch(t, "add-a", map[string]string{"a.txt": "a\n"})
	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-a")

	lock := sb.integrationStaleLockPlant(t, "", time.Now())
	m := sb.newMerger(t, MergerOptions{})
	m.removeStaleIndexLocks(context.Background(), filepath.Join(sb.umbrella, ".worktrees", "_integration"))
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("a fresh index.lock was removed: %v", err)
	}
}
