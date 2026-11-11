package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// VisualQAItem is a request to check something in the running application. Status is "pending",
// "in_progress", "dropped", or the verdict it was resolved with: "verified", "regression",
// "not-visual", "cannot-verify".
type VisualQAItem struct {
	ItemID      string `json:"id"`
	ChangeName  string `json:"change_name,omitempty"`
	What        string `json:"what"`
	Status      string `json:"status"`
	VerdictNote string `json:"verdict_note,omitempty"`
	CreatedAt   string `json:"created_at"`
	ResolvedAt  string `json:"resolved_at,omitempty"`
}

// normalizeVisualQAStatus maps a status to the ones the table holds; the verdict names of the
// file-based kit (passed, failed, skipped) become the ones in use.
func normalizeVisualQAStatus(status string) string {
	switch status {
	case "pending", "in_progress", "verified", "regression", "not-visual", "cannot-verify", "dropped":
		return status
	case "":
		return "pending"
	case "passed":
		return "verified"
	case "failed":
		return "regression"
	case "skipped":
		return "not-visual"
	}
	return "cannot-verify"
}

func visualQAItemID(itemID string) (int64, error) {
	id, err := strconv.ParseInt(itemID, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("store: %q is not a visual QA item", itemID)
	}
	return id, nil
}

func (s *Store) insertVisualQATx(ctx context.Context, tx *sql.Tx, id int64, item VisualQAItem) (int64, error) {
	now := s.nowMS()
	if err := s.ensureChangeTx(ctx, tx, item.ChangeName, now); err != nil {
		return 0, err
	}
	created := rfc3339ToMS(item.CreatedAt)
	if created == 0 {
		created = now
	}
	status := normalizeVisualQAStatus(item.Status)
	var resolved any
	if r := rfc3339ToMS(item.ResolvedAt); r > 0 {
		resolved = r
	}
	cycle := nullStr(ScopeFrom(ctx).CycleStamp)
	if cycle != nil {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM cycles WHERE stamp = ?`, cycle).Scan(&n); err != nil || n == 0 {
			cycle = nil
		}
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO visual_qa_items (id, change_name, what, status, verdict_note, cycle_stamp, created_at, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, nullInt(id), nullStr(item.ChangeName), item.What, status, nullStr(item.VerdictNote), cycle, created, resolved)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EnqueueVisualQA inserts a visual QA item under the id it carries, unless that id is taken. An item
// with no numeric id gets the next one.
func (s *Store) EnqueueVisualQA(ctx context.Context, item VisualQAItem) error {
	id, _ := strconv.ParseInt(item.ItemID, 10, 64)
	if id < 0 {
		id = 0
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := s.insertVisualQATx(ctx, tx, id, item)
		return err
	})
}

// EnqueueVisualQANext inserts item under the next id and returns it. The id is allocated by the
// insert itself, so concurrent requesters (CLI agents, the portal, the cycle) never get the same one.
func (s *Store) EnqueueVisualQANext(ctx context.Context, item VisualQAItem) (string, error) {
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.insertVisualQATx(ctx, tx, 0, item)
		return err
	})
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(id, 10), nil
}

// StartVisualQA marks an item as being checked by the session `note` names.
func (s *Store) StartVisualQA(ctx context.Context, itemID, note string) error {
	id, err := visualQAItemID(itemID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE visual_qa_items SET status = 'in_progress', verdict_note = ?, started_at = ? WHERE id = ?
	`, nullStr(note), s.nowMS(), id)
	return err
}

// RequeueVisualQA puts an item that was being checked back in the queue.
func (s *Store) RequeueVisualQA(ctx context.Context, itemID string) error {
	id, err := visualQAItemID(itemID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE visual_qa_items SET status = 'pending', verdict_note = NULL, started_at = NULL WHERE id = ?
	`, id)
	return err
}

// ResolveVisualQA closes a visual QA item with a verdict, or as dropped.
func (s *Store) ResolveVisualQA(ctx context.Context, itemID, status, note string) error {
	id, err := visualQAItemID(itemID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE visual_qa_items SET status = ?, verdict_note = ?, resolved_at = ? WHERE id = ?
	`, normalizeVisualQAStatus(status), nullStr(note), s.nowMS(), id)
	return err
}

// ListVisualQA returns the visual QA items, oldest first, filtered by optional status.
func (s *Store) ListVisualQA(ctx context.Context, status string) ([]VisualQAItem, error) {
	q := "SELECT id, change_name, what, status, verdict_note, created_at, resolved_at FROM visual_qa_items"
	var args []any
	if status != "" {
		q += " WHERE status = ?"
		args = append(args, status)
	}
	q += " ORDER BY created_at ASC, id ASC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []VisualQAItem
	for rows.Next() {
		var it VisualQAItem
		var id, created int64
		var chg, note sql.NullString
		var res sql.NullInt64
		if err := rows.Scan(&id, &chg, &it.What, &it.Status, &note, &created, &res); err != nil {
			return nil, err
		}
		it.ItemID = strconv.FormatInt(id, 10)
		it.ChangeName = chg.String
		it.VerdictNote = note.String
		it.CreatedAt = msToRFC3339(created)
		it.ResolvedAt = nullMSToRFC3339(res)
		out = append(out, it)
	}
	return out, rows.Err()
}
