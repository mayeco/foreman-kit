package orchestrator

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

// After a heartbeat gap longer than the lease (a sleeping laptop), another live process may take the
// merger lock: Merger.Serve must notice before it integrates anything.
func TestMergerServeStopsWhenItsLockWasTakenOver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	hub := NewHub()
	m, err := NewMerger(MergerOptions{Root: umbrellaDir, Store: st, Hub: hub, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- m.Serve(ctx) }()
	if !waitUntil(3*time.Second, func() bool { l, _ := st.LiveLock(ctx, "merger"); return l != nil }) {
		t.Fatal("Merger.Serve never took the merger lock")
	}

	// A cron cycle's merger judged the lock stale by its TTL and took it.
	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	now := time.Now().UTC().Unix()
	if _, err := st.DB().Exec(`UPDATE locks SET pid = ?, acquired_at = ? * 1000, heartbeat_at = ? * 1000 WHERE lock_key = 'merger'`, holder.Process.Pid, now, now); err != nil {
		t.Fatal(err)
	}

	const change = "lock-taken-over"
	buildReadyManifest(t, ctx, umbrellaDir, st, change)
	hub.ReadyEnqueued()

	select {
	case err := <-serveDone:
		if !errors.Is(err, store.ErrLockHeld) {
			t.Fatalf("expected store.ErrLockHeld, got %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("Serve kept running after another process took the merger lock (integrated: %v)", integratedOnOrigin(umbrellaDir, change))
	}
	if integratedOnOrigin(umbrellaDir, change) {
		t.Fatal("integrated while another merger owned the lock")
	}
}

// A takeover while an integration runs its gates stops those gates, and the change is neither
// quarantined nor removed from the merge queue, nor is the workspace paused: the new owner integrates it.
func TestMergerServeStopsAnIntegrationWhenItsLockIsTakenOver(t *testing.T) {
	settingstest.Give(t, settings.LocksStaleAfter, "3s") // heartbeat every second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)
	const change = "takeover-mid-gates"
	buildReadyManifest(t, ctx, umbrellaDir, st, change)

	gatesStarted, gatesStopped := make(chan struct{}), make(chan struct{})
	hub := NewHub()
	m, err := NewMerger(MergerOptions{Root: umbrellaDir, Store: st, Hub: hub, Out: io.Discard,
		GateRunner: func(ctx context.Context, req GateRunRequest) (GateRunResult, error) {
			close(gatesStarted)
			<-ctx.Done()
			close(gatesStopped)
			return GateRunResult{Output: "stopped", ExitCode: 143}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- m.Serve(ctx) }()
	hub.ReadyEnqueued()
	select {
	case <-gatesStarted:
	case <-time.After(20 * time.Second):
		t.Fatal("the merger never reached the gates")
	}

	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	now := time.Now().UTC().Unix()
	if _, err := st.DB().Exec(`UPDATE locks SET pid = ?, acquired_at = ? * 1000, heartbeat_at = ? * 1000 WHERE lock_key = 'merger'`, holder.Process.Pid, now, now); err != nil {
		t.Fatal(err)
	}

	select {
	case <-gatesStopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the gates of the integration in flight kept running after another process took the lock")
	}
	select {
	case err := <-serveDone:
		if !errors.Is(err, store.ErrLockHeld) {
			t.Fatalf("expected store.ErrLockHeld, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve kept running after another process took the merger lock")
	}
	if q, _ := st.GetQuarantine(context.Background(), change); q != nil && q.Status == "blocked" {
		t.Fatalf("the change was quarantined by a merger that no longer owned integration: %s", q.Reason)
	}
	if !inReadyQueue(context.Background(), st, change) {
		t.Fatal("the change left the merge queue")
	}
	if ws, _ := st.GetWorkspaceState(context.Background()); ws.Paused {
		t.Fatalf("workspace paused: %s", ws.PauseReason)
	}
}

// The dispatcher's Serve, too, stops once another process took its lock, and stops its workers.
func TestDispatcherServeStopsWhenItsLockWasTakenOver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)
	writeBacklogJSON(t, umbrellaDir, map[string]any{"change_name": "dispatch-taken-over", "description": "A long build", "priority": "high"})

	agentStarted, agentStopped := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hub := NewHub()
	d, err := NewDispatcher(DispatcherOptions{Root: umbrellaDir, Store: st, MaxWorkers: 1, Hub: hub, Out: io.Discard,
		AgentRunner: func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
			once.Do(func() { close(agentStarted) })
			<-ctx.Done()
			select {
			case <-agentStopped:
			default:
				close(agentStopped)
			}
			return AgentRunResult{ExitCode: 143, Output: "stopped"}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	serveDone := make(chan error, 1)
	go func() { serveDone <- d.Serve(ctx) }()
	select {
	case <-agentStarted:
	case <-time.After(20 * time.Second):
		t.Fatal("the dispatcher never started a worker")
	}

	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	now := time.Now().UTC().Unix()
	if _, err := st.DB().Exec(`UPDATE locks SET pid = ?, acquired_at = ? * 1000, heartbeat_at = ? * 1000 WHERE lock_key = 'dispatcher'`, holder.Process.Pid, now, now); err != nil {
		t.Fatal(err)
	}
	hub.BacklogChanged()

	select {
	case err := <-serveDone:
		if !errors.Is(err, store.ErrLockHeld) {
			t.Fatalf("expected store.ErrLockHeld, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Dispatcher.Serve kept running after another process took the dispatcher lock")
	}
	select {
	case <-agentStopped:
	default:
		t.Fatal("the worker of the dispatcher that lost its lock kept running")
	}
}
