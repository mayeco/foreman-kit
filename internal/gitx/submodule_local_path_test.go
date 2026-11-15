package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSubmoduleUpdateInitAllowsALocalPathChild: with git's defaults only (no GIT_CONFIG_* variable, an
// empty global configuration) a child recorded by a local path is initialized, and the allowance is
// written to no configuration file.
func TestSubmoduleUpdateInitAllowsALocalPathChild(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	for _, name := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_PARAMETERS"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
		return string(out)
	}

	base := t.TempDir()
	childBare := filepath.Join(base, "child.git")
	git(base, "init", "-q", "--bare", childBare)
	childWork := filepath.Join(base, "child-work")
	git(base, "clone", "-q", childBare, childWork)
	if err := os.WriteFile(filepath.Join(childWork, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(childWork, "add", "a.txt")
	git(childWork, "commit", "-q", "-m", "one")
	git(childWork, "push", "-q", "origin", "HEAD")
	want := strings.TrimSpace(git(childWork, "rev-parse", "HEAD"))

	seed := filepath.Join(base, "seed")
	git(base, "init", "-q", seed)
	git(seed, "-c", "protocol.file.allow=always", "submodule", "add", "-q", childBare, "repos/child")
	git(seed, "commit", "-q", "-m", "umbrella")
	clone := filepath.Join(base, "clone")
	git(base, "clone", "-q", seed, clone)

	probe := filepath.Join(base, "probe")
	git(base, "clone", "-q", seed, probe)
	probeCmd := exec.Command("git", "submodule", "update", "--init", "--quiet")
	probeCmd.Dir = probe
	probeCmd.Env = os.Environ()
	if probeCmd.Run() == nil {
		t.Skip("this git allows submodules over the file transport by default (older than 2.38.1)")
	}

	if out, err := SubmoduleUpdateInit(context.Background(), clone, "--quiet", "--", "repos/child"); err != nil {
		t.Fatalf("SubmoduleUpdateInit: %v\n%s", err, out)
	}
	childDir := filepath.Join(clone, "repos", "child")
	if got := strings.TrimSpace(git(childDir, "rev-parse", "HEAD")); got != want {
		t.Fatalf("child at %s, want %s", got, want)
	}
	for _, dir := range []string{clone, childDir} {
		cmd := exec.Command("git", "config", "--show-origin", "--list")
		cmd.Dir = dir
		cmd.Env = os.Environ()
		out, _ := cmd.CombinedOutput()
		if strings.Contains(string(out), "protocol.file.allow") {
			t.Errorf("configuration of %s holds protocol.file.allow:\n%s", dir, out)
		}
	}
}
