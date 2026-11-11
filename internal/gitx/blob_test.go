package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBlobAtTellsAnAbsentFileFromOneItCannotRead: a commit that does not have the file answers
// "not found" with no error. A commit that names the file and whose object cannot be loaded, or a
// revision that does not exist, is an error: go-git answers "file not found" for both, which is why
// this asks git.
func TestBlobAtTellsAnAbsentFileFromOneItCannotRead(t *testing.T) {
	main, _ := ffSetup(t)
	ctx := context.Background()
	ffWrite(t, filepath.Join(main, "memory", "notes.md"), "a note\nwith two lines\n")
	ffGit(t, main, "add", "-A")
	ffGit(t, main, "commit", "-qm", "notes")

	content, found, err := BlobAt(ctx, main, "HEAD", "memory/notes.md")
	if err != nil || !found || string(content) != "a note\nwith two lines\n" {
		t.Fatalf("a file the commit has: %q, %v, %v", content, found, err)
	}
	if content, found, err := BlobAt(ctx, main, "HEAD", ".foreman-kit/agent_backlog.json"); err != nil || !found || string(content) != "[]\n" {
		t.Fatalf("a file of the first commit: %q, %v, %v", content, found, err)
	}
	for _, absent := range []string{"MEMORY.md", "memory/other.md", "nowhere/notes.md", "memory"} {
		if content, found, err := BlobAt(ctx, main, "HEAD", absent); err != nil || found || content != nil {
			t.Errorf("%s, which the commit does not have as a file: %q, %v, %v", absent, content, found, err)
		}
	}
	if _, found, err := BlobAt(ctx, main, "HEAD~1", "memory/notes.md"); err != nil || found {
		t.Errorf("a file the commit before does not have: %v, %v", found, err)
	}
	if _, _, err := BlobAt(ctx, main, "no-such-branch", "memory/notes.md"); err == nil {
		t.Error("a revision that does not exist was taken for a commit without the file")
	}

	hash := ffGit(t, main, "rev-parse", "HEAD:memory/notes.md")
	if err := os.Remove(filepath.Join(main, ".git", "objects", hash[:2], hash[2:])); err != nil {
		t.Fatal(err)
	}
	if _, found, err := BlobAt(ctx, main, "HEAD", "memory/notes.md"); err == nil {
		t.Errorf("a file whose object cannot be read was taken for found=%v with no error", found)
	} else if !strings.Contains(err.Error(), "cat-file") {
		t.Errorf("the error does not say what could not be read: %v", err)
	}
}
