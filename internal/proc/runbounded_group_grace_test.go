package proc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// groupGraceSignalWriter closes ready the first time the output it receives contains marker.
type groupGraceSignalWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	marker string
	ready  chan struct{}
	once   sync.Once
}

func (w *groupGraceSignalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.marker) {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

func (w *groupGraceSignalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// groupGraceRun runs script through bash under RunBounded and cancels it once its output contains
// "ready", like a gate stopped by Ctrl-C, a daemon stop or its time limit.
func groupGraceRun(t *testing.T, grace time.Duration, env []string, script string) (*RunResult, time.Duration, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &groupGraceSignalWriter{marker: "ready", ready: make(chan struct{})}
	go func() {
		select {
		case <-out.ready:
			cancel()
		case <-ctx.Done():
		}
	}()
	started := time.Now()
	res, err := RunBounded(ctx, RunOptions{
		Env:         append(os.Environ(), env...),
		Stdout:      out,
		Stderr:      out,
		Timeout:     time.Minute,
		GracePeriod: grace,
	}, "bash", "-c", script)
	if err != nil {
		t.Fatalf("RunBounded returned error: %v", err)
	}
	select {
	case <-out.ready:
	default:
		t.Fatalf("the command never reported ready: %+v", res)
	}
	return res, time.Since(started), out.String()
}

// TestRunBoundedGroupKeepsGraceAfterLeaderExits stops a compound shell command, the usual shape of a
// gate (`cd web && npm test`). bash, the group leader, dies on SIGTERM at once; the command it was
// running handles the same signal and needs a moment to clean up. It must get the grace period instead
// of a SIGKILL right after the leader's exit, and RunBounded must return as soon as the group is gone.
func TestRunBoundedGroupKeepsGraceAfterLeaderExits(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleaned")
	script := `
bash -c 'trap "sleep 0.5; echo cleaned > \"$GROUP_GRACE_MARKER\"; exit 0" TERM; echo ready; while :; do sleep 0.05; done' &
wait
`
	const grace = 30 * time.Second
	res, elapsed, output := groupGraceRun(t, grace, []string{"GROUP_GRACE_MARKER=" + marker}, script)

	if b, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(b)) != "cleaned" {
		t.Fatalf("the command was killed before it finished handling SIGTERM (marker: %q, %v); result %+v, output %q", b, err, res, output)
	}
	if res.ExitCode != 143 {
		t.Fatalf("expected exit 143 for the canceled leader, got %+v", res)
	}
	if elapsed > grace/2 {
		t.Fatalf("RunBounded took %v: it must return when the group is gone, not wait out the grace period", elapsed)
	}
}

// TestRunBoundedGroupGraceEndsWithKill keeps the bound: a member that ignores SIGTERM is killed when
// the grace period ends, and RunBounded returns then.
func TestRunBoundedGroupGraceEndsWithKill(t *testing.T) {
	script := `
bash -c 'trap "" TERM; echo stubborn=$$; echo ready; while :; do sleep 0.05; done' &
wait
`
	const grace = 700 * time.Millisecond
	res, elapsed, output := groupGraceRun(t, grace, nil, script)

	m := regexp.MustCompile(`stubborn=(\d+)`).FindStringSubmatch(output)
	if m == nil {
		t.Fatalf("no stubborn pid in output %q", output)
	}
	stubborn, _ := strconv.Atoi(m[1])
	for deadline := time.Now().Add(5 * time.Second); store.IsPIDAlive(stubborn) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if store.IsPIDAlive(stubborn) {
		t.Fatalf("process %d ignored SIGTERM and outlived the command", stubborn)
	}
	if elapsed < grace/2 || elapsed > 20*time.Second {
		t.Fatalf("RunBounded returned after %v with a %v grace period; result %+v", elapsed, grace, res)
	}
}

// TestParseProcStatStateGroup reads the state and process group from /proc/<pid>/stat lines whose
// command name contains spaces and parentheses.
func TestParseProcStatStateGroup(t *testing.T) {
	cases := []struct {
		stat      string
		wantState byte
		wantPgrp  int
		wantOK    bool
	}{
		{"4242 (bash) S 4000 4242 4000 34816 4242 4194304 1 2 3", 'S', 4242, true},
		{"77 (tmux: server) S 1 77 77 0 -1 4194368 5 6", 'S', 77, true},
		{"901 (a) R 1 2 (b) Z 900 880 880 0 -1 4", 'Z', 880, true},
		{"12 (sd-pam)) X 1 12 12 0 -1", 'X', 12, true},
		{"55 (node) Z 54 50 50 0 -1 4228100\n", 'Z', 50, true},
		{"55 (node", 0, 0, false},
		{"55 (node) Z 54", 0, 0, false},
		{"55 (node) Z 54 fifty 50", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		state, pgrp, ok := parseProcStatStateGroup(c.stat)
		if state != c.wantState || pgrp != c.wantPgrp || ok != c.wantOK {
			t.Errorf("parseProcStatStateGroup(%q) = %q, %d, %v; want %q, %d, %v", c.stat, state, pgrp, ok, c.wantState, c.wantPgrp, c.wantOK)
		}
	}
}
