package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// evalReportGate is the part of a gate of evaluation-report --json that names the changes' outcomes.
type evalReportGate struct {
	Gate                   string         `json:"gate"`
	EnforceWouldStop       map[string]int `json:"enforce_would_stop"`
	EnforceWouldLetThrough map[string]int `json:"enforce_would_let_through"`
	Changes                []struct {
		Change  string `json:"change"`
		Outcome string `json:"outcome"`
	} `json:"changes"`
}

// TestEvaluationReportShowsWorkspaceOutcomes: evaluation-report names what became of each judged change
// (integrated, blocked with its reason, in flight) instead of calling every change pending or abandoned.
func TestEvaluationReportShowsWorkspaceOutcomes(t *testing.T) {
	tmp := t.TempDir()
	appDir := filepath.Join(tmp, "sample-app")
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(tmp, "state"))
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_APP_ROOT", appDir)
	ctx := context.Background()

	run := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errBuf bytes.Buffer
		code := Run(ctx, args, &out, &errBuf)
		return code, out.String(), errBuf.String()
	}
	if code, out, errOut := run("init", appDir, "--agents", "claude"); code != 0 {
		t.Fatalf("init failed (%d): %s %s", code, out, errOut)
	}

	if err := os.MkdirAll(filepath.Join(appDir, "openspec", "changes", "archive", "2026-11-12-landed-change"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(appDir)
	if err != nil {
		t.Fatal(err)
	}
	answer := func(change, status string) {
		t.Helper()
		if err := st.EmitEvent(ctx, store.EventInput{
			Event:  "evaluator.answer",
			Change: change,
			Payload: map[string]any{
				"gate":     "scenario_coverage",
				"change":   change,
				"mode":     "advisory",
				"status":   status,
				"answers":  `{"every_scenario_tested":0.9}`,
				"cost_usd": 0.0001,
				"cached":   false,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	answer("landed-change", "failed")
	answer("stuck-change", "passed")
	answer("building-change", "passed")
	if err := st.BlockChange(ctx, store.QuarantineRecord{ChangeName: "stuck-change", Status: "blocked", Reason: "gate_failed", Details: "go test failed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireLock(ctx, store.WorkerLeaseKey("building-change"), store.LockOptions{OwnerType: "worker"}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	code, out, errOut := run("evaluation-report", "--since", "1h", "--json")
	if code != 0 {
		t.Fatalf("evaluation-report --json failed (%d): %s", code, errOut)
	}
	var rep struct {
		Gates []evalReportGate `json:"gates"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("unexpected evaluation-report --json (%v):\n%s", err, out)
	}
	var g evalReportGate
	found := false
	for _, gate := range rep.Gates {
		if gate.Gate == "scenario_coverage" {
			g, found = gate, true
		}
	}
	if !found {
		t.Fatalf("no scenario_coverage gate in the report:\n%s", out)
	}
	outcomes := map[string]string{}
	for _, c := range g.Changes {
		outcomes[c.Change] = c.Outcome
	}
	for change, want := range map[string]string{"landed-change": "integrated", "stuck-change": "blocked (gate_failed)", "building-change": "in flight"} {
		if outcomes[change] != want {
			t.Errorf("outcome of %s = %q, want %q", change, outcomes[change], want)
		}
	}
	if len(g.EnforceWouldStop) != 1 || g.EnforceWouldStop["integrated"] != 1 {
		t.Errorf("enforce_would_stop = %v, want {integrated: 1}", g.EnforceWouldStop)
	}
	if len(g.EnforceWouldLetThrough) != 2 || g.EnforceWouldLetThrough["blocked (gate_failed)"] != 1 || g.EnforceWouldLetThrough["in flight"] != 1 {
		t.Errorf("enforce_would_let_through = %v, want {blocked (gate_failed): 1, in flight: 1}", g.EnforceWouldLetThrough)
	}
	if strings.Contains(out, "pending or abandoned") {
		t.Errorf("report still tallies pending or abandoned:\n%s", out)
	}

	code, out, errOut = run("evaluation-report", "--since", "1h")
	if code != 0 {
		t.Fatalf("evaluation-report failed (%d): %s", code, errOut)
	}
	for _, want := range []string{
		"enforce would stop 1 change(s): integrated 1",
		"- landed-change: repair the tests; now integrated",
		"enforce would let through 2 change(s): blocked (gate_failed) 1, in flight 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pending or abandoned") {
		t.Errorf("text report still says pending or abandoned:\n%s", out)
	}
}
