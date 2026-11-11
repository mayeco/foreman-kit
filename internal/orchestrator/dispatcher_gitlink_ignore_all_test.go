package orchestrator

import (
	"context"
	"path/filepath"
	"testing"
)

// TestDispatcherRecordsTheGitlinkOfAChildMarkedIgnoreAll: with "ignore = all" for a child in .gitmodules,
// git 2.54 and later skip "git add <child>" with exit status 0 and hide the child from git diff, staged
// or committed. The worker committed its change without the gitlink of the child it had built in: the
// branch it queued recorded none of the agent's commits, which existed only in the worker's worktree.
func TestDispatcherRecordsTheGitlinkOfAChildMarkedIgnoreAll(t *testing.T) {
	sb := newMCFSandbox(t, "")
	ctx := context.Background()

	// The team keeps core out of git status, before the change is built.
	mcfGit(t, sb.umbrella, "config", "-f", ".gitmodules", "submodule.repos/core.ignore", "all")
	mcfGit(t, sb.umbrella, "commit", "--quiet", "-am", "chore: keep core out of git status")
	mcfGit(t, sb.umbrella, "push", "--quiet", "origin", "master")

	sb.buildCoreChangeWriting(t, "add-a", map[string]string{"a.txt": "a\n"})
	ready, err := sb.st.ListReadyQueue(ctx)
	if err != nil || len(ready) != 1 {
		t.Fatalf("the merge queue = %+v (err %v), want add-a alone", ready, err)
	}
	built := mcfGit(t, filepath.Join(ready[0].Worktree, "repos", "core"), "rev-parse", "HEAD")
	recorded := mcfGit(t, sb.umbrella, "rev-parse", ready[0].HeadSHA+":repos/core")
	if recorded != built {
		t.Fatalf("the worker built core at %s but the branch it queued records core at %s", built, recorded)
	}
	fork := mcfGit(t, sb.umbrella, "merge-base", ready[0].HeadSHA, "origin/master")
	if !childGitlinksMoved(ctx, ready[0].Worktree, fork, []string{"repos/core"}) {
		t.Fatalf("the branch records a new commit of core, but the worker does not see its gitlink as moved")
	}

	// The merger integrates the change with its code.
	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-a")
	mcfGit(t, sb.umbrella, "fetch", "--quiet", "origin", "master")
	onBase := mcfGit(t, sb.umbrella, "rev-parse", "origin/master:repos/core")
	pushed := mcfGit(t, sb.childBare["core"], "rev-parse", "refs/heads/main")
	if onBase != pushed {
		t.Fatalf("core's main is at %s but the umbrella's base records core at %s", pushed, onBase)
	}
	if got := mcfGit(t, sb.childBare["core"], "show", "refs/heads/main:a.txt"); got != "a" {
		t.Fatalf("the change's a.txt did not reach core's base: %q", got)
	}
}
