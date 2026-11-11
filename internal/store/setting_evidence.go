package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"
)

// SettingEvidence is what the kit recorded, in a stretch of time, that says whether its own settings
// fit the work it did: the cycles and what each left the workers to do, the agents and gates a limit
// cut, the retries that helped and the ones that did not. It holds facts only. What follows from
// them for a setting is decided by whoever asks (the recommendations of `foreman-kit config
// recommend`), which is why nothing here names a setting's value.
type SettingEvidence struct {
	// From and To bound the stretch: From included, To excluded.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// FirstDispatch is when the first dispatcher run on record began (zero when there is none), and
	// Dispatched how long dispatchers ran in the evidenceHistory before To: the history the kit has
	// to judge from.
	FirstDispatch time.Time     `json:"first_dispatch"`
	Dispatched    time.Duration `json:"dispatched"`

	// Dispatchers are the dispatcher runs that overlap the stretch, Runs the worker runs that do,
	// and Cycles the cycles that began in it. All three are in the order they began.
	Dispatchers []EvidenceDispatcher `json:"dispatchers"`
	Runs        []EvidenceRun        `json:"runs"`
	Cycles      []EvidenceCycle      `json:"cycles"`
	// Pauses are the stretches the workspace was paused, and Deferrals the ones a change waited for
	// an engine to come back: neither is a worker with nothing to start.
	Pauses    []EvidenceSpan `json:"pauses"`
	Deferrals []EvidenceSpan `json:"deferrals"`

	// Idle is what the idle watchdog saw of the agents.
	Idle IdleEvidence `json:"idle"`
	// AgentTimeouts counts the agent calls of workers that reached their limit, AgentTimeoutsByRole
	// the same per role ("plan", "build": each role has its own limit), and GateTimeouts the gates
	// that did.
	AgentTimeouts       LimitEvidence            `json:"agent_timeouts"`
	AgentTimeoutsByRole map[string]LimitEvidence `json:"agent_timeouts_by_role"`
	GateTimeouts        LimitEvidence            `json:"gate_timeouts"`
	// BlockRetries is what became of the changes the dispatcher retried by itself after a block.
	BlockRetries BlockRetryEvidence `json:"block_retries"`
	// AgentRetries is what became of the calls tried again with the same agent.
	AgentRetries AgentRetryEvidence `json:"agent_retries"`
}

// EvidenceSpan is a stretch of time. To is zero while it lasts.
type EvidenceSpan struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// EvidenceDispatcher is one dispatcher run. EndedAt is when it ended, or, for one that left no end
// on record, the latest moment it can have run: the start of the run after it, now while its
// process holds the dispatcher's lock, and otherwise the last thing its workers recorded.
type EvidenceDispatcher struct {
	Mode      string    `json:"mode"` // "once" or "continuous"
	Workers   int       `json:"workers"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

// EvidenceRun is one pass of a worker over a change. FinishedAt is zero while it runs; a run whose
// worker is gone ends where its lease ran out.
type EvidenceRun struct {
	Change string `json:"change"`
	Origin string `json:"origin"` // "backlog", "retry" or "rebuild"
	Result string `json:"result"` // "" while it runs
	// QueuedAt is when the backlog queued the change; zero when the registry does not say.
	QueuedAt   time.Time `json:"queued_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// EvidenceCycle is one cycle, with what the pipeline did from its start to the start of the next one.
type EvidenceCycle struct {
	Stamp     string    `json:"stamp"`
	Trigger   string    `json:"trigger"`
	StartedAt time.Time `json:"started_at"`
	// FinishedAt is zero while the cycle runs.
	FinishedAt time.Time `json:"finished_at"`
	// NextStart is when the cycle after it began; zero for the latest one on record.
	NextStart time.Time `json:"next_start"`
	// FirstMove is when the first change was built, blocked or integrated after the cycle began and
	// before the next one did; zero when none was.
	FirstMove time.Time `json:"first_move"`
	// PreviousMoved says whether a change was built, blocked or integrated during the cycle before
	// this one.
	PreviousMoved bool `json:"previous_moved"`
	// QueueFull says the cycle found as many changes open as the queue takes: it announced that it
	// could queue nothing.
	QueueFull bool `json:"queue_full"`
}

// IdleEvidence is what the idle watchdog recorded: the agents it stopped, and the pauses an agent
// came back from by itself.
type IdleEvidence struct {
	Stalled int `json:"stalled"`
	Resumed int `json:"resumed"`
	// LongestResumed is the longest pause an agent went on working after.
	LongestResumed time.Duration `json:"longest_resumed"`
}

// LimitEvidence counts what a time limit cut. Cut is how many times it did. CutAgain is how many of
// those were followed by the same work being cut again (the next call of the same agent role on the
// same change; the next run of the same gate of the same repository for the same change), and
// Repeated how long those repetitions took.
type LimitEvidence struct {
	Cut      int           `json:"cut"`
	CutAgain int           `json:"cut_again"`
	Repeated time.Duration `json:"repeated"`
}

// BlockRetryEvidence counts the runs the dispatcher started by itself to retry a blocked change and
// that ended built or blocked again, and how many of them ended built.
type BlockRetryEvidence struct {
	Retried int `json:"retried"`
	Ready   int `json:"ready"`
}

// AgentRetryEvidence counts the calls tried again with the same agent after a failure. Failed is how
// long the ones that failed again took, and Waited how long the kit waited before starting them.
type AgentRetryEvidence struct {
	Retries   int           `json:"retries"`
	Succeeded int           `json:"succeeded"`
	Failed    time.Duration `json:"failed"`
	Waited    time.Duration `json:"waited"`
}

// Every statement of this read is a search: on an index of instants, on a primary key below a cursor,
// or on the partial index of what is open. None walks a table that grows with use, so the read costs
// what its stretch holds, however long the kit has run (TestNoReadOfAViewWalksATableThatGrowsWithoutBound).
const (
	// evidenceHistory is how far back the dispatcher runs are added up.
	evidenceHistory = 90 * 24 * time.Hour
	// evidenceReach is how long before a stretch a row that still matters in it can have begun: an
	// agent call that was running when it started, a change that was waiting for an engine.
	evidenceReach = 24 * time.Hour
	// evidencePage is how many rows a walk down a primary key reads at a time.
	evidencePage = 200
)

func timeOfMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func timeOfNullMS(ms sql.NullInt64) time.Time {
	if !ms.Valid {
		return time.Time{}
	}
	return timeOfMS(ms.Int64)
}

// SettingEvidence reads the evidence of the stretch from `from`, included, to `to`, excluded, as the
// database stands now.
func (s *Store) SettingEvidence(ctx context.Context, from, to time.Time) (SettingEvidence, error) {
	var ev SettingEvidence
	err := s.View(ctx, func(v *View) error {
		var err error
		ev, err = v.SettingEvidence(ctx, from, to)
		return err
	})
	return ev, err
}

// SettingEvidence reads the evidence of the stretch from `from`, included, to `to`, excluded. A zero
// `to` means up to the instant of the view.
func (v *View) SettingEvidence(ctx context.Context, from, to time.Time) (SettingEvidence, error) {
	if to.IsZero() {
		to = v.At()
	}
	ev := SettingEvidence{From: from.UTC(), To: to.UTC()}
	fromMS, toMS := from.UnixMilli(), to.UnixMilli()

	if err := v.dispatcherEvidence(ctx, &ev); err != nil {
		return ev, err
	}
	// The cycle before the stretch says whether the first cycle of the stretch followed one that
	// moved something; what the workers did is read from its start.
	lower := fromMS
	var previousStart sql.NullInt64
	if err := v.q.QueryRowContext(ctx, `SELECT MAX(started_at) FROM cycles WHERE started_at < ?`, fromMS).Scan(&previousStart); err != nil {
		return ev, err
	}
	if previousStart.Valid {
		lower = previousStart.Int64
	}
	if err := v.cycleEvidence(ctx, &ev, lower, fromMS, toMS); err != nil {
		return ev, err
	}
	if err := v.runEvidence(ctx, &ev, lower, toMS); err != nil {
		return ev, err
	}
	var err error
	// Pauses do not overlap and their ids follow time: the walk stops at the first one that ended
	// before the stretch.
	if ev.Pauses, err = v.spansBelow(ctx, lower, toMS, func(from int64, until sql.NullInt64) bool {
		return until.Valid && until.Int64 < lower
	}, `SELECT id, paused_at, resumed_at FROM workspace_pauses WHERE id < ? ORDER BY id DESC LIMIT ?`); err != nil {
		return ev, err
	}
	// Deferrals of different changes overlap: the walk goes on until they began long enough before
	// the stretch to be over. The ones still open, however old, are read from their own index.
	closed, err := v.spansBelow(ctx, lower, toMS, func(from int64, until sql.NullInt64) bool {
		return from < lower-evidenceReach.Milliseconds()
	}, `SELECT id, deferred_at, cleared_at FROM change_deferrals WHERE id < ? ORDER BY id DESC LIMIT ?`)
	if err != nil {
		return ev, err
	}
	for _, span := range closed {
		if !span.To.IsZero() {
			ev.Deferrals = append(ev.Deferrals, span)
		}
	}
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var from int64
		if err := scan(&from); err != nil {
			return err
		}
		if from < toMS {
			ev.Deferrals = append(ev.Deferrals, EvidenceSpan{From: timeOfMS(from)})
		}
		return nil
	}, `SELECT deferred_at FROM change_deferrals WHERE cleared_at IS NULL`)
	if err != nil {
		return ev, err
	}
	sort.Slice(ev.Deferrals, func(i, j int) bool { return ev.Deferrals[i].From.Before(ev.Deferrals[j].From) })
	return ev, v.limitEvidence(ctx, &ev, fromMS, toMS)
}

// spansBelow walks a table of spans down its primary key, the newest first, with a query that takes a
// cursor and a page size and returns id, start and end. It returns the spans that overlap the stretch
// from lower to toMS, in the order they began, and stops at the first row for which done holds: the
// rows below it are over.
func (v *View) spansBelow(ctx context.Context, lower, toMS int64, done func(from int64, until sql.NullInt64) bool, query string) ([]EvidenceSpan, error) {
	var out []EvidenceSpan
	cursor, finished := int64(1<<62), false
	for !finished {
		rows := 0
		err := v.each(ctx, func(scan func(dest ...any) error) error {
			var id, from int64
			var until sql.NullInt64
			if err := scan(&id, &from, &until); err != nil {
				return err
			}
			rows++
			cursor = id
			if finished || done(from, until) {
				finished = true
				return nil
			}
			if from < toMS && (!until.Valid || until.Int64 >= lower) {
				out = append(out, EvidenceSpan{From: timeOfMS(from), To: timeOfNullMS(until)})
			}
			return nil
		}, query, cursor, evidencePage)
		if err != nil {
			return nil, err
		}
		if rows < evidencePage {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From.Before(out[j].From) })
	return out, nil
}

// dispatcherEvidence reads the dispatcher runs, the newest first, as far back as evidenceHistory
// before the end of the stretch: how long they ran, and the ones that overlap the stretch.
func (v *View) dispatcherEvidence(ctx context.Context, ev *SettingEvidence) error {
	var first sql.NullInt64
	if err := v.q.QueryRowContext(ctx, `SELECT started_at FROM dispatcher_runs WHERE id > 0 ORDER BY id LIMIT 1`).Scan(&first); err != nil && err != sql.ErrNoRows {
		return err
	}
	if !first.Valid {
		return nil
	}
	ev.FirstDispatch = timeOfMS(first.Int64)

	fromMS, toMS := ev.From.UnixMilli(), ev.To.UnixMilli()
	oldest := toMS - evidenceHistory.Milliseconds()
	if fromMS < oldest {
		oldest = fromMS
	}
	type run struct {
		id, pid        int64
		mode           string
		workers        int
		started, ended int64
		open           bool
	}
	// newer is the run that began after the one being read, when there is one: a run that left no
	// end on record ended, at the latest, when that one began.
	var newer *run
	cursor, finished := int64(1<<62), false
	for !finished {
		var page []run
		err := v.each(ctx, func(scan func(dest ...any) error) error {
			var r run
			var end sql.NullInt64
			if err := scan(&r.id, &r.pid, &r.mode, &r.workers, &r.started, &end); err != nil {
				return err
			}
			r.ended, r.open = end.Int64, !end.Valid
			page = append(page, r)
			return nil
		}, `SELECT id, pid, mode, workers, started_at, finished_at FROM dispatcher_runs WHERE id < ? ORDER BY id DESC LIMIT ?`, cursor, evidencePage)
		if err != nil {
			return err
		}
		for i := range page {
			r := page[i]
			cursor = r.id
			switch {
			case !r.open:
			case newer != nil:
				r.ended = newer.started
			default:
				// The latest run: it still runs while its process holds the dispatcher's lock;
				// otherwise it ended with the last thing its workers recorded.
				alive, err := v.dispatcherAlive(ctx, r.pid)
				if err != nil {
					return err
				}
				r.ended = r.started
				if alive {
					r.ended = v.now
				} else {
					var last sql.NullInt64
					if err := v.q.QueryRowContext(ctx, `
						SELECT MAX(COALESCE(finished_at, started_at)) FROM change_runs WHERE dispatcher_run_id = ?
					`, r.id).Scan(&last); err != nil {
						return err
					}
					if last.Valid && last.Int64 > r.started {
						r.ended = last.Int64
					}
				}
			}
			newer = &page[i]
			if r.ended < oldest {
				finished = true
				break
			}
			if from, until := max(r.started, oldest), min(r.ended, toMS); until > from {
				ev.Dispatched += time.Duration(until-from) * time.Millisecond
			}
			if r.started < toMS && r.ended >= fromMS {
				ev.Dispatchers = append(ev.Dispatchers, EvidenceDispatcher{
					Mode: r.mode, Workers: r.workers, StartedAt: timeOfMS(r.started), EndedAt: timeOfMS(r.ended),
				})
			}
		}
		if len(page) < evidencePage {
			break
		}
	}
	sort.Slice(ev.Dispatchers, func(i, j int) bool { return ev.Dispatchers[i].StartedAt.Before(ev.Dispatchers[j].StartedAt) })
	return nil
}

// dispatcherAlive reports whether the process pid holds one of the dispatcher's locks now.
func (v *View) dispatcherAlive(ctx context.Context, pid int64) (bool, error) {
	alive := false
	nowUnix := v.now / 1000
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		var rec LockRecord
		var acquired, heartbeat int64
		if err := scan(&rec.LockKey, &rec.OwnerType, &rec.PID, &rec.Host, &acquired, &heartbeat, &rec.TTLSeconds); err != nil {
			return err
		}
		rec.AcquiredAt, rec.HeartbeatAt = acquired/1000, heartbeat/1000
		if !IsLockStale(rec, v.s.host, nowUnix) {
			alive = true
		}
		return nil
	}, `
		SELECT lock_key, owner_type, pid, host, acquired_at, heartbeat_at, ttl_seconds
		FROM locks WHERE lock_key IN ('dispatcher', 'dispatch') AND pid = ?
	`, pid)
	return alive, err
}

// cycleEvidence reads the cycles that began in the stretch, with what moved during each and during
// the one before it.
func (v *View) cycleEvidence(ctx context.Context, ev *SettingEvidence, lower, fromMS, toMS int64) error {
	type cycle struct {
		EvidenceCycle
		started int64
	}
	var cycles []cycle // from the one before the stretch on
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		var c cycle
		var trigger sql.NullString
		var finished sql.NullInt64
		if err := scan(&c.Stamp, &trigger, &c.started, &finished); err != nil {
			return err
		}
		c.Trigger, c.StartedAt, c.FinishedAt = trigger.String, timeOfMS(c.started), timeOfNullMS(finished)
		cycles = append(cycles, c)
		return nil
	}, `SELECT stamp, trigger, started_at, finished_at FROM cycles WHERE started_at >= ? AND started_at < ? ORDER BY started_at, stamp`, lower, toMS)
	if err != nil || len(cycles) == 0 {
		return err
	}
	// The cycle that began after the stretch, when there is one, bounds the last cycle of the stretch.
	var after sql.NullInt64
	if err := v.q.QueryRowContext(ctx, `SELECT MIN(started_at) FROM cycles WHERE started_at >= ?`, toMS).Scan(&after); err != nil {
		return err
	}

	// Every instant something moved, from the first of those cycles on.
	var moves []int64
	upper := toMS
	if after.Valid {
		upper = after.Int64
	}
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var at int64
		if err := scan(&at); err != nil {
			return err
		}
		moves = append(moves, at)
		return nil
	}, `
		SELECT finished_at FROM change_runs WHERE result = 'ready' AND finished_at >= ?1 AND finished_at < ?2
		UNION ALL
		SELECT blocked_at FROM change_blocks WHERE blocked_at >= ?1 AND blocked_at < ?2
		UNION ALL
		SELECT finished_at FROM merge_attempts WHERE result = 'integrated' AND finished_at >= ?1 AND finished_at < ?2
		ORDER BY 1
	`, lower, upper)
	if err != nil {
		return err
	}

	full := map[string]bool{}
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var stamp sql.NullString
		if err := scan(&stamp); err != nil {
			return err
		}
		full[stamp.String] = true
		return nil
	}, `
		SELECT cycle_stamp FROM events
		WHERE event = 'cycle.queue_limited' AND ts >= ? AND ts < ? AND json_extract(payload_json, '$.findings') = 0
	`, lower, upper)
	if err != nil {
		return err
	}

	// firstMove returns the first instant something moved from `from` to `until`, or 0.
	firstMove := func(from, until int64) int64 {
		i := sort.Search(len(moves), func(i int) bool { return moves[i] >= from })
		if i < len(moves) && moves[i] < until {
			return moves[i]
		}
		return 0
	}
	for i, c := range cycles {
		next := upper
		if i+1 < len(cycles) {
			next = cycles[i+1].started
			c.NextStart = timeOfMS(next)
		} else if after.Valid {
			c.NextStart = timeOfMS(after.Int64)
		}
		if at := firstMove(c.started, next); at > 0 {
			c.FirstMove = timeOfMS(at)
		}
		if i > 0 {
			c.PreviousMoved = firstMove(cycles[i-1].started, c.started) > 0
		}
		c.QueueFull = full[c.Stamp]
		if c.started >= fromMS {
			ev.Cycles = append(ev.Cycles, c.EvidenceCycle)
		}
	}
	return nil
}

// runEvidence reads the worker runs that overlap the stretch from lower to toMS.
func (v *View) runEvidence(ctx context.Context, ev *SettingEvidence, lower, toMS int64) error {
	// An open run whose worker is gone did not build all the time since: it ends where its lease
	// ran out, or where it began when nothing says more.
	lost := map[int64]int64{}
	gone, err := v.ListLostRuns(ctx)
	if err != nil {
		return err
	}
	for _, l := range gone {
		lost[l.ID] = rfc3339ToMS(l.LeaseExpiredAt)
	}
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var r EvidenceRun
		var id, started int64
		var result sql.NullString
		var finished, queued sql.NullInt64
		if err := scan(&id, &r.Change, &r.Origin, &result, &started, &finished, &queued); err != nil {
			return err
		}
		r.Result, r.StartedAt, r.FinishedAt, r.QueuedAt = result.String, timeOfMS(started), timeOfNullMS(finished), timeOfNullMS(queued)
		if expired, isLost := lost[id]; isLost && !finished.Valid {
			r.Result, r.FinishedAt = "lost", r.StartedAt
			if expired > started {
				r.FinishedAt = timeOfMS(expired)
			}
		}
		ev.Runs = append(ev.Runs, r)
		return nil
	}, `
		SELECT r.id, r.change_name, r.origin, r.result, r.started_at, r.finished_at, c.queued_at
		FROM change_runs r JOIN changes c ON c.change_name = r.change_name
		WHERE r.finished_at >= ?1 AND r.started_at < ?2
		UNION ALL
		SELECT r.id, r.change_name, r.origin, r.result, r.started_at, r.finished_at, c.queued_at
		FROM change_runs r JOIN changes c ON c.change_name = r.change_name
		WHERE r.finished_at IS NULL AND r.started_at < ?2
	`, lower, toMS)
	if err != nil {
		return err
	}
	sort.SliceStable(ev.Runs, func(i, j int) bool { return ev.Runs[i].StartedAt.Before(ev.Runs[j].StartedAt) })
	return nil
}

// limitEvidence reads what the limits and the retries did in the stretch.
func (v *View) limitEvidence(ctx context.Context, ev *SettingEvidence, fromMS, toMS int64) error {
	// A row that matters in the stretch belongs to an agent call that began at most this long
	// before it.
	reachMS := fromMS - evidenceReach.Milliseconds()

	// The idle periods are reached through the calls they belong to, which have an index of instants.
	var longest sql.NullInt64
	if err := v.q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(p.ended_by = 'stalled'), 0), COALESCE(SUM(p.ended_by = 'activity'), 0),
		       MAX(CASE WHEN p.ended_by = 'activity' THEN p.ended_at - p.started_at END)
		FROM execution_attempts a JOIN attempt_idle_periods p ON p.attempt_id = a.id
		WHERE a.started_at >= ?1 AND a.started_at < ?3 AND p.started_at >= ?2 AND p.started_at < ?3
	`, reachMS, fromMS, toMS).Scan(&ev.Idle.Stalled, &ev.Idle.Resumed, &longest); err != nil {
		return err
	}
	ev.Idle.LongestResumed = time.Duration(longest.Int64) * time.Millisecond

	// An agent call is followed by the next call of the same role on the same change, whether the
	// same execution tried again or a later one did. CROSS JOIN keeps the calls of the stretch as
	// the outer loop: left to choose, SQLite walks every worker execution on record instead.
	rows, err := v.q.QueryContext(ctx, `
		WITH calls AS (
			SELECT a.outcome, COALESCE(NULLIF(e.role, ''), e.phase) AS role,
			       LEAD(a.outcome) OVER w AS next_outcome,
			       LEAD(a.finished_at - a.started_at) OVER w AS next_ms
			FROM execution_attempts a CROSS JOIN executions e ON e.exec_id = a.exec_id
			WHERE a.started_at >= ?1 AND a.started_at < ?2 AND a.finished_at IS NOT NULL AND e.kind = 'worker'
			WINDOW w AS (PARTITION BY e.change_name, COALESCE(NULLIF(e.role, ''), e.phase) ORDER BY a.started_at, a.id)
		)
		SELECT role, COUNT(*), COALESCE(SUM(next_outcome = 'timeout'), 0),
		       COALESCE(SUM(CASE WHEN next_outcome = 'timeout' THEN next_ms END), 0)
		FROM calls WHERE outcome = 'timeout' GROUP BY role
	`, fromMS, toMS)
	if err != nil {
		return err
	}
	ev.AgentTimeoutsByRole = map[string]LimitEvidence{}
	for rows.Next() {
		var role sql.NullString
		var cut, cutAgain int
		var repeatedMS int64
		if err := rows.Scan(&role, &cut, &cutAgain, &repeatedMS); err != nil {
			rows.Close()
			return err
		}
		// A call recorded by its phase alone belongs to the role that runs the phase.
		name := "build"
		if strings.HasPrefix(role.String, "plan") {
			name = "plan"
		}
		of := ev.AgentTimeoutsByRole[name]
		of.Cut += cut
		of.CutAgain += cutAgain
		of.Repeated += time.Duration(repeatedMS) * time.Millisecond
		ev.AgentTimeoutsByRole[name] = of
		ev.AgentTimeouts.Cut += cut
		ev.AgentTimeouts.CutAgain += cutAgain
		ev.AgentTimeouts.Repeated += time.Duration(repeatedMS) * time.Millisecond
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	var repeated int64
	// A gate is repeated when the same gate of the same repository runs again for the same change.
	// Exit code 124 is the one a command gets when its time limit stops it.
	if err := v.q.QueryRowContext(ctx, `
		WITH gates AS (
			SELECT g.exit_code,
			       LEAD(g.exit_code) OVER w AS next_exit,
			       LEAD(g.duration_ms) OVER w AS next_ms
			FROM gate_runs g JOIN gate_rounds r ON r.id = g.gate_round_id
			WHERE g.started_at >= ?1 AND g.started_at < ?2 AND r.change_name IS NOT NULL
			WINDOW w AS (PARTITION BY r.change_name, g.repo_path, g.gate_name ORDER BY g.started_at, g.id)
		)
		SELECT COUNT(*), COALESCE(SUM(next_exit = 124), 0),
		       COALESCE(SUM(CASE WHEN next_exit = 124 THEN next_ms END), 0)
		FROM gates WHERE exit_code = 124
	`, fromMS, toMS).Scan(&ev.GateTimeouts.Cut, &ev.GateTimeouts.CutAgain, &repeated); err != nil {
		return err
	}
	ev.GateTimeouts.Repeated = time.Duration(repeated) * time.Millisecond

	// The run that retries a block is the one that names it; a run that never got to build (held,
	// deferred, stopped) says nothing about the retry.
	if err := v.q.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(r.result = 'ready'), 0)
		FROM change_runs r JOIN change_blocks b ON b.id = r.retry_of_block_id
		WHERE r.started_at >= ? AND r.started_at < ? AND r.origin = 'retry' AND r.result IN ('ready', 'blocked')
		  AND b.resolution = 'auto_retry'
	`, fromMS, toMS).Scan(&ev.BlockRetries.Retried, &ev.BlockRetries.Ready); err != nil {
		return err
	}

	// try_no counts the tries of one agent within a call: from 1 on it is a retry. The try before it
	// may have begun before the stretch.
	var failed, waited int64
	if err := v.q.QueryRowContext(ctx, `
		WITH tries AS (
			SELECT a.started_at, a.finished_at, a.try_no, a.outcome,
			       LAG(a.finished_at) OVER (PARTITION BY a.exec_id ORDER BY a.attempt_no) AS previous_end
			FROM execution_attempts a
			WHERE a.started_at >= ?1 AND a.started_at < ?3 AND a.finished_at IS NOT NULL
		)
		SELECT COUNT(*), COALESCE(SUM(outcome = 'ok'), 0),
		       COALESCE(SUM(CASE WHEN outcome <> 'ok' THEN finished_at - started_at END), 0),
		       COALESCE(SUM(CASE WHEN outcome <> 'ok' AND previous_end IS NOT NULL AND started_at > previous_end
		                         THEN started_at - previous_end END), 0)
		FROM tries WHERE try_no >= 1 AND started_at >= ?2
	`, reachMS, fromMS, toMS).Scan(&ev.AgentRetries.Retries, &ev.AgentRetries.Succeeded, &failed, &waited); err != nil {
		return err
	}
	ev.AgentRetries.Failed = time.Duration(failed) * time.Millisecond
	ev.AgentRetries.Waited = time.Duration(waited) * time.Millisecond
	return nil
}
