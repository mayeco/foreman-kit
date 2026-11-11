package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
)

// The daemon's dispatcher marks its lock "mode": "continuous", which foreman-kit progress and a cron
// cycle read to tell it from a dispatcher started by hand. Recording a finished worker rewrites the lock
// metadata with the results; it must keep the mode, or every report after the first finished worker
// calls the daemon's dispatcher "a dispatcher started by hand".
func TestDispatcherServeKeepsItsContinuousModeAfterAWorkerFinishes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	const change = "serve-mode-item"
	writeBacklogJSON(t, umbrellaDir, map[string]any{"change_name": change, "description": "Keep the dispatcher mode", "priority": "high"})
	disp, err := NewDispatcher(DispatcherOptions{Root: umbrellaDir, Store: st, MaxWorkers: 1, Hub: NewHub(), AgentRunner: continuousStubAgent(), Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer disp.Close()
	serveDone := make(chan error, 1)
	go func() { serveDone <- disp.Serve(ctx) }()
	defer func() {
		cancel()
		<-serveDone
	}()

	// recordWorkerOutcome rewrites the lock metadata before it emits change.finished.
	if !waitUntil(30*time.Second, func() bool { return hasEvent(ctx, st, "change.finished", change) }) {
		t.Fatalf("expected %s to finish", change)
	}
	l, err := st.GetLock(ctx, "dispatcher")
	if err != nil || l == nil {
		t.Fatalf("expected the dispatcher lock, got %+v (err=%v)", l, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(l.MetadataJSON), &meta); err != nil {
		t.Fatalf("lock metadata %q: %v", l.MetadataJSON, err)
	}
	if meta["mode"] != "continuous" {
		t.Fatalf("the dispatcher lock lost its continuous mode after a worker finished: %s", l.MetadataJSON)
	}
	if results, _ := meta["results"].(map[string]any); results[change] == nil {
		t.Fatalf("expected the lock metadata to carry %s's result, got %s", change, l.MetadataJSON)
	}

	rep, err := cycle.Progress(ctx, umbrellaDir, st, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	now, _ := rep["dashboard"].(map[string]any)
	if now["dispatcher_mode"] != "continuous" {
		t.Fatalf("foreman-kit progress reports dispatcher_mode %v for the daemon's dispatcher, want continuous", now["dispatcher_mode"])
	}
}
