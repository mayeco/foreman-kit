package portal

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/gitx"
)

// TestReadingSettingsAndSetupLeavesTheGitIndexOfTheCheckoutAsItWas: a GET writes nothing, and the
// index of the main checkout is part of it. The settings read runs config.Check and the setup read
// runs cycle.SetupStatus, each with a "git status"; plain git rewrites .git/index under index.lock
// when the stat data of a file is stale, which collides with the merger and the dispatcher running
// git in the same checkout. After each of the four reads the index has the same bytes and mtime and
// no lock was left, while the dirty file is still reported.
func TestReadingSettingsAndSetupLeavesTheGitIndexOfTheCheckoutAsItWas(t *testing.T) {
	srv, _, root := setupTestPortal(t)
	ctx := context.Background()

	for name, content := range map[string]string{"notes.txt": "one\n", "other.txt": "two\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "two files"}} {
		if _, err := gitx.RunGit(ctx, root, args...); err != nil {
			t.Fatalf("git %v failed: %v", args, err)
		}
	}
	// Stale stat data on both files; one of them really differs from what was committed.
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

	for _, path := range []string{"/system/settings", "/api/system/settings", "/system/setup", "/api/system/setup"} {
		code, body, _ := getPage(t, srv, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
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
			t.Errorf("GET %s rewrote .git/index", path)
		}
		if !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
			t.Errorf("GET %s changed the mtime of .git/index from %v to %v", path, beforeInfo.ModTime(), afterInfo.ModTime())
		}
		if _, err := os.Stat(indexPath + ".lock"); err == nil {
			t.Errorf("GET %s left .git/index.lock", path)
		}
		// The dirty file is still reported by the read that checks for it.
		want := map[string]string{
			"/api/system/settings": "other.txt",
			"/system/settings":     "other.txt",
			"/api/system/setup":    "uncommitted path(s)",
			"/system/setup":        "uncommitted path(s)",
		}[path]
		if !strings.Contains(body, want) {
			t.Errorf("GET %s does not report the dirty file (want %q in the answer)", path, want)
		}
	}
}
