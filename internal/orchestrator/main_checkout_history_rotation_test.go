package orchestrator

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMainCheckoutFastForwardsPastAHistoryRotation: the dispatcher fast-forwards the main checkout before
// it reads the backlog from it, restoring first the files the merger wrote there after publishing them.
// The file a rotation of the history creates was not among them: untracked in the main checkout, it
// counted as a local edit, and from the first rotation on the dispatcher read a backlog that no longer
// followed origin.
func TestMainCheckoutFastForwardsPastAHistoryRotation(t *testing.T) {
	sb := newMCFSandbox(t, "")
	ctx := context.Background()

	// An integration rotates the history: on the base...
	rotated, current := `[{"change_name":"a"}]`+"\n", `[{"change_name":"b"}]`+"\n"
	mate := filepath.Join(sb.tmp, "rotation-mate")
	mcfGit(t, sb.tmp, "clone", "--quiet", "-b", "master", sb.umbrellaBare, mate)
	mustWriteFile(t, filepath.Join(mate, ".foreman-kit", "agent_history.1.json"), rotated)
	mustWriteFile(t, filepath.Join(mate, ".foreman-kit", "agent_history.json"), current)
	mcfGit(t, mate, "add", "-A")
	mcfGit(t, mate, "commit", "--quiet", "-m", "chore(merge): integrate b")
	mcfGit(t, mate, "push", "--quiet", "origin", "master")
	// ...and, written by the merger, in the main checkout.
	mustWriteFile(t, filepath.Join(sb.umbrella, ".foreman-kit", "agent_history.1.json"), rotated)
	mustWriteFile(t, filepath.Join(sb.umbrella, ".foreman-kit", "agent_history.json"), current)

	disp, err := NewDispatcher(DispatcherOptions{
		Root:       sb.umbrella,
		Store:      sb.st,
		MaxWorkers: 1,
		AgentRunner: func(context.Context, AgentRunRequest) (AgentRunResult, error) {
			return AgentRunResult{ExitCode: 0, UsedEngine: "stub"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = disp.Close() }()
	disp.fetchOriginQuiet(ctx)
	disp.fastForwardMainCheckout(ctx)

	head := mcfGit(t, sb.umbrella, "rev-parse", "HEAD")
	if want := mcfGit(t, sb.umbrella, "rev-parse", "origin/master"); head != want {
		t.Fatalf("the main checkout stayed at %s, behind origin/master %s", head, want)
	}
	if st := mcfGit(t, sb.umbrella, "status", "--porcelain", "--ignore-submodules=all"); st != "" {
		t.Fatalf("the main checkout is not clean after the fast-forward:\n%s", st)
	}
}
