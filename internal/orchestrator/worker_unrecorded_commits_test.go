package orchestrator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/gitx"
)

// TestMergerRefusesAChangeWhoseWorkerHoldsUnrecordedChildCommits: the merger integrates the child commits
// the change's branch records and then deletes the worker's worktree. When the worker's checkout of a
// child holds commits the branch does not record (the gitlink was never committed: git 2.54 and later
// skip "git add <child>", exit status 0, for a child .gitmodules marks "ignore = all"), the change was
// reported as integrated without them and the only copy of those commits was deleted with the worktree.
func TestMergerRefusesAChangeWhoseWorkerHoldsUnrecordedChildCommits(t *testing.T) {
	sb := newMCFSandbox(t, "")
	ctx := context.Background()
	sb.runCoreDispatch(t, "add-a", map[string]string{"a.txt": "a\n"})
	ready, err := sb.st.ListReadyQueue(ctx)
	if err != nil || len(ready) != 1 {
		t.Fatalf("the merge queue = %+v (err %v), want add-a alone", ready, err)
	}
	worker := ready[0].Worktree
	core := filepath.Join(worker, "repos", "core")
	remoteBefore := mcfGit(t, sb.childBare["core"], "rev-parse", "refs/heads/main")

	// A commit of the worker's core checkout that the change's branch does not record.
	mustWriteFile(t, filepath.Join(core, "unrecorded.txt"), "work the umbrella branch does not point at\n")
	mcfGit(t, core, "add", "-A")
	mcfGit(t, core, "commit", "--quiet", "-m", "feat(core): more work")
	unrecorded := mcfGit(t, core, "rev-parse", "HEAD")

	sb.mergerRun(t)

	mcfGit(t, sb.umbrella, "fetch", "--quiet", "origin", "master")
	rootRepo, err := gitx.Open(sb.umbrella)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := rootRepo.HasCommitWithTrailer("origin/master", "add-a"); ok {
		t.Fatal("add-a was integrated although its worker holds a core commit the change does not record")
	}
	if got := mcfGit(t, sb.childBare["core"], "rev-parse", "refs/heads/main"); got != remoteBefore {
		t.Fatalf("core's main moved to %s although add-a was refused", got)
	}
	q, err := sb.st.GetQuarantine(ctx, "add-a")
	if err != nil || q == nil || q.Reason != "missing_child_commits" || !strings.Contains(q.Details, "repos/core") {
		t.Fatalf("quarantine of add-a = %+v (err %v), want missing_child_commits naming repos/core", q, err)
	}
	if !dirExists(worker) {
		t.Fatalf("the worker worktree %s was deleted", worker)
	}
	if out, err := gitx.RunGit(ctx, core, "cat-file", "-t", unrecorded); err != nil || strings.TrimSpace(out) != "commit" {
		t.Fatalf("the unrecorded core commit %s is lost (%q, %v)", unrecorded, strings.TrimSpace(out), err)
	}
}
