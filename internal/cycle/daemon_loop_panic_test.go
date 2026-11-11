package cycle

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// daemonLoopPanicLog collects the daemon's output; its loops write while the test reads.
type daemonLoopPanicLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *daemonLoopPanicLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *daemonLoopPanicLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// daemonLoopPanicSetup prepares an application and its store for a foreground daemon.
func daemonLoopPanicSetup(t *testing.T) (context.Context, context.CancelFunc, string, *store.Store) {
	t.Helper()
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	appDir := filepath.Join(t.TempDir(), "daemon-app")
	if err := Init(ctx, appDir, KitOptions{Agents: "claude"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	return ctx, cancel, appDir, openTestStore(t, appDir)
}

// daemonLoopPanicWait waits until cond holds, the daemon exits, or the time is up.
func daemonLoopPanicWait(t *testing.T, what string, done <-chan int, log *daemonLoopPanicLog, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		select {
		case code := <-done:
			t.Fatalf("the daemon exited (%d) before %s; log:\n%s", code, what, log.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s; log:\n%s", what, log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDaemonContainsAPanicInOneLoop makes the continuous dispatcher panic once. The panic used to end
// the whole daemon: the merger and the portal went down with it, and the agents it had started kept
// running with no daemon left to stop them. It must be logged with its stack, the dispatcher started
// again after the retry delay, and the merger left running.
func TestDaemonContainsAPanicInOneLoop(t *testing.T) {
	ctx, cancel, appDir, st := daemonLoopPanicSetup(t)

	var dispatcherRuns, mergerRuns atomic.Int32
	var log, errLog daemonLoopPanicLog
	done := make(chan int, 1)
	go func() {
		done <- StartDaemon(ctx, appDir, st, DaemonOptions{
			NoInitialCycle: true,
			Interval:       10 * time.Minute,
			LoopRetryDelay: 50 * time.Millisecond,
			ServeDispatcherFunc: func(ctx context.Context, root string, out io.Writer) error {
				if dispatcherRuns.Add(1) == 1 {
					var results map[string]string
					results["first"] = "built" // a write to a nil map, as a bug in the loop would be
				}
				<-ctx.Done()
				return nil
			},
			ServeMergerFunc: func(ctx context.Context, root string, out io.Writer) error {
				mergerRuns.Add(1)
				<-ctx.Done()
				return nil
			},
		}, &log, &errLog)
	}()

	daemonLoopPanicWait(t, "the dispatcher was started again", done, &log, func() bool { return dispatcherRuns.Load() >= 2 })
	daemonLoopPanicWait(t, "the merger runs", done, &log, func() bool { return mergerRuns.Load() >= 1 })
	if n := mergerRuns.Load(); n != 1 {
		t.Fatalf("the merger was started %d times: the dispatcher's panic must not disturb it", n)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("daemon exit code %d after cancel; log:\n%s", code, log.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop")
	}
	for _, want := range []string{
		"daemon: dispatcher panicked: assignment to entry in nil map",
		"daemon_loop_panic_test.go", // the stack names where it happened
		"daemon: dispatcher stopped: panic: assignment to entry in nil map; restarting in 50ms",
	} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("daemon log lacks %q:\n%s", want, log.String())
		}
	}
}

// TestDaemonContainsAPanicInACycle makes the periodic cycle panic. The daemon must keep running, and
// the cycle must leave neither its lock nor a "running" execution behind: the daemon's PID stays
// alive, so nothing would ever clear them.
func TestDaemonContainsAPanicInACycle(t *testing.T) {
	ctx, cancel, appDir, st := daemonLoopPanicSetup(t)

	var dispatcherRuns atomic.Int32
	var log, errLog daemonLoopPanicLog
	done := make(chan int, 1)
	go func() {
		done <- StartDaemon(ctx, appDir, st, DaemonOptions{
			Interval:       10 * time.Minute,
			LoopRetryDelay: 50 * time.Millisecond,
			CycleOpts: CycleOptions{
				SkipWorktreePrep: true,
				PreflightFunc: func(ctx context.Context, root string, out io.Writer) error {
					var checks []string
					_ = checks[3] // an index out of range, as a bug in the cycle would be
					return nil
				},
			},
			ServeDispatcherFunc: func(ctx context.Context, root string, out io.Writer) error {
				dispatcherRuns.Add(1)
				<-ctx.Done()
				return nil
			},
		}, &log, &errLog)
	}()

	daemonLoopPanicWait(t, "the cycle's panic is logged", done, &log, func() bool {
		return strings.Contains(log.String(), "daemon: cycle panicked: runtime error: index out of range")
	})
	if l, err := st.GetLock(ctx, "cycle"); err != nil || l != nil {
		t.Fatalf("the cycle lock was not released after the panic: %+v (%v)", l, err)
	}
	cycles, err := st.ListExecutions(ctx, "cycle", "", 10)
	if err != nil || len(cycles) != 1 || cycles[0].Status != "failed" {
		t.Fatalf("expected the panicked cycle to be recorded as failed, got %+v (%v)", cycles, err)
	}
	daemonLoopPanicWait(t, "the dispatcher runs", done, &log, func() bool { return dispatcherRuns.Load() >= 1 })
	if n := dispatcherRuns.Load(); n != 1 {
		t.Fatalf("the dispatcher was started %d times: the cycle's panic must not disturb it", n)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("daemon exit code %d after cancel; log:\n%s", code, log.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop")
	}
}
