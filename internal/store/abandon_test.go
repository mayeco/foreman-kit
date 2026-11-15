package store

import (
	"context"
	"strings"
	"testing"
)

// TestAbandonRefusalForNamesTheChangesThatAreNotAbandoned: a change nobody knows, one a worker
// builds or the merger integrates, and one that is done are refused with the words the portal shows;
// a change that waits with nothing running on it is not.
func TestAbandonRefusalForNamesTheChangesThatAreNotAbandoned(t *testing.T) {
	for _, tc := range []struct {
		state    string
		wantKind string // "" when the change may be abandoned
		wantText string
	}{
		{"", AbandonUnknown, "the kit knows no change named add-ui"},
		{ChangeBuilding, AbandonInFlight, "a worker is building add-ui: it can be abandoned once its run ends"},
		{ChangeMerging, AbandonInFlight, "the merger is integrating add-ui: it can be abandoned once that attempt ends"},
		{ChangeIntegrated, AbandonDone, "add-ui is integrated: its commits are on the base branches"},
		{ChangeDiscarded, AbandonDone, "add-ui is already discarded"},
		{ChangeQueued, "", ""},
		{ChangeReady, "", ""},
		{ChangeBlocked, "", ""},
		{ChangeRebuildRequested, "", ""},
		{ChangeUnknown, "", ""},
	} {
		got := AbandonRefusalFor("add-ui", tc.state)
		if tc.wantKind == "" {
			if got != nil {
				t.Errorf("a change in state %q is refused: %v", tc.state, got)
			}
			continue
		}
		if got == nil || got.Kind != tc.wantKind || got.Error() != tc.wantText || got.State != tc.state {
			t.Errorf("state %q: got %+v, want kind %q and %q", tc.state, got, tc.wantKind, tc.wantText)
		}
	}
}

// TestCheckAbandonableReadsTheStateOfTheChange: the check sees what the rows say. A change with an
// open run is building and one with an open merge attempt is merging; neither is abandoned, and
// the check writes nothing, not even a row for a name the registry does not know.
func TestCheckAbandonableReadsTheStateOfTheChange(t *testing.T) {
	ctx := context.Background()
	st := openMovementStore(t)

	if err := st.CheckAbandonable(ctx, "never-queued"); AsAbandonRefusal(err) == nil || AsAbandonRefusal(err).Kind != AbandonUnknown {
		t.Fatalf("an unknown name is not refused as unknown: %v", err)
	}
	if state, err := st.ChangeState(ctx, "never-queued"); err != nil || state != "" {
		t.Fatalf("the check registered an unknown name: %q, %v", state, err)
	}

	if _, err := st.BeginChangeRun(ctx, "builds", 1, "/wt/builds", "feat/spec-builds"); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueReady(ctx, ReadyManifest{ChangeName: "merges", Branch: "feat/spec-merges", Worktree: "/wt/merges", HeadSHA: "abc123"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginMergeAttempt(ctx, "merges"); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueReady(ctx, ReadyManifest{ChangeName: "is-ready", Branch: "feat/spec-is-ready", Worktree: "/wt/is-ready", HeadSHA: "abc123"}); err != nil {
		t.Fatal(err)
	}
	if err := st.BlockChange(ctx, QuarantineRecord{ChangeName: "is-blocked", Reason: "gate_failure"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkChangeIntegrated(ctx, "lands", "abc123"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkChangeDiscarded(ctx, "goes"); err != nil {
		t.Fatal(err)
	}

	for change, want := range map[string]string{
		"builds": "a worker is building builds", "merges": "the merger is integrating merges",
		"lands": "lands is integrated", "goes": "goes is already discarded",
	} {
		err := st.CheckAbandonable(ctx, change)
		if r := AsAbandonRefusal(err); r == nil || !strings.Contains(r.Error(), want) {
			t.Errorf("%s: got %v, want a refusal that says %q", change, err, want)
		}
	}
	for _, change := range []string{"is-ready", "is-blocked"} {
		if err := st.CheckAbandonable(ctx, change); err != nil {
			t.Errorf("%s is refused: %v", change, err)
		}
	}
}
