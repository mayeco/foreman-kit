package gitx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// pushSourceIsolateGit keeps the developer's global and system git configuration out of the test.
func pushSourceIsolateGit(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// TestPushReachesTheRemoteWhenGoGitCannotReadTheSourceCommit reproduces the merger's umbrella push: its
// Repo was opened, and read packed objects, before "git commit" created the commit to push, and an auto gc
// then moved that commit into a pack this Repo has never seen. go-git drops a source commit it cannot read
// without an error and answers "already up to date", which Push reported as a successful push although
// the remote never received the commit.
func TestPushReachesTheRemoteWhenGoGitCannotReadTheSourceCommit(t *testing.T) {
	pushSourceIsolateGit(t)
	url, bare := newHTTPRemote(t)
	local := t.TempDir()
	ctx := context.Background()

	gitT(t, local, "init", "--quiet", "-b", "main")
	gitT(t, local, "remote", "add", "origin", url)
	gitT(t, local, "commit", "--quiet", "--allow-empty", "-m", "c1")
	c1 := gitT(t, local, "rev-parse", "HEAD")
	gitT(t, local, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	gitT(t, local, "repack", "-a", "-d", "-q")

	repo, err := Open(local)
	if err != nil {
		t.Fatal(err)
	}
	// Reading a packed commit makes go-git load its pack index, once for the life of the Repo.
	if got, err := repo.RevParse("HEAD"); err != nil || got != c1 {
		t.Fatalf("RevParse(HEAD) = %q, %v, want %s", got, err, c1)
	}

	// What "git commit" followed by its auto gc leaves: the new commit only in a new pack.
	gitT(t, local, "commit", "--quiet", "--allow-empty", "-m", "c2")
	c2 := gitT(t, local, "rev-parse", "HEAD")
	gitT(t, local, "repack", "-a", "-d", "-q")

	if err := repo.Push(ctx, "origin", "HEAD:refs/heads/main"); err != nil {
		t.Fatalf("push failed: %v", err)
	}
	if got := gitT(t, bare, "rev-parse", "refs/heads/main"); got != c2 {
		t.Fatalf("Push reported success but the remote main is at %s, want the pushed commit %s (it was %s before)", got, c2, c1)
	}

	// Pushing it again, when there really is nothing to push, still succeeds.
	if err := repo.Push(ctx, "origin", "HEAD:refs/heads/main"); err != nil {
		t.Fatalf("repeated push failed: %v", err)
	}
	if got := gitT(t, bare, "rev-parse", "refs/heads/main"); got != c2 {
		t.Fatalf("remote main moved to %s after a repeated push, want %s", got, c2)
	}
}
