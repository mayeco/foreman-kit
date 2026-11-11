package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestRollbackRawTxWithCanceledContext reproduces a raw BEGIN IMMEDIATE transaction abandoned after
// the caller's context was canceled (as AcquireLock and ArrangeAgents do on error paths) and verifies
// that the single writer connection comes back clean: the uncommitted row is gone and a new
// transaction can begin.
func TestRollbackRawTxWithCanceledContext(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenAt(filepath.Join(dir, "state.db"), "ws-rawtx", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO locks (lock_key, owner_type, pid, host, acquired_at, heartbeat_at, ttl_seconds, metadata_json)
		VALUES ('abandoned', 'worker', 1, 'host', 1, 1, 60, '{}')
	`); err != nil {
		t.Fatal(err)
	}
	cancel()
	rollbackRawTx(ctx, conn)
	_ = conn.Close()

	bg := context.Background()
	var n int
	if err := st.db.QueryRowContext(bg, "SELECT count(*) FROM locks WHERE lock_key = 'abandoned'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected the abandoned transaction to be rolled back, found %d uncommitted row(s) on the writer pool", n)
	}
	if _, err := st.AcquireLock(bg, "after-abandoned-tx", LockOptions{}); err != nil {
		t.Fatalf("expected AcquireLock to start a fresh transaction on the writer pool, got %v", err)
	}
}

// TestAcquireLockWithCanceledContextLeavesWriterUsable verifies that a canceled context passed to
// AcquireLock cannot leave the writer pool inside an open transaction.
func TestAcquireLockWithCanceledContextLeavesWriterUsable(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenAt(filepath.Join(dir, "state.db"), "ws-rawtx-2", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = st.AcquireLock(ctx, "canceled", LockOptions{})

	bg := context.Background()
	if _, err := st.AcquireLock(bg, "fresh", LockOptions{}); err != nil {
		t.Fatalf("expected AcquireLock to succeed after a canceled attempt, got %v", err)
	}
	if rec, err := st.GetLock(bg, "fresh"); err != nil || rec == nil {
		t.Fatalf("expected the lock acquired after a canceled attempt to be committed, got rec=%v err=%v", rec, err)
	}
}
