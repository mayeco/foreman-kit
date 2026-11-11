package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestBaseBranchMissingOnOriginWarnsWithoutTheNoOpRemedy: when the checkout is already on the
// configured base branch, "foreman-kit config set pipeline.repository.base_branch <it>" changes nothing,
// so the warning only says to push it.
func TestBaseBranchMissingOnOriginWarnsWithoutTheNoOpRemedy(t *testing.T) {
	bare := t.TempDir()
	gitT(t, bare, "init", "-q", "--bare")
	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "pipeline.yaml"), []byte("repository:\n  base_branch: main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "init")
	gitT(t, root, "remote", "add", "origin", bare)

	warn := func() string {
		for _, w := range checkApplicationProblems(root).Warnings {
			if strings.Contains(w, "origin has no branch") {
				return w
			}
		}
		return ""
	}
	w := warn()
	if !strings.Contains(w, "push it (git push -u origin main)") || strings.Contains(w, "config set") {
		t.Fatalf("warning = %q, want only the push remedy", w)
	}

	gitT(t, root, "switch", "-q", "-c", "develop")
	if w := warn(); !strings.Contains(w, "foreman-kit config set pipeline.repository.base_branch develop") {
		t.Fatalf("on another branch the warning should offer to set it: %q", w)
	}
}
