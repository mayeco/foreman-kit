package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// ExecutionFilter selects a page of the list of executions. A field left empty does not filter.
type ExecutionFilter struct {
	Kind   string // "worker", "cycle", "portal_run" or "portal_job"
	Status string // "running", "succeeded", "failed", "stopped" or "timed_out"
	// Engine keeps the executions an engine worked on: the one that did the work, or one of those tried.
	Engine string
	Change string
	Cycle  string
	// Before is the cursor a page gave as Next; Limit, how many executions the page holds.
	Before string
	Limit  int
}

// ExecutionPage is one page of the list of executions, the latest first.
type ExecutionPage struct {
	Items []ExecutionDetail `json:"items"`
	// Next is the cursor of the page that follows, "" when this one is the last.
	Next string `json:"next,omitempty"`
}

// ListExecutionsPage returns one page of the executions of every kind that match f, the latest first,
// each with its attempts. The list pages by cursor over (started_at, exec_id): rows keep coming in,
// and an offset would repeat or skip some between one page and the next.
func (v *View) ListExecutionsPage(ctx context.Context, f ExecutionFilter) (ExecutionPage, error) {
	var page ExecutionPage
	at, key, err := timeCursor(f.Before)
	if err != nil {
		return page, err
	}
	limit := pageSize(f.Limit)

	conds := []string{`(e.started_at, e.exec_id) < (?, ?)`}
	args := []any{at, key}
	if f.Kind != "" {
		conds = append(conds, `e.kind = ?`)
		args = append(args, f.Kind)
	}
	if f.Status != "" {
		conds = append(conds, `e.status = ?`)
		args = append(args, f.Status)
	}
	if f.Engine != "" {
		conds = append(conds, `(e.engine_used = ? OR EXISTS (SELECT 1 FROM execution_attempts a WHERE a.exec_id = e.exec_id AND a.engine = ?))`)
		args = append(args, f.Engine, f.Engine)
	}
	if f.Change != "" {
		conds = append(conds, `e.change_name = ?`)
		args = append(args, f.Change)
	}
	if f.Cycle != "" {
		conds = append(conds, `e.cycle_stamp = ?`)
		args = append(args, f.Cycle)
	}
	args = append(args, limit+1)

	var started []int64
	byExec := map[string]*ExecutionDetail{}
	var order []*ExecutionDetail
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var ms int64
		e, err := scanExecution(func(dest ...any) error { return scan(append(dest, &ms)...) })
		if err != nil {
			return err
		}
		order = append(order, v.executionDetail(e))
		started = append(started, ms)
		return nil
	}, `SELECT `+executionListColumns+`, e.started_at FROM executions e
	    WHERE `+strings.Join(conds, ` AND `)+`
	    ORDER BY e.started_at DESC, e.exec_id DESC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	if len(order) > limit {
		order = order[:limit]
		page.Next = formatTimeCursor(started[limit-1], order[limit-1].ExecID)
	}
	execIDs := make([]any, 0, len(order))
	for _, d := range order {
		byExec[d.ExecID] = d
		execIDs = append(execIDs, d.ExecID)
	}
	if err := v.loadAttempts(ctx, execIDs, byExec); err != nil {
		return page, err
	}
	page.Items = make([]ExecutionDetail, 0, len(order))
	for _, d := range order {
		page.Items = append(page.Items, *d)
	}
	return page, nil
}

// EventPageFilter selects a page of the events. A field left empty or zero does not filter. Every
// field is a column of the event, held by an index.
type EventPageFilter struct {
	Event          string
	Change         string
	ExecID         string
	ChangeRunID    int64
	MergeAttemptID int64
	GateRoundID    int64
	CycleStamp     string
	// Before is the cursor a page gave as Next; Limit, how many events the page holds.
	Before string
	Limit  int
}

// EventPage is one page of events, the latest first.
type EventPage struct {
	Items []EventRecord `json:"items"`
	// Next is the cursor of the page that follows, "" when this one is the last.
	Next string `json:"next,omitempty"`
}

func (f EventPageFilter) where(withEvent bool) ([]string, []any) {
	var conds []string
	var args []any
	add := func(cond string, arg any) {
		conds = append(conds, cond)
		args = append(args, arg)
	}
	if withEvent && f.Event != "" {
		add(`event = ?`, f.Event)
	}
	if f.Change != "" {
		add(`change_name = ?`, f.Change)
	}
	if f.ExecID != "" {
		add(`exec_id = ?`, f.ExecID)
	}
	if f.ChangeRunID > 0 {
		add(`change_run_id = ?`, f.ChangeRunID)
	}
	if f.MergeAttemptID > 0 {
		add(`merge_attempt_id = ?`, f.MergeAttemptID)
	}
	if f.GateRoundID > 0 {
		add(`gate_round_id = ?`, f.GateRoundID)
	}
	if f.CycleStamp != "" {
		add(`cycle_stamp = ?`, f.CycleStamp)
	}
	return conds, args
}

// ListEventsPage returns one page of the events that match f, the latest first, paged by cursor over
// the event id.
func (v *View) ListEventsPage(ctx context.Context, f EventPageFilter) (EventPage, error) {
	var page EventPage
	before, err := idCursor(f.Before)
	if err != nil {
		return page, err
	}
	limit := pageSize(f.Limit)
	conds, args := f.where(true)
	conds = append(conds, `id < ?`)
	args = append(args, before, limit+1)
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		rec, err := scanEvent(scan)
		if err == nil {
			page.Items = append(page.Items, rec)
		}
		return err
	}, `SELECT `+eventColumns+` FROM events WHERE `+strings.Join(conds, ` AND `)+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = strconv.FormatInt(page.Items[limit-1].ID, 10)
	}
	return page, nil
}

// EventTypeCount is how many events of one type there are.
type EventTypeCount struct {
	Event string `json:"event"`
	Count int    `json:"count"`
}

// CountEventsByType returns how many events of each type match f, by type. f.Event, the cursor and
// the limit are not taken into account: the counts are those of the filter of types itself, over
// every event, not over the page.
func (v *View) CountEventsByType(ctx context.Context, f EventPageFilter) ([]EventTypeCount, error) {
	conds, args := f.where(false)
	query := `SELECT event, COUNT(*) FROM events`
	if len(conds) > 0 {
		query += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	var out []EventTypeCount
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		var c EventTypeCount
		if err := scan(&c.Event, &c.Count); err != nil {
			return err
		}
		out = append(out, c)
		return nil
	}, query+` GROUP BY event ORDER BY event`, args...)
	return out, err
}

// Pause is a pause of the workspace: who put it and why, and who lifted it.
type Pause struct {
	ID         int64  `json:"id"`
	Reason     string `json:"reason"`
	PausedBy   string `json:"paused_by"`
	ChangeName string `json:"change_name,omitempty"`
	PausedAt   string `json:"paused_at"`
	// ResumedAt is "" while the pause is in force.
	ResumedAt string `json:"resumed_at,omitempty"`
	ResumedBy string `json:"resumed_by,omitempty"`
}

// PausePage is one page of the pauses of the workspace, the latest first.
type PausePage struct {
	Items []Pause `json:"items"`
	Next  string  `json:"next,omitempty"`
}

func scanPause(scan func(dest ...any) error) (Pause, error) {
	var p Pause
	var change, by sql.NullString
	var resumed sql.NullInt64
	var paused int64
	err := scan(&p.ID, &p.Reason, &p.PausedBy, &change, &paused, &resumed, &by)
	p.ChangeName, p.ResumedBy = change.String, by.String
	p.PausedAt = msToRFC3339(paused)
	p.ResumedAt = nullMSToRFC3339(resumed)
	return p, err
}

const pauseColumns = `id, reason, paused_by, change_name, paused_at, resumed_at, resumed_by`

// ListPauses returns one page of the pauses of the workspace, the latest first: the one in force,
// when there is one, and the ones that were lifted.
func (v *View) ListPauses(ctx context.Context, before string, limit int) (PausePage, error) {
	var page PausePage
	cursor, err := idCursor(before)
	if err != nil {
		return page, err
	}
	limit = pageSize(limit)
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		p, err := scanPause(scan)
		if err == nil {
			page.Items = append(page.Items, p)
		}
		return err
	}, `SELECT `+pauseColumns+` FROM workspace_pauses WHERE id < ? ORDER BY id DESC LIMIT ?`, cursor, limit+1)
	if err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = strconv.FormatInt(page.Items[limit-1].ID, 10)
	}
	return page, nil
}

// OpenPause returns the pause in force, or nil when the workspace is not paused.
func (v *View) OpenPause(ctx context.Context) (*Pause, error) {
	p, err := scanPause(v.q.QueryRowContext(ctx, `SELECT `+pauseColumns+` FROM workspace_pauses WHERE resumed_at IS NULL`).Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// DispatchRecord is a run of the dispatcher with what its workers did: how many changes they took
// and how each of those runs ended.
type DispatchRecord struct {
	DispatcherRun
	ChangeRuns int `json:"change_runs"`
	// Results counts the worker runs by result; a run still open counts under "".
	Results map[string]int `json:"results"`
}

// DispatchPage is one page of the runs of the dispatcher, the latest first.
type DispatchPage struct {
	Items []DispatchRecord `json:"items"`
	Next  string           `json:"next,omitempty"`
}

// ListDispatcherRuns returns one page of the runs of the dispatcher, the latest first.
func (v *View) ListDispatcherRuns(ctx context.Context, before string, limit int) (DispatchPage, error) {
	var page DispatchPage
	cursor, err := idCursor(before)
	if err != nil {
		return page, err
	}
	limit = pageSize(limit)
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var d DispatchRecord
		var reason sql.NullString
		var finished sql.NullInt64
		var started int64
		if err := scan(&d.ID, &d.PID, &d.Host, &d.Mode, &d.Workers, &d.BacklogSize, &reason, &started, &finished); err != nil {
			return err
		}
		d.EndReason = reason.String
		d.StartedAt = msToRFC3339(started)
		d.FinishedAt = nullMSToRFC3339(finished)
		d.Results = map[string]int{}
		page.Items = append(page.Items, d)
		return nil
	}, `SELECT id, pid, host, mode, workers, backlog_size, end_reason, started_at, finished_at
	    FROM dispatcher_runs WHERE id < ? ORDER BY id DESC LIMIT ?`, cursor, limit+1)
	if err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = strconv.FormatInt(page.Items[limit-1].ID, 10)
	}
	if len(page.Items) == 0 {
		return page, nil
	}

	at := make(map[int64]int, len(page.Items))
	ids := make([]any, 0, len(page.Items))
	for i := range page.Items {
		at[page.Items[i].ID] = i
		ids = append(ids, page.Items[i].ID)
	}
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var id int64
		var result sql.NullString
		var n int
		if err := scan(&id, &result, &n); err != nil {
			return err
		}
		if i, ok := at[id]; ok {
			page.Items[i].ChangeRuns += n
			page.Items[i].Results[result.String] += n
		}
		return nil
	}, `SELECT dispatcher_run_id, result, COUNT(*) FROM change_runs
	    WHERE dispatcher_run_id IN (`+placeholders(len(ids))+`) GROUP BY dispatcher_run_id, result`, ids...)
	return page, err
}

// ListIdleAgents returns the agents that are idle now, the one idle for longest first, as
// Store.ListIdleAgents does: an agent whose orchestrator died on this host is left out.
func (v *View) ListIdleAgents(ctx context.Context) ([]IdleAgent, error) {
	return v.s.listIdleAgents(ctx, v.q, v.now)
}

// ListEngineNames returns the engines the kit has on record, by name.
func (v *View) ListEngineNames(ctx context.Context) ([]string, error) {
	var names []string
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		var name string
		if err := scan(&name); err != nil {
			return err
		}
		names = append(names, name)
		return nil
	}, `SELECT engine FROM engines ORDER BY engine`)
	return names, err
}
