package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/store"
)

// removeWorkerIndexLocks deletes the index.lock files a killed git left in the git directory of the
// worker worktree wt (which holds its submodules' git directories) and in the git directories of its
// children. A git killed mid-write (the SIGKILL that follows an ignored SIGTERM, a crash, a power loss)
// leaves one, and every git add of the resumed change then fails on it: an orchestrator error that
// pauses the workspace.
//
// The caller holds the change's worker lease and reapOrphanAgents has stopped the agents a dead
// dispatcher left, so no git of the change is running and a lock needs no age to be a leftover. Only
// this worker's git directories are walked: another worker's, and the main checkout's, are never touched.
func (d *Dispatcher) removeWorkerIndexLocks(ctx context.Context, wt, change string, children []string) {
	removed := removeIndexLocks(ctx, wt, 0)
	walked := ""
	if out, err := gitx.RunGit(ctx, wt, "rev-parse", "--absolute-git-dir"); err == nil {
		walked = realPath(strings.TrimSpace(out))
	}
	for _, cp := range children {
		childDir := filepath.Join(wt, filepath.FromSlash(cp))
		if _, err := os.Stat(filepath.Join(childDir, ".git")); err != nil {
			continue
		}
		out, err := gitx.RunGit(ctx, childDir, "rev-parse", "--absolute-git-dir")
		if err != nil {
			continue
		}
		gitDir := realPath(strings.TrimSpace(out))
		if walked != "" && within(walked, gitDir) {
			continue // removeIndexLocks(wt) already walked it
		}
		if d.sharedWithMainCheckout(ctx, cp, gitDir) {
			continue // a person's git may hold that one
		}
		lock := filepath.Join(gitDir, "index.lock")
		if os.Remove(lock) == nil {
			removed = append(removed, lock)
		}
	}
	if len(removed) == 0 {
		return
	}
	for _, p := range removed {
		d.logf("dispatcher: %s: removed %s, left by a git that was killed", change, p)
	}
	_ = d.store.EmitEvent(ctx, store.EventInput{
		Event:  "change.index_lock_removed",
		Change: change,
		Payload: map[string]any{
			"paths": strings.Join(removed, ","),
		},
	})
}

// sharedWithMainCheckout reports whether gitDir is the git directory of the main checkout's own copy
// of the child cp.
func (d *Dispatcher) sharedWithMainCheckout(ctx context.Context, cp, gitDir string) bool {
	if d.mainRoot == "" {
		return false
	}
	mainChild := filepath.Join(d.mainRoot, filepath.FromSlash(cp))
	if _, err := os.Stat(filepath.Join(mainChild, ".git")); err != nil {
		return false
	}
	out, err := gitx.RunGit(ctx, mainChild, "rev-parse", "--absolute-git-dir")
	return err == nil && realPath(strings.TrimSpace(out)) == gitDir
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// within reports whether p is dir or sits below it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
