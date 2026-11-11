package cycle

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// Progress reports a process lock only while its owner holds it. A live PID is not enough: a row whose
// heartbeat is older than its TTL may be taken by another process (store.IsLockStale), and a PID the
// system reused for a later process says nothing about the lock. Both read as no process.
func TestProgressIgnoresStaleLocksOfLivePIDs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := openTestStore(t, root)

	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	pid := holder.Process.Pid

	keys := map[string]string{"cycle": "cycle_judging_pid", "dispatch": "dispatch_pid", "dispatcher": "dispatcher_pid", "merger": "merger_pid"}
	for lock := range keys {
		if _, err := st.AcquireLock(ctx, lock, store.LockOptions{OwnerType: lock, PID: pid, TTL: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	got := progressNow(t, ctx, root, st)
	for lock, key := range keys {
		if got[key] != pid {
			t.Fatalf("%s: want pid %d while the lock is live, got %v", lock, pid, got[key])
		}
	}

	// The heartbeat expired: the owner lives (a laptop asleep), but the lock is up for the taking.
	old := time.Now().UTC().Add(-time.Hour).Unix()
	if _, err := st.DB().Exec(`UPDATE locks SET heartbeat_at = ? * 1000 WHERE lock_key IN ('cycle', 'dispatch', 'dispatcher', 'merger')`, old); err != nil {
		t.Fatal(err)
	}
	got = progressNow(t, ctx, root, st)
	for lock, key := range keys {
		if got[key] != nil {
			t.Errorf("%s: an expired lock of a live PID was reported as held: %s = %v", lock, key, got[key])
		}
	}
	if got["dispatcher_mode"] != nil {
		t.Errorf("dispatcher_mode reported for an expired dispatcher lock: %v", got["dispatcher_mode"])
	}

	// The PID was reused: the row was last written (acquired and refreshed) a day before the process that
	// now has the PID started. A row refreshed since then belongs to that process: only an owner refreshes.
	day := time.Now().UTC().Add(-24 * time.Hour).Unix()
	if _, err := st.DB().Exec(`UPDATE locks SET acquired_at = ? * 1000, heartbeat_at = ? * 1000, ttl_seconds = 315360000 WHERE lock_key IN ('cycle', 'dispatch', 'dispatcher', 'merger')`, day, day); err != nil {
		t.Fatal(err)
	}
	got = progressNow(t, ctx, root, st)
	for lock, key := range keys {
		if got[key] != nil {
			t.Errorf("%s: a lock whose PID was reused was reported as held: %s = %v", lock, key, got[key])
		}
	}
}

func progressNow(t *testing.T, ctx context.Context, root string, st *store.Store) map[string]any {
	t.Helper()
	res, err := Progress(ctx, root, st, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	now, ok := res["dashboard"].(map[string]any)
	if !ok {
		t.Fatalf("Progress returned no now section: %v", res)
	}
	return now
}
