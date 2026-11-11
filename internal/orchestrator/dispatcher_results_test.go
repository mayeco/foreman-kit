package orchestrator

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// TestDispatcherFinishReportsChangesTheMergerBlockedAfterwards: dispatcher.finish used to say
// "ready" for a change its worker left ready although the merger blocked it before the dispatch
// ended (a rebase conflict its rebuilds could not resolve).
func TestDispatcherFinishReportsChangesTheMergerBlockedAfterwards(t *testing.T) {
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.BlockChange(ctx, store.QuarantineRecord{ChangeName: "blocked-one", Reason: "rebase_conflict"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RequestRebuild(ctx, "rebuilding-one", "feat/spec-rebuilding-one", "conflict"); err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{store: st}
	results := map[string]string{"blocked-one": "ready", "rebuilding-one": "ready", "fine-one": "ready", "held-one": "held"}
	d.reconcileResults(ctx, results)
	want := map[string]string{"blocked-one": "blocked:rebase_conflict", "rebuilding-one": "rebuild_requested", "fine-one": "ready", "held-one": "held"}
	for k, v := range want {
		if results[k] != v {
			t.Errorf("%s = %q, want %q", k, results[k], v)
		}
	}
}

func TestSummarizeResultsGroupsOutcomes(t *testing.T) {
	got := summarizeResults(map[string]string{"b": "ready", "a": "ready", "c": "blocked:spec_ambiguity"}, []string{"d"})
	want := "blocked:spec_ambiguity c; ready a, b; waiting d"
	if got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if got := summarizeResults(nil, nil); got != "nothing built" {
		t.Fatalf("empty summary = %q", got)
	}
}
