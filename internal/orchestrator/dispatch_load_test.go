package orchestrator

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// A worker is out of work when one is free and nothing waits for it; a pipeline that is paused, full
// or holding an item it could not start is not.
func TestDispatchLoadOutOfWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		load DispatchLoad
		want bool
	}{
		{"every worker free, nothing waiting", DispatchLoad{Workers: 2}, true},
		{"one worker free, nothing waiting", DispatchLoad{Workers: 2, Running: 1}, true},
		{"every worker building", DispatchLoad{Workers: 2, Running: 2}, false},
		{"a worker free but an item waits", DispatchLoad{Workers: 2, Running: 1, Waiting: 1}, false},
		{"halted", DispatchLoad{Workers: 2, Halted: true}, false},
		{"no load at all", DispatchLoad{}, false},
	} {
		if got := tc.load.OutOfWork(); got != tc.want {
			t.Errorf("%s: OutOfWork() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The Hub keeps what the latest pass left, coalesces its wake-ups, forgets the load when the
// dispatcher stops, and stays a no-op when nil.
func TestHubKeepsTheLatestDispatchLoad(t *testing.T) {
	var none *Hub
	none.DispatchPassed(DispatchLoad{Workers: 1})
	none.DispatchStopped()
	if _, ok := none.DispatchLoad(); ok || none.Dispatched() != nil {
		t.Fatal("a nil Hub has no load and no wake-ups")
	}

	h := NewHub()
	if _, ok := h.DispatchLoad(); ok {
		t.Fatal("no dispatcher has passed yet: there is no load")
	}
	at := time.Now()
	for running := 0; running < 3; running++ {
		h.DispatchPassed(DispatchLoad{Workers: 3, Running: running, At: at})
	}
	load, ok := h.DispatchLoad()
	if !ok || load.Running != 2 || !load.At.Equal(at) {
		t.Fatalf("expected the load of the latest pass, got %+v (ok=%v)", load, ok)
	}
	select {
	case <-h.Dispatched():
	default:
		t.Fatal("a pass that ended wakes the cycle loop")
	}
	select {
	case <-h.Dispatched():
		t.Fatal("three passes before a wake-up deliver one wake-up")
	default:
	}

	h.DispatchStopped()
	if _, ok := h.DispatchLoad(); ok {
		t.Fatal("a stopped dispatcher leaves no load")
	}
	select {
	case <-h.Dispatched():
	default:
		t.Fatal("stopping wakes the cycle loop, so it stops counting on the old load")
	}
}

// The continuous dispatcher tells after each pass what it left: with one worker and two items it is
// not out of work while one builds and the other waits, it is once both are built, and it says nothing
// once it has stopped.
func TestDispatcherServeReportsItsLoad(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	const first, second = "load-first-item", "load-second-item"
	writeBacklogJSON(t, umbrellaDir,
		map[string]any{"change_name": first, "description": "First item", "priority": "high"},
		map[string]any{"change_name": second, "description": "Second item", "priority": "normal"},
	)

	// The agent of the first change to plan waits until the test has looked at the load.
	release := make(chan struct{})
	var once sync.Once
	stub := continuousStubAgent()
	agent := func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		if req.Role == "plan" {
			held := false
			once.Do(func() { held = true })
			if held {
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
		}
		return stub(ctx, req)
	}

	hub := NewHub()
	var log syncBuffer
	started := time.Now()
	disp, err := NewDispatcher(DispatcherOptions{
		Root: umbrellaDir, Store: st, MaxWorkers: 1, Hub: hub, AgentRunner: agent, Out: &log,
		FallbackInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- disp.Serve(ctx) }()

	busy := func() bool {
		l, ok := hub.DispatchLoad()
		return ok && l.Workers == 1 && l.Running == 1 && l.Waiting == 1
	}
	if !waitUntil(20*time.Second, busy) {
		l, ok := hub.DispatchLoad()
		t.Fatalf("expected one worker building and one item waiting, got %+v (ok=%v); log:\n%s", l, ok, log.String())
	}
	if l, _ := hub.DispatchLoad(); l.OutOfWork() {
		t.Fatalf("a dispatcher with an item waiting for its only worker is not out of work: %+v", l)
	}
	close(release)

	idle := func() bool {
		l, ok := hub.DispatchLoad()
		return ok && l.OutOfWork() && inReadyQueue(ctx, st, first) && inReadyQueue(ctx, st, second)
	}
	if !waitUntil(40*time.Second, idle) {
		l, ok := hub.DispatchLoad()
		t.Fatalf("expected the dispatcher out of work once both items were built, got %+v (ok=%v); log:\n%s", l, ok, log.String())
	}
	l, _ := hub.DispatchLoad()
	if l.Workers != 1 || l.Running != 0 || l.Waiting != 0 || l.Halted {
		t.Fatalf("expected a free worker and nothing waiting, got %+v", l)
	}
	if l.At.Before(started) || l.At.After(time.Now()) {
		t.Fatalf("the load is dated when its pass began, got %s (test started %s)", l.At, started)
	}

	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve returned %v on cancellation", err)
	}
	if l, ok := hub.DispatchLoad(); ok {
		t.Fatalf("a dispatcher that stopped leaves no load, got %+v", l)
	}
}

// An item an engine outage deferred still waits for a worker: while every engine cools down the
// dispatcher has work it cannot start, which is not the same as having none.
func TestDispatcherLoadCountsDeferredItemsAsWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	const change = "load-quota-outage"
	writeBacklogJSON(t, umbrellaDir, map[string]any{"change_name": change, "description": "Waits out a quota outage", "priority": "high"})

	hub := NewHub()
	disp, err := NewDispatcher(DispatcherOptions{
		Root: umbrellaDir, Store: st, MaxWorkers: 1, Hub: hub, Out: io.Discard,
		FallbackInterval: 20 * time.Millisecond,
		AgentRunner: func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
			return AgentRunResult{ExitCode: EnginesDownExitCode, Output: "every engine: quota exceeded"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- disp.Serve(ctx) }()

	if !waitUntil(10*time.Second, func() bool { return hasEvent(ctx, st, "change.deferred", change) }) {
		t.Fatal("expected the outage to defer the change")
	}
	parked := func() bool {
		l, ok := hub.DispatchLoad()
		return ok && l.Running == 0 && l.Waiting == 1
	}
	if !waitUntil(5*time.Second, parked) {
		l, ok := hub.DispatchLoad()
		t.Fatalf("expected the deferred item to count as waiting, got %+v (ok=%v)", l, ok)
	}
	if l, _ := hub.DispatchLoad(); l.OutOfWork() {
		t.Fatalf("a dispatcher waiting out an engine outage is not out of work: %+v", l)
	}

	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve returned %v", err)
	}
}
