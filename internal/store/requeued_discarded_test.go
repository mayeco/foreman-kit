package store

import (
	"context"
	"testing"
)

// A name the registry holds as discarded stays discarded when the backlog lists it again, and the cap
// on the queue does not count it: abandoning is terminal, like integrating. The dispatcher agrees (it
// starts no run for such a name), so no reader sees a change that builds while it says discarded.
func TestARequeuedDiscardedNameStaysDiscardedAndUncounted(t *testing.T) {
	st := openMovementStore(t)
	ctx := context.Background()
	main := t.TempDir()
	const name = "fix-foo"

	mirror := func(backlog string) {
		t.Helper()
		writeMirrored(t, main, "agent_backlog.json", backlog)
		if err := st.SyncJSONToSQLite(ctx, main); err != nil {
			t.Fatal(err)
		}
	}
	state := func(when string) {
		t.Helper()
		if got, err := st.ChangeState(ctx, name); err != nil || got != ChangeDiscarded {
			t.Fatalf("%s, %s is %q (err %v), want %q", when, name, got, err, ChangeDiscarded)
		}
		if n, err := st.CountOpenChanges(ctx, []string{name}); err != nil || n != 0 {
			t.Fatalf("%s, %d open changes for a backlog of the discarded name (err %v), want 0", when, n, err)
		}
	}

	mirror(`[{"change_name": "fix-foo"}]`)
	if got, _ := st.ChangeState(ctx, name); got != ChangeQueued {
		t.Fatalf("a listed change is %q, want %q", got, ChangeQueued)
	}
	mirror(`[]`)
	if err := st.MarkChangeDiscarded(ctx, name); err != nil {
		t.Fatal(err)
	}
	state("after the abandon")

	// Queued again under the same name.
	mirror(`[{"change_name": "fix-foo"}]`)
	state("after the name is queued again")
	if n, err := st.CountOpenChanges(ctx, nil); err != nil || n != 0 {
		t.Fatalf("%d open changes with an empty backlog (err %v), want 0", n, err)
	}
}
