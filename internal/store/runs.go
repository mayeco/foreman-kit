package store

import (
	"context"
	"database/sql"
	"os"
)

// DispatcherRun is one run of the dispatcher, from dispatcher.start to dispatcher.finish.
type DispatcherRun struct {
	ID          int64  `json:"id"`
	PID         int    `json:"pid"`
	Host        string `json:"host"`
	Mode        string `json:"mode"` // "once" or "continuous"
	Workers     int    `json:"workers"`
	BacklogSize int    `json:"backlog_size"`
	EndReason   string `json:"end_reason,omitempty"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
}

// BeginDispatcherRun records that this process starts dispatching and returns the run's id.
func (s *Store) BeginDispatcherRun(ctx context.Context, mode string, workers, backlogSize int) (int64, error) {
	if mode != "continuous" {
		mode = "once"
	}
	if workers < 1 {
		workers = 1
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO dispatcher_runs (pid, host, mode, workers, backlog_size, started_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, os.Getpid(), s.host, mode, workers, backlogSize, s.nowMS())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishDispatcherRun records that a dispatcher run ended and why.
func (s *Store) FinishDispatcherRun(ctx context.Context, id int64, endReason string) error {
	if id <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE dispatcher_runs SET finished_at = ?, end_reason = ? WHERE id = ? AND finished_at IS NULL
	`, s.nowMS(), nullStr(endReason), id)
	return err
}

// ChangeRun is one pass of a worker over a change, from change.start to change.finished.
type ChangeRun struct {
	ID               int64  `json:"id"`
	ChangeName       string `json:"change_name"`
	DispatcherRunID  int64  `json:"dispatcher_run_id,omitempty"`
	PID              int    `json:"pid"`
	Host             string `json:"host"`
	Slot             int    `json:"slot"`
	Worktree         string `json:"worktree"`
	Branch           string `json:"branch"`
	Origin           string `json:"origin"` // "backlog", "retry" or "rebuild"
	RetryOfBlockID   int64  `json:"retry_of_block_id,omitempty"`
	RebuildRequestID int64  `json:"rebuild_request_id,omitempty"`
	Result           string `json:"result,omitempty"`
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at,omitempty"`
}

// Results of a change run.
var changeRunResults = map[string]bool{
	"ready": true, "blocked": true, "held": true, "deferred": true, "paused": true, "stopped": true, "lost": true,
}

// BeginChangeRun records that a worker of this process starts on change, and returns the run's id.
// The caller holds the change's lease: a run of that change left open by an earlier owner (a process
// that died, a lease taken over) is closed as lost in the same transaction, since a change has at
// most one open run. Whether the run is a retry of a block or a rebuild the merger asked for is read
// from the change's quarantine.
func (s *Store) BeginChangeRun(ctx context.Context, change string, slot int, worktree, branch string) (int64, error) {
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		if err := s.ensureChangeTx(ctx, tx, change, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE change_runs SET result = 'lost', finished_at = ? WHERE change_name = ? AND finished_at IS NULL
		`, now, change); err != nil {
			return err
		}
		q, err := s.loadQuarantineTx(ctx, tx, change)
		if err != nil {
			return err
		}
		origin := "backlog"
		var retryOf, rebuildOf int64
		if pending := q.pendingRequest(); pending != nil {
			origin, rebuildOf = "rebuild", pending.id
		} else if last := q.lastRetried(); last != nil && !last.retryTold {
			origin, retryOf = "retry", last.id
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO change_runs (change_name, dispatcher_run_id, pid, host, slot, worktree, branch,
			                         origin, retry_of_block_id, rebuild_request_id, started_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, change, nullInt(ScopeFrom(ctx).DispatcherRunID), os.Getpid(), s.host, slot, worktree, branch,
			origin, nullInt(retryOf), nullInt(rebuildOf), now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// FinishChangeRun records how a change run ended. A run that was already closed (as lost, by the
// worker that took the change over) keeps the result it has.
func (s *Store) FinishChangeRun(ctx context.Context, id int64, result string) error {
	if id <= 0 {
		return nil
	}
	if !changeRunResults[result] {
		result = "stopped"
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE change_runs SET result = ?, finished_at = ? WHERE id = ? AND finished_at IS NULL
	`, result, s.nowMS(), id)
	return err
}

const changeRunColumns = `r.id, r.change_name, r.dispatcher_run_id, r.pid, r.host, r.slot, r.worktree, r.branch, r.origin,
	r.retry_of_block_id, r.rebuild_request_id, r.result, r.started_at, r.finished_at`

func scanChangeRun(scan func(dest ...any) error) (ChangeRun, error) {
	var r ChangeRun
	var dispID, retryOf, rebuildOf, finished sql.NullInt64
	var result sql.NullString
	var started int64
	err := scan(&r.ID, &r.ChangeName, &dispID, &r.PID, &r.Host, &r.Slot, &r.Worktree, &r.Branch, &r.Origin,
		&retryOf, &rebuildOf, &result, &started, &finished)
	r.DispatcherRunID = dispID.Int64
	r.RetryOfBlockID = retryOf.Int64
	r.RebuildRequestID = rebuildOf.Int64
	r.Result = result.String
	r.StartedAt = msToRFC3339(started)
	r.FinishedAt = nullMSToRFC3339(finished)
	return r, err
}

// ListChangeRuns returns the runs of a change, newest first (every change when change is empty).
func (s *Store) ListChangeRuns(ctx context.Context, change string, limit int) ([]ChangeRun, error) {
	query := `SELECT ` + changeRunColumns + ` FROM change_runs r`
	var args []any
	if change != "" {
		query += ` WHERE r.change_name = ?`
		args = append(args, change)
	}
	query += ` ORDER BY r.id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.ReadDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChangeRun
	for rows.Next() {
		r, err := scanChangeRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetDispatcherRunBacklog records how many changes the backlog held when a dispatcher run started.
func (s *Store) SetDispatcherRunBacklog(ctx context.Context, id int64, backlogSize int) error {
	if id <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE dispatcher_runs SET backlog_size = ? WHERE id = ?`, backlogSize, id)
	return err
}
