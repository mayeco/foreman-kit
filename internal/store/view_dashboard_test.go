package store

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestViewSaysWhatTheKitIsDoingNow: Dashboard reads what is open (the worker runs with the execution each
// is on, the merge attempt, the cycle, what a person started, the idle agents and the pause) and the
// latest outcomes, each from the rows of its own table.
func TestViewSaysWhatTheKitIsDoingNow(t *testing.T) {
	w := seedPortalWorld(t)
	st, ctx := w.st, context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// The cycle of the seeded world ends, and another is left open by a process that died.
	must(st.BeginCycle(ctx, Cycle{Stamp: "20261105T090000Z", Trigger: CycleTriggerInterval, PID: os.Getpid()}))
	must(st.FinishCycle(ctx, "20261105T090000Z", "done", ""))
	must(st.BeginCycle(ctx, Cycle{Stamp: "20261106T090000Z", Trigger: CycleTriggerStartup, PID: 2147483646}))
	// A run that was held closed nothing: the dispatcher takes the change again by itself.
	held, err := st.BeginChangeRun(ctx, "add-ui", 3, "/wt/worker-add-ui", "feat/spec-add-ui")
	must(err)
	must(st.FinishChangeRun(ctx, held, "held"))

	kinds := func(outcomes []Outcome) string {
		var out []string
		for _, o := range outcomes {
			switch o.Kind {
			case OutcomeRun:
				out = append(out, "run:"+o.Run.ChangeName+":"+o.Run.Result)
			case OutcomeMerge:
				out = append(out, "merge:"+o.Merge.ChangeName+":"+o.Merge.Result)
			case OutcomeBlock:
				out = append(out, "block:"+o.Block.ChangeName+":"+o.Block.Reason)
			case OutcomeCycle:
				out = append(out, "cycle:"+o.Cycle.Stamp+":"+o.Cycle.Outcome)
			}
			if o.At == "" {
				t.Errorf("the outcome %s has no instant", out[len(out)-1])
			}
		}
		return strings.Join(out, " ")
	}

	view(t, st, func(v *View) error {
		now, err := v.Dashboard(ctx, 10)
		if err != nil {
			return err
		}
		if now.Counts[ChangeBuilding] != 2 || now.Counts[ChangeMerging] != 1 || now.Counts[ChangeBlocked] != 1 ||
			now.Counts[ChangeRebuildRequested] != 1 || now.Counts[ChangeIntegrated] != 1 || len(now.Counts) != len(ChangeStates) {
			t.Errorf("counts = %v", now.Counts)
		}

		// The open runs, the first one first: the one being built is on its BUILD, and the worker of
		// the other is gone.
		if len(now.Runs) != 2 || now.Runs[0].ChangeName != "add-search" || now.Runs[1].ChangeName != "add-export" {
			t.Fatalf("open runs = %+v", now.Runs)
		}
		search, export := now.Runs[0], now.Runs[1]
		if search.Lost || search.Execution == nil || search.Execution.ExecID != "search-apply" || search.Execution.OwnerGone ||
			len(search.Execution.Attempts) != 1 || len(search.Execution.Attempts[0].IdlePeriods) != 1 {
			t.Errorf("the run of add-search = %+v, on %+v", search, search.Execution)
		}
		if !export.Lost || export.Execution != nil {
			t.Errorf("the run of add-export = %+v", export)
		}

		if now.Merge == nil || now.Merge.ID != w.secondMerge || now.Merge.ChangeName != "add-api" {
			t.Errorf("the merge attempt in progress = %+v", now.Merge)
		}
		// What a person started and still runs stands on its own: it belongs to no worker run.
		if len(now.Executions) != 1 || now.Executions[0].ExecID != "billing-prompt" {
			t.Errorf("what runs outside a worker run = %+v", now.Executions)
		}
		if len(now.Idle) != 1 || now.Idle[0].ExecID != "search-apply" || now.Idle[0].LimitSeconds != 1200 {
			t.Errorf("idle agents = %+v", now.Idle)
		}
		if now.Pause == nil || now.Pause.Reason != "maintenance window" {
			t.Errorf("pause = %+v", now.Pause)
		}
		// No cycle runs: the one left open belongs to a process that is gone.
		if now.Cycle != nil || now.LastCycle == nil || now.LastCycle.Stamp != "20261105T090000Z" || now.LastCycle.Trigger != CycleTriggerInterval {
			t.Errorf("cycle = %+v, last cycle = %+v", now.Cycle, now.LastCycle)
		}

		// The latest outcomes, the latest first. The held run is not one, and the run that ended
		// blocked is told by its block.
		want := "cycle:20261105T090000Z:done block:add-billing:gate_failure merge:add-api:push_rejected run:add-api:ready"
		if got := kinds(now.Recent); got != want {
			t.Errorf("recent outcomes =\n  %s\nwant\n  %s", got, want)
		}
		latest, err := v.Dashboard(ctx, 2)
		if err != nil {
			return err
		}
		if got := kinds(latest.Recent); got != "cycle:20261105T090000Z:done block:add-billing:gate_failure" {
			t.Errorf("the two latest outcomes = %s", got)
		}
		return nil
	})

	// A cycle this process runs is the one in progress.
	must(st.BeginCycle(ctx, Cycle{Stamp: "20261107T090000Z", Trigger: CycleTriggerOutOfWork, PID: os.Getpid()}))
	view(t, st, func(v *View) error {
		now, err := v.Dashboard(ctx, 0)
		if err != nil {
			return err
		}
		if now.Cycle == nil || now.Cycle.Stamp != "20261107T090000Z" || now.Cycle.Trigger != CycleTriggerOutOfWork || now.Cycle.FinishedAt != "" {
			t.Errorf("the cycle in progress = %+v", now.Cycle)
		}
		return nil
	})
}

// TestDashboardOfAnEmptyWorkspace: with nothing on record, Dashboard has every list, empty, and a count of zero
// for every state.
func TestDashboardOfAnEmptyWorkspace(t *testing.T) {
	st := openTestStore(t)
	view(t, st, func(v *View) error {
		now, err := v.Dashboard(context.Background(), 0)
		if err != nil {
			return err
		}
		if now.Runs == nil || now.Executions == nil || now.Idle == nil || now.Recent == nil ||
			len(now.Runs)+len(now.Executions)+len(now.Idle)+len(now.Recent) != 0 || now.Merge != nil || now.Cycle != nil ||
			now.LastCycle != nil || now.Pause != nil || len(now.Counts) != len(ChangeStates) {
			t.Errorf("Dashboard of an empty workspace = %+v", now)
		}
		return nil
	})
}
