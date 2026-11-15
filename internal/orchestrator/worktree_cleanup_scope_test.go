package orchestrator

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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

// TestBaseAcceptNeverPrunesAWorktreeItCannotReach: base accept checked a child's base in worktrees of its
// own and then ran "git worktree prune" in the umbrella and in the child, deleting the administrative
// directory of every linked worktree it could not reach, with the git directories of its child
// repositories and the commits only they hold. It now forgets only the two worktrees it created.
func TestBaseAcceptNeverPrunesAWorktreeItCannotReach(t *testing.T) {
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
	registrations := func() []string {
		entries, _ := os.ReadDir(filepath.Join(sb.umbrella, ".git", "worktrees"))
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}
	before := registrations()
	if len(before) != 1 {
		t.Fatalf("the person's worktree registrations = %v, want one", before)
	}

	// The volume is not mounted while base accept checks the base on its own.
	unmounted := filepath.Join(sb.tmp, "volume-unmounted")
	if err := os.Rename(volume, unmounted); err != nil {
		t.Fatal(err)
	}
	want := mcfGit(t, filepath.Join(sb.umbrella, "repos", "core"), "rev-parse", "HEAD")
	sha, err := VerifyBaseForAccept(ctx, sb.umbrella, sb.st, "repos/core", "", io.Discard)
	if err != nil || sha != want {
		t.Fatalf("VerifyBaseForAccept = %q, %v; want %q", sha, err, want)
	}
	if err := os.Rename(unmounted, volume); err != nil {
		t.Fatal(err)
	}

	// The person's worktree is intact once its volume is back.
	if out, err := gitx.RunGit(ctx, core, "cat-file", "-t", work); err != nil || strings.TrimSpace(out) != "commit" {
		t.Fatalf("base accept deleted the administrative directory of a worktree it could not reach: its core commit %s is lost (%q, %v)", work, strings.TrimSpace(out), err)
	}
	if out, err := gitx.RunGit(ctx, own, "status", "--porcelain"); err != nil {
		t.Fatalf("git status fails in the person's worktree after base accept: %v\n%s", err, out)
	}
	if after := registrations(); len(after) != 1 || after[0] != before[0] {
		t.Fatalf("the umbrella's worktree registrations = %v, want the person's %v alone", after, before)
	}

	// Base accept's own worktrees are forgotten in the umbrella and in the child, and nothing is left of them.
	for _, dir := range []string{sb.umbrella, filepath.Join(sb.umbrella, "repos", "core")} {
		if list := mcfGit(t, dir, "worktree", "list", "--porcelain"); strings.Contains(list, "_base_accept-") {
			t.Errorf("a worktree of the check is still registered in %s:\n%s", dir, list)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(sb.umbrella, ".worktrees")); len(entries) != 0 {
		t.Errorf("something of the check is left under .worktrees: %v", entries)
	}
}

// TestNoSourceRunsWorktreePrune: "git worktree prune" deletes the administrative directory of every
// linked worktree whose path cannot be reached at that moment, including a person's on a volume that is
// not mounted and every worker's after the umbrella was moved. The kit forgets only the worktrees it
// created, by path; no non-test source of the module may run prune. Comments that explain the rule stay
// legal: the scan reads the syntax tree, not the text.
func TestNoSourceRunsWorktreePrune(t *testing.T) {
	root := moduleRoot(t)
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		at := func(n ast.Node) string {
			return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), fset.Position(n.Pos()).Line)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if v, ok := stringLiteral(n); ok && strings.Contains(v, "worktree prune") {
					found = append(found, at(n)+": "+n.Value)
				}
			case *ast.CallExpr:
				for i := 0; i+1 < len(n.Args); i++ {
					first, ok1 := stringLiteral(n.Args[i])
					second, ok2 := stringLiteral(n.Args[i+1])
					if ok1 && ok2 && first == "worktree" && second == "prune" {
						found = append(found, at(n)+": git worktree prune")
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Fatalf("a source runs \"git worktree prune\", which deletes the worktrees the kit cannot reach; forget only the worktree created, by path (gitx.Repo.RemoveWorktree):\n%s", strings.Join(found, "\n"))
	}
}

// stringLiteral returns the value of e when it is a string literal.
func stringLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// moduleRoot is the directory of the go.mod above the package being tested.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}
