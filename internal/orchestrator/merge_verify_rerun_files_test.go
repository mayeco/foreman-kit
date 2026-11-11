package orchestrator

import (
	"context"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// diffAwareCoreTest is a test gate that, like `rspec $(script/related_specs)`, runs only the tests of
// the files in FOREMANKIT_CHANGED_FILES: the tests of feature.txt fail when a file named BROKEN exists,
// and the tests of a.txt fail when a.txt and hotfix.txt both exist.
const diffAwareCoreTest = `{ printf '%s\n' "$FOREMANKIT_CHANGED_FILES" | grep -qx feature.txt && [ -e BROKEN ]; } || { printf '%s\n' "$FOREMANKIT_CHANGED_FILES" | grep -qx a.txt && [ -e a.txt ] && [ -e hotfix.txt ]; }`

// A failing gate reruns on the base alone with the change's own files: a diff-aware gate then runs the
// tests that failed, so a hotfix that broke them is found, and the change is held, not blamed.
func TestMergeVerifyGateRerunUsesTheChangesOwnFiles(t *testing.T) {
	sb := newMCFSandboxWith(t, mcfOptions{coreTestFailsWhen: diffAwareCoreTest})
	sb.buildCoreChange(t, "core-feature")
	hotfix := sb.pushCoreChanges(t, map[string]string{"hotfix.txt": "hotfix\n", "BROKEN": "broken\n"}, nil, "fix(core): a hotfix that breaks feature.txt's tests")

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	sb.requireHeld(t, "core-feature")
	tests := sb.coreRuns(t, "test")
	if len(tests) != 2 || tests[1].head != hotfix || tests[1].files != "feature.txt" {
		t.Fatalf("expected the rerun on the base alone with feature.txt, got %+v", tests)
	}
	if v := sb.verdict(t, hotfix, BaseCheckGatePrefix+"test"); v == nil || v.Status != store.BaseVerdictFailed {
		t.Fatalf("expected a failed gate:test verdict, got %+v", v)
	}
}

// A passed gate verdict is not reused for another change: a-change's rerun passes on the base (its own
// tests), and core-feature's rerun, with its own files, fails there and replaces that verdict.
func TestMergeVerifyPassedGateVerdictIsNotReusedForAnotherChange(t *testing.T) {
	sb := newMCFSandboxWith(t, mcfOptions{coreTestFailsWhen: diffAwareCoreTest})
	sb.buildCoreChangeWriting(t, "a-change", map[string]string{"a.txt": "a\n"})
	sb.buildCoreChange(t, "core-feature")
	hotfix := sb.pushCoreChanges(t, map[string]string{"hotfix.txt": "hotfix\n", "BROKEN": "broken\n"}, nil, "fix(core): a hotfix")
	ctx := context.Background()

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	if q, _ := sb.st.GetQuarantine(ctx, "a-change"); q == nil || q.Reason != "gate_failure" {
		t.Fatalf("expected a-change quarantined as gate_failure (its own tests pass on the base alone), got %+v", q)
	}
	held := false
	for _, r := range sb.readyNames(t) {
		held = held || r == "core-feature"
	}
	if q, _ := sb.st.GetQuarantine(ctx, "core-feature"); !held || q != nil {
		t.Fatalf("expected core-feature held in the merge queue without quarantine, ready=%v quarantine=%+v", sb.readyNames(t), q)
	}
	if v := sb.verdict(t, hotfix, BaseCheckGatePrefix+"test"); v == nil || v.Status != store.BaseVerdictFailed {
		t.Fatalf("expected core-feature's rerun to replace the passed verdict with a failed one, got %+v", v)
	}
}
