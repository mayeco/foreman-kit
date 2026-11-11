package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The files a change touches were handed to git log, show and diff as pathspecs. A name such as
// "pages/[id].go" (a dynamic route) is also a pattern there: it matches pages/i.go and pages/d.go, so a
// teammate's commit on one of those counted as concurrent work on the change's files and was put before
// the semantic_merge_conflict gate.
func TestConcurrentCommitsOnFilesReadsFileNamesLiterally(t *testing.T) {
	ctx := context.Background()
	dir, git := newTestRepo(t)
	commitAt := func(when time.Time, msg string) string {
		t.Helper()
		stamp := when.Format(time.RFC3339)
		t.Setenv("GIT_COMMITTER_DATE", stamp)
		git("add", "-A")
		git("commit", "-q", "--date", stamp, "-m", msg)
		return git("rev-parse", "HEAD")
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	write("pages/[id].go", "a\n")
	write("pages/i.go", "x\n")
	commitAt(start.Add(-time.Hour), "early")
	// A teammate's commit after the change started, on another file.
	write("pages/i.go", "y\n")
	onto := commitAt(start.Add(5*time.Minute), "chore: another page")
	// The change touches only the dynamic route.
	write("pages/[id].go", "a\nb\n")
	rebased := commitAt(start.Add(10*time.Minute), "feat: dynamic route")

	cs, diff, err := ConcurrentCommitsOnFiles(ctx, dir, "repos/web", onto, rebased, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Fatalf("no commit on the base touched pages/[id].go since the change started, got %+v", cs)
	}
	if strings.Contains(diff, "pages/i.go") {
		t.Fatalf("the change's diff covers a file it did not touch:\n%s", diff)
	}

	// With a concurrent commit on the route itself, that commit (and only its part on that file) is listed.
	write("pages/[id].go", "a\n// invariant: a first\n")
	write("pages/i.go", "z\n")
	onto = commitAt(start.Add(20*time.Minute), "fix: invariant on the route")
	write("pages/[id].go", "b\na\n// invariant: a first\n")
	rebased = commitAt(start.Add(30*time.Minute), "feat: b first")

	cs, diff, err = ConcurrentCommitsOnFiles(ctx, dir, "repos/web", onto, rebased, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Message != "fix: invariant on the route" || cs[1].Message != "feat: dynamic route" {
		t.Fatalf("want the two commits on pages/[id].go since the change started, got %+v", cs)
	}
	if strings.Contains(cs[0].Diff, "pages/i.go") || !strings.Contains(cs[0].Diff, "+// invariant: a first") {
		t.Fatalf("the concurrent commit's diff must cover pages/[id].go only:\n%s", cs[0].Diff)
	}
	if !strings.Contains(diff, "+b") || strings.Contains(diff, "pages/i.go") {
		t.Fatalf("the change's diff must cover pages/[id].go only:\n%s", diff)
	}
}
