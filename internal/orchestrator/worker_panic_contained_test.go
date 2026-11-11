package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// A panic under one worker (here in its BUILD agent runner) must stay that change's failure. It used to
// end the process: the resident daemon with its portal and merger, and the agents of every other worker
// left running with nobody to stop them. The panicking change goes through the orchestrator-error path
// (blocked with the panic text; the system-error handling decides about pausing), its lease, slot and
// capabilities are freed, the worker next to it finishes its build, and the dispatcher keeps running.
func TestWorkerPanicIsContainedToItsChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root, _, _, st := setupTestWorkspaceWithSubmodule(t)
	const panicking, healthy = "panics-in-its-build", "builds-next-to-it"
	const panicText = "index out of range in a parser of the build phase"
	writeBacklogJSON(t, root,
		map[string]any{"change_name": panicking, "description": "Its worker panics", "priority": "high"},
		map[string]any{"change_name": healthy, "description": "Its worker must not notice", "priority": "high"},
	)

	healthyBuilding := make(chan struct{})
	var once sync.Once
	stub := continuousStubAgent()
	agent := func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		if req.Role == "plan" {
			return stub(ctx, req)
		}
		if req.Change == panicking {
			// Panic only once the other worker's agent is running: it must survive.
			select {
			case <-healthyBuilding:
			case <-time.After(60 * time.Second):
			}
			panic(panicText)
		}
		once.Do(func() { close(healthyBuilding) })
		time.Sleep(500 * time.Millisecond)
		return stub(ctx, req)
	}
	var log syncBuffer
	disp, err := NewDispatcher(DispatcherOptions{Root: root, Store: st, MaxWorkers: 2, Hub: NewHub(), AgentRunner: agent, Out: &log})
	if err != nil {
		t.Fatal(err)
	}
	defer disp.Close()
	serveDone := make(chan error, 1)
	go func() { serveDone <- disp.Serve(ctx) }()

	if !waitUntil(90*time.Second, func() bool {
		return hasEvent(ctx, st, "change.finished", panicking) && hasEvent(ctx, st, "change.finished", healthy)
	}) {
		t.Fatalf("both workers must end; log:\n%s", log.String())
	}

	q, _ := st.GetQuarantine(ctx, panicking)
	if q == nil || q.Status != "blocked" || q.Reason != "orchestrator_error" || !strings.Contains(q.Details, panicText) {
		t.Errorf("want the panicking change blocked as orchestrator_error with the panic text, got %+v", q)
	}
	if out := log.String(); !strings.Contains(out, panicking+": panic in its worker: "+panicText) || !strings.Contains(out, "goroutine ") {
		t.Errorf("want the panic and its stack in the dispatcher log, got:\n%s", out)
	}
	if !inReadyQueue(ctx, st, healthy) {
		t.Errorf("the worker next to the panicking one did not finish its build; log:\n%s", log.String())
	}
	if l, _ := st.GetLock(ctx, store.WorkerLeaseKey(panicking)); l != nil {
		t.Errorf("the panicking worker's lease was not released: %+v", l)
	}
	disp.stateMu.Lock()
	_, active := disp.active[panicking]
	_, claimed := disp.building[panicking]
	disp.stateMu.Unlock()
	if active || claimed {
		t.Errorf("the panicking worker kept its slot (%v) or its capabilities (%v)", active, claimed)
	}

	// The dispatcher did not stop: it still serves, and ends only when it is cancelled.
	select {
	case err := <-serveDone:
		t.Fatalf("Serve ended after the panic with %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned %v on cancellation", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

// A panic raised while stateMu is held without a deferred unlock leaves it locked for good, and both the
// recovery and the scheduling loop need it: containing that panic would hang the dispatcher silently.
// It is raised again instead, which ends the process as before.
func TestWorkerPanicThatLeftTheStateLockedIsRaisedAgain(t *testing.T) {
	root, _, _, st := setupTestWorkspaceWithSubmodule(t)
	var log syncBuffer
	disp, err := NewDispatcher(DispatcherOptions{Root: root, Store: st, MaxWorkers: 1, Out: &log})
	if err != nil {
		t.Fatal(err)
	}
	defer disp.Close()
	prev := workerPanicLockWait
	workerPanicLockWait = 50 * time.Millisecond
	defer func() { workerPanicLockWait = prev }()

	disp.stateMu.Lock()
	defer disp.stateMu.Unlock()
	defer func() {
		if r := recover(); r != "left the state locked" {
			t.Errorf("want the panic raised again, got %v", r)
		}
		if !strings.Contains(log.String(), "cannot be contained") {
			t.Errorf("want the log to say why the panic is not contained, got:\n%s", log.String())
		}
	}()
	disp.workerPanicked("some-change", "left the state locked", func(error) string {
		t.Error("the error path must not run while the state is locked")
		return "blocked"
	})
	t.Error("workerPanicked returned although the state was locked")
}
