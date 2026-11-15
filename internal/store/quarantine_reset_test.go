package store

import (
	"context"
	"testing"
	"time"
)

type closedBlock struct {
	id         int64
	reason     string
	resolvedAt int64
	resolution string
	resolvedBy string
}

// blocksOf reads every block of change as it is stored, the oldest first.
func blocksOf(t *testing.T, st *Store, change string) []closedBlock {
	t.Helper()
	rows, err := st.db.Query(`
		SELECT id, reason, COALESCE(resolved_at, 0), COALESCE(resolution, ''), COALESCE(resolved_by, '')
		FROM change_blocks WHERE change_name = ? ORDER BY id`, change)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []closedBlock
	for rows.Next() {
		var b closedBlock
		if err := rows.Scan(&b.id, &b.reason, &b.resolvedAt, &b.resolution, &b.resolvedBy); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func blockRetryEvidence(t *testing.T, st *Store) BlockRetryEvidence {
	t.Helper()
	ev, err := st.SettingEvidence(context.Background(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return ev.BlockRetries
}

// TestResolveQuarantineResetKeepsAClosedBlockAsItWas: an operator resets a change whose block the
// dispatcher already retried and that built ready again. The block keeps its resolution, its stamp
// and the retry it counts as in the evidence; the counters start over through a block of their own.
func TestResolveQuarantineResetKeepsAClosedBlockAsItWas(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const change = "retry-then-reset"

	if err := st.BlockChange(ctx, QuarantineRecord{ChangeName: change, Reason: "gate_failure", Details: "tests failed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceQuarantineRetry(ctx, change); err != nil {
		t.Fatal(err)
	}
	id, err := st.BeginChangeRun(ctx, change, 1, "/wt/"+change, "feat/spec-"+change)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishChangeRun(ctx, id, "ready"); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueReady(ctx, ReadyManifest{ChangeName: change, Branch: "feat/spec-" + change, Worktree: "/wt/" + change, HeadSHA: "abc123"}); err != nil {
		t.Fatal(err)
	}

	before := blocksOf(t, st, change)
	if len(before) != 1 || before[0].resolution != BlockAutoRetry {
		t.Fatalf("blocks before the reset = %+v, want one auto_retry", before)
	}
	if got, want := blockRetryEvidence(t, st), (BlockRetryEvidence{Retried: 1, Ready: 1}); got != want {
		t.Fatalf("retry evidence before the reset = %+v, want %+v", got, want)
	}
	if q, _ := st.GetQuarantine(ctx, change); q == nil || q.Retries != 1 {
		t.Fatalf("quarantine before the reset = %+v, want one retry", q)
	}

	if err := st.ResolveQuarantine(ctx, change, QuarantineResolution{Resolution: BlockUnblocked, By: "cli", Forget: true}); err != nil {
		t.Fatal(err)
	}

	after := blocksOf(t, st, change)
	if len(after) != 2 {
		t.Fatalf("blocks after the reset = %+v, want the original and one marker", after)
	}
	if after[0] != before[0] {
		t.Fatalf("the closed block was rewritten: %+v, was %+v", after[0], before[0])
	}
	if m := after[1]; m.reason != BlockReasonCountersReset || m.resolution != BlockUnblocked || m.resolvedBy != "cli" || m.resolvedAt <= before[0].resolvedAt {
		t.Fatalf("the marker of the reset = %+v", m)
	}
	if got, want := blockRetryEvidence(t, st), (BlockRetryEvidence{Retried: 1, Ready: 1}); got != want {
		t.Fatalf("retry evidence after the reset = %+v, want %+v", got, want)
	}
	if q, _ := st.GetQuarantine(ctx, change); q != nil && (q.Retries != 0 || q.Rebuilds != 0) {
		t.Fatalf("counters after the reset = %+v, want 0 retries and 0 rebuilds", q)
	}
	history, _ := st.ListQuarantineHistory(ctx, change, 0)
	var kept bool
	for _, h := range history {
		if h.Resolution == BlockAutoRetry && h.Reason == "gate_failure" {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("the history lost the auto_retry block: %+v", history)
	}

	// The next block counts from zero, and is not a retry of the old one.
	if err := st.BlockChange(ctx, QuarantineRecord{ChangeName: change, Reason: "gate_failure"}); err != nil {
		t.Fatal(err)
	}
	if q, _ := st.GetQuarantine(ctx, change); q == nil || q.Retries != 0 || q.Status != "blocked" {
		t.Fatalf("after a new block: %+v, want blocked with 0 retries", q)
	}
}

// TestResolveQuarantineResetKeepsAnApprovedBlock: the block a person approved keeps who approved it
// when the operator resets the change afterwards.
func TestResolveQuarantineResetKeepsAnApprovedBlock(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const change = "approved-then-reset"

	if err := st.BlockChange(ctx, QuarantineRecord{ChangeName: change, Reason: "risk_review"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApproveChangeRiskBy(ctx, change, "alice"); err != nil {
		t.Fatal(err)
	}
	before := blocksOf(t, st, change)
	if len(before) != 1 || before[0].resolution != BlockApproved || before[0].resolvedBy != "alice" {
		t.Fatalf("blocks before the reset = %+v, want one block approved by alice", before)
	}

	if err := st.ResolveQuarantine(ctx, change, QuarantineResolution{Resolution: BlockUnblocked, By: "cli", Forget: true}); err != nil {
		t.Fatal(err)
	}
	after := blocksOf(t, st, change)
	if len(after) != 2 || after[0] != before[0] {
		t.Fatalf("blocks after the reset = %+v, want the approved block untouched and a marker", after)
	}
	if q, _ := st.GetQuarantine(ctx, change); q != nil {
		t.Fatalf("a reset change still has a quarantine: %+v", q)
	}
}

// TestResolveQuarantineAbandonKeepsAClosedBlock: abandoning a change with no open block records the
// abandonment beside its closed blocks instead of rewriting the latest one.
func TestResolveQuarantineAbandonKeepsAClosedBlock(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const change = "retried-then-abandoned"

	if err := st.BlockChange(ctx, QuarantineRecord{ChangeName: change, Reason: "gate_failure"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceQuarantineRetry(ctx, change); err != nil {
		t.Fatal(err)
	}
	before := blocksOf(t, st, change)
	if err := st.ResolveQuarantine(ctx, change, QuarantineResolution{Resolution: BlockAbandoned, By: "portal", Forget: true}); err != nil {
		t.Fatal(err)
	}
	after := blocksOf(t, st, change)
	if len(after) != 2 || after[0] != before[0] || after[1].resolution != BlockAbandoned || after[1].resolvedBy != "portal" {
		t.Fatalf("blocks after the abandonment = %+v (before %+v)", after, before)
	}
}

// TestResetMarkerIsNotMovement: the block that marks where the counters start over is neither a
// block of the change in MovementBetween nor in the evidence the cycles read. Markers of earlier
// versions (reason rebuild_requested) are left out the same way.
func TestResetMarkerIsNotMovement(t *testing.T) {
	st := openMovementStore(t)
	ctx := context.Background()
	const change = "marker-not-movement"

	if err := st.BlockChange(ctx, QuarantineRecord{ChangeName: change, Reason: "gate_failure"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceQuarantineRetry(ctx, change); err != nil {
		t.Fatal(err)
	}
	if err := st.ResolveQuarantine(ctx, change, QuarantineResolution{Resolution: BlockUnblocked, By: "cli", Forget: true}); err != nil {
		t.Fatal(err)
	}
	// A marker as an earlier version recorded it, for a change that only had rebuild requests.
	now := time.Now().UnixMilli()
	if _, err := st.db.Exec(`INSERT INTO change_blocks (change_name, reason, blocked_at, resolved_at, resolution)
		VALUES (?, 'rebuild_requested', ?, ?, 'unblocked')`, change, now+1000, now+1000); err != nil {
		t.Fatal(err)
	}

	blocks := blocksOf(t, st, change)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %+v, want the block and two markers", blocks)
	}
	got, err := st.MovementBetween(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.Blocked != 1 {
		t.Fatalf("MovementBetween counts %d blocks, want 1 (the markers are not blocks)", got.Blocked)
	}
	// Only the markers' stretch: nothing moved.
	only, err := st.MovementBetween(ctx, time.UnixMilli(blocks[0].resolvedAt), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if only.Any() {
		t.Fatalf("a stretch holding only markers moved %+v, want nothing", only)
	}
}
