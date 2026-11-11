package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wtRegUmbrella returns an umbrella with one child repository (repos/app) and a linked worktree
// .worktrees/a whose child holds a commit that exists nowhere else, as a worker's does before its
// change is integrated. It returns the umbrella, the worktree and that commit.
func wtRegUmbrella(t *testing.T) (umbrella, wtA, childWork string) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	base := t.TempDir()
	childSrc := filepath.Join(base, "app-origin")
	if err := os.MkdirAll(childSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, childSrc, "init", "--quiet", "-b", "main")
	gitT(t, childSrc, "commit", "--quiet", "--allow-empty", "-m", "app: initial")

	umbrella = filepath.Join(base, "umbrella")
	if err := os.MkdirAll(umbrella, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, umbrella, "init", "--quiet", "-b", "main")
	gitT(t, umbrella, "submodule", "--quiet", "add", childSrc, "repos/app")
	gitT(t, umbrella, "commit", "--quiet", "-m", "umbrella: add repos/app")

	repo, err := Open(umbrella)
	if err != nil {
		t.Fatal(err)
	}
	wtA = filepath.Join(umbrella, ".worktrees", "a")
	if err := repo.AddWorktree(context.Background(), wtA, "feat/a", "HEAD", false); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtA, "submodule", "--quiet", "update", "--init")
	child := filepath.Join(wtA, "repos", "app")
	gitT(t, child, "commit", "--quiet", "--allow-empty", "-m", "app: work not pushed yet")
	return umbrella, wtA, gitT(t, child, "rev-parse", "HEAD")
}

func wtRegListed(t *testing.T, umbrella, wt string) bool {
	t.Helper()
	want, err := filepath.EvalSymlinks(filepath.Dir(wt))
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(want, filepath.Base(wt))
	for _, line := range strings.Split(gitT(t, umbrella, "worktree", "list", "--porcelain"), "\n") {
		if line == "worktree "+want || line == "worktree "+wt {
			return true
		}
	}
	return false
}

// TestAddWorktreeAfterTheUmbrellaMovedKeepsTheOtherWorktrees: after the umbrella is moved (or mounted at
// another path, as in a container), every linked worktree's recorded path is gone until "git worktree
// repair" runs. AddWorktree used to start with "git worktree prune", which deleted their administrative
// directories: the index, HEAD and the git directories of their child repositories, with every commit
// only those held.
func TestAddWorktreeAfterTheUmbrellaMovedKeepsTheOtherWorktrees(t *testing.T) {
	ctx := context.Background()
	umbrella, _, childWork := wtRegUmbrella(t)

	moved := filepath.Join(filepath.Dir(umbrella), "umbrella-moved")
	if err := os.Rename(umbrella, moved); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(moved)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AddWorktree(ctx, filepath.Join(moved, ".worktrees", "b"), "", "HEAD", true); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(moved, ".git", "worktrees", "a")); err != nil {
		t.Fatalf("adding worktree b deleted the administrative directory of the moved worktree a: %v", err)
	}
	wtA := filepath.Join(moved, ".worktrees", "a")
	if out, err := RunGit(ctx, moved, "worktree", "repair", wtA); err != nil {
		t.Fatalf("git worktree repair: %v\n%s", err, out)
	}
	if !IsWorktreeRoot(ctx, wtA) {
		t.Fatal("worktree a is not usable after git worktree repair")
	}
	if out, err := RunGit(ctx, filepath.Join(wtA, "repos", "app"), "cat-file", "-t", childWork); err != nil || strings.TrimSpace(out) != "commit" {
		t.Fatalf("the child commit of worktree a is lost: %q, %v", out, err)
	}
}

// TestAddWorktreeReusesAPathWhoseDirectoryWasDeleted: without the prune, a path that is still registered
// but whose directory is gone must still be usable.
func TestAddWorktreeReusesAPathWhoseDirectoryWasDeleted(t *testing.T) {
	ctx := context.Background()
	umbrella, wtA, _ := wtRegUmbrella(t)
	repo, err := Open(umbrella)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wtA); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddWorktree(ctx, wtA, "feat/a", "HEAD", false); err != nil {
		t.Fatalf("re-adding the branch worktree: %v", err)
	}
	if !IsWorktreeRoot(ctx, wtA) || gitT(t, wtA, "branch", "--show-current") != "feat/a" {
		t.Fatal("worktree a was not created again on feat/a")
	}
	if err := os.RemoveAll(wtA); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddWorktree(ctx, wtA, "", "HEAD", true); err != nil {
		t.Fatalf("re-adding the detached worktree: %v", err)
	}
	if !IsWorktreeRoot(ctx, wtA) {
		t.Fatal("worktree a was not created again detached")
	}
}

// TestRemoveWorktreeForgetsOnlyTheWorktreeItRemoves: RemoveWorktree drops the registration of its own
// worktree, even one whose .git git cannot read, and leaves alone a worktree that cannot be reached right
// now (an unmounted volume), which "git worktree prune" deleted with the child commits it held.
func TestRemoveWorktreeForgetsOnlyTheWorktreeItRemoves(t *testing.T) {
	ctx := context.Background()
	umbrella, wtA, childWork := wtRegUmbrella(t)
	repo, err := Open(umbrella)
	if err != nil {
		t.Fatal(err)
	}
	wtB := filepath.Join(umbrella, ".worktrees", "b")
	if err := repo.AddWorktree(ctx, wtB, "feat/b", "HEAD", false); err != nil {
		t.Fatal(err)
	}
	// Worktree a becomes unreachable, and b's gitfile unreadable.
	away := filepath.Join(filepath.Dir(umbrella), "unmounted-a")
	if err := os.Rename(wtA, away); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtB, ".git"), []byte("gitdir: /nonexistent/worktrees/b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := repo.RemoveWorktree(ctx, wtB); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wtB); err == nil {
		t.Fatal("worktree b still exists")
	}
	if wtRegListed(t, umbrella, wtB) {
		t.Fatal("worktree b is still registered")
	}
	gitT(t, umbrella, "branch", "-D", "feat/b") // fails while a registered worktree holds the branch

	if _, err := os.Stat(filepath.Join(umbrella, ".git", "worktrees", "a")); err != nil {
		t.Fatalf("removing worktree b deleted the administrative directory of the unreachable worktree a: %v", err)
	}
	if err := os.Rename(away, wtA); err != nil {
		t.Fatal(err)
	}
	if out, err := RunGit(ctx, filepath.Join(wtA, "repos", "app"), "cat-file", "-t", childWork); err != nil || strings.TrimSpace(out) != "commit" {
		t.Fatalf("the child commit of worktree a is lost: %q, %v", out, err)
	}
}
