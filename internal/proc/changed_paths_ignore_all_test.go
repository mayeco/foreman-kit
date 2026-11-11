package proc

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

// The gates decide which child repositories a change touched from the paths that differ from the base.
// With "ignore = all" for a child in .gitmodules (a setting teams use to keep a dirty submodule out of
// git status), git diff reports no change of its gitlink, committed or not: `foreman-kit gates` then
// selected no repository for a change that had moved the child, and ran none of its checks.
func TestChangedPathsInUmbrellaListsAChildMarkedIgnoreAll(t *testing.T) {
	isolateGitConfig(t)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	base := t.TempDir()
	childSrc := filepath.Join(base, "app-origin")
	mkdirIn(t, base, "app-origin")
	gitIn(t, childSrc, "init", "--quiet", "-b", "main")
	gitIn(t, childSrc, "commit", "--quiet", "--allow-empty", "-m", "app: first")

	umbrella := filepath.Join(base, "umbrella")
	mkdirIn(t, base, "umbrella")
	gitIn(t, umbrella, "init", "--quiet", "-b", "main")
	gitIn(t, umbrella, "submodule", "--quiet", "add", childSrc, "repos/app")
	gitIn(t, umbrella, "config", "-f", ".gitmodules", "submodule.repos/app.ignore", "all")
	gitIn(t, umbrella, "add", ".gitmodules")
	gitIn(t, umbrella, "commit", "--quiet", "-m", "umbrella: add repos/app")
	before := gitIn(t, umbrella, "rev-parse", "HEAD")

	// The change commits in the child and records the new gitlink in the umbrella.
	child := filepath.Join(umbrella, "repos", "app")
	gitIn(t, child, "commit", "--quiet", "--allow-empty", "-m", "app: second")
	gitIn(t, umbrella, "add", "--force", "--", "repos/app")
	gitIn(t, umbrella, "commit", "--quiet", "-m", "umbrella: record app's second commit")

	got := changedPathsInUmbrella(context.Background(), umbrella, before)
	if !slices.Contains(got, "repos/app") {
		t.Fatalf("changedPathsInUmbrella since %s = %q, want repos/app: its gitlink was committed since", before, got)
	}
}
