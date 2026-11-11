package orchestrator

import (
	"path/filepath"
	"testing"
)

// TestMergerRecordsTheGitlinkOfAChildMarkedIgnoreAll: with "ignore = all" for a child in .gitmodules (a
// setting that keeps its checkout out of git status), git 2.54 and later skip "git add <child>" with exit
// status 0, and git status and git diff never list the child. The merger then ran none of the child's
// gates, pushed the change's commits to the child's base branch and published an integration commit
// that still recorded the old gitlink: the umbrella never received the code it reported as integrated.
func TestMergerRecordsTheGitlinkOfAChildMarkedIgnoreAll(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.runCoreDispatch(t, "add-a", map[string]string{"a.txt": "a\n"})

	// Meanwhile the team marks core "ignore = all" on the base.
	mate := filepath.Join(sb.tmp, "ignore-all-mate")
	mcfGit(t, sb.tmp, "clone", "--quiet", "-b", "master", sb.umbrellaBare, mate)
	mcfGit(t, mate, "config", "-f", ".gitmodules", "submodule.repos/core.ignore", "all")
	mcfGit(t, mate, "commit", "--quiet", "-am", "chore: keep core out of git status")
	mcfGit(t, mate, "push", "--quiet", "origin", "master")

	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-a")
	mcfGit(t, sb.umbrella, "fetch", "--quiet", "origin", "master")
	recorded := mcfGit(t, sb.umbrella, "rev-parse", "origin/master:repos/core")
	pushed := mcfGit(t, sb.childBare["core"], "rev-parse", "refs/heads/main")
	if recorded != pushed {
		t.Fatalf("the merge of add-a pushed core's main to %s but the umbrella still records core at %s", pushed, recorded)
	}
	if line := mergerLine(t, sb.marker("core.ran")); line != "slot=99 files=a.txt" {
		t.Fatalf("core's gates at merge got %q, want the change's a.txt", line)
	}
}
