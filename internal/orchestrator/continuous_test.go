package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/store"
)

// continuousStubAgent plans a valid change with its own capability (so changes never hold each other's
// capabilities or collide on archive) and builds it by writing the file the child repository's gate
// checks plus one file of its own.
func continuousStubAgent() AgentRunnerFunc {
	return func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		chgDir := filepath.Join(req.Worktree, "openspec", "changes", req.Change)
		capName := "cap-" + req.Change
		if req.Role == "plan" {
			_ = os.MkdirAll(filepath.Join(chgDir, "specs", capName), 0o755)
			_ = os.WriteFile(filepath.Join(chgDir, "proposal.md"), []byte("# Proposal\n\n## Why\nExercise the continuous pipeline.\n\n## What Changes\n- Add a file to repos/backend.\n"), 0o644)
			_ = os.WriteFile(filepath.Join(chgDir, "design.md"), []byte("# Design\n\n## Goals\n- Integrate on the fly.\n"), 0o644)
			_ = os.WriteFile(filepath.Join(chgDir, "tasks.md"), []byte("# Tasks\n\n- [ ] 1.1 Add the file in repos/backend\n"), 0o644)
			spec := fmt.Sprintf("## Purpose\nVerifies that the continuous pipeline dispatches and integrates %s on the fly.\n\n## ADDED Requirements\n\n### Requirement: %s file\nThe backend SHALL provide %s.txt.\n\n#### Scenario: File present\n- **WHEN** built\n- **THEN** %s.txt exists\n", req.Change, req.Change, req.Change, req.Change)
			_ = os.WriteFile(filepath.Join(chgDir, "specs", capName, "spec.md"), []byte(spec), 0o644)
			return AgentRunResult{ExitCode: 0, UsedEngine: "claude"}, nil
		}
		child := filepath.Join(req.Worktree, "repos", "backend")
		_ = os.WriteFile(filepath.Join(child, "feature.txt"), []byte("continuous-payload\n"), 0o644)
		_ = os.WriteFile(filepath.Join(child, req.Change+".txt"), []byte(req.Change+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(chgDir, "tasks.md"), []byte("# Tasks\n\n- [x] 1.1 Add the file in repos/backend\n"), 0o644)
		return AgentRunResult{ExitCode: 0, UsedEngine: "claude"}, nil
	}
}

func writeBacklogJSON(t *testing.T, root string, items ...map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "agent_backlog.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitUntil polls cond every 20ms until it holds or timeout elapses, and reports whether it held.
func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func integratedOnOrigin(root, change string) bool {
	repo, err := gitx.Open(root)
	if err != nil {
		return false
	}
	ok, _ := repo.HasCommitWithTrailer("origin/master", change)
	return ok
}

func hasEvent(ctx context.Context, st *store.Store, event, change string) bool {
	evts, _ := st.QueryEvents(ctx, store.EventFilter{Event: event, ChangeName: change})
	return len(evts) > 0
}

func inReadyQueue(ctx context.Context, st *store.Store, change string) bool {
	items, _ := st.ListReadyQueue(ctx)
	for _, it := range items {
		if it.ChangeName == change {
			return true
		}
	}
	return false
}

// buildReadyManifest runs a one-shot dispatcher (no Hub) until change sits in the merge queue.
func buildReadyManifest(t *testing.T, ctx context.Context, root string, st *store.Store, change string) {
	t.Helper()
	writeBacklogJSON(t, root, map[string]any{"change_name": change, "description": "Integrate " + change + " on the fly", "priority": "high"})
	disp, err := NewDispatcher(DispatcherOptions{Root: root, Store: st, MaxWorkers: 1, AgentRunner: continuousStubAgent(), Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp.Run(ctx); err != nil {
		t.Fatal(err)
	}
	_ = disp.Close()
	if !inReadyQueue(ctx, st, change) {
		evts, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: change})
		t.Fatalf("expected %s in the merge queue after the one-shot dispatch; events: %+v", change, evts)
	}
}

// syncBuffer is an io.Writer safe for the concurrent loops under test.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestHubCoalesces(t *testing.T) {
	h := NewHub()
	for i := 0; i < 1000; i++ {
		h.ReadyEnqueued()
		h.BacklogChanged()
	}
	for _, ch := range []<-chan struct{}{h.Ready(), h.Backlog()} {
		select {
		case <-ch:
		default:
			t.Fatal("expected one pending wake-up after 1000 notifications")
		}
		select {
		case <-ch:
			t.Fatal("expected 1000 notifications to coalesce into exactly one wake-up")
		default:
		}
	}

	var nilHub *Hub
	nilHub.ReadyEnqueued()
	nilHub.BacklogChanged()
	if nilHub.Ready() != nil || nilHub.Backlog() != nil {
		t.Fatal("expected a nil Hub to expose nil channels")
	}
	select {
	case <-nilHub.Ready():
		t.Fatal("a nil Hub's channel must never deliver")
	case <-time.After(10 * time.Millisecond):
	}
}

func TestMergerServeIntegratesImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	hub := NewHub()
	var log syncBuffer
	merger, err := NewMerger(MergerOptions{Root: umbrellaDir, Store: st, Hub: hub, Out: &log})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- merger.Serve(ctx) }()
	if !waitUntil(3*time.Second, func() bool { l, _ := st.LiveLock(ctx, "merger"); return l != nil }) {
		t.Fatal("Merger.Serve never took the merger lock")
	}

	const change = "serve-merge-now"
	buildReadyManifest(t, ctx, umbrellaDir, st, change)

	// Only the signal can wake the merger: its fallback is 30s.
	signaled := time.Now()
	hub.ReadyEnqueued()
	if !waitUntil(3*time.Second, func() bool { return integratedOnOrigin(umbrellaDir, change) }) {
		t.Fatalf("expected %s on origin/master within 3s of the ready signal; merger log:\n%s", change, log.String())
	}
	t.Logf("integrated %s after %s", change, time.Since(signaled).Round(time.Millisecond))
	if !strings.Contains(log.String(), "merger: woke (ready)") {
		t.Fatalf("expected the merger log to name its wake-up reason, got:\n%s", log.String())
	}
	// The merger removes the manifest after pushing and cleaning up the worker.
	if !waitUntil(3*time.Second, func() bool { return !inReadyQueue(ctx, st, change) }) {
		t.Fatalf("expected %s to leave the merge queue once integrated", change)
	}

	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve returned %v on cancellation", err)
	}
	if l, _ := st.GetLock(context.Background(), "merger"); l != nil {
		t.Fatalf("expected Serve to release the merger lock, still held by pid %d", l.PID)
	}
}

func TestDispatcherServePicksUpNewBacklogItems(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	const first, second = "serve-first-item", "serve-second-item"
	writeBacklogJSON(t, umbrellaDir, map[string]any{"change_name": first, "description": "First item", "priority": "high"})

	hub := NewHub()
	var log syncBuffer
	disp, err := NewDispatcher(DispatcherOptions{Root: umbrellaDir, Store: st, MaxWorkers: 1, Hub: hub, AgentRunner: continuousStubAgent(), Out: &log})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- disp.Serve(ctx) }()

	if !waitUntil(20*time.Second, func() bool { return hasEvent(ctx, st, "change.finished", first) }) {
		t.Fatalf("expected %s to finish; log:\n%s", first, log.String())
	}
	if !inReadyQueue(ctx, st, first) {
		t.Fatalf("expected %s in the merge queue", first)
	}
	// Let the pass that followed the finished worker complete: from here on only a signal (or the 30s
	// fallback) makes the dispatcher read the backlog again.
	time.Sleep(300 * time.Millisecond)

	writeBacklogJSON(t, umbrellaDir,
		map[string]any{"change_name": first, "description": "First item", "priority": "high"},
		map[string]any{"change_name": second, "description": "Second item", "priority": "high"},
	)
	signaled := time.Now()
	hub.BacklogChanged()
	if !waitUntil(3*time.Second, func() bool { return hasEvent(ctx, st, "change.start", second) }) {
		t.Fatalf("expected %s to start within 3s of the backlog signal; log:\n%s", second, log.String())
	}
	t.Logf("started %s after %s", second, time.Since(signaled).Round(time.Millisecond))
	if !strings.Contains(log.String(), "dispatcher: woke (backlog)") {
		t.Fatalf("expected the dispatcher log to name its wake-up reason, got:\n%s", log.String())
	}

	// The loop never exits on its own, even with nothing left to do.
	if !waitUntil(20*time.Second, func() bool { return hasEvent(ctx, st, "change.finished", second) }) {
		t.Fatalf("expected %s to finish", second)
	}
	select {
	case err := <-serveDone:
		t.Fatalf("Serve exited on its own with %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve returned %v on cancellation", err)
	}
}

func TestDependentStartsWhenDependencyIntegrates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	const depA, depB = "pipeline-dep-a", "pipeline-dep-b"
	writeBacklogJSON(t, umbrellaDir,
		map[string]any{"change_name": depA, "description": "Dependency", "priority": "high"},
		map[string]any{"change_name": depB, "description": "Dependant", "priority": "high", "depends_on": []string{depA}},
	)

	hub := NewHub()
	var log syncBuffer
	merger, err := NewMerger(MergerOptions{Root: umbrellaDir, Store: st, Hub: hub, Out: &log})
	if err != nil {
		t.Fatal(err)
	}
	disp, err := NewDispatcher(DispatcherOptions{Root: umbrellaDir, Store: st, MaxWorkers: 2, Hub: hub, AgentRunner: continuousStubAgent(), Out: &log})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = merger.Serve(ctx) }()
	go func() { defer wg.Done(); _ = disp.Serve(ctx) }()

	if !waitUntil(30*time.Second, func() bool { return hasEvent(ctx, st, "merge.integrated", depA) }) {
		t.Fatalf("expected %s to integrate; log:\n%s", depA, log.String())
	}
	integrated := time.Now()
	if hasEvent(ctx, st, "change.start", depB) {
		t.Fatalf("%s started before its dependency %s was integrated", depB, depA)
	}
	if !waitUntil(3*time.Second, func() bool { return hasEvent(ctx, st, "change.start", depB) }) {
		t.Fatalf("expected %s to start within 3s of %s's integration; log:\n%s", depB, depA, log.String())
	}
	t.Logf("%s started %s after %s integrated", depB, time.Since(integrated).Round(time.Millisecond), depA)

	if !waitUntil(30*time.Second, func() bool { return integratedOnOrigin(umbrellaDir, depB) }) {
		t.Fatalf("expected %s to integrate too; log:\n%s", depB, log.String())
	}
	cancel()
	wg.Wait()
}

func TestEnginesDownDoesNotPauseQuickly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	const change = "quota-outage"
	writeBacklogJSON(t, umbrellaDir, map[string]any{"change_name": change, "description": "Waits out a quota outage", "priority": "high"})

	// A fake clock: the test advances it 2s every 25ms, so a minute of dispatcher time takes under a
	// second while the default 60s engine backoff stays in force.
	var clockMu sync.Mutex
	start := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	fake := start
	now := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return fake }

	var calls atomic.Int32
	disp, err := NewDispatcher(DispatcherOptions{
		Root:             umbrellaDir,
		Store:            st,
		MaxWorkers:       1,
		Hub:              NewHub(),
		NowFunc:          now,
		FallbackInterval: 20 * time.Millisecond,
		Out:              io.Discard,
		AgentRunner: func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
			calls.Add(1)
			return AgentRunResult{ExitCode: EnginesDownExitCode, Output: "every engine: quota exceeded"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- disp.Serve(ctx) }()

	if !waitUntil(10*time.Second, func() bool { return hasEvent(ctx, st, "change.deferred", change) }) {
		t.Fatal("expected the first outage to defer the change")
	}
	for now().Before(start.Add(62 * time.Second)) {
		time.Sleep(25 * time.Millisecond)
		clockMu.Lock()
		fake = fake.Add(2 * time.Second)
		clockMu.Unlock()
	}
	// Let a worker started at the end of the minute finish before reading the counters.
	waitUntil(5*time.Second, func() bool { return disp.activeCount() == 0 })
	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve returned %v", err)
	}

	if evts, _ := st.QueryEvents(context.Background(), store.EventFilter{Event: "system.paused"}); len(evts) > 0 {
		t.Fatalf("an engine outage must not pause the system within a minute, got %+v", evts)
	}
	if ws, _ := st.GetWorkspaceState(context.Background()); ws.Paused {
		t.Fatalf("workspace paused: %s", ws.PauseReason)
	}
	var counted int
	_ = st.DB().QueryRow(`SELECT deferrals FROM v_deferral_count WHERE change_name = ?`, change).Scan(&counted)
	if counted > 1 {
		t.Fatalf("expected at most 1 deferral counted in a minute, got %d", counted)
	}
	if q, _ := st.GetQuarantine(context.Background(), change); q != nil && q.Status == "blocked" {
		t.Fatalf("expected %s deferred, not blocked (%s)", change, q.Reason)
	}
	if calls.Load() < 1 || calls.Load() > 3 {
		t.Fatalf("expected the cooldown to space the attempts (1 to 3 agent calls in a minute), got %d", calls.Load())
	}
}

// daemonTestOptions wires a foreground daemon to a continuous merger the way cmd/foreman-kit does.
func daemonTestOptions(st *store.Store, hub *Hub, cycleOpts cycle.CycleOptions) cycle.DaemonOptions {
	return cycle.DaemonOptions{
		Interval:       time.Hour,
		LoopRetryDelay: 200 * time.Millisecond,
		CycleOpts:      cycleOpts,
		Notifier:       hub,
		ServeMergerFunc: func(ctx context.Context, root string, out io.Writer) error {
			m, err := NewMerger(MergerOptions{Root: root, Store: st, Hub: hub, Out: out})
			if err != nil {
				return err
			}
			defer m.Close()
			return m.Serve(ctx)
		},
	}
}

func readDaemonLog(root string) string {
	b, _ := os.ReadFile(store.RuntimePath(root, "daemon.log"))
	return string(b)
}

func TestCycleDoesNotBlockMerges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	// Build the manifest first, then hold it back until the judgment is under way.
	const change = "merge-during-judgment"
	buildReadyManifest(t, ctx, umbrellaDir, st, change)
	queued, err := st.ListReadyQueue(ctx)
	if err != nil || len(queued) != 1 {
		t.Fatalf("expected one ready manifest, got %d (%v)", len(queued), err)
	}
	manifest := queued[0]
	if err := st.RemoveReady(ctx, change); err != nil {
		t.Fatal(err)
	}

	judging := make(chan struct{})
	var judgmentEnded atomic.Bool
	hub := NewHub()
	opts := daemonTestOptions(st, hub, cycle.CycleOptions{
		SkipWorktreePrep: true,
		AgentExecFunc: func(ctx context.Context, workdir, promptFile, engine string, timeout time.Duration, out io.Writer) error {
			close(judging)
			select { // a long judgment
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
			}
			judgmentEnded.Store(true)
			return nil
		},
	})

	daemonDone := make(chan int, 1)
	go func() { daemonDone <- cycle.StartDaemon(ctx, umbrellaDir, st, opts, io.Discard, io.Discard) }()

	select {
	case <-judging:
	case <-time.After(15 * time.Second):
		t.Fatalf("the initial cycle never reached its judgment; daemon log:\n%s", readDaemonLog(umbrellaDir))
	}
	if err := st.EnqueueReady(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	hub.ReadyEnqueued()
	if !waitUntil(4*time.Second, func() bool { return integratedOnOrigin(umbrellaDir, change) }) {
		t.Fatalf("expected %s integrated while the judgment runs; daemon log:\n%s", change, readDaemonLog(umbrellaDir))
	}
	if judgmentEnded.Load() {
		t.Fatal("the merge waited for the judgment to end")
	}

	cancel()
	select {
	case <-daemonDone:
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestDaemonLoopsYieldToCronLocks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	const change = "yield-to-cron-merger"
	buildReadyManifest(t, ctx, umbrellaDir, st, change)

	// A cron cycle's merger, in another process, owns integration.
	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	defer func() {
		if !reaped {
			_ = holder.Process.Kill()
			_ = holder.Wait()
		}
	}()
	if _, err := st.AcquireLock(ctx, "merger", store.LockOptions{OwnerType: "merger", PID: holder.Process.Pid, Slot: MergerSlot, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}

	hub := NewHub()
	m, err := NewMerger(MergerOptions{Root: umbrellaDir, Store: st, Hub: hub, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Serve(ctx); !errors.Is(err, store.ErrLockHeld) {
		t.Fatalf("expected Serve to return store.ErrLockHeld while another process holds the merger lock, got %v", err)
	}

	opts := daemonTestOptions(st, hub, cycle.CycleOptions{})
	opts.NoInitialCycle = true
	daemonDone := make(chan int, 1)
	go func() { daemonDone <- cycle.StartDaemon(ctx, umbrellaDir, st, opts, io.Discard, io.Discard) }()

	hub.ReadyEnqueued()
	time.Sleep(1500 * time.Millisecond)
	if integratedOnOrigin(umbrellaDir, change) {
		t.Fatal("the daemon integrated while another process held the merger lock")
	}
	if !inReadyQueue(ctx, st, change) {
		t.Fatal("expected the change to stay in the merge queue while the lock is held elsewhere")
	}
	if log := readDaemonLog(umbrellaDir); !strings.Contains(log, "daemon: merger waits") {
		t.Fatalf("expected the daemon log to say the merger waits for the lock, got:\n%s", log)
	}

	// The cron merger exits: its lock goes stale and the daemon's merger takes over.
	_ = holder.Process.Kill()
	_ = holder.Wait()
	reaped = true
	if !waitUntil(10*time.Second, func() bool { return integratedOnOrigin(umbrellaDir, change) }) {
		t.Fatalf("expected the daemon to integrate %s once the lock was released; daemon log:\n%s", change, readDaemonLog(umbrellaDir))
	}

	cancel()
	select {
	case <-daemonDone:
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not stop")
	}
}
