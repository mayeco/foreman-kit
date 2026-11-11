package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// EventInput is a structured event to persist. The relation fields (ExecID and the ones after it)
// say what the event is about; the ones left empty are taken from the Scope of the context. Payload
// holds only what has no column.
type EventInput struct {
	TS      time.Time
	Event   string
	PID     int
	Change  string
	Engine  string
	Phase   string
	Payload map[string]any

	ExecID          string
	ChangeRunID     int64
	MergeAttemptID  int64
	DispatcherRunID int64
	GateRoundID     int64
	CycleStamp      string
	// RepoPath is the child repository the event is about (its path under the umbrella).
	RepoPath string
}

// EventRecord is a stored event. Payload is the event as its readers expect it: the stored payload
// together with the event's own columns (ts, event, pid, change, engine, phase and the relation ids).
type EventRecord struct {
	ID      int64          `json:"id"`
	TS      string         `json:"ts"`
	TSUnix  int64          `json:"ts_unix"`
	Event   string         `json:"event"`
	PID     int            `json:"pid"`
	Change  string         `json:"change,omitempty"`
	Engine  string         `json:"engine,omitempty"`
	Phase   string         `json:"phase,omitempty"`
	Payload map[string]any `json:"payload"`

	ExecID          string `json:"exec_id,omitempty"`
	ChangeRunID     int64  `json:"change_run_id,omitempty"`
	MergeAttemptID  int64  `json:"merge_attempt_id,omitempty"`
	DispatcherRunID int64  `json:"dispatcher_run_id,omitempty"`
	GateRoundID     int64  `json:"gate_round_id,omitempty"`
	CycleStamp      string `json:"cycle_stamp,omitempty"`
	RepoPath        string `json:"repo_path,omitempty"`
}

// EventFilter filters QueryEvents.
type EventFilter struct {
	Since      time.Time
	Event      string
	ChangeName string
	// ExecID keeps only the events of one execution; ChangeRunID, of one worker run.
	ExecID      string
	ChangeRunID int64
	// AfterID keeps only events with id > AfterID (an incremental cursor).
	AfterID int64
	// Limit caps the number of events returned: the oldest matching ones, or the newest ones when
	// Newest is set. Results are always in ascending id order.
	Limit  int
	Newest bool
}

// EmitEvent inserts a structured event.
func (s *Store) EmitEvent(ctx context.Context, in EventInput) error {
	return s.emitEventTx(ctx, s.db, in)
}

// emitEventTx inserts an event with q, which is the transaction of the fact the event announces
// when there is one: the fact and its event are then written together or not at all.
func (s *Store) emitEventTx(ctx context.Context, q querier, in EventInput) error {
	var ts int64
	if in.TS.IsZero() {
		ts = s.nowMS()
	} else {
		ts = in.TS.UTC().UnixMilli()
	}
	pid := in.PID
	if pid <= 0 {
		pid = os.Getpid()
	}

	sc := ScopeFrom(ctx)
	// A scope describes the work in progress: an event that names another change is not about it.
	sameChange := in.Change == "" || sc.Change == "" || in.Change == sc.Change
	if in.ExecID == "" && sameChange {
		in.ExecID = sc.ExecID
	}
	if in.ChangeRunID == 0 && sameChange {
		in.ChangeRunID = sc.ChangeRunID
	}
	if in.MergeAttemptID == 0 && sameChange {
		in.MergeAttemptID = sc.MergeAttemptID
	}
	if in.GateRoundID == 0 && sameChange {
		in.GateRoundID = sc.GateRoundID
	}
	if in.DispatcherRunID == 0 {
		in.DispatcherRunID = sc.DispatcherRunID
	}
	if in.CycleStamp == "" {
		in.CycleStamp = sc.CycleStamp
	}

	payload := in.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	if in.RepoPath == "" {
		// Emitters name the child repository an event is about under "repo": it is its column too.
		if repo, ok := payload["repo"].(string); ok {
			in.RepoPath = repo
		}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	engine := normalizeEngine(in.Engine)
	if err := s.ensureChangeTx(ctx, q, in.Change, ts); err != nil {
		return err
	}
	if err := s.ensureEngineTx(ctx, q, engine); err != nil {
		return err
	}
	repoPath := ""
	if in.RepoPath != "" {
		if repoPath, err = s.ensureRepoTx(ctx, q, in.RepoPath, ""); err != nil {
			return err
		}
	}

	insert := func(withParents bool) error {
		var execID, cycle any
		var runID, mergeID, dispID, roundID any
		if withParents {
			execID, cycle = nullStr(in.ExecID), nullStr(in.CycleStamp)
			runID, mergeID = nullInt(in.ChangeRunID), nullInt(in.MergeAttemptID)
			dispID, roundID = nullInt(in.DispatcherRunID), nullInt(in.GateRoundID)
		}
		_, err := q.ExecContext(ctx, `
			INSERT INTO events (ts, event, pid, host, change_name, exec_id, change_run_id, merge_attempt_id,
			                    dispatcher_run_id, cycle_stamp, gate_round_id, repo_path, engine, phase, payload_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, ts, in.Event, pid, nullStr(s.host), nullStr(in.Change), execID, runID, mergeID,
			dispID, cycle, roundID, nullStr(repoPath), nullStr(engine), nullStr(in.Phase), string(b))
		return err
	}
	err = insert(true)
	if err != nil && isForeignKeyError(err) {
		// A parent the scope names is gone or was never written (its own insert failed): the event is
		// worth more without the pointer than not at all.
		err = insert(false)
	}
	return err
}

func isForeignKeyError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

const eventColumns = `id, ts, event, pid, change_name, engine, phase, payload_json, exec_id, change_run_id,
	merge_attempt_id, dispatcher_run_id, gate_round_id, cycle_stamp, repo_path`

func scanEvent(scan func(dest ...any) error) (EventRecord, error) {
	var rec EventRecord
	var ts int64
	var chg, eng, phase, execID, cycle, repo sql.NullString
	var runID, mergeID, dispID, roundID sql.NullInt64
	var payloadStr string
	if err := scan(&rec.ID, &ts, &rec.Event, &rec.PID, &chg, &eng, &phase, &payloadStr, &execID, &runID,
		&mergeID, &dispID, &roundID, &cycle, &repo); err != nil {
		return rec, err
	}
	rec.TS = msToRFC3339(ts)
	rec.TSUnix = ts / 1000
	rec.Change = chg.String
	rec.Engine = eng.String
	rec.Phase = phase.String
	rec.ExecID = execID.String
	rec.ChangeRunID = runID.Int64
	rec.MergeAttemptID = mergeID.Int64
	rec.DispatcherRunID = dispID.Int64
	rec.GateRoundID = roundID.Int64
	rec.CycleStamp = cycle.String
	rec.RepoPath = repo.String

	p := map[string]any{}
	_ = json.Unmarshal([]byte(payloadStr), &p)
	if p == nil {
		p = map[string]any{}
	}
	// The event's own columns, for readers that take the event as one map. A key the payload already
	// holds stays as the emitter wrote it.
	set := func(k string, v any) {
		if _, ok := p[k]; !ok {
			p[k] = v
		}
	}
	p["ts"] = rec.TS
	p["event"] = rec.Event
	p["pid"] = rec.PID
	if rec.Change != "" {
		set("change", rec.Change)
	}
	if rec.Engine != "" {
		set("engine", rec.Engine)
	}
	if rec.Phase != "" {
		set("phase", rec.Phase)
	}
	if rec.ExecID != "" {
		set("exec_id", rec.ExecID)
	}
	if rec.ChangeRunID != 0 {
		set("change_run_id", rec.ChangeRunID)
	}
	if rec.MergeAttemptID != 0 {
		set("merge_attempt_id", rec.MergeAttemptID)
	}
	if rec.DispatcherRunID != 0 {
		set("dispatcher_run_id", rec.DispatcherRunID)
	}
	if rec.GateRoundID != 0 {
		set("gate_round_id", rec.GateRoundID)
	}
	if rec.CycleStamp != "" {
		set("cycle_stamp", rec.CycleStamp)
	}
	if rec.RepoPath != "" {
		set("repo", rec.RepoPath)
	}
	rec.Payload = p
	return rec, nil
}

// QueryEvents retrieves structured events matching filter.
func (s *Store) QueryEvents(ctx context.Context, f EventFilter) ([]EventRecord, error) {
	query := `SELECT ` + eventColumns + ` FROM events WHERE 1=1`
	var args []any
	if !f.Since.IsZero() {
		// To the second, as callers give Since: an event of the same second is included.
		query += " AND ts >= ?"
		args = append(args, f.Since.UTC().Unix()*1000)
	}
	if f.Event != "" {
		query += " AND event = ?"
		args = append(args, f.Event)
	}
	if f.ChangeName != "" {
		query += " AND change_name = ?"
		args = append(args, f.ChangeName)
	}
	if f.ExecID != "" {
		query += " AND exec_id = ?"
		args = append(args, f.ExecID)
	}
	if f.ChangeRunID > 0 {
		query += " AND change_run_id = ?"
		args = append(args, f.ChangeRunID)
	}
	if f.AfterID > 0 {
		query += " AND id > ?"
		args = append(args, f.AfterID)
	}
	if f.Limit > 0 && f.Newest {
		query = "SELECT * FROM (" + query + " ORDER BY id DESC LIMIT ?) ORDER BY id ASC"
		args = append(args, f.Limit)
	} else {
		query += " ORDER BY id ASC"
		if f.Limit > 0 {
			query += " LIMIT ?"
			args = append(args, f.Limit)
		}
	}

	// Read from the reader pool: SSE clients poll this every few milliseconds and must not occupy the
	// single writer connection that every agent output chunk, heartbeat and event is written through.
	rows, err := s.ReadDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EventRecord
	for rows.Next() {
		rec, err := scanEvent(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// EventsOfExecution returns the events of one execution: those that name it, and those of its
// worker run that name no execution and happened from its start until the next execution of that
// run started (a gate round, an answer of the evaluator, the block that ended it).
func (s *Store) EventsOfExecution(ctx context.Context, execID string) ([]EventRecord, error) {
	return eventsOfExecution(ctx, s.ReadDB(), execID)
}

func eventsOfExecution(ctx context.Context, q reader, execID string) ([]EventRecord, error) {
	rows, err := q.QueryContext(ctx, `
		WITH cur AS (
			SELECT exec_id, change_run_id, started_at, rowid AS rid FROM executions WHERE exec_id = ?1
		), nxt AS (
			SELECT MIN(e.started_at) AS started_at
			FROM executions e, cur
			WHERE cur.change_run_id IS NOT NULL AND e.change_run_id = cur.change_run_id AND e.rowid > cur.rid
		)
		SELECT `+eventColumns+` FROM events
		WHERE exec_id = ?1
		   OR (exec_id IS NULL
		       AND change_run_id IS NOT NULL
		       AND change_run_id = (SELECT change_run_id FROM cur)
		       AND ts >= (SELECT started_at FROM cur)
		       AND ts < COALESCE((SELECT started_at FROM nxt), 9223372036854775807))
		ORDER BY id ASC
	`, execID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRecord
	for rows.Next() {
		rec, err := scanEvent(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// MaxEventID returns the id of the newest event, or 0 when there is none.
func (s *Store) MaxEventID(ctx context.Context) (int64, error) {
	var id int64
	err := s.ReadDB().QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&id)
	return id, err
}
