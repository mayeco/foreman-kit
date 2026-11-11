package portal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// writeExecutable writes a POSIX shell script to dir/name and returns its path.
func writeExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKitOperationsRunKitSubcommandsInTheirDirectory(t *testing.T) {
	_, st, root := setupTestPortal(t)
	exe := writeExecutable(t, t.TempDir(), "foreman-kit", `echo "kit $* in $(pwd -P)"`)
	kit := NewKit(st, exe)
	ctx := context.Background()

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	// At the root the gates are told to check every repository: nothing there differs from HEAD,
	// which is what they check when they are told nothing.
	for kind, command := range map[string]string{"dispatch": "dispatch", "merge": "merge", "gates": "gates --all"} {
		out, code, err := kit.RunOperation(ctx, root, kind)
		if err != nil || code != 0 || !strings.Contains(out, "kit "+command+" in "+realRoot) {
			t.Fatalf("job %s: expected `kit %s` to run in %s, got out=%q code=%d err=%v", kind, command, realRoot, out, code, err)
		}
	}

	wt := filepath.Join(root, ".worktrees", "worker-demo")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	realWT, _ := filepath.EvalSymlinks(wt)
	out, code, err := kit.RunOperation(ctx, wt, "gates-demo")
	if err != nil || code != 0 || !strings.Contains(out, "kit gates in "+realWT) {
		t.Fatalf("gates-demo: expected `kit gates` in the worktree %s, got out=%q code=%d err=%v", realWT, out, code, err)
	}

	// An operation the kit does not have is refused.
	if _, code, err := kit.RunOperation(ctx, root, "no-such-kind"); err == nil || code == 0 {
		t.Fatalf("expected an operation the kit does not have to be refused, got code=%d err=%v", code, err)
	}
}

func TestKitRunsAndPingsTheEngine(t *testing.T) {
	_, st, root := setupTestPortal(t)
	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", `echo "engine claude ran"`)
	writeExecutable(t, binDir, "hermes", `echo "hermes is broken" >&2; exit 1`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	kit := NewKit(st, "unused")
	ctx := context.Background()

	out, code, err := kit.RunAgent(ctx, root, "claude", "hello from the portal")
	if err != nil || code != 0 || !strings.Contains(out, "engine claude ran") {
		t.Fatalf("expected the claude engine to run, got out=%q code=%d err=%v", out, code, err)
	}

	// Several engines, as the portal gives them for "auto": the kit goes on to the next one when
	// one cannot run at all.
	out, code, err = kit.RunAgent(ctx, root, "codex,claude", "hello again")
	if err != nil || code != 0 || !strings.Contains(out, "engine claude ran") {
		t.Fatalf("expected the kit to run claude after codex, which is not installed; got out=%q code=%d err=%v", out, code, err)
	}

	if res := kit.PingEngine(ctx, root, "claude"); res.Status != "ok" {
		t.Fatalf("expected ping of a working engine to be ok, got %+v", res)
	}
	if res := kit.PingEngine(ctx, root, "hermes"); res.Status != "failed" {
		t.Fatalf("expected ping of a failing engine to report failed, got %+v", res)
	}
	if res := kit.PingEngine(ctx, root, "codex"); res.Status != "not_installed" {
		t.Fatalf("expected ping of a missing engine to report not_installed, got %+v", res)
	}
}

// TestTheKitRecordsOutputWhileTheWorkRuns: with an execution in the scope of the call, what an
// operation or an agent prints is the output of that execution as soon as it is printed. Whoever
// follows the execution reads it while the work goes on; before, it was recorded when the work ended.
func TestTheKitRecordsOutputWhileTheWorkRuns(t *testing.T) {
	_, st, root := setupTestPortal(t)
	ctx := context.Background()
	recorded := func(id, want string) string {
		t.Helper()
		var out string
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if out, _ = st.ReadFullLog(ctx, id); strings.Contains(out, want) {
				return out
			}
		}
		t.Fatalf("%q is not on record for %s while it runs; on record: %q", want, id, out)
		return ""
	}

	// An operation: it prints, then waits to be told to end.
	gate := filepath.Join(t.TempDir(), "go-on")
	exe := writeExecutable(t, t.TempDir(), "foreman-kit", `echo "first line of $1"; while [ ! -e "`+gate+`" ]; do sleep 0.05; done; echo "last line of $1"`)
	kit := NewKit(st, exe)
	if err := st.CreateExecution(ctx, store.Execution{ExecID: "op-live", Kind: "portal_job", JobKind: "merge", CommandText: "foreman-kit merge"}); err != nil {
		t.Fatal(err)
	}
	type result struct {
		out  string
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		out, code, err := kit.RunOperation(store.WithExecution(ctx, "op-live"), root, "merge")
		done <- result{out, code, err}
	}()
	recorded("op-live", "first line of merge")
	select {
	case r := <-done:
		t.Fatalf("the operation ended before it was told to: %+v", r)
	default:
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.code != 0 || r.out != "" {
		t.Fatalf("an operation under an execution returns what it did not record, which is nothing: %+v", r)
	}
	if out := recorded("op-live", "last line of merge"); strings.Count(out, "first line of merge") != 1 {
		t.Fatalf("the output is on record more than once: %q", out)
	}

	// An agent: what the engine prints, and what the kit has to say about running it.
	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", `echo "claude is thinking"`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	if err := st.CreateExecution(ctx, store.Execution{ExecID: "run-live", Kind: "portal_run", EnginesRequested: "claude", PromptText: "think"}); err != nil {
		t.Fatal(err)
	}
	out, code, err := NewKit(st, "unused").RunAgent(store.WithExecution(ctx, "run-live"), root, "claude", "think")
	if err != nil || code != 0 {
		t.Fatalf("the agent exited with %d (%v)", code, err)
	}
	log := recorded("run-live", "claude is thinking")
	if strings.Count(log, "claude is thinking") != 1 || strings.Contains(out, "claude is thinking") {
		t.Fatalf("what the engine printed is on record %d times and returned as %q; want it once, on record", strings.Count(log, "claude is thinking"), out)
	}
	if !strings.Contains(log, "agent_exec:") {
		t.Errorf("what the kit said about the run (how the engine exited) is not in the output: %q", log)
	}
	attempts, err := st.ListAttempts(ctx, "run-live")
	if err != nil || len(attempts) == 0 || attempts[0].Engine != "claude" {
		t.Errorf("the tries of the engine are not on record under the execution: %+v, %v", attempts, err)
	}
}
