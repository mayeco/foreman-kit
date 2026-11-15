package orchestrator

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// orphanReuseExecution plants a running execution that began `age` ago, started by orchestrator pid,
// with one recorded process group: the leader of a `sleep` the test started, standing for the agent.
func orphanReuseExecution(t *testing.T, st *store.Store, execID string, pid int, age time.Duration) (agent int) {
	t.Helper()
	ctx := context.Background()
	began := time.Now().UTC().Add(-age).Format(time.RFC3339Nano)
	if err := st.CreateExecution(ctx, store.Execution{ExecID: execID, Kind: "worker", ChangeName: "add-search", Phase: "apply", Status: "running", PID: pid, StartedAt: began}); err != nil {
		t.Fatal(err)
	}
	agent = orphanReuseBystander(t)
	if err := st.AddExecutionProcessGroup(ctx, execID, agent); err != nil {
		t.Fatal(err)
	}
	return agent
}

// orphanReuseReap runs the reaper and returns how many process groups it stopped.
func orphanReuseReap(t *testing.T, st *store.Store) int {
	t.Helper()
	defer func(g time.Duration) { orphanStopGrace = g }(orphanStopGrace)
	orphanStopGrace = 2 * time.Second
	return reapOrphanAgents(context.Background(), st, t.Logf)
}

// orphanReuseWaitDead waits for the process group of pid to disappear.
func orphanReuseWaitDead(pid int) bool {
	for i := 0; i < 50; i++ {
		if !processGroupAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !processGroupAlive(pid)
}

// TestListRunningExecutionsReturnsWhenTheExecutionStarted checks the start time the reaper compares
// with the start time of the orchestrator's process.
func TestListRunningExecutionsReturnsWhenTheExecutionStarted(t *testing.T) {
	st := orphanReuseStore(t)
	want := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := st.CreateExecution(context.Background(), store.Execution{ExecID: "worker-add-search-apply-1", Kind: "worker", ChangeName: "add-search", Phase: "apply", Status: "running", PID: 1234, StartedAt: want.Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	rs, err := st.ListRunningExecutions(context.Background())
	if err != nil || len(rs) != 1 {
		t.Fatalf("running executions = %+v, %v", rs, err)
	}
	if rs[0].StartedAt != want.Unix() {
		t.Fatalf("StartedAt = %d, want %d", rs[0].StartedAt, want.Unix())
	}
}

// TestReapOrphanAgentsStopsTheAgentWhenTheOrchestratorPIDWasReused reproduces a crash followed by the
// system giving the orchestrator's process ID to an unrelated program: the program is alive, but it
// started long after the execution began, so it is not the orchestrator and the agent is an orphan.
func TestReapOrphanAgentsStopsTheAgentWhenTheOrchestratorPIDWasReused(t *testing.T) {
	st := orphanReuseStore(t)
	reused := orphanReuseBystander(t)
	agent := orphanReuseExecution(t, st, "worker-add-search-apply-1", reused, time.Hour)

	if n := orphanReuseReap(t, st); n != 1 {
		t.Fatalf("stopped %d process groups, want 1", n)
	}
	if !orphanReuseWaitDead(agent) {
		t.Fatal("the orphaned agent's process group is still alive")
	}
	if !store.IsPIDAlive(reused) {
		t.Fatal("the reaper signaled the unrelated program that holds the orchestrator's old process ID")
	}
	if rs, _ := st.ListRunningExecutions(context.Background()); len(rs) != 0 {
		t.Fatalf("the execution is still marked running: %+v", rs)
	}
}

// TestReapOrphanAgentsStopsTheAgentWhenTheNewDaemonHasTheOldPID covers a reboot: the restarted daemon
// got the process ID the dead orchestrator had, which this process now holds.
func TestReapOrphanAgentsStopsTheAgentWhenTheNewDaemonHasTheOldPID(t *testing.T) {
	st := orphanReuseStore(t)
	agent := orphanReuseExecution(t, st, "worker-add-search-apply-1", os.Getpid(), time.Hour)

	if n := orphanReuseReap(t, st); n != 1 {
		t.Fatalf("stopped %d process groups, want 1", n)
	}
	if !orphanReuseWaitDead(agent) {
		t.Fatal("the orphaned agent's process group is still alive")
	}
	if rs, _ := st.ListRunningExecutions(context.Background()); len(rs) != 0 {
		t.Fatalf("the execution is still marked running: %+v", rs)
	}
}

// TestReapOrphanAgentsLeavesALiveOrchestratorThatStartedBeforeItsExecution is the case the fix must
// not break: the process holding the ID started before the execution began, so it is the orchestrator.
func TestReapOrphanAgentsLeavesALiveOrchestratorThatStartedBeforeItsExecution(t *testing.T) {
	st := orphanReuseStore(t)
	orchestrator := orphanReuseBystander(t)
	// The execution began now: after the process that stands for its orchestrator started.
	agent := orphanReuseExecution(t, st, "worker-add-search-apply-1", orchestrator, 0)
	own := orphanReuseExecution(t, st, "worker-other-apply-1", os.Getpid(), 0)

	if n := orphanReuseReap(t, st); n != 0 {
		t.Fatalf("stopped %d process groups, want 0", n)
	}
	time.Sleep(300 * time.Millisecond)
	if !processGroupAlive(agent) || !processGroupAlive(own) {
		t.Fatal("the reaper stopped the agent of a live orchestrator")
	}
	if rs, _ := st.ListRunningExecutions(context.Background()); len(rs) != 2 {
		t.Fatalf("running executions = %+v, want both", rs)
	}
}
