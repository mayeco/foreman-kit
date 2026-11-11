package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/gitx"
)

// workLossParseLost builds a repository in which rel is committed before the change starts (before), a
// teammate edits it after the start (teammate) and the change edits it on top (change), with userConfig
// as the user's global git configuration. It returns the teammate's commit and what
// DetectConcurrentWorkLoss reports for the change.
func workLossParseLost(t *testing.T, userConfig, rel, before, teammate, change string) (string, []LostCommit) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte(userConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := func(args ...string) string {
		t.Helper()
		out, err := gitx.RunGit(ctx, dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	// commitAt commits content with both dates at when: blame reports the committer time.
	commitAt := func(when time.Time, author, content string) string {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		stamp := when.Format(time.RFC3339)
		t.Setenv("GIT_COMMITTER_DATE", stamp)
		git("add", "-A")
		git("-c", "user.name="+author, "-c", "user.email="+author+"@example.com", "-c", "commit.gpgsign=false",
			"commit", "-q", "--date", stamp, "-m", author+" edits "+rel)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q", "-b", "main")

	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	commitAt(start.Add(-time.Hour), "early", before)
	onto := commitAt(start.Add(10*time.Minute), "teammate", teammate)
	rebased := commitAt(start.Add(20*time.Minute), "agent", change)

	lost, err := DetectConcurrentWorkLoss(ctx, dir, "repos/backend", onto, rebased, start)
	if err != nil {
		t.Fatal(err)
	}
	return onto, lost
}

// requireWorkLossParseLine checks that lost is the teammate's commit alone, losing exactly want
// ("file:line: text").
func requireWorkLossParseLine(t *testing.T, teammate string, lost []LostCommit, want string) {
	t.Helper()
	if len(lost) != 1 || lost[0].Commit != teammate || lost[0].TotalLost != 1 || len(lost[0].LostLines) != 1 || lost[0].LostLines[0] != want {
		t.Fatalf("lost = %+v, want the teammate's commit %s losing %q", lost, teammate, want)
	}
}

// git ends the "--- a/<name>" header with a tab when the name holds a space; read as part of the name,
// it made blame fail and the file's lost lines go unreported.
func TestWorkLossIsDetectedInAFileWhoseNameHasASpace(t *testing.T) {
	teammate, lost := workLossParseLost(t, "", "docs/user guide.md",
		"intro\n",
		"intro\nhotfix: restart the worker\n",
		"intro\nmine\n")
	requireWorkLossParseLine(t, teammate, lost, "docs/user guide.md:2: hotfix: restart the worker")
}

// A removed line that starts with "-- " (an SQL, Lua or Haskell comment) reads "--- ..." in the diff,
// like the header naming the old file: it was taken for one, and every later lost line of the file was
// blamed in a file that does not exist.
func TestWorkLossIsDetectedAfterARemovedLineThatLooksLikeAFileHeader(t *testing.T) {
	teammate, lost := workLossParseLost(t, "", "db/schema.sql",
		"-- users\ncreate table users;\n",
		"-- users\ncreate table users;\ncreate index hotfix on users;\n",
		"create table users;\ncreate table mine;\n")
	requireWorkLossParseLine(t, teammate, lost, "db/schema.sql:3: create index hotfix on users;")
}

// With diff.noprefix the header carries no "a/": stripping one from a path that really starts with a/
// blamed another file.
func TestWorkLossDetectionDoesNotDependOnTheUsersDiffPrefix(t *testing.T) {
	teammate, lost := workLossParseLost(t, "[diff]\n\tnoprefix = true\n", "a/routes.go",
		"package a\n",
		"package a\n// hotfix\n",
		"package a\n// mine\n")
	requireWorkLossParseLine(t, teammate, lost, "a/routes.go:2: // hotfix")
}

// With diff.interHunkContext git fuses nearby hunks and prints the unchanged lines between them, which
// were not counted: every lost line after them was blamed one line too early.
func TestWorkLossDetectionCountsTheContextBetweenFusedHunks(t *testing.T) {
	teammate, lost := workLossParseLost(t, "[diff]\n\tinterHunkContext = 3\n", "routes.go",
		"keep1\nold\nkeep2\nkeep3\n",
		"keep1\nold\nkeep2\nhotfix\nkeep3\n",
		"keep1\nkeep2\nkeep3\n")
	requireWorkLossParseLine(t, teammate, lost, "routes.go:4: hotfix")
}

// A global blame.ignoreRevsFile naming a file the repository does not have makes every git blame fail.
func TestWorkLossDetectionDoesNotDependOnTheUsersBlameIgnoreRevsFile(t *testing.T) {
	teammate, lost := workLossParseLost(t, "[blame]\n\tignoreRevsFile = .git-blame-ignore-revs\n", "routes.go",
		"keep\n",
		"keep\nhotfix\n",
		"keep\nmine\n")
	requireWorkLossParseLine(t, teammate, lost, "routes.go:2: hotfix")
}

func TestRemovedLinesReadsHunksByTheirLineCounts(t *testing.T) {
	diff := "diff --git a/my file.sql b/my file.sql\n" +
		"index 1..2 100644\n" +
		"--- a/my file.sql\t\n" +
		"+++ b/my file.sql\t\n" +
		"@@ -2,2 +1,0 @@ create table t;\n" +
		"--- drop the old index\n" +
		"-drop index old;\n" +
		"@@ -7 +5 @@\n" +
		"-last line\n" +
		"\\ No newline at end of file\n" +
		"+++ last line\n" +
		"diff --git \"a/tab\\there.txt\" \"b/tab\\there.txt\"\n" +
		"--- \"a/tab\\there.txt\"\n" +
		"+++ \"b/tab\\there.txt\"\n" +
		"@@ -4 +3,0 @@\n" +
		"-gone\n"
	got := removedLines(diff)
	sql := got["my file.sql"]
	if len(sql) != 3 || sql[0] != (removedLine{2, "-- drop the old index"}) || sql[1] != (removedLine{3, "drop index old;"}) || sql[2] != (removedLine{7, "last line"}) {
		t.Fatalf("my file.sql loses %+v", sql)
	}
	if tab := got["tab\there.txt"]; len(tab) != 1 || tab[0] != (removedLine{4, "gone"}) {
		t.Fatalf("tab<TAB>here.txt loses %+v", tab)
	}
	if len(got) != 2 {
		t.Fatalf("unexpected files: %+v", got)
	}
}
