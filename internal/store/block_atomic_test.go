package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// blockWriteState reads what a block writes, as stored: the open blocks, the change.blocked events,
// the outcome of the change's manifest and the result of its (only) merge attempt.
type blockWriteState struct {
	openBlocks      int
	events          int
	manifestOutcome string
	manifestOpen    bool
	attemptResult   string
	attemptOpen     bool
}

func readBlockWriteState(t *testing.T, st *Store, change string) blockWriteState {
	t.Helper()
	var s blockWriteState
	q := func(query string, dest ...any) {
		t.Helper()
		if err := st.db.QueryRow(query, change).Scan(dest...); err != nil {
			t.Fatal(err)
		}
	}
	q(`SELECT COUNT(*) FROM change_blocks WHERE change_name = ? AND resolved_at IS NULL`, &s.openBlocks)
	q(`SELECT COUNT(*) FROM events WHERE change_name = ? AND event = 'change.blocked'`, &s.events)
	q(`SELECT COALESCE(outcome, ''), closed_at IS NULL FROM ready_manifests WHERE change_name = ?`, &s.manifestOutcome, &s.manifestOpen)
	q(`SELECT COALESCE(a.result, ''), a.finished_at IS NULL FROM merge_attempts a
	   JOIN ready_manifests r ON r.id = a.manifest_id WHERE r.change_name = ?`, &s.attemptResult, &s.attemptOpen)
	return s
}

// queuedWithAttempt puts change in the merge queue with a merge attempt open.
func queuedWithAttempt(t *testing.T, st *Store, change string) (context.Context, int64) {
	t.Helper()
	ctx := context.Background()
	if err := st.EnqueueReady(ctx, ReadyManifest{ChangeName: change, Branch: "feat/" + change, Worktree: "/tmp/wt", HeadSHA: "abc123"}); err != nil {
		t.Fatal(err)
	}
	id, err := st.BeginMergeAttempt(ctx, change)
	if err != nil {
		t.Fatal(err)
	}
	return WithMergeAttempt(ctx, id, change), id
}

func mergerBlockEffects(change string, attemptID int64) BlockEffects {
	return BlockEffects{
		Event:         &EventInput{Event: "change.blocked", Change: change, Payload: map[string]any{"reason": "gate_failure"}},
		CloseManifest: true,
		AttemptID:     attemptID,
		AttemptResult: MergeBlocked,
		AttemptReason: "gate_failure",
	}
}

// The merger's block is one commit: the open block, the event, the manifest closed blocked and the
// attempt finished blocked.
func TestBlockChangeWithWritesBlockEventManifestAndAttemptTogether(t *testing.T) {
	st := openTestStore(t)
	const change = "blocked-in-one-commit"
	ctx, attemptID := queuedWithAttempt(t, st, change)

	if err := st.BlockChangeWith(ctx, QuarantineRecord{ChangeName: change, Reason: "gate_failure"}, mergerBlockEffects(change, attemptID)); err != nil {
		t.Fatal(err)
	}
	got := readBlockWriteState(t, st, change)
	want := blockWriteState{openBlocks: 1, events: 1, manifestOutcome: ManifestBlocked, attemptResult: MergeBlocked}
	if got != want {
		t.Fatalf("after the block: got %+v, want %+v", got, want)
	}
}

// A failing statement of the block leaves none of its writes: not the block, not the event, not the
// manifest's exit from the queue, not the attempt's result.
func TestBlockChangeWithForcedFailureLeavesNothing(t *testing.T) {
	cases := []struct{ name, trigger string }{
		{"the event insert", `BEFORE INSERT ON events WHEN NEW.event = 'change.blocked'`},
		{"the manifest close", `BEFORE UPDATE ON ready_manifests WHEN NEW.outcome = 'blocked'`},
		{"the attempt finish", `BEFORE UPDATE ON merge_attempts WHEN NEW.result = 'blocked'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openTestStore(t)
			const change = "block-that-fails"
			ctx, attemptID := queuedWithAttempt(t, st, change)
			before := readBlockWriteState(t, st, change)
			if _, err := st.db.Exec(`CREATE TRIGGER forced_failure ` + tc.trigger + ` BEGIN SELECT RAISE(ABORT, 'forced failure'); END`); err != nil {
				t.Fatal(err)
			}

			if err := st.BlockChangeWith(ctx, QuarantineRecord{ChangeName: change, Reason: "gate_failure"}, mergerBlockEffects(change, attemptID)); err == nil {
				t.Fatal("the block succeeded although a statement of it failed")
			}
			if got := readBlockWriteState(t, st, change); got != before {
				t.Fatalf("a failed block left writes behind: got %+v, want %+v", got, before)
			}
			if !before.manifestOpen || !before.attemptOpen || before.openBlocks != 0 {
				t.Fatalf("the starting state is wrong: %+v", before)
			}
		})
	}
}

// The dispatcher's block has no manifest or attempt: the block and its event are written together,
// and a failing event leaves no block.
func TestBlockChangeWithEventOnly(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	ev := BlockEffects{Event: &EventInput{Event: "change.blocked", Change: "dispatcher-block", Payload: map[string]any{"reason": "gate_failure"}}}

	if err := st.BlockChangeWith(ctx, QuarantineRecord{ChangeName: "dispatcher-block", Reason: "gate_failure"}, ev); err != nil {
		t.Fatal(err)
	}
	var blocks, events int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM change_blocks WHERE change_name = 'dispatcher-block' AND resolved_at IS NULL`).Scan(&blocks); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM events WHERE change_name = 'dispatcher-block' AND event = 'change.blocked'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if blocks != 1 || events != 1 {
		t.Fatalf("expected one block and one event, got %d and %d", blocks, events)
	}

	if _, err := st.db.Exec(`CREATE TRIGGER forced_failure BEFORE INSERT ON events WHEN NEW.event = 'change.blocked' BEGIN SELECT RAISE(ABORT, 'forced failure'); END`); err != nil {
		t.Fatal(err)
	}
	ev.Event.Change = "dispatcher-block-fails"
	if err := st.BlockChangeWith(ctx, QuarantineRecord{ChangeName: "dispatcher-block-fails", Reason: "gate_failure"}, ev); err == nil {
		t.Fatal("the block succeeded although its event failed")
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM change_blocks WHERE change_name = 'dispatcher-block-fails'`).Scan(&blocks); err != nil {
		t.Fatal(err)
	}
	if blocks != 0 {
		t.Fatalf("a block was left without its event: %d", blocks)
	}
}

// staleDaemonLock leaves a lock row of a dead local process, which the next AcquireLock reclaims.
func staleDaemonLock(t *testing.T, st *Store, key string) string {
	t.Helper()
	hostname, _ := os.Hostname()
	if _, err := st.AcquireLock(context.Background(), key, LockOptions{OwnerType: "worker", PID: 99999999, Host: hostname, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	return hostname
}

func lockOwnerPID(t *testing.T, st *Store, key string) int {
	t.Helper()
	var pid int
	if err := st.db.QueryRow(`SELECT pid FROM locks WHERE lock_key = ?`, key).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

// A reclaimed stale lock and its lease.stale_reclaimed event are written in one transaction.
func TestAcquireLockWritesTheReclaimEventInItsTransaction(t *testing.T) {
	const key = "change:reclaimed"
	t.Run("the event commits with the row", func(t *testing.T) {
		st := openTestStore(t)
		hostname := staleDaemonLock(t, st, key)
		if _, err := st.AcquireLock(context.Background(), key, LockOptions{OwnerType: "worker", PID: os.Getpid(), Host: hostname, TTL: time.Hour}); err != nil {
			t.Fatal(err)
		}
		evts, err := st.QueryEvents(context.Background(), EventFilter{Event: "lease.stale_reclaimed"})
		if err != nil {
			t.Fatal(err)
		}
		if len(evts) != 1 || evts[0].Payload["lock_key"] != key {
			t.Fatalf("expected one lease.stale_reclaimed event for %s, got %+v", key, evts)
		}
		if pid := lockOwnerPID(t, st, key); pid != os.Getpid() {
			t.Fatalf("the lock was not taken over: pid %d", pid)
		}
	})

	t.Run("a failing event keeps the stale row", func(t *testing.T) {
		st := openTestStore(t)
		hostname := staleDaemonLock(t, st, key)
		if _, err := st.db.Exec(`CREATE TRIGGER forced_failure BEFORE INSERT ON events WHEN NEW.event = 'lease.stale_reclaimed' BEGIN SELECT RAISE(ABORT, 'forced failure'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AcquireLock(context.Background(), key, LockOptions{OwnerType: "worker", PID: os.Getpid(), Host: hostname, TTL: time.Hour}); err == nil {
			t.Fatal("AcquireLock succeeded although its event failed")
		}
		if pid := lockOwnerPID(t, st, key); pid != 99999999 {
			t.Fatalf("the stale row was replaced without its event: pid %d", pid)
		}
		// The writer connection is free again: the lock can be taken once the failure is gone.
		if _, err := st.db.Exec(`DROP TRIGGER forced_failure`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AcquireLock(context.Background(), key, LockOptions{OwnerType: "worker", PID: os.Getpid(), Host: hostname, TTL: time.Hour}); err != nil {
			t.Fatal(err)
		}
	})
}
