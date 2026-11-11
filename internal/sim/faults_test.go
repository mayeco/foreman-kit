package sim

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// TestFaultInjectionScenarios runs every fault-injection scenario on a fresh umbrella stack.
// It takes several minutes, so it is skipped with -short; set FOREMANKIT_SIM_FAULT to run one scenario.
func TestFaultInjectionScenarios(t *testing.T) {
	if testing.Short() {
		t.Skip("fault-injection simulation skipped in -short mode")
	}
	ctx := context.Background()
	names := FaultScenarios
	if only := os.Getenv("FOREMANKIT_SIM_FAULT"); only != "" {
		names = []string{only}
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
