package orchestrator

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// The daemon's dispatcher lives as long as the daemon. An engine switched in .foreman-kit/agents.yaml during
// a quota outage must apply to its next worker, and end the engine cooldown the outage started, without
// restarting the daemon (before the continuous dispatcher, every dispatch read agents.yaml afresh).
func TestDispatcherServeSwitchesEnginesWhenAgentsYAMLChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)
	writeRoles := func(engine string) {
		settingstest.WriteFile(t, umbrellaDir, "agents", "roles:\n  plan:\n    agents: ["+engine+"]\n  build:\n    agents: ["+engine+"]\n")
	}
	writeRoles("engine-out")

	const change = "switch-engines"
	writeBacklogJSON(t, umbrellaDir, map[string]any{"change_name": change, "description": "Survives an engine switch", "priority": "high"})

	var mu sync.Mutex
	var engines []string
	stub := continuousStubAgent()
	hub := NewHub()
	disp, err := NewDispatcher(DispatcherOptions{
		Root:       umbrellaDir,
		Store:      st,
		MaxWorkers: 1,
		Hub:        hub,
		Out:        io.Discard,
		// Alone, the cooldown of the outage would hold every start for an hour.
		EngineBackoff: time.Hour,
		AgentRunner: func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
			mu.Lock()
			engines = append(engines, req.Engine)
			mu.Unlock()
			if req.Engine == "engine-out" {
				return AgentRunResult{ExitCode: EnginesDownExitCode, Output: "engine-out: quota exceeded"}, nil
			}
			return stub(ctx, req)
		},
	})
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

	if !waitUntil(20*time.Second, func() bool { return hasEvent(ctx, st, "change.deferred", change) }) {
		t.Fatal("expected the outage of engine-out to defer the change")
	}
	writeRoles("engine-ok")
	hub.BacklogChanged()
	if !waitUntil(30*time.Second, func() bool { return inReadyQueue(ctx, st, change) }) {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("expected %s built with engine-ok after the switch, agent calls used %v", change, engines)
	}
	mu.Lock()
	defer mu.Unlock()
	if last := engines[len(engines)-1]; last != "engine-ok" {
		t.Fatalf("expected the last agent call on engine-ok, calls used %v", engines)
	}
}

// The daemon's merger lives as long as the daemon too: a gate timeout raised in .foreman-kit/pipeline.yaml
// applies to its next integration.
func TestMergerServeReadsPipelineSettingsAgainOnEveryWakeUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)
	writeGateSeconds := func(n string) {
		settingstest.WriteFile(t, umbrellaDir, "pipeline", "repository:\n  base_branch: master\ngates:\n  timeout: "+n+"s\n")
	}
	writeGateSeconds("111")

	var mu sync.Mutex
	var timeouts []time.Duration
	hub := NewHub()
	merger, err := NewMerger(MergerOptions{
		Root:  umbrellaDir,
		Store: st,
		Hub:   hub,
		Out:   io.Discard,
		GateRunner: func(ctx context.Context, req GateRunRequest) (GateRunResult, error) {
			mu.Lock()
			timeouts = append(timeouts, req.Timeout)
			mu.Unlock()
			return GateRunResult{OK: true, Output: "gates: OK\n"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer merger.Close()
	serveDone := make(chan error, 1)
	go func() { serveDone <- merger.Serve(ctx) }()
	defer func() {
		cancel()
		<-serveDone
	}()
	if !waitUntil(3*time.Second, func() bool { l, _ := st.LiveLock(ctx, "merger"); return l != nil }) {
		t.Fatal("Merger.Serve never took the merger lock")
	}

	for i, step := range []struct {
		change string
		want   time.Duration
	}{{"reload-first", 111 * time.Second}, {"reload-second", 222 * time.Second}} {
		if i == 1 {
			writeGateSeconds("222")
		}
		buildReadyManifest(t, ctx, umbrellaDir, st, step.change)
		hub.ReadyEnqueued()
		if !waitUntil(20*time.Second, func() bool { return integratedOnOrigin(umbrellaDir, step.change) }) {
			t.Fatalf("expected %s integrated", step.change)
		}
		mu.Lock()
		got := timeouts[len(timeouts)-1]
		mu.Unlock()
		if got != step.want {
			t.Fatalf("%s: the merger ran its gates with a %s timeout, want %s from .foreman-kit/pipeline.yaml", step.change, got, step.want)
		}
	}
}

// A file the dispatcher reads may lose a key while the daemon runs: an edit by hand, a merge. The
// dispatcher then starts no change, says once which key keeps it, and starts the change as soon as
// the file is whole again, without a restart. It builds nothing with a value nobody wrote.
func TestDispatcherServeStartsNothingWhileAFileItReadsLacksAKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	var calls atomic.Int32
	stub := continuousStubAgent()
	hub := NewHub()
	log := &syncBuffer{}
	disp, err := NewDispatcher(DispatcherOptions{
		Root:       umbrellaDir,
		Store:      st,
		MaxWorkers: 1,
		Hub:        hub,
		Out:        log,
		AgentRunner: func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
			calls.Add(1)
			return stub(ctx, req)
		},
	})
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
	if !waitUntil(3*time.Second, func() bool { l, _ := st.LiveLock(ctx, "dispatcher"); return l != nil }) {
		t.Fatal("Dispatcher.Serve never took the dispatcher lock")
	}

	// The file loses its keys: the dispatcher says so on its next pass. A change queued after that
	// (a pass that had begun on the whole file may still start what it finds) does not start.
	mustWriteFile(t, filepath.Join(umbrellaDir, ".foreman-kit", "agents.yaml"), "agents: [claude]\n")
	hub.BacklogChanged()
	if !waitUntil(10*time.Second, func() bool { return strings.Contains(log.String(), "dispatcher: .foreman-kit/agents.yaml: ") }) {
		t.Fatalf("the dispatcher does not say which file keeps it:\n%s", log.String())
	}
	const change = "waits-for-the-file"
	writeBacklogJSON(t, umbrellaDir, map[string]any{"change_name": change, "description": "Starts once the file is whole", "priority": "high"})
	// Woken again, it starts nothing, and does not say it twice.
	hub.BacklogChanged()
	time.Sleep(1500 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("the dispatcher called an agent %d time(s) on a file that lacks keys:\n%s", n, log.String())
	}
	if n := strings.Count(log.String(), "dispatcher: .foreman-kit/agents.yaml: "); n != 1 {
		t.Fatalf("the dispatcher said why it waits %d times, want once:\n%s", n, log.String())
	}
	if !strings.Contains(log.String(), "no change starts until it is mended") {
		t.Fatalf("the dispatcher does not say that nothing starts:\n%s", log.String())
	}

	// The file is whole again: the change is built.
	settingstest.WriteFile(t, umbrellaDir, "agents", "")
	hub.BacklogChanged()
	if !waitUntil(30*time.Second, func() bool { return inReadyQueue(ctx, st, change) }) {
		t.Fatalf("expected %s built once .foreman-kit/agents.yaml was whole again:\n%s", change, log.String())
	}
}
