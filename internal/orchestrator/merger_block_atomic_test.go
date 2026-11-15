package orchestrator

import (
	"context"
	"io"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// The merger's block is one store write: when it fails (here the event cannot be inserted) the change
// is not half blocked. It keeps its manifest in the merge queue with no block, so the next pass takes
// it again, and only the attempt is closed, as stopped.
func TestMergerBlockThatFailsLeavesTheChangeQueuedWithNoBlock(t *testing.T) {
	bg := context.Background()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)
	const change = "block-write-fails"
	buildReadyManifest(t, bg, umbrellaDir, st, change)
	m, err := NewMerger(MergerOptions{Root: umbrellaDir, Store: st, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	manifest := readyManifestOf(t, st, change)
	if m.attemptID, err = st.BeginMergeAttempt(bg, change); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`CREATE TRIGGER forced_failure BEFORE INSERT ON events WHEN NEW.event = 'change.blocked' BEGIN SELECT RAISE(ABORT, 'forced failure'); END`); err != nil {
		t.Fatal(err)
	}

	m.blockChange(store.WithMergeAttempt(bg, m.attemptID, change), manifest, NewBlockedError("gate_failure", "unit tests failed"))

	if q, _ := st.GetQuarantine(bg, change); q != nil && q.Status == "blocked" {
		t.Fatalf("a block was recorded although its write failed: %+v", q)
	}
	if !inReadyQueue(bg, st, change) {
		t.Fatal("the change left the merge queue although it was not blocked")
	}
	if hasEvent(bg, st, "change.blocked", change) {
		t.Fatal("a change.blocked event was recorded for a block that was not written")
	}
	attempts, _ := st.ListMergeAttempts(bg, change, 10)
	if len(attempts) != 1 || attempts[0].Result != store.MergeStopped {
		t.Fatalf("expected the attempt closed as stopped, got %+v", attempts)
	}
	if m.attemptID != 0 {
		t.Fatalf("the merger still holds attempt %d", m.attemptID)
	}
}
