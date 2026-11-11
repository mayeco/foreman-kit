package proc

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestRunBoundedWatchdogSeesChildTMPDIRActivity verifies that the idle watchdog watches the TMPDIR the
// child actually receives (workers run with a per-change scratch TMPDIR), not the orchestrator's own.
// The child uses no CPU, writes no output and leaves the worktree untouched: its only sign of life is
// a log growing in its TMPDIR, so it must not be killed as stalled.
func TestRunBoundedWatchdogSeesChildTMPDIRActivity(t *testing.T) {
	root, _ := setupProcTestWorkspace(t)
	parentTmp := t.TempDir()
	childTmp := t.TempDir()
	t.Setenv("TMPDIR", parentTmp)

	res, err := RunBounded(context.Background(), RunOptions{
		Dir:          root,
		Env:          append(os.Environ(), "TMPDIR="+childTmp),
		Timeout:      20 * time.Second,
		IdleTimeout:  700 * time.Millisecond,
		WatchdogPoll: 100 * time.Millisecond,
		GracePeriod:  100 * time.Millisecond,
	}, "bash", "-c", `for i in $(seq 1 15); do echo "$i" >> "$TMPDIR/gate.log"; sleep 0.1; done`)
	if err != nil {
		t.Fatalf("RunBounded returned error: %v", err)
	}
	if res.IdleStalled || res.ExitCode != 0 {
		t.Fatalf("expected the child writing to its own TMPDIR to finish (not stall), got %+v", res)
	}
}

func TestEnvLookupLastValueWins(t *testing.T) {
	env := []string{"TMPDIR=/first", "PATH=/bin", "TMPDIR=/second"}
	if v, ok := envLookup(env, "TMPDIR"); !ok || v != "/second" {
		t.Fatalf("expected the last TMPDIR to win, got %q ok=%v", v, ok)
	}
	if _, ok := envLookup(env, "TMP"); ok {
		t.Fatal("expected no match for a key that is only a prefix of another key")
	}
}
