package cycle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// movedBaseSession returns a session checkout of a bare remote whose base branch (main) a teammate
// moved after the session cloned it: README.md holds "v1" and the backlog one item in the session,
// and the remote also holds the teammate's commit.
func movedBaseSession(t *testing.T) string {
	t.Helper()
	stopTestGit(t)
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", bare)
	session := filepath.Join(base, "session")
	gitIn(t, base, "clone", "-q", bare, session)
	commitFile(t, session, memRel, "# Memory\n- base lesson\n", "initial")
	commitFile(t, session, "README.md", "v1\n", "docs: readme")
	commitFile(t, session, BacklogFile, `[{"change_name":"add-a"}]`+"\n", "chore: update backlog")
	gitIn(t, session, "branch", "-q", "--set-upstream-to=origin/main")
	mate := filepath.Join(base, "mate")
	gitIn(t, base, "clone", "-q", bare, mate)
	commitFile(t, mate, "main.go", "package main\n", "feat: the teammate's work")
	return session
}

// commitDocsAndBacklog makes the commit of the report: README.md plus the backlog, one commit.
func commitDocsAndBacklog(t *testing.T, session string) {
	t.Helper()
	writeText(t, filepath.Join(session, "README.md"), "v2 by the operator\n")
	writeText(t, filepath.Join(session, filepath.FromSlash(BacklogFile)), `[{"change_name":"add-a"},{"change_name":"add-b"}]`+"\n")
	gitIn(t, session, "commit", "-q", "-am", "docs and backlog")
}

// TestRebaseProposalsRefusesACommitOutsideTheRecordedScope: the rebase makes the recorded files again
// on top of the remote and nothing else, so a commit that also changes README.md would be lost to its
// reset. It refuses before the reset and names the path; HEAD and the working tree stay as they were.
func TestRebaseProposalsRefusesACommitOutsideTheRecordedScope(t *testing.T) {
	session := movedBaseSession(t)
	commitDocsAndBacklog(t, session)
	head := headOf(t, session)

	err := RebaseProposals(context.Background(), session, "main", io.Discard)
	if err == nil {
		t.Fatalf("the rebase of a commit that changes README.md said it was done; HEAD is now:\n%s", gitIn(t, session, "log", "--stat", "--oneline", "-2"))
	}
	if !strings.Contains(err.Error(), "README.md") {
		t.Errorf("the error does not name README.md: %v", err)
	}
	if strings.Contains(err.Error(), "agent_backlog.json") {
		t.Errorf("the error names a recorded file as outside the scope: %v", err)
	}
	if at := headOf(t, session); at != head {
		t.Errorf("HEAD is %s after the refusal, and the commit is %s", at, head)
	}
	if got := readText(t, filepath.Join(session, "README.md")); got != "v2 by the operator\n" {
		t.Errorf("README.md was left as %q", got)
	}
	if got := readText(t, filepath.Join(session, filepath.FromSlash(BacklogFile))); !strings.Contains(got, "add-b") {
		t.Errorf("the backlog was left as %q", got)
	}
	if st := strings.TrimSpace(gitIn(t, session, "status", "--porcelain")); st != "" {
		t.Errorf("the working tree is not clean after the refusal:\n%s", st)
	}
}

// TestRebaseProposalsRefusesADeletionOutsideTheRecordedScope: a path the commit removes counts too.
func TestRebaseProposalsRefusesADeletionOutsideTheRecordedScope(t *testing.T) {
	session := movedBaseSession(t)
	gitIn(t, session, "rm", "-q", "README.md")
	gitIn(t, session, "commit", "-q", "-m", "docs: drop the readme")
	head := headOf(t, session)

	err := RebaseProposals(context.Background(), session, "main", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "README.md") {
		t.Fatalf("expected a refusal naming README.md, got %v", err)
	}
	if at := headOf(t, session); at != head {
		t.Errorf("HEAD is %s after the refusal, and the commit is %s", at, head)
	}
	if _, statErr := os.Stat(filepath.Join(session, "README.md")); statErr == nil {
		t.Errorf("README.md came back in the working tree")
	}
}

// TestRebaseProposalsStillRebasesARecordOnlyCommit: a commit that changes only recorded files is made
// again on top of the moved remote, with its message and its edits.
func TestRebaseProposalsStillRebasesARecordOnlyCommit(t *testing.T) {
	session := movedBaseSession(t)
	writeText(t, memPath(session), "# Memory\n- base lesson\n- lesson from the cycle\n")
	writeText(t, filepath.Join(session, filepath.FromSlash(BacklogFile)), `[{"change_name":"add-a"},{"change_name":"add-b"}]`+"\n")
	gitIn(t, session, "commit", "-q", "-am", "chore(memory): scheduled cycle 20261111T120000Z")

	if err := RebaseProposals(context.Background(), session, "main", io.Discard); err != nil {
		t.Fatalf("a record-only commit was refused: %v", err)
	}
	gitIn(t, session, "merge-base", "--is-ancestor", "origin/main", "HEAD")
	if parent := strings.TrimSpace(gitIn(t, session, "rev-parse", "HEAD~1")); parent != strings.TrimSpace(gitIn(t, session, "rev-parse", "origin/main")) {
		t.Errorf("the record commit is not on top of origin/main:\n%s", gitIn(t, session, "log", "--oneline", "-4"))
	}
	if msg := strings.TrimSpace(gitIn(t, session, "log", "-1", "--format=%s")); msg != "chore(memory): scheduled cycle 20261111T120000Z" {
		t.Errorf("the record commit has the message %q", msg)
	}
	if got := readText(t, memPath(session)); !strings.Contains(got, "lesson from the cycle") {
		t.Errorf("the memory lost the cycle's lesson: %q", got)
	}
	if got := readText(t, filepath.Join(session, filepath.FromSlash(BacklogFile))); !strings.Contains(got, "add-b") {
		t.Errorf("the backlog lost add-b: %q", got)
	}
	if got := readText(t, filepath.Join(session, "main.go")); got != "package main\n" {
		t.Errorf("the teammate's file is %q", got)
	}
}

// TestProposalsCLIRebaseExitsOneWhenItRefuses: `foreman-kit proposals rebase <base>` says why on
// stderr, exits 1 and leaves the checkout as it was.
func TestProposalsCLIRebaseExitsOneWhenItRefuses(t *testing.T) {
	session := movedBaseSession(t)
	commitDocsAndBacklog(t, session)
	head := headOf(t, session)

	var stdout, stderr bytes.Buffer
	rc := RunProposalsCLI(context.Background(), session, nil, nil, []string{"rebase", "main"}, &stdout, &stderr)
	if rc != 1 {
		t.Fatalf("proposals rebase exited %d, want 1 (stdout %q, stderr %q)", rc, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "README.md") {
		t.Errorf("stderr does not name README.md: %q", stderr.String())
	}
	if at := headOf(t, session); at != head {
		t.Errorf("HEAD is %s after the refusal, and the commit is %s", at, head)
	}
	if got := readText(t, filepath.Join(session, "README.md")); got != "v2 by the operator\n" {
		t.Errorf("README.md was left as %q", got)
	}
}
