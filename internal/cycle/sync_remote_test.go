package cycle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

// remoteApp is an umbrella cloned with `-o upstream`, whose pipeline.yaml names that remote
// (repository.remote: upstream). A teammate then pushes chore(merge): integrate add-feature to it.
// withFork also gives the umbrella an `origin` remote: a fork that lags upstream by that commit.
type remoteApp struct {
	base, upstream, root string
	head                 string // the head of upstream's main once the teammate pushed
}

func newRemoteApp(t *testing.T, withFork bool) remoteApp {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	base := t.TempDir()
	upstream := filepath.Join(base, "upstream.git")
	gitIn(t, base, "init", "-q", "--bare", upstream)
	gitIn(t, upstream, "symbolic-ref", "HEAD", "refs/heads/main")

	mate := filepath.Join(base, "mate")
	gitIn(t, base, "clone", "-q", upstream, mate)
	gitIn(t, mate, "checkout", "-q", "-b", "main")
	settingstest.WriteFiles(t, mate, map[string]string{"pipeline": "repository:\n  base_branch: main\n  remote: upstream\n"})
	if err := os.WriteFile(filepath.Join(mate, ".gitignore"), []byte(".worktrees/\nlog/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, mate, "add", "-A")
	gitIn(t, mate, "commit", "-q", "-m", "init")
	gitIn(t, mate, "push", "-q", "origin", "HEAD:main")

	root := filepath.Join(base, "root")
	gitIn(t, base, "clone", "-q", "-o", "upstream", upstream, root)
	if withFork {
		fork := filepath.Join(base, "fork.git")
		gitIn(t, base, "clone", "-q", "--bare", upstream, fork)
		gitIn(t, root, "remote", "add", "origin", fork)
		gitIn(t, root, "fetch", "-q", "origin")
	}

	if err := os.WriteFile(filepath.Join(mate, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, mate, "add", "feature.txt")
	gitIn(t, mate, "commit", "-q", "-m", "chore(merge): integrate add-feature")
	gitIn(t, mate, "push", "-q", "origin", "HEAD:main")
	head := strings.TrimSpace(gitIn(t, base, "--git-dir", upstream, "rev-parse", "main"))
	return remoteApp{base: base, upstream: upstream, root: root, head: head}
}

// The cycle judges what the remote repository.remote names holds: its worktree is cut from
// <remote>/<base> after fetching that remote, with or without an `origin` that lags.
func TestCycleSyncsAndCutsItsWorktreeFromTheConfiguredRemote(t *testing.T) {
	for name, withFork := range map[string]bool{"no origin remote": false, "a lagging origin remote": true} {
		t.Run(name, func(t *testing.T) {
			app := newRemoteApp(t, withFork)
			st, err := store.OpenAt(filepath.Join(t.TempDir(), "state.db"), "ws-cycle-remote", app.root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			ctx := context.Background()

			var judgedAt, judgedIn string
			var sawFeature bool
			var out bytes.Buffer
			err = RunCycle(ctx, app.root, st, CycleOptions{
				Stamp:        "20261115T000000Z",
				SkipMerge:    true,
				SkipDispatch: true,
				AgentExecFunc: func(_ context.Context, workdir, _, _ string, _ time.Duration, _ io.Writer) error {
					judgedIn = workdir
					judgedAt = headOf(t, workdir)
					_, serr := os.Stat(filepath.Join(workdir, "feature.txt"))
					sawFeature = serr == nil
					return nil
				},
			}, &out)
			if err != nil {
				t.Fatalf("RunCycle: %v\n%s", err, out.String())
			}
			if judgedIn == "" {
				t.Fatalf("no judgment ran:\n%s", out.String())
			}
			if judgedAt != app.head || !sawFeature {
				t.Fatalf("the judgment ran at %s (sees the integrated file: %v), want upstream/main %s:\n%s", judgedAt, sawFeature, app.head, out.String())
			}
			c, err := st.GetCycle(ctx, "20261115T000000Z")
			if err != nil || c == nil || c.UmbrellaSHA != app.head {
				t.Fatalf("the cycle recorded umbrella_sha %+v (%v), want %s", c, err, app.head)
			}
			if got := headOf(t, app.root); got != app.head {
				t.Fatalf("the main checkout is at %s, want %s: the sync did not pull from upstream:\n%s", got, app.head, out.String())
			}
		})
	}
}

// foreman-kit sync pulls from repository.remote, whatever other remotes the umbrella has.
func TestSyncReposPullsFromTheConfiguredRemote(t *testing.T) {
	for name, withFork := range map[string]bool{"no origin remote": false, "a lagging origin remote": true} {
		t.Run(name, func(t *testing.T) {
			app := newRemoteApp(t, withFork)
			var out bytes.Buffer
			if err := SyncRepos(context.Background(), app.root, nil, false, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "root: pulled 1 commit(s) [main]") {
				t.Fatalf("sync did not pull the teammate's commit:\n%s", out.String())
			}
			if got := headOf(t, app.root); got != app.head {
				t.Fatalf("the main checkout is at %s, want %s", got, app.head)
			}
			out.Reset()
			if err := SyncRepos(context.Background(), app.root, nil, false, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "root: up to date with upstream [main]") {
				t.Fatalf("a second sync does not name the remote it follows:\n%s", out.String())
			}
		})
	}
}

// foreman-kit sync --publish fetches a child from, and pushes the child's commits and the
// umbrella's new pointer to, repository.remote: the merger and the dispatcher use that name for the
// children too.
func TestSyncPublishPushesToTheConfiguredRemote(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	base := t.TempDir()
	childBare := filepath.Join(base, "app.git")
	gitIn(t, base, "init", "-q", "--bare", childBare)
	gitIn(t, childBare, "symbolic-ref", "HEAD", "refs/heads/main")
	seed := filepath.Join(base, "seed")
	gitIn(t, base, "clone", "-q", childBare, seed)
	gitIn(t, seed, "checkout", "-q", "-b", "main")
	gitIn(t, seed, "commit", "-q", "--allow-empty", "-m", "app: first")
	gitIn(t, seed, "push", "-q", "origin", "HEAD:main")

	rootBare := filepath.Join(base, "upstream.git")
	gitIn(t, base, "init", "-q", "--bare", rootBare)
	gitIn(t, rootBare, "symbolic-ref", "HEAD", "refs/heads/main")
	root := filepath.Join(base, "root")
	gitIn(t, base, "clone", "-q", "-o", "upstream", rootBare, root)
	gitIn(t, root, "checkout", "-q", "-b", "main")
	settingstest.WriteFiles(t, root, map[string]string{"pipeline": "repository:\n  base_branch: main\n  remote: upstream\n"})
	gitIn(t, root, "submodule", "--quiet", "add", "-b", "main", childBare, "repos/app")
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "umbrella: add repos/app")
	gitIn(t, root, "push", "-q", "-u", "upstream", "HEAD:main")

	child := filepath.Join(root, "repos", "app")
	// A child of an application whose remote is called upstream has no origin.
	gitIn(t, child, "remote", "rename", "origin", "upstream")
	gitIn(t, child, "fetch", "-q", "upstream")
	gitIn(t, child, "checkout", "-q", "-B", "main", "--track", "upstream/main")
	gitIn(t, child, "commit", "-q", "--allow-empty", "-m", "app: second")
	second := headOf(t, child)

	var out bytes.Buffer
	if err := SyncRepos(context.Background(), root, nil, true, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "repos/app: pushed 1 commit(s)") ||
		!strings.Contains(out.String(), "root: recorded pointers for repos/app") {
		t.Fatalf("the child's commit was not pushed and recorded:\n%s", out.String())
	}
	if got := strings.TrimSpace(gitIn(t, base, "--git-dir", childBare, "rev-parse", "main")); got != second {
		t.Fatalf("the child's remote holds %s, want %s", got, second)
	}
	if entry := strings.Fields(gitIn(t, base, "--git-dir", rootBare, "ls-tree", "main", "repos/app")); len(entry) < 3 || entry[2] != second {
		t.Fatalf("upstream records repos/app as %q, want the pushed commit %s", entry, second)
	}
}

// The visual session's worktree is cut from <repository.remote>/<base> too.
func TestVisualSessionWorktreeIsCutFromTheConfiguredRemote(t *testing.T) {
	for name, withFork := range map[string]bool{"no origin remote": false, "a lagging origin remote": true} {
		t.Run(name, func(t *testing.T) {
			app := newRemoteApp(t, withFork)
			st, err := store.OpenAt(filepath.Join(t.TempDir(), "state.db"), "ws-visual-remote", app.root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			ctx := context.Background()
			var out bytes.Buffer
			err = VisualQABegin(ctx, app.root, st, VisualQAOptions{
				Stamp:              "20261115T010000Z",
				ServiceCheckFunc:   func(context.Context, string) error { return nil },
				ServicePrepareFunc: func(context.Context, string, int) error { return nil },
			}, &out)
			if err != nil {
				t.Fatalf("VisualQABegin: %v\n%s", err, out.String())
			}
			t.Cleanup(func() { _ = st.ReleaseLock(ctx, "visual_qa", 0) })
			wt := filepath.Join(app.root, ".worktrees", "_visual_qa")
			if got := headOf(t, wt); got != app.head {
				t.Fatalf("the visual worktree is at %s, want upstream/main %s:\n%s", got, app.head, out.String())
			}
			if _, err := os.Stat(filepath.Join(wt, "feature.txt")); err != nil {
				t.Fatalf("the visual worktree does not hold the integrated file: %v", err)
			}
		})
	}
}
