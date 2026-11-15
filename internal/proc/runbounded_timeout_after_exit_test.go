package proc

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// detachedLeftover returns the shell prefix that starts, in the background, a process that leaves the
// command's process group (setsid) and keeps the output pipe open for 9 s, and waits until it has left it:
// the leader's exit kills whatever is still in the group, so a slow perl start would hide the defect.
func detachedLeftover(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl is needed to leave the process group")
	}
	marker := filepath.Join(t.TempDir(), "detached")
	return "perl -MPOSIX -e 'POSIX::setsid(); open(F, \">\", $ARGV[0]); close(F); sleep 9' '" + marker + "' & " +
		"i=0; while [ ! -e '" + marker + "' ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done; "
}

// TestRunBoundedExitedWithinLimitIsNotATimeoutWhenTheDrainRunsPastTheDeadline runs a command that exits 0
// after about a second, well inside its 3 s limit, while a descendant that left the process group keeps
// the output pipe open. Draining the output then takes outputDrainTimeout (5 s), which ends after the
// deadline: the command's own status must still be reported, not a timeout.
func TestRunBoundedExitedWithinLimitIsNotATimeoutWhenTheDrainRunsPastTheDeadline(t *testing.T) {
	script := detachedLeftover(t) + "sleep 1; echo done; exit 0"
	res, err := RunBounded(context.Background(), RunOptions{Timeout: 4 * time.Second, GracePeriod: time.Second},
		"sh", "-c", script)
	if err != nil {
		t.Fatalf("RunBounded returned error: %v", err)
	}
	if res.TimedOut || res.ExitCode != 0 {
		t.Fatalf("expected the command's own exit 0 and no timeout, got exit=%d timedOut=%v duration=%v output=%q",
			res.ExitCode, res.TimedOut, res.Duration, res.Output)
	}
	if res.Duration < outputDrainTimeout {
		t.Fatalf("the test no longer exercises a drain that outlives the deadline: duration=%v", res.Duration)
	}
	if !strings.Contains(res.Output, "done") {
		t.Fatalf("expected the command's output, got %q", res.Output)
	}
}

// TestRunBoundedExitStatusIsKeptWhenItExitsNonZeroWithinTheLimit keeps the command's own failing status
// when the drain outlives the deadline.
func TestRunBoundedExitStatusIsKeptWhenItExitsNonZeroWithinTheLimit(t *testing.T) {
	script := detachedLeftover(t) + "sleep 0.5; exit 3"
	res, err := RunBounded(context.Background(), RunOptions{Timeout: 3 * time.Second, GracePeriod: time.Second},
		"sh", "-c", script)
	if err != nil {
		t.Fatalf("RunBounded returned error: %v", err)
	}
	if res.TimedOut || res.ExitCode != 3 {
		t.Fatalf("expected exit 3 and no timeout, got exit=%d timedOut=%v", res.ExitCode, res.TimedOut)
	}
}

// TestRunBoundedStillTimesOutWhenTheCommandRunsPastItsLimit keeps 124 for a command the deadline stopped,
// also when a descendant outside the group holds the output pipe past the deadline.
func TestRunBoundedStillTimesOutWhenTheCommandRunsPastItsLimit(t *testing.T) {
	res, _ := RunBounded(context.Background(), RunOptions{Timeout: 300 * time.Millisecond, GracePeriod: 100 * time.Millisecond},
		"sh", "-c", "sleep 30")
	if !res.TimedOut || res.ExitCode != 124 {
		t.Fatalf("expected a timeout (124), got exit=%d timedOut=%v", res.ExitCode, res.TimedOut)
	}
	script := detachedLeftover(t) + "sleep 30"
	res, _ = RunBounded(context.Background(), RunOptions{Timeout: 3 * time.Second, GracePeriod: 100 * time.Millisecond},
		"sh", "-c", script)
	if !res.TimedOut || res.ExitCode != 124 {
		t.Fatalf("expected a timeout (124) with a leftover outside the group, got exit=%d timedOut=%v", res.ExitCode, res.TimedOut)
	}
}

// TestRunBoundedCancellationCodeUnchangedAfterTheFix keeps 143 for a caller that cancels and no timeout flag.
func TestRunBoundedCancellationCodeUnchangedAfterTheFix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	res, _ := RunBounded(ctx, RunOptions{Timeout: time.Minute, GracePeriod: 100 * time.Millisecond}, "sleep", "30")
	if res.ExitCode != 143 || res.TimedOut {
		t.Fatalf("expected exit 143 and no timeout, got exit=%d timedOut=%v", res.ExitCode, res.TimedOut)
	}
}
