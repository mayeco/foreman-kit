package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ffGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func ffWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ffSetup returns a main checkout and a teammate's clone of the same origin, both on main.
func ffSetup(t *testing.T) (main, mate string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	ffGit(t, base, "init", "-q", "--bare", "-b", "main", bare)
	main = filepath.Join(base, "main")
	ffGit(t, base, "clone", "-q", bare, main)
	ffWrite(t, filepath.Join(main, ".foreman-kit", "agent_backlog.json"), "[]\n")
	ffGit(t, main, "add", "-A")
	ffGit(t, main, "commit", "-qm", "init")
	ffGit(t, main, "push", "-q", "-u", "origin", "main")
	mate = filepath.Join(base, "mate")
	ffGit(t, base, "clone", "-q", bare, mate)
	return main, mate
}

func pushBacklog(t *testing.T, mate, content string) {
	t.Helper()
	ffWrite(t, filepath.Join(mate, ".foreman-kit", "agent_backlog.json"), content)
	ffGit(t, mate, "commit", "-qam", "chore(backlog): queue an item")
	ffGit(t, mate, "push", "-q", "origin", "main")
}

var restorable = []string{".foreman-kit/agent_backlog.json", ".foreman-kit/agent_history.json"}

func TestFastForwardCheckoutPullsATeammatesBacklog(t *testing.T) {
	main, mate := ffSetup(t)
	pushBacklog(t, mate, `[{"change_name":"x"}]`+"\n")
	ctx := context.Background()
	ffGit(t, main, "fetch", "-q", "origin")
	res := FastForwardCheckout(ctx, main, "origin", "main", restorable)
	if res.Pulled != 1 || res.Skipped != "" {
		t.Fatalf("result = %+v, want 1 commit pulled", res)
	}
	if b, _ := os.ReadFile(filepath.Join(main, ".foreman-kit", "agent_backlog.json")); !strings.Contains(string(b), `"x"`) {
		t.Fatalf("backlog = %s", b)
	}
	if res := FastForwardCheckout(ctx, main, "origin", "main", restorable); res.Pulled != 0 || res.Skipped != "" {
		t.Fatalf("second call = %+v, want nothing to do", res)
	}
}

// TestFastForwardCheckoutRestoresAPublishedBacklog: the merger writes into the main checkout the
// backlog it just published; that local copy must not stop the fast-forward.
func TestFastForwardCheckoutRestoresAPublishedBacklog(t *testing.T) {
	main, mate := ffSetup(t)
	pushBacklog(t, mate, `[{"change_name":"x"}]`+"\n")
	pushBacklog(t, mate, `[{"change_name":"x"},{"change_name":"y"}]`+"\n")
	ffWrite(t, filepath.Join(main, ".foreman-kit", "agent_backlog.json"), `[{"change_name":"x"}]`+"\n")
	ffGit(t, main, "fetch", "-q", "origin")
	res := FastForwardCheckout(context.Background(), main, "origin", "main", restorable)
	if res.Pulled != 2 {
		t.Fatalf("result = %+v, want 2 commits pulled", res)
	}
	if b, _ := os.ReadFile(filepath.Join(main, ".foreman-kit", "agent_backlog.json")); !strings.Contains(string(b), `"y"`) {
		t.Fatalf("backlog = %s", b)
	}
}

func TestFastForwardCheckoutLeavesLocalWorkAlone(t *testing.T) {
	ctx := context.Background()

	main, mate := ffSetup(t)
	pushBacklog(t, mate, `[{"change_name":"x"}]`+"\n")
	ffWrite(t, filepath.Join(main, ".foreman-kit", "agent_backlog.json"), `[{"change_name":"mine"}]`+"\n")
	ffGit(t, main, "fetch", "-q", "origin")
	if res := FastForwardCheckout(ctx, main, "origin", "main", restorable); res.Pulled != 0 || !strings.Contains(res.Skipped, "unpublished edits in .foreman-kit/agent_backlog.json") {
		t.Fatalf("unpublished backlog edit: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(main, ".foreman-kit", "agent_backlog.json")); !strings.Contains(string(b), "mine") {
		t.Fatal("the local backlog edit was lost")
	}

	main, mate = ffSetup(t)
	pushBacklog(t, mate, `[{"change_name":"x"}]`+"\n")
	ffWrite(t, filepath.Join(main, "README.md"), "draft\n")
	ffGit(t, main, "fetch", "-q", "origin")
	if res := FastForwardCheckout(ctx, main, "origin", "main", restorable); res.Pulled != 0 || res.Skipped != "it has local edits" {
		t.Fatalf("untracked file: %+v", res)
	}

	main, mate = ffSetup(t)
	pushBacklog(t, mate, `[{"change_name":"x"}]`+"\n")
	ffGit(t, main, "commit", "-q", "--allow-empty", "-m", "local")
	ffGit(t, main, "fetch", "-q", "origin")
	if res := FastForwardCheckout(ctx, main, "origin", "main", restorable); res.Pulled != 0 || !strings.Contains(res.Skipped, "commits not on origin/main") {
		t.Fatalf("local commit: %+v", res)
	}

	main, mate = ffSetup(t)
	pushBacklog(t, mate, `[{"change_name":"x"}]`+"\n")
	ffGit(t, main, "switch", "-q", "-c", "topic")
	ffGit(t, main, "fetch", "-q", "origin")
	if res := FastForwardCheckout(ctx, main, "origin", "main", restorable); res.Pulled != 0 || res.Skipped != "not on main" {
		t.Fatalf("other branch: %+v", res)
	}
}

// TestFastForwardCheckoutIsNotStoppedHalfway: the fast-forward restores the files the merger wrote
// into the checkout and then moves it, two commands that rewrite the working tree. A process told to
// stop between them, or while git writes a file, must not leave the checkout with the older backlog
// or with half of one: the daemon stops in the middle of whatever its dispatcher was doing, and the
// backlog of the main checkout is the queue. Once the fast-forward begins, it ends.
func TestFastForwardCheckoutIsNotStoppedHalfway(t *testing.T) {
	main, mate := ffSetup(t)
	pushBacklog(t, mate, `[{"change_name":"x"}]`+"\n")
	pushBacklog(t, mate, `[{"change_name":"x"},{"change_name":"y"}]`+"\n")
	backlog := filepath.Join(main, ".foreman-kit", "agent_backlog.json")
	ffWrite(t, backlog, `[{"change_name":"x"}]`+"\n")
	ffGit(t, main, "fetch", "-q", "origin")

	// git runs this hook once the restore has written the file: the order to stop arrives there,
	// while git is still at work in the checkout.
	restored := filepath.Join(t.TempDir(), "restored")
	hook := filepath.Join(main, ".git", "hooks", "post-checkout")
	ffWrite(t, hook, "#!/bin/sh\n: > '"+restored+"'\nsleep 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan FastForwardResult, 1)
	go func() { done <- FastForwardCheckout(ctx, main, "origin", "main", restorable) }()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(restored); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the restore never ran its hook")
		}
	}
	cancel()

	if res := <-done; res.Pulled != 2 || res.Skipped != "" {
		t.Fatalf("stopped while it restored the backlog: %+v, want the 2 commits pulled", res)
	}
	if b, _ := os.ReadFile(backlog); !strings.Contains(string(b), `"y"`) {
		t.Fatalf("the backlog was left as %q", b)
	}
	if dirty := ffGit(t, main, "status", "--porcelain"); dirty != "" {
		t.Fatalf("the checkout was left with edits:\n%s", dirty)
	}

	// Told to stop before it began, it touches nothing.
	pushBacklog(t, mate, `[{"change_name":"x"},{"change_name":"y"},{"change_name":"z"}]`+"\n")
	ffGit(t, main, "fetch", "-q", "origin")
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if res := FastForwardCheckout(ctx, main, "origin", "main", restorable); res.Pulled != 0 {
		t.Fatalf("already told to stop: %+v, want nothing done", res)
	}
	if b, _ := os.ReadFile(backlog); strings.Contains(string(b), `"z"`) {
		t.Fatalf("a checkout told to stop moved anyway: %q", b)
	}
}

// TestRestorePublishedTellsAMissingFileFromAGitThatCouldNotSay: a file HEAD does not have is removed
// from the checkout, so that the fast-forward can bring it. A git that was stopped, or that failed,
// has not said that: the file stays. Taking one for the other removed the backlog and the history of
// a main checkout whose daemon was told to stop while its dispatcher fast-forwarded it, and the
// checkout, left with two deleted files, was never moved again.
func TestRestorePublishedTellsAMissingFileFromAGitThatCouldNotSay(t *testing.T) {
	main, mate := ffSetup(t)
	pushBacklog(t, mate, `[{"change_name":"x"}]`+"\n")
	backlog := filepath.Join(main, ".foreman-kit", "agent_backlog.json")
	ffWrite(t, backlog, `[{"change_name":"x"}]`+"\n")
	ffGit(t, main, "fetch", "-q", "origin")

	// Told to stop before it asks: nothing is touched, and the caller is told.
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RestorePublished(stopped, main, ".foreman-kit/agent_backlog.json"); err == nil {
		t.Error("a restore told to stop said it was done")
	}
	if b, err := os.ReadFile(backlog); err != nil || !strings.Contains(string(b), `"x"`) {
		t.Fatalf("the backlog of a checkout told to stop: %q (%v)", b, err)
	}
	if res := FastForwardCheckout(stopped, main, "origin", "main", restorable); res.Pulled != 0 {
		t.Errorf("a fast-forward told to stop: %+v", res)
	}
	if b, err := os.ReadFile(backlog); err != nil || !strings.Contains(string(b), `"x"`) {
		t.Fatalf("the backlog after a fast-forward told to stop: %q (%v)", b, err)
	}

	// A git that cannot answer (here, a directory that is no repository) has not said the file is
	// missing either.
	plain := t.TempDir()
	stray := filepath.Join(plain, ".foreman-kit", "agent_backlog.json")
	ffWrite(t, stray, "[]\n")
	if err := RestorePublished(context.Background(), plain, ".foreman-kit/agent_backlog.json"); err == nil {
		t.Error("a restore where git cannot answer said it was done")
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("the file was removed although git never said HEAD lacks it: %v", err)
	}

	// A file HEAD really does not have is removed, as before: the fast-forward brings it.
	rotated := filepath.Join(main, ".foreman-kit", "agent_history.2026.json")
	ffWrite(t, rotated, "[]\n")
	if err := RestorePublished(context.Background(), main, ".foreman-kit/agent_history.2026.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rotated); !os.IsNotExist(err) {
		t.Fatalf("a file HEAD does not have was left in the checkout (%v)", err)
	}
	// And one HEAD has is put back as HEAD has it.
	ffWrite(t, backlog, `[{"change_name":"x"}]`+"\n")
	if err := RestorePublished(context.Background(), main, ".foreman-kit/agent_backlog.json"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(backlog); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("the backlog was not put back as HEAD has it: %q", b)
	}
}
