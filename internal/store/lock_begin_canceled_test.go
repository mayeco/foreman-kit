package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// AcquireLock opens its transaction with a raw BEGIN IMMEDIATE. The driver reports a statement that ran
// while its context ended as canceled, so a BEGIN can fail after it opened the transaction. AcquireLock
// used to return there without ROLLBACK: the single writer connection went back to the pool inside that
// transaction, every later write of the process ran in it (never committed, invisible to every other
// process) and no lock could be acquired again ("cannot start a transaction within a transaction").
// Workers take their lease through AcquireLock under the dispatcher's context, which a daemon stop or a
// lost lock cancels.
func TestAcquireLockEndsATransactionWhoseBeginWasReportedCanceled(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	st, err := OpenAt(dbPath, "ws-lock-begin", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var armed atomic.Bool
	inner := st.db.Driver()
	if err := st.db.Close(); err != nil {
		t.Fatal(err)
	}
	st.db = sql.OpenDB(rawTxBeginDriver{inner: inner, dsn: "file:" + dbPath + "?_pragma=busy_timeout(15000)", armed: &armed})
	st.db.SetMaxOpenConns(1)

	ctx := context.Background()
	armed.Store(true)
	if _, err := st.AcquireLock(ctx, "dispatcher", LockOptions{TTL: time.Minute}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want the BEGIN reported as canceled to fail AcquireLock with context.Canceled, got %v", err)
	}

	// The failed attempt wrote nothing, and the writer connection can begin the next transaction.
	if _, err := st.AcquireLock(ctx, "dispatcher", LockOptions{TTL: time.Minute}); err != nil {
		t.Fatalf("the writer connection was left inside the transaction of the BEGIN reported as canceled: %v", err)
	}
	// The lock is committed: the reader pool, like every other process, sees it.
	var n int
	if err := st.ReadDB().QueryRowContext(ctx, "SELECT count(*) FROM locks WHERE lock_key = 'dispatcher'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want the lock taken after the failed BEGIN committed and visible to readers, found %d row(s)", n)
	}
	// A plain write on the writer pool commits on its own too.
	if err := st.ReleaseLock(ctx, "dispatcher", 0); err != nil {
		t.Fatal(err)
	}
	if err := st.ReadDB().QueryRowContext(ctx, "SELECT count(*) FROM locks WHERE lock_key = 'dispatcher'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("want the release committed and visible to readers, still found %d row(s)", n)
	}
}
