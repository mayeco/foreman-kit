package orchestrator

import (
	"context"
	"testing"
)

// End to end: the rerun on the base alone must not see the state the change's gates left in their slot
// (a migration applied to the test database): the change is quarantined, the base is not broken.
func TestMergeVerifyBaseRerunDoesNotSeeTheChangesSlotState(t *testing.T) {
	cond := `[ "$FOREMANKIT_SLOT" = 99 ] && { if [ -e MIGRATION ]; then echo migrated > "$MARKER_DIR/db-99"; fi; [ -s "$MARKER_DIR/db-99" ]; }`
	sb := newMCFSandboxWith(t, mcfOptions{coreTestFailsWhen: cond})
	sb.buildCoreChangeWriting(t, "core-migration", map[string]string{"MIGRATION": "add_column users.tenant_id null: false\n"})
	sb.pushCoreCommit(t, "notes.txt", "release notes\n", "docs(core): release notes")

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))

	if evs := sb.events(t, "merge.base_broken"); len(evs) != 0 {
		t.Fatalf("the change's own slot state made the base look broken: %+v", evs)
	}
	q, _ := sb.st.GetQuarantine(context.Background(), "core-migration")
	if q == nil || q.Reason != "gate_failure" {
		t.Fatalf("want core-migration quarantined as gate_failure, got %+v", q)
	}
}

// End to end: the rerun on the base alone must not run the change's version of a sibling repository:
// core's test fails only with the change's mobile (BREAKS_CORE) and core's hotfix (notes.txt) together,
// so it passes on the base alone, where mobile is at its own base.
func TestMergeVerifyBaseRerunDoesNotSeeTheChangesSibling(t *testing.T) {
	cond := `[ -e ../mobile/BREAKS_CORE ] && [ -e notes.txt ]`
	sb := newMCFSandboxWith(t, mcfOptions{coreTestFailsWhen: cond})
	sb.buildCoreChangeWriting(t, "two-repos", map[string]string{"feature.txt": "core payload\n", "repos/mobile/BREAKS_CORE": "x\n"})
	sb.pushCoreCommit(t, "notes.txt", "release notes\n", "docs(core): release notes")

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))

	if evs := sb.events(t, "merge.base_broken"); len(evs) != 0 {
		t.Fatalf("the change's sibling made the base look broken: %+v", evs)
	}
	q, _ := sb.st.GetQuarantine(context.Background(), "two-repos")
	if q == nil || q.Reason != "gate_failure" {
		t.Fatalf("want two-repos quarantined as gate_failure, got %+v", q)
	}
}
