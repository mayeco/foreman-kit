package gitx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestIsWorktreeRootComparesDirectoriesNotSpellings: on a case-insensitive file system (macOS) the
// umbrella may be reached through a path spelled in another case than the directory has on disk (a "cd"
// or a crontab line typed by hand). git reports the worktree's top level with the on-disk spelling, so
// IsWorktreeRoot, which compared the two strings, answered false for every worktree under that umbrella:
// the merger refused its own integration worktree and the dispatcher every worktree it resumed.
func TestIsWorktreeRootComparesDirectoriesNotSpellings(t *testing.T) {
	ctx := context.Background()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	base := t.TempDir()
	umbrella := filepath.Join(base, "Umbrella")
	if err := os.MkdirAll(umbrella, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, umbrella, "init", "--quiet", "-b", "main")
	gitT(t, umbrella, "commit", "--quiet", "--allow-empty", "-m", "initial")

	// What it must keep refusing: a directory inside the worktree, and one whose gitfile is stale.
	inside := filepath.Join(umbrella, "docs")
	stale := filepath.Join(umbrella, "stale")
	for _, dir := range []string{inside, stale} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stale, ".git"), []byte("gitdir: /nonexistent/worktrees/stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if IsWorktreeRoot(ctx, inside) || IsWorktreeRoot(ctx, stale) {
		t.Fatal("a directory inside the worktree or with a stale gitfile was taken for a worktree root")
	}

	link := filepath.Join(base, "link")
	if err := os.Symlink(umbrella, link); err != nil {
		t.Fatal(err)
	}
	if !IsWorktreeRoot(ctx, umbrella) || !IsWorktreeRoot(ctx, link) {
		t.Fatal("the umbrella, or a symlink to it, is not recognised as a worktree root")
	}

	spelled := filepath.Join(base, "umbrella")
	if _, err := os.Stat(spelled); err != nil {
		t.Skip("case-sensitive file system: a path in another case is another directory")
	}
	if !IsWorktreeRoot(ctx, spelled) {
		t.Fatalf("%s is the umbrella %s spelled in another case, but is not recognised as a worktree root", spelled, umbrella)
	}
	repo, err := Open(spelled)
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(spelled, ".worktrees", "_integration")
	if err := repo.AddWorktree(ctx, wt, "", "HEAD", true); err != nil {
		t.Fatal(err)
	}
	if !IsWorktreeRoot(ctx, wt) {
		t.Fatalf("the worktree %s, created under the umbrella as it was spelled, is not recognised as a worktree root", wt)
	}
}
