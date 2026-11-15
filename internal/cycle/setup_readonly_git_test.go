package cycle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSetupStatusLeavesTheGitIndexAsItWasAndStillCountsDirtyPaths: setup status is a read, and the
// portal answers GET /system/setup and GET /api/system/setup with it. Plain "git status" rewrites
// .git/index (under index.lock) when the stat data of a file is stale, so the dirty check runs with
// --no-optional-locks: the index keeps its bytes and its mtime, no lock appears, and the changed
// path is still counted.
func TestSetupStatusLeavesTheGitIndexAsItWasAndStillCountsDirtyPaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := Init(ctx, root, KitOptions{Agents: "claude"}, nil); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	gitIn(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "init")

	// Stale stat data on a file whose content is committed, and one file that really differs.
	stale := time.Now().Add(-time.Hour)
	for _, name := range []string{"notes.txt", "other.txt"} {
		if err := os.Chtimes(filepath.Join(root, name), stale, stale); err != nil {
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

	rep, err := SetupStatus(ctx, root, openTestStore(t, root))
	if err != nil {
		t.Fatalf("SetupStatus failed: %v", err)
	}

	if text := FormatSetupStatusText(rep); !strings.Contains(text, "1 uncommitted path(s) in the umbrella") {
		t.Fatalf("the dirty path is not counted:\n%s", text)
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
		t.Fatal("SetupStatus rewrote .git/index")
	}
	if !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatalf(".git/index mtime changed from %v to %v", beforeInfo.ModTime(), afterInfo.ModTime())
	}
	if _, err := os.Stat(indexPath + ".lock"); err == nil {
		t.Fatal(".git/index.lock was left behind")
	}
}
