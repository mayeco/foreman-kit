package proc

import (
	"bytes"
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// TestRunBoundedReturnsWhenLeaderExitsAndKillsLeftovers runs a passing command that leaves a background
// process holding stdout (like `rails s & ... rspec`). It must report exit 0 right away instead of
// stalling for the grace period and reporting exit 1, and the leftover must not keep running.
func TestRunBoundedReturnsWhenLeaderExitsAndKillsLeftovers(t *testing.T) {
	started := time.Now()
	res, err := RunBounded(context.Background(), RunOptions{GracePeriod: 3 * time.Second},
		"bash", "-c", "sleep 300 & echo bgpid=$!; echo passed")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("RunBounded returned error: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "passed") {
		t.Fatalf("expected exit 0 with the command's output, got %+v", res)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("RunBounded waited %v for a leftover background process", elapsed)
	}
	m := regexp.MustCompile(`bgpid=(\d+)`).FindStringSubmatch(res.Output)
	if m == nil {
		t.Fatalf("no bgpid in output %q", res.Output)
	}
	bg, _ := strconv.Atoi(m[1])
	for deadline := time.Now().Add(2 * time.Second); store.IsPIDAlive(bg) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if store.IsPIDAlive(bg) {
		t.Fatalf("background process %d outlived the command", bg)
	}
}

// TestRunBoundedExitCodeContract keeps the exit codes callers depend on.
func TestRunBoundedExitCodeContract(t *testing.T) {
	ctx := context.Background()

	var out, errOut bytes.Buffer
	res, err := RunBounded(ctx, RunOptions{Stdout: &out, Stderr: &errOut}, "bash", "-c", "echo o; echo e >&2; exit 3")
	if err != nil || res.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %+v err=%v", res, err)
	}
	if !strings.Contains(out.String(), "o") || !strings.Contains(errOut.String(), "e") ||
		!strings.Contains(res.Output, "o") || !strings.Contains(res.Output, "e") {
		t.Fatalf("expected separate writers to receive their streams and Output both: out=%q err=%q output=%q",
			out.String(), errOut.String(), res.Output)
	}

	res, err = RunBounded(ctx, RunOptions{}, "foreman-no-such-binary")
	if err == nil || res.ExitCode != 127 {
		t.Fatalf("expected exit 127 and an error for a missing binary, got %+v err=%v", res, err)
	}

	res, _ = RunBounded(ctx, RunOptions{Timeout: 200 * time.Millisecond, GracePeriod: 100 * time.Millisecond}, "sleep", "30")
	if !res.TimedOut || res.ExitCode != 124 {
		t.Fatalf("expected a timeout (124), got %+v", res)
	}

	cctx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	res, _ = RunBounded(cctx, RunOptions{GracePeriod: 100 * time.Millisecond}, "sleep", "30")
	if res.ExitCode != 143 {
		t.Fatalf("expected exit 143 after the caller canceled, got %+v", res)
	}
}
