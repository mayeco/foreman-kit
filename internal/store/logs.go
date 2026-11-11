package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"time"
)

// Execution is one invocation the kit ran and kept the output of: the agent of a worker's PLAN or
// BUILD, an autonomous cycle, a run or a job started from the portal.
type Execution struct {
	ExecID     string `json:"exec_id"`
	Kind       string `json:"kind"` // "worker", "cycle", "portal_run", "portal_job"
	ChangeName string `json:"change_name,omitempty"`
	// Agent is the engine to show for the execution: the one that did the work once it is known,
	// the engines asked for until then. Written, it is taken as the engines asked for.
	Agent        string `json:"agent,omitempty"`
	Phase        string `json:"phase,omitempty"`
	Status       string `json:"status"` // "running", "succeeded", "failed", "stopped", "timed_out"
	PID          int    `json:"pid,omitempty"`
	ExitCode     int    `json:"exit_code,omitempty"`
	PromptText   string `json:"prompt_text,omitempty"`
	MetadataJSON string `json:"metadata_json,omitempty"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at,omitempty"`

	Role             string `json:"role,omitempty"`
	EnginesRequested string `json:"engines_requested,omitempty"`
	EngineUsed       string `json:"engine_used,omitempty"`
	PromptVersion    string `json:"prompt_version,omitempty"`
	MemoryVersion    string `json:"memory_version,omitempty"`
	ChangeRunID      int64  `json:"change_run_id,omitempty"`
	CycleStamp       string `json:"cycle_stamp,omitempty"`
	ParentExecID     string `json:"parent_exec_id,omitempty"`
	ParentRelation   string `json:"parent_relation,omitempty"`
	JobKind          string `json:"job_kind,omitempty"`
	CommandText      string `json:"command_text,omitempty"`
	Workdir          string `json:"workdir,omitempty"`
	Summary          string `json:"summary,omitempty"`
	Host             string `json:"host,omitempty"`
	// LogPrunedAt is when the execution's output was removed by retention ("" while it is kept).
	LogPrunedAt string `json:"log_pruned_at,omitempty"`
}

// ExecutionAttempt is one try of one engine inside an execution.
type ExecutionAttempt struct {
	ID          int64  `json:"id"`
	ExecID      string `json:"exec_id"`
	AttemptNo   int    `json:"attempt_no"`
	Engine      string `json:"engine"`
	TryNo       int    `json:"try_no"`
	ExitCode    int    `json:"exit_code"`
	Outcome     string `json:"outcome,omitempty"`
	IdleSeconds int    `json:"idle_seconds,omitempty"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
}

// Outcomes of an execution attempt.
const (
	AttemptOK              = "ok"
	AttemptError           = "error"
	AttemptTimeout         = "timeout"
	AttemptIdleStalled     = "idle_stalled"
	AttemptQuotaExceeded   = "quota_exceeded"
	AttemptAuthFailed      = "auth_failed"
	AttemptNotInstalled    = "not_installed"
	AttemptContextOverflow = "context_overflow"
	AttemptTooLarge        = "too_large"
	AttemptCancelled       = "cancelled"
)

// LogChunk represents a streaming chunk of output in log_chunks.
type LogChunk struct {
	ID       int64  `json:"id"`
	ExecID   string `json:"exec_id"`
	TSUnixMS int64  `json:"ts_unix_ms"`
	Stream   string `json:"stream"`
	Content  string `json:"content"`
}

func normalizeExecStatus(status string, exitCode int) string {
	switch status {
	case "running", "succeeded", "failed", "stopped", "timed_out":
		return status
	case "":
		return "running"
	case "finished", "done", "ok", "success":
		return "succeeded"
	}
	if exitCode == 0 {
		return "succeeded"
	}
	return "failed"
}

func normalizeExecKind(kind string) string {
	switch kind {
	case "runs":
		return "portal_run"
	case "jobs":
		return "portal_job"
	}
	return kind
}

// execMeta is what the portal keeps about a run or a job in Execution.MetadataJSON. Each key has a
// column: the JSON is only how the portal hands them over and reads them back.
type execMeta struct {
	Workdir  string `json:"workdir,omitempty"`
	Parent   string `json:"parent,omitempty"`
	Relation string `json:"relation,omitempty"`
	Summary  string `json:"summary,omitempty"`
	JobKind  string `json:"job_kind,omitempty"`
	PGID     int    `json:"pgid,omitempty"`
}

func parseExecMeta(raw string) execMeta {
	var m execMeta
	if raw == "" {
		return m
	}
	var loose map[string]any
	if json.Unmarshal([]byte(raw), &loose) != nil {
		return m
	}
	str := func(k string) string {
		if v, ok := loose[k].(string); ok {
			return v
		}
		return ""
	}
	m.Workdir, m.Parent, m.Relation, m.Summary, m.JobKind = str("workdir"), str("parent"), str("relation"), str("summary"), str("job_kind")
	if v, ok := loose["pgid"].(float64); ok && v > 0 {
		m.PGID = int(v)
	}
	return m
}

// CreateExecution inserts an execution. Called again for an execution that exists, it only refreshes
// what a running execution learns after it started: its PID, the engines asked for, and what the
// portal keeps in MetadataJSON (the process group of a job among them).
func (s *Store) CreateExecution(ctx context.Context, e Execution) error {
	meta := parseExecMeta(e.MetadataJSON)
	if e.Workdir == "" {
		e.Workdir = meta.Workdir
	}
	if e.ParentExecID == "" {
		e.ParentExecID = meta.Parent
	}
	if e.ParentRelation == "" {
		e.ParentRelation = meta.Relation
	}
	if e.Summary == "" {
		e.Summary = meta.Summary
	}
	if e.JobKind == "" {
		e.JobKind = meta.JobKind
	}
	e.Kind = normalizeExecKind(e.Kind)
	if e.EnginesRequested == "" {
		e.EnginesRequested = e.Agent
	}
	if e.Kind == "portal_job" {
		if e.CommandText == "" {
			e.CommandText = e.PromptText
		}
		e.PromptText = ""
		if e.JobKind == "" {
			e.JobKind = e.Phase
		}
	}
	sc := ScopeFrom(ctx)
	if e.Kind == "cycle" && e.CycleStamp == "" {
		e.CycleStamp = sc.CycleStamp
		if e.CycleStamp == "" {
			e.CycleStamp = strings.TrimPrefix(e.ExecID, "cycle-")
		}
	}
	if e.ChangeRunID == 0 {
		e.ChangeRunID = sc.runFor(e.ChangeName)
	}
	status := normalizeExecStatus(e.Status, e.ExitCode)
	engineUsed := normalizeEngine(e.EngineUsed)

	return s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		started := rfc3339ToMS(e.StartedAt)
		if started == 0 {
			started = now
		}
		if err := s.ensureChangeTx(ctx, tx, e.ChangeName, now); err != nil {
			return err
		}
		if err := s.ensureEngineTx(ctx, tx, engineUsed); err != nil {
			return err
		}
		if e.CycleStamp != "" {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO cycles (stamp, pid, host, started_at) VALUES (?, ?, ?, ?)
				ON CONFLICT(stamp) DO NOTHING
			`, e.CycleStamp, nullInt(int64(e.PID)), nullStr(s.host), started); err != nil {
				return err
			}
		}
		parent := e.ParentExecID
		if parent != "" {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM executions WHERE exec_id = ?`, parent).Scan(&n); err != nil {
				return err
			}
			if n == 0 || parent == e.ExecID {
				parent = ""
			}
		}
		var finished, exitCode any
		if status != "running" {
			fin := rfc3339ToMS(e.FinishedAt)
			if fin == 0 {
				fin = now
			}
			finished, exitCode = fin, e.ExitCode
		}
		host := e.Host
		if host == "" {
			host = s.host
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO executions (
				exec_id, kind, change_name, change_run_id, cycle_stamp, parent_exec_id, parent_relation,
				phase, role, engines_requested, engine_used, status, exit_code, pid, host, workdir,
				prompt_text, prompt_version, memory_version, job_kind, command_text, summary,
				started_at, finished_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(exec_id) DO UPDATE SET
				pid = COALESCE(excluded.pid, executions.pid),
				engines_requested = CASE WHEN excluded.engines_requested != '' THEN excluded.engines_requested ELSE executions.engines_requested END,
				workdir = COALESCE(excluded.workdir, executions.workdir),
				parent_exec_id = COALESCE(excluded.parent_exec_id, executions.parent_exec_id),
				parent_relation = COALESCE(excluded.parent_relation, executions.parent_relation),
				summary = COALESCE(excluded.summary, executions.summary),
				job_kind = COALESCE(excluded.job_kind, executions.job_kind)
		`, e.ExecID, e.Kind, nullStr(e.ChangeName), nullInt(e.ChangeRunID), nullStr(e.CycleStamp), nullStr(parent), nullStr(e.ParentRelation),
			nullStr(e.Phase), nullStr(e.Role), e.EnginesRequested, nullStr(engineUsed), status, exitCode, nullInt(int64(e.PID)), nullStr(host), nullStr(e.Workdir),
			nullStr(e.PromptText), nullStr(e.PromptVersion), nullStr(e.MemoryVersion), nullStr(e.JobKind), nullStr(e.CommandText), nullStr(e.Summary),
			started, finished)
		if err != nil {
			return err
		}
		if meta.PGID > 0 {
			return s.addProcessGroupTx(ctx, tx, e.ExecID, meta.PGID, 0, now)
		}
		return nil
	})
}

func (s *Store) addProcessGroupTx(ctx context.Context, q querier, execID string, pgid int, attemptID, now int64) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO execution_process_groups (exec_id, pgid, attempt_id, recorded_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(exec_id, pgid) DO NOTHING
	`, execID, pgid, nullInt(attemptID), now)
	return err
}

// AddExecutionProcessGroup records pgid, the process group of a command the execution started (an
// agent CLI and its children, the command of a job), so a later process can stop that group when the
// orchestrator that started it died without stopping it. The time it is recorded at tells the
// group's leader, which was running by then, from a later process given the same ID.
func (s *Store) AddExecutionProcessGroup(ctx context.Context, execID string, pgid int) error {
	if execID == "" || pgid <= 0 {
		return nil
	}
	return s.addProcessGroupTx(ctx, s.db, execID, pgid, ScopeFrom(ctx).AttemptID, s.nowMS())
}

// RunningExecution is an execution still marked running, with the process groups it started.
type RunningExecution struct {
	ExecID     string
	Kind       string
	ChangeName string
	Phase      string
	PID        int
	PGIDs      []int
	// PGIDRecordedAt is when each process group was recorded, in Unix seconds.
	PGIDRecordedAt map[int]int64
}

// ListRunningExecutions returns the executions still marked running.
func (s *Store) ListRunningExecutions(ctx context.Context) ([]RunningExecution, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.exec_id, e.kind, coalesce(e.change_name, ''), coalesce(e.phase, ''), coalesce(e.pid, 0),
		       g.pgid, g.recorded_at
		FROM executions e
		LEFT JOIN execution_process_groups g ON g.exec_id = e.exec_id
		WHERE e.status = 'running'
		ORDER BY e.rowid, g.recorded_at, g.pgid
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunningExecution
	index := make(map[string]int)
	for rows.Next() {
		var e RunningExecution
		var pgid, recordedAt sql.NullInt64
		if err := rows.Scan(&e.ExecID, &e.Kind, &e.ChangeName, &e.Phase, &e.PID, &pgid, &recordedAt); err != nil {
			return nil, err
		}
		i, ok := index[e.ExecID]
		if !ok {
			index[e.ExecID] = len(out)
			out = append(out, e)
			i = len(out) - 1
		}
		if pgid.Valid {
			out[i].PGIDs = append(out[i].PGIDs, int(pgid.Int64))
			if out[i].PGIDRecordedAt == nil {
				out[i].PGIDRecordedAt = make(map[int]int64)
			}
			out[i].PGIDRecordedAt[int(pgid.Int64)] = recordedAt.Int64 / 1000
		}
	}
	return out, rows.Err()
}

// ExecutionEnd is how an execution ended.
type ExecutionEnd struct {
	Status   string
	ExitCode int
	// EngineUsed is the engine that did the work ("" keeps what the attempts recorded).
	EngineUsed string
	// OnlyIfRunning leaves an execution that was finished in the meantime as it is.
	OnlyIfRunning bool
	// AgentExit emits agent.exit for the execution in the same transaction, with ExitPayload added
	// to what the execution row already says (code, seconds, engines, prompt and memory versions).
	AgentExit   bool
	ExitPayload map[string]any
}

// EndExecution records how an execution ended, closes the attempts it left open and, when asked,
// emits its agent.exit event, all in one transaction. It reports whether it wrote.
func (s *Store) EndExecution(ctx context.Context, execID string, end ExecutionEnd) (bool, error) {
	wrote := false
	status := normalizeExecStatus(end.Status, end.ExitCode)
	if status == "running" {
		status = "failed"
	}
	engineUsed := normalizeEngine(end.EngineUsed)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		if err := s.ensureEngineTx(ctx, tx, engineUsed); err != nil {
			return err
		}
		query := `
			UPDATE executions
			SET status = ?, exit_code = ?, finished_at = ?,
			    engine_used = COALESCE(?, engine_used,
			        (SELECT a.engine FROM execution_attempts a WHERE a.exec_id = executions.exec_id ORDER BY a.attempt_no DESC LIMIT 1))
			WHERE exec_id = ?`
		if end.OnlyIfRunning {
			query += ` AND status = 'running'`
		}
		res, err := tx.ExecContext(ctx, query, status, end.ExitCode, now, nullStr(engineUsed), execID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		wrote = true
		if _, err := tx.ExecContext(ctx, `
			UPDATE execution_attempts SET outcome = ?, finished_at = ?
			WHERE exec_id = ? AND finished_at IS NULL
		`, AttemptCancelled, now, execID); err != nil {
			return err
		}
		// Nor does an idle period outlive the execution: one still open belongs to an attempt that
		// ended with it (FinishAttempt closes its own).
		if _, err := tx.ExecContext(ctx, `
			UPDATE attempt_idle_periods SET ended_at = ?, ended_by = ?
			WHERE ended_at IS NULL AND attempt_id IN (SELECT id FROM execution_attempts WHERE exec_id = ?)
		`, now, IdleEndedByExit, execID); err != nil {
			return err
		}
		if !end.AgentExit {
			return nil
		}
		var change, phase, used, promptVer, memVer sql.NullString
		var requested string
		var started int64
		var runID sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT change_name, phase, engine_used, engines_requested, prompt_version, memory_version, started_at, change_run_id
			FROM executions WHERE exec_id = ?
		`, execID).Scan(&change, &phase, &used, &requested, &promptVer, &memVer, &started, &runID); err != nil {
			return err
		}
		payload := map[string]any{
			"code":    end.ExitCode,
			"seconds": int((now - started + 500) / 1000),
			"engines": requested,
		}
		if promptVer.String != "" {
			payload["prompt_version"] = promptVer.String
		}
		if memVer.String != "" {
			payload["memory_version"] = memVer.String
		}
		for k, v := range end.ExitPayload {
			payload[k] = v
		}
		return s.emitEventTx(ctx, tx, EventInput{
			Event:       "agent.exit",
			Change:      change.String,
			Engine:      used.String,
			Phase:       phase.String,
			ExecID:      execID,
			ChangeRunID: runID.Int64,
			Payload:     payload,
		})
	})
	return wrote, err
}

// FinishExecution marks an execution as finished with status and exitCode.
func (s *Store) FinishExecution(ctx context.Context, execID, status string, exitCode int) error {
	_, err := s.EndExecution(ctx, execID, ExecutionEnd{Status: status, ExitCode: exitCode})
	return err
}

// FinishRunningExecution is FinishExecution for an execution that is still marked running: one that was
// finished in the meantime, like a job an operator stopped while it was ending, keeps the status it was
// given. It reports whether it wrote. The check and the write are one statement, so no other writer
// comes between them.
func (s *Store) FinishRunningExecution(ctx context.Context, execID, status string, exitCode int) (bool, error) {
	return s.EndExecution(ctx, execID, ExecutionEnd{Status: status, ExitCode: exitCode, OnlyIfRunning: true})
}

// BeginAttempt records that an execution starts a try of engine (tryNo counts the retries of that
// engine, from 0) and returns the attempt's id.
func (s *Store) BeginAttempt(ctx context.Context, execID, engine string, tryNo int) (int64, error) {
	engine = normalizeEngine(engine)
	if execID == "" || engine == "" {
		return 0, nil
	}
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.ensureEngineTx(ctx, tx, engine); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO execution_attempts (exec_id, attempt_no, engine, try_no, started_at)
			SELECT ?, COALESCE(MAX(attempt_no), 0) + 1, ?, ?, ? FROM execution_attempts WHERE exec_id = ?
		`, execID, engine, tryNo, s.nowMS(), execID)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// FinishAttempt records how an attempt ended and closes the idle period it left open. idleSeconds is
// how long the attempt had shown no activity when the idle watchdog stopped it (0: it did not): such
// an attempt is announced as agent.stalled in the same transaction, whatever outcome it is given (an
// agent that froze on a usage limit is recorded as out of quota).
func (s *Store) FinishAttempt(ctx context.Context, attemptID int64, exitCode int, outcome string, idleSeconds int) error {
	if attemptID <= 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		res, err := tx.ExecContext(ctx, `
			UPDATE execution_attempts SET exit_code = ?, outcome = ?, idle_seconds = ?, finished_at = ?
			WHERE id = ?
		`, exitCode, outcome, nullInt(int64(idleSeconds)), now, attemptID)
		if err != nil {
			return err
		}
		endedBy := IdleEndedByExit
		if idleSeconds > 0 {
			endedBy = IdleEndedByStall
		}
		if _, _, err := s.closeIdlePeriodTx(ctx, tx, attemptID, endedBy, now); err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 || idleSeconds <= 0 {
			return nil
		}
		ref, err := s.attemptRefTx(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		return s.emitEventTx(ctx, tx, EventInput{
			Event:       "agent.stalled",
			Change:      ref.change,
			Engine:      ref.engine,
			Phase:       ref.phase,
			ExecID:      ref.execID,
			ChangeRunID: ref.runID,
			Payload: map[string]any{
				"idle_seconds": idleSeconds,
				"outcome":      outcome,
			},
		})
	})
}

const attemptColumns = `a.id, a.exec_id, a.attempt_no, a.engine, a.try_no, a.exit_code, a.outcome, a.idle_seconds, a.started_at, a.finished_at`

func scanAttempt(scan func(dest ...any) error) (ExecutionAttempt, error) {
	var a ExecutionAttempt
	var exitCode, idle, finished sql.NullInt64
	var outcome sql.NullString
	var started int64
	err := scan(&a.ID, &a.ExecID, &a.AttemptNo, &a.Engine, &a.TryNo, &exitCode, &outcome, &idle, &started, &finished)
	a.ExitCode = int(exitCode.Int64)
	a.Outcome = outcome.String
	a.IdleSeconds = int(idle.Int64)
	a.StartedAt = msToRFC3339(started)
	a.FinishedAt = nullMSToRFC3339(finished)
	return a, err
}

// ListAttempts returns the attempts of an execution, in order.
func (s *Store) ListAttempts(ctx context.Context, execID string) ([]ExecutionAttempt, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `
		SELECT `+attemptColumns+` FROM execution_attempts a WHERE a.exec_id = ? ORDER BY a.attempt_no
	`, execID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExecutionAttempt
	for rows.Next() {
		a, err := scanAttempt(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Retention of execution output. An execution is never deleted; only its output is, whole, and the
// execution then says when (log_pruned_at). The output of a finished execution goes once it is older
// than LogRetention, and the oldest ones go while the table holds more than LogChunkBudget chunks.
var (
	LogRetention   = 30 * 24 * time.Hour
	LogChunkBudget = 50000
)

var appendedChunks atomic.Int64

func normalizeStream(stream string) string {
	switch stream {
	case "combined", "stdout", "stderr", "system":
		return stream
	}
	return "combined"
}

// AppendLogChunk appends a chunk of output of an execution. The attempt it belongs to is the one in
// the scope of ctx.
func (s *Store) AppendLogChunk(ctx context.Context, execID, stream, content string) error {
	if content == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO log_chunks (exec_id, attempt_id, ts, stream, content) VALUES (?, ?, ?, ?, ?)
	`, execID, nullInt(ScopeFrom(ctx).AttemptID), s.nowMS(), normalizeStream(stream), content)
	if err != nil {
		return err
	}
	if appendedChunks.Add(1)%500 == 0 {
		_, _ = s.PruneLogs(context.WithoutCancel(ctx), time.Now())
	}
	return nil
}

// PruneLogs applies the retention of execution output as of now and returns how many executions lost
// their output.
func (s *Store) PruneLogs(ctx context.Context, now time.Time) (int, error) {
	pruned := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		stamp := s.nowMS()
		var ids []string
		rows, err := tx.QueryContext(ctx, `
			SELECT exec_id FROM executions
			WHERE status != 'running' AND log_pruned_at IS NULL AND finished_at < ?
			  AND EXISTS (SELECT 1 FROM log_chunks c WHERE c.exec_id = executions.exec_id)
		`, now.Add(-LogRetention).UnixMilli())
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()

		var total int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM log_chunks`).Scan(&total); err != nil {
			return err
		}
		if total > LogChunkBudget {
			// Oldest finished executions first, never the newest one that has output.
			rows, err := tx.QueryContext(ctx, `
				SELECT e.exec_id, COUNT(c.id) FROM executions e JOIN log_chunks c ON c.exec_id = e.exec_id
				WHERE e.status != 'running' AND e.log_pruned_at IS NULL
				GROUP BY e.exec_id ORDER BY e.rowid ASC
			`)
			if err != nil {
				return err
			}
			type held struct {
				id string
				n  int
			}
			var all []held
			for rows.Next() {
				var h held
				if err := rows.Scan(&h.id, &h.n); err != nil {
					rows.Close()
					return err
				}
				all = append(all, h)
			}
			rows.Close()
			already := make(map[string]bool, len(ids))
			for _, id := range ids {
				already[id] = true
			}
			for _, h := range all {
				if already[h.id] {
					total -= h.n
				}
			}
			for i := 0; i < len(all)-1 && total > LogChunkBudget; i++ {
				if already[all[i].id] {
					continue
				}
				ids = append(ids, all[i].id)
				total -= all[i].n
			}
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM log_chunks WHERE exec_id = ?`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE executions SET log_pruned_at = ? WHERE exec_id = ?`, stamp, id); err != nil {
				return err
			}
		}
		pruned = len(ids)
		return nil
	})
	return pruned, err
}

// ReadLogChunks returns log chunks for execID with id > afterID, ordered by id ASC.
func (s *Store) ReadLogChunks(ctx context.Context, execID string, afterID int64) ([]LogChunk, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, exec_id, ts, stream, content
		FROM log_chunks
		WHERE exec_id = ? AND id > ?
		ORDER BY id ASC
	`, execID, afterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogChunk
	for rows.Next() {
		var c LogChunk
		if err := rows.Scan(&c.ID, &c.ExecID, &c.TSUnixMS, &c.Stream, &c.Content); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReadFullLog concatenates all log chunks for execID.
func (s *Store) ReadFullLog(ctx context.Context, execID string) (string, error) {
	chunks, err := s.ReadLogChunks(ctx, execID, 0)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c.Content)
	}
	return b.String(), nil
}

const executionColumns = `e.exec_id, e.kind, e.change_name, e.change_run_id, e.cycle_stamp, e.parent_exec_id, e.parent_relation,
	e.phase, e.role, e.engines_requested, e.engine_used, e.status, e.exit_code, e.pid, e.host, e.workdir,
	e.prompt_text, e.prompt_version, e.memory_version, e.job_kind, e.command_text, e.summary,
	e.log_pruned_at, e.started_at, e.finished_at,
	(SELECT g.pgid FROM execution_process_groups g WHERE g.exec_id = e.exec_id ORDER BY g.recorded_at DESC, g.pgid DESC LIMIT 1)`

func scanExecution(scan func(dest ...any) error) (Execution, error) {
	var e Execution
	var chg, cycle, parent, relation, phase, role, used, host, workdir, prompt, promptVer, memVer, jobKind, command, summary sql.NullString
	var runID, exitCode, pid, pruned, finished, pgid sql.NullInt64
	var started int64
	err := scan(&e.ExecID, &e.Kind, &chg, &runID, &cycle, &parent, &relation,
		&phase, &role, &e.EnginesRequested, &used, &e.Status, &exitCode, &pid, &host, &workdir,
		&prompt, &promptVer, &memVer, &jobKind, &command, &summary,
		&pruned, &started, &finished, &pgid)
	if err != nil {
		return e, err
	}
	e.ChangeName = chg.String
	e.ChangeRunID = runID.Int64
	e.CycleStamp = cycle.String
	e.ParentExecID = parent.String
	e.ParentRelation = relation.String
	e.Phase = phase.String
	e.Role = role.String
	e.EngineUsed = used.String
	e.ExitCode = int(exitCode.Int64)
	e.PID = int(pid.Int64)
	e.Host = host.String
	e.Workdir = workdir.String
	e.PromptText = prompt.String
	e.PromptVersion = promptVer.String
	e.MemoryVersion = memVer.String
	e.JobKind = jobKind.String
	e.CommandText = command.String
	e.Summary = summary.String
	e.LogPrunedAt = nullMSToRFC3339(pruned)
	e.StartedAt = msToRFC3339(started)
	e.FinishedAt = nullMSToRFC3339(finished)

	e.Agent = e.EngineUsed
	if e.Agent == "" {
		e.Agent = e.EnginesRequested
	}
	if e.Kind == "portal_job" && e.PromptText == "" {
		e.PromptText = e.CommandText
	}
	meta := map[string]any{}
	if e.Workdir != "" {
		meta["workdir"] = e.Workdir
	}
	if e.ParentExecID != "" {
		meta["parent"] = e.ParentExecID
	}
	if e.ParentRelation != "" {
		meta["relation"] = e.ParentRelation
	}
	if e.Summary != "" {
		meta["summary"] = e.Summary
	}
	if e.JobKind != "" {
		meta["job_kind"] = e.JobKind
	}
	if pgid.Valid {
		meta["pgid"] = pgid.Int64
	}
	b, _ := json.Marshal(meta)
	e.MetadataJSON = string(b)
	return e, nil
}

// GetExecution retrieves a single execution by ID.
func (s *Store) GetExecution(ctx context.Context, execID string) (*Execution, error) {
	e, err := scanExecution(s.db.QueryRowContext(ctx, `SELECT `+executionColumns+` FROM executions e WHERE e.exec_id = ?`, execID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ListExecutions lists executions filtered by optional kind and changeName, newest first.
func (s *Store) ListExecutions(ctx context.Context, kind, changeName string, limit int) ([]Execution, error) {
	query := `SELECT ` + executionColumns + ` FROM executions e WHERE 1=1`
	var args []any
	if kind != "" {
		query += " AND e.kind = ?"
		args = append(args, normalizeExecKind(kind))
	}
	if changeName != "" {
		query += " AND e.change_name = ?"
		args = append(args, changeName)
	}
	query += " ORDER BY e.started_at DESC, e.rowid DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Execution
	for rows.Next() {
		e, err := scanExecution(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// NextExecution returns the execution of the same change that was started right after execID, or
// nil when it is the last one. The order is the order the executions were recorded in.
func (s *Store) NextExecution(ctx context.Context, execID string) (*Execution, error) {
	e, err := scanExecution(s.ReadDB().QueryRowContext(ctx, `
		SELECT `+executionColumns+`
		FROM executions e
		JOIN executions cur ON cur.exec_id = ? AND e.change_name = cur.change_name AND e.rowid > cur.rowid
		ORDER BY e.rowid ASC LIMIT 1
	`, execID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}
