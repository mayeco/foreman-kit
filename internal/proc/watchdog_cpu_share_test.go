package proc

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestRunBoundedWatchdogStopsACommandThatOnlyKeepsWakingUp runs a command that never works and never
// sleeps for long either: it wakes up twenty times a second, as a CLI that froze does for its timers.
// The little CPU those wake-ups use is not activity.
func TestRunBoundedWatchdogStopsACommandThatOnlyKeepsWakingUp(t *testing.T) {
	root, _ := setupProcTestWorkspace(t)

	res, err := RunBounded(context.Background(), RunOptions{
		Dir:            root,
		Timeout:        20 * time.Second,
		IdleTimeout:    1200 * time.Millisecond,
		IdleCPUPercent: 50,
		WatchdogPoll:   200 * time.Millisecond,
		GracePeriod:    100 * time.Millisecond,
	}, "bash", "-c", `while :; do sleep 0.05; done`)
	if err != nil {
		t.Fatalf("RunBounded returned error: %v", err)
	}
	if !res.IdleStalled || res.ExitCode != 124 {
		t.Fatalf("expected the command that only wakes up to be stopped by the idle watchdog, got %+v", res)
	}
}

// TestRunBoundedWatchdogKeepsACommandThatWorks runs a command that prints nothing and changes no file
// but uses a whole core, like a test run or a build: its CPU use is its only sign of life, and it must
// not be stopped as stalled.
func TestRunBoundedWatchdogKeepsACommandThatWorks(t *testing.T) {
	root, _ := setupProcTestWorkspace(t)

	res, err := RunBounded(context.Background(), RunOptions{
		Dir:            root,
		Timeout:        30 * time.Second,
		IdleTimeout:    700 * time.Millisecond,
		IdleCPUPercent: 10,
		WatchdogPoll:   100 * time.Millisecond,
		GracePeriod:    100 * time.Millisecond,
	}, "bash", "-c", `end=$((SECONDS + 3)); while [ "$SECONDS" -lt "$end" ]; do :; done`)
	if err != nil {
		t.Fatalf("RunBounded returned error: %v", err)
	}
	if res.IdleStalled || res.ExitCode != 0 {
		t.Fatalf("expected the command that used a whole core to finish (not stall), got %+v", res)
	}
}

// TestRunBoundedReportsIdleTimeWhileItHappens: the watchdog tells after each poll how long the command
// has shown no activity, so that it can be shown before the command is stopped, not only after.
func TestRunBoundedReportsIdleTimeWhileItHappens(t *testing.T) {
	root, _ := setupProcTestWorkspace(t)

	var mu sync.Mutex
	var reports []time.Duration
	res, err := RunBounded(context.Background(), RunOptions{
		Dir:          root,
		Timeout:      20 * time.Second,
		IdleTimeout:  600 * time.Millisecond,
		WatchdogPoll: 100 * time.Millisecond,
		GracePeriod:  100 * time.Millisecond,
		OnIdle: func(idle time.Duration) {
			mu.Lock()
			reports = append(reports, idle)
			mu.Unlock()
		},
	}, "bash", "-c", "sleep 30")
	if err != nil {
		t.Fatalf("RunBounded returned error: %v", err)
	}
	if !res.IdleStalled {
		t.Fatalf("expected the sleeping command to stall, got %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) < 3 {
		t.Fatalf("expected a report after each poll before the stall, got %v", reports)
	}
	last := reports[len(reports)-1]
	if last < 600*time.Millisecond {
		t.Fatalf("the last report (%v) is under the idle limit the command was stopped at: %v", last, reports)
	}
	// The idle time only grows until the command works again, which this one never does (a first
	// poll that still sees the CPU of its start may reset it once).
	grew := 0
	for i := 1; i < len(reports); i++ {
		if reports[i] > reports[i-1] {
			grew++
		}
	}
	if grew < len(reports)-2 {
		t.Fatalf("the reported idle time does not grow from poll to poll: %v", reports)
	}
}
