package cycle

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestStartDaemonReportsADaemonThatExitsAtStartup starts a detached daemon that dies at once, as it
// does when --port names a port in use or the state database cannot be opened. `daemon start` must say
// so and fail, instead of printing "daemon: started pid=..." for a daemon that is not running.
func TestStartDaemonReportsADaemonThatExitsAtStartup(t *testing.T) {
	detachedSpawnStandIn(t, `echo "daemon: failed to start web portal: address already in use" >&2; exit 1`)
	root := bareApp(t)
	st := openTestStore(t, root)

	var out, errOut bytes.Buffer
	rc := StartDaemon(context.Background(), root, st, DaemonOptions{Detach: true}, &out, &errOut)
	if rc == 0 || strings.Contains(out.String(), "daemon: started") {
		t.Fatalf("a daemon that exited at startup was reported as started (rc=%d): stdout %q stderr %q", rc, out.String(), errOut.String())
	}
	for _, want := range []string{"daemon: failed to start", "exit status 1", "daemon.log"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("expected %q in the error, got %q", want, errOut.String())
		}
	}
}

// TestStartDaemonStillReportsARunningDaemon keeps the answer for a detached daemon that is still
// running when the wait for its lock ends.
func TestStartDaemonStillReportsARunningDaemon(t *testing.T) {
	detachedSpawnStandIn(t, "exec sleep 60")
	root := bareApp(t)
	st := openTestStore(t, root)

	var out, errOut bytes.Buffer
	rc := StartDaemon(context.Background(), root, st, DaemonOptions{Detach: true}, &out, &errOut)
	if m := detachedSpawnPIDRe.FindStringSubmatch(out.String() + errOut.String()); m != nil {
		// Stop the stand-in this test started.
		if pid, err := strconv.Atoi(m[1]); err == nil && pid > 1 {
			defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
		}
	}
	if rc != 0 || !strings.Contains(out.String(), "daemon: started pid=") {
		t.Fatalf("expected the running daemon to be reported as started (rc=%d): stdout %q stderr %q", rc, out.String(), errOut.String())
	}
}

// TestGivenToDaemonReadsWhatTheLockRecords: the lock of a daemon started with --set lists what it
// was given, and a lock that lists nothing (a daemon given nothing, a lock of another process,
// metadata nobody can read) gives nothing.
func TestGivenToDaemonReadsWhatTheLockRecords(t *testing.T) {
	given := GivenToDaemon(`{"web":true,"set":["evaluator.enabled=false","pipeline.dispatch.workers=4"]}`)
	if len(given) != 2 || given[0] != "evaluator.enabled=false" || given[1] != "pipeline.dispatch.workers=4" {
		t.Fatalf("GivenToDaemon = %q, want the two values the lock lists, in its order", given)
	}
	if got := givenForTheRun(given); got != "--set evaluator.enabled=false --set pipeline.dispatch.workers=4" {
		t.Fatalf("givenForTheRun = %q, want what the command line said", got)
	}
	for name, metadata := range map[string]string{
		"a daemon given nothing":    `{"web":true,"interval":"1h0m0s"}`,
		"no metadata":               ``,
		"metadata that is no JSON":  `{"set":`,
		"a set that is no list":     `{"set":"pipeline.dispatch.workers=4"}`,
		"a list of what is no text": `{"set":[4,null,""]}`,
	} {
		if got := GivenToDaemon(metadata); len(got) != 0 {
			t.Errorf("%s: GivenToDaemon = %q, want nothing", name, got)
		}
	}
	if got := givenForTheRun(nil); got != "" {
		t.Errorf("givenForTheRun of nothing = %q, want nothing", got)
	}
}
