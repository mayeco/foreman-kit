package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// isolateGitConfig makes git run with its defaults (core.quotePath on, diff.renames on), whatever the
// machine's own configuration says.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "empty.gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeIn(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirIn(t *testing.T, dir, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
}

const renamedBody = "a body long enough for git to detect the rename by its content\n"

// The files a child repository's change touched (FOREMANKIT_CHANGED_FILES, the paths regression.when
// matches) are its paths as they are: a non-ASCII name unquoted, and both sides of a rename, so a file
// moved out of a regression.when path still triggers the regression.
func TestChangedFilesInChildRepoUnquotesPathsAndKeepsRenameSources(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	child := filepath.Join(root, "repos", "core")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, child, "init", "-q", "-b", "main")
	writeIn(t, child, "config/locales/es.yml", renamedBody)
	writeIn(t, child, "README.md", "# core\n")
	gitIn(t, child, "add", "-A")
	gitIn(t, child, "commit", "-q", "-m", "initial")
	base := gitIn(t, child, "rev-parse", "HEAD")

	gitIn(t, root, "init", "-q", "-b", "master")
	gitIn(t, root, "update-index", "--add", "--cacheinfo", "160000,"+base+",repos/core")
	gitIn(t, root, "commit", "-q", "-m", "umbrella")

	mkdirIn(t, child, "lib")
	gitIn(t, child, "mv", "config/locales/es.yml", "lib/es.yml")
	writeIn(t, child, "config/locales/año.yml", "es: {}\n")
	gitIn(t, child, "add", "-A")
	gitIn(t, child, "commit", "-q", "-m", "change")
	writeIn(t, child, "notas/café.txt", "untracked\n")

	got := changedFilesInChildRepo(context.Background(), root, child, "core", "HEAD")
	for _, want := range []string{"config/locales/es.yml", "lib/es.yml", "config/locales/año.yml", "notas/café.txt"} {
		if !slices.Contains(got, want) {
			t.Errorf("changed files %q lack %q", got, want)
		}
	}
	for _, p := range got {
		if strings.ContainsAny(p, "\"\\") {
			t.Errorf("changed files hold a C-quoted path: %q", p)
		}
	}
}

// The umbrella's changed paths, which decide whether the OpenSpec layer is validated and which
// repositories' gates run, keep non-ASCII names unquoted and both sides of a rename.
func TestChangedPathsInUmbrellaUnquotesPathsAndKeepsRenameSources(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "master")
	writeIn(t, root, "openspec/changes/old-name/proposal.md", renamedBody)
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "initial")
	base := gitIn(t, root, "rev-parse", "HEAD")

	mkdirIn(t, root, "docs")
	gitIn(t, root, "mv", "openspec/changes/old-name/proposal.md", "docs/proposal.md")
	writeIn(t, root, "docs/diseño.md", "# diseño\n")
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "change")

	got := changedPathsInUmbrella(context.Background(), root, base)
	for _, want := range []string{"openspec/changes/old-name/proposal.md", "docs/proposal.md", "docs/diseño.md"} {
		if !slices.Contains(got, want) {
			t.Errorf("changed paths %q lack %q", got, want)
		}
	}
}
