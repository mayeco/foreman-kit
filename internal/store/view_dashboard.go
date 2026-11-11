package store

import (
	"context"
	"database/sql"
	"math"
	"sort"
)

// RunningRun is a worker run that is open, with what it runs now.
type RunningRun struct {
	ChangeRun
	// Execution is the execution of the run that runs now: its PLAN, its BUILD or a repair. It is nil
	// between two of them, while the worker runs the gates or waits for the evaluator.
	Execution *ExecutionDetail `json:"execution,omitempty"`
	// Lost says nothing holds the lease of the change: its worker is gone, and the run stays open
	// until the lease is given back.
	Lost bool `json:"lost,omitempty"`
}

// Kinds of outcome.
const (
	OutcomeRun   = "run"
	OutcomeMerge = "merge"
	OutcomeBlock = "block"
	OutcomeCycle = "cycle"
)

// Outcome is something the kit closed: a worker run or a merge attempt that ended, a block that
// stopped a change, a cycle that finished. One of Run, Merge, Block and Cycle is set, by Kind.
type Outcome struct {
	Kind string `json:"kind"`
	// At is when it happened.
	At    string        `json:"at"`
	Run   *ChangeRun    `json:"run,omitempty"`
	Merge *MergeAttempt `json:"merge,omitempty"`
	Block *BlockRecord  `json:"block,omitempty"`
	Cycle *Cycle        `json:"cycle,omitempty"`
}

// Dashboard is what the kit is doing at the instant of the view, and what it closed last: what the
// portal's Dashboard shows.
type Dashboard struct {
	// Counts is how many changes are in each state of ChangeStates.
	Counts map[string]int `json:"counts"`
	// Runs holds the worker runs that are open, the one open for longest first.
	Runs []RunningRun `json:"runs"`
	// Merge is the merge attempt in progress, nil while the merger is idle.
	Merge *MergeAttempt `json:"merge,omitempty"`
	// Cycle is the cycle that runs now, nil between two cycles. LastCycle is the latest that ended.
	Cycle     *Cycle `json:"cycle,omitempty"`
	LastCycle *Cycle `json:"last_cycle,omitempty"`
	// Executions holds what runs now outside a worker run, the latest first: the judgment of the
	// cycle, the agents and the operations a person started.
	Executions []ExecutionDetail `json:"executions"`
	// Idle holds the agents that show no activity now, the one idle for longest first.
	Idle []IdleAgent `json:"idle"`
	// Pause is the pause in force, nil when the workspace is not paused.
	Pause *Pause `json:"pause,omitempty"`
	// Recent holds the latest outcomes, the latest first.
	Recent []Outcome `json:"recent"`
}

// Results of a worker run and of a merge attempt that close something, as Dashboard.Recent lists them. A
// run that was held or deferred did no work: the dispatcher takes the change again by itself, and
// every pass would add one. A run or an attempt that ended blocked is told by its block, which says
// why. An attempt that was held or stopped is tried again as it is.
const (
	closingRunResults   = `'ready', 'paused', 'stopped', 'lost'`
	closingMergeResults = `'integrated', 'already_integrated', 'sent_back', 'push_rejected'`
)

// Dashboard returns what the kit is doing, with its latest recent outcomes. Every list of outcomes is a
// search below an instant on the index of when they happened: none walks its table.
func (v *View) Dashboard(ctx context.Context, recent int) (Dashboard, error) {
	now := Dashboard{Runs: []RunningRun{}, Executions: []ExecutionDetail{}, Idle: []IdleAgent{}, Recent: []Outcome{}}
	var err error
	if now.Counts, err = v.CountChangesByState(ctx, ""); err != nil {
		return now, err
	}

	// The open runs. No ORDER BY: with one, SQLite walks every run in that order instead of reading
	// the open ones from their index. They are few, and are sorted here.
	var started []int64
	at := map[int64]int{}
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var ms int64
		r, err := scanChangeRun(func(dest ...any) error { return scan(append(dest, &ms)...) })
		if err == nil {
			now.Runs = append(now.Runs, RunningRun{ChangeRun: r})
			started = append(started, ms)
		}
		return err
	}, `SELECT `+changeRunColumns+`, r.started_at FROM change_runs r WHERE r.finished_at IS NULL`)
	if err != nil {
		return now, err
	}
	sort.Sort(byStart{runs: now.Runs, started: started})
	for i, r := range now.Runs {
		at[r.ID] = i
	}
	lost, err := v.ListLostRuns(ctx)
	if err != nil {
		return now, err
	}
	for _, l := range lost {
		if i, ok := at[l.ID]; ok {
			now.Runs[i].Lost = true
		}
	}

	// What runs now, from the index of the executions that are open: the one of each run goes with
	// its run, the rest stand on their own.
	byExec := map[string]*ExecutionDetail{}
	var running []*ExecutionDetail
	since := map[string]int64{}
	var ids []any
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var ms int64
		e, err := scanExecution(func(dest ...any) error { return scan(append(dest, &ms)...) })
		if err == nil {
			d := v.executionDetail(e)
			running = append(running, d)
			byExec[d.ExecID], since[d.ExecID] = d, ms
			ids = append(ids, d.ExecID)
		}
		return err
	}, `SELECT `+executionListColumns+`, e.started_at FROM executions e WHERE e.status = 'running'`)
	if err != nil {
		return now, err
	}
	if err := v.loadAttempts(ctx, ids, byExec); err != nil {
		return now, err
	}
	sort.SliceStable(running, func(i, j int) bool {
		if a, b := since[running[i].ExecID], since[running[j].ExecID]; a != b {
			return a > b
		}
		return running[i].ExecID > running[j].ExecID
	})
	for _, d := range running {
		if i, ok := at[d.ChangeRunID]; ok && d.ChangeRunID != 0 {
			// The latest one started is the one the run is on: the list is sorted that way.
			if now.Runs[i].Execution == nil {
				now.Runs[i].Execution = d
			}
			continue
		}
		now.Executions = append(now.Executions, *d)
	}

	const attempts = ` FROM merge_attempts m JOIN ready_manifests r ON r.id = m.manifest_id`
	cur, err := scanMergeAttempt(v.q.QueryRowContext(ctx, `SELECT `+mergeAttemptColumns+attempts+` WHERE m.finished_at IS NULL`).Scan)
	switch {
	case err == sql.ErrNoRows:
	case err != nil:
		return now, err
	default:
		now.Merge = &cur
	}

	// A cycle left open by a process that died does not run: the next cycle closes it.
	open, err := listCyclesOn(ctx, v.q, ` WHERE finished_at IS NULL ORDER BY started_at DESC, stamp DESC LIMIT 1`)
	if err != nil {
		return now, err
	}
	if len(open) == 1 && !(open[0].PID > 0 && (open[0].Host == "" || open[0].Host == v.s.host) && !IsPIDAlive(open[0].PID)) {
		now.Cycle = &open[0]
	}

	if now.Idle, err = v.ListIdleAgents(ctx); err != nil {
		return now, err
	}
	if now.Idle == nil {
		now.Idle = []IdleAgent{}
	}
	if now.Pause, err = v.OpenPause(ctx); err != nil {
		return now, err
	}

	if recent <= 0 {
		recent = 10
	}
	if recent > MaxPageSize {
		recent = MaxPageSize
	}
	var outcomes []Outcome
	var when []int64
	add := func(o Outcome, ms int64) {
		o.At = msToRFC3339(ms)
		outcomes = append(outcomes, o)
		when = append(when, ms)
	}
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var ms int64
		r, err := scanChangeRun(func(dest ...any) error { return scan(append(dest, &ms)...) })
		if err == nil {
			add(Outcome{Kind: OutcomeRun, Run: &r}, ms)
		}
		return err
	}, `SELECT `+changeRunColumns+`, r.finished_at FROM change_runs r
	    WHERE r.finished_at < ? AND r.result IN (`+closingRunResults+`)
	    ORDER BY r.finished_at DESC, r.id DESC LIMIT ?`, int64(math.MaxInt64), recent)
	if err != nil {
		return now, err
	}
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var ms int64
		a, err := scanMergeAttempt(func(dest ...any) error { return scan(append(dest, &ms)...) })
		if err == nil {
			add(Outcome{Kind: OutcomeMerge, Merge: &a}, ms)
		}
		return err
	}, `SELECT `+mergeAttemptColumns+`, m.finished_at`+attempts+`
	    WHERE m.finished_at < ? AND m.result IN (`+closingMergeResults+`)
	    ORDER BY m.finished_at DESC, m.id DESC LIMIT ?`, int64(math.MaxInt64), recent)
	if err != nil {
		return now, err
	}
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var ms int64
		b, err := scanBlockRecord(func(dest ...any) error { return scan(append(dest, &ms)...) })
		if err == nil {
			add(Outcome{Kind: OutcomeBlock, Block: &b}, ms)
		}
		return err
	}, `SELECT `+blockRecordColumns+`, blocked_at FROM change_blocks WHERE blocked_at < ?
	    ORDER BY blocked_at DESC, id DESC LIMIT ?`, int64(math.MaxInt64), recent)
	if err != nil {
		return now, err
	}
	finished, instants, err := listCyclesTimed(ctx, v.q, ` WHERE started_at < ? AND finished_at IS NOT NULL
	    ORDER BY started_at DESC, stamp DESC LIMIT ?`, int64(math.MaxInt64), recent)
	if err != nil {
		return now, err
	}
	for i := range finished {
		if i == 0 {
			now.LastCycle = &finished[0]
		}
		add(Outcome{Kind: OutcomeCycle, Cycle: &finished[i]}, instants[i].ended)
	}

	order := make([]int, len(outcomes))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return when[order[a]] > when[order[b]] })
	if len(order) > recent {
		order = order[:recent]
	}
	for _, i := range order {
		now.Recent = append(now.Recent, outcomes[i])
	}
	return now, nil
}

// byStart sorts the open runs by when they started, the first one first.
type byStart struct {
	runs    []RunningRun
	started []int64
}

func (s byStart) Len() int { return len(s.runs) }
func (s byStart) Less(i, j int) bool {
	if s.started[i] != s.started[j] {
		return s.started[i] < s.started[j]
	}
	return s.runs[i].ID < s.runs[j].ID
}
func (s byStart) Swap(i, j int) {
	s.runs[i], s.runs[j] = s.runs[j], s.runs[i]
	s.started[i], s.started[j] = s.started[j], s.started[i]
}
