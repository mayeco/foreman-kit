package store

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

// stillOpen, as the minute something ended, says that it has not.
var stillOpen = math.Inf(1)

// evidenceSeed writes rows through the store's own functions and then places them in time, since the
// store stamps what it writes with the clock.
type evidenceSeed struct {
	t    *testing.T
	st   *Store
	ctx  context.Context
	base time.Time
}

func newEvidenceSeed(t *testing.T) evidenceSeed {
	t.Helper()
	return evidenceSeed{t: t, st: openMovementStore(t), ctx: context.Background(), base: time.Now().UTC().Add(-12 * time.Hour).Truncate(time.Second)}
}

// at is the instant so many minutes after the base.
func (s evidenceSeed) at(minutes float64) time.Time {
	return s.base.Add(time.Duration(minutes * float64(time.Minute)))
}

func (s evidenceSeed) ms(minutes float64) int64 { return s.at(minutes).UnixMilli() }

func (s evidenceSeed) exec(query string, args ...any) {
	s.t.Helper()
	if _, err := s.st.db.ExecContext(s.ctx, query, args...); err != nil {
		s.t.Fatalf("%s: %v", query, err)
	}
}

func (s evidenceSeed) lastID(table string) int64 {
	s.t.Helper()
	var id int64
	if err := s.st.db.QueryRowContext(s.ctx, `SELECT MAX(id) FROM `+table).Scan(&id); err != nil {
		s.t.Fatal(err)
	}
	return id
}

// dispatcher records a dispatcher run from start to end (stillOpen leaves it open).
func (s evidenceSeed) dispatcher(mode string, workers int, start, end float64) int64 {
	s.t.Helper()
	id, err := s.st.BeginDispatcherRun(s.ctx, mode, workers, 0)
	if err != nil {
		s.t.Fatal(err)
	}
	s.exec(`UPDATE dispatcher_runs SET started_at = ?, pid = ? WHERE id = ?`, s.ms(start), 900000+id, id)
	if end != stillOpen {
		s.exec(`UPDATE dispatcher_runs SET finished_at = ?, end_reason = 'done' WHERE id = ?`, s.ms(end), id)
	}
	return id
}

// run records a worker run of change from start to end with result (stillOpen leaves it open).
func (s evidenceSeed) run(dispatcher int64, change string, start, end float64, result string) int64 {
	s.t.Helper()
	id, err := s.st.BeginChangeRun(WithDispatcherRun(s.ctx, dispatcher), change, 1, "/wt/"+change, "feat/spec-"+change)
	if err != nil {
		s.t.Fatal(err)
	}
	s.exec(`UPDATE change_runs SET started_at = ? WHERE id = ?`, s.ms(start), id)
	if end != stillOpen {
		s.exec(`UPDATE change_runs SET finished_at = ?, result = ? WHERE id = ?`, s.ms(end), result, id)
	}
	return id
}

func (s evidenceSeed) cycle(stamp string, start, end float64) {
	s.t.Helper()
	if err := s.st.BeginCycle(s.ctx, Cycle{Stamp: stamp, Trigger: CycleTriggerInterval, StartedAt: s.at(start).Format(time.RFC3339Nano)}); err != nil {
		s.t.Fatal(err)
	}
	if end != stillOpen {
		if err := s.st.FinishCycle(s.ctx, stamp, "done", s.at(end).Format(time.RFC3339Nano)); err != nil {
			s.t.Fatal(err)
		}
	}
}

// block records that change was blocked at the given minute.
func (s evidenceSeed) block(change string, at float64) int64 {
	s.t.Helper()
	if err := s.st.BlockChange(s.ctx, QuarantineRecord{ChangeName: change, Reason: "gate_failure"}); err != nil {
		s.t.Fatal(err)
	}
	id := s.lastID("change_blocks")
	s.exec(`UPDATE change_blocks SET blocked_at = ? WHERE id = ?`, s.ms(at), id)
	return id
}

// retried records that the dispatcher retried the open block of change at the given minute, and that
// the run of the retry ended with result.
func (s evidenceSeed) retried(dispatcher int64, change string, blockedAt, retriedAt float64, result string) {
	s.t.Helper()
	blockID := s.block(change, blockedAt)
	if _, err := s.st.AdvanceQuarantineRetry(s.ctx, change); err != nil {
		s.t.Fatal(err)
	}
	s.exec(`UPDATE change_blocks SET resolved_at = ? WHERE id = ?`, s.ms(retriedAt), blockID)
	runID := s.run(dispatcher, change, retriedAt, retriedAt+5, result)
	var origin string
	var retryOf int64
	if err := s.st.db.QueryRowContext(s.ctx, `SELECT origin, COALESCE(retry_of_block_id, 0) FROM change_runs WHERE id = ?`, runID).Scan(&origin, &retryOf); err != nil {
		s.t.Fatal(err)
	}
	if origin != "retry" || retryOf != blockID {
		s.t.Fatalf("the run after the retry of %s is recorded as %q of block %d, want a retry of block %d", change, origin, retryOf, blockID)
	}
}

// execution opens an execution of kind for change (a cycle stamp when kind is "cycle").
func (s evidenceSeed) execution(id, kind, change, role, phase string) {
	s.t.Helper()
	e := Execution{ExecID: id, Kind: kind, Role: role, Phase: phase, Status: "running", PID: 1}
	if kind == "cycle" {
		e.CycleStamp = change
	} else {
		e.ChangeName = change
	}
	if err := s.st.CreateExecution(s.ctx, e); err != nil {
		s.t.Fatal(err)
	}
}

// attempt records a try of an execution from start to end with outcome. tryNo counts the tries of
// the same agent, from 0.
func (s evidenceSeed) attempt(execID string, tryNo int, start, end float64, outcome string) int64 {
	s.t.Helper()
	id, err := s.st.BeginAttempt(s.ctx, execID, "claude", tryNo)
	if err != nil {
		s.t.Fatal(err)
	}
	exit := 0
	if outcome != "ok" {
		exit = 1
	}
	if outcome == "timeout" {
		exit = 124
	}
	if err := s.st.FinishAttempt(s.ctx, id, exit, outcome, 0); err != nil {
		s.t.Fatal(err)
	}
	s.exec(`UPDATE execution_attempts SET started_at = ?, finished_at = ? WHERE id = ?`, s.ms(start), s.ms(end), id)
	return id
}

// idle records an idle period of an attempt from start to end, ended as told.
func (s evidenceSeed) idle(attemptID int64, start, end float64, endedBy string) {
	s.t.Helper()
	s.exec(`INSERT INTO attempt_idle_periods (attempt_id, started_at, limit_seconds, ended_at, ended_by) VALUES (?, ?, 1200, ?, ?)`,
		attemptID, s.ms(start), s.ms(end), endedBy)
}

// gate records a run of a gate for change that began at start, took minutes and exited with code.
func (s evidenceSeed) gate(change, name string, start, minutes float64, code int) {
	s.t.Helper()
	if err := s.st.RecordGateRun(s.ctx, GateRunRecord{ChangeName: change, Context: "cli", RepoPath: "repos/backend", RepoName: "backend", GateName: name, Command: "make " + name, ExitCode: code}); err != nil {
		s.t.Fatal(err)
	}
	s.exec(`UPDATE gate_runs SET started_at = ?, finished_at = ?, duration_ms = ? WHERE id = ?`,
		s.ms(start), s.ms(start+minutes), int64(minutes*60000), s.lastID("gate_runs"))
}

func sameInstant(a, b time.Time) bool { return a.UnixMilli() == b.UnixMilli() }

// The evidence of a stretch says how long the kit has dispatched in all, which dispatchers, worker
// runs and cycles belong to the stretch, and what moved during each cycle and the one before it.
func TestSettingEvidenceReadsWhatTheWorkersHadToDo(t *testing.T) {
	s := newEvidenceSeed(t)

	// Three dispatchers: one that ended long before the stretch, one left open that the next one
	// superseded, and the latest, left open by a process that is gone.
	s.dispatcher("continuous", 2, -1800, -240)
	d2 := s.dispatcher("continuous", 2, -60, stillOpen)
	d3 := s.dispatcher("continuous", 3, 300, stillOpen)

	s.cycle("c0", -50, -45)
	s.cycle("c1", 10, 17)
	s.cycle("c2", 70, 78)
	s.cycle("c3", 130, stillOpen)
	s.cycle("c4", 540, 545) // after the stretch: it bounds the last cycle of it

	s.block("blocked-before", -20)            // moved during c0
	s.run(d2, "built-in-c1", 18, 25, "ready") // moved during c1
	s.run(d2, "ran-across-the-start", -10, 5, "stopped")
	s.run(d2, "ended-long-before", -180, -120, "ready") // not in the stretch
	s.run(d2, "held-run", 80, 80.1, "held")
	s.run(d3, "left-open", 310, stillOpen, "")
	s.exec(`UPDATE changes SET queued_at = ? WHERE change_name = 'built-in-c1'`, s.ms(17))

	// c2 found the queue full; c1 only had less room than its findings.
	for stamp, findings := range map[string]int{"c1": 1, "c2": 0} {
		at := map[string]float64{"c1": 10.1, "c2": 70.1}[stamp]
		if err := s.st.EmitEvent(s.ctx, EventInput{TS: s.at(at), Event: "cycle.queue_limited", CycleStamp: stamp, Payload: map[string]any{"open": 8, "max_open": 8, "findings": findings}}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.st.Pause(s.ctx, "by a test", "cli", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.Resume(s.ctx, "cli"); err != nil {
		t.Fatal(err)
	}
	s.exec(`UPDATE workspace_pauses SET paused_at = ?, resumed_at = ?`, s.ms(120), s.ms(150))
	if err := s.st.RecordDeferral(s.ctx, "waits-for-an-engine", "build", true, s.at(180), s.at(195), ""); err != nil {
		t.Fatal(err)
	}

	ev, err := s.st.SettingEvidence(s.ctx, s.at(0), s.at(480))
	if err != nil {
		t.Fatal(err)
	}
	if !sameInstant(ev.From, s.at(0)) || !sameInstant(ev.To, s.at(480)) {
		t.Fatalf("the stretch is %s to %s", ev.From, ev.To)
	}

	// History: 26 hours of the first dispatcher, 6 of the second (until the third began), and the
	// third until the last thing its workers recorded, which is the start of the run it left open.
	if !sameInstant(ev.FirstDispatch, s.at(-1800)) {
		t.Errorf("the first dispatch is %s, want %s", ev.FirstDispatch, s.at(-1800))
	}
	if want := 26*time.Hour + 6*time.Hour + 10*time.Minute; ev.Dispatched != want {
		t.Errorf("dispatched %s in all, want %s", ev.Dispatched, want)
	}
	if len(ev.Dispatchers) != 2 || ev.Dispatchers[0].Workers != 2 || !sameInstant(ev.Dispatchers[0].EndedAt, s.at(300)) ||
		ev.Dispatchers[1].Workers != 3 || !sameInstant(ev.Dispatchers[1].EndedAt, s.at(310)) || ev.Dispatchers[1].Mode != "continuous" {
		t.Errorf("the dispatchers of the stretch: %+v", ev.Dispatchers)
	}

	// Cycles: the three that began in the stretch, each bounded by the next.
	if len(ev.Cycles) != 3 {
		t.Fatalf("got %d cycles, want c1, c2 and c3: %+v", len(ev.Cycles), ev.Cycles)
	}
	c1, c2, c3 := ev.Cycles[0], ev.Cycles[1], ev.Cycles[2]
	if c1.Stamp != "c1" || !sameInstant(c1.FinishedAt, s.at(17)) || !sameInstant(c1.NextStart, s.at(70)) || !sameInstant(c1.FirstMove, s.at(25)) || !c1.PreviousMoved || c1.QueueFull {
		t.Errorf("c1: %+v", c1)
	}
	if c2.Stamp != "c2" || !c2.FirstMove.IsZero() || !c2.PreviousMoved || !c2.QueueFull || c2.Trigger != CycleTriggerInterval {
		t.Errorf("c2: %+v", c2)
	}
	if c3.Stamp != "c3" || !c3.FinishedAt.IsZero() || !sameInstant(c3.NextStart, s.at(540)) || c3.PreviousMoved || c3.QueueFull {
		t.Errorf("c3: %+v", c3)
	}

	// Runs: from the start of the cycle before the stretch on. The one left open by a worker that is
	// gone is lost, and lasted no longer than the records say.
	var runs []string
	for _, r := range ev.Runs {
		runs = append(runs, fmt.Sprintf("%s=%s", r.Change, r.Result))
	}
	if got := fmt.Sprint(runs); got != "[ran-across-the-start=stopped built-in-c1=ready held-run=held left-open=lost]" {
		t.Errorf("the runs of the stretch: %s", got)
	}
	for _, r := range ev.Runs {
		switch r.Change {
		case "built-in-c1":
			if !sameInstant(r.QueuedAt, s.at(17)) || !sameInstant(r.StartedAt, s.at(18)) || !sameInstant(r.FinishedAt, s.at(25)) || r.Origin != "backlog" {
				t.Errorf("built-in-c1: %+v", r)
			}
		case "left-open":
			if !sameInstant(r.FinishedAt, r.StartedAt) {
				t.Errorf("a run whose worker is gone, with no lease on record, ends where it began: %+v", r)
			}
		}
	}

	if len(ev.Pauses) != 1 || !sameInstant(ev.Pauses[0].From, s.at(120)) || !sameInstant(ev.Pauses[0].To, s.at(150)) {
		t.Errorf("the pauses: %+v", ev.Pauses)
	}
	if len(ev.Deferrals) != 1 || !sameInstant(ev.Deferrals[0].From, s.at(180)) || !ev.Deferrals[0].To.IsZero() {
		t.Errorf("the deferrals: %+v", ev.Deferrals)
	}

	// A stretch before anything was recorded is empty: no dispatcher had run by then, though the
	// first one on record is known.
	early, err := s.st.SettingEvidence(s.ctx, s.at(-6000), s.at(-5000))
	if err != nil {
		t.Fatal(err)
	}
	if len(early.Cycles)+len(early.Runs)+len(early.Dispatchers)+len(early.Pauses)+len(early.Deferrals) != 0 || early.Dispatched != 0 || !sameInstant(early.FirstDispatch, s.at(-1800)) {
		t.Errorf("a stretch before the records: %+v", early)
	}

	// The history is added up over the ninety days before the end of the stretch: a dispatcher that
	// ran before that is not part of it.
	s.exec(`UPDATE dispatcher_runs SET started_at = ?, finished_at = ? WHERE id = (SELECT MIN(id) FROM dispatcher_runs)`,
		s.at(0).Add(-120*24*time.Hour).UnixMilli(), s.at(0).Add(-119*24*time.Hour).UnixMilli())
	recent, err := s.st.SettingEvidence(s.ctx, s.at(0), s.at(480))
	if err != nil {
		t.Fatal(err)
	}
	if want := 6*time.Hour + 10*time.Minute; recent.Dispatched != want || !sameInstant(recent.FirstDispatch, s.at(0).Add(-120*24*time.Hour)) {
		t.Errorf("with the first dispatcher four months back: dispatched %s (want %s), first %s", recent.Dispatched, want, recent.FirstDispatch)
	}
}

// The evidence counts what the limits cut and whether the same work was cut again, and what became
// of the retries.
func TestSettingEvidenceCountsWhatTheLimitsAndTheRetriesDid(t *testing.T) {
	s := newEvidenceSeed(t)
	d := s.dispatcher("continuous", 2, -60, 600)

	// Agent calls. For the build of change-a: a call cut by the limit, cut again when the same
	// execution tried once more, cut again in the execution that followed, and then one that ended
	// well. For the plan of change-b: one cut, with nothing after it.
	s.execution("build-a-1", "worker", "change-a", "build", "build")
	s.attempt("build-a-1", 0, 30, 40, "timeout")
	s.attempt("build-a-1", 1, 42, 51, "timeout") // 9 minutes
	s.execution("build-a-2", "worker", "change-a", "build", "build-repair-1")
	s.attempt("build-a-2", 0, 60, 68, "timeout") // 8 minutes
	s.attempt("build-a-2", 1, 70, 75, "ok")
	s.execution("plan-b-1", "worker", "change-b", "plan", "plan")
	s.attempt("plan-b-1", 0, 80, 90, "timeout")
	// The judgment of a cycle is not held to the workers' limit.
	s.cycle("c1", 10, 20)
	s.execution("cycle-c1", "cycle", "c1", "cycle", "cycle")
	s.attempt("cycle-c1", 0, 10, 19, "timeout")
	s.attempt("cycle-c1", 1, 19.5, 20, "timeout")
	// Before the stretch: counted by a longer one only.
	s.execution("build-old", "worker", "change-old", "build", "build")
	s.attempt("build-old", 0, -300, -290, "timeout")
	s.attempt("build-old", 1, -288, -280, "timeout")

	// Idle periods: one stop, and two pauses an agent came back from.
	idleAttempt := s.attempt("build-a-2", 2, 100, 140, "ok")
	s.idle(idleAttempt, 101, 103, IdleEndedByActivity)
	s.idle(idleAttempt, 110, 116, IdleEndedByActivity)
	s.idle(s.attempt("plan-b-1", 1, 150, 175, "idle_stalled"), 155, 175, IdleEndedByStall)
	s.idle(idleAttempt, -400, -380, IdleEndedByActivity) // before the stretch

	// Gates: the unit gate of change-a was cut, cut again when repeated (45 minutes), then passed;
	// its lint gate was cut once and then failed for another reason; a gate run outside any change
	// is nobody's repetition.
	s.gate("change-a", "unit", 26, 45, 124)
	s.gate("change-a", "unit", 200, 45, 124)
	s.gate("change-a", "unit", 260, 12, 0)
	s.gate("change-a", "lint", 27, 45, 124)
	s.gate("change-a", "lint", 201, 1, 1)
	s.gate("", "unit", 28, 45, 124)
	s.gate("", "unit", 202, 45, 124)

	// Retries of blocked changes: one built, one blocked again, one that never got to build.
	s.retried(d, "retry-built", 60, 120, "ready")
	s.retried(d, "retry-blocked", 61, 121, "blocked")
	s.retried(d, "retry-held", 62, 122, "held")

	ev, err := s.st.SettingEvidence(s.ctx, s.at(0), s.at(480))
	if err != nil {
		t.Fatal(err)
	}
	if want := (LimitEvidence{Cut: 4, CutAgain: 2, Repeated: 17 * time.Minute}); ev.AgentTimeouts != want {
		t.Errorf("agent calls cut by the limit: %+v, want %+v", ev.AgentTimeouts, want)
	}
	if want := (LimitEvidence{Cut: 3, CutAgain: 1, Repeated: 45 * time.Minute}); ev.GateTimeouts != want {
		t.Errorf("gates cut by the limit: %+v, want %+v", ev.GateTimeouts, want)
	}
	if want := (IdleEvidence{Stalled: 1, Resumed: 2, LongestResumed: 6 * time.Minute}); ev.Idle != want {
		t.Errorf("what the idle watchdog saw: %+v, want %+v", ev.Idle, want)
	}
	if want := (BlockRetryEvidence{Retried: 2, Ready: 1}); ev.BlockRetries != want {
		t.Errorf("retries of blocked changes: %+v, want %+v", ev.BlockRetries, want)
	}
	// Retries with the same agent, of any kind of execution: build-a-1 (failed, 9 minutes, 2 minutes
	// after the first try), build-a-2 (ended well), cycle-c1 (failed, half a minute, half a minute
	// after), and the two later tries of build-a-2 and plan-b-1, which are numbered as retries too.
	if ev.AgentRetries.Retries != 5 || ev.AgentRetries.Succeeded != 2 {
		t.Errorf("retries with the same agent: %+v", ev.AgentRetries)
	}
	if want := 9*time.Minute + 30*time.Second + 25*time.Minute; ev.AgentRetries.Failed != want {
		t.Errorf("the retries that failed took %s, want %s", ev.AgentRetries.Failed, want)
	}
	if want := 2*time.Minute + 30*time.Second + 60*time.Minute; ev.AgentRetries.Waited != want {
		t.Errorf("the kit waited %s before the retries that failed, want %s", ev.AgentRetries.Waited, want)
	}

	// A longer stretch also counts what came before.
	longer, err := s.st.SettingEvidence(s.ctx, s.at(-600), s.at(480))
	if err != nil {
		t.Fatal(err)
	}
	if longer.AgentTimeouts.Cut != 6 || longer.AgentTimeouts.CutAgain != 3 || longer.Idle.Resumed != 3 || longer.Idle.LongestResumed != 20*time.Minute {
		t.Errorf("a longer stretch: %+v, %+v", longer.AgentTimeouts, longer.Idle)
	}
}
