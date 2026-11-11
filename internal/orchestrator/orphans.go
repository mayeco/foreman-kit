package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// orphanStopGrace is how long an orphaned agent's process group gets after SIGTERM before SIGKILL.
var orphanStopGrace = 10 * time.Second

// reapOrphanAgents stops the agent CLIs an orchestrator that died without stopping them (kill -9,
// a crash, a reboot of its terminal) left running. They run in their own process group, so its
// death does not reach them: one would keep editing a worktree that the next dispatcher reclaims
// and hands to a new agent, two agents on one worktree. An execution still marked running whose
// orchestrator is dead has its recorded process groups stopped and is marked stopped. A group is
// signaled only while its leader is that command: alive, still leading the group, and already
// running when the group was recorded, so a process ID the system gave to another program since
// is never signaled. It returns how many process groups it stopped.
func reapOrphanAgents(ctx context.Context, st *store.Store, logf func(string, ...any)) int {
	if st == nil {
		return 0
	}
	running, err := st.ListRunningExecutions(ctx)
	if err != nil {
		return 0
	}
	stopped := 0
	for _, e := range running {
		if e.PID <= 0 || e.PID == os.Getpid() || store.IsPIDAlive(e.PID) {
			continue
		}
		var groups []int
		for _, pgid := range e.PGIDs {
			if !processGroupLeaderAlive(pgid) {
				continue
			}
			if !recordedLeaderStillRuns(pgid, e.PGIDRecordedAt[pgid]) {
				if logf != nil {
					logf("dispatcher: %s: left process group %d alone: it cannot be identified as the %s agent the dead orchestrator %d started (its ID may belong to another program now)", e.ChangeName, pgid, e.Phase, e.PID)
				}
				continue
			}
			groups = append(groups, pgid)
		}
		for _, pgid := range groups {
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
		}
		deadline := time.Now().Add(orphanStopGrace)
		for _, pgid := range groups {
			for processGroupAlive(pgid) && time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
			}
			if processGroupAlive(pgid) {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
			stopped++
			if logf != nil {
				logf("dispatcher: %s: stopped the %s agent (process group %d) that the dead orchestrator %d left running", e.ChangeName, e.Phase, pgid, e.PID)
			}
			_ = st.EmitEvent(ctx, store.EventInput{
				Event:  "agent.orphan_stopped",
				Change: e.ChangeName,
				Phase:  e.Phase,
				ExecID: e.ExecID,
				Payload: map[string]any{
					"exec_id":      e.ExecID,
					"pgid":         pgid,
					"previous_pid": e.PID,
				},
			})
		}
		_ = st.FinishExecution(ctx, e.ExecID, "stopped", -1)
	}
	return stopped
}

// processGroupAlive reports whether any process of group pgid is alive.
func processGroupAlive(pgid int) bool {
	return pgid > 0 && syscall.Kill(-pgid, 0) == nil
}

// processGroupLeaderAlive reports whether pgid's leader is alive and still leads that group. That
// alone does not make it the recorded command: see recordedLeaderStillRuns.
func processGroupLeaderAlive(pgid int) bool {
	if pgid <= 1 || !store.IsPIDAlive(pgid) {
		return false
	}
	got, err := syscall.Getpgid(pgid)
	return err == nil && got == pgid && !isZombie(pgid)
}

// recordedLeaderStartSlack tolerates the one-second resolution of process start times and of the record.
const recordedLeaderStartSlack = 5

// recordedLeaderStillRuns reports whether the process leading group pgid is the one the group was
// recorded for at recordedAt (Unix seconds): it was already running then. After a reboot, or once
// process IDs wrap around, the recorded ID belongs to another program, and any program that leads
// its own group (a shell job, a desktop application, a service) would otherwise pass for the agent
// and be killed. A process started after the record was given the ID later. Without a record time
// (a row written by a version that kept none) or a readable start time nothing identifies the
// leader, and such a group is never signaled; neither is a genuine orphan that looks younger than
// its record because the clock was stepped forward since.
func recordedLeaderStillRuns(pgid int, recordedAt int64) bool {
	if recordedAt <= 0 {
		return false
	}
	started := store.ProcessStartUnix(pgid)
	return started > 0 && started <= recordedAt+recordedLeaderStartSlack
}

func isZombie(pid int) bool {
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return false
		}
		s := string(raw)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
			return s[i+2] == 'Z'
		}
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
