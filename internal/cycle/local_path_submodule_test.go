package cycle

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// localPathChildEnv gives the test a git that knows nothing but its defaults: an empty global and
// system configuration and no GIT_CONFIG_* variable, the way a person's shell or the scheduled cycle
// runs. The simulator hides the defect these tests guard against by exporting GIT_CONFIG_KEY_0.
func localPathChildEnv(t *testing.T) {
	t.Helper()
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
}

// localPathChildApp is an umbrella whose only child repository is recorded with the path of a local
// bare repository as its URL. clone is a fresh clone of the umbrella with the child not initialized.
type localPathChildApp struct {
	childBare string
	clone     string
	gitlink   string // the commit of the child the umbrella records
}

func newLocalPathChildApp(t *testing.T) localPathChildApp {
	t.Helper()
	localPathChildEnv(t)
	base := t.TempDir()

	childBare := filepath.Join(base, "child.git")
	gitIn(t, base, "init", "-q", "--bare", childBare)
	gitIn(t, childBare, "symbolic-ref", "HEAD", "refs/heads/main")
	childWork := filepath.Join(base, "child-work")
	gitIn(t, base, "clone", "-q", childBare, childWork)
	gitIn(t, childWork, "checkout", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(childWork, "app.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, childWork, "add", "app.txt")
	gitIn(t, childWork, "commit", "-q", "-m", "child one")
	gitIn(t, childWork, "push", "-q", "origin", "HEAD:main")
	gitlink := strings.TrimSpace(gitIn(t, childWork, "rev-parse", "HEAD"))

	umbrellaBare := filepath.Join(base, "umbrella.git")
	gitIn(t, base, "init", "-q", "--bare", umbrellaBare)
	gitIn(t, umbrellaBare, "symbolic-ref", "HEAD", "refs/heads/master")
	seed := filepath.Join(base, "umbrella-seed")
	gitIn(t, base, "clone", "-q", umbrellaBare, seed)
	gitIn(t, seed, "checkout", "-q", "-b", "master")
	if err := os.WriteFile(filepath.Join(seed, ".gitignore"), []byte(".worktrees/\nlog/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	settingstest.WriteFiles(t, seed, nil)
	gitIn(t, seed, "add", "-A")
	// The option is given to this one command, which builds the fixture: the code under test gets none.
	gitIn(t, seed, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "-b", "main", childBare, "repos/child")
	gitIn(t, seed, "commit", "-q", "-m", "umbrella with a local-path child")
	gitIn(t, seed, "push", "-q", "origin", "HEAD:master")

	clone := filepath.Join(base, "umbrella")
	gitIn(t, base, "clone", "-q", umbrellaBare, clone)
	if got := strings.TrimSpace(gitIn(t, clone, "ls-tree", "HEAD", "repos/child")); !strings.Contains(got, gitlink) {
		t.Fatalf("the umbrella records %q, want the child at %s", got, gitlink)
	}

	// The fixture proves nothing on a git that does not refuse the file transport by default.
	probe := filepath.Join(base, "probe")
	gitIn(t, base, "clone", "-q", umbrellaBare, probe)
	cmd := exec.Command("git", "submodule", "update", "--init", "--quiet")
	cmd.Dir = probe
	cmd.Env = os.Environ()
	if err := cmd.Run(); err == nil {
		t.Skip("this git allows submodules over the file transport by default (older than 2.38.1)")
	}
	return localPathChildApp{childBare: childBare, clone: clone, gitlink: gitlink}
}

func requireNoFileTransportInConfig(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		cmd := exec.Command("git", "config", "--show-origin", "--list")
		cmd.Dir = dir
		cmd.Env = os.Environ()
		out, _ := cmd.CombinedOutput()
		if strings.Contains(string(out), "protocol.file.allow") {
			t.Errorf("git configuration of %s holds protocol.file.allow:\n%s", dir, out)
		}
	}
}

// The cycle and `foreman-kit visual-qa begin` prepare their session worktree with
// PrepareSessionWorktree: a child recorded by the path of a local bare repository must come out
// checked out at the umbrella's gitlink, with no git configuration but git's defaults.
func TestPrepareSessionWorktreeInitializesLocalPathChildren(t *testing.T) {
	app := newLocalPathChildApp(t)
	wt := filepath.Join(app.clone, ".worktrees", "_visual_qa")
	baseRef := strings.TrimSpace(gitIn(t, app.clone, "rev-parse", "HEAD"))

	if err := PrepareSessionWorktree(context.Background(), app.clone, wt, baseRef, "master", io.Discard); err != nil {
		t.Fatalf("PrepareSessionWorktree over a local-path child: %v", err)
	}
	childDir := filepath.Join(wt, "repos", "child")
	if got := strings.TrimSpace(gitIn(t, childDir, "rev-parse", "HEAD")); got != app.gitlink {
		t.Fatalf("child in the session worktree is at %s, want the gitlink %s", got, app.gitlink)
	}
	if b, err := os.ReadFile(filepath.Join(childDir, "app.txt")); err != nil || string(b) != "one\n" {
		t.Fatalf("child files were not checked out: %q, %v", b, err)
	}
	requireNoFileTransportInConfig(t, app.clone, wt, childDir)
}

// `foreman-kit sync` initializes a child that is not initialized in the checkout, and says so.
func TestSyncReposInitializesLocalPathChildren(t *testing.T) {
	app := newLocalPathChildApp(t)
	var out bytes.Buffer
	if err := SyncRepos(context.Background(), app.clone, nil, false, &out); err != nil {
		t.Fatalf("SyncRepos: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "repos/child: at recorded pointer") {
		t.Fatalf("sync did not initialize the child at its recorded pointer:\n%s", out.String())
	}
	if strings.Contains(out.String(), "ATTENTION") {
		t.Fatalf("sync flagged the child:\n%s", out.String())
	}
	childDir := filepath.Join(app.clone, "repos", "child")
	if got := strings.TrimSpace(gitIn(t, childDir, "rev-parse", "HEAD")); got != app.gitlink {
		t.Fatalf("child is at %s, want the gitlink %s", got, app.gitlink)
	}
	requireNoFileTransportInConfig(t, app.clone, childDir)
}
