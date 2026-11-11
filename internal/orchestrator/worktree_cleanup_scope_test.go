package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/gitx"
)

// TestMergerNeverPrunesAWorktreeItCannotReach: the merger used to run "git worktree prune" when it
// created its integration worktree and after every integration. That deletes the administrative
// directory of every linked worktree whose path cannot be reached at that moment, with the git
// directories of its child repositories and the commits only they hold: a person's own worktree on a
// volume that is not mounted, or every worker's after the umbrella was moved or mounted at another path.
// The merger now forgets only the worktree of the change it integrated.
func TestMergerNeverPrunesAWorktreeItCannotReach(t *testing.T) {
	sb := newMCFSandbox(t, "")
	ctx := context.Background()

	// A person's own worktree of the umbrella on another volume, with work in core that exists nowhere else.
	volume := filepath.Join(sb.tmp, "volume")
	own := filepath.Join(volume, "hotfix")
	mcfGit(t, sb.umbrella, "worktree", "add", "--quiet", "-b", "hotfix", own, "HEAD")
	mcfGit(t, own, "submodule", "--quiet", "update", "--init", "repos/core")
	core := filepath.Join(own, "repos", "core")
	mustWriteFile(t, filepath.Join(core, "hotfix.txt"), "work in progress\n")
	mcfGit(t, core, "add", "-A")
	mcfGit(t, core, "commit", "--quiet", "-m", "wip: hotfix")
	work := mcfGit(t, core, "rev-parse", "HEAD")

	sb.runCoreDispatch(t, "add-a", map[string]string{"a.txt": "a\n"})
	ready, err := sb.st.ListReadyQueue(ctx)
	if err != nil || len(ready) != 1 {
		t.Fatalf("the merge queue = %+v (err %v), want add-a alone", ready, err)
	}
	worker, branch := ready[0].Worktree, ready[0].Branch

	// The volume is not mounted while the merger integrates the change.
	unmounted := filepath.Join(sb.tmp, "volume-unmounted")
	if err := os.Rename(volume, unmounted); err != nil {
		t.Fatal(err)
	}
	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-a")

	// The integrated change's worktree, its registration and its branch are gone.
	if dirExists(worker) {
		t.Fatalf("the worker worktree %s still exists", worker)
	}
	if list := mcfGit(t, sb.umbrella, "worktree", "list", "--porcelain"); strings.Contains(list+"\n", "/"+filepath.Base(worker)+"\n") {
		t.Fatalf("the worker worktree is still registered:\n%s", list)
	}
	if _, err := gitx.RunGit(ctx, sb.umbrella, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		t.Fatalf("the worker branch %s was not deleted", branch)
	}

	// The person's worktree is intact once its volume is back.
	if err := os.Rename(unmounted, volume); err != nil {
		t.Fatal(err)
	}
	if out, err := gitx.RunGit(ctx, core, "cat-file", "-t", work); err != nil || strings.TrimSpace(out) != "commit" {
		t.Fatalf("the merger deleted the administrative directory of a worktree it could not reach: its core commit %s is lost (%q, %v)", work, strings.TrimSpace(out), err)
	}
}
