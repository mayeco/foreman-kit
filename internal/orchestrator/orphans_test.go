package orchestrator

import (
	"context"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/proc"
	"github.com/mayeco/foreman-kit/internal/store"
)

// deadPID returns the process ID of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return c.Process.Pid
}

// TestReapOrphanAgentsStopsWhatADeadOrchestratorLeftRunning reproduces a kill -9 of foreman-kit
// run: its agent, in its own process group, kept running; the restarted dispatcher reclaimed the
// change's lease and handed the worktree to a new agent while the old one still edited it.
func TestReapOrphanAgentsStopsWhatADeadOrchestratorLeftRunning(t *testing.T) {
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	defer func(g time.Duration) { orphanStopGrace = g }(orphanStopGrace)
	orphanStopGrace = 2 * time.Second

	// The orphan: started through RunBounded (which records its process group), then left behind.
	execID := "worker-tune-web-test-gate-plan-1"
	if err := st.CreateExecution(ctx, store.Execution{ExecID: execID, Kind: "worker", ChangeName: "tune-web-test-gate", Phase: "plan", Status: "running", PID: deadPID(t)}); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = proc.RunBounded(runCtx, proc.RunOptions{Store: st, ExecID: execID, Timeout: time.Minute}, "sh", "-c", "sleep 60 & wait")
	}()
	var pgid int
	for i := 0; i < 50 && pgid == 0; i++ {
		time.Sleep(100 * time.Millisecond)
		if rs, _ := st.ListRunningExecutions(ctx); len(rs) == 1 && len(rs[0].PGIDs) == 1 {
			pgid = rs[0].PGIDs[0]
		}
	}
	if pgid == 0 {
		t.Fatal("RunBounded did not record the process group")
	}

	// A live orchestrator's executions are left alone.
	if err := st.CreateExecution(ctx, store.Execution{ExecID: "live", Kind: "worker", ChangeName: "other", Status: "running", PID: syscall.Getpid()}); err != nil {
		t.Fatal(err)
	}

	if n := reapOrphanAgents(ctx, st, t.Logf); n != 1 {
		t.Fatalf("stopped %d process groups, want 1", n)
	}
	if processGroupAlive(pgid) {
		t.Fatal("the orphaned agent's process group is still alive")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunBounded did not return after its process group was stopped")
	}
	rs, _ := st.ListRunningExecutions(ctx)
	if len(rs) != 1 || rs[0].ExecID != "live" {
		t.Fatalf("running executions = %+v, want only the live one", rs)
	}
	var stopped int
	_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE event = 'agent.orphan_stopped' AND change_name = 'tune-web-test-gate'`).Scan(&stopped)
	if stopped != 1 {
		t.Fatalf("agent.orphan_stopped events = %d, want 1", stopped)
	}
	if n := reapOrphanAgents(ctx, st, t.Logf); n != 0 {
		t.Fatalf("a second reap stopped %d groups, want 0", n)
	}
}
