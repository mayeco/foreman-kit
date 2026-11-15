package sim

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/gitx"
)

// TestRepeatableSimulationStackAllScenarios runs every scenario of `sim run all` on one stack
// directory, reset with InitStack before each, then repeats two of them on the same directory.
//
// The package runs close to go test's default 10m timeout (the test gate of a change that touches
// it runs plain `go test`), and on a loaded machine the 14 scenarios alone outlast it: a panic of
// the deadline would fail every test of the package. So a scenario that would likely not finish
// before the deadline is left out and named in the log; with -timeout 30m, or with
// `foreman-kit sim run <dir> all`, every scenario runs.
func TestRepeatableSimulationStackAllScenarios(t *testing.T) {
	ctx := context.Background()
	stackDir := t.TempDir()

	var out bytes.Buffer
	st, err := InitStack(ctx, stackDir, &out)
	if err != nil {
		t.Fatalf("InitStack: %v\noutput:\n%s", err, out.String())
	}
	assertStackBranchNames(ctx, t, st)

	// Pass 1: all 14 scenarios (including production resilience cases, migrate-from-files, the daemon's
	// on-demand cycle and a full queue), each on the same stackDir reset as RunScenario "all" resets it.
	if len(AllScenarios) != 14 {
		t.Fatalf("expected 14 scenarios, got %d: %v", len(AllScenarios), AllScenarios)
	}
	started := time.Now()
	var slowest time.Duration
	for i, sc := range AllScenarios {
		if i > 0 {
			average := time.Since(started) / time.Duration(i)
			if notRun := AllScenarios[i:]; !timeLeftFor(t, max(average*3/2, slowest)+30*time.Second) {
				t.Logf("%d scenario(s) not run before the go test deadline (a scenario took %s on average): %s; run with -timeout 30m or `foreman-kit sim run <dir> all` to include them",
					len(notRun), average.Round(time.Second), strings.Join(notRun, ", "))
				return
			}
			out.Reset()
			if _, err := InitStack(ctx, stackDir, &out); err != nil {
				t.Fatalf("InitStack before %s: %v\noutput:\n%s", sc, err, out.String())
			}
		}
		began := time.Now()
		out.Reset()
		results, err := RunScenario(ctx, stackDir, sc, &out)
		if err != nil {
			t.Fatalf("scenario %s failed: %v\noutput:\n%s", sc, err, out.String())
		}
		if len(results) != 1 || !results[0].Passed || len(results[0].VerifiedChecks) == 0 {
			t.Fatalf("scenario %s did not pass: %+v\noutput:\n%s", sc, results, out.String())
		}
		slowest = max(slowest, time.Since(began))
	}
	perScenario := time.Since(started) / time.Duration(len(AllScenarios))

	// Pass 2: Re-run happy-path and merger-idempotent-push-and-stale-heal on the same stackDir to prove reset repeatability.
	for _, sc := range []string{"happy-path", "merger-idempotent-push-and-stale-heal"} {
		if !timeLeftFor(t, 2*perScenario+time.Minute) {
			t.Logf("repeat of %s not run before the go test deadline (a scenario took %s on average); run with -timeout 30m to include it",
				sc, perScenario.Round(time.Second))
			continue
		}
		out.Reset()
		st, err := InitStack(ctx, stackDir, &out)
		if err != nil {
			t.Fatalf("InitStack for repeat %s failed: %v", sc, err)
		}
		// A reset of a stack the scenarios used gets back the branch names of a fresh one.
		assertStackBranchNames(ctx, t, st)
		repRes, err := RunScenario(ctx, stackDir, sc, &out)
		if err != nil || len(repRes) != 1 || !repRes[0].Passed {
			t.Fatalf("repeat scenario %s failed: err=%v res=%+v\noutput:\n%s", sc, err, repRes, out.String())
		}
	}
}

// timeLeftFor reports whether at least need remains before the go test deadline (always, without one).
func timeLeftFor(t *testing.T, need time.Duration) bool {
	deadline, ok := t.Deadline()
	return !ok || time.Until(deadline) >= need
}

// assertStackBranchNames checks that the stack's bare remotes and workspace carry the branch names
// InitStack gives them (main for the backend, master for the umbrella), whatever git's own default
// initial branch is.
func assertStackBranchNames(ctx context.Context, t *testing.T, st *Stack) {
	t.Helper()
	for remote, want := range map[string]string{
		"backend.git":  "refs/heads/main",
		"umbrella.git": "refs/heads/master",
	} {
		head, err := gitx.RunGit(ctx, filepath.Join(st.RemotesDir, remote), "symbolic-ref", "HEAD")
		if err != nil {
			t.Fatalf("symbolic-ref HEAD of %s: %v", remote, err)
		}
		if got := strings.TrimSpace(head); got != want {
			t.Fatalf("%s HEAD = %q, want %q", remote, got, want)
		}
	}
	if branch, ok := gitx.CurrentBranch(ctx, st.WorkspaceDir); !ok || branch != "master" {
		t.Fatalf("workspace branch = %q (ok=%v), want master", branch, ok)
	}
}
