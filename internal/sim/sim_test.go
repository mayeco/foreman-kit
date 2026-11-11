package sim

import (
	"bytes"
	"context"
	"testing"
)

func TestRepeatableSimulationStackAllScenarios(t *testing.T) {
	ctx := context.Background()
	stackDir := t.TempDir()

	var out bytes.Buffer
	// Pass 1: Run all 14 scenarios (including production resilience cases, migrate-from-files, the daemon's on-demand cycle and a full queue)
	results, err := RunScenario(ctx, stackDir, "all", &out)
	if err != nil {
		t.Fatalf("all scenarios pass failed: %v\noutput:\n%s", err, out.String())
	}
	if len(results) != 14 {
		t.Fatalf("expected 14 scenarios, got %d", len(results))
	}
	for _, r := range results {
		if !r.Passed || len(r.VerifiedChecks) == 0 {
			t.Fatalf("scenario %s did not pass: %+v", r.Scenario, r)
		}
	}

	// Pass 2: Re-run happy-path and merger-idempotent-push-and-stale-heal on the same stackDir to prove reset repeatability
	for _, sc := range []string{"happy-path", "merger-idempotent-push-and-stale-heal"} {
		out.Reset()
		if _, err := InitStack(ctx, stackDir, &out); err != nil {
			t.Fatalf("InitStack for repeat %s failed: %v", sc, err)
		}
		repRes, err := RunScenario(ctx, stackDir, sc, &out)
		if err != nil || len(repRes) != 1 || !repRes[0].Passed {
			t.Fatalf("repeat scenario %s failed: err=%v res=%+v\noutput:\n%s", sc, err, repRes, out.String())
		}
	}
}
