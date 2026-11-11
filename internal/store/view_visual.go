package store

import (
	"context"
	"database/sql"
	"strconv"
)

// VisualQAStatuses are the statuses of a check in a browser, in the order one goes through them:
// asked for, taken by a session, and then how it ended.
var VisualQAStatuses = []string{"pending", "in_progress", "verified", "regression", "not-visual", "cannot-verify", "dropped"}

// VisualQACheck is a check in a browser with everything the registry has of it.
type VisualQACheck struct {
	VisualQAItem
	// CycleStamp is the cycle that asked for it, "" when a person or a merge did.
	CycleStamp string `json:"cycle_stamp,omitempty"`
	// StartedAt is when a session took it.
	StartedAt string `json:"started_at,omitempty"`
}

// VisualQAPage is one page of the checks in a browser, the latest first.
type VisualQAPage struct {
	Items []VisualQACheck `json:"items"`
	// Next is the cursor of the page that follows, "" when this one is the last.
	Next string `json:"next,omitempty"`
}

const visualQACheckColumns = `id, change_name, what, status, verdict_note, cycle_stamp, created_at, started_at, resolved_at`

func scanVisualQACheck(scan func(dest ...any) error) (VisualQACheck, error) {
	var c VisualQACheck
	var id, created int64
	var change, note, cycle sql.NullString
	var started, resolved sql.NullInt64
	err := scan(&id, &change, &c.What, &c.Status, &note, &cycle, &created, &started, &resolved)
	c.ItemID = strconv.FormatInt(id, 10)
	c.ChangeName, c.VerdictNote, c.CycleStamp = change.String, note.String, cycle.String
	c.CreatedAt = msToRFC3339(created)
	c.StartedAt = nullMSToRFC3339(started)
	c.ResolvedAt = nullMSToRFC3339(resolved)
	return c, err
}

// VisualQACheck returns the check in a browser with that id, nil when there is none.
func (v *View) VisualQACheck(ctx context.Context, itemID string) (*VisualQACheck, error) {
	id, err := strconv.ParseInt(itemID, 10, 64)
	if err != nil || id <= 0 {
		return nil, nil
	}
	c, err := scanVisualQACheck(v.q.QueryRowContext(ctx, `SELECT `+visualQACheckColumns+` FROM visual_qa_items WHERE id = ?`, id).Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListVisualQAPage returns one page of the checks in a browser, the latest first, paged by cursor
// over their id. status keeps the checks in one status; every status when it is empty.
func (v *View) ListVisualQAPage(ctx context.Context, status, before string, limit int) (VisualQAPage, error) {
	page := VisualQAPage{Items: []VisualQACheck{}}
	below, err := idCursor(before)
	if err != nil {
		return page, err
	}
	limit = pageSize(limit)
	query := `SELECT ` + visualQACheckColumns + ` FROM visual_qa_items WHERE id < ?`
	args := []any{below}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit+1)
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		c, err := scanVisualQACheck(scan)
		if err == nil {
			page.Items = append(page.Items, c)
		}
		return err
	}, query, args...)
	if err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = page.Items[limit-1].ItemID
	}
	return page, nil
}

// CountVisualQAByStatus returns how many checks in a browser are in each status. Every status of
// VisualQAStatuses has an entry. Each count is a search of the index of its status.
func (v *View) CountVisualQAByStatus(ctx context.Context) (map[string]int, error) {
	out := make(map[string]int, len(VisualQAStatuses))
	for _, status := range VisualQAStatuses {
		var n int
		if err := v.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM visual_qa_items WHERE status = ?`, status).Scan(&n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, nil
}

// VisualSession is the visual QA session in progress: the lock it holds until it ends or expires.
type VisualSession struct {
	// Stamp names the session; PID is the process that opened it, which may be gone: a session
	// outlives the command that begins it.
	Stamp     string `json:"stamp"`
	PID       int    `json:"pid,omitempty"`
	StartedAt string `json:"started_at"`
	// ExpiresAt is when the session stops counting as open if nobody ends it.
	ExpiresAt string `json:"expires_at"`
}

// VisualSession returns the visual QA session in progress, nil when none is open. A session whose
// time has run out is not open: the next one that begins takes its place.
func (v *View) VisualSession(ctx context.Context) (*VisualSession, error) {
	var pid int
	var stamp sql.NullString
	var acquired, ttl int64
	err := v.q.QueryRowContext(ctx, `SELECT pid, stamp, acquired_at, ttl_seconds FROM locks WHERE lock_key = 'visual_qa'`).Scan(&pid, &stamp, &acquired, &ttl)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	expires := acquired + ttl*1000
	if expires <= v.now {
		return nil, nil
	}
	return &VisualSession{Stamp: stamp.String, PID: pid, StartedAt: msToRFC3339(acquired), ExpiresAt: msToRFC3339(expires)}, nil
}
