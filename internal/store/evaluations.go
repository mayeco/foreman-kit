package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// The two caches hold answers that can be reused for the same input. They can be emptied at any
// time: what was asked, for which change and with what verdict is kept by evaluations.

// GetEvaluationCache returns the cached response_json for cacheKey if present.
func (s *Store) GetEvaluationCache(ctx context.Context, cacheKey string) (string, bool, error) {
	var resp string
	err := s.db.QueryRowContext(ctx, `
		SELECT response_json FROM evaluation_cache WHERE cache_key = ?
	`, cacheKey).Scan(&resp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return resp, true, nil
}

// PutEvaluationCache stores a response of the evaluator in evaluation_cache.
func (s *Store) PutEvaluationCache(ctx context.Context, cacheKey, gate, model, stateJSON, responseJSON string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO evaluation_cache (cache_key, gate, model, state_json, response_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(cache_key) DO UPDATE SET
			response_json = excluded.response_json,
			created_at = excluded.created_at
	`, cacheKey, gate, model, stateJSON, responseJSON, s.nowMS())
	return err
}

// Evaluation is one question put to the evaluator: the gate asked, the verdict, the answers, what it cost,
// who asked (the change, and the worker run or merge attempt in progress) and the action the
// orchestrator took on it.
type Evaluation struct {
	ID             int64          `json:"id"`
	Gate           string         `json:"gate"`
	Mode           string         `json:"mode,omitempty"`
	Status         string         `json:"status"`
	Model          string         `json:"model,omitempty"`
	Cached         bool           `json:"cached"`
	CacheKey       string         `json:"cache_key,omitempty"`
	CostUSD        float64        `json:"cost_usd"`
	AnswersJSON    string         `json:"answers_json,omitempty"`
	RouteAction    string         `json:"route_action,omitempty"`
	RouteDetail    map[string]any `json:"route_detail,omitempty"`
	ChangeName     string         `json:"change_name,omitempty"`
	ChangeRunID    int64          `json:"change_run_id,omitempty"`
	MergeAttemptID int64          `json:"merge_attempt_id,omitempty"`
	ExecID         string         `json:"exec_id,omitempty"`
	CreatedAt      string         `json:"created_at"`
}

// RecordEvaluation records a question put to the evaluator and its verdict and returns the evaluation's
// id. The parents left empty are taken from the scope of ctx. When event is not nil it is emitted in
// the same transaction, as the announcement of that evaluation.
func (s *Store) RecordEvaluation(ctx context.Context, ev Evaluation, event *EventInput) (int64, error) {
	sc := ScopeFrom(ctx)
	if ev.ChangeRunID == 0 {
		ev.ChangeRunID = sc.runFor(ev.ChangeName)
	}
	if ev.MergeAttemptID == 0 && ev.ChangeRunID == 0 {
		ev.MergeAttemptID = sc.mergeFor(ev.ChangeName)
	}
	if ev.ExecID == "" && (ev.ChangeName == "" || sc.Change == "" || sc.Change == ev.ChangeName) {
		ev.ExecID = sc.ExecID
	}
	if ev.Status == "" {
		ev.Status = "unknown"
	}
	var answers any
	if ev.AnswersJSON != "" && json.Valid([]byte(ev.AnswersJSON)) {
		answers = ev.AnswersJSON
	}
	var id int64
	insert := func(withParents bool) error {
		return s.inTx(ctx, func(tx *sql.Tx) error {
			now := s.nowMS()
			if err := s.ensureChangeTx(ctx, tx, ev.ChangeName, now); err != nil {
				return err
			}
			var runID, mergeID, execID any
			if withParents {
				runID, mergeID, execID = nullInt(ev.ChangeRunID), nullInt(ev.MergeAttemptID), nullStr(ev.ExecID)
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO evaluations (gate, mode, status, model, cached, cache_key, cost_usd, answers_json,
				                             change_name, change_run_id, merge_attempt_id, exec_id, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, ev.Gate, nullStr(ev.Mode), ev.Status, nullStr(ev.Model), boolToInt(ev.Cached), nullStr(ev.CacheKey), ev.CostUSD, answers,
				nullStr(ev.ChangeName), runID, mergeID, execID, now)
			if err != nil {
				return err
			}
			if id, err = res.LastInsertId(); err != nil {
				return err
			}
			if event == nil {
				return nil
			}
			in := *event
			if in.Payload == nil {
				in.Payload = map[string]any{}
			}
			in.Payload["evaluation_id"] = id
			return s.emitEventTx(ctx, tx, in)
		})
	}
	err := insert(true)
	if err != nil && isForeignKeyError(err) {
		err = insert(false)
	}
	return id, err
}

// SetEvaluationRoute records the action the orchestrator took on an evaluation (for example "escalate",
// "rerun_gates", "needs_human"), with what it was decided on.
func (s *Store) SetEvaluationRoute(ctx context.Context, evaluationID int64, action string, detail map[string]any) error {
	if evaluationID <= 0 || action == "" {
		return nil
	}
	var detailJSON any
	if len(detail) > 0 {
		if b, err := json.Marshal(detail); err == nil {
			detailJSON = string(b)
		}
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE evaluations SET route_action = ?, route_detail_json = ? WHERE id = ?
	`, action, detailJSON, evaluationID)
	return err
}

// ListEvaluations returns the evaluations of a change, oldest first (of every change when change
// is empty), at most limit when it is positive.
func (s *Store) ListEvaluations(ctx context.Context, change string, limit int) ([]Evaluation, error) {
	query := `SELECT ` + evaluationColumns + ` FROM evaluations v`
	var args []any
	if change != "" {
		query += ` WHERE v.change_name = ?`
		args = append(args, change)
	}
	query += ` ORDER BY v.id ASC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.ReadDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Evaluation
	for rows.Next() {
		ev, err := scanEvaluation(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

const evaluationColumns = `v.id, v.gate, v.mode, v.status, v.model, v.cached, v.cache_key, v.cost_usd, v.answers_json, v.route_action,
	v.route_detail_json, v.change_name, v.change_run_id, v.merge_attempt_id, v.exec_id, v.created_at`

func scanEvaluation(scan func(dest ...any) error) (Evaluation, error) {
	var ev Evaluation
	var mode, model, cacheKey, answers, action, detail, change, execID sql.NullString
	var runID, mergeID sql.NullInt64
	var cached int
	var created int64
	err := scan(&ev.ID, &ev.Gate, &mode, &ev.Status, &model, &cached, &cacheKey, &ev.CostUSD, &answers, &action,
		&detail, &change, &runID, &mergeID, &execID, &created)
	ev.Mode, ev.Model, ev.CacheKey, ev.AnswersJSON, ev.RouteAction = mode.String, model.String, cacheKey.String, answers.String, action.String
	ev.Cached = cached != 0
	if detail.String != "" {
		// What cannot be read stays empty: the verdict and the action are in their own columns.
		_ = json.Unmarshal([]byte(detail.String), &ev.RouteDetail)
	}
	ev.ChangeName, ev.ExecID = change.String, execID.String
	ev.ChangeRunID, ev.MergeAttemptID = runID.Int64, mergeID.Int64
	ev.CreatedAt = msToRFC3339(created)
	return ev, err
}

// GetLLMCache returns the cached structured response_json from llm_cache if present.
func (s *Store) GetLLMCache(ctx context.Context, cacheKey string) (string, bool, error) {
	var resp string
	err := s.ReadDB().QueryRowContext(ctx, `
		SELECT response_json FROM llm_cache WHERE cache_key = ?
	`, cacheKey).Scan(&resp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return resp, true, nil
}

// PutLLMCache stores a structured LLM response in llm_cache.
func (s *Store) PutLLMCache(ctx context.Context, cacheKey, taskKind, model, responseJSON string, inputTokens, outputTokens int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO llm_cache (cache_key, task_kind, model, response_json, input_tokens, output_tokens, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(cache_key) DO UPDATE SET
			response_json = excluded.response_json,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			created_at = excluded.created_at
	`, cacheKey, taskKind, model, responseJSON, inputTokens, outputTokens, s.nowMS())
	return err
}
