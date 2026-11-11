package proc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestAgentExecStoppedByItsCallerIsNotAnEngineOutage: an engine its caller stops ends with a non-zero
// status without having changed anything, which AgentExec reported as "no engine could work" (exit 75):
// the cycle and the portal then treated a run they had stopped themselves as an outage of the engines.
func TestAgentExecStoppedByItsCallerIsNotAnEngineOutage(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	settingstest.Give(t, settings.AgentRetries, "0")
	// The work tree is clean, as a worktree is when an agent starts: under an ended context git cannot
	// be run, and the state AgentExec then reads is the one of a tree nothing was changed in. Only the
	// state database of the test workspace lives in the tree, which a real workspace's does not.
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte("state.db*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirty, err := gitx.StatusPorcelain(context.Background(), root, true); err != nil || strings.TrimSpace(dirty) != "" {
		t.Fatalf("the workspace is not clean (err %v):\n%s", err, dirty)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	marker := filepath.Join(binDir, "started")
	engine := "#!/usr/bin/env bash\ntouch \"" + marker + "\"\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(engine), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var out, errOut bytes.Buffer
	done := make(chan *AgentExecResult, 1)
	go func() {
		res, _ := AgentExec(ctx, AgentExecOptions{
			Workdir:   root,
			AppRoot:   root,
			Prompt:    "do the work",
			Engines:   "claude",
			Store:     st,
			SkipEvent: true,
			Stdout:    &out,
			Stderr:    &errOut,
		})
		done <- res
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the engine did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()

	var res *AgentExecResult
	select {
	case res = <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("AgentExec did not return after its context ended")
	}
	if res == nil || res.ExitCode == 0 {
		t.Fatalf("expected a stopped agent call, got %+v", res)
	}
	if res.ExitCode == ExitEnginesDown || strings.Contains(res.Output, "no engine could work") {
		t.Fatalf("a run its caller stopped was reported as an engine outage (exit %d): %q", res.ExitCode, res.Output)
	}
}
