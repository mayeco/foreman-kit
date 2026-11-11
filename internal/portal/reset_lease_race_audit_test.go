package portal

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// While a reset runs, a dispatcher must not be able to (re)acquire the change's worker lease,
// and ReclaimLease must never delete a lease that a live worker took over after the liveness check.
func TestAuditResetWorkerHoldsLeaseDuringReset(t *testing.T) {
	root := t.TempDir()
	st, err := store.OpenAt(filepath.Join(root, "log", "openspec.db"), "ws-reset-race-audit", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	key := store.WorkerLeaseKey("demo")

	// A dead worker's lease, as left behind by a crashed dispatcher.
	if _, err := st.AcquireLock(ctx, key, store.LockOptions{OwnerType: "worker", PID: 999997, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".worktrees", "worker-demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, _ := newTestServer(t, root, st)
	if _, err := srv.changes.Reset(ctx, "demo"); err != nil {
		t.Fatalf("reset of a dead worker failed: %v", err)
	}
	if rec, _ := st.GetLock(ctx, key); rec != nil {
		t.Fatalf("reset must release the lease it held, found %+v", rec)
	}

	// ReclaimLease: the stale owner died, then a live worker reclaimed the lease.
	if _, err := st.AcquireLock(ctx, key, store.LockOptions{OwnerType: "worker", PID: 999997, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireLock(ctx, key, store.LockOptions{OwnerType: "worker", PID: os.Getpid(), TTL: time.Hour}); err != nil {
		t.Fatalf("a live worker must reclaim a dead worker's lease: %v", err)
	}
	if err := st.ReleaseLock(ctx, key, 999997); err != nil {
		t.Fatal(err)
	}
	if rec, _ := st.GetLock(ctx, key); rec == nil || rec.PID != os.Getpid() {
		t.Fatalf("an owner-checked release of the dead PID must keep the live worker's lease, got %+v", rec)
	}
}
