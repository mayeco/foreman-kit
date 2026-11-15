package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCheckLeavesTheGitIndexAsItWasAndStillReportsDirtyFiles: config check is a read, and the portal
// answers GET /system/settings and GET /api/system/settings with it. Plain "git status" refreshes
// .git/index when the stat data of a file is stale (it takes index.lock and rewrites the file), so the
// uncommitted-files check runs with --no-optional-locks: the index is the same bytes with the same
// mtime afterwards, no lock appeared, and the dirty file is still reported.
func TestCheckLeavesTheGitIndexAsItWasAndStillReportsDirtyFiles(t *testing.T) {
	root := t.TempDir()
	gitT(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "init")

	// Stale stat data: the content of both files is what was committed, their mtime is not. The
	// second file really differs, so it must be reported.
	later := time.Now().Add(-time.Hour)
	for _, name := range []string{"notes.txt", "other.txt"} {
		if err := os.Chtimes(filepath.Join(root, name), later, later); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	indexPath := filepath.Join(root, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	var warning string
	for _, w := range checkApplicationProblems(root).Warnings {
		if strings.Contains(w, "uncommitted outside the cycle's write scope") {
			warning = w
		}
	}

	if !strings.Contains(warning, "other.txt") {
		t.Fatalf("the dirty file is not reported: %q", warning)
	}
	if strings.Contains(warning, "notes.txt") {
		t.Fatalf("a file whose content is committed is reported as dirty: %q", warning)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("config check rewrote .git/index")
	}
	if !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatalf(".git/index mtime changed from %v to %v", beforeInfo.ModTime(), afterInfo.ModTime())
	}
	if _, err := os.Stat(indexPath + ".lock"); err == nil {
		t.Fatal(".git/index.lock was left behind")
	}
}
