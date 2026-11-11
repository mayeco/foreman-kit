package cycle

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestSyncReposSaysWhenItPulledTheRoot: a fast-forward of the main checkout used to print
// "root: up to date with origin" although it pulled a teammate's commits.
func TestSyncReposSaysWhenItPulledTheRoot(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", bare)
	root := filepath.Join(base, "root")
	gitIn(t, base, "clone", "-q", bare, root)
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	gitIn(t, root, "push", "-q", "origin", "HEAD:main")
	gitIn(t, root, "branch", "-q", "--set-upstream-to=origin/main")
	mate := filepath.Join(base, "mate")
	gitIn(t, base, "clone", "-q", bare, mate)
	for _, m := range []string{"one", "two"} {
		gitIn(t, mate, "commit", "-q", "--allow-empty", "-m", m)
	}
	gitIn(t, mate, "push", "-q", "origin", "HEAD:main")

	ctx := context.Background()
	var out bytes.Buffer
	if err := SyncRepos(ctx, root, nil, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "root: pulled 2 commit(s) [main]") {
		t.Fatalf("first sync:\n%s", out.String())
	}
	out.Reset()
	if err := SyncRepos(ctx, root, nil, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "root: up to date with origin [main]") {
		t.Fatalf("second sync:\n%s", out.String())
	}
}
