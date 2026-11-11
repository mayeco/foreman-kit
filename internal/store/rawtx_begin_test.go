package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// rawTxBeginDriver wraps the SQLite driver so that, once armed, the next "BEGIN IMMEDIATE" runs and is
// then reported as canceled. The real driver does that when the context ends just as the statement
// starts; the wrapper makes it happen on demand. Like the real connection in that case, its connections
// look healthy to database/sql (no driver.Validator).
type rawTxBeginDriver struct {
	inner driver.Driver
	dsn   string
	armed *atomic.Bool
}

func (d rawTxBeginDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &rawTxBeginConn{Conn: c, armed: d.armed}, nil
}

func (d rawTxBeginDriver) Connect(context.Context) (driver.Conn, error) { return d.Open(d.dsn) }
func (d rawTxBeginDriver) Driver() driver.Driver                        { return d }

type rawTxBeginConn struct {
	driver.Conn
	armed *atomic.Bool
}

func (c *rawTxBeginConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	res, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err == nil && query == "BEGIN IMMEDIATE" && c.armed.CompareAndSwap(true, false) {
		return nil, context.Canceled
	}
	return res, err
}

func (c *rawTxBeginConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

// A BEGIN IMMEDIATE that opened its transaction and was reported as canceled must not leave the single
// writer connection inside that transaction: every later write of the process would run in it, never
// committed and invisible to everyone else, and no raw transaction could begin again.
func TestArrangeAgentsEndsATransactionWhoseBeginWasReportedCanceled(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	st, err := OpenAt(dbPath, "ws-rawtx-begin", dir)
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
	candidates := []string{"claude"}
	armed.Store(true)
	if _, err := st.ArrangeAgents(ctx, candidates, "in_order", 0, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want the BEGIN reported as canceled to fail ArrangeAgents with context.Canceled, got %v", err)
	}

	pid := os.Getpid()
	if _, err := st.ArrangeAgents(ctx, candidates, "in_order", pid, nil); err != nil {
		t.Fatalf("the writer connection was left inside the transaction of the BEGIN reported as canceled: %v", err)
	}
	// The claim is committed: the reader pool sees it.
	var n int
	if err := st.ReadDB().QueryRowContext(ctx, "SELECT count(*) FROM engine_claims WHERE claim_id = ?", pid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want the claim made after the failed BEGIN committed and visible to readers, found %d row(s)", n)
	}
}

// rawTxLateDoneContext turns done at its k-th Done call: the checks database/sql and the driver make
// before a statement see it alive, and the goroutine the driver starts to watch it during the statement
// sees it done at once. For the right k that is a cancellation landing as BEGIN IMMEDIATE starts, which
// a wall-clock cancellation hits once in thousands of attempts.
type rawTxLateDoneContext struct {
	k      int32
	calls  atomic.Int32
	open   chan struct{}
	closed chan struct{}
}

func newRawTxLateDoneContext(k int32) *rawTxLateDoneContext {
	c := &rawTxLateDoneContext{k: k, open: make(chan struct{}), closed: make(chan struct{})}
	close(c.closed)
	return c
}

func (c *rawTxLateDoneContext) Done() <-chan struct{} {
	if c.calls.Add(1) >= c.k {
		return c.closed
	}
	return c.open
}

func (c *rawTxLateDoneContext) Err() error {
	if c.calls.Load() >= c.k {
		return context.Canceled
	}
	return nil
}

func (c *rawTxLateDoneContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *rawTxLateDoneContext) Value(any) any               { return nil }

// The same with the real driver, for a cancellation that lands before BEGIN IMMEDIATE runs (BEGIN fails
// and opened nothing: the connection is discarded), as it starts, and after it (the next statement
// fails): the writer pool must always be able to begin the next transaction. Whether an attempt hits the
// interleaving that left the transaction open depends on scheduling (before beginRawTx it did a few times
// in a few thousand attempts on a busy machine), so the deterministic guard is the test above.
func TestArrangeAgentsCanceledAsItBeginsLeavesTheWriterUsable(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenAt(filepath.Join(dir, "state.db"), "ws-rawtx-begin-late", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	bg := context.Background()
	candidates := []string{"claude"}
	for k := int32(3); k <= 7; k++ {
		for i := 0; i < 40; i++ {
			_, _ = st.ArrangeAgents(newRawTxLateDoneContext(k), candidates, "in_order", 0, nil)
			if _, err := st.ArrangeAgents(bg, candidates, "in_order", 0, nil); err != nil {
				t.Fatalf("after an ArrangeAgents whose context turned done at its Done call %d (attempt %d), the writer pool cannot begin a transaction: %v", k, i, err)
			}
		}
	}
}
