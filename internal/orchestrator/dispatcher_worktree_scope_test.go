package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/gitx"
)

// TestDispatcherNeverPrunesAWorktreeItCannotReach: before it created a worker's worktree the dispatcher
// ran "git worktree prune", which deletes the administrative directory of every linked worktree whose
// path cannot be reached at that moment, with the git directories of its child repositories and the
// commits only they hold: a person's own worktree on a volume that is not mounted, or every worker's
// after the umbrella was moved or mounted at another path.
func TestDispatcherNeverPrunesAWorktreeItCannotReach(t *testing.T) {
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

	// The volume is not mounted while the dispatcher builds a change.
	unmounted := filepath.Join(sb.tmp, "volume-unmounted")
	if err := os.Rename(volume, unmounted); err != nil {
		t.Fatal(err)
	}
	sb.buildCoreChangeWriting(t, "add-a", map[string]string{"a.txt": "a\n"})

	// The person's worktree is intact once its volume is back.
	if err := os.Rename(unmounted, volume); err != nil {
		t.Fatal(err)
	}
	if out, err := gitx.RunGit(ctx, core, "cat-file", "-t", work); err != nil || strings.TrimSpace(out) != "commit" {
		t.Fatalf("the dispatcher deleted the administrative directory of a worktree it could not reach: its core commit %s is lost (%q, %v)", work, strings.TrimSpace(out), err)
	}
}

// TestDispatcherReprovisionsAWorktreeWhoseDirectoryWasDeleted: without the prune, the registration git
// keeps for a worker's worktree whose directory is gone (someone removed .worktrees) must still be
// dropped, or "git worktree add" refuses the path as "missing but already registered".
func TestDispatcherReprovisionsAWorktreeWhoseDirectoryWasDeleted(t *testing.T) {
	sb := newMCFSandbox(t, "")

	// A worker's worktree of an earlier attempt, registered and then deleted with its whole directory.
	worktrees := filepath.Join(sb.umbrella, ".worktrees")
	mcfGit(t, sb.umbrella, "worktree", "add", "--quiet", "-b", "feat/spec-add-a", filepath.Join(worktrees, "worker-add-a"), "HEAD")
	if err := os.RemoveAll(worktrees); err != nil {
		t.Fatal(err)
	}
	if list := mcfGit(t, sb.umbrella, "worktree", "list", "--porcelain"); !strings.Contains(list, "worker-add-a") {
		t.Fatalf("the deleted worktree should still be registered:\n%s", list)
	}

	// buildCoreChangeWriting fails the test unless the change reaches the merge queue.
	sb.buildCoreChangeWriting(t, "add-a", map[string]string{"a.txt": "a\n"})
	if !dirExists(filepath.Join(worktrees, "worker-add-a", "openspec", "changes", "add-a")) {
		t.Fatalf("the change was not built in its re-created worktree")
	}
}
