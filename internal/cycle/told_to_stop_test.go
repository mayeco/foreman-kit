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
)

// A cycle runs inside the daemon, with its context: a daemon told to stop ends that context in the
// middle of whatever the cycle was doing. The tests of this file tell a command to stop at the worst
// moment, which a git hook marks, and check that no checkout is left halfway.

// stopTestGit makes git ignore the configuration of the machine and gives it an identity.
func stopTestGit(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "protocol.file.allow", "GIT_CONFIG_VALUE_0": "always",
	} {
		t.Setenv(name, value)
	}
}

func writeHook(t *testing.T, hooksDir, name, script string) {
	t.Helper()
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeRefHook has git write marker, and wait a second, the first time it reaches state for ref:
// "prepared" is when git has written the files of the commit a branch moves to and has not moved the
// branch yet, "committed" when it has just moved it.
func writeRefHook(t *testing.T, hooksDir, state, ref, marker string) {
	t.Helper()
	writeHook(t, hooksDir, "reference-transaction", "#!/bin/sh\n"+
		"state=\"$1\"\n"+
		"while read -r old new ref; do\n"+
		"\tif [ \"$state\" = '"+state+"' ] && [ \"$ref\" = '"+ref+"' ] && [ ! -e '"+marker+"' ]; then\n"+
		"\t\t: > '"+marker+"'\n"+
		"\t\tsleep 1\n"+
		"\tfi\n"+
		"done\n")
}

// toldToStopAt runs fn and ends its context when marker appears: the order to stop arrives while git
// stands at the point its hook marks. It returns what fn returned.
func toldToStopAt(t *testing.T, marker string, fn func(ctx context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("it ended (%v) before git reached the point where it is told to stop: the hook never ran", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("git never reached the point where it is told to stop")
		}
	}
	cancel()
	return <-done
}

func headOf(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(gitIn(t, dir, "rev-parse", "HEAD"))
}

// commitFile writes content as rel in the checkout at dir, commits it and pushes it to main.
func commitFile(t *testing.T, dir, rel, content, message string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "--", rel)
	gitIn(t, dir, "commit", "-q", "-m", message)
	gitIn(t, dir, "push", "-q", "origin", "HEAD:main")
}

// The sync of the main checkout rewrites it: it restores the backlog the merger wrote into it and
// moves it forward. Told to stop while git moves it, the files git had already written were left as
// local edits with the branch one commit behind, and a checkout with local edits is never moved
// again, neither by the sync nor by the dispatcher: the queue stopped following what was published.
// Told to stop between the restore and the move, it kept the backlog of the older commit. Once the
// sync begins to rewrite the checkout it ends, and one already told to stop begins nothing.
func TestSyncReposToldToStopLeavesTheMainCheckoutWhole(t *testing.T) {
	stopTestGit(t)
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", bare)
	root := filepath.Join(base, "root")
	gitIn(t, base, "clone", "-q", bare, root)
	commitFile(t, root, memRel, "# Memory\n- one\n", "memory: one")
	commitFile(t, root, BacklogFile, `[{"change_name":"x"}]`+"\n", "backlog: x")
	gitIn(t, root, "branch", "-q", "--set-upstream-to=origin/main")
	mate := filepath.Join(base, "mate")
	gitIn(t, base, "clone", "-q", bare, mate)
	hooks := filepath.Join(root, ".git", "hooks")

	whole := func(when, pulled string, out *bytes.Buffer, err error) {
		t.Helper()
		if err != nil || !strings.Contains(out.String(), pulled) {
			t.Fatalf("told to stop %s, the sync said (%v):\n%s\nwant %q", when, err, out.String(), pulled)
		}
		if dirty := gitIn(t, root, "status", "--porcelain"); dirty != "" {
			t.Fatalf("told to stop %s, the main checkout was left with edits nobody made:\n%s", when, dirty)
		}
		if at, want := headOf(t, root), headOf(t, mate); at != want {
			t.Fatalf("told to stop %s, the main checkout is at %s, and origin at %s", when, at, want)
		}
	}

	// Told to stop while git moves the checkout: the files are written, the branch is not moved yet.
	commitFile(t, mate, memRel, "# Memory\n- one\n- two\n", "memory: two")
	moving := filepath.Join(t.TempDir(), "moving")
	writeRefHook(t, hooks, "prepared", "refs/heads/main", moving)
	var out bytes.Buffer
	err := toldToStopAt(t, moving, func(ctx context.Context) error { return SyncRepos(ctx, root, nil, false, &out) })
	whole("while git moved it", "root: pulled 1 commit(s) [main]", &out, err)
	if err := os.Remove(filepath.Join(hooks, "reference-transaction")); err != nil {
		t.Fatal(err)
	}

	// Told to stop between the two steps: the main checkout holds, uncommitted, the backlog the merger
	// published. It is restored, and the move that brings it back must follow.
	published := `[{"change_name":"x"},{"change_name":"y"}]` + "\n"
	commitFile(t, mate, BacklogFile, published, "backlog: y")
	backlog := filepath.Join(root, filepath.FromSlash(BacklogFile))
	if err := os.WriteFile(backlog, []byte(published), 0o644); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	writeHook(t, hooks, "post-checkout", "#!/bin/sh\n: > '"+restored+"'\nsleep 1\n")
	out.Reset()
	err = toldToStopAt(t, restored, func(ctx context.Context) error { return SyncRepos(ctx, root, nil, false, &out) })
	whole("after it restored the backlog", "root: pulled 1 commit(s) [main]", &out, err)
	if b, _ := os.ReadFile(backlog); string(b) != published {
		t.Fatalf("the backlog of the main checkout was left as %q, want what was published", b)
	}
	if err := os.Remove(filepath.Join(hooks, "post-checkout")); err != nil {
		t.Fatal(err)
	}

	// Already told to stop, it begins nothing, and says so rather than "up to date".
	commitFile(t, mate, memRel, "# Memory\n- one\n- two\n- three\n", "memory: three")
	gitIn(t, root, "fetch", "-q", "origin")
	before := headOf(t, root)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	out.Reset()
	if err := SyncRepos(stopped, root, nil, false, &out); err == nil {
		t.Errorf("a sync told to stop before it began said it was done:\n%s", out.String())
	}
	if at := headOf(t, root); at != before || strings.Contains(out.String(), "root:") {
		t.Fatalf("a sync told to stop before it began moved the main checkout from %s to %s, or spoke of it:\n%s", before, at, out.String())
	}
}

// A child repository is moved forward the same way, and a child left with the files git had already
// written is skipped from then on for its uncommitted changes.
func TestSyncReposToldToStopLeavesAChildWhole(t *testing.T) {
	stopTestGit(t)
	base := t.TempDir()
	childBare := filepath.Join(base, "app.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", childBare)
	seed := filepath.Join(base, "seed")
	gitIn(t, base, "clone", "-q", childBare, seed)
	commitFile(t, seed, "app.txt", "one\n", "app: one")

	rootBare := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", rootBare)
	root := filepath.Join(base, "root")
	gitIn(t, base, "clone", "-q", rootBare, root)
	gitIn(t, root, "submodule", "--quiet", "add", "-b", "main", childBare, "repos/app")
	gitIn(t, root, "commit", "-q", "-m", "umbrella: add repos/app")
	gitIn(t, root, "push", "-q", "origin", "HEAD:main")
	gitIn(t, root, "branch", "-q", "--set-upstream-to=origin/main")
	child := filepath.Join(root, "repos", "app")

	commitFile(t, seed, "app.txt", "one\ntwo\n", "app: two")
	moving := filepath.Join(t.TempDir(), "moving")
	hooks := filepath.Join(strings.TrimSpace(gitIn(t, child, "rev-parse", "--absolute-git-dir")), "hooks")
	writeRefHook(t, hooks, "prepared", "refs/heads/main", moving)

	var out bytes.Buffer
	err := toldToStopAt(t, moving, func(ctx context.Context) error { return SyncRepos(ctx, root, nil, false, &out) })
	if err != nil || !strings.Contains(out.String(), "repos/app: pulled 1 commit(s)") {
		t.Fatalf("told to stop while git moved the child, the sync said (%v):\n%s", err, out.String())
	}
	if dirty := gitIn(t, child, "status", "--porcelain"); dirty != "" {
		t.Fatalf("the child was left with edits nobody made:\n%s", dirty)
	}
	if at, want := headOf(t, child), headOf(t, seed); at != want {
		t.Fatalf("the child is at %s, and its origin at %s", at, want)
	}
}

// When the base branch moved under a session, its record commit is made again on top of it: a reset
// onto the remote, the reconciled files, the commit. Told to stop once the reset had moved the branch,
// the record was in no commit HEAD reached, and the reset that puts it back was refused like every
// other command: what the cycle had queued and learned was left for nobody to publish. Once the
// rebase begins it ends, and one already told to stop begins nothing.
func TestRebaseProposalsToldToStopKeepsTheRecord(t *testing.T) {
	stopTestGit(t)
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", bare)
	session := filepath.Join(base, "session")
	gitIn(t, base, "clone", "-q", bare, session)
	commitFile(t, session, memRel, "# Memory\n- base lesson\n", "initial")
	gitIn(t, session, "branch", "-q", "--set-upstream-to=origin/main")
	mate := filepath.Join(base, "mate")
	gitIn(t, base, "clone", "-q", bare, mate)
	commitFile(t, mate, memRel, "# Memory\n- base lesson\n- lesson from the merger\n", "chore(memory): merge")

	const subject = "chore(memory): scheduled cycle 20261111T090000Z"
	memory := memPath(session)
	if err := os.WriteFile(memory, []byte("# Memory\n- base lesson\n- lesson from the cycle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, session, "commit", "-q", "-am", subject)
	record := headOf(t, session)

	// Already told to stop, it begins nothing: the record commit stays where it is.
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RebaseProposals(stopped, session, "main", io.Discard); err == nil {
		t.Error("a rebase told to stop before it began said it was done")
	}
	if at, dirty := headOf(t, session), gitIn(t, session, "status", "--porcelain"); at != record || dirty != "" {
		t.Fatalf("a rebase told to stop before it began left HEAD at %s (the record is %s) and these edits:\n%s", at, record, dirty)
	}

	// Told to stop when the reset has just moved the branch onto the remote.
	reset := filepath.Join(t.TempDir(), "reset")
	writeRefHook(t, filepath.Join(session, ".git", "hooks"), "committed", "refs/heads/main", reset)
	err := toldToStopAt(t, reset, func(ctx context.Context) error { return RebaseProposals(ctx, session, "main", io.Discard) })
	if err != nil {
		t.Fatalf("told to stop after the reset, the rebase failed: %v", err)
	}
	if got := strings.TrimSpace(gitIn(t, session, "log", "-1", "--format=%s")); got != subject {
		t.Fatalf("HEAD is %q, want the record commit %q made again", got, subject)
	}
	if parent, want := strings.TrimSpace(gitIn(t, session, "rev-parse", "HEAD~1")), headOf(t, mate); parent != want {
		t.Fatalf("the record commit sits on %s, want the commit origin has, %s", parent, want)
	}
	if b, _ := os.ReadFile(memory); !strings.Contains(string(b), "lesson from the cycle") || !strings.Contains(string(b), "lesson from the merger") {
		t.Fatalf("the memory was left as:\n%s\nwant the lesson of the cycle and the one origin had", b)
	}
	if dirty := gitIn(t, session, "status", "--porcelain"); dirty != "" {
		t.Fatalf("the session was left with edits in no commit:\n%s", dirty)
	}
}
