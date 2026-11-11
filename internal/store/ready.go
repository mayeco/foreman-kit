package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
)

// ErrNoReadyManifest is returned when a change has no manifest waiting for the merger.
var ErrNoReadyManifest = errors.New("store: the change has no manifest in the merge queue")

// ErrNoMergeAttempt is returned when a write needs the merge attempt in progress and there is none.
var ErrNoMergeAttempt = errors.New("store: no merge attempt is in progress")

// ReadyManifest is a change a worker handed to the merger: one row of ready_manifests that stays
// open until the merger integrates it, sends it back or blocks it, and is then closed with that
// outcome instead of being deleted.
type ReadyManifest struct {
	ID             int64             `json:"id,omitempty"`
	ChangeName     string            `json:"change_name"`
	Branch         string            `json:"branch"`
	Worktree       string            `json:"worktree"`
	HeadSHA        string            `json:"head_sha"`
	Capabilities   []string          `json:"capabilities"`
	Assessment     map[string]any    `json:"assessment,omitempty"`
	Item           map[string]any    `json:"item,omitempty"`
	ChildrenPushed map[string]string `json:"children_pushed,omitempty"`
	EnqueuedAt     string            `json:"enqueued_at"`
}

// Outcomes of a ready manifest.
const (
	ManifestIntegrated        = "integrated"
	ManifestSentBack          = "sent_back"
	ManifestBlocked           = "blocked"
	ManifestRemoved           = "removed"
	ManifestInvalid           = "invalid"
	ManifestAlreadyIntegrated = "already_integrated"
)

// EnqueueReady queues a change for the merger. A manifest the change still had in the queue is
// closed as removed: the new one replaces it.
func (s *Store) EnqueueReady(ctx context.Context, m ReadyManifest) error {
	if m.HeadSHA == "" {
		return fmt.Errorf("store: the manifest of %s has no head commit", m.ChangeName)
	}
	sc := ScopeFrom(ctx)
	var assessment, item any
	if len(m.Assessment) > 0 {
		if b, err := json.Marshal(m.Assessment); err == nil {
			assessment = string(b)
		}
	}
	if len(m.Item) > 0 {
		if b, err := json.Marshal(m.Item); err == nil {
			item = string(b)
		}
	}
	var evaluationID int64
	switch v := m.Assessment["evaluation_id"].(type) {
	case int64:
		evaluationID = v
	case int:
		evaluationID = int64(v)
	case float64:
		evaluationID = int64(v)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		if err := s.ensureChangeTx(ctx, tx, m.ChangeName, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE ready_manifests SET closed_at = ?, outcome = ? WHERE change_name = ? AND closed_at IS NULL
		`, now, ManifestRemoved, m.ChangeName); err != nil {
			return err
		}
		if evaluationID > 0 {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM evaluations WHERE id = ?`, evaluationID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				evaluationID = 0
			}
		}
		enqueuedAt := rfc3339ToMS(m.EnqueuedAt)
		if enqueuedAt == 0 {
			enqueuedAt = s.nowMS()
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO ready_manifests (change_name, change_run_id, branch, worktree, head_sha,
			                             risk_evaluation_id, assessment_json, item_json, enqueued_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, m.ChangeName, nullInt(sc.runFor(m.ChangeName)), m.Branch, m.Worktree, m.HeadSHA,
			nullInt(evaluationID), assessment, item, enqueuedAt)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		seen := make(map[string]bool, len(m.Capabilities))
		for _, capability := range m.Capabilities {
			if capability == "" || seen[capability] {
				continue
			}
			seen[capability] = true
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO manifest_capabilities (manifest_id, capability) VALUES (?, ?)
			`, id, capability); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) openManifestIDTx(ctx context.Context, q querier, change string) (int64, string, error) {
	var id int64
	var head string
	err := q.QueryRowContext(ctx, `
		SELECT id, head_sha FROM ready_manifests WHERE change_name = ? AND closed_at IS NULL
	`, change).Scan(&id, &head)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNoReadyManifest
	}
	return id, head, err
}

// RecordChildPushed records the commit the merger pushed of a child repository for the change's
// manifest. It belongs to the manifest, not to the attempt: an attempt that resumes the merge finds
// what was already pushed.
func (s *Store) RecordChildPushed(ctx context.Context, change, childPath, pushedSHA string) error {
	sc := ScopeFrom(ctx)
	return s.inTx(ctx, func(tx *sql.Tx) error {
		manifestID, _, err := s.openManifestIDTx(ctx, tx, change)
		if err != nil {
			return err
		}
		attemptID := sc.mergeFor(change)
		if attemptID == 0 {
			err := tx.QueryRowContext(ctx, `
				SELECT id FROM merge_attempts WHERE manifest_id = ? AND finished_at IS NULL
			`, manifestID).Scan(&attemptID)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNoMergeAttempt
			}
			if err != nil {
				return err
			}
		}
		repo, err := s.ensureRepoTx(ctx, tx, childPath, "")
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO manifest_pushes (manifest_id, repo_path, pushed_sha, merge_attempt_id, pushed_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(manifest_id, repo_path) DO UPDATE SET
				pushed_sha = excluded.pushed_sha,
				merge_attempt_id = excluded.merge_attempt_id,
				pushed_at = excluded.pushed_at
		`, manifestID, repo, pushedSHA, attemptID, s.nowMS())
		return err
	})
}

// UpdateReadyHead records that the branch of a queued change moved to newHeadSHA: the manifest
// follows it, and the merge attempt in progress keeps both the head it found and the new one.
func (s *Store) UpdateReadyHead(ctx context.Context, change, newHeadSHA string) error {
	if newHeadSHA == "" {
		return nil
	}
	sc := ScopeFrom(ctx)
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE ready_manifests SET head_sha = ? WHERE change_name = ? AND closed_at IS NULL
		`, newHeadSHA, change); err != nil {
			return err
		}
		if id := sc.mergeFor(change); id > 0 {
			_, err := tx.ExecContext(ctx, `UPDATE merge_attempts SET branch_tip_sha = ? WHERE id = ?`, newHeadSHA, id)
			return err
		}
		return nil
	})
}

// ListReadyQueue returns the manifests waiting for the merger, oldest first.
func (s *Store) ListReadyQueue(ctx context.Context) ([]ReadyManifest, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `
		SELECT id, change_name, branch, worktree, head_sha, assessment_json, item_json, enqueued_at
		FROM ready_manifests WHERE closed_at IS NULL
		ORDER BY enqueued_at ASC, change_name ASC
	`)
	if err != nil {
		return nil, err
	}
	var out []ReadyManifest
	for rows.Next() {
		var m ReadyManifest
		var assessment, item sql.NullString
		var enqueuedAt int64
		if err := rows.Scan(&m.ID, &m.ChangeName, &m.Branch, &m.Worktree, &m.HeadSHA, &assessment, &item, &enqueuedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if assessment.String != "" {
			_ = json.Unmarshal([]byte(assessment.String), &m.Assessment)
		}
		if item.String != "" {
			_ = json.Unmarshal([]byte(item.String), &m.Item)
		}
		m.EnqueuedAt = msToRFC3339(enqueuedAt)
		m.ChildrenPushed = make(map[string]string)
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		caps, err := s.ReadDB().QueryContext(ctx, `SELECT capability FROM manifest_capabilities WHERE manifest_id = ? ORDER BY capability`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for caps.Next() {
			var c string
			if err := caps.Scan(&c); err != nil {
				caps.Close()
				return nil, err
			}
			out[i].Capabilities = append(out[i].Capabilities, c)
		}
		caps.Close()
		pushes, err := s.ReadDB().QueryContext(ctx, `SELECT repo_path, pushed_sha FROM manifest_pushes WHERE manifest_id = ?`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for pushes.Next() {
			var repo, sha string
			if err := pushes.Scan(&repo, &sha); err != nil {
				pushes.Close()
				return nil, err
			}
			out[i].ChildrenPushed[repo] = sha
		}
		pushes.Close()
	}
	return out, nil
}

// CloseReady takes the manifest of a change out of the merge queue with the outcome it had (a
// Manifest* constant). A change with no manifest in the queue is left as it is.
func (s *Store) CloseReady(ctx context.Context, change, outcome string) error {
	switch outcome {
	case ManifestIntegrated, ManifestSentBack, ManifestBlocked, ManifestRemoved, ManifestInvalid, ManifestAlreadyIntegrated:
	default:
		outcome = ManifestRemoved
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE ready_manifests SET closed_at = ?, outcome = ? WHERE change_name = ? AND closed_at IS NULL
	`, s.nowMS(), outcome, change)
	return err
}

// RemoveReady takes the manifest of a change out of the merge queue without an outcome of the
// merger: an operator removed it, or the change was abandoned.
func (s *Store) RemoveReady(ctx context.Context, change string) error {
	return s.CloseReady(ctx, change, ManifestRemoved)
}

// MergeAttempt is one pass of the merger over a manifest.
type MergeAttempt struct {
	ID               int64  `json:"id"`
	ManifestID       int64  `json:"manifest_id"`
	ChangeName       string `json:"change_name"`
	PID              int    `json:"pid"`
	Host             string `json:"host"`
	State            string `json:"state"`
	ManifestHeadSHA  string `json:"manifest_head_sha"`
	BranchTipSHA     string `json:"branch_tip_sha,omitempty"`
	Result           string `json:"result,omitempty"`
	ResultReason     string `json:"result_reason,omitempty"`
	IntegratedCommit string `json:"integrated_commit,omitempty"`
	SpecsJSON        string `json:"specs_json,omitempty"`
	ResumedFromID    int64  `json:"resumed_from_id,omitempty"`
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at,omitempty"`
}

// Results of a merge attempt.
const (
	MergeIntegrated        = "integrated"
	MergeSentBack          = "sent_back"
	MergeBlocked           = "blocked"
	MergePushRejected      = "push_rejected"
	MergeAlreadyIntegrated = "already_integrated"
	MergeHeld              = "held"
	MergeStopped           = "stopped"
)

// BeginMergeAttempt records that the merger of this process starts on the manifest of change and
// returns the attempt's id. The merger holds its lock: an attempt left open by an earlier owner (a
// process that died, a lock taken over) is closed as stopped in the same transaction, since only
// one attempt is open at a time. The new attempt points at the previous one of the same manifest.
func (s *Store) BeginMergeAttempt(ctx context.Context, change string) (int64, error) {
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		manifestID, head, err := s.openManifestIDTx(ctx, tx, change)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE merge_attempts SET result = ?, result_reason = 'left open by an earlier merger', finished_at = ?
			WHERE finished_at IS NULL
		`, MergeStopped, now); err != nil {
			return err
		}
		var previous sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(id) FROM merge_attempts WHERE manifest_id = ?`, manifestID).Scan(&previous); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO merge_attempts (manifest_id, pid, host, state, manifest_head_sha, resumed_from_id, started_at)
			VALUES (?, ?, ?, 'CLAIMED', ?, ?, ?)
		`, manifestID, os.Getpid(), s.host, head, nullInt(previous.Int64), s.nowMS())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

var mergeStates = map[string]bool{
	"CLAIMED": true, "POLICY_CHECK": true, "REBASING_CHILDREN": true, "CHECKING_WORK_LOSS": true,
	"ASSEMBLING": true, "AUDITING_TASKS": true, "VERIFYING_BASE": true, "RUNNING_GATES": true,
	"ARCHIVING": true, "COMMITTING": true, "PUSHING_CHILDREN": true, "PUSHING_ROOT": true, "COMPLETED": true,
}

// SetMergeAttemptState records the state an open merge attempt entered.
func (s *Store) SetMergeAttemptState(ctx context.Context, id int64, state string) error {
	if id <= 0 || !mergeStates[state] {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE merge_attempts SET state = ? WHERE id = ? AND finished_at IS NULL`, state, id)
	return err
}

// MarkMergeAttemptIntegrated records the integration commit an open merge attempt produced, with the
// totals of the specs it archived (JSON, or "").
func (s *Store) MarkMergeAttemptIntegrated(ctx context.Context, id int64, commit, specsJSON string) error {
	if id <= 0 {
		return nil
	}
	if specsJSON != "" && !json.Valid([]byte(specsJSON)) {
		specsJSON = ""
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE merge_attempts SET integrated_commit = ?, specs_json = ?, state = 'COMPLETED'
		WHERE id = ? AND finished_at IS NULL
	`, nullStr(commit), nullStr(specsJSON), id)
	return err
}

// FinishMergeAttempt records how an open merge attempt ended (a Merge* constant) and why. An attempt
// that was already closed (as stopped, by the merger that took over) keeps the result it has.
func (s *Store) FinishMergeAttempt(ctx context.Context, id int64, result, reason string) error {
	if id <= 0 {
		return nil
	}
	switch result {
	case MergeIntegrated, MergeSentBack, MergeBlocked, MergePushRejected, MergeAlreadyIntegrated, MergeHeld, MergeStopped:
	default:
		result = MergeStopped
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE merge_attempts
		SET result = CASE WHEN ?1 = 'integrated' AND integrated_commit IS NULL THEN 'already_integrated' ELSE ?1 END,
		    result_reason = ?2, finished_at = ?3
		WHERE id = ?4 AND finished_at IS NULL
	`, result, nullStr(reason), s.nowMS(), id)
	return err
}

const mergeAttemptColumns = `m.id, m.manifest_id, r.change_name, m.pid, m.host, m.state, m.manifest_head_sha, m.branch_tip_sha,
	m.result, m.result_reason, m.integrated_commit, m.specs_json, m.resumed_from_id, m.started_at, m.finished_at`

func scanMergeAttempt(scan func(dest ...any) error) (MergeAttempt, error) {
	var a MergeAttempt
	var tip, result, reason, commit, specs sql.NullString
	var resumed, finished sql.NullInt64
	var started int64
	err := scan(&a.ID, &a.ManifestID, &a.ChangeName, &a.PID, &a.Host, &a.State, &a.ManifestHeadSHA, &tip,
		&result, &reason, &commit, &specs, &resumed, &started, &finished)
	a.BranchTipSHA, a.Result, a.ResultReason, a.IntegratedCommit, a.SpecsJSON = tip.String, result.String, reason.String, commit.String, specs.String
	a.ResumedFromID = resumed.Int64
	a.StartedAt = msToRFC3339(started)
	a.FinishedAt = nullMSToRFC3339(finished)
	return a, err
}

// OpenMergeAttempt returns the merge attempt in progress, or nil when the merger is idle.
func (s *Store) OpenMergeAttempt(ctx context.Context) (*MergeAttempt, error) {
	a, err := scanMergeAttempt(s.db.QueryRowContext(ctx, `
		SELECT `+mergeAttemptColumns+`
		FROM merge_attempts m JOIN ready_manifests r ON r.id = m.manifest_id
		WHERE m.finished_at IS NULL
	`).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListMergeAttempts returns the merge attempts of a change, newest first (of every change when
// change is empty).
func (s *Store) ListMergeAttempts(ctx context.Context, change string, limit int) ([]MergeAttempt, error) {
	query := `SELECT ` + mergeAttemptColumns + ` FROM merge_attempts m JOIN ready_manifests r ON r.id = m.manifest_id`
	var args []any
	if change != "" {
		query += ` WHERE r.change_name = ?`
		args = append(args, change)
	}
	query += ` ORDER BY m.id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.ReadDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MergeAttempt
	for rows.Next() {
		a, err := scanMergeAttempt(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CapabilitiesInQueue returns the capabilities claimed by the manifests waiting for the merger, with
// the changes that claim each.
func (s *Store) CapabilitiesInQueue(ctx context.Context) (map[string][]string, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `
		SELECT c.capability, r.change_name
		FROM manifest_capabilities c JOIN ready_manifests r ON r.id = c.manifest_id
		WHERE r.closed_at IS NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]string)
	for rows.Next() {
		var capability, change string
		if err := rows.Scan(&capability, &change); err != nil {
			return nil, err
		}
		out[capability] = append(out[capability], change)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out, rows.Err()
}
