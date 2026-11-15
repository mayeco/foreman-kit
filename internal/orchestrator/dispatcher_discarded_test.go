package orchestrator

import (
	"context"
	"strings"
	"testing"
)

// A backlog item whose name the registry holds as discarded is not taken, as an integrated one is
// not: queued again under the same name it used to be built while every reader of the registry said
// discarded, so the cap on the queue did not count it and the portal refused to abandon it.
func TestADiscardedNameInTheBacklogIsNotDispatched(t *testing.T) {
	ctx := context.Background()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	const gone, other, dependent = "discarded-item", "other-item", "needs-discarded"
	if err := st.MarkChangeDiscarded(ctx, gone); err != nil {
		t.Fatal(err)
	}
	writeBacklogJSON(t, umbrellaDir,
		map[string]any{"change_name": gone, "description": "Queued again after it was abandoned", "priority": "high"},
		map[string]any{"change_name": dependent, "description": "Depends on the abandoned name", "priority": "high", "depends_on": []string{gone}},
		map[string]any{"change_name": other, "description": "An ordinary item", "priority": "normal"},
	)

	// The dry run lists the item and says why it will not start.
	dry, err := NewDispatcher(DispatcherOptions{Root: umbrellaDir, Store: st, DryRun: true, Out: &syncBuffer{}})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := dry.DryRun(ctx)
	_ = dry.Close()
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]DryRunItem{}
	for _, it := range rep.Items {
		rows[it.Change] = it
	}
	if r, ok := rows[gone]; !ok || r.State != "discarded" || r.Next ||
		!strings.Contains(r.Why, "abandoned earlier") || !strings.Contains(r.Why, "new name") {
		t.Fatalf("the dry run does not explain the discarded name: %+v\n%s", rows[gone], rep.FormatText(1, 0))
	}
	if r := rows[dependent]; r.State != "waiting" || !strings.Contains(r.Why, gone+" (discarded)") {
		t.Fatalf("the dependent does not name its discarded dependency: %+v", r)
	}
	if r := rows[other]; r.State != "ready" || !r.Next {
		t.Fatalf("the ordinary item is not next: %+v", r)
	}

	// A dispatch builds the ordinary item and opens nothing for the discarded name or its dependent.
	var log syncBuffer
	disp, err := NewDispatcher(DispatcherOptions{Root: umbrellaDir, Store: st, MaxWorkers: 1, AgentRunner: continuousStubAgent(), Out: &log})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp.Run(ctx); err != nil {
		t.Fatal(err)
	}
	_ = disp.Close()

	if !inReadyQueue(ctx, st, other) {
		t.Fatalf("the ordinary item was not built; log:\n%s", log.String())
	}
	for _, name := range []string{gone, dependent} {
		if runs, err := st.ListChangeRuns(ctx, name, 10); err != nil || len(runs) != 0 {
			t.Fatalf("%s has %d change runs (err %v), want none", name, len(runs), err)
		}
		if inReadyQueue(ctx, st, name) {
			t.Fatalf("%s has a manifest", name)
		}
		if q, err := st.GetQuarantine(ctx, name); err != nil || q != nil {
			t.Fatalf("%s has a block (err %v): %+v", name, err, q)
		}
	}
	if got, err := st.ChangeState(ctx, gone); err != nil || got != "discarded" {
		t.Fatalf("%s is %q (err %v), want discarded", gone, got, err)
	}
	if n := strings.Count(log.String(), "queue the work under a new name"); n != 1 {
		t.Fatalf("the log says why %d times, want once; log:\n%s", n, log.String())
	}
}
