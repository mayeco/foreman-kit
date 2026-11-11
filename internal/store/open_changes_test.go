package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCountOpenChangesCountsWhatThePipelineHasNotFinished: a change counts from the moment the
// backlog lists it until it is integrated or discarded, whatever happens to it in between.
func TestCountOpenChangesCountsWhatThePipelineHasNotFinished(t *testing.T) {
	ctx := context.Background()
	st := openMovementStore(t)
	count := func(queued ...string) int {
		t.Helper()
		n, err := st.CountOpenChanges(ctx, queued)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	if n := count(); n != 0 {
		t.Fatalf("an empty registry and an empty backlog have %d open changes", n)
	}
	// The backlog counts before the registry has met its items, and a name it lists twice, or with
	// spaces around it, is one change.
	backlog := []string{"waits", "builds", "is-ready", "merges", "is-blocked", "rebuilds", "lands", "goes"}
	if n := count(append(append([]string{}, backlog...), " waits ", "", "builds")...); n != 8 {
		t.Fatalf("got %d open changes for a backlog of eight, want 8", n)
	}

	if _, err := st.BeginChangeRun(ctx, "builds", 1, "/wt/builds", "feat/spec-builds"); err != nil {
		t.Fatal(err)
	}
	endRun(t, st, "is-ready", "ready")
	if err := st.EnqueueReady(ctx, ReadyManifest{ChangeName: "is-ready", Branch: "feat/spec-is-ready", Worktree: "/wt/is-ready", HeadSHA: "abc123"}); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueReady(ctx, ReadyManifest{ChangeName: "merges", Branch: "feat/spec-merges", Worktree: "/wt/merges", HeadSHA: "abc123"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginMergeAttempt(ctx, "merges"); err != nil {
		t.Fatal(err)
	}
	if err := st.BlockChange(ctx, QuarantineRecord{ChangeName: "is-blocked", Reason: "gate_failure"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RequestRebuild(ctx, "rebuilds", "feat/spec-rebuilds", "conflict"); err != nil {
		t.Fatal(err)
	}
	for change, want := range map[string]string{
		"builds": ChangeBuilding, "is-ready": ChangeReady, "merges": ChangeMerging,
		"is-blocked": ChangeBlocked, "rebuilds": ChangeRebuildRequested,
	} {
		if got, err := st.ChangeState(ctx, change); err != nil || got != want {
			t.Fatalf("%s is %q (err %v), want %q", change, got, err, want)
		}
	}
	// Still eight: what a change goes through does not take it out of the count, and a change in
	// progress is counted once whether or not the backlog still lists it.
	if n := count(backlog...); n != 8 {
		t.Fatalf("got %d open changes while five of them move, want 8", n)
	}
	if n := count(); n != 5 {
		t.Fatalf("got %d open changes without a backlog, want the 5 in progress", n)
	}

	// An integrated or abandoned change is finished, even while the backlog still lists it.
	if err := st.MarkChangeIntegrated(ctx, "lands", "abc123"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkChangeDiscarded(ctx, "goes"); err != nil {
		t.Fatal(err)
	}
	if n := count(backlog...); n != 6 {
		t.Fatalf("got %d open changes after one was integrated and one discarded, want 6", n)
	}
	if err := st.MarkChangeDiscarded(ctx, "is-blocked"); err != nil {
		t.Fatal(err)
	}
	if n := count(backlog...); n != 5 {
		t.Fatalf("got %d open changes after a blocked one was abandoned, want 5", n)
	}
}

// TestCountOpenChangesForgetsAnItemThatLeftTheBacklog: an item that left the file is not queued in
// the registry any more, and the count follows the file.
func TestCountOpenChangesForgetsAnItemThatLeftTheBacklog(t *testing.T) {
	ctx := context.Background()
	st := openMovementStore(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	backlog := filepath.Join(dir, ".foreman-kit", "agent_backlog.json")
	if err := os.WriteFile(backlog, []byte(`[{"change_name": "stays"}, {"change_name": "leaves"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncJSONToSQLite(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backlog, []byte(`[{"change_name": "stays"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncJSONToSQLite(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ChangeState(ctx, "leaves"); err != nil || got != ChangeUnknown {
		t.Fatalf("the registry holds the item that left as %q (err %v), want it out of the queue: %q", got, err, ChangeUnknown)
	}
	if n, err := st.CountOpenChanges(ctx, []string{"stays"}); err != nil || n != 1 {
		t.Fatalf("got %d open changes (err %v) for a backlog of one, want 1", n, err)
	}
}
