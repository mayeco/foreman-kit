package store

import "context"

// Scope names the rows the work running under a context belongs to: the dispatcher run, the worker's
// run of a change, the merge attempt, the execution and its attempt, the gate round, the cycle. The
// store reads it wherever it writes a row that has one of them as a parent (an event, a gate run, a
// block, an execution), so the code between the place that opens a parent and the place that writes
// a child does not have to carry identifiers.
type Scope struct {
	DispatcherRunID int64
	ChangeRunID     int64
	MergeAttemptID  int64
	GateRoundID     int64
	AttemptID       int64
	ExecID          string
	CycleStamp      string
	// Change is the change of ChangeRunID or MergeAttemptID.
	Change string
}

type scopeKey struct{}

// ScopeFrom returns the scope ctx carries (the zero Scope when it carries none).
func ScopeFrom(ctx context.Context) Scope {
	if ctx == nil {
		return Scope{}
	}
	sc, _ := ctx.Value(scopeKey{}).(Scope)
	return sc
}

func withScope(ctx context.Context, edit func(*Scope)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	sc := ScopeFrom(ctx)
	edit(&sc)
	return context.WithValue(ctx, scopeKey{}, sc)
}

// WithDispatcherRun returns ctx scoped to a dispatcher run. A run has executions of its own: one
// the caller ran under (the cycle that started the dispatch) is not what the run's rows are about.
func WithDispatcherRun(ctx context.Context, id int64) context.Context {
	return withScope(ctx, func(sc *Scope) {
		sc.DispatcherRunID = id
		sc.clearWork()
	})
}

// WithChangeRun returns ctx scoped to a worker's run of change.
func WithChangeRun(ctx context.Context, id int64, change string) context.Context {
	return withScope(ctx, func(sc *Scope) {
		sc.clearWork()
		sc.MergeAttemptID = 0
		sc.ChangeRunID = id
		sc.Change = change
	})
}

// WithMergeAttempt returns ctx scoped to a merge attempt of change.
func WithMergeAttempt(ctx context.Context, id int64, change string) context.Context {
	return withScope(ctx, func(sc *Scope) {
		sc.clearWork()
		sc.ChangeRunID = 0
		sc.MergeAttemptID = id
		sc.Change = change
	})
}

// clearWork drops what belongs to the work the context was scoped to before: its execution, the
// attempt of it and its gate round.
func (sc *Scope) clearWork() {
	sc.ExecID = ""
	sc.AttemptID = 0
	sc.GateRoundID = 0
}

// WithExecution returns ctx scoped to an execution.
func WithExecution(ctx context.Context, execID string) context.Context {
	return withScope(ctx, func(sc *Scope) { sc.ExecID = execID })
}

// WithAttempt returns ctx scoped to one attempt of the execution in scope.
func WithAttempt(ctx context.Context, attemptID int64) context.Context {
	return withScope(ctx, func(sc *Scope) { sc.AttemptID = attemptID })
}

// WithGateRound returns ctx scoped to a gate round.
func WithGateRound(ctx context.Context, id int64) context.Context {
	return withScope(ctx, func(sc *Scope) { sc.GateRoundID = id })
}

// WithCycle returns ctx scoped to an autonomous cycle.
func WithCycle(ctx context.Context, stamp string) context.Context {
	return withScope(ctx, func(sc *Scope) { sc.CycleStamp = stamp })
}

// runFor returns the change run of the scope when it is a run of change, 0 otherwise: a row of one
// change never points at the run of another.
func (sc Scope) runFor(change string) int64 {
	if sc.ChangeRunID > 0 && change != "" && sc.Change == change {
		return sc.ChangeRunID
	}
	return 0
}

// mergeFor is runFor for the merge attempt.
func (sc Scope) mergeFor(change string) int64 {
	if sc.MergeAttemptID > 0 && (change == "" || sc.Change == change) {
		return sc.MergeAttemptID
	}
	return 0
}
