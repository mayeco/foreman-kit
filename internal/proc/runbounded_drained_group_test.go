package proc

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// drainedGroupSignals records the signals a groupStop sends and delivers them for real.
type drainedGroupSignals struct {
	mu   sync.Mutex
	sent []string
}

func (s *drainedGroupSignals) kill(pid int, sig syscall.Signal) error {
	s.mu.Lock()
	s.sent = append(s.sent, fmt.Sprintf("%d:%d", pid, int(sig)))
	s.mu.Unlock()
	return syscall.Kill(pid, sig)
}

func (s *drainedGroupSignals) list() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.sent, " ")
}

// drainedGroupStart starts script through bash as the leader of its own process group, the way
// RunBounded starts a command, and waits until it has printed "ready".
func drainedGroupStart(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("bash", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	buf := make([]byte, 64)
	seen := ""
	for !strings.Contains(seen, "ready") {
		n, err := out.Read(buf)
		seen += string(buf[:n])
		if err != nil {
			t.Fatalf("the command ended before it was ready: %q (%v)", seen, err)
		}
	}
	return cmd
}

// TestGroupStopNeverSignalsADrainedGroup stops a group whose members all exit on SIGTERM. Once the
// leader is reaped and the group is seen empty, its ID can be given to a new process, which leads its
// own group if this kit started it. Neither the final SIGKILL nor the grace-period timer may signal
// that ID any more.
func TestGroupStopNeverSignalsADrainedGroup(t *testing.T) {
	const grace = 2 * time.Second
	signals := &drainedGroupSignals{}
	stop := &groupStop{grace: grace, kill: signals.kill}

	cmd := drainedGroupStart(t, `sleep 30 & echo ready; wait`)
	pgid := cmd.Process.Pid
	sent := time.Now()
	if err := stop.terminate(pgid); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if time.Since(sent) >= grace {
		t.Skip("reaping the leader took longer than the grace period: the timer was entitled to fire")
	}
	stop.leaderReaped(pgid)
	if processGroupRunning(pgid) {
		t.Fatal("leaderReaped returned while the group still had a running member")
	}

	// Past the grace period: the timer armed by terminate has had its chance to fire.
	time.Sleep(time.Until(sent.Add(grace + 300*time.Millisecond)))
	if got, want := signals.list(), fmt.Sprintf("%d:%d", -pgid, int(syscall.SIGTERM)); got != want {
		t.Fatalf("signals sent to the group: %q, want only the SIGTERM %q: a drained group was signaled again", got, want)
	}
}

// TestGroupStopKillsWhatOutlivesTheGracePeriod keeps the escalation: a member that ignores SIGTERM is
// killed when the grace period ends, and not before.
func TestGroupStopKillsWhatOutlivesTheGracePeriod(t *testing.T) {
	const grace = 500 * time.Millisecond
	signals := &drainedGroupSignals{}
	stop := &groupStop{grace: grace, kill: signals.kill}

	cmd := drainedGroupStart(t, `bash -c 'trap "" TERM; echo ready; while :; do sleep 0.05; done' & wait`)
	pgid := cmd.Process.Pid
	sent := time.Now()
	if err := stop.terminate(pgid); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	stop.leaderReaped(pgid)
	if elapsed := time.Since(sent); elapsed < grace/2 {
		t.Fatalf("the stubborn member was killed after %v, before the %v grace period ended", elapsed, grace)
	}
	for deadline := time.Now().Add(5 * time.Second); processGroupRunning(pgid) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if processGroupRunning(pgid) {
		t.Fatal("a member that ignored SIGTERM is still running after the grace period")
	}

	got := signals.list()
	term := fmt.Sprintf("%d:%d", -pgid, int(syscall.SIGTERM))
	kill := fmt.Sprintf("%d:%d", -pgid, int(syscall.SIGKILL))
	if !strings.HasPrefix(got, term) || !strings.Contains(got, kill) {
		t.Fatalf("signals sent to the group: %q, want %q and then %q", got, term, kill)
	}
}

// TestGroupStopKillsLeftoversOfANormalExitAtOnce keeps the other half: what a command leaves running
// when it exits on its own is killed without waiting.
func TestGroupStopKillsLeftoversOfANormalExitAtOnce(t *testing.T) {
	signals := &drainedGroupSignals{}
	stop := &groupStop{grace: time.Minute, kill: signals.kill}

	cmd := drainedGroupStart(t, `sleep 30 & echo ready`)
	pgid := cmd.Process.Pid
	_ = cmd.Wait()
	started := time.Now()
	stop.leaderReaped(pgid)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("leftovers of a normal exit were not killed at once (%v)", elapsed)
	}
	for deadline := time.Now().Add(5 * time.Second); processGroupRunning(pgid) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if processGroupRunning(pgid) {
		t.Fatal("the leftover process is still running")
	}
	if got, want := signals.list(), fmt.Sprintf("%d:%d", -pgid, int(syscall.SIGKILL)); got != want {
		t.Fatalf("signals sent to the group: %q, want %q", got, want)
	}
}
