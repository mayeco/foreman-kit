package gitx

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// TestChangedPathsReportsBothSidesOfACommittedRename: the worker's policy check lists what a change
// touched since its fork point with ChangedPaths. git diff detects renames by default and names only the
// destination, so a file moved from outside the allowed paths into the change's folder, and committed,
// came back as one allowed path: the file removed outside went unnoticed until the merge.
func TestChangedPathsReportsBothSidesOfACommittedRename(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	repo := t.TempDir()
	gitT(t, repo, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Project\n\nOne.\nTwo.\nThree.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "README.md")
	gitT(t, repo, "commit", "--quiet", "-m", "base")
	fork := gitT(t, repo, "rev-parse", "HEAD")

	if err := os.MkdirAll(filepath.Join(repo, "openspec", "changes", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "mv", "README.md", "openspec/changes/x/README.md")
	gitT(t, repo, "commit", "--quiet", "-m", "move the README into the change")

	got, err := ChangedPaths(context.Background(), repo, fork)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"README.md", "openspec/changes/x/README.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ChangedPaths since the fork = %q, want both sides of the committed rename %q", got, want)
	}
}
