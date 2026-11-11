package cycle

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// RunCycle's finishExec must not erase visual_qa_last_sha recorded by VisualQAEnqueueIntegrated.
func TestAuditRunCycleKeepsVisualQALastSHA(t *testing.T) {
	root := t.TempDir()
	st, err := store.OpenAt(filepath.Join(root, "log", "openspec.db"), "ws-cycle-state-audit", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	setVisualQALastSHA(ctx, st, "abc123")
	if got := getVisualQALastSHA(ctx, st); got != "abc123" {
		t.Fatalf("precondition: got %q", got)
	}
	if err := RunCycle(ctx, root, st, CycleOptions{SkipWorktreePrep: true, Stamp: "20261108T000000Z"}, io.Discard); err != nil {
		t.Logf("RunCycle returned %v (irrelevant for this check)", err)
	}
	ws, _ := st.GetWorkspaceState(ctx)
	if got := getVisualQALastSHA(ctx, st); got != "abc123" {
		t.Fatalf("visual_qa_last_sha lost after a cycle: got %q, cycle_state_json=%s", got, ws.CycleStateJSON)
	}
	t.Logf("cycle_state_json=%s", ws.CycleStateJSON)
}
