package portal

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// A reset deletes a change's worktree and branch and then makes the change dispatchable again. It
// ran every step under the request context: when the client went away half way, the worktree was gone
// but the branch, its ready_manifests row and the quarantine record stayed, because git commands and store
// writes fail under an ended context, and the reset still answered ok.
func TestResetWorkerFinishesAfterItsClientDisconnected(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(realGit, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	root := t.TempDir()
	git(root, "init", "-q", "-b", "main")
	git(root, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(root, ".worktrees", "worker-demo")
	git(root, "worktree", "add", "-q", "-b", "feat/spec-demo", wt, "HEAD")

	st, err := store.OpenAt(filepath.Join(root, "log", "openspec.db"), "ws-reset-disconnect", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	bg := context.Background()
	if err := st.EnqueueReady(bg, store.ReadyManifest{ChangeName: "demo", Branch: "feat/spec-demo", Worktree: wt, HeadSHA: "0000000"}); err != nil {
		t.Fatal(err)
	}
	if err := st.BlockChange(bg, store.QuarantineRecord{ChangeName: "demo", Reason: "gate_failure", Details: "tests failed", Worktree: wt, Branch: "feat/spec-demo"}); err != nil {
		t.Fatal(err)
	}

	// A git that takes a moment to remove a worktree: the client goes away meanwhile.
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "removing")
	slowGit := "#!/bin/sh\ncase \"$*\" in *\"worktree remove\"*) touch \"" + marker + "\"; sleep 1;; esac\nexec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(slowGit), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv, _ := newTestServer(t, root, st)
	ctx, disconnect := context.WithCancel(bg)
	defer disconnect()
	done := make(chan error, 1)
	go func() {
		_, err := srv.changes.Reset(ctx, "demo")
		done <- err
	}()
	deadline := time.Now().Add(30 * time.Second)
	for !isFile(marker) {
		if time.Now().After(deadline) {
			t.Fatal("the reset never reached the removal of the worktree")
		}
		time.Sleep(5 * time.Millisecond)
	}
	disconnect()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Reset: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Reset did not return")
	}

	if isDir(wt) {
		t.Errorf("the worktree %s is still there", wt)
	}
	if err := exec.Command(realGit, "-C", root, "rev-parse", "--verify", "--quiet", "refs/heads/feat/spec-demo").Run(); err == nil {
		t.Errorf("the reset answered ok but left the branch feat/spec-demo")
	}
	if ready, err := st.ListReadyQueue(bg); err != nil || len(ready) != 0 {
		t.Errorf("the reset answered ok but left the change in the merge queue: %+v (err %v)", ready, err)
	}
	if q, err := st.GetQuarantine(bg, "demo"); err != nil || q != nil {
		t.Errorf("the reset answered ok but left the quarantine record: %+v (err %v)", q, err)
	}
}
