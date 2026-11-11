package cycle

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// detachedSpawnStandIn points kitExecutable at a shell script standing in for the foreman-kit binary a
// detached cycle or daemon runs, and keeps runtime files out of the real state directory.
func detachedSpawnStandIn(t *testing.T, script string) {
	t.Helper()
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	path := filepath.Join(t.TempDir(), "foreman-stand-in")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := kitExecutable
	kitExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { kitExecutable = prev })
}

var detachedSpawnPIDRe = regexp.MustCompile(`pid=(\d+)`)

// detachedSpawnPID extracts the child PID a "started pid=N" line reports.
func detachedSpawnPID(t *testing.T, output string) int {
	t.Helper()
	m := detachedSpawnPIDRe.FindStringSubmatch(output)
	if m == nil {
		t.Fatalf("no pid in output %q", output)
	}
	pid, _ := strconv.Atoi(m[1])
	return pid
}

// TestDetachedCycleRunsTheRequestedStrategy runs `foreman-kit cycle --detach --strategy security`: the
// cycle that starts in the background must be given the strategy, not fall back to the rotation.
func TestDetachedCycleRunsTheRequestedStrategy(t *testing.T) {
	record := filepath.Join(t.TempDir(), "argv")
	t.Setenv("DETACHED_SPAWN_RECORD", record)
	detachedSpawnStandIn(t, `{ printf '%s\n' "$@"; echo "stamp=$CYCLE_STAMP"; } > "$DETACHED_SPAWN_RECORD.tmp" && mv "$DETACHED_SPAWN_RECORD.tmp" "$DETACHED_SPAWN_RECORD"`)

	var out, errOut bytes.Buffer
	args := []string{"--detach", "--strategy", "security"}
	if rc := RunCycleCLI(context.Background(), bareApp(t), nil, CycleOptions{Stamp: "20260102T030405Z"}, args, &out, &errOut); rc != 0 {
		t.Fatalf("cycle --detach failed (%d): %s%s", rc, out.String(), errOut.String())
	}

	var got []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(record); err == nil {
			got = b
			break
		}
	}
	if want := "cycle\n--strategy\nsecurity\nstamp=20260102T030405Z\n"; string(got) != want {
		t.Fatalf("the detached cycle was started with %q, want %q", got, want)
	}
}

// TestDetachedCycleIsReapedByALongLivedParent starts a detached cycle the way the portal's "run a cycle"
// action does, from a process that keeps running. Once the cycle exits it must be reaped: an unreaped
// child stays a zombie, holding its PID, until the daemon or the portal that started it exits.
func TestDetachedCycleIsReapedByALongLivedParent(t *testing.T) {
	detachedSpawnStandIn(t, "exit 0")

	var out bytes.Buffer
	if err := RunCycle(context.Background(), bareApp(t), nil, CycleOptions{Detach: true}, &out); err != nil {
		t.Fatal(err)
	}
	pid := detachedSpawnPID(t, out.String())

	// kill(pid, 0) answers for a zombie and reports ESRCH only once the child was waited for.
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached cycle %d exited but was never reaped (kill(pid, 0) = %v): it stays a zombie for as long as its parent runs", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
