package portal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// While a reset deletes a worktree, the worker lease must be held so a dispatcher cannot start
// the change in the directory being deleted.
func TestAuditResetWorkerLeaseHeldWhileDeleting(t *testing.T) {
	root := t.TempDir()
	st, err := store.OpenAt(filepath.Join(root, "log", "openspec.db"), "ws-reset-overlap-audit", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	key := store.WorkerLeaseKey("demo")

	wt := filepath.Join(root, ".worktrees", "worker-demo")
	for i := 0; i < 6000; i++ {
		dir := filepath.Join(wt, fmt.Sprintf("d%03d", i%120))
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d", i)), []byte("x"), 0o644)
	}
	srv, _ := newTestServer(t, root, st)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = srv.changes.Reset(ctx, "demo")
	}()

	sawDeleting := false
	for {
		select {
		case <-done:
			if !sawDeleting {
				t.Skip("the reset finished before the deletion could be observed")
			}
			return
		default:
		}
		entries, err := os.ReadDir(wt)
		if err != nil || len(entries) == 0 || len(entries) >= 120 {
			time.Sleep(200 * time.Microsecond)
			continue
		}
		sawDeleting = true
		// The worktree is half deleted: a dispatcher polling now must not get the lease.
		if _, err := st.AcquireLock(ctx, key, store.LockOptions{OwnerType: "worker", PID: os.Getpid(), TTL: time.Hour}); err == nil {
			_ = st.ReleaseLock(ctx, key, os.Getpid())
			<-done
			t.Fatalf("a dispatcher acquired %s while the reset was deleting the worktree (%d of 120 dirs left)", key, len(entries))
		}
	}
}
