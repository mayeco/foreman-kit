package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"time"
)

// rawTxRollbackTimeout bounds the ROLLBACK issued when a raw transaction is abandoned.
const rawTxRollbackTimeout = 10 * time.Second

// beginRawTx opens a transaction on conn with a raw "BEGIN IMMEDIATE".
//
// A BEGIN that reports an error may have opened the transaction all the same: the driver reports
// ctx.Err() for a statement that ran to completion when ctx ended meanwhile. When the cancellation lands
// just before the statement starts, it interrupts nothing (SQLite drops an interrupt delivered before a
// statement starts) and the connection then looks healthy to database/sql, which puts it back in the
// pool. A caller that returned without ROLLBACK would leave the single writer connection inside that
// transaction (see rollbackRawTx). So a BEGIN that fails is cleaned up like an abandoned transaction:
// ROLLBACK ends the transaction if there is one and, if there is none, fails, which discards the
// connection.
func beginRawTx(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		rollbackRawTx(ctx, conn)
		return err
	}
	return nil
}

// rollbackRawTx ends a transaction that was opened with a raw "BEGIN IMMEDIATE" on conn.
//
// database/sql does not know about raw transactions, so it would return conn to the pool even if the
// transaction were still open. ROLLBACK therefore runs on a context detached from the caller's
// cancellation: with an already-canceled ctx the driver never executes it, and every later statement
// on the single writer connection would silently run inside the abandoned transaction (and the next
// BEGIN would fail with "cannot start a transaction within a transaction"). If ROLLBACK still fails,
// the connection is marked bad so database/sql closes it; closing an SQLite connection rolls back any
// transaction it holds.
func rollbackRawTx(ctx context.Context, conn *sql.Conn) {
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rawTxRollbackTimeout)
	defer cancel()
	if _, err := conn.ExecContext(rbCtx, "ROLLBACK"); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}
