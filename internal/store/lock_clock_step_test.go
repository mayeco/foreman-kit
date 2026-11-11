package store

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// lockClockStepOwner starts a process that stands in for the live owner of a lock and returns its PID
// and the start time the OS reports for it.
func lockClockStepOwner(t *testing.T) (pid int, started int64) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid = cmd.Process.Pid
	for deadline := time.Now().Add(5 * time.Second); started == 0 && time.Now().Before(deadline); {
		if started = processStartTimeUnix(pid); started == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if started == 0 {
		t.Skip("the start time of a process cannot be read on this system")
	}
	return pid, started
}

// TestIsLockStaleKeepsHeartbeatedLockAcrossClockStep reproduces what another process sees after the wall
// clock is stepped forward while a daemon holds its locks (a VM or WSL2 guest resumed after its host
// slept, the first time sync of a board without a clock). Linux derives a process start time from the
// current boot time, so the owner then appears to have started long after it took the lock. It keeps
// refreshing the row, which a later process that merely got the same PID never does, and must keep it.
func TestIsLockStaleKeepsHeartbeatedLockAcrossClockStep(t *testing.T) {
	pid, procStart := lockClockStepOwner(t)
	host, _ := os.Hostname()
	now := time.Now().UTC().Unix()

	held := LockRecord{
		LockKey: "daemon", OwnerType: "daemon", PID: pid, Host: host,
		AcquiredAt: procStart - 3600, HeartbeatAt: now, TTLSeconds: 7200,
	}
	if IsLockStale(held, host, now) {
		t.Fatal("a lock its live owner refreshed after that process started was reported stale")
	}

	// PID reuse is still caught: nobody refreshed the row since the process that has the PID started.
	reused := held
	reused.HeartbeatAt = procStart - 3000
	if !IsLockStale(reused, host, now) {
		t.Fatal("a lock last refreshed before the process holding its PID started must be stale")
	}
	// A row that was never refreshed is judged by its acquisition, as before.
	neverRefreshed := held
	neverRefreshed.HeartbeatAt = 0
	if !IsLockStale(neverRefreshed, host, now) {
		t.Fatal("a lock acquired before the process holding its PID started, and never refreshed, must be stale")
	}
}

// TestLockOwnerReplacedIgnoresStartTimeInTheFuture covers the opposite step: macOS keeps the start time
// a process was given, so after the clock is set back a live owner started "in the future" and every
// lock it takes from then on predates it. That says nothing about PID reuse.
func TestLockOwnerReplacedIgnoresStartTimeInTheFuture(t *testing.T) {
	now := time.Now().UTC().Unix()
	rec := LockRecord{LockKey: "cycle", PID: 4242, AcquiredAt: now - 30, HeartbeatAt: now - 2, TTLSeconds: 7200}

	cases := []struct {
		name      string
		procStart int64
		want      bool
	}{
		{"start time unknown", 0, false},
		{"started before the lock was taken", now - 600, false},
		{"started between acquisition and the last refresh", now - 20, false},
		{"started within the tolerance of the last refresh", now, false},
		{"started after the clock was set back", now + 7200, false},
	}
	for _, c := range cases {
		if got := lockOwnerReplaced(rec, c.procStart, now); got != c.want {
			t.Errorf("%s: lockOwnerReplaced = %v, want %v", c.name, got, c.want)
		}
	}

	abandoned := LockRecord{LockKey: "cycle", PID: 4242, AcquiredAt: now - 900, HeartbeatAt: now - 600, TTLSeconds: 7200}
	if !lockOwnerReplaced(abandoned, now-60, now) {
		t.Error("a process started after the last refresh of the row is not its owner")
	}
}
