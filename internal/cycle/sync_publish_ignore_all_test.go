package cycle

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// `foreman-kit sync --publish` pushes a child's unpushed commits and then records its new gitlink in the
// umbrella. With "ignore = all" for the child in .gitmodules, git 2.54 and later skip "git add <child>"
// with exit status 0: nothing was staged, the commit failed, and the commits just pushed stayed
// unrecorded while sync reported "root pointer push failed".
func TestSyncPublishRecordsTheGitlinkOfAChildMarkedIgnoreAll(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	base := t.TempDir()
	childBare := filepath.Join(base, "app.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", childBare)
	seed := filepath.Join(base, "seed")
	gitIn(t, base, "clone", "-q", childBare, seed)
	gitIn(t, seed, "commit", "-q", "--allow-empty", "-m", "app: first")
	gitIn(t, seed, "push", "-q", "origin", "HEAD:main")

	rootBare := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", rootBare)
	root := filepath.Join(base, "root")
	gitIn(t, base, "clone", "-q", rootBare, root)
	gitIn(t, root, "submodule", "--quiet", "add", "-b", "main", childBare, "repos/app")
	gitIn(t, root, "config", "-f", ".gitmodules", "submodule.repos/app.ignore", "all")
	gitIn(t, root, "add", ".gitmodules")
	gitIn(t, root, "commit", "-q", "-m", "umbrella: add repos/app")
	gitIn(t, root, "push", "-q", "origin", "HEAD:main")
	gitIn(t, root, "branch", "-q", "--set-upstream-to=origin/main")

	child := filepath.Join(root, "repos", "app")
	gitIn(t, child, "commit", "-q", "--allow-empty", "-m", "app: second")
	second := strings.TrimSpace(gitIn(t, child, "rev-parse", "HEAD"))

	var out bytes.Buffer
	if err := SyncRepos(context.Background(), root, nil, true, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "repos/app: pushed 1 commit(s)") {
		t.Fatalf("the child's commit was not pushed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "root: recorded pointers for repos/app") {
		t.Fatalf("the pushed commit was not recorded in the umbrella:\n%s", out.String())
	}
	// "160000 commit <sha>\trepos/app" on origin's base.
	if entry := strings.Fields(gitIn(t, base, "--git-dir", rootBare, "ls-tree", "main", "repos/app")); len(entry) < 3 || entry[2] != second {
		t.Fatalf("origin records repos/app as %q, want the pushed commit %s", entry, second)
	}
}
