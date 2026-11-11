package store

import "context"

// RowsWritten returns how many rows the writer of this store has inserted, updated or deleted since
// the store was opened. It only grows, and it does not move while nothing is written: a test that
// asks the portal for its pages checks that reading them leaves it as it was.
func (s *Store) RowsWritten(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&n)
	return n, err
}
