package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
)

// What started a cycle (Cycle.Trigger).
const (
	// CycleTriggerStartup: the daemon, as it came up.
	CycleTriggerStartup = "startup"
	// CycleTriggerInterval: the daemon's tick (settings.CycleEvery).
	CycleTriggerInterval = "interval"
	// CycleTriggerOutOfWork: the daemon, before its tick, because a worker was free with nothing to
	// start (settings.CycleEveryWhenOutOfWork).
	CycleTriggerOutOfWork = "out_of_work"
	// CycleTriggerManual: someone asked for it, from the command line, a scheduler or the portal.
	CycleTriggerManual = "manual"
)

// cycleTrigger returns trigger when it is one of the CycleTrigger* values, and "" otherwise.
func cycleTrigger(trigger string) string {
	switch trigger {
	case CycleTriggerStartup, CycleTriggerInterval, CycleTriggerOutOfWork, CycleTriggerManual:
		return trigger
	}
	return ""
}

// Cycle is one run of the autonomous cycle.
type Cycle struct {
	Stamp string `json:"stamp"`
	PID   int    `json:"pid,omitempty"`
	Host  string `json:"host,omitempty"`
	// Trigger is what started the cycle, a CycleTrigger* value; "" for a cycle recorded before the
	// kit kept it.
	Trigger       string `json:"trigger,omitempty"`
	Outcome       string `json:"outcome"`
	Strategy      string `json:"strategy,omitempty"`
	JudgeEngine   string `json:"judge_engine,omitempty"`
	UmbrellaSHA   string `json:"umbrella_sha,omitempty"`
	FindingsCount int    `json:"findings_count"`
	ReportPath    string `json:"report_path,omitempty"`
	LogPath       string `json:"log_path,omitempty"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at,omitempty"`
}

// BeginCycle records that this process starts the cycle named by c.Stamp. A cycle that is already on
// record keeps what it has and takes what c adds.
func (s *Store) BeginCycle(ctx context.Context, c Cycle) error {
	if c.Stamp == "" {
		return errors.New("store: a cycle needs a stamp")
	}
	judge := normalizeEngine(c.JudgeEngine)
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.ensureEngineTx(ctx, tx, judge); err != nil {
			return err
		}
		pid := c.PID
		if pid <= 0 {
			pid = os.Getpid()
		}
		started := rfc3339ToMS(c.StartedAt)
		if started == 0 {
			started = s.nowMS()
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO cycles (stamp, pid, host, trigger, outcome, strategy, judge_engine, umbrella_sha, report_path, log_path, started_at)
			VALUES (?, ?, ?, ?, 'running', ?, ?, ?, ?, ?, ?)
			ON CONFLICT(stamp) DO UPDATE SET
				pid = excluded.pid,
				host = excluded.host,
				trigger = COALESCE(excluded.trigger, cycles.trigger),
				strategy = COALESCE(excluded.strategy, cycles.strategy),
				judge_engine = COALESCE(excluded.judge_engine, cycles.judge_engine),
				umbrella_sha = COALESCE(excluded.umbrella_sha, cycles.umbrella_sha),
				report_path = COALESCE(excluded.report_path, cycles.report_path),
				log_path = COALESCE(excluded.log_path, cycles.log_path)
		`, c.Stamp, pid, s.host, nullStr(cycleTrigger(c.Trigger)), nullStr(c.Strategy), nullStr(judge), nullStr(c.UmbrellaSHA), nullStr(c.ReportPath), nullStr(c.LogPath), started)
		return err
	})
}

// UpdateCycle records what a running cycle learned about itself: the strategy it chose, the engine
// that judged, the commit it ran on, where its report is, how many findings it has. Empty fields and
// a negative FindingsCount leave what the cycle has.
func (s *Store) UpdateCycle(ctx context.Context, c Cycle) error {
	judge := normalizeEngine(c.JudgeEngine)
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.ensureEngineTx(ctx, tx, judge); err != nil {
			return err
		}
		var findings any
		if c.FindingsCount >= 0 {
			findings = c.FindingsCount
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE cycles SET
				strategy = COALESCE(?, strategy),
				judge_engine = COALESCE(?, judge_engine),
				umbrella_sha = COALESCE(?, umbrella_sha),
				report_path = COALESCE(?, report_path),
				log_path = COALESCE(?, log_path),
				findings_count = COALESCE(?, findings_count)
			WHERE stamp = ?
		`, nullStr(c.Strategy), nullStr(judge), nullStr(c.UmbrellaSHA), nullStr(c.ReportPath), nullStr(c.LogPath), findings, c.Stamp)
		return err
	})
}

// FinishCycle records how a cycle ended. finishedAt is an RFC 3339 instant, or "" for now. A cycle
// the kit has no record of (one of the file-based kit) is recorded as finished at that instant.
func (s *Store) FinishCycle(ctx context.Context, stamp, outcome, finishedAt string) error {
	if stamp == "" {
		return nil
	}
	if outcome == "" || outcome == "running" {
		outcome = "done"
	}
	at := s.msOrNow(finishedAt)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cycles (stamp, host, outcome, started_at, finished_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(stamp) DO UPDATE SET outcome = excluded.outcome, finished_at = excluded.finished_at
	`, stamp, s.host, outcome, at, at)
	return err
}

// GetCycle returns the cycle named by stamp, or nil when there is none.
func (s *Store) GetCycle(ctx context.Context, stamp string) (*Cycle, error) {
	cycles, err := s.listCycles(ctx, ` WHERE stamp = ?`, stamp)
	if err != nil || len(cycles) == 0 {
		return nil, err
	}
	return &cycles[0], nil
}

// ListCycles returns the cycles on record, newest first, at most limit when it is positive.
func (s *Store) ListCycles(ctx context.Context, limit int) ([]Cycle, error) {
	if limit > 0 {
		return s.listCycles(ctx, ` ORDER BY started_at DESC, stamp DESC LIMIT ?`, limit)
	}
	return s.listCycles(ctx, ` ORDER BY started_at DESC, stamp DESC`)
}

func (s *Store) listCycles(ctx context.Context, tail string, args ...any) ([]Cycle, error) {
	return listCyclesOn(ctx, s.ReadDB(), tail, args...)
}

// listCyclesOn reads cycles through q: the pool of the store, or the snapshot of a View.
func listCyclesOn(ctx context.Context, q reader, tail string, args ...any) ([]Cycle, error) {
	cycles, _, err := listCyclesTimed(ctx, q, tail, args...)
	return cycles, err
}

// cycleInstants are the instants a cycle began and ended, in milliseconds (ended is 0 for one that
// has not): a Cycle tells them to the second.
type cycleInstants struct{ started, ended int64 }

// listCyclesTimed is listCyclesOn with the instants of each cycle.
func listCyclesTimed(ctx context.Context, q reader, tail string, args ...any) ([]Cycle, []cycleInstants, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT stamp, pid, host, trigger, outcome, strategy, judge_engine, umbrella_sha, findings_count, report_path, log_path, started_at, finished_at
		FROM cycles`+tail, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []Cycle
	var at []cycleInstants
	for rows.Next() {
		var c Cycle
		var pid, finished sql.NullInt64
		var host, trigger, strategy, judge, sha, report, logPath sql.NullString
		var started int64
		if err := rows.Scan(&c.Stamp, &pid, &host, &trigger, &c.Outcome, &strategy, &judge, &sha, &c.FindingsCount, &report, &logPath, &started, &finished); err != nil {
			return nil, nil, err
		}
		c.PID = int(pid.Int64)
		c.Trigger = trigger.String
		c.Host, c.Strategy, c.JudgeEngine, c.UmbrellaSHA = host.String, strategy.String, judge.String, sha.String
		c.ReportPath, c.LogPath = report.String, logPath.String
		c.StartedAt = msToRFC3339(started)
		c.FinishedAt = nullMSToRFC3339(finished)
		out = append(out, c)
		at = append(at, cycleInstants{started: started, ended: finished.Int64})
	}
	return out, at, rows.Err()
}

// ChangesQueuedByCycle returns the changes the cycle stamp queued, oldest first: the ones it added to
// the backlog (RecordCycleWork), whether or not the main checkout lists them yet.
func (s *Store) ChangesQueuedByCycle(ctx context.Context, stamp string) ([]string, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `
		SELECT change_name FROM changes WHERE cycle_stamp = ? ORDER BY COALESCE(queued_at, first_seen_at), change_name
	`, stamp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
