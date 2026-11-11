package cycle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The merger writes the history it published into the main checkout. When that history was rotated, the
// file holding its older entries (.foreman-kit/agent_history.1.json) is untracked there until the commit that
// added it is pulled, and sync answered "local changes present; skipping pull" from then on.
func TestSyncReposPullsAfterAHistoryRotation(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	write := func(dir, rel, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", bare)
	root := filepath.Join(base, "root")
	gitIn(t, base, "clone", "-q", bare, root)
	write(root, BacklogFile, "[]\n")
	write(root, HistoryFile, `[{"change_name":"a"}]`+"\n")
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "init")
	gitIn(t, root, "push", "-q", "origin", "HEAD:main")
	gitIn(t, root, "branch", "-q", "--set-upstream-to=origin/main")

	// An integration rotates the history: on the base...
	rotated, current := `[{"change_name":"a"}]`+"\n", `[{"change_name":"b"}]`+"\n"
	mate := filepath.Join(base, "mate")
	gitIn(t, base, "clone", "-q", bare, mate)
	write(mate, ".foreman-kit/agent_history.1.json", rotated)
	write(mate, HistoryFile, current)
	gitIn(t, mate, "add", "-A")
	gitIn(t, mate, "commit", "-q", "-m", "chore(merge): integrate b")
	gitIn(t, mate, "push", "-q", "origin", "HEAD:main")
	// ...and, written by the merger, in the main checkout.
	write(root, ".foreman-kit/agent_history.1.json", rotated)
	write(root, HistoryFile, current)

	var out bytes.Buffer
	if err := SyncRepos(context.Background(), root, nil, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "root: pulled 1 commit(s) [main]") {
		t.Fatalf("sync did not pull the rotation:\n%s", out.String())
	}
	if st := strings.TrimSpace(gitIn(t, root, "status", "--porcelain")); st != "" {
		t.Fatalf("the main checkout is not clean after the sync:\n%s", st)
	}
	if got, err := os.ReadFile(filepath.Join(root, ".foreman-kit", "agent_history.1.json")); err != nil || string(got) != rotated {
		t.Fatalf(".foreman-kit/agent_history.1.json = %q (err %v), want the published rotation", got, err)
	}
}
