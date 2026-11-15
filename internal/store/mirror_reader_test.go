package store

import (
	"context"
	"database/sql"
	"testing"
)

// The mirror reads the backlog, the history and the proposals the way the dispatcher, the merger and
// cycle.ReadJSONList do: a JSON list, or the legacy {"changes": [...]} wrapper, pretty-printed or not.
// A wrapper read whole is whole: it queues what it lists and takes out of the queue what it no longer
// lists, and its history marks changes integrated.
func TestTheMirrorReadsTheChangesWrapperLikeTheDispatcher(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	dir := t.TempDir()
	sync := func() {
		t.Helper()
		if err := st.SyncJSONToSQLite(ctx, dir); err != nil {
			t.Fatal(err)
		}
	}
	state := func(change string) string {
		t.Helper()
		got, err := st.ChangeState(ctx, change)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	writeMirrored(t, dir, "agent_backlog.json", `[{"change_name": "old-item"}]`)
	sync()
	if got := state("old-item"); got != ChangeQueued {
		t.Fatalf("old-item is %q after a list backlog, want queued", got)
	}

	writeMirrored(t, dir, "agent_backlog.json", `{
  "changes": [
    {
      "change_name": "new-item",
      "priority": "high",
      "description": "listed in a wrapper"
    }
  ]
}
`)
	writeMirrored(t, dir, "agent_history.json", `{
  "changes": [
    {"change_name": "done-item", "integrated_at": "2026-11-01T10:00:00Z"},
    {"change_name": "dropped-item", "status": "discarded"}
  ]
}`)
	writeMirrored(t, dir, "agent_proposals.json", `{"changes": [{"change_name": "proposed-item", "status": "approved"}]}`)
	sync()

	for change, want := range map[string]string{
		"new-item":     ChangeQueued,
		"old-item":     ChangeUnknown,
		"done-item":    ChangeIntegrated,
		"dropped-item": ChangeDiscarded,
	} {
		if got := state(change); got != want {
			t.Errorf("%s is %q after the wrapper files, want %q", change, got, want)
		}
	}
	var priority, description string
	if err := st.DB().QueryRowContext(ctx, `SELECT priority, description FROM changes WHERE change_name = 'new-item'`).Scan(&priority, &description); err != nil {
		t.Fatal(err)
	}
	if priority != "high" || description != "listed in a wrapper" {
		t.Errorf("new-item holds priority %q and description %q, want what its wrapper item says", priority, description)
	}
	var proposalStatus string
	if err := st.DB().QueryRowContext(ctx, `SELECT status FROM proposals WHERE proposal_id = 'proposed-item'`).Scan(&proposalStatus); err != nil || proposalStatus != "approved" {
		t.Errorf("the proposal of a wrapper is %q (err %v), want approved", proposalStatus, err)
	}

	// An empty wrapper is a backlog read whole that lists nothing.
	writeMirrored(t, dir, "agent_backlog.json", `{"changes": []}`)
	sync()
	if got := state("new-item"); got != ChangeUnknown {
		t.Errorf("new-item is %q after an empty wrapper backlog, want unknown", got)
	}
}

// A JSON object without a "changes" list is not a list of changes, for the dispatcher, the merger or
// the cycle: nothing is read from it, and, not read whole, it takes nothing out of the queue.
func TestTheMirrorTakesNothingFromAnObjectThatIsNotAWrapper(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	dir := t.TempDir()
	writeMirrored(t, dir, "agent_backlog.json", `[{"change_name": "queued-item"}]`)
	if err := st.SyncJSONToSQLite(ctx, dir); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{
		`{"backlog": [{"change_name": "other-item"}]}`,
		`{"change_name": "one-item-without-a-list"}`,
		`{"changes": null}`,
		`"text"`,
	} {
		writeMirrored(t, dir, "agent_backlog.json", content)
		if err := st.SyncJSONToSQLite(ctx, dir); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.ChangeState(ctx, "queued-item"); got != ChangeQueued {
			t.Errorf("queued-item is %q after a backlog %s, want it left queued", got, content)
		}
		for _, name := range []string{"other-item", "one-item-without-a-list"} {
			if got, _ := st.ChangeState(ctx, name); got != "" {
				t.Errorf("%s is %q after a backlog %s, want it not registered", name, got, content)
			}
		}
	}
}

// What is added to a backlog or a proposals file in the wrapper shape is what the cycle queued and
// proposed, as for a list.
func TestAddedSinceReadsTheChangesWrapper(t *testing.T) {
	dir := t.TempDir()
	writeMirrored(t, dir, "agent_backlog.json", `{"changes": [{"change_name": "queued-before"}]}`)
	writeMirrored(t, dir, "agent_proposals.json", `{"changes": [{"change_name": "proposed-before"}]}`)
	before := ListMirrored(dir)
	if !before.backlogKnown || !before.proposalsKnown || !before.backlog["queued-before"] || !before.proposals["proposed-before"] {
		t.Fatalf("a wrapper listing was not read whole: %+v", before)
	}

	writeMirrored(t, dir, "agent_backlog.json", `{
  "changes": [
    {"change_name": "queued-before"},
    {"change_name": "found-now", "description": "added to a wrapper"}
  ]
}`)
	writeMirrored(t, dir, "agent_proposals.json", `{"changes": [{"change_name": "proposed-before"}, {"change_name": "proposed-now"}]}`)
	work := before.AddedSince(dir)
	if len(work.Queued) != 1 || work.Queued[0]["change_name"] != "found-now" {
		t.Errorf("the wrapper backlog gained %v, want found-now", work.Queued)
	}
	if len(work.Proposed) != 1 || proposalID(work.Proposed[0]) != "proposed-now" {
		t.Errorf("the wrapper proposals gained %v, want proposed-now", work.Proposed)
	}
}

// An impact_score written as a numeric string is the number it spells, as the dispatcher reads it;
// text that is no number leaves it unset.
func TestTheMirrorStoresAnImpactScoreWrittenAsAString(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	dir := t.TempDir()
	writeMirrored(t, dir, "agent_backlog.json", `[
		{"change_name": "as-string", "impact_score": "0.8"},
		{"change_name": "as-padded-string", "impact_score": " 0.25 "},
		{"change_name": "as-number", "impact_score": 0.6},
		{"change_name": "as-text", "impact_score": "high"},
		{"change_name": "as-none"}
	]`)
	writeMirrored(t, dir, "agent_history.json", `[{"change_name": "done-as-string", "impact_score": "0.7"}]`)
	if err := st.SyncJSONToSQLite(ctx, dir); err != nil {
		t.Fatal(err)
	}
	for change, want := range map[string]sql.NullFloat64{
		"as-string":        {Float64: 0.8, Valid: true},
		"as-padded-string": {Float64: 0.25, Valid: true},
		"as-number":        {Float64: 0.6, Valid: true},
		"done-as-string":   {Float64: 0.7, Valid: true},
		"as-text":          {},
		"as-none":          {},
	} {
		var got sql.NullFloat64
		if err := st.DB().QueryRowContext(ctx, `SELECT impact_score FROM changes WHERE change_name = ?`, change).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s has impact_score %v, want %v", change, got, want)
		}
	}
}
