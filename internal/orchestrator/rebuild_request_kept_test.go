package orchestrator

import (
	"context"
	"io"
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
