package orchestrator

import (
	"context"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// orphanReuseBystander starts an unrelated program that leads its own process group, like every shell
// job, desktop application and service does, and returns its process ID. It is reaped as soon as it dies.
func orphanReuseBystander(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return pid
}

// orphanReuseStore opens a store in a temporary state directory.
func orphanReuseStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// orphanReuseAssertLeftAlone runs the reaper over the planted execution and checks that it marked it
// stopped without signaling the bystander.
func orphanReuseAssertLeftAlone(t *testing.T, st *store.Store, bystander int) {
	t.Helper()
	ctx := context.Background()
	defer func(g time.Duration) { orphanStopGrace = g }(orphanStopGrace)
	orphanStopGrace = time.Second

	if n := reapOrphanAgents(ctx, st, t.Logf); n != 0 {
		t.Errorf("stopped %d process groups, want 0", n)
	}
	// SIGTERM kills sleep at once; give a wrongly sent one time to show.
	time.Sleep(300 * time.Millisecond)
	if !store.IsPIDAlive(bystander) {
		t.Fatal("the reaper signaled an unrelated program that had been given a recorded process group ID")
	}
	if rs, _ := st.ListRunningExecutions(ctx); len(rs) != 0 {
		t.Fatalf("the dead orchestrator's execution is still marked running: %+v", rs)
	}
}

// TestReapOrphanAgentsNeverSignalsAReusedProcessGroupID reproduces a restart after a reboot (or after
// process IDs wrapped around): the orchestrator and its agent are gone, and the process group ID the
// execution recorded now belongs to another program that leads its own group. It must not be killed.
func TestReapOrphanAgentsNeverSignalsAReusedProcessGroupID(t *testing.T) {
	st := orphanReuseStore(t)
	ctx := context.Background()
	execID := "worker-add-search-apply-1"
	if err := st.CreateExecution(ctx, store.Execution{ExecID: execID, Kind: "worker", ChangeName: "add-search", Phase: "apply", Status: "running", PID: deadPID(t)}); err != nil {
		t.Fatal(err)
	}

	bystander := orphanReuseBystander(t)
	// The agent's group was recorded ten minutes before the bystander started.
	if err := st.AddExecutionProcessGroup(ctx, execID, bystander); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `
		UPDATE execution_process_groups SET recorded_at = ? WHERE exec_id = ? AND pgid = ?
	`, (time.Now().UTC().Unix()-600)*1000, execID, bystander); err != nil {
		t.Fatal(err)
	}
	rs, _ := st.ListRunningExecutions(ctx)
	if len(rs) != 1 || len(rs[0].PGIDs) != 1 || rs[0].PGIDs[0] != bystander || rs[0].PGIDRecordedAt[bystander] == 0 {
		t.Fatalf("planted execution not read back: %+v", rs)
	}

	orphanReuseAssertLeftAlone(t, st, bystander)
}

// TestReapOrphanAgentsNeverSignalsAnUnidentifiedProcessGroup covers a row written by a version that
// recorded process groups without the time: nothing tells its leader from a later program with the
// same ID, so it is left alone.
func TestReapOrphanAgentsNeverSignalsAnUnidentifiedProcessGroup(t *testing.T) {
	st := orphanReuseStore(t)
	ctx := context.Background()
	execID := "worker-add-search-plan-1"
	if err := st.CreateExecution(ctx, store.Execution{ExecID: execID, Kind: "worker", ChangeName: "add-search", Phase: "plan", Status: "running", PID: deadPID(t)}); err != nil {
		t.Fatal(err)
	}

	bystander := orphanReuseBystander(t)
	// A process group with no usable record time: nothing identifies its leader.
	if _, err := st.DB().ExecContext(ctx, `
		INSERT INTO execution_process_groups (exec_id, pgid, recorded_at) VALUES (?, ?, 0)
	`, execID, bystander); err != nil {
		t.Fatal(err)
	}

	orphanReuseAssertLeftAlone(t, st, bystander)
}
