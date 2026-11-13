package sim

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// fullFaultSuiteBudget is the least go test deadline under which TestFaultInjectionScenarios runs
// every scenario: the suite takes about 15 minutes.
const fullFaultSuiteBudget = 30 * time.Minute

// TestFaultInjectionScenarios runs every fault-injection scenario on a fresh umbrella stack.
// It takes about 15 minutes, so it is skipped with -short or under a deadline shorter than
// fullFaultSuiteBudget; set FOREMANKIT_SIM_FAULT to run one scenario.
func TestFaultInjectionScenarios(t *testing.T) {
	if testing.Short() {
		t.Skip("fault-injection simulation skipped in -short mode")
	}
	ctx := context.Background()
	names := FaultScenarios
	if only := os.Getenv("FOREMANKIT_SIM_FAULT"); only != "" {
		names = []string{only}
	} else if deadline, ok := t.Deadline(); ok && time.Until(deadline) < fullFaultSuiteBudget {
		// The whole suite outlasts go test's default 10m timeout, so a plain `go test ./internal/sim`
		// (the test gate of a change that touches this package) would always time out.
		t.Skipf("the full fault suite needs about %s: run it with -timeout %s, with FOREMANKIT_SIM_FAULT=<scenario>, or with `foreman-kit sim run <dir> faults`",
			fullFaultSuiteBudget, fullFaultSuiteBudget)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			stackDir := t.TempDir()
			var out bytes.Buffer
			if _, err := InitStack(ctx, stackDir, &out); err != nil {
				t.Fatalf("InitStack: %v\n%s", err, out.String())
			}
			results, err := RunScenario(ctx, stackDir, name, &out)
			if err != nil {
				t.Fatalf("%s failed: %v\noutput:\n%s", name, err, out.String())
			}
			if len(results) != 1 || !results[0].Passed || len(results[0].VerifiedChecks) == 0 {
				t.Fatalf("%s did not pass: %+v", name, results)
			}
			for _, c := range results[0].VerifiedChecks {
				t.Logf("ok: %s", c)
			}
			if results[0].Details != "" {
				t.Logf("notes: %s", results[0].Details)
			}
		})
	}
}

// TestFaultChildPushDuringPublish runs the two scenarios in which a person pushes to a child
// repository while the merger publishes a change, and asserts their outcomes by check name: both
// commits on the child's base, a reachable gitlink and the change's final state.
func TestFaultChildPushDuringPublish(t *testing.T) {
	if testing.Short() {
		t.Skip("fault-injection simulation skipped in -short mode")
	}
	ctx := context.Background()
	want := []string{
		"person's commit on the child's base",
		"change's commit on the child's base",
		"umbrella gitlink reachable and on the child's base",
		"change state: integrated",
	}
	for _, name := range []string{"fault-git-child-push-during-pushing-children", "fault-git-child-push-before-pushing-root"} {
		t.Run(name, func(t *testing.T) {
			stackDir := t.TempDir()
			var out bytes.Buffer
			if _, err := InitStack(ctx, stackDir, &out); err != nil {
				t.Fatalf("InitStack: %v\n%s", err, out.String())
			}
			results, err := RunScenario(ctx, stackDir, name, &out)
			if err != nil {
				t.Fatalf("%s failed: %v\noutput:\n%s", name, err, out.String())
			}
			if len(results) != 1 || !results[0].Passed {
				t.Fatalf("%s did not pass: %+v", name, results)
			}
			for _, w := range want {
				found := false
				for _, c := range results[0].VerifiedChecks {
					if strings.HasPrefix(c, w) {
						found = true
					}
				}
				if !found {
					t.Errorf("%s: no verified check starts with %q; checks: %q", name, w, results[0].VerifiedChecks)
				}
			}
		})
	}
}
