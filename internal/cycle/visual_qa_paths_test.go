package cycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// backendOnlyFixture builds an umbrella whose last commit moves repos/web's gitlink over one child
// commit made by change, and returns the umbrella and that commit.
func backendOnlyFixture(t *testing.T, before map[string]string, change func(child string)) (string, string) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "empty.gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// git's defaults (core.quotePath, diff.renames), whatever the machine's configuration says.
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	write := func(dir, rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	child := filepath.Join(root, "repos", "web")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, child, "init", "-q", "-b", "main")
	write(child, "repo.config.yaml", "kind: web\n")
	for rel, body := range before {
		write(child, rel, body)
	}
	gitRun(t, child, "add", "-A")
	gitRun(t, child, "commit", "-q", "-m", "initial")
	first := gitRun(t, child, "rev-parse", "HEAD")

	gitRun(t, root, "init", "-q", "-b", "master")
	gitRun(t, root, "update-index", "--add", "--cacheinfo", "160000,"+first+",repos/web")
	gitRun(t, root, "commit", "-q", "-m", "umbrella")

	change(child)
	gitRun(t, child, "add", "-A")
	gitRun(t, child, "commit", "-q", "-m", "change")
	second := gitRun(t, child, "rev-parse", "HEAD")
	gitRun(t, root, "update-index", "--cacheinfo", "160000,"+second+",repos/web")
	gitRun(t, root, "commit", "-q", "-m", "integrate")
	return root, gitRun(t, root, "rev-parse", "HEAD")
}

// A view moved into lib/ removes a view: the commit is not backend-only, so visual QA still checks it.
// Plain `git diff --name-only` reports only the destination of a rename.
func TestVisualQABackendOnlySeesTheSourceOfARename(t *testing.T) {
	body := "<p>a view long enough for git to detect the rename by its content</p>\n"
	root, sha := backendOnlyFixture(t, map[string]string{"app/views/reports/show.html.erb": body}, func(child string) {
		if err := os.MkdirAll(filepath.Join(child, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		gitRun(t, child, "mv", "app/views/reports/show.html.erb", "lib/show_template.rb")
	})
	if isChildCommitBackendOnly(context.Background(), root, sha, "repos/web") {
		t.Fatal("a commit that moves a view out of app/views was judged backend-only")
	}
}

// A migration with a non-ASCII name is backend-only. Plain output C-quotes it ("db/migrate/...\303\261..."),
// which then matches no backend prefix.
func TestVisualQABackendOnlyReadsNonASCIIPaths(t *testing.T) {
	root, sha := backendOnlyFixture(t, map[string]string{"db/schema.rb": "schema\n"}, func(child string) {
		p := filepath.Join(child, "db", "migrate", "20260101_añadir_campo.rb")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("class AñadirCampo; end\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if !isChildCommitBackendOnly(context.Background(), root, sha, "repos/web") {
		t.Fatal("a commit that only adds a migration with a non-ASCII name was not judged backend-only")
	}
}

// An umbrella commit whose only change is under a CLI repository with a non-ASCII path is terminal-only.
func TestVisualQATerminalOnlyReadsNonASCIIPaths(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty.gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "master")
	if err := os.MkdirAll(filepath.Join(root, "repos", "herramienta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "repos", "herramienta", "repo.config.yaml"), []byte("kind: cli\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-q", "-m", "initial")
	if err := os.WriteFile(filepath.Join(root, "repos", "herramienta", "guía.md"), []byte("# guía\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-q", "-m", "docs")
	if !isCommitTerminalOnly(context.Background(), root, gitRun(t, root, "rev-parse", "HEAD")) {
		t.Fatal("a commit that only touches a CLI repository was not judged terminal-only")
	}
}
