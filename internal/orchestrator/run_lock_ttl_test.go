package orchestrator

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

// runLockProbe reads the lock a one-shot run holds before and after one of its heartbeats.
type runLockProbe struct {
	once          sync.Once
	before, after *store.LockRecord
	waited        bool
}

// observe is called from inside the run (its agent or gate stand-in), while it holds the lock.
func (p *runLockProbe) observe(t *testing.T, ctx context.Context, st *store.Store, key string) {
	p.once.Do(func() {
		before, err := st.GetLock(ctx, key)
		if err != nil || before == nil {
			t.Errorf("%s lock not held while the run works: %v, %v", key, before, err)
			return
		}
		p.before = before
		// The heartbeat of a lock with the hard-coded 2h lease beats every 15s: stay past the first of
		// those beats, and past the heartbeats the run itself writes without a TTL.
		time.Sleep(16 * time.Second)
		p.after, _ = st.GetLock(ctx, key)
		p.waited = p.after != nil && p.after.HeartbeatAt > before.HeartbeatAt
	})
}

// check asserts the lease is locks.stale_after (3s) at acquisition and after a beat, and that the row
// goes stale for another process once a beat is missing for longer than that.
func (p *runLockProbe) check(t *testing.T, key string) {
	t.Helper()
	if p.before == nil || p.after == nil {
		t.Fatalf("the %s lock was never observed", key)
	}
	if !p.waited {
		t.Fatalf("the %s lock was never heartbeated: heartbeat_at stayed %d", key, p.before.HeartbeatAt)
	}
	const want = 3
	if p.before.TTLSeconds != want || p.after.TTLSeconds != want {
		t.Fatalf("%s ttl_seconds: at acquisition %d, after one heartbeat %d; want %d (locks.stale_after) both times",
			key, p.before.TTLSeconds, p.after.TTLSeconds, want)
	}
	hb := p.after.HeartbeatAt
	// The owner is this very process, alive: only the TTL can make the row stale.
	if !store.IsLockStale(*p.after, "", hb+want+1) {
		t.Fatalf("%s lock whose heartbeat is %ds old is not stale for another process", key, want+1)
	}
	if store.IsLockStale(*p.after, "", hb) {
		t.Fatalf("%s lock just heartbeated is stale", key)
	}
}

// A one-shot dispatch keeps the dispatcher lock's lease at locks.stale_after after its heartbeat: the
// heartbeat used to rewrite ttl_seconds with a hard-coded 2h.
func TestDispatcherRunKeepsLockTTLAtStaleAfterAcrossHeartbeats(t *testing.T) {
	settingstest.Give(t, settings.LocksStaleAfter, "3s") // heartbeat every second
	ctx := context.Background()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)
	writeBacklogJSON(t, umbrellaDir, map[string]any{"change_name": "run-lock-ttl-dispatch", "description": "Hold the lock a while", "priority": "high"})

	probe := &runLockProbe{}
	stub := continuousStubAgent()
	d, err := NewDispatcher(DispatcherOptions{Root: umbrellaDir, Store: st, MaxWorkers: 1, Out: io.Discard,
		AgentRunner: func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
			probe.observe(t, ctx, st, "dispatcher")
			return stub(ctx, req)
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Run(ctx); err != nil {
		t.Fatal(err)
	}
	probe.check(t, "dispatcher")
}

// A one-shot merge keeps the merger lock's lease at locks.stale_after after its heartbeat.
func TestMergerRunKeepsLockTTLAtStaleAfterAcrossHeartbeats(t *testing.T) {
	settingstest.Give(t, settings.LocksStaleAfter, "3s") // heartbeat every second
	ctx := context.Background()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)
	buildReadyManifest(t, ctx, umbrellaDir, st, "run-lock-ttl-merge")

	probe := &runLockProbe{}
	m, err := NewMerger(MergerOptions{Root: umbrellaDir, Store: st, Out: io.Discard,
		GateRunner: func(ctx context.Context, req GateRunRequest) (GateRunResult, error) {
			probe.observe(t, ctx, st, "merger")
			return GateRunResult{OK: true}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}
	probe.check(t, "merger")
}
