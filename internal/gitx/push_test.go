package gitx

import (
	"context"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitT runs git in dir with a fixed identity and fails the test on error.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newHTTPRemote serves a bare repository over smart HTTP with `git http-backend`, so pushes take the
// go-git transport (the git CLI path is only used for local, ProxyJump/ProxyCommand or GIT_SSH_COMMAND remotes).
func newHTTPRemote(t *testing.T) (url, bareDir string) {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	serveRoot := t.TempDir()
	bareDir = filepath.Join(serveRoot, "remote.git")
	gitT(t, serveRoot, "init", "--quiet", "--bare", "-b", "main", bareDir)
	gitT(t, bareDir, "config", "http.receivepack", "true")
	srv := httptest.NewServer(&cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + serveRoot, "GIT_HTTP_EXPORT_ALL=1"},
	})
	t.Cleanup(srv.Close)
	return srv.URL + "/remote.git", bareDir
}

// TestPushFromAttachedHEADReachesTheRemote reproduces the merger's child push: HEAD is attached to an
// integrate/<change> branch and the refspec is "HEAD:refs/heads/<base>". go-git used to skip the symbolic
// HEAD and report success without pushing anything.
func TestPushFromAttachedHEADReachesTheRemote(t *testing.T) {
	url, bare := newHTTPRemote(t)
	local := t.TempDir()
	ctx := context.Background()

	gitT(t, local, "init", "--quiet", "-b", "main")
	gitT(t, local, "remote", "add", "origin", url)
	gitT(t, local, "commit", "--quiet", "--allow-empty", "-m", "c1")
	c1 := gitT(t, local, "rev-parse", "HEAD")
	gitT(t, local, "push", "--quiet", "origin", "HEAD:refs/heads/main")

	gitT(t, local, "checkout", "--quiet", "-B", "integrate/demo")
	gitT(t, local, "commit", "--quiet", "--allow-empty", "-m", "c2")
	c2 := gitT(t, local, "rev-parse", "HEAD")

	repo, err := Open(local)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(ctx, "origin", "HEAD:refs/heads/main"); err != nil {
		t.Fatalf("push from an attached HEAD failed: %v", err)
	}
	if got := gitT(t, bare, "rev-parse", "refs/heads/main"); got != c2 {
		t.Fatalf("remote main = %s, want the pushed commit %s (it was %s before the push)", got, c2, c1)
	}

	// Pushing the same commit again is an idempotent no-op.
	if err := repo.Push(ctx, "origin", "HEAD:refs/heads/main"); err != nil {
		t.Fatalf("repeated push failed: %v", err)
	}

	// A bare "HEAD" refspec pushes the current branch under its own name.
	gitT(t, local, "checkout", "--quiet", "-b", "feature/x")
	gitT(t, local, "commit", "--quiet", "--allow-empty", "-m", "c3")
	c3 := gitT(t, local, "rev-parse", "HEAD")
	if err := repo.Push(ctx, "origin", "HEAD"); err != nil {
		t.Fatalf("push of bare HEAD failed: %v", err)
	}
	if got := gitT(t, bare, "rev-parse", "refs/heads/feature/x"); got != c3 {
		t.Fatalf("remote feature/x = %s, want %s", got, c3)
	}

	// A non-fast-forward update is still rejected: no force push is ever introduced.
	gitT(t, local, "checkout", "--quiet", "-B", "stale", c1)
	gitT(t, local, "commit", "--quiet", "--allow-empty", "-m", "diverged")
	if err := repo.Push(ctx, "origin", "HEAD:refs/heads/main"); err == nil {
		t.Fatal("expected a non-fast-forward push to be rejected")
	}
	if got := gitT(t, bare, "rev-parse", "refs/heads/main"); got != c2 {
		t.Fatalf("remote main moved to %s after a rejected push, want %s", got, c2)
	}
}
