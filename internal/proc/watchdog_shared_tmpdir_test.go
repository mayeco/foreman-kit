package proc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunBoundedWatchdogIgnoresSharedTMPDIRChurn watches a stalled command that shares its temporary
// directory with this process, as the cycle's agent, a portal run and `foreman-kit agent-exec` do,
// while another program keeps creating and deleting temp files there. That churn is not the command's
// activity: it used to reset the idle clock at every poll, so the stalled command was never stopped
// before its time limit (and the watchdog test failed whenever the rest of the suite was busy in the
// same directory).
func TestRunBoundedWatchdogIgnoresSharedTMPDIRChurn(t *testing.T) {
	root, _ := setupProcTestWorkspace(t)
	shared := t.TempDir()
	t.Setenv("TMPDIR", shared)

	stopChurn := make(chan struct{})
	churnDone := make(chan struct{})
	go func() {
		defer close(churnDone)
		for i := 0; ; i++ {
			select {
			case <-stopChurn:
				return
			default:
			}
			p := filepath.Join(shared, fmt.Sprintf("another-program-%d.tmp", i))
			_ = os.WriteFile(p, []byte("x"), 0o600)
			time.Sleep(20 * time.Millisecond)
			_ = os.Remove(p)
		}
	}()
	defer func() {
		close(stopChurn)
		<-churnDone
	}()

	res, err := RunBounded(context.Background(), RunOptions{
		Dir:          root,
		Timeout:      8 * time.Second,
		IdleTimeout:  500 * time.Millisecond,
		WatchdogPoll: 100 * time.Millisecond,
		GracePeriod:  100 * time.Millisecond,
	}, "bash", "-c", "echo starting; sleep 30")
	if err != nil {
		t.Fatalf("RunBounded returned error: %v", err)
	}
	if !res.IdleStalled || res.ExitCode != 124 {
		t.Fatalf("expected the stalled command to be stopped by the idle watchdog, got %+v", res)
	}
}
