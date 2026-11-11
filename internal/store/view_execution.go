package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

// ExecutionRecord is what the kit has on record about one execution: the execution with its whole
// prompt and its attempts, where it comes from and what followed it.
type ExecutionRecord struct {
	ExecutionDetail
	// Run is the worker run the execution is part of, nil outside one.
	Run *ChangeRun `json:"run,omitempty"`
	// Parent is the execution this one continues or repeats; Continuations, the ones that continue
	// or repeat it. Their PromptText holds only the beginning of the prompt.
	Parent        *Execution  `json:"parent,omitempty"`
	Continuations []Execution `json:"continuations"`
	// Next is the execution of the same change that was started right after it.
	Next *Execution `json:"next,omitempty"`
	// Repair is the repair round the execution carried out, nil when it is not a repair.
	Repair *RepairRoundRecord `json:"repair,omitempty"`
	// GateRounds are the rounds of gates that verified its work.
	GateRounds []GateRoundDetail `json:"gate_rounds"`
	// Evaluations are the questions put to the evaluator while it was the work at hand.
	Evaluations []Evaluation `json:"evaluations"`
}

// Execution returns what is on record about the execution execID, or nil when there is none.
func (v *View) Execution(ctx context.Context, execID string) (*ExecutionRecord, error) {
	ex, err := scanExecution(v.q.QueryRowContext(ctx, `SELECT `+executionColumns+` FROM executions e WHERE e.exec_id = ?`, execID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec := &ExecutionRecord{
		ExecutionDetail: *v.executionDetail(ex),
		Continuations:   []Execution{},
		GateRounds:      []GateRoundDetail{},
		Evaluations:     []Evaluation{},
	}
	if err := v.loadAttempts(ctx, []any{execID}, map[string]*ExecutionDetail{execID: &rec.ExecutionDetail}); err != nil {
		return nil, err
	}

	one := func(query string, args ...any) (*Execution, error) {
		e, err := scanExecution(v.q.QueryRowContext(ctx, query, args...).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &e, nil
	}
	if ex.ParentExecID != "" {
		if rec.Parent, err = one(`SELECT `+executionListColumns+` FROM executions e WHERE e.exec_id = ?`, ex.ParentExecID); err != nil {
			return nil, err
		}
	}
	if err := v.each(ctx, func(scan func(dest ...any) error) error {
		e, err := scanExecution(scan)
		if err == nil {
			rec.Continuations = append(rec.Continuations, e)
		}
		return err
	}, `SELECT `+executionListColumns+` FROM executions e WHERE e.parent_exec_id = ? ORDER BY e.started_at, e.exec_id`, execID); err != nil {
		return nil, err
	}
	if ex.ChangeName != "" {
		// The order is the order the executions were recorded in, as Store.NextExecution has it. The
		// one that follows is found in the index of the change, and it alone is read: a change with
		// thousands of executions is not sorted to show one.
		if rec.Next, err = one(`
			SELECT `+executionListColumns+`
			FROM executions e
			WHERE e.rowid = (
				SELECT MIN(x.rowid) FROM executions x
				JOIN executions cur ON cur.exec_id = ? AND x.change_name = cur.change_name AND x.rowid > cur.rowid)
		`, execID); err != nil {
			return nil, err
		}
	}

	switch {
	case ex.ChangeRunID != 0:
		run, err := scanChangeRun(v.q.QueryRowContext(ctx, `SELECT `+changeRunColumns+` FROM change_runs r WHERE r.id = ?`, ex.ChangeRunID).Scan)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			rec.Run = &run
		}
		rr, err := scanRepairRound(v.q.QueryRowContext(ctx, `SELECT `+repairRoundColumns+repairRoundTables+`
			WHERE rr.change_run_id = ? AND rr.exec_id = ? ORDER BY rr.id LIMIT 1`, ex.ChangeRunID, execID).Scan)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			rec.Repair = &rr
		}
		rounds := map[int64]*GateRoundDetail{}
		var order []*GateRoundDetail
		if err := v.each(ctx, func(scan func(dest ...any) error) error {
			r, err := scanGateRound(scan)
			if err == nil {
				d := &GateRoundDetail{GateRound: r, Runs: []GateRunRecord{}}
				rounds[r.ID] = d
				order = append(order, d)
			}
			return err
		}, `SELECT `+gateRoundColumns+` FROM gate_rounds r WHERE r.change_run_id = ? AND r.after_exec_id = ? ORDER BY r.id`, ex.ChangeRunID, execID); err != nil {
			return nil, err
		}
		if err := v.loadGateRuns(ctx, rounds); err != nil {
			return nil, err
		}
		for _, d := range order {
			rec.GateRounds = append(rec.GateRounds, *d)
		}
		if err := v.each(ctx, func(scan func(dest ...any) error) error {
			ev, err := scanEvaluation(scan)
			if err == nil {
				rec.Evaluations = append(rec.Evaluations, ev)
			}
			return err
		}, `SELECT `+evaluationColumns+` FROM evaluations v WHERE v.change_run_id = ? AND v.exec_id = ? ORDER BY v.id`, ex.ChangeRunID, execID); err != nil {
			return nil, err
		}
	case ex.ChangeName != "":
		if err := v.each(ctx, func(scan func(dest ...any) error) error {
			ev, err := scanEvaluation(scan)
			if err == nil {
				rec.Evaluations = append(rec.Evaluations, ev)
			}
			return err
		}, `SELECT `+evaluationColumns+` FROM evaluations v WHERE v.change_name = ? AND v.exec_id = ? ORDER BY v.id`, ex.ChangeName, execID); err != nil {
			return nil, err
		}
	}
	return rec, nil
}

// ExecutionStatus returns the status of the execution execID, and false when there is none. It is the
// read of whoever follows an execution and needs only to know whether it still runs.
func (v *View) ExecutionStatus(ctx context.Context, execID string) (string, bool, error) {
	var status string
	err := v.q.QueryRowContext(ctx, `SELECT status FROM executions WHERE exec_id = ?`, execID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return status, err == nil, err
}

// loadGateRuns fills the gate runs of the rounds in byRound.
func (v *View) loadGateRuns(ctx context.Context, byRound map[int64]*GateRoundDetail) error {
	ids := make([]any, 0, len(byRound))
	for id := range byRound {
		ids = append(ids, id)
	}
	return chunked(ids, func(part []any) error {
		return v.each(ctx, func(scan func(dest ...any) error) error {
			g, err := scanGateRun(scan)
			if err != nil {
				return err
			}
			if d := byRound[g.GateRoundID]; d != nil {
				d.Runs = append(d.Runs, g)
			}
			return nil
		}, `SELECT `+gateRunColumns+gateRunTables+` WHERE g.gate_round_id IN (`+placeholders(len(part))+`) ORDER BY g.id`, part...)
	})
}

// GateRound returns a round of gates with the result of each gate, or nil when there is none.
func (v *View) GateRound(ctx context.Context, id int64) (*GateRoundDetail, error) {
	r, err := scanGateRound(v.q.QueryRowContext(ctx, `SELECT `+gateRoundColumns+` FROM gate_rounds r WHERE r.id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d := &GateRoundDetail{GateRound: r, Runs: []GateRunRecord{}}
	if err := v.loadGateRuns(ctx, map[int64]*GateRoundDetail{id: d}); err != nil {
		return nil, err
	}
	return d, nil
}

// EventsOfExecution returns the events of one execution, as Store.EventsOfExecution does: those that
// name it, and those of its worker run that name no execution and happened while it was the latest
// execution of that run.
func (v *View) EventsOfExecution(ctx context.Context, execID string) ([]EventRecord, error) {
	return eventsOfExecution(ctx, v.q, execID)
}

// LogTail is the end of the output of an execution.
// LatestJob returns the latest operation of the kit a person started from the portal whose kind is
// one of kinds ("preflight", "dispatch", ...), nil when none of them was ever started.
func (v *View) LatestJob(ctx context.Context, kinds ...string) (*ExecutionDetail, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(kinds))
	for _, kind := range kinds {
		args = append(args, kind)
	}
	e, err := scanExecution(v.q.QueryRowContext(ctx, `SELECT `+executionListColumns+` FROM executions e
	    WHERE e.kind = 'portal_job' AND e.job_kind IN (`+placeholders(len(kinds))+`)
	    ORDER BY e.started_at DESC, e.exec_id DESC LIMIT 1`, args...).Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return v.executionDetail(e), nil
}

type LogTail struct {
	Content string `json:"content"`
	// LastID is the id of the last chunk of the output: a stream of it continues after that chunk.
	LastID int64 `json:"last_id"`
	// Bytes is the size of the whole output; Truncated says Content is only its end.
	Bytes     int64 `json:"bytes"`
	Truncated bool  `json:"truncated"`
}

// LogTail returns the output of an execution: all of it when it fits in maxBytes or maxBytes is not
// positive, its last chunks otherwise. A chunk is never cut.
func (v *View) LogTail(ctx context.Context, execID string, maxBytes int) (LogTail, error) {
	var tail LogTail
	if err := v.q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(octet_length(content)), 0), COALESCE(MAX(id), 0) FROM log_chunks WHERE exec_id = ?
	`, execID).Scan(&tail.Bytes, &tail.LastID); err != nil {
		return tail, err
	}
	if tail.Bytes == 0 {
		return tail, nil
	}
	if maxBytes <= 0 || tail.Bytes <= int64(maxBytes) {
		var b strings.Builder
		err := v.each(ctx, func(scan func(dest ...any) error) error {
			var content string
			if err := scan(&content); err != nil {
				return err
			}
			b.WriteString(content)
			return nil
		}, `SELECT content FROM log_chunks WHERE exec_id = ? AND id <= ? ORDER BY id`, execID, tail.LastID)
		tail.Content = b.String()
		return tail, err
	}

	// The end of a long output: its last chunks, read from the last one back until they fill the size.
	var chunks []string
	size := 0
	rows, err := v.q.QueryContext(ctx, `SELECT content FROM log_chunks WHERE exec_id = ? AND id <= ? ORDER BY id DESC`, execID, tail.LastID)
	if err != nil {
		return tail, err
	}
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			rows.Close()
			return tail, err
		}
		if len(chunks) > 0 && size+len(content) > maxBytes {
			break
		}
		chunks = append(chunks, content)
		size += len(content)
	}
	if err := rows.Close(); err != nil {
		return tail, err
	}
	var b strings.Builder
	for i := len(chunks) - 1; i >= 0; i-- {
		b.WriteString(chunks[i])
	}
	tail.Content, tail.Truncated = b.String(), true
	return tail, nil
}

// LogPage is a run of chunks of the output of an execution, in the order they were written.
type LogPage struct {
	Items []LogChunk `json:"items"`
	// Next is the cursor of the chunks that follow, "" when these are the last ones written so far.
	Next string `json:"next,omitempty"`
}

// ReadLogChunks returns the chunks of the output of an execution written after the chunk the cursor
// names ("" for the beginning), at most limit of them.
func (v *View) ReadLogChunks(ctx context.Context, execID, after string, limit int) (LogPage, error) {
	var page LogPage
	afterID := int64(0)
	if after = strings.TrimSpace(after); after != "" {
		id, err := strconv.ParseInt(after, 10, 64)
		if err != nil || id < 0 {
			return page, ErrBadCursor
		}
		afterID = id
	}
	limit = pageSize(limit)
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		var c LogChunk
		if err := scan(&c.ID, &c.ExecID, &c.TSUnixMS, &c.Stream, &c.Content); err != nil {
			return err
		}
		page.Items = append(page.Items, c)
		return nil
	}, `SELECT id, exec_id, ts, stream, content FROM log_chunks WHERE exec_id = ? AND id > ? ORDER BY id LIMIT ?`, execID, afterID, limit+1)
	if err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = strconv.FormatInt(page.Items[limit-1].ID, 10)
	}
	return page, nil
}
