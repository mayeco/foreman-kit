package cycle

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

func gitRun(t *testing.T, dir string, args ...string) string {
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

// TestRecordSessionRefusesToPublishCommitsOutsideTheScope commits an out-of-scope file (as an agent's own
// commit, or the operator's unpushed work in the main checkout) and leaves the memory dirty. The record
// push would publish that commit to the base branch, so RecordSession must refuse and pause instead.
func TestRecordSessionRefusesToPublishCommitsOutsideTheScope(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, filepath.Dir(bare), "init", "--quiet", "--bare", "-b", "main", bare)
	wt := t.TempDir()
	gitRun(t, wt, "init", "--quiet", "-b", "main")
	gitRun(t, wt, "remote", "add", "origin", bare)
	if err := os.WriteFile(memPath(wt), []byte("# memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "add", memRel)
	gitRun(t, wt, "commit", "--quiet", "-m", "base")
	gitRun(t, wt, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	gitRun(t, wt, "fetch", "--quiet", "origin")
	before := gitRun(t, bare, "rev-parse", "refs/heads/main")

	if err := os.MkdirAll(filepath.Join(wt, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".foreman-kit", "pipeline.yaml"), []byte("repository:\n  base_branch: main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "add", ".foreman-kit/pipeline.yaml")
	gitRun(t, wt, "commit", "--quiet", "-m", "unreviewed change")
	if err := os.WriteFile(memPath(wt), []byte("# memory\nlesson\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenAt(filepath.Join(t.TempDir(), "state.db"), "ws-record-scope", wt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	var out bytes.Buffer
	if err := RecordSession(ctx, wt, "main", "chore(memory): record", st, &out); err == nil {
		t.Fatalf("expected RecordSession to refuse an out-of-scope commit, output:\n%s", out.String())
	}
	if after := gitRun(t, bare, "rev-parse", "refs/heads/main"); after != before {
		t.Fatalf("the out-of-scope commit was published: remote main moved from %s to %s", before, after)
	}
	if ws, err := st.GetWorkspaceState(ctx); err != nil || !ws.Paused {
		t.Fatalf("expected the workspace to be paused for a human, got %+v (err=%v)", ws, err)
	}
}
