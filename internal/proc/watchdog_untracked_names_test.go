package proc

import (
	"context"
	"path/filepath"
	"testing"
)

// The names an agent may give a file that git prints C-quoted (core.quotePath): non-ASCII, a double
// quote, a tab and a backslash, next to a plain one.
var untrackedNamesGitQuotes = []string{
	"notes.md",
	"résumé.md",
	`dir with "quote".txt`,
	"with\ttab.txt",
	`back\slash.txt`,
	"notas/diseño.md",
	" padded name .txt",
}

// activityRepos returns an umbrella with a committed child repository at repos/core (declared in
// .gitmodules, its gitlink in the index) and the child's directory.
func activityRepos(t *testing.T) (root, child string) {
	t.Helper()
	isolateGitConfig(t)
	root = t.TempDir()
	child = filepath.Join(root, "repos", "core")
	mkdirIn(t, root, "repos/core")
	gitIn(t, child, "init", "-q", "-b", "main")
	writeIn(t, child, "README.md", "# core\n")
	gitIn(t, child, "add", "-A")
	gitIn(t, child, "commit", "-q", "-m", "initial")
	head := gitIn(t, child, "rev-parse", "HEAD")

	gitIn(t, root, "init", "-q", "-b", "master")
	writeIn(t, root, ".gitmodules", "[submodule \"core\"]\n\tpath = repos/core\n\turl = ./core.git\n")
	writeIn(t, root, "README.md", "# umbrella\n")
	gitIn(t, root, "add", ".gitmodules", "README.md")
	gitIn(t, root, "update-index", "--add", "--cacheinfo", "160000,"+head+",repos/core")
	gitIn(t, root, "commit", "-q", "-m", "umbrella")
	return root, child
}

func assertRewriteChangesActivityState(t *testing.T, root, repoDir, name string) {
	t.Helper()
	ctx := context.Background()
	writeIn(t, repoDir, name, "first version\n")
	before := ActivityState(ctx, root, "", 0)
	if again := ActivityState(ctx, root, "", 0); again != before {
		t.Fatalf("%q: ActivityState changed with no change to any file", name)
	}
	writeIn(t, repoDir, name, "other version\n") // same size, other content
	if after := ActivityState(ctx, root, "", 0); after == before {
		t.Errorf("%q: ActivityState did not change after the untracked file was rewritten", name)
	}
}

// An agent rewriting an untracked file of the umbrella is active whatever git prints for its name.
func TestActivityStateSeesRewrittenUntrackedFileInUmbrella(t *testing.T) {
	for _, name := range untrackedNamesGitQuotes {
		t.Run(name, func(t *testing.T) {
			root, _ := activityRepos(t)
			assertRewriteChangesActivityState(t, root, root, name)
		})
	}
}

// The same in a child repository of the umbrella.
func TestActivityStateSeesRewrittenUntrackedFileInChildRepo(t *testing.T) {
	for _, name := range untrackedNamesGitQuotes {
		t.Run(name, func(t *testing.T) {
			root, child := activityRepos(t)
			assertRewriteChangesActivityState(t, root, child, name)
		})
	}
}
