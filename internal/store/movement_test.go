package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openMovementStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenAt(filepath.Join(dir, "state.db"), "ws-movement", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// endRun records a worker's pass over change that ends with result.
func endRun(t *testing.T, st *Store, change, result string) {
	t.Helper()
	ctx := context.Background()
	id, err := st.BeginChangeRun(ctx, change, 1, "/wt/"+change, "feat/spec-"+change)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishChangeRun(ctx, id, result); err != nil {
		t.Fatal(err)
	}
}

// endMerge queues change for the merger and records a merge attempt that ends with result.
func endMerge(t *testing.T, st *Store, change, result string) {
	t.Helper()
	ctx := context.Background()
	if err := st.EnqueueReady(ctx, ReadyManifest{ChangeName: change, Branch: "feat/spec-" + change, Worktree: "/wt/" + change, HeadSHA: "abc123"}); err != nil {
		t.Fatal(err)
	}
	id, err := st.BeginMergeAttempt(ctx, change)
	if err != nil {
		t.Fatal(err)
	}
	if result == MergeIntegrated {
		if err := st.MarkMergeAttemptIntegrated(ctx, id, "commit-"+change, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FinishMergeAttempt(ctx, id, result, ""); err != nil {
		t.Fatal(err)
	}
}

// Movement is a change built, blocked or integrated. A run that ended without taking its change
// anywhere (held behind another change, deferred by an engine outage, stopped with the dispatcher) is
// not, and neither is a merge attempt that integrated nothing.
func TestMovementCountsWhatMovedAndNotWhatWaited(t *testing.T) {
	st := openMovementStore(t)
	ctx := context.Background()

	before := time.Now()
	time.Sleep(3 * time.Millisecond)

	endRun(t, st, "built-change", "ready")
	for _, waited := range []string{"held", "deferred", "paused", "stopped", "lost"} {
		endRun(t, st, "run-"+waited, waited)
	}
	if err := st.BlockChange(ctx, QuarantineRecord{ChangeName: "blocked-change", Reason: "gate_failure"}); err != nil {
		t.Fatal(err)
	}
	endMerge(t, st, "merged-change", MergeIntegrated)
	for _, notMerged := range []string{MergeStopped, MergePushRejected, MergeHeld, MergeSentBack} {
		endMerge(t, st, "merge-"+notMerged, notMerged)
	}

	got, err := st.MovementBetween(ctx, before, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Movement{Built: 1, Blocked: 1, Integrated: 1}); got != want {
		t.Fatalf("movement since the start: got %+v, want %+v", got, want)
	}
	if !got.Any() {
		t.Fatal("a change built, one blocked and one integrated is movement")
	}

	// The stretch ends where it is told to: nothing had moved before the first run.
	earlier, err := st.MovementBetween(ctx, before.Add(-time.Hour), before)
	if err != nil {
		t.Fatal(err)
	}
	if earlier.Any() {
		t.Fatalf("nothing moved before the first run, got %+v", earlier)
	}
}

// Each cycle owns what moved from its start to the start of the next one, and the latest cycle what
// moved since it began: that is what tells a cycle that queued work from one that found nothing.
func TestRecentCycleMovementSplitsAtEachCycleStart(t *testing.T) {
	st := openMovementStore(t)
	ctx := context.Background()

	if recent, err := st.RecentCycleMovement(ctx, 2); err != nil || len(recent) != 0 {
		t.Fatalf("no cycle on record: got %+v (%v)", recent, err)
	}

	const first, second = "20261110T051820Z", "20261110T061820Z"
	if err := st.BeginCycle(ctx, Cycle{Stamp: first}); err != nil {
		t.Fatal(err)
	}
	endRun(t, st, "built-in-the-first", "ready")
	endMerge(t, st, "built-in-the-first", MergeIntegrated)
	time.Sleep(3 * time.Millisecond)

	if err := st.BeginCycle(ctx, Cycle{Stamp: second}); err != nil {
		t.Fatal(err)
	}
	endRun(t, st, "held-in-the-second", "held")

	recent, err := st.RecentCycleMovement(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 || recent[0].Stamp != second || recent[1].Stamp != first {
		t.Fatalf("expected the two cycles, newest first, got %+v", recent)
	}
	if !recent[0].StartedAt.After(recent[1].StartedAt) {
		t.Fatalf("the second cycle began after the first: %s, %s", recent[0].StartedAt, recent[1].StartedAt)
	}
	if recent[0].Any() {
		t.Fatalf("a held run is not movement of the second cycle, got %+v", recent[0].Movement)
	}
	if want := (Movement{Built: 1, Integrated: 1}); recent[1].Movement != want {
		t.Fatalf("movement of the first cycle: got %+v, want %+v", recent[1].Movement, want)
	}

	// What moves from now on belongs to the latest cycle, and leaves the earlier one as it was.
	if err := st.BlockChange(ctx, QuarantineRecord{ChangeName: "held-in-the-second", Reason: "gate_failure"}); err != nil {
		t.Fatal(err)
	}
	recent, err = st.RecentCycleMovement(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Fatalf("asking for more cycles than there are returns those there are, got %d", len(recent))
	}
	if want := (Movement{Blocked: 1}); recent[0].Movement != want {
		t.Fatalf("movement of the latest cycle: got %+v, want %+v", recent[0].Movement, want)
	}
	if want := (Movement{Built: 1, Integrated: 1}); recent[1].Movement != want {
		t.Fatalf("movement of the first cycle changed: got %+v, want %+v", recent[1].Movement, want)
	}
}
