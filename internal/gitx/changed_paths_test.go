package gitx

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestChangedPathsReturnsUnquotedPathsAndBothRenameSides verifies that paths with spaces or non-ASCII
// characters come back exactly as named (plain porcelain output C-quotes them, which turned files inside an
// allowed folder into false policy violations) and that both sides of a staged rename are reported.
func TestChangedPathsReturnsUnquotedPathsAndBothRenameSides(t *testing.T) {
	repo := t.TempDir()
	gitT(t, repo, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "old.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "old.md")
	gitT(t, repo, "commit", "--quiet", "-m", "base")
	gitT(t, repo, "mv", "old.md", "renamed.md")
	dir := filepath.Join(repo, "openspec", "changes", "x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"my notes.md", "café.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("n\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ChangedPaths(context.Background(), repo, "")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"old.md", "openspec/changes/x/café.md", "openspec/changes/x/my notes.md", "renamed.md"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("ChangedPaths = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ChangedPaths = %q, want %q", got, want)
		}
	}
}
