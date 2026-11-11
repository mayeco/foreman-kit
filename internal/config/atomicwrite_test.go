package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileAtomicKeepsModeFollowsSymlinksAndLeavesNoTemp verifies the replacement semantics config
// writers rely on.
func TestWriteFileAtomicKeepsModeFollowsSymlinksAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "agents.yaml")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(link, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "new\n" {
		t.Fatalf("symlink target not updated: %q", b)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink itself was replaced")
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed to %v", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("expected only the file and the link, found %d entries (a temp file was left behind)", len(entries))
	}
}
