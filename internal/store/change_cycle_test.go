package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMirrored(t *testing.T, dir, file, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".foreman-kit", file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// cycleOf returns the cycle the registry names for a change, "" when it names none.
func cycleOf(t *testing.T, st *Store, change string) string {
	t.Helper()
	var stamp *string
	if err := st.DB().QueryRowContext(context.Background(), `SELECT cycle_stamp FROM changes WHERE change_name = ?`, change).Scan(&stamp); err != nil {
		t.Fatalf("%s: %v", change, err)
	}
	if stamp == nil {
		return ""
	}
	return *stamp
}

func backlogStatusOf(t *testing.T, st *Store, change string) string {
	t.Helper()
	var status string
	if err := st.DB().QueryRowContext(context.Background(), `SELECT backlog_status FROM changes WHERE change_name = ?`, change).Scan(&status); err != nil {
		t.Fatalf("%s: %v", change, err)
	}
	return status
}

// proposalOf returns the status the registry gives a proposal and the cycle it names for it, "" when
// it names none.
func proposalOf(t *testing.T, st *Store, id string) (status, cycle string) {
	t.Helper()
	var stamp *string
	if err := st.DB().QueryRowContext(context.Background(), `SELECT status, cycle_stamp FROM proposals WHERE proposal_id = ?`, id).Scan(&status, &stamp); err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	if stamp != nil {
		cycle = *stamp
	}
	return status, cycle
}

// waitingForADecision returns the proposals a person is asked to decide, as the inbox reads them.
func waitingForADecision(t *testing.T, st *Store) string {
	t.Helper()
	var ids []string
	err := st.View(context.Background(), func(v *View) error {
		in, err := v.ListInbox(context.Background())
		for _, p := range in.Proposals {
			ids = append(ids, p.ID)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(ids, ",")
}

// What a checkout lists now and did not list before is what was added to it: each item once, a list
// that was not read whole adds nothing, and a file that was not there listed nothing.
func TestAddedSinceIsWhatAListingDidNotHold(t *testing.T) {
	dir := t.TempDir()
	writeMirrored(t, dir, "agent_backlog.json", `[{"change_name": "queued-before"}]`)
	before := ListMirrored(dir)

	writeMirrored(t, dir, "agent_backlog.json", `[
		{"change_name": "queued-before"},
		{"change_name": "found-now", "description": "what it is about"},
		{"change_name": "found-now"},
		{"description": "an item without a name"}
	]`)
	writeMirrored(t, dir, "agent_proposals.json", `[{"change_name": "proposed-now"}, {"id": "proposed-by-its-id"}]`)
	work := before.AddedSince(dir)
	if len(work.Queued) != 1 || work.Queued[0]["change_name"] != "found-now" || work.Queued[0]["description"] != "what it is about" {
		t.Errorf("the backlog gained %v, want found-now once, as its file lists it", work.Queued)
	}
	if len(work.Proposed) != 2 || proposalID(work.Proposed[0]) != "proposed-now" || proposalID(work.Proposed[1]) != "proposed-by-its-id" {
		t.Errorf("the proposals gained %v, want the two a file that was not there now lists", work.Proposed)
	}

	// A backlog caught half-written is not known: nothing is said to have been added to it.
	writeMirrored(t, dir, "agent_backlog.json", `[{"change_name": "queued-before"},`)
	torn := ListMirrored(dir)
	writeMirrored(t, dir, "agent_backlog.json", `[{"change_name": "queued-before"}, {"change_name": "found-later"}]`)
	if work := torn.AddedSince(dir); len(work.Queued) != 0 || len(work.Proposed) != 0 {
		t.Errorf("a backlog that was not read whole gained %v, and the proposals %v", work.Queued, work.Proposed)
	}
}

// A cycle says what it queued and proposed. The queue and the proposals that wait for a decision
// follow the main checkout alone: what a cycle queued or proposed is on record, in no queue and
// before nobody, until that checkout lists it, and a checkout that lags behind neither takes it from
// its cycle nor hands it to the next one.
func TestACycleSaysWhatItQueued(t *testing.T) {
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
	queuedBy := func(stamp string) string {
		t.Helper()
		got, err := st.ChangesQueuedByCycle(ctx, stamp)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}

	// A person queued one change before any cycle.
	mirror(`[{"change_name": "queued-by-hand"}]`)

	// The first cycle queues a change and makes a proposal, in its own worktree. The main checkout
	// does not list them yet: a visual QA session keeps it as it is.
	work := CycleWork{
		Queued:   []map[string]any{{"change_name": "found-by-the-first", "description": "what the first cycle found", "priority": "high"}},
		Proposed: []map[string]any{{"change_name": "proposed-by-the-first", "status": "proposed"}},
	}
	if err := st.RecordCycleWork(ctx, first, work); err != nil {
		t.Fatal(err)
	}
	if got := queuedBy(first); got != "found-by-the-first" {
		t.Errorf("the first cycle queued %q, want found-by-the-first", got)
	}
	if status := backlogStatusOf(t, st, "found-by-the-first"); status != "unknown" {
		t.Errorf("a change the main checkout does not list is %s, want on record and in no queue", status)
	}
	var description, priority string
	if err := st.DB().QueryRowContext(ctx, `SELECT description, priority FROM changes WHERE change_name = 'found-by-the-first'`).Scan(&description, &priority); err != nil || description != "what the first cycle found" || priority != "high" {
		t.Errorf("the change is on record as %q, %q (%v), want what its item says", description, priority, err)
	}
	// So is the proposal, and it waits for no decision: a decision is written in the proposals file
	// of the main checkout, which does not list it.
	if status, by := proposalOf(t, st, "proposed-by-the-first"); status != ProposalUnknown || by != first {
		t.Errorf("a proposal the main checkout does not list is %s under the cycle %q, want %s under %q", status, by, ProposalUnknown, first)
	}
	if waiting := waitingForADecision(t, st); waiting != "" {
		t.Errorf("%s waits for a decision, and the main checkout does not list it", waiting)
	}

	// The mirror of that checkout, which still lags behind, changes nothing of it; neither does the
	// next cycle, which found the change already listed in its own worktree and added another.
	mirror(`[{"change_name": "queued-by-hand"}]`)
	if err := st.RecordCycleWork(ctx, second, CycleWork{Queued: []map[string]any{{"change_name": "found-by-the-second"}}}); err != nil {
		t.Fatal(err)
	}
	mirror(`[{"change_name": "queued-by-hand"}]`)
	if got, status := queuedBy(first), backlogStatusOf(t, st, "found-by-the-first"); got != "found-by-the-first" || status != "unknown" {
		t.Errorf("while the main checkout lags the first cycle queued %q and its change is %s", got, status)
	}
	if got := queuedBy(second); got != "found-by-the-second" {
		t.Errorf("the second cycle queued %q, want found-by-the-second alone", got)
	}
	if status, by := proposalOf(t, st, "proposed-by-the-first"); status != ProposalUnknown || by != first {
		t.Errorf("while the main checkout lags the proposal is %s under the cycle %q", status, by)
	}

	// The main checkout catches up: the changes are queued, each still its cycle's, and the proposal
	// waits for a decision, still its cycle's.
	writeMirrored(t, main, "agent_proposals.json", `[{"change_name": "proposed-by-the-first", "status": "proposed"}]`)
	mirror(`[{"change_name": "queued-by-hand"}, {"change_name": "found-by-the-first"}, {"change_name": "found-by-the-second"}]`)
	for change, stamp := range map[string]string{"queued-by-hand": "", "found-by-the-first": first, "found-by-the-second": second} {
		if status, got := backlogStatusOf(t, st, change), cycleOf(t, st, change); status != "queued" || got != stamp {
			t.Errorf("%s is %s and names the cycle %q, want queued and %q", change, status, got, stamp)
		}
	}
	if status, by := proposalOf(t, st, "proposed-by-the-first"); status != "proposed" || by != first {
		t.Errorf("once the main checkout lists it the proposal is %s under the cycle %q, want proposed under %q", status, by, first)
	}
	if waiting := waitingForADecision(t, st); waiting != "proposed-by-the-first" {
		t.Errorf("%q waits for a decision, want the proposal the main checkout now lists", waiting)
	}

	// A change that left the backlog and is queued again by a later cycle is that cycle's. So is a
	// proposal made again, which keeps the status the main checkout gives it: its item in the
	// worktree of the cycle does not decide that.
	mirror(`[{"change_name": "queued-by-hand"}, {"change_name": "found-by-the-second"}]`)
	again := CycleWork{
		Queued:   []map[string]any{{"change_name": "found-by-the-first"}},
		Proposed: []map[string]any{{"change_name": "proposed-by-the-first", "status": "approved"}},
	}
	if err := st.RecordCycleWork(ctx, second, again); err != nil {
		t.Fatal(err)
	}
	if got := queuedBy(second); got != "found-by-the-first,found-by-the-second" {
		t.Errorf("a change queued again belongs to the cycle that queued it again, got %q", got)
	}
	if status, by := proposalOf(t, st, "proposed-by-the-first"); status != "proposed" || by != second {
		t.Errorf("a proposal made again is %s under the cycle %q, want proposed, as the main checkout lists it, under %q", status, by, second)
	}

	// A change that was integrated keeps what it says, whatever names it again. So does a proposal.
	writeMirrored(t, main, "agent_history.json", `[{"change_name": "queued-by-hand", "status": "integrated"}]`)
	writeMirrored(t, main, "agent_proposals.json", `[{"change_name": "proposed-by-the-first", "status": "integrated"}]`)
	mirror(`[{"change_name": "found-by-the-second"}]`)
	integrated := CycleWork{
		Queued:   []map[string]any{{"change_name": "queued-by-hand"}},
		Proposed: []map[string]any{{"change_name": "proposed-by-the-first", "status": "proposed"}},
	}
	if err := st.RecordCycleWork(ctx, first, integrated); err != nil {
		t.Fatal(err)
	}
	if got, status := cycleOf(t, st, "queued-by-hand"), backlogStatusOf(t, st, "queued-by-hand"); got != "" || status != "integrated" {
		t.Errorf("an integrated change names the cycle %q and is %s", got, status)
	}
	if status, by := proposalOf(t, st, "proposed-by-the-first"); status != "integrated" || by != second {
		t.Errorf("an integrated proposal is %s under the cycle %q, want integrated under %q still", status, by, second)
	}

	// A cycle the registry does not know records nothing: there is no cycle to point at.
	if err := st.RecordCycleWork(ctx, "20261110T999999Z", CycleWork{Queued: []map[string]any{{"change_name": "found-by-nobody"}}}); err == nil {
		t.Error("work was recorded under a cycle that is not on record")
	}
}
