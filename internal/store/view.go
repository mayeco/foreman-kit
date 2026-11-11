package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// View is a read transaction over the state database. Every read made through one View sees the rows
// as they were at one instant, so the counts and the lists of a page agree with each other. A View
// writes nothing. It is not safe for concurrent use: a page reads through it one query at a time.
type View struct {
	s   *Store
	q   reader
	now int64
}

// View runs fn with a read transaction over the database as it is now. The transaction ends when fn
// returns: a View must not be kept to read again afterwards.
func (s *Store) View(ctx context.Context, fn func(v *View) error) error {
	return s.view(ctx, nil, fn)
}

// view is View with every statement the View runs reported to trace, for the tests that check how
// the reads are planned.
func (s *Store) view(ctx context.Context, trace func(query string, args []any), fn func(v *View) error) error {
	tx, err := s.ReadDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	// A read transaction has nothing to commit: rolling it back releases its snapshot.
	defer func() { _ = tx.Rollback() }()

	// The snapshot of a deferred transaction is taken by its first read. Take it here, so the instant
	// the view reports is the one every later read sees.
	var rows int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspace`).Scan(&rows); err != nil {
		return err
	}
	return fn(&View{s: s, q: viewReader{tx: tx, trace: trace}, now: time.Now().UnixMilli()})
}

// At returns the instant the view shows: what is still open is measured up to it.
func (v *View) At() time.Time { return time.UnixMilli(v.now).UTC() }

// viewReader is the transaction of a View, as the reads take it.
type viewReader struct {
	tx    *sql.Tx
	trace func(query string, args []any)
}

func (r viewReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if r.trace != nil {
		r.trace(query, args)
	}
	return r.tx.QueryContext(ctx, query, args...)
}

func (r viewReader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if r.trace != nil {
		r.trace(query, args)
	}
	return r.tx.QueryRowContext(ctx, query, args...)
}

// Sizes of a page of a list. A caller that asks for none gets DefaultPageSize; none gets more than
// MaxPageSize.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// ErrBadCursor is returned by a paged read given a cursor that no page handed out.
var ErrBadCursor = errors.New("store: the page cursor is not valid")

func pageSize(limit int) int {
	switch {
	case limit <= 0:
		return DefaultPageSize
	case limit > MaxPageSize:
		return MaxPageSize
	}
	return limit
}

// idCursor reads the cursor of a list ordered by descending row id: the id of the last row of the
// page before. The first page starts from the largest id there is, so every page, the first one
// included, is a search below a key and never a walk of the table.
func idCursor(cursor string) (int64, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return math.MaxInt64, nil
	}
	id, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrBadCursor
	}
	return id, nil
}

// timeCursor reads the cursor of a list ordered by descending (instant, key), written by
// formatTimeCursor. The first page starts after every instant.
func timeCursor(cursor string) (int64, string, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return math.MaxInt64, "", nil
	}
	at, key, ok := strings.Cut(cursor, ".")
	ms, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil || ms < 0 || key == "" {
		return 0, "", ErrBadCursor
	}
	return ms, key, nil
}

func formatTimeCursor(ms int64, key string) string {
	return strconv.FormatInt(ms, 10) + "." + key
}

// placeholders returns "?,?,?" for n values (n is at least 1).
func placeholders(n int) string {
	if n <= 1 {
		return "?"
	}
	return strings.Repeat("?,", n-1) + "?"
}

// likePattern returns the LIKE pattern that finds text anywhere in a value, with the wildcards of
// text taken literally. It is used with ESCAPE '\'.
func likePattern(text string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(text) + "%"
}

// each runs a query and hands every row to scan, and closes the rows before it returns: a View has
// one connection, so a read is over before the next one starts.
func (v *View) each(ctx context.Context, scan func(scan func(dest ...any) error) error, query string, args ...any) error {
	rows, err := v.q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}
