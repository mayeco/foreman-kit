package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestIdlePeriodsAreOpenedAndClosedNeverOverwritten: an agent that goes idle is a row that opens and
// closes, with its events written in the same transactions. The open rows of attempts still running
// are the agents idle now; the closed ones keep how long each period lasted and what ended it.
func TestIdlePeriodsAreOpenedAndClosedNeverOverwritten(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const change, limit = "add-search", 20 * time.Minute

	runID, err := st.BeginChangeRun(ctx, change, 1, "/wt/add-search", "feat/spec-add-search")
	if err != nil {
		t.Fatal(err)
	}
	runCtx := WithChangeRun(ctx, runID, change)
	newExec := func(execID string) int64 {
		t.Helper()
		if err := st.CreateExecution(runCtx, Execution{ExecID: execID, Kind: "worker", ChangeName: change, Phase: "apply", PID: os.Getpid()}); err != nil {
			t.Fatal(err)
		}
		attempt, err := st.BeginAttempt(runCtx, execID, "claude", 0)
		if err != nil || attempt == 0 {
			t.Fatalf("BeginAttempt(%s) = %d, %v", execID, attempt, err)
		}
		// The attempt has been running for half an hour.
		if _, err := st.DB().ExecContext(ctx, `UPDATE execution_attempts SET started_at = started_at - 1800000 WHERE id = ?`, attempt); err != nil {
			t.Fatal(err)
		}
		return attempt
	}
	events := func(execID string) string {
		t.Helper()
		recs, err := st.QueryEvents(ctx, EventFilter{ExecID: execID})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, r := range recs {
			names = append(names, r.Event)
		}
		return strings.Join(names, ",")
	}

	// 1. Idle for six minutes, then working again.
	first := newExec("exec-1")
	since := time.Now().Add(-6 * time.Minute)
	if err := st.BeginIdlePeriod(runCtx, first, since, limit); err != nil {
		t.Fatal(err)
	}
	// Told again at the next poll: it is the same period.
	if err := st.BeginIdlePeriod(runCtx, first, since.Add(time.Minute), limit); err != nil {
		t.Fatal(err)
	}
	idle, err := st.ListIdleAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(idle) != 1 || idle[0].ExecID != "exec-1" || idle[0].ChangeName != change || idle[0].Engine != "claude" || idle[0].Phase != "apply" ||
		idle[0].Kind != "worker" || idle[0].PID != os.Getpid() || idle[0].LimitSeconds != 1200 || idle[0].IdleSeconds < 359 || idle[0].IdleSeconds > 370 {
		t.Fatalf("idle agents = %+v, want claude on exec-1, idle for six minutes of twenty", idle)
	}
	if got := events("exec-1"); got != "agent.idle" {
		t.Fatalf("events = %s, want one agent.idle", got)
	}
	recs, _ := st.QueryEvents(ctx, EventFilter{Event: "agent.idle", ExecID: "exec-1"})
	if len(recs) != 1 || recs[0].Change != change || recs[0].Engine != "claude" || recs[0].Phase != "apply" || recs[0].ChangeRunID != runID ||
		recs[0].Payload["limit_seconds"] != float64(1200) || recs[0].Payload["idle_seconds"] != float64(360) {
		t.Fatalf("agent.idle = %+v", recs)
	}

	if err := st.EndIdlePeriod(runCtx, first); err != nil {
		t.Fatal(err)
	}
	if err := st.EndIdlePeriod(runCtx, first); err != nil { // nothing open: nothing to do
		t.Fatal(err)
	}
	if idle, _ := st.ListIdleAgents(ctx); len(idle) != 0 {
		t.Fatalf("an agent that works again is still idle: %+v", idle)
	}
	if got := events("exec-1"); got != "agent.idle,agent.resumed" {
		t.Fatalf("events = %s, want agent.idle,agent.resumed", got)
	}

	// 2. Idle again, and this time stopped by the watchdog at its limit.
	if err := st.BeginIdlePeriod(runCtx, first, time.Now().Add(-5*time.Minute), limit); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishAttempt(runCtx, first, 124, AttemptIdleStalled, 1200); err != nil {
		t.Fatal(err)
	}
	periods, err := st.ListIdlePeriods(ctx, "exec-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != 2 || periods[0].EndedBy != IdleEndedByActivity || periods[0].Seconds < 359 || periods[0].Seconds > 370 ||
		periods[1].EndedBy != IdleEndedByStall || periods[1].EndedAt == "" || periods[1].Engine != "claude" || periods[1].LimitSeconds != 1200 {
		t.Fatalf("idle periods = %+v, want one ended by activity and one by the stall", periods)
	}
	if got := events("exec-1"); got != "agent.idle,agent.resumed,agent.idle,agent.stalled" {
		t.Fatalf("events = %s", got)
	}
	stalled, _ := st.QueryEvents(ctx, EventFilter{Event: "agent.stalled", ExecID: "exec-1"})
	if len(stalled) != 1 || stalled[0].Payload["idle_seconds"] != float64(1200) || stalled[0].Engine != "claude" || stalled[0].ChangeRunID != runID {
		t.Fatalf("agent.stalled = %+v", stalled)
	}
	// An attempt that has ended does not go idle.
	if err := st.BeginIdlePeriod(runCtx, first, time.Now(), limit); err != nil {
		t.Fatal(err)
	}
	if periods, _ := st.ListIdlePeriods(ctx, "exec-1"); len(periods) != 2 {
		t.Fatalf("an idle period was opened for a finished attempt: %+v", periods)
	}

	// 3. An attempt that finishes by itself while idle: the period ends with it, and nothing stalled.
	second := newExec("exec-2")
	_ = st.BeginIdlePeriod(runCtx, second, time.Now().Add(-7*time.Minute), limit)
	if err := st.FinishAttempt(runCtx, second, 0, AttemptOK, 0); err != nil {
		t.Fatal(err)
	}
	if periods, _ := st.ListIdlePeriods(ctx, "exec-2"); len(periods) != 1 || periods[0].EndedBy != IdleEndedByExit {
		t.Fatalf("idle periods of exec-2 = %+v, want one ended by the exit", periods)
	}
	if got := events("exec-2"); got != "agent.idle" {
		t.Fatalf("events of exec-2 = %s, want only agent.idle", got)
	}

	// 4. An execution that is ended with an attempt still open (its orchestrator died, a later one
	// reaps it): the idle period does not stay open.
	third := newExec("exec-3")
	_ = st.BeginIdlePeriod(runCtx, third, time.Now().Add(-8*time.Minute), limit)
	if idle, _ := st.ListIdleAgents(ctx); len(idle) != 1 || idle[0].ExecID != "exec-3" {
		t.Fatalf("idle agents = %+v, want exec-3 only", idle)
	}
	if err := st.FinishExecution(ctx, "exec-3", "stopped", -1); err != nil {
		t.Fatal(err)
	}
	if idle, _ := st.ListIdleAgents(ctx); len(idle) != 0 {
		t.Fatalf("a stopped execution is still shown as idle: %+v", idle)
	}
	if periods, _ := st.ListIdlePeriods(ctx, "exec-3"); len(periods) != 1 || periods[0].EndedBy != IdleEndedByExit {
		t.Fatalf("idle periods of exec-3 = %+v, want one ended by the exit", periods)
	}

	// A period never starts before its attempt did, whatever the caller's clock says.
	fourth := newExec("exec-4")
	_ = st.BeginIdlePeriod(runCtx, fourth, time.Now().Add(-48*time.Hour), limit)
	if idle, _ := st.ListIdleAgents(ctx); len(idle) != 1 || idle[0].IdleSeconds > 1810 {
		t.Fatalf("idle agents = %+v, want exec-4 idle since its attempt started at the most", idle)
	}

	// The database refuses a second open period for one attempt, and a period without an attempt.
	for name, stmt := range map[string]string{
		"second open period of an attempt": `INSERT INTO attempt_idle_periods (attempt_id, started_at, limit_seconds) SELECT attempt_id, 1, 60 FROM attempt_idle_periods WHERE ended_at IS NULL LIMIT 1`,
		"period of an unknown attempt":     `INSERT INTO attempt_idle_periods (attempt_id, started_at, limit_seconds) VALUES (987654, 1, 60)`,
		"period closed without a reason":   `INSERT INTO attempt_idle_periods (attempt_id, started_at, limit_seconds, ended_at) SELECT id, 1, 60, 2 FROM execution_attempts LIMIT 1`,
	} {
		if _, err := st.DB().ExecContext(ctx, stmt); err == nil {
			t.Errorf("%s: the insert was accepted", name)
		}
	}
}
