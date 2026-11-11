package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/store"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestPrepareIntegrationWorktreeNeverTouchesTheMainCheckout leaves .worktrees/_integration behind without
// a .git (as an interrupted `worktree add` or a partial removal does). git would then resolve the main
// checkout, and the forced checkout and reset --hard would discard the operator's uncommitted work.
func TestPrepareIntegrationWorktreeNeverTouchesTheMainCheckout(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "--quiet", "-b", "master")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "a.txt")
	gitIn(t, root, "commit", "--quiet", "-m", "v1")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("uncommitted operator work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".worktrees", "_integration"), 0o755); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenAt(filepath.Join(t.TempDir(), "state.db"), "ws-integration-guard", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m, err := NewMerger(MergerOptions{Root: root, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := m.prepareIntegrationWorktree(ctx, "master"); err != nil {
		t.Fatalf("prepareIntegrationWorktree failed: %v", err)
	}

	if b, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(b) != "uncommitted operator work\n" {
		t.Fatalf("the main checkout's uncommitted work was discarded: a.txt = %q", b)
	}
	if head := gitIn(t, root, "rev-parse", "--abbrev-ref", "HEAD"); head != "master" {
		t.Fatalf("the main checkout's HEAD moved to %q", head)
	}
	if !gitx.IsWorktreeRoot(ctx, m.integrationWorktree()) {
		t.Fatal("expected _integration to be recreated as its own worktree")
	}
}
