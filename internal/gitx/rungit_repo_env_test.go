package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunGitRunsInItsDirWhateverRepositoryTheEnvironmentNames: a process started from a git hook, a "!"
// alias or "git rebase --exec" in a linked worktree or a submodule inherits GIT_DIR and GIT_INDEX_FILE
// (and GIT_WORK_TREE, GIT_PREFIX) of that repository. RunGit passed them on, so every git command ran
// against that repository and its index, whatever directory it was given.
func TestRunGitRunsInItsDirWhateverRepositoryTheEnvironmentNames(t *testing.T) {
	ctx := context.Background()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	mine, hooked := t.TempDir(), t.TempDir()
	for _, dir := range []string{mine, hooked} {
		gitT(t, dir, "init", "--quiet", "-b", "main")
		gitT(t, dir, "commit", "--quiet", "--allow-empty", "-m", "initial commit of "+filepath.Base(dir))
	}
	mineHead := gitT(t, mine, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(mine, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// What a commit hook of the repository "hooked" exports.
	t.Setenv("GIT_DIR", filepath.Join(hooked, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(hooked, ".git", "index"))
	t.Setenv("GIT_WORK_TREE", hooked)
	t.Setenv("GIT_PREFIX", "")

	out, err := RunGit(ctx, mine, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(out) != mineHead {
		t.Fatalf("rev-parse HEAD in %s answered %q (err %v), want its own HEAD %s", mine, strings.TrimSpace(out), err, mineHead)
	}
	if _, err := RunGit(ctx, mine, "add", "new.txt"); err != nil {
		t.Fatalf("git add in %s: %v", mine, err)
	}
	if out, err := RunGit(ctx, mine, "ls-files"); err != nil || strings.TrimSpace(out) != "new.txt" {
		t.Fatalf("ls-files in %s = %q (err %v), want new.txt staged in its own index", mine, strings.TrimSpace(out), err)
	}
	if out, err := RunGit(ctx, hooked, "ls-files"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("git add in %s wrote to the index of %s: ls-files there = %q (err %v)", mine, hooked, strings.TrimSpace(out), err)
	}
}

// TestRunGitKeepsTheConfigurationPassedThroughTheEnvironment: unlike the variables that name a repository,
// "git -c" settings travel in GIT_CONFIG_COUNT/GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n and must reach git.
func TestRunGitKeepsTheConfigurationPassedThroughTheEnvironment(t *testing.T) {
	repo := t.TempDir()
	gitT(t, repo, "init", "--quiet", "-b", "main")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "openspec.probe")
	t.Setenv("GIT_CONFIG_VALUE_0", "kept")
	out, err := RunGit(context.Background(), repo, "config", "--get", "openspec.probe")
	if err != nil || strings.TrimSpace(out) != "kept" {
		t.Fatalf("config from the environment = %q (err %v), want kept", strings.TrimSpace(out), err)
	}
}
