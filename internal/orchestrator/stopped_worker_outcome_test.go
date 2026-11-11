package orchestrator

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// stoppedWorkerServe runs a continuous dispatcher over one backlog item until stopWhen is closed, then
// cancels it (a daemon stop) and returns once Serve has returned.
func stoppedWorkerServe(t *testing.T, change string, agent AgentRunnerFunc, gate GateRunnerFunc, stopWhen <-chan struct{}) (root string, st *store.Store, log *syncBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root, _, _, st = setupTestWorkspaceWithSubmodule(t)
	writeBacklogJSON(t, root, map[string]any{"change_name": change, "description": "Stopped while it was being repaired", "priority": "high"})

	log = &syncBuffer{}
	disp, err := NewDispatcher(DispatcherOptions{Root: root, Store: st, MaxWorkers: 1, Hub: NewHub(), AgentRunner: agent, GateRunner: gate, Out: log})
	if err != nil {
		t.Fatal(err)
	}
	defer disp.Close()
	serveDone := make(chan error, 1)
	go func() { serveDone <- disp.Serve(ctx) }()

	select {
	case <-stopWhen:
	case <-time.After(60 * time.Second):
		t.Fatalf("the change never reached its repair round; log:\n%s", log.String())
	}
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned %v on cancellation", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("Serve did not return after cancellation; log:\n%s", log.String())
	}
	return root, st, log
}

// stoppedWorkerAssertNothingRecorded checks that the worker a daemon stop interrupted left no trace that
// reads as a verdict on the change.
func stoppedWorkerAssertNothingRecorded(t *testing.T, root string, st *store.Store, log *syncBuffer, change string) {
	t.Helper()
	ctx := context.Background()
	rounds, err := st.ListRepairRounds(ctx, change)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 0 {
		t.Errorf("the stopped repair round was recorded in repair_rounds as if verification had judged it: %+v", rounds)
	}
	repairs, _ := os.ReadFile(filepath.Join(root, ".worktrees", "worker-"+change, "openspec", "changes", change, "repairs.md"))
	if strings.Contains(string(repairs), "## repair-") {
		t.Errorf("the stopped repair round was written to repairs.md:\n%s", repairs)
	}
	if q, _ := st.GetQuarantine(ctx, change); q != nil && q.Status == "blocked" {
		t.Errorf("the stopped change was quarantined: %+v", q)
	}
	evs, err := st.QueryEvents(ctx, store.EventFilter{Event: "change.finished", ChangeName: change})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Payload["result"] != "stopped" {
			t.Errorf("the stopped change finished with result %v, want \"stopped\"", ev.Payload["result"])
		}
	}
	if out := log.String(); !strings.Contains(out, change+": stopped with the dispatcher") || strings.Contains(out, change+": blocked (") {
		t.Errorf("want the dispatcher log to report the change as stopped, never as blocked:\n%s", out)
	}
}

// A daemon stop cancels a worker while its repair agent runs. The killed agent and the verification
// that follows under the cancelled context (every git command fails) say nothing about the change: the
// round must not be recorded as failed, and the change must not end "blocked".
func TestWorkerStoppedDuringARepairAgentRecordsNoRound(t *testing.T) {
	const change = "stopped-in-repair-agent"
	repairing := make(chan struct{})
	var once sync.Once
	var builds atomic.Int32
	plan := continuousStubAgent()
	agent := func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		if req.Role == "plan" {
			return plan(ctx, req)
		}
		if builds.Add(1) == 1 {
			// Every task checked, but the file the child's gate tests is missing: verification sends
			// the change back for a repair round.
			child := filepath.Join(req.Worktree, "repos", "backend")
			_ = os.WriteFile(filepath.Join(child, req.Change+".txt"), []byte(req.Change+"\n"), 0o644)
			_ = os.WriteFile(filepath.Join(req.Worktree, "openspec", "changes", req.Change, "tasks.md"), []byte("# Tasks\n\n- [x] 1.1 Add the file in repos/backend\n"), 0o644)
			return AgentRunResult{ExitCode: 0, UsedEngine: "claude"}, nil
		}
		once.Do(func() { close(repairing) })
		<-ctx.Done()
		return AgentRunResult{ExitCode: 143, UsedEngine: "claude", Output: "terminated"}, nil
	}
	root, st, log := stoppedWorkerServe(t, change, agent, nil, repairing)
	stoppedWorkerAssertNothingRecorded(t, root, st, log, change)
}

// The same stop while the repair round's gates run: the gates the cancellation killed report a
// failure, which is the cancellation's and not the change's.
func TestWorkerStoppedDuringRepairVerificationRecordsNoRound(t *testing.T) {
	const change = "stopped-in-repair-gates"
	verifying := make(chan struct{})
	var once sync.Once
	var gates atomic.Int32
	gate := func(ctx context.Context, req GateRunRequest) (GateRunResult, error) {
		if gates.Add(1) == 1 {
			return GateRunResult{Output: "---- [FAIL] backend:test (exit 1)\ngates: FAIL (backend:test)\n", ExitCode: 1, OK: false,
				Failures: []GateFailure{{Child: "repos/backend", Gate: "test", ExitCode: 1}}}, nil
		}
		once.Do(func() { close(verifying) })
		<-ctx.Done()
		// What DefaultGateRunner returns when a cancellation kills the gate's process group.
		return GateRunResult{Output: "---- [FAIL] backend:test (exit 143)\ngates: FAIL (backend:test)\n", ExitCode: 1, OK: false,
			Failures: []GateFailure{{Child: "repos/backend", Gate: "test", ExitCode: 143}}}, nil
	}
	root, st, log := stoppedWorkerServe(t, change, continuousStubAgent(), gate, verifying)
	stoppedWorkerAssertNothingRecorded(t, root, st, log, change)
}

// A worker whose lease another dispatcher took over is stopped the same way, in the middle of a repair
// round. It records nothing for the change, and that includes the round: the new owner builds in the
// same worktree and writes its own repairs.md there.
func TestWorkerThatLostItsLeaseDuringARepairRecordsNoRound(t *testing.T) {
	root, _, _, st := setupTestWorkspaceWithSubmodule(t)
	const change = "lease-lost-in-repair"
	writeBacklogJSON(t, root, map[string]any{"change_name": change, "description": "Its lease is taken over while it is repaired", "priority": "high"})

	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()

	var builds atomic.Int32
	plan := continuousStubAgent()
	disp, err := NewDispatcher(DispatcherOptions{Root: root, Store: st, MaxWorkers: 1, Out: io.Discard,
		AgentRunner: func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
			if req.Role == "plan" {
				return plan(ctx, req)
			}
			if builds.Add(1) == 1 {
				child := filepath.Join(req.Worktree, "repos", "backend")
				_ = os.WriteFile(filepath.Join(child, req.Change+".txt"), []byte(req.Change+"\n"), 0o644)
				_ = os.WriteFile(filepath.Join(req.Worktree, "openspec", "changes", req.Change, "tasks.md"), []byte("# Tasks\n\n- [x] 1.1 Add the file in repos/backend\n"), 0o644)
				return AgentRunResult{ExitCode: 0, UsedEngine: "claude"}, nil
			}
			// While the repair runs, another dispatcher judged the lease stale by its TTL and took it.
			now := time.Now().UTC().Unix()
			if _, err := st.DB().Exec(`UPDATE locks SET pid = ?, acquired_at = ? * 1000, heartbeat_at = ? * 1000 WHERE lock_key = ?`,
				holder.Process.Pid, now, now, store.WorkerLeaseKey(req.Change)); err != nil {
				t.Error(err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(20 * time.Second):
				t.Error("the repair agent kept running after the worker lost its lease")
			}
			return AgentRunResult{ExitCode: 0, UsedEngine: "claude"}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = disp.Close()

	ctx := context.Background()
	if rounds, _ := st.ListRepairRounds(ctx, change); len(rounds) != 0 {
		t.Errorf("a worker that lost its lease recorded a repair round: %+v", rounds)
	}
	repairs, _ := os.ReadFile(filepath.Join(root, ".worktrees", "worker-"+change, "openspec", "changes", change, "repairs.md"))
	if strings.Contains(string(repairs), "## repair-") {
		t.Errorf("a worker that lost its lease wrote to repairs.md:\n%s", repairs)
	}
	evs, err := st.QueryEvents(ctx, store.EventFilter{Event: "change.finished", ChangeName: change})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 || evs[0].Payload["result"] != "lost" {
		t.Errorf("want the worker to end \"lost\", got %+v", evs)
	}
}
