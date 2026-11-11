package evaluator

import (
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"os"
	"path/filepath"
	"testing"
)

func judgeWithPolicy(t *testing.T, policy string) *Judge {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "evaluator.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	judge, err := NewJudge(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return judge
}

// enabled: false turns every gate off, whatever its mode, and the evaluator is not asked.
func TestEnabledFalseTurnsEveryGateOff(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "ts-test-key")
	const gates = "gates:\n  plan_review:\n    mode: enforce\n  change_risk:\n    mode: advisory\n"

	on := judgeWithPolicy(t, "enabled: true\nmodel: jev-1.14.0\n"+gates)
	if on.Policy().GateMode("plan_review") != "enforce" || !on.Available() || len(on.Policy().ActiveGates()) != 2 {
		t.Fatalf("an enabled evaluator: plan_review %s, available %v, active %v", on.Policy().GateMode("plan_review"), on.Available(), on.Policy().ActiveGates())
	}

	off := judgeWithPolicy(t, "enabled: false\nmodel: jev-1.14.0\n"+gates)
	if got := off.Policy().GateMode("plan_review"); got != "off" {
		t.Errorf("plan_review of a disabled evaluator is %s", got)
	}
	if off.Available() || len(off.Policy().ActiveGates()) != 0 {
		t.Errorf("a disabled evaluator is available (%v) or has active gates (%v)", off.Available(), off.Policy().ActiveGates())
	}
	// With no gate active it needs no key either.
	t.Setenv("TYPESAFE_API_KEY", "")
	if err := off.Policy().RequireAPIKeyForActiveGates(); err != nil {
		t.Errorf("a disabled evaluator asks for a key: %v", err)
	}
	if err := on.Policy().RequireAPIKeyForActiveGates(); err == nil {
		t.Error("active gates without a key were accepted")
	}
}

// The endpoint comes from the file, or from what --set gave the run. The key has one name: the
// provider's.
func TestBaseURLAndKeyOfTheEvaluator(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("JEV_API_KEY", "an-alias-nothing-reads")
	judge := judgeWithPolicy(t, "model: jev-1.14.0\nbase_url: https://evaluator.internal/v1\nretries: 0\nstate:\n  max_chars: 500\n  include_diffs: false\n")
	pol := judge.Policy()
	if pol.EffectiveBaseURL() != "https://evaluator.internal/v1" || pol.Retries != 0 || pol.State.MaxChars != 500 || *pol.State.IncludeDiffs {
		t.Errorf("the policy read from the file: %+v", pol)
	}
	settingstest.Give(t, settings.EvaluatorBaseURL, "http://127.0.0.1:9")
	if got := pol.EffectiveBaseURL(); got != "http://127.0.0.1:9" {
		t.Errorf("the endpoint the run was given is not used: %s", got)
	}
	if got := APIKeyFromEnv(); got != "" {
		t.Errorf("the alias of the key was read: %q", got)
	}
	t.Setenv("TYPESAFE_API_KEY", "ts-key")
	if got := APIKeyFromEnv(); got != "ts-key" {
		t.Errorf("the key was not read: %q", got)
	}
}
