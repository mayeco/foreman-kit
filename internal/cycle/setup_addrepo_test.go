package cycle

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestSetupAddRepoRejectsOptionLikeURLAndBranch verifies that a repository URL or branch that git would
// parse as an option is refused before git runs: --upload-pack=<command> would otherwise execute
// <command> (the URL is reachable from the portal's POST /api/setup/repo).
func TestSetupAddRepoRejectsOptionLikeURLAndBranch(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "injected")
	ctx := context.Background()

	if err := SetupAddRepo(ctx, root, "--upload-pack=touch "+marker, "x", "", io.Discard); err == nil {
		t.Fatal("expected an option-like repository URL to be rejected")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command injected through the repository URL ran")
	}
	if err := SetupAddRepo(ctx, root, "https://example.com/r.git\n--upload-pack=x", "x", "", io.Discard); err == nil {
		t.Fatal("expected a repository URL with a line break to be rejected")
	}
	if err := SetupAddRepo(ctx, root, "https://example.com/r.git", "x", "--orphan", io.Discard); err == nil {
		t.Fatal("expected an option-like branch to be rejected")
	}
}
