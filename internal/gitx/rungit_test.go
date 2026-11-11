package gitx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunGitCancellationLeavesNoIndexLock cancels a commit while a slow pre-commit hook runs. git only
// deletes .git/index.lock from its SIGTERM/SIGINT handler, so RunGit must not SIGKILL it on cancellation.
func TestRunGitCancellationLeavesNoIndexLock(t *testing.T) {
	repo := t.TempDir()
	gitT(t, repo, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "f.txt")
	gitT(t, repo, "commit", "--quiet", "-m", "base")
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nsleep 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// "commit -a" holds index.lock while the pre-commit hook runs (an as-is commit releases it first).
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	if _, err := RunGit(ctx, repo, "commit", "-a", "-m", "slow"); err == nil {
		t.Fatal("expected the canceled commit to fail")
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "index.lock")); err == nil {
		t.Fatal("a canceled git command left .git/index.lock behind")
	}
}

// TestRebaseBranchDetectsConflictUnderTranslatedLocale verifies that a content conflict is recognised
// even when the operator's locale would translate git's messages.
func TestRebaseBranchDetectsConflictUnderTranslatedLocale(t *testing.T) {
	repo := t.TempDir()
	gitT(t, repo, "init", "--quiet", "-b", "main")
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("base\n")
	gitT(t, repo, "add", "f.txt")
	gitT(t, repo, "commit", "--quiet", "-m", "base")
	gitT(t, repo, "checkout", "--quiet", "-b", "feature")
	write("feature\n")
	gitT(t, repo, "commit", "--quiet", "-am", "feature")
	gitT(t, repo, "checkout", "--quiet", "main")
	write("main\n")
	gitT(t, repo, "commit", "--quiet", "-am", "main")
	gitT(t, repo, "checkout", "--quiet", "feature")

	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	res := RebaseBranch(context.Background(), repo, "main")
	if res.OK || !res.ContentConflict {
		t.Fatalf("expected a content conflict to be detected, got %+v", res)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "rebase-merge")); err == nil {
		t.Fatal("RebaseBranch left a rebase in progress")
	}
}
