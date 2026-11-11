package cycle

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The record of a cycle (its memory, the backlog, the history, the proposals) is reconciled from
// what three places hold: the commit before it, the record itself and the checkout it lands on. A
// place that cannot be read has not said "the file is not here". Taken for that, a record whose
// backlog could not be read made the reconcile drop every item of the queue, a memory file that
// could not be read was deleted, and the cycle committed and published both.

// loseBlob removes from the object store of the repository at dir the object that holds rel as rev
// records it: the commit still names the file, and nothing can read it. It is what a reader meets
// when the store changed under it.
func loseBlob(t *testing.T, dir, rev, rel string) {
	t.Helper()
	hash := strings.TrimSpace(gitIn(t, dir, "rev-parse", rev+":"+rel))
	gitDir := strings.TrimSpace(gitIn(t, dir, "rev-parse", "--absolute-git-dir"))
	object := filepath.Join(gitDir, "objects", hash[:2], hash[2:])
	if err := os.Remove(object); err != nil {
		t.Fatalf("cannot remove the object of %s at %s: %v", rel, rev, err)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(b)
}

// TestRebaseProposalsStopsWhenTheRecordCannotBeRead: the base branch moved under a session, and the
// record commit is made again on top of it from what the commit holds. When one of its files cannot
// be read the rebase stops before it touches anything: the record commit stays, and with it the
// items the cycle queued and what it learned.
func TestRebaseProposalsStopsWhenTheRecordCannotBeRead(t *testing.T) {
	stopTestGit(t)
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", bare)
	session := filepath.Join(base, "session")
	gitIn(t, base, "clone", "-q", bare, session)
	commitFile(t, session, memRel, "# Memory\n- base lesson\n", "initial")
	commitFile(t, session, BacklogFile, `[{"change_name":"add-a"},{"change_name":"add-b"}]`+"\n", "chore: update backlog")
	gitIn(t, session, "branch", "-q", "--set-upstream-to=origin/main")
	mate := filepath.Join(base, "mate")
	gitIn(t, base, "clone", "-q", bare, mate)
	commitFile(t, mate, "README.md", "# App\n", "docs: readme")

	memory, backlog := memPath(session), filepath.Join(session, filepath.FromSlash(BacklogFile))
	const learned = "# Memory\n- base lesson\n- lesson from the cycle\n"
	const queued = `[{"change_name":"add-a"},{"change_name":"add-b"},{"change_name":"add-c"}]` + "\n"
	if err := os.WriteFile(memory, []byte(learned), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backlog, []byte(queued), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, session, "commit", "-q", "-am", "chore(memory): scheduled cycle 20261111T120000Z")
	record := headOf(t, session)

	for _, rel := range []string{memRel, BacklogFile} {
		loseBlob(t, session, "HEAD", rel)
	}

	err := RebaseProposals(context.Background(), session, "main", io.Discard)
	if err == nil {
		t.Error("the rebase of a record it could not read said it was done")
	}
	if at := headOf(t, session); at != record {
		t.Errorf("HEAD is %s after the rebase, and the record commit is %s:\n%s", at, record, gitIn(t, session, "log", "--oneline", "-4"))
	}
	if got, readErr := os.ReadFile(memory); readErr != nil {
		t.Errorf("the memory file was removed from the checkout: %v", readErr)
	} else if string(got) != learned {
		t.Errorf("the memory file was left as:\n%s", got)
	}
	if got, readErr := os.ReadFile(backlog); readErr != nil || string(got) != queued {
		t.Errorf("the backlog was left as %q (%v), and the cycle had queued add-c on add-a and add-b", strings.TrimSpace(string(got)), readErr)
	}
}

// TestCarryProposalsStopsWhenTheBaseCannotBeRead: what a session edited and did not commit is
// carried to another checkout by reconciling it against the commit the session stands on. When that
// commit's copy cannot be read, nothing is carried and the caller is told. (git refuses to list the
// edits against a commit it cannot read, so this held before the carry read its base with git: the
// test keeps it so.)
func TestCarryProposalsStopsWhenTheBaseCannotBeRead(t *testing.T) {
	stopTestGit(t)
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", bare)
	src := filepath.Join(base, "session")
	gitIn(t, base, "clone", "-q", bare, src)
	const committed = `[{"change_name":"add-a"},{"change_name":"add-b"}]` + "\n"
	commitFile(t, src, BacklogFile, committed, "chore: update backlog")
	dst := filepath.Join(base, "main-checkout")
	gitIn(t, base, "clone", "-q", bare, dst)

	// The session abandoned add-b and has not committed it.
	if err := os.WriteFile(filepath.Join(src, filepath.FromSlash(BacklogFile)), []byte(`[{"change_name":"add-a"}]`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loseBlob(t, src, "HEAD", BacklogFile)

	if err := CarryProposals(context.Background(), dst, src, io.Discard); err == nil {
		t.Error("the carry of edits whose base it could not read said it was done")
	}
	if got := readText(t, filepath.Join(dst, filepath.FromSlash(BacklogFile))); got != committed {
		t.Errorf("the backlog of the other checkout was rewritten as %s", strings.TrimSpace(got))
	}
}

// TestAFileThatCannotBeReadIsNotAnAbsentOne: a file of the checkout that is not there reads as
// absent, and one that cannot be read (here, a directory where the file should be) is an error.
func TestAFileThatCannotBeReadIsNotAnAbsentOne(t *testing.T) {
	dir := t.TempDir()
	if text, err := fileOrAbsent(memPath(dir)); err != nil || text != nil {
		t.Errorf("a file that is not there: %v, %v", text, err)
	}
	if err := os.WriteFile(memPath(dir), []byte("# Memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if text, err := fileOrAbsent(memPath(dir)); err != nil || text == nil || *text != "# Memory\n" {
		t.Errorf("a file that is there: %v, %v", text, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if text, err := fileOrAbsent(filepath.Join(dir, "memory")); err == nil {
		t.Errorf("a path that cannot be read as a file was taken for %v", text)
	}

	// The reconcile reads every file it lands on before it writes the first one.
	local, baseText := "# Memory\n- lesson\n", "# Memory\n"
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "memory", "notes.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memPath(root), []byte(baseText), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Reconcile3Way(context.Background(), root,
		map[string]*string{memRel: &baseText, "memory/notes.md": nil},
		map[string]*string{memRel: &local, "memory/notes.md": &local})
	if err == nil {
		t.Error("a reconcile onto a file it could not read said it was done")
	}
	if got := readText(t, memPath(root)); got != baseText {
		t.Errorf("the memory file was written although the reconcile could not read every file it lands on:\n%s", got)
	}
}
