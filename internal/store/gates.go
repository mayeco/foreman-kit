package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrNoChangeRun is returned when a row that belongs to a worker's run of a change is written
// outside one.
var ErrNoChangeRun = errors.New("store: no run of the change is in progress")

// GateRound is one launch of a set of gates: the verification of a worker's build, the merger's
// gates, the check of a repository's base alone, or a run from the command line.
type GateRound struct {
	ID             int64  `json:"id"`
	Context        string `json:"context"` // "worker_verify", "merger", "base_verify", "cli"
	ChangeName     string `json:"change_name,omitempty"`
	ChangeRunID    int64  `json:"change_run_id,omitempty"`
	MergeAttemptID int64  `json:"merge_attempt_id,omitempty"`
	AfterExecID    string `json:"after_exec_id,omitempty"`
	Slot           int    `json:"slot"`
	BaseRef        string `json:"base_ref,omitempty"`
	HeadSHA        string `json:"head_sha,omitempty"`
	Result         string `json:"result,omitempty"` // "passed", "failed", "stopped"
	StartedAt      string `json:"started_at"`
	FinishedAt     string `json:"finished_at,omitempty"`
}

// GateRunRecord stores an individual gate execution result.
type GateRunRecord struct {
	ID          int64  `json:"id"`
	GateRoundID int64  `json:"gate_round_id,omitempty"`
	ChangeName  string `json:"change_name,omitempty"`
	Context     string `json:"context"`
	// RepoName is the repository's short name (the label of its gates); RepoPath, its path under the
	// umbrella. Either identifies the repository when a run is recorded.
	RepoName       string `json:"repo_name"`
	RepoPath       string `json:"repo_path,omitempty"`
	GateName       string `json:"gate_name"`
	Command        string `json:"command"`
	Slot           int    `json:"slot"`
	ExitCode       int    `json:"exit_code"`
	DurationMS     int64  `json:"duration_ms"`
	FailureExcerpt string `json:"failure_excerpt,omitempty"`
	LogFilePath    string `json:"log_file_path,omitempty"`
	CreatedAt      string `json:"created_at"`
}

// gateContext returns the context a round can be recorded with: the one asked for when its parent
// is in scope, "cli" otherwise (a round of a worker's verification belongs to a run, one of the
// merger to a merge attempt).
func gateContext(asked string, runID, mergeID int64) string {
	switch asked {
	case "worker_verify":
		if runID > 0 {
			return asked
		}
	case "merger":
		if mergeID > 0 {
			return asked
		}
	case "base_verify":
		return asked
	}
	return "cli"
}

func (s *Store) beginGateRoundTx(ctx context.Context, tx *sql.Tx, r GateRound) (int64, error) {
	sc := ScopeFrom(ctx)
	now := s.nowMS()
	if err := s.ensureChangeTx(ctx, tx, r.ChangeName, now); err != nil {
		return 0, err
	}
	if r.ChangeRunID == 0 {
		r.ChangeRunID = sc.runFor(r.ChangeName)
	}
	if r.MergeAttemptID == 0 {
		r.MergeAttemptID = sc.mergeFor(r.ChangeName)
	}
	if r.AfterExecID == "" {
		r.AfterExecID = sc.ExecID
	}
	if r.AfterExecID == "" && r.ChangeRunID > 0 {
		// The execution whose work the round verifies: the latest one of the run.
		var execID sql.NullString
		if err := tx.QueryRowContext(ctx, `
			SELECT exec_id FROM executions WHERE change_run_id = ? ORDER BY rowid DESC LIMIT 1
		`, r.ChangeRunID).Scan(&execID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		r.AfterExecID = execID.String
	}
	started := rfc3339ToMS(r.StartedAt)
	if started == 0 {
		started = now
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO gate_rounds (context, change_name, change_run_id, merge_attempt_id, after_exec_id, slot, base_ref, head_sha, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, gateContext(r.Context, r.ChangeRunID, r.MergeAttemptID), nullStr(r.ChangeName), nullInt(r.ChangeRunID), nullInt(r.MergeAttemptID),
		nullStr(r.AfterExecID), r.Slot, nullStr(r.BaseRef), nullStr(r.HeadSHA), started)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// BeginGateRound records that a set of gates starts and returns the round's id. The worker run or
// merge attempt it belongs to is the one in the scope of ctx.
func (s *Store) BeginGateRound(ctx context.Context, r GateRound) (int64, error) {
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.beginGateRoundTx(ctx, tx, r)
		return err
	})
	return id, err
}

// FinishGateRound records how a gate round ended: "passed", "failed" or "stopped".
func (s *Store) FinishGateRound(ctx context.Context, id int64, result string) error {
	if id <= 0 {
		return nil
	}
	switch result {
	case "passed", "failed", "stopped":
	default:
		result = "stopped"
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE gate_rounds SET result = ?, finished_at = ? WHERE id = ? AND finished_at IS NULL
	`, result, s.nowMS(), id)
	return err
}

// RecordGateRun records the result of one gate in the round rec names or, failing that, the one in
// the scope of ctx. A gate run outside any round gets a round of its own.
func (s *Store) RecordGateRun(ctx context.Context, rec GateRunRecord) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		roundID := rec.GateRoundID
		if roundID == 0 {
			roundID = ScopeFrom(ctx).GateRoundID
		}
		own := false
		if roundID == 0 {
			var err error
			roundID, err = s.beginGateRoundTx(ctx, tx, GateRound{Context: rec.Context, ChangeName: rec.ChangeName, Slot: rec.Slot})
			if err != nil {
				return err
			}
			own = true
		}
		repo, err := s.ensureRepoTx(ctx, tx, rec.RepoPath, rec.RepoName)
		if err != nil {
			return err
		}
		if repo == "" {
			if repo, err = s.ensureRepoTx(ctx, tx, UmbrellaRepoPath, ""); err != nil {
				return err
			}
		}
		finished := rfc3339ToMS(rec.CreatedAt)
		if finished == 0 {
			finished = s.nowMS()
		}
		if rec.DurationMS < 0 {
			rec.DurationMS = 0
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO gate_runs (gate_round_id, repo_path, gate_name, command, exit_code, duration_ms,
			                       failure_excerpt, output_path, started_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, roundID, repo, rec.GateName, rec.Command, rec.ExitCode, rec.DurationMS,
			nullStr(rec.FailureExcerpt), nullStr(rec.LogFilePath), finished-rec.DurationMS, finished); err != nil {
			return err
		}
		if own {
			result := "passed"
			if rec.ExitCode != 0 {
				result = "failed"
			}
			_, err = tx.ExecContext(ctx, `UPDATE gate_rounds SET result = ?, finished_at = ? WHERE id = ?`, result, s.nowMS(), roundID)
		}
		return err
	})
}

// ListGateRuns lists the gate runs of a change in the order they were recorded (of every change
// when change is empty).
func (s *Store) ListGateRuns(ctx context.Context, change string) ([]GateRunRecord, error) {
	q := `SELECT ` + gateRunColumns + gateRunTables
	var args []any
	if change != "" {
		q += ` WHERE r.change_name = ?`
		args = append(args, change)
	}
	q += ` ORDER BY g.id ASC`
	rows, err := s.ReadDB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GateRunRecord
	for rows.Next() {
		r, err := scanGateRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// A gate run is read with the round it belongs to and the name of its repository.
const (
	gateRunColumns = `g.id, g.gate_round_id, r.change_name, r.context, p.name, g.repo_path, g.gate_name, g.command, r.slot,
		g.exit_code, g.duration_ms, g.failure_excerpt, g.output_path, g.finished_at`
	gateRunTables = `
		FROM gate_runs g
		JOIN gate_rounds r ON r.id = g.gate_round_id
		JOIN repos p ON p.repo_path = g.repo_path`
)

func scanGateRun(scan func(dest ...any) error) (GateRunRecord, error) {
	var r GateRunRecord
	var chg, exc, logP sql.NullString
	var finished int64
	err := scan(&r.ID, &r.GateRoundID, &chg, &r.Context, &r.RepoName, &r.RepoPath, &r.GateName, &r.Command, &r.Slot,
		&r.ExitCode, &r.DurationMS, &exc, &logP, &finished)
	r.ChangeName = chg.String
	r.FailureExcerpt = exc.String
	r.LogFilePath = logP.String
	r.CreatedAt = msToRFC3339(finished)
	return r, err
}

const gateRoundColumns = `r.id, r.context, r.change_name, r.change_run_id, r.merge_attempt_id, r.after_exec_id, r.slot,
	r.base_ref, r.head_sha, r.result, r.started_at, r.finished_at`

func scanGateRound(scan func(dest ...any) error) (GateRound, error) {
	var r GateRound
	var change, after, baseRef, head, result sql.NullString
	var runID, mergeID, finished sql.NullInt64
	var started int64
	err := scan(&r.ID, &r.Context, &change, &runID, &mergeID, &after, &r.Slot, &baseRef, &head, &result, &started, &finished)
	r.ChangeName, r.AfterExecID, r.BaseRef, r.HeadSHA, r.Result = change.String, after.String, baseRef.String, head.String, result.String
	r.ChangeRunID, r.MergeAttemptID = runID.Int64, mergeID.Int64
	r.StartedAt = msToRFC3339(started)
	r.FinishedAt = nullMSToRFC3339(finished)
	return r, err
}

// RepairRoundRecord is one round of repair inside a worker's run: what failed, the execution that
// repaired it, and how it ended.
type RepairRoundRecord struct {
	ID          int64  `json:"id"`
	ChangeName  string `json:"change_name"`
	ChangeRunID int64  `json:"change_run_id,omitempty"`
	RoundNo     int    `json:"round_no,omitempty"`
	Phase       string `json:"phase"`
	// Engine is the engine that did the repair. It is read from the round's execution (ExecID).
	Engine           string `json:"engine"`
	ExecID           string `json:"exec_id,omitempty"`
	AskedReason      string `json:"asked_reason"`
	AskedDetails     string `json:"asked_details"`
	AskedGateRoundID int64  `json:"asked_gate_round_id,omitempty"`
	ChangedDiffstat  string `json:"changed_diffstat,omitempty"`
	ResultPassed     bool   `json:"result_passed"`
	ResultNextReason string `json:"result_next_reason,omitempty"`
	// ResultGateRoundID is the gate round that verified the repair: left 0, it is taken to be the
	// latest verification round of the run, when one ran after the round the repair was asked for.
	ResultGateRoundID int64  `json:"result_gate_round_id,omitempty"`
	CreatedAt         string `json:"created_at"`
}

// RecordRepairRound records a repair round in the worker run rec names or, failing that, the one in
// the scope of ctx. It returns ErrNoChangeRun outside a run of the change.
func (s *Store) RecordRepairRound(ctx context.Context, rec RepairRoundRecord) error {
	runID := rec.ChangeRunID
	if runID == 0 {
		runID = ScopeFrom(ctx).runFor(rec.ChangeName)
	}
	if runID == 0 {
		return ErrNoChangeRun
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		execID := rec.ExecID
		if execID != "" {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM executions WHERE exec_id = ?`, execID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				execID = ""
			}
		}
		resultRound := rec.ResultGateRoundID
		if resultRound == 0 {
			var latest sql.NullInt64
			if err := tx.QueryRowContext(ctx, `
				SELECT MAX(id) FROM gate_rounds WHERE change_run_id = ? AND context = 'worker_verify'
			`, runID).Scan(&latest); err != nil {
				return err
			}
			if latest.Int64 > rec.AskedGateRoundID {
				resultRound = latest.Int64
			}
		}
		created := rfc3339ToMS(rec.CreatedAt)
		if created == 0 {
			created = s.nowMS()
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO repair_rounds (change_run_id, round_no, phase, exec_id, asked_reason, asked_details,
			                           asked_gate_round_id, changed_diffstat, result_passed, result_next_reason,
			                           result_gate_round_id, created_at)
			SELECT ?, COALESCE(MAX(round_no), 0) + 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
			FROM repair_rounds WHERE change_run_id = ? AND phase = ?
		`, runID, rec.Phase, nullStr(execID), rec.AskedReason, rec.AskedDetails,
			nullInt(rec.AskedGateRoundID), nullStr(rec.ChangedDiffstat), boolToInt(rec.ResultPassed), nullStr(rec.ResultNextReason),
			nullInt(resultRound), created, runID, rec.Phase)
		return err
	})
}

// ListRepairRounds lists the repair rounds of a change in the order they were recorded (of every
// change when change is empty).
func (s *Store) ListRepairRounds(ctx context.Context, change string) ([]RepairRoundRecord, error) {
	q := `SELECT ` + repairRoundColumns + repairRoundTables
	var args []any
	if change != "" {
		q += ` WHERE r.change_name = ?`
		args = append(args, change)
	}
	q += ` ORDER BY rr.id ASC`
	rows, err := s.ReadDB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepairRoundRecord
	for rows.Next() {
		r, err := scanRepairRound(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// A repair round is read with the change of its run and the engine of its execution.
const (
	repairRoundColumns = `rr.id, r.change_name, rr.change_run_id, rr.round_no, rr.phase, rr.exec_id,
		COALESCE(e.engine_used, e.engines_requested, ''),
		rr.asked_reason, rr.asked_details, rr.asked_gate_round_id, rr.changed_diffstat,
		rr.result_passed, rr.result_next_reason, rr.result_gate_round_id, rr.created_at`
	repairRoundTables = `
		FROM repair_rounds rr
		JOIN change_runs r ON r.id = rr.change_run_id
		LEFT JOIN executions e ON e.exec_id = rr.exec_id`
)

func scanRepairRound(scan func(dest ...any) error) (RepairRoundRecord, error) {
	var r RepairRoundRecord
	var execID, diff, nextR sql.NullString
	var asked, result sql.NullInt64
	var passed int
	var created int64
	err := scan(&r.ID, &r.ChangeName, &r.ChangeRunID, &r.RoundNo, &r.Phase, &execID, &r.Engine,
		&r.AskedReason, &r.AskedDetails, &asked, &diff, &passed, &nextR, &result, &created)
	r.ExecID = execID.String
	r.AskedGateRoundID = asked.Int64
	r.ChangedDiffstat = diff.String
	r.ResultPassed = passed != 0
	r.ResultNextReason = nextR.String
	r.ResultGateRoundID = result.Int64
	r.CreatedAt = msToRFC3339(created)
	return r, err
}
