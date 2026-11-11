package orchestrator

import (
	"context"
	"testing"
)

// TestABaseThatMovedOffIsNoLongerBrokenWhenTheAttemptIsSentBack: a person fixed a broken base, but
// the held change then conflicted with the fix and was sent back for a rebuild. The broken verdict
// was only resolved by an attempt that got past the rebase, so status kept saying BASE BROKEN and every
// merger process announced the move again.
func TestABaseThatMovedOffIsNoLongerBrokenWhenTheAttemptIsSentBack(t *testing.T) {
	sb := newMCFSandbox(t, "verify")
	ctx := context.Background()

	sb.runCoreDispatch(t, "add-greeting", map[string]string{"greeting.txt": "hola\n"})
	sb.pushCoreCommit(t, "config/locales/BROKEN", "x\n", "fix(core): a locale that breaks the regression")
	sb.mergerRun(t)
	if broken, _ := sb.st.ListBrokenBases(ctx); len(broken) != 1 {
		t.Fatalf("broken bases = %+v, want core's", broken)
	}

	// The person's fix also writes greeting.txt, so the held change now conflicts.
	sb.pushCoreChanges(t, map[string]string{"greeting.txt": "buenas\n"}, []string{"config/locales/BROKEN"}, "fix(core): remove the broken locale")
	sb.mergerRun(t)
	if q, _ := sb.st.GetQuarantine(ctx, "add-greeting"); q == nil || q.Status != "rebuild_requested" {
		t.Fatalf("quarantine = %+v, want the change sent back for a rebuild", q)
	}
	if broken, _ := sb.st.ListBrokenBases(ctx); len(broken) != 0 {
		t.Fatalf("broken bases = %+v, want none: the base moved off the broken sha", broken)
	}
}
