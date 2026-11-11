package orchestrator

import (
	"context"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGateStoppedByItsDeadlineIsRecorded: the default gate runner recorded each gate run under the context
// that carries the deadline of the gates. The gate that deadline stopped, the one an operator most needs
// to see in gate_runs, was written under an ended context, which fails without running.
func TestGateStoppedByItsDeadlineIsRecorded(t *testing.T) {
	sb := newMCFSandboxWith(t, mcfOptions{coreTestFailsWhen: "[ -e SLOW ] && sleep 30"})
	ctx := context.Background()

	sb.buildCoreChangeWriting(t, "add-a", map[string]string{"a.txt": "a\n"})
	ready, err := sb.st.ListReadyQueue(ctx)
	if err != nil || len(ready) != 1 {
		t.Fatalf("the merge queue = %+v (err %v), want add-a alone", ready, err)
	}
	before, err := sb.st.ListGateRuns(ctx, "add-a")
	if err != nil {
		t.Fatal(err)
	}

	// The next run of core's test gate outlives the deadline of the gates.
	mustWriteFile(t, filepath.Join(ready[0].Worktree, "repos", "core", "SLOW"), "")
	started := time.Now()
	res, err := DefaultGateRunner(sb.st)(ctx, GateRunRequest{
		Change:   "add-a",
		Worktree: ready[0].Worktree,
		BaseRef:  "origin/master",
		Slot:     1,
		Timeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatalf("a gate that outlived its deadline passed: %+v", res)
	}
	if took := time.Since(started); took > 25*time.Second {
		t.Fatalf("the gate was not stopped at its deadline: the run took %s", took.Round(time.Second))
	}

	after, err := sb.st.ListGateRuns(ctx, "add-a")
	if err != nil {
		t.Fatal(err)
	}
	stopped := 0
	for _, run := range after {
		if run.RepoName == "core" && run.GateName == "test" && run.ExitCode != 0 {
			stopped++
		}
	}
	if stopped != 1 {
		t.Fatalf("gate_runs holds %d failed run(s) of core:test after the deadline stopped it, want 1 (%d rows before, %d after)", stopped, len(before), len(after))
	}
}

// TestWorkerExecutionOfAStoppedAgentIsFinished: the default agent runner wrote the outcome of an agent
// under the worker's context. When that context ended (a daemon stop, a lost lease) the write failed
// without running and the execution stayed "running" under the dispatcher's live PID.
func TestWorkerExecutionOfAStoppedAgentIsFinished(t *testing.T) {
	umbrella, _, _, st := setupTestWorkspaceWithSubmodule(t)
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "started")
	engine := "#!/bin/sh\ntouch \"" + marker + "\"\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(engine), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	settingstest.Give(t, settings.AgentRetries, "0")

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan AgentRunResult, 1)
	go func() {
		res, _ := DefaultAgentRunner(st)(ctx, AgentRunRequest{
			Change:   "add-a",
			Phase:    "build",
			Role:     "build",
			Engine:   "claude",
			Worktree: umbrella,
			Prompt:   "build the change",
			Timeout:  time.Minute,
		})
		done <- res
	}()

	// The dispatcher stops the worker while its agent runs.
	deadline := time.Now().Add(30 * time.Second)
	for !fileExists(marker) {
		if time.Now().After(deadline) {
			t.Fatal("the agent did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("the agent runner did not return after its context ended")
	}

	execs, err := st.ListExecutions(context.Background(), "worker", "add-a", 10)
	if err != nil || len(execs) != 1 {
		t.Fatalf("executions of add-a = %+v (err %v), want the one worker run", execs, err)
	}
	if execs[0].Status != "stopped" {
		t.Fatalf("the execution of the stopped agent is recorded as %q, want stopped", execs[0].Status)
	}
}
