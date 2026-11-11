package orchestrator

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A broken base the merger saw move and that was force-pushed back (or that the attempt's fetch did not
// bring) holds its changes again until the next recheck: they are not attempted on every pass.
func TestMergeVerifyBaseForcePushedBackIsNotRetriedOnEveryPass(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.buildCoreChange(t, "core-feature")
	broken := sb.pushCoreCommit(t, "config/locales/BROKEN", "broken\n", "fix(core): a hotfix that breaks the regression")
	ctx := context.Background()

	m := sb.newMerger(t, MergerOptions{BaseRecheckInterval: time.Hour, Out: &bytes.Buffer{}})
	if n := m.drainQueue(ctx); n != 1 {
		t.Fatalf("expected the first drain to attempt the change, got %d", n)
	}
	sb.requireHeld(t, "core-feature")

	// The base moves and the merger sees it move...
	sb.pushCoreCommit(t, "notes.txt", "notes\n", "docs(core): notes")
	ready, err := sb.st.ListReadyQueue(ctx)
	if err != nil || len(ready) != 1 {
		t.Fatalf("the merge queue: %+v (err=%v)", ready, err)
	}
	m.opts.BaseRecheckInterval = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	if m.heldByBrokenBase(ctx, ready[0]) {
		t.Fatal("expected the moved base to release the change")
	}
	m.opts.BaseRecheckInterval = time.Hour

	// ...then it is force-pushed back to the broken commit before the attempt fetches it.
	mcfGit(t, filepath.Join(sb.tmp, "side-core"), "push", "--quiet", "--force", "origin", broken+":refs/heads/main")
	if n := m.drainQueue(ctx); n != 1 {
		t.Fatalf("expected one attempt, which finds the broken base again, got %d", n)
	}
	sb.requireHeld(t, "core-feature")
	for i := 0; i < 2; i++ {
		if n := m.drainQueue(ctx); n != 0 {
			t.Fatalf("drain %d: the held change was attempted again before the recheck interval (%d)", i, n)
		}
	}
}

// Where a child repository is not checked out, git ls-remote would run in the umbrella and read the
// umbrella remote's branch of the same name: remoteChildBase reads nothing instead.
func TestRemoteChildBaseNeverReadsTheUmbrellaRemote(t *testing.T) {
	sb := newMCFSandbox(t, "")
	ctx := context.Background()
	// The umbrella's remote gets a branch named like core's base branch.
	mcfGit(t, sb.umbrella, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	mcfGit(t, sb.umbrella, "submodule", "deinit", "--quiet", "-f", "repos/core")
	if _, err := os.Stat(filepath.Join(sb.umbrella, "repos", "core")); err != nil {
		t.Fatal(err)
	}

	m := sb.newMerger(t, MergerOptions{Out: &bytes.Buffer{}})
	if got := m.remoteChildBase(ctx, "repos/core"); got != "" {
		t.Fatalf("remoteChildBase read %s from the umbrella's remote for an uninitialized child", got)
	}
}
