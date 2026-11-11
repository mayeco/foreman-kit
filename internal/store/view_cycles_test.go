package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestViewReadsTheCyclesAndWhatEachProduced: the list of cycles is paged by cursor from the table of
// cycles, and one cycle comes with its executions, the changes it queued, the proposals and the
// browser checks recorded under it, and the cycles next to it.
func TestViewReadsTheCyclesAndWhatEachProduced(t *testing.T) {
	w := seedPortalWorld(t)
	st, ctx := w.st, context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Three cycles: one from the day before, the one of the seeded world, which ends, and one that
	// runs now.
	const before, seeded, running = "20261104T090000Z", "20261105T090000Z", "20261106T090000Z"
	must(st.BeginCycle(ctx, Cycle{Stamp: before, Trigger: CycleTriggerStartup, Strategy: "specs-versus-code", StartedAt: "2026-11-04T09:00:00Z"}))
	must(st.UpdateCycle(ctx, Cycle{Stamp: before, FindingsCount: 2}))
	must(st.FinishCycle(ctx, before, "done", "2026-11-04T09:10:00Z"))
	must(st.BeginCycle(ctx, Cycle{Stamp: seeded, Trigger: CycleTriggerInterval, JudgeEngine: "claude"}))
	must(st.FinishCycle(ctx, seeded, "done", ""))
	must(st.BeginCycle(ctx, Cycle{Stamp: running, Trigger: CycleTriggerOutOfWork}))
	// What the seeded cycle produced: a check in a browser, recorded under the cycle that asked for
	// it, a proposal and two changes it queued. A cycle says which proposals it made and which
	// changes it queued (RecordCycleWork); the rows say it here.
	_, err := st.EnqueueVisualQANext(WithCycle(ctx, seeded), VisualQAItem{What: "the search page shows results", ChangeName: "add-search"})
	must(err)
	if _, err := st.DB().ExecContext(ctx, `UPDATE proposals SET cycle_stamp = ? WHERE proposal_id = 'add-cache'`, seeded); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE changes SET cycle_stamp = ? WHERE change_name IN ('add-export', 'add-search')`, seeded); err != nil {
		t.Fatal(err)
	}

	view(t, st, func(v *View) error {
		first, err := v.ListCyclesPage(ctx, "", 2)
		if err != nil {
			return err
		}
		if len(first.Items) != 2 || first.Items[0].Stamp != running || first.Items[1].Stamp != seeded || first.Next == "" {
			t.Fatalf("the first page of cycles = %+v", first)
		}
		if c := first.Items[0]; c.Outcome != "running" || c.Trigger != CycleTriggerOutOfWork || c.FinishedAt != "" {
			t.Errorf("the cycle that runs = %+v", c)
		}
		second, err := v.ListCyclesPage(ctx, first.Next, 2)
		if err != nil {
			return err
		}
		if len(second.Items) != 1 || second.Items[0].Stamp != before || second.Next != "" {
			t.Fatalf("the second page of cycles = %+v", second)
		}
		if c := second.Items[0]; c.Outcome != "done" || c.Strategy != "specs-versus-code" || c.FindingsCount != 2 || c.Trigger != CycleTriggerStartup {
			t.Errorf("the first cycle = %+v", c)
		}
		if _, err := v.ListCyclesPage(ctx, "not-a-cursor", 2); !errors.Is(err, ErrBadCursor) {
			t.Errorf("a cursor no page handed out = %v, want ErrBadCursor", err)
		}

		rec, err := v.Cycle(ctx, seeded)
		if err != nil || rec == nil {
			t.Fatalf("Cycle(%s) = %v, %v", seeded, rec, err)
		}
		if rec.Outcome != "done" || rec.JudgeEngine != "claude" || rec.Newer != running || rec.Older != before {
			t.Errorf("the seeded cycle = %+v", rec.Cycle)
		}
		if len(rec.Executions) != 1 || rec.Executions[0].ExecID != "cycle-"+seeded || rec.Executions[0].Kind != "cycle" {
			t.Errorf("the executions of the cycle = %+v", rec.Executions)
		}
		if len(rec.VisualQA) != 1 || rec.VisualQA[0].What != "the search page shows results" || rec.VisualQA[0].ChangeName != "add-search" {
			t.Errorf("the browser checks of the cycle = %+v", rec.VisualQA)
		}
		if len(rec.Proposals) != 1 || rec.Proposals[0].ID != "add-cache" {
			t.Errorf("the proposals of the cycle = %+v", rec.Proposals)
		}
		// The changes it queued, the first one queued first, each with the state it is in now: the
		// one the list of changes gives it.
		if len(rec.Queued) != 2 || rec.Queued[0].ChangeName != "add-search" || rec.Queued[1].ChangeName != "add-export" {
			t.Fatalf("the changes the cycle queued = %+v", rec.Queued)
		}
		for _, q := range rec.Queued {
			listed, err := v.ListChangeOverviews(ctx, ChangeFilter{Query: q.ChangeName})
			if err != nil || len(listed.Items) != 1 || listed.Items[0] != q {
				t.Errorf("the cycle gives %+v for a change the list gives as %+v (%v)", q, listed.Items, err)
			}
		}

		oldest, err := v.Cycle(ctx, before)
		if err != nil || oldest == nil || oldest.Older != "" || oldest.Newer != seeded {
			t.Errorf("the first cycle has neighbours %+v (err %v)", oldest, err)
		}
		if len(oldest.Executions) != 0 || len(oldest.Queued) != 0 || len(oldest.Proposals) != 0 || len(oldest.VisualQA) != 0 {
			t.Errorf("a cycle with nothing under it = %+v", oldest)
		}
		latest, err := v.Cycle(ctx, running)
		if err != nil || latest == nil || latest.Newer != "" || latest.Older != seeded {
			t.Errorf("the cycle that runs has neighbours %+v (err %v)", latest, err)
		}
		if none, err := v.Cycle(ctx, "20200101T000000Z"); err != nil || none != nil {
			t.Errorf("a cycle that is not on record = %+v, %v", none, err)
		}
		return nil
	})

	// The cycle read through a View and the read of the registry name the same changes, in the same
	// order.
	registry, err := st.ChangesQueuedByCycle(ctx, seeded)
	if err != nil || strings.Join(registry, ",") != "add-search,add-export" {
		t.Errorf("ChangesQueuedByCycle(%s) = %v, %v", seeded, registry, err)
	}
}

// TestViewListsWhatCyclesQueuedAndTheBacklogDoesNotList: a change a cycle queued is on record before
// the main checkout lists it, in no state of the pipeline. The read returns those changes with their
// cycle, the ones of the latest cycle first, and stops returning one when the main checkout lists
// it, for good: a person who takes it out of the backlog afterwards does not bring it back. A change
// that is in no state of the pipeline and that no cycle queued is not one of them.
func TestViewListsWhatCyclesQueuedAndTheBacklogDoesNotList(t *testing.T) {
	st := openMovementStore(t)
	ctx := context.Background()
	main := t.TempDir()
	const first, second = "20261110T212317Z", "20261110T214110Z"
	for _, stamp := range []string{first, second} {
		if err := st.BeginCycle(ctx, Cycle{Stamp: stamp}); err != nil {
			t.Fatal(err)
		}
	}
	mirror := func(backlog string) {
		t.Helper()
		writeMirrored(t, main, "agent_backlog.json", backlog)
		if err := st.SyncJSONToSQLite(ctx, main); err != nil {
			t.Fatal(err)
		}
	}
	unlisted := func() string {
		t.Helper()
		var got []string
		view(t, st, func(v *View) error {
			list, err := v.ListUnlistedQueued(ctx, 0)
			for _, u := range list {
				got = append(got, u.ChangeName+"@"+u.CycleStamp)
			}
			return err
		})
		return strings.Join(got, " ")
	}

	// A person queued a change and took it out again: it is in no state of the pipeline, and no
	// cycle queued it.
	mirror(`[{"change_name": "queued-by-hand"}]`)
	mirror(`[]`)
	if status := backlogStatusOf(t, st, "queued-by-hand"); status != "unknown" {
		t.Fatalf("a change taken out of the backlog has the status %s, and the test needs it unknown", status)
	}
	if got := unlisted(); got != "" {
		t.Errorf("with no cycle that queued anything the read returns %q", got)
	}

	// Two cycles queue a change each, in their own worktrees.
	for stamp, change := range map[string]string{first: "found-by-the-first", second: "found-by-the-second"} {
		if err := st.RecordCycleWork(ctx, stamp, CycleWork{Queued: []map[string]any{{"change_name": change}}}); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := unlisted(), "found-by-the-second@"+second+" found-by-the-first@"+first; got != want {
		t.Errorf("what cycles queued and the backlog does not list = %q, want %q", got, want)
	}
	view(t, st, func(v *View) error {
		one, err := v.ListUnlistedQueued(ctx, 1)
		if err != nil || len(one) != 1 || one[0].ChangeName != "found-by-the-second" {
			t.Errorf("the first of them = %+v, %v", one, err)
		}
		return nil
	})

	// The main checkout lists the one of the first cycle: it is queued, and the read lets it go.
	mirror(`[{"change_name": "found-by-the-first"}]`)
	if got, want := unlisted(), "found-by-the-second@"+second; got != want {
		t.Errorf("after the main checkout listed one of them the read returns %q, want %q", got, want)
	}

	// A person takes it out of the backlog. It is in no state of the pipeline again, under the cycle
	// that queued it, and it waits for no checkout to list it: the backlog listed it once.
	mirror(`[]`)
	if status := backlogStatusOf(t, st, "found-by-the-first"); status != "unknown" {
		t.Fatalf("a change taken out of the backlog has the status %s, and the test needs it unknown", status)
	}
	if by, err := st.ChangesQueuedByCycle(ctx, first); err != nil || strings.Join(by, ",") != "found-by-the-first" {
		t.Fatalf("the first cycle queued %v, %v, and the test needs the change it took out still under it", by, err)
	}
	if got, want := unlisted(), "found-by-the-second@"+second; got != want {
		t.Errorf("after a person took the listed one out of the backlog the read returns %q, want %q", got, want)
	}
}

// TestAChangeOfACycleThatWasCutIsNotWaitedForForever: a cycle recorded what it queued and was cut
// before it published it, so the main checkout will never list the change. Once a later cycle has
// ended, which brings the main checkout up to date, the read stops returning it: Dashboard counted it
// as waiting to be listed for good. A later cycle that is still running proves nothing yet.
func TestAChangeOfACycleThatWasCutIsNotWaitedForForever(t *testing.T) {
	st := openMovementStore(t)
	ctx := context.Background()
	const cut, next = "20261111T021342Z", "20261111T030000Z"
	if err := st.BeginCycle(ctx, Cycle{Stamp: cut, StartedAt: "2026-11-11T02:13:42Z"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCycleWork(ctx, cut, CycleWork{Queued: []map[string]any{{"change_name": "fix-home-tagline-position"}}}); err != nil {
		t.Fatal(err)
	}
	unlisted := func() string {
		t.Helper()
		var got []string
		view(t, st, func(v *View) error {
			list, err := v.ListUnlistedQueued(ctx, 0)
			for _, u := range list {
				got = append(got, u.ChangeName)
			}
			return err
		})
		return strings.Join(got, " ")
	}
	if got := unlisted(); got != "fix-home-tagline-position" {
		t.Fatalf("right after the cycle the read returns %q", got)
	}
	// The process died: the kit finds the cycle stopped. Its record may still be published by the
	// next one (a record commit left behind), so the change still waits.
	if err := st.FinishCycle(ctx, cut, "stopped", "2026-11-11T02:36:36Z"); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginCycle(ctx, Cycle{Stamp: next, StartedAt: "2026-11-11T03:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if got := unlisted(); got != "fix-home-tagline-position" {
		t.Errorf("while the next cycle runs the read returns %q", got)
	}
	if err := st.FinishCycle(ctx, next, "done", "2026-11-11T03:20:00Z"); err != nil {
		t.Fatal(err)
	}
	if got := unlisted(); got != "" {
		t.Errorf("after a later cycle ended the read still returns %q", got)
	}
}

// TestViewReadsOneEvent: an event is read by its id, with the rows it is about.
func TestViewReadsOneEvent(t *testing.T) {
	w := seedPortalWorld(t)
	st, ctx := w.st, context.Background()
	events, err := st.QueryEvents(ctx, EventFilter{ChangeName: "add-api", Event: "change.start"})
	if err != nil || len(events) == 0 {
		t.Fatalf("the seeded world has no change.start of add-api: %v", err)
	}
	want := events[0]
	view(t, st, func(v *View) error {
		got, err := v.Event(ctx, want.ID)
		if err != nil || got == nil {
			t.Fatalf("Event(%d) = %v, %v", want.ID, got, err)
		}
		if got.ID != want.ID || got.Event != "change.start" || got.Change != "add-api" || got.ChangeRunID != w.apiRun || got.DispatcherRunID != w.dispatch || got.TS == "" {
			t.Errorf("the event = %+v", got)
		}
		for _, id := range []int64{0, -1, 1 << 40} {
			if none, err := v.Event(ctx, id); err != nil || none != nil {
				t.Errorf("Event(%d) = %+v, %v, want none", id, none, err)
			}
		}
		return nil
	})
}
