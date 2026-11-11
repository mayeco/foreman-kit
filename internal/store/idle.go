package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// What ends an idle period.
const (
	// IdleEndedByActivity: the attempt worked again.
	IdleEndedByActivity = "activity"
	// IdleEndedByStall: the idle watchdog stopped the attempt at its limit.
	IdleEndedByStall = "stalled"
	// IdleEndedByExit: the attempt ended while it was idle (it finished, or was stopped for another reason).
	IdleEndedByExit = "exit"
)

// IdlePeriod is a stretch of an execution attempt in which the idle watchdog saw no activity: no file
// changed, nothing printed, no command of the agent using CPU.
type IdlePeriod struct {
	ID        int64  `json:"id"`
	AttemptID int64  `json:"attempt_id"`
	Engine    string `json:"engine"`
	// StartedAt is the last activity the watchdog saw before the period.
	StartedAt string `json:"started_at"`
	// LimitSeconds is the idle limit in force: the attempt is stopped once idle this long.
	LimitSeconds int    `json:"limit_seconds"`
	EndedAt      string `json:"ended_at,omitempty"`
	EndedBy      string `json:"ended_by,omitempty"`
	// Seconds is how long the period lasted, or has lasted so far while it is open.
	Seconds int `json:"seconds"`
}

// IdleAgent is an agent running now that has shown no activity for a while.
type IdleAgent struct {
	ExecID     string `json:"exec_id"`
	Kind       string `json:"kind"` // "worker", "cycle", "portal_run"
	ChangeName string `json:"change_name,omitempty"`
	CycleStamp string `json:"cycle_stamp,omitempty"`
	Phase      string `json:"phase,omitempty"`
	Engine     string `json:"engine"`
	// PID is the process that runs the execution (the orchestrator, not the agent CLI).
	PID int `json:"pid,omitempty"`
	// IdleSince is the last activity the watchdog saw.
	IdleSince string `json:"idle_since"`
	// IdleSeconds is the time since then.
	IdleSeconds int `json:"idle_seconds"`
	// LimitSeconds is the idle time at which the agent is stopped.
	LimitSeconds int `json:"limit_seconds"`
}

// attemptRef is what an event about an attempt says of it.
type attemptRef struct {
	execID string
	engine string
	change string
	phase  string
	runID  int64
}

func (s *Store) attemptRefTx(ctx context.Context, q querier, attemptID int64) (attemptRef, error) {
	var ref attemptRef
	var change, phase sql.NullString
	var runID sql.NullInt64
	err := q.QueryRowContext(ctx, `
		SELECT a.exec_id, a.engine, e.change_name, e.phase, e.change_run_id
		FROM execution_attempts a JOIN executions e ON e.exec_id = a.exec_id
		WHERE a.id = ?
	`, attemptID).Scan(&ref.execID, &ref.engine, &change, &phase, &runID)
	ref.change, ref.phase, ref.runID = change.String, phase.String, runID.Int64
	return ref, err
}

// BeginIdlePeriod records that a running attempt has shown no activity since the given moment and
// is stopped once that lasts limit, and announces it (agent.idle). An attempt has one open idle
// period at most: while it has one, or once it has ended, the call records nothing.
func (s *Store) BeginIdlePeriod(ctx context.Context, attemptID int64, since time.Time, limit time.Duration) error {
	limitSec := int64(limit / time.Second)
	if attemptID <= 0 || limitSec <= 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		start := since.UnixMilli()
		if since.IsZero() || start > now {
			start = now
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO attempt_idle_periods (attempt_id, started_at, limit_seconds)
			SELECT a.id, MAX(?, a.started_at), ? FROM execution_attempts a
			WHERE a.id = ? AND a.finished_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM attempt_idle_periods p WHERE p.attempt_id = a.id AND p.ended_at IS NULL)
		`, start, limitSec, attemptID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		ref, err := s.attemptRefTx(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		return s.emitEventTx(ctx, tx, EventInput{
			Event:       "agent.idle",
			Change:      ref.change,
			Engine:      ref.engine,
			Phase:       ref.phase,
			ExecID:      ref.execID,
			ChangeRunID: ref.runID,
			Payload: map[string]any{
				"idle_seconds":  (now - start + 500) / 1000,
				"limit_seconds": limitSec,
			},
		})
	})
}

// EndIdlePeriod closes the open idle period of an attempt that works again, and announces it
// (agent.resumed). It does nothing when the attempt has none.
func (s *Store) EndIdlePeriod(ctx context.Context, attemptID int64) error {
	if attemptID <= 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		idle, closed, err := s.closeIdlePeriodTx(ctx, tx, attemptID, IdleEndedByActivity, now)
		if err != nil || !closed {
			return err
		}
		ref, err := s.attemptRefTx(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		return s.emitEventTx(ctx, tx, EventInput{
			Event:       "agent.resumed",
			Change:      ref.change,
			Engine:      ref.engine,
			Phase:       ref.phase,
			ExecID:      ref.execID,
			ChangeRunID: ref.runID,
			Payload:     map[string]any{"idle_seconds": idle},
		})
	})
}

// closeIdlePeriodTx closes the open idle period of an attempt, if it has one, and returns how many
// seconds it lasted.
func (s *Store) closeIdlePeriodTx(ctx context.Context, q querier, attemptID int64, endedBy string, now int64) (int64, bool, error) {
	var id, started int64
	err := q.QueryRowContext(ctx, `
		SELECT id, started_at FROM attempt_idle_periods WHERE attempt_id = ? AND ended_at IS NULL
	`, attemptID).Scan(&id, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE attempt_idle_periods SET ended_at = ?, ended_by = ? WHERE id = ? AND ended_at IS NULL
	`, now, endedBy, id); err != nil {
		return 0, false, err
	}
	return (now - started + 500) / 1000, true, nil
}

// ListIdleAgents returns the agents that are idle now (the open idle period of an attempt still
// running), the one idle for longest first. An agent whose orchestrator died on this host is left
// out: nothing watches it any more, and the next dispatcher stops it as an orphan.
func (s *Store) ListIdleAgents(ctx context.Context) ([]IdleAgent, error) {
	return s.listIdleAgents(ctx, s.ReadDB(), time.Now().UnixMilli())
}

func (s *Store) listIdleAgents(ctx context.Context, q reader, now int64) ([]IdleAgent, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT exec_id, kind, change_name, cycle_stamp, phase, engine, pid, host, idle_since, limit_seconds
		FROM v_idle_agents ORDER BY idle_since, idle_period_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IdleAgent
	for rows.Next() {
		var a IdleAgent
		var change, cycle, phase, host sql.NullString
		var pid sql.NullInt64
		var since int64
		if err := rows.Scan(&a.ExecID, &a.Kind, &change, &cycle, &phase, &a.Engine, &pid, &host, &since, &a.LimitSeconds); err != nil {
			return nil, err
		}
		a.ChangeName, a.CycleStamp, a.Phase, a.PID = change.String, cycle.String, phase.String, int(pid.Int64)
		if a.PID > 0 && (host.String == "" || host.String == s.host) && !IsPIDAlive(a.PID) {
			continue
		}
		a.IdleSince = msToRFC3339(since)
		if now > since {
			a.IdleSeconds = int((now - since) / 1000)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const idlePeriodColumns = `p.id, p.attempt_id, a.engine, p.started_at, p.limit_seconds, p.ended_at, p.ended_by`

// scanIdlePeriod reads an idle period; one still open has lasted until now.
func scanIdlePeriod(scan func(dest ...any) error, now int64) (IdlePeriod, error) {
	var p IdlePeriod
	var started int64
	var ended sql.NullInt64
	var endedBy sql.NullString
	err := scan(&p.ID, &p.AttemptID, &p.Engine, &started, &p.LimitSeconds, &ended, &endedBy)
	p.StartedAt = msToRFC3339(started)
	p.EndedAt = nullMSToRFC3339(ended)
	p.EndedBy = endedBy.String
	until := now
	if ended.Valid {
		until = ended.Int64
	}
	if until > started {
		p.Seconds = int((until - started) / 1000)
	}
	return p, err
}

// ListIdlePeriods returns the idle periods of an execution's attempts, in order.
func (s *Store) ListIdlePeriods(ctx context.Context, execID string) ([]IdlePeriod, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `
		SELECT `+idlePeriodColumns+`
		FROM attempt_idle_periods p JOIN execution_attempts a ON a.id = p.attempt_id
		WHERE a.exec_id = ? ORDER BY p.id
	`, execID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now().UnixMilli()
	var out []IdlePeriod
	for rows.Next() {
		p, err := scanIdlePeriod(rows.Scan, now)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
