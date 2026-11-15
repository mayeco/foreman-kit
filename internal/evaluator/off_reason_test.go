package evaluator

import (
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

const offReasonGates = `gates:
  plan_review:
    mode: enforce
  change_risk:
    mode: advisory
  scenario_coverage:
    mode: "off"
  custom_gate:
    mode: "off"
    file: checks/custom.yaml
  silent:
    mode: ""
`

func TestOffReasonOfTheEvaluatorAndOfItsGates(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	on := judgeWithPolicy(t, "enabled: true\n"+offReasonGates)
	for _, file := range []string{"", "plan_review", "plan_review.yaml", "checks/unknown.yaml", "canary"} {
		if got := on.OffReason(file); got != "" {
			t.Errorf("OffReason(%q) of an enabled evaluator = %q, want it asked", file, got)
		}
	}
	if got := on.OffReason("change_risk"); got != "" {
		t.Errorf("an advisory gate is off: %q", got)
	}

	for _, file := range []string{"scenario_coverage", "scenario_coverage.yml", "silent", "checks/custom.yaml"} {
		got := on.OffReason(file)
		if got == "" || !strings.Contains(got, "is off") {
			t.Errorf("OffReason(%q) = %q, want the gate named off", file, got)
		}
	}
	if got := on.OffReason("scenario_coverage"); !strings.Contains(got, "scenario_coverage") {
		t.Errorf("the reason does not name the gate: %q", got)
	}
	if got := on.OffReason(""); got != "" {
		t.Errorf("the evaluator's own switch is on, yet OffReason(\"\") = %q", got)
	}

	disabled := judgeWithPolicy(t, "enabled: false\n"+offReasonGates)
	for _, file := range []string{"", "plan_review", "checks/unknown.yaml", "canary"} {
		got := disabled.OffReason(file)
		if got == "" || !strings.Contains(got, "the evaluator is off") {
			t.Errorf("OffReason(%q) of a disabled evaluator = %q", file, got)
		}
	}
}

func TestOffReasonFollowsTheSwitchGivenForTheRun(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	on := judgeWithPolicy(t, "enabled: true\n"+offReasonGates)
	if on.OffReason("plan_review") != "" {
		t.Fatal("plan_review is off before the run was given the switch")
	}
	settingstest.Give(t, settings.EvaluatorEnabled, "false")
	if got := on.OffReason("plan_review"); !strings.Contains(got, "the evaluator is off") {
		t.Errorf("a run given evaluator.enabled=false: %q", got)
	}
}
