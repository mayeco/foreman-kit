package store

import (
	"context"
	"database/sql"
	"sort"
	"time"
)

// sinceMS returns the instant an aggregate starts from: the beginning of the records for a zero time.
func sinceMS(since time.Time) int64 {
	if since.IsZero() {
		return 0
	}
	return since.UTC().UnixMilli()
}

// GateStat is how one gate of one repository did since a date.
//
// The umbrella validates the specs of each change with a gate that carries the name of that change.
// Those are one gate, not one for every change the backlog ever had: their runs are counted
// together, in a GateStat with no GateName whose Changes says how many changes it validated.
type GateStat struct {
	RepoPath string `json:"repo_path"`
	RepoName string `json:"repo_name"`
	GateName string `json:"gate_name"`
	// Changes is how many changes the gate ran for, when it is the one that validates the specs of
	// a change; 0 for a gate with a name.
	Changes  int `json:"changes,omitempty"`
	Runs     int `json:"runs"`
	Failures int `json:"failures"`
	// AvgMS, MaxMS and TotalMS are how long its runs took.
	AvgMS        int64  `json:"avg_ms"`
	MaxMS        int64  `json:"max_ms"`
	TotalMS      int64  `json:"total_ms"`
	LastRunAt    string `json:"last_run_at"`
	LastFailedAt string `json:"last_failed_at,omitempty"`
	// LastFailedRoundID is the latest round of gates in which it failed, 0 when it did not fail.
	LastFailedRoundID int64 `json:"last_failed_round_id,omitempty"`
}

// GateStats returns, per repository and gate, how many times the gate ran since `since`, how many
// of those failed and how long they took. A zero since covers every run on record.
func (v *View) GateStats(ctx context.Context, since time.Time) ([]GateStat, error) {
	var out []GateStat
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		var g GateStat
		var avg sql.NullFloat64
		var last int64
		var lastFailed, lastFailedRound sql.NullInt64
		if err := scan(&g.RepoPath, &g.RepoName, &g.GateName, &g.Changes, &g.Runs, &g.Failures, &avg, &g.MaxMS, &g.TotalMS,
			&last, &lastFailed, &lastFailedRound); err != nil {
			return err
		}
		g.AvgMS = int64(avg.Float64 + 0.5)
		g.LastRunAt = msToRFC3339(last)
		g.LastFailedAt = nullMSToRFC3339(lastFailed)
		g.LastFailedRoundID = lastFailedRound.Int64
		out = append(out, g)
		return nil
	}, `
		SELECT g.repo_path, p.name,
		       CASE WHEN c.change_name IS NULL THEN g.gate_name ELSE '' END AS gate,
		       COUNT(DISTINCT c.change_name), COUNT(*), COALESCE(SUM(g.exit_code <> 0), 0),
		       AVG(g.duration_ms), MAX(g.duration_ms), SUM(g.duration_ms),
		       MAX(g.started_at), MAX(CASE WHEN g.exit_code <> 0 THEN g.started_at END),
		       MAX(CASE WHEN g.exit_code <> 0 THEN g.gate_round_id END)
		FROM gate_runs g JOIN repos p ON p.repo_path = g.repo_path
		LEFT JOIN changes c ON g.repo_path = ? AND c.change_name = g.gate_name
		WHERE g.started_at >= ?
		GROUP BY g.repo_path, gate
		ORDER BY p.name, gate
	`, UmbrellaRepoPath, sinceMS(since))
	return out, err
}

// EvaluationStat is how the evaluator answered one gate since a date.
type EvaluationStat struct {
	Gate        string `json:"gate"`
	Evaluations int    `json:"evaluations"`
	// Passed counts the evaluations with status "passed"; ByStatus, every status.
	Passed   int            `json:"passed"`
	ByStatus map[string]int `json:"by_status"`
	// ByMode counts them by the mode the gate was in when it was asked: the orchestrator acts on the
	// answer of an enforced gate, and only records the answer of an advisory one.
	ByMode map[string]int `json:"by_mode"`
	// ByRoute counts the ones a rule of the gate turned into an action, by that action.
	ByRoute map[string]int `json:"by_route,omitempty"`
	// Cached counts the answers that came from the cache, which cost nothing.
	Cached  int     `json:"cached"`
	CostUSD float64 `json:"cost_usd"`
	LastAt  string  `json:"last_at"`
}

// EvaluationStats returns, per gate of the evaluator, how many questions it was asked since `since`,
// how they were answered, what the orchestrator did about the answers and what they cost. A zero
// since covers every evaluation on record.
func (v *View) EvaluationStats(ctx context.Context, since time.Time) ([]EvaluationStat, error) {
	byGate := map[string]*EvaluationStat{}
	var lasts = map[string]int64{}
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		var gate, status, mode, route string
		var n, cached int
		var cost float64
		var last int64
		if err := scan(&gate, &status, &mode, &route, &n, &cached, &cost, &last); err != nil {
			return err
		}
		st := byGate[gate]
		if st == nil {
			st = &EvaluationStat{Gate: gate, ByStatus: map[string]int{}, ByMode: map[string]int{}}
			byGate[gate] = st
		}
		st.Evaluations += n
		st.ByStatus[status] += n
		if status == "passed" {
			st.Passed += n
		}
		if mode != "" {
			st.ByMode[mode] += n
		}
		if route != "" {
			if st.ByRoute == nil {
				st.ByRoute = map[string]int{}
			}
			st.ByRoute[route] += n
		}
		st.Cached += cached
		st.CostUSD += cost
		if last > lasts[gate] {
			lasts[gate] = last
		}
		return nil
	}, `
		SELECT gate, status, COALESCE(mode, ''), COALESCE(route_action, ''),
		       COUNT(*), COALESCE(SUM(cached), 0), COALESCE(SUM(cost_usd), 0), MAX(created_at)
		FROM evaluations WHERE created_at >= ?
		GROUP BY gate, status, mode, route_action
	`, sinceMS(since))
	if err != nil {
		return nil, err
	}
	out := make([]EvaluationStat, 0, len(byGate))
	for gate, st := range byGate {
		st.LastAt = msToRFC3339(lasts[gate])
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Gate < out[j].Gate })
	return out, nil
}

// EngineStat is how much an engine worked since a date, measured in attempts, and how it behaved.
type EngineStat struct {
	Engine   string `json:"engine"`
	Attempts int    `json:"attempts"`
	// ByOutcome counts the attempts that ended, by outcome; Running, the ones still going.
	ByOutcome map[string]int `json:"by_outcome"`
	Running   int            `json:"running"`
	// WorkSeconds is how long its finished attempts took in all.
	WorkSeconds int64  `json:"work_seconds"`
	LastUsedAt  string `json:"last_used_at,omitempty"`
	// IdlePeriods counts the stretches in which the idle watchdog saw no activity from it; Stalls,
	// those that ended with the watchdog stopping it.
	IdlePeriods int `json:"idle_periods"`
	Stalls      int `json:"stalls"`
	// LongestRecoveredIdleSeconds is the longest idle stretch after which it worked again: the idle
	// limit has to stay above it.
	LongestRecoveredIdleSeconds int `json:"longest_recovered_idle_seconds"`
}

// EngineStats returns, per engine, the attempts it made since `since`, how they ended and how idle
// it was. A zero since covers every attempt on record.
func (v *View) EngineStats(ctx context.Context, since time.Time) ([]EngineStat, error) {
	byEngine := map[string]*EngineStat{}
	lasts := map[string]int64{}
	stat := func(engine string) *EngineStat {
		st := byEngine[engine]
		if st == nil {
			st = &EngineStat{Engine: engine, ByOutcome: map[string]int{}}
			byEngine[engine] = st
		}
		return st
	}
	from := sinceMS(since)
	// "+a.engine" in both GROUP BY clauses: grouping by the bare column makes SQLite walk the index of
	// (engine, started_at) from end to end, when the attempts since a date are a search on started_at.
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		var engine string
		var outcome sql.NullString
		var n int
		var last, workMS int64
		if err := scan(&engine, &outcome, &n, &last, &workMS); err != nil {
			return err
		}
		st := stat(engine)
		st.Attempts += n
		if outcome.Valid {
			st.ByOutcome[outcome.String] += n
		} else {
			st.Running += n
		}
		st.WorkSeconds += workMS / 1000
		if last > lasts[engine] {
			lasts[engine] = last
		}
		return nil
	}, `
		SELECT a.engine, a.outcome, COUNT(*), MAX(a.started_at),
		       COALESCE(SUM(CASE WHEN a.finished_at IS NOT NULL THEN a.finished_at - a.started_at ELSE 0 END), 0)
		FROM execution_attempts a WHERE a.started_at >= ?
		GROUP BY +a.engine, a.outcome
	`, from)
	if err != nil {
		return nil, err
	}
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var engine string
		var periods int
		var stalls, longest sql.NullInt64
		if err := scan(&engine, &periods, &stalls, &longest); err != nil {
			return err
		}
		st := stat(engine)
		st.IdlePeriods = periods
		st.Stalls = int(stalls.Int64)
		st.LongestRecoveredIdleSeconds = int(longest.Int64 / 1000)
		return nil
	}, `
		SELECT a.engine, COUNT(*), SUM(p.ended_by = 'stalled'),
		       MAX(CASE WHEN p.ended_by = 'activity' THEN p.ended_at - p.started_at END)
		FROM execution_attempts a JOIN attempt_idle_periods p ON p.attempt_id = a.id
		WHERE a.started_at >= ?
		GROUP BY +a.engine
	`, from)
	if err != nil {
		return nil, err
	}
	out := make([]EngineStat, 0, len(byEngine))
	for engine, st := range byEngine {
		st.LastUsedAt = msToRFC3339(lasts[engine])
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Engine < out[j].Engine })
	return out, nil
}
