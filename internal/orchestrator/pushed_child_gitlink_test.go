package orchestrator

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/store"
)

// pushedChildRecordCoreOnMaster moves the umbrella's master the way another pipeline's integration
// does: core's gitlink set to core's current main, in a commit pushed from a separate clone.
func (sb *mcfSandbox) pushedChildRecordCoreOnMaster(t *testing.T, change string) {
	t.Helper()
	mate := filepath.Join(sb.tmp, "pushed-child-mate")
	mcfGit(t, sb.tmp, "clone", "--quiet", "-b", "master", sb.umbrellaBare, mate)
	mcfGit(t, mate, "submodule", "update", "--init", "--quiet", "repos/core")
	mcfGit(t, filepath.Join(mate, "repos", "core"), "fetch", "--quiet", "origin", "main")
	mcfGit(t, filepath.Join(mate, "repos", "core"), "checkout", "--quiet", "origin/main")
	mcfGit(t, mate, "add", "repos/core")
	mcfGit(t, mate, "commit", "--quiet", "-m", "chore(merge): integrate "+change+"\n\nOpenSpec-Change: "+change)
	mcfGit(t, mate, "push", "--quiet", "origin", "master")
}

// TestMergerRetryAfterAChildPushNeverMovesTheGitlinkBack: a merger that stops between its two pushes
// (a restart, a lost lock, a hold on another repository's base) leaves the change in the merge queue with
// the child commit it pushed. When it integrates the change later, the umbrella may already record
// more of that child: another pipeline rebased its own change onto the pushed commit and integrated
// it. The retry used to check the child out at the commit it had pushed and record that, moving the
// gitlink back and dropping the other change from the umbrella, without running any gate.
func TestMergerRetryAfterAChildPushNeverMovesTheGitlinkBack(t *testing.T) {
	sb := newMCFSandbox(t, "")
	ctx := context.Background()
	sb.runCoreDispatch(t, "add-a", map[string]string{"a.txt": "a\n"})

	// What the stopped merger left: add-a's core commit on core's main and in the manifest, the
	// umbrella not pushed. Core's main had not moved, so the rebased commit is the worker's own.
	ready, err := sb.st.ListReadyQueue(ctx)
	if err != nil || len(ready) != 1 || ready[0].ChangeName != "add-a" {
		t.Fatalf("the merge queue = %+v (err %v), want add-a alone", ready, err)
	}
	workerCore := filepath.Join(ready[0].Worktree, "repos", "core")
	pushed := mcfGit(t, workerCore, "rev-parse", "HEAD")
	mcfGit(t, workerCore, "push", "--quiet", sb.childBare["core"], "HEAD:refs/heads/main")
	// The push belongs to the attempt of the merger that stopped, which it left open.
	stopped, err := sb.st.BeginMergeAttempt(ctx, "add-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.st.RecordChildPushed(store.WithMergeAttempt(ctx, stopped, "add-a"), "add-a", "repos/core", pushed); err != nil {
		t.Fatal(err)
	}

	// Meanwhile another pipeline integrates its own core change, built on the pushed commit.
	later := sb.pushCoreCommit(t, "b.txt", "b\n", "feat(core): another pipeline's change")
	sb.pushedChildRecordCoreOnMaster(t, "add-b")

	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-a")
	mcfGit(t, sb.umbrella, "fetch", "--quiet", "origin", "master")
	if got := mcfGit(t, sb.umbrella, "rev-parse", "origin/master:repos/core"); got != later {
		t.Fatalf("the merge of add-a recorded core at %s, the commit it had pushed (%s): the gitlink moved back from %s, dropping add-b", got, pushed, later)
	}
	core := filepath.Join(sb.umbrella, "repos", "core")
	mcfGit(t, core, "fetch", "--quiet", "origin")
	if rb := gitx.FindPointerRollback(ctx, sb.umbrella, "origin/master", "repos/core", 200); rb != nil {
		t.Fatalf("the merge of add-a rolled core's gitlink back: %+v", rb)
	}
}

// TestMergerRetryAfterAChildPushRecordsThePushedCommit: the usual retry, with the umbrella still at the
// gitlink from before the push, records the pushed commit forward without running the gates again.
func TestMergerRetryAfterAChildPushRecordsThePushedCommit(t *testing.T) {
	sb := newMCFSandbox(t, "")
	ctx := context.Background()
	sb.runCoreDispatch(t, "add-a", map[string]string{"a.txt": "a\n"})

	ready, err := sb.st.ListReadyQueue(ctx)
	if err != nil || len(ready) != 1 {
		t.Fatalf("the merge queue = %+v (err %v), want add-a alone", ready, err)
	}
	workerCore := filepath.Join(ready[0].Worktree, "repos", "core")
	pushed := mcfGit(t, workerCore, "rev-parse", "HEAD")
	mcfGit(t, workerCore, "push", "--quiet", sb.childBare["core"], "HEAD:refs/heads/main")
	// The push belongs to the attempt of the merger that stopped, which it left open.
	stopped, err := sb.st.BeginMergeAttempt(ctx, "add-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.st.RecordChildPushed(store.WithMergeAttempt(ctx, stopped, "add-a"), "add-a", "repos/core", pushed); err != nil {
		t.Fatal(err)
	}

	gateCalls := 0
	runMergerOnce(t, sb.newMerger(t, MergerOptions{
		GateRunner: func(ctx context.Context, req GateRunRequest) (GateRunResult, error) {
			gateCalls++
			return DefaultGateRunner(sb.st)(ctx, req)
		},
	}))
	sb.requireIntegrated(t, "add-a")
	if gateCalls != 0 {
		t.Fatalf("the gates ran %d time(s) for a child that was already pushed", gateCalls)
	}
	if got := mcfGit(t, sb.umbrella, "rev-parse", "origin/master:repos/core"); got != pushed {
		t.Fatalf("the merge of add-a recorded core at %s, want the pushed commit %s", got, pushed)
	}
}
