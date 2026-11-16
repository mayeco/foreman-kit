package orchestrator

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// rebuildRequestKeptRun runs one one-shot dispatch with agent.
func rebuildRequestKeptRun(t *testing.T, root string, st *store.Store, agent AgentRunnerFunc) {
	t.Helper()
	disp, err := NewDispatcher(DispatcherOptions{Root: root, Store: st, MaxWorkers: 1, AgentRunner: agent, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer disp.Close()
	if err := disp.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// The merger sends a change back with a rebuild request that carries what the BUILD agent must do (here
// a Jev judgment: the rebase is clean, so the feedback is all the agent has). An attempt that ends
// before the change is rebuilt, here because every build engine is out, must leave the request pending:
// the next attempt is still a rebuild and still gives the agent the merger's feedback.
func TestRebuildRequestSurvivesAnAttemptThatEndsEarly(t *testing.T) {
	ctx := context.Background()
	root, _, _, st := setupTestWorkspaceWithSubmodule(t)
	const change = "rebuild-after-an-outage"
	buildReadyManifest(t, ctx, root, st, change)

	const restore = "Restore the retry loop that commit abc1234 added to client.go."
	if _, err := st.RequestRebuild(ctx, change, "feat/spec-"+change, WorkLossMarker+"\n\n"+restore); err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveReady(ctx, change); err != nil {
		t.Fatal(err)
	}

	stub := continuousStubAgent()
	rebuildRequestKeptRun(t, root, st, func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		if req.Role == "plan" {
			return stub(ctx, req)
		}
		return AgentRunResult{ExitCode: EnginesDownExitCode, Output: "agent_exec: no engine could work (each failed without changing anything); exit 75: try again later\n"}, nil
	})
	if inReadyQueue(ctx, st, change) {
		t.Fatal("the change was queued for the merger although no engine could rebuild it")
	}
	if q, _ := st.GetQuarantine(ctx, change); q == nil || q.Status != "rebuild_requested" || q.Rebuilds != 1 {
		t.Errorf("the rebuild request must stay pending after an attempt that could not rebuild the change, got %+v", q)
	}

	var mu sync.Mutex
	var buildPrompts []string
	rebuildRequestKeptRun(t, root, st, func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		if req.Role != "plan" {
			mu.Lock()
			buildPrompts = append(buildPrompts, req.Prompt)
			mu.Unlock()
		}
		return stub(ctx, req)
	})
	if !inReadyQueue(ctx, st, change) {
		t.Fatal("the change was not rebuilt once the engines were back")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(buildPrompts) == 0 || !strings.Contains(buildPrompts[0], restore) {
		t.Errorf("the BUILD agent of the next attempt was not given the merger's feedback; its prompts: %d", len(buildPrompts))
	}
	// Queued again: the request is consumed, and it counted as one rebuild, not two.
	if q, _ := st.GetQuarantine(ctx, change); q == nil || q.Status != "cleared" || q.Rebuilds != 1 {
		t.Errorf("want the request consumed once the rebuilt change is queued (status cleared, rebuilds 1), got %+v", q)
	}
}

// TestConflictRebuildAfterUnblockAndInterruptedRebuild verifies that:
//  1. When a child conflict blocks a change as rebase_conflict, the block details include the structured
//     summary ("repos/core (... behind, ... ahead; conflicts in ...)") and conflict markers.
//  2. When a human unblocks the change without --reset-retries, the next dispatch pass rebases the child
//     branch onto the remote child base (even when wasRebuildReq is false and the umbrella gitlink did not move)
//     and starts a fresh conflict_rebuilds budget.
//  3. If the rebuild pass resets the child branch onto the new base (saving <branch>-conflict-N) and stops
//     before buildPhase finishes, the next pass still reopens tasks and hands the kept-branch instructions
//     to the BUILD agent instead of running a no-op build.
func TestConflictRebuildAfterUnblockAndInterruptedRebuild(t *testing.T) {
	ctx := context.Background()
	root, childOrigin, _, st := setupTestWorkspaceWithSubmodule(t)

	const change = "admin-kpi-revenue-chart"
	buildReadyManifest(t, ctx, root, st, change)

	// 2. Push a conflicting commit on feature.txt directly to childOrigin's main (outside the umbrella gitlink).
	hotfixClone := t.TempDir() + "/hotfix"
	mcfGit(t, root, "clone", "-q", "-b", "main", childOrigin, hotfixClone)
	mcfGit(t, hotfixClone, "config", "user.email", "test@example.com")
	mcfGit(t, hotfixClone, "config", "user.name", "Test")
	mustWriteFile(t, hotfixClone+"/feature.txt", "upstream hotfix line 1\nupstream hotfix line 2\n")
	mcfGit(t, hotfixClone, "add", "feature.txt")
	mcfGit(t, hotfixClone, "commit", "-qm", "fix(backend): upstream hotfix on feature.txt")
	mcfGit(t, hotfixClone, "push", "-q", "origin", "HEAD:main")

	// 3. First merge attempt sends the change back for rebuild 1 with rich conflict details.
	m, err := NewMerger(MergerOptions{Root: root, Store: st, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	m.ProcessQueue(ctx)
	q, err := st.GetQuarantine(ctx, change)
	if err != nil || q == nil || q.Status != "rebuild_requested" || q.Rebuilds != 1 {
		t.Fatalf("after first merge conflict: quarantine = %+v, want rebuild_requested with 1 rebuild", q)
	}
	for _, want := range []string{
		"repos/backend (1 behind,",
		"conflicts in feature.txt)",
		"Conflicting commits on origin/main:",
		"Conflict markers:",
	} {
		if !strings.Contains(q.ConflictDetails, want) {
			t.Errorf("expected %q in conflict details, got:\n%s", want, q.ConflictDetails)
		}
	}

	// 4. Exhaust the rebuild and block the change as rebase_conflict.
	if err := st.ClearQuarantine(ctx, change, false); err != nil {
		t.Fatal(err)
	}
	if err := st.BlockChange(ctx, store.QuarantineRecord{
		ChangeName: change,
		Reason:     "rebase_conflict",
		Details:    q.ConflictDetails,
	}); err != nil {
		t.Fatal(err)
	}

	// 5. Operator clears the block without resetting retries (BlockCleared, Forget: false).
	if err := st.ResolveQuarantine(ctx, change, store.QuarantineResolution{
		Resolution: store.BlockCleared,
		By:         "cli",
		Forget:     false,
	}); err != nil {
		t.Fatal(err)
	}

	// 6. Dispatch pass 1 after unblock: refreshPhase rebases onto origin/master of repos/backend (even though
	// wasRebuildReq is false and the umbrella gitlink did not move!), creates feat/spec-<change>-conflict-1,
	// resets feat/spec-<change> onto the remote child base, and saves the kept-branch feedback in the pending
	// rebuild request; then buildPhase fails early with EnginesDownExitCode.
	stub := continuousStubAgent()
	rebuildRequestKeptRun(t, root, st, func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		if req.Role == "plan" {
			return stub(ctx, req)
		}
		return AgentRunResult{ExitCode: EnginesDownExitCode, Output: "all engines cooling down\n"}, nil
	})
	if inReadyQueue(ctx, st, change) {
		t.Fatal("change must not be ready when build engines are down")
	}
	qAfterStop, _ := st.GetQuarantine(ctx, change)
	if qAfterStop == nil || qAfterStop.Status != "rebuild_requested" || !strings.Contains(qAfterStop.ConflictDetails, "kept on branch feat/spec-"+change+"-conflict-") {
		t.Fatalf("expected kept-branch feedback persisted on pending rebuild request after stopped build, got %+v", qAfterStop)
	}

	// 7. Dispatch pass 2 after unblock: the child branch in the worktree is ALREADY at target (from pass 1),
	// so RebaseBranch is a no-op, yet the BUILD agent must STILL receive the kept-branch rebuild feedback!
	var buildPrompt string
	rebuildRequestKeptRun(t, root, st, func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		if req.Role != "plan" {
			buildPrompt = req.Prompt
		}
		return stub(ctx, req)
	})
	if !strings.Contains(buildPrompt, "kept on branch feat/spec-"+change+"-conflict-") {
		t.Fatalf("expected BUILD prompt on second pass to still contain kept-branch rebuild instructions, got:\n%s", buildPrompt)
	}
	if !inReadyQueue(ctx, st, change) {
		t.Fatal("expected change to be queued ready after rebuild")
	}

	// 8. Merger integrates the rebuilt change cleanly.
	m.ProcessQueue(ctx)
	if !integratedOnOrigin(root, change) {
		t.Fatalf("expected %s to be integrated on origin/master after rebuild", change)
	}
}

// TestDryRunAndManualRebaseHoldUnderContinuousDispatcher verifies that:
//  1. Once an operator unblocks a change while a continuous dispatcher holds the lock, DryRun immediately
//     reports READY instead of "starts in the next dispatch: this one already ended it (blocked)".
//  2. While a manual git rebase is in progress in the change's worktree, DryRun reports HELD with
//     "held: a rebase is in progress in its worktree (...)" and isEligible skips it without aborting the rebase.
func TestDryRunAndManualRebaseHoldUnderContinuousDispatcher(t *testing.T) {
	ctx := context.Background()
	root, _, _, st := setupTestWorkspaceWithSubmodule(t)
	const change = "admin-kpi-revenue-chart"
	writeBacklogJSON(t, root, map[string]any{"change_name": change, "description": "Add KPI chart", "priority": "high"})

	// Simulate a continuous dispatcher that recorded results[change] = "blocked" before the operator unblocked it.
	if err := st.BlockChange(ctx, store.QuarantineRecord{ChangeName: change, Reason: "rebase_conflict", Details: "conflict"}); err != nil {
		t.Fatal(err)
	}
	lockRec, err := st.AcquireLock(ctx, "dispatcher", store.LockOptions{
		PID: os.Getpid(),
		Metadata: map[string]any{
			"mode":    "continuous",
			"workers": 3,
			"results": map[string]string{change: "blocked"},
		},
	})
	if err != nil || lockRec == nil {
		t.Fatalf("AcquireLock: lockRec=%v err=%v", lockRec, err)
	}
	defer func() { _ = st.ReleaseLock(ctx, "dispatcher", os.Getpid()) }()

	// Unblock the change.
	if err := st.ResolveQuarantine(ctx, change, store.QuarantineResolution{Resolution: store.BlockUnblocked, By: "cli"}); err != nil {
		t.Fatal(err)
	}

	disp, err := NewDispatcher(DispatcherOptions{Root: root, Store: st, MaxWorkers: 3, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer disp.Close()

	rep, err := disp.DryRun(ctx)
	if err != nil || rep == nil || len(rep.Items) != 1 {
		t.Fatalf("DryRun: rep=%+v err=%v", rep, err)
	}
	if rep.Items[0].State != "ready" {
		t.Fatalf("after unblock under continuous dispatcher, DryRun state = %q (%s), want \"ready\"", rep.Items[0].State, rep.Items[0].Why)
	}

	// Now simulate an operator running an interactive/manual git rebase in the change's child worktree.
	wtChild := root + "/.worktrees/worker-" + change + "/repos/backend"
	mcfGit(t, root, "init", "-q", wtChild)
	wtChildGit := wtChild + "/.git/rebase-merge"
	mustWriteFile(t, wtChildGit+"/head-name", "refs/heads/feat/spec-"+change+"\n")

	rep, err = disp.DryRun(ctx)
	if err != nil || rep == nil || len(rep.Items) != 1 {
		t.Fatalf("DryRun during manual rebase: rep=%+v err=%v", rep, err)
	}
	if rep.Items[0].State != "held" || !strings.Contains(rep.Items[0].Why, "a rebase is in progress in its worktree (repos/backend)") {
		t.Fatalf("during manual rebase, DryRun = %+v, want held with rebase in progress", rep.Items[0])
	}
	if disp.isEligible(ctx, BacklogItem{ChangeName: change}) {
		t.Fatal("change must not be eligible while a manual rebase is in progress in its worktree")
	}
}
