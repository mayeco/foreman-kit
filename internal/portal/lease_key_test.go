package portal

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// TestResetWorkerRefusesWhileDispatcherLeaseIsAlive seeds the lease exactly as the dispatcher writes it
// (store.WorkerLeaseKey) and verifies that "Reset worker" refuses to delete a worktree under a live
// worker, and that the change catalog reports the change as in flight.
func TestResetWorkerRefusesWhileDispatcherLeaseIsAlive(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	ctx := context.Background()

	wt := filepath.Join(root, ".worktrees", "worker-live-change")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireLock(ctx, store.WorkerLeaseKey("live-change"), store.LockOptions{
		OwnerType: "worker",
		PID:       os.Getpid(),
		TTL:       10 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	// The daemon lock must never surface as a phantom change.
	if _, err := st.AcquireLock(ctx, "daemon", store.LockOptions{OwnerType: "daemon", PID: os.Getpid(), TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}

	code, body, raw, _ := doReq(t, srv, http.MethodPost, "/api/changes/live-change/reset", map[string]any{})
	if c, _ := errorOf(body); code != http.StatusConflict || c != "conflict" {
		t.Fatalf("expected 409 when resetting a change under a live worker lease, got %d: %s", code, raw)
	}
	if !isDir(wt) {
		t.Fatal("reset deleted the worktree of a live worker")
	}

	// The worker that holds the lease recorded its run, as a dispatcher does.
	if _, err := st.BeginChangeRun(ctx, "live-change", 1, wt, "feat/spec-live-change"); err != nil {
		t.Fatal(err)
	}
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/changes/live-change/reclaim", map[string]any{})
	if c, msg := errorOf(body); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, "alive") {
		t.Fatalf("expected the lease of a live worker not to be given back, got %d: %s", code, raw)
	}
	if lease, _ := st.GetLock(ctx, store.WorkerLeaseKey("live-change")); lease == nil || lease.PID != os.Getpid() {
		t.Fatalf("the lease of a live worker was given back: %+v", lease)
	}

	// The page of processes lists the lease with its change and as held, and the lock of the daemon
	// with no change at all.
	_, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/system/processes", nil)
	inFlight, sawDaemon := false, false
	locks, _ := data["locks"].([]any)
	for _, it := range locks {
		l, _ := it.(map[string]any)
		switch l["change_name"] {
		case "live-change":
			inFlight = l["stale"] == false
		case "daemon":
			sawDaemon = true
		}
	}
	if !inFlight || sawDaemon || len(locks) != 2 {
		t.Fatalf("expected the locks to list live-change as held and the daemon with no change (inFlight=%v sawDaemon=%v): %s", inFlight, sawDaemon, raw)
	}
}
