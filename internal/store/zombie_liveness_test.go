package store

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// zombieLivenessChild starts a child that exits at once and leaves it unreaped: a zombie, which is what
// a kit process becomes when the parent that started it keeps running without waiting for it. The
// returned function reaps it.
func zombieLivenessChild(t *testing.T) (pid int, reap func()) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid = cmd.Process.Pid
	reaped := false
	reap = func() {
		if !reaped {
			reaped = true
			_ = cmd.Wait()
		}
	}
	t.Cleanup(reap)
	return pid, reap
}

// TestIsPIDAliveReportsZombieAsDead keeps the liveness verdict the same on Linux and macOS: an exited
// process its parent has not reaped still answers kill(pid, 0) on both, and must not count as alive.
func TestIsPIDAliveReportsZombieAsDead(t *testing.T) {
	pid, reap := zombieLivenessChild(t)

	// The child becomes a zombie as soon as it exits; give it time to run.
	deadline := time.Now().Add(10 * time.Second)
	for IsPIDAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the unreaped child should still exist as a zombie, kill(pid, 0) = %v", err)
	}
	if IsPIDAlive(pid) {
		t.Fatalf("IsPIDAlive(%d) = true for a zombie", pid)
	}

	// A lock the zombie left behind is stale at once instead of holding until its TTL.
	now := time.Now().UTC().Unix()
	host, _ := os.Hostname()
	rec := LockRecord{LockKey: "cycle", OwnerType: "cycle", PID: pid, Host: host, AcquiredAt: now, HeartbeatAt: now, TTLSeconds: 7200}
	if !IsLockStale(rec, host, now) {
		t.Fatal("a lock whose owner is a zombie must be stale")
	}

	reap()
	if IsPIDAlive(pid) {
		t.Fatalf("IsPIDAlive(%d) = true after the child was reaped", pid)
	}
}

// TestIsPIDAliveKeepsLiveProcessesAlive guards the zombie check against reporting a running process,
// in its own process group or not, as dead.
func TestIsPIDAliveKeepsLiveProcessesAlive(t *testing.T) {
	if !IsPIDAlive(os.Getpid()) {
		t.Fatal("IsPIDAlive(own pid) = false")
	}
	for _, setpgid := range []bool{false, true} {
		cmd := exec.Command("sleep", "30")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: setpgid}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		alive := IsPIDAlive(pid)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if !alive {
			t.Fatalf("IsPIDAlive(%d) = false for a running process (setpgid=%v)", pid, setpgid)
		}
	}
}
