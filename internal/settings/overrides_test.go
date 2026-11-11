package settings_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestParseOverrideTakesWhatConfigSetTakes: an argument of --set names a key of the catalog with
// its file and gives a value the setting accepts, written as the file would write it. Anything else
// is refused with what is wrong, before the run starts. A key that is not one value (a list, the
// arguments of a role, the commands of the agents) is edited in its file, as `config set` says:
// some of them are read straight from the file, and a value for one run would change nothing.
func TestParseOverrideTakesWhatConfigSetTakes(t *testing.T) {
	for arg, want := range map[string]string{
		"pipeline.dispatch.workers=3":             "3",
		"agents.limits.idle_timeout=never":        "never",
		"agents.roles.build.timeout = 30m":        "30m",
		"agents.roles.build.agents=[codex]":       "[codex]",
		"agents.roles.cycle.agents=codex,claude":  "codex,claude",
		"evaluator.enabled=false":                 "false",
		"evaluator.gates.plan_review.mode=off":    "off",
		"evaluator.when_unavailable=stop":         "stop",
		"daemon.cycle.every_when_out_of_work=10m": "10m",
	} {
		key, text, err := settings.ParseOverride(arg)
		if err != nil || text != want || !strings.HasPrefix(arg, string(key)) {
			t.Errorf("ParseOverride(%q) = %q, %q, %v", arg, key, text, err)
		}
	}
	for arg, want := range map[string]string{
		"pipeline.dispatch.workers":                  "--set takes <file>.<key>=<value>",
		"=3":                                         "--set takes <file>.<key>=<value>",
		"dispatch.workers=3":                         "--set dispatch.workers: not a setting",
		"pipeline.dispatch.workers=banana":           "--set pipeline.dispatch.workers: must be",
		"pipeline.dispatch.workers=":                 "--set pipeline.dispatch.workers: it has no value",
		"agents.limits.idle_timeout=20":              "--set agents.limits.idle_timeout: must be",
		"agents.permissions=everything":              "--set agents.permissions: must be",
		"pipeline.dispatch.workers=[1, 2":            "is not a value",
		"pipeline.repository.protected_paths=[a, b]": "--set pipeline.repository.protected_paths: it is not one value, and --set gives one: edit .foreman-kit/pipeline.yaml by hand",
		"agents.commands={}":                         "--set agents.commands: it is not one value",
		"agents.roles.build.args={claude: --fast}":   "--set agents.roles.build.args: it is not one value",
		"evaluator.abstain_band=[0.3, 0.7]":          "--set evaluator.abstain_band: it is not one value",
	} {
		if _, _, err := settings.ParseOverride(arg); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseOverride(%q): %v, want an error with %q", arg, err, want)
		}
	}
}

// TestAValueGivenForOneRunIsReadInPlaceOfTheFile: what --set gave a run is what every reader of
// that run gets for the key, whatever its file says, and the key says what replaced it. The file
// itself is judged as it stands: a value for one run mends nothing in it.
func TestAValueGivenForOneRunIsReadInPlaceOfTheFile(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, map[string]string{"pipeline": "dispatch:\n  workers: 2\n"})
	t.Setenv(settings.RunOverridesEnv, settings.EncodeOverrides(map[settings.Key]string{
		settings.Workers:              "6",
		settings.RoleTimeout("build"): "12m",
		settings.RoleAgents("plan"):   "[codex, claude]",
		settings.EvaluatorEnabled:     "false",
	}))

	var notes bytes.Buffer
	r := &settings.Reader{Root: root, Stderr: &notes}
	if got := r.Int(settings.Workers); got != 6 {
		t.Errorf("dispatch.workers = %d, want the 6 of --set", got)
	}
	if got := r.Duration(settings.RoleTimeout("build")); got != 12*time.Minute {
		t.Errorf("roles.build.timeout = %s, want the 12m of --set", got)
	}
	if got := strings.Join(r.Strings(settings.RoleAgents("plan")), " "); got != "codex claude" {
		t.Errorf("roles.plan.agents = %q, want the list of --set", got)
	}
	if r.Bool(settings.EvaluatorEnabled) {
		t.Error("evaluator.enabled is true, --set said false")
	}
	// A key --set did not name is the one of the file.
	if got := r.Int(settings.BlockedRetries); got != 1 {
		t.Errorf("blocked_changes.retries = %d, want the 1 of the file", got)
	}
	if _, source, _ := r.Lookup(settings.Workers); source != "--set pipeline.dispatch.workers" {
		t.Errorf("the source of dispatch.workers = %q", source)
	}
	d := r.Describe(settings.Workers)
	if d.OverriddenBy != "--set pipeline.dispatch.workers=6" || d.InEffect != "6" || d.Value != 2 {
		t.Errorf("Describe(dispatch.workers) = overridden by %q, in effect %q, in the file %v", d.OverriddenBy, d.InEffect, d.Value)
	}
	if notes.Len() != 0 {
		t.Errorf("notes: %s", notes.String())
	}
	if got := settings.RunOverrides(); len(got) != 4 || got[3] != "pipeline.dispatch.workers=6" {
		t.Errorf("RunOverrides() = %v", got)
	}

	// The file is judged alone: a key it lacks is still missing.
	settingstest.WriteFile(t, root, "pipeline", "")
	whole := (&settings.Reader{Root: root}).Problems("pipeline")
	if len(whole) != 0 {
		t.Fatalf("problems of a whole file: %v", whole)
	}
	if why := settings.Unusable(root, "pipeline"); why != "" {
		t.Errorf("a whole file with a value for one run: %s", why)
	}

	if err := settings.RunOverridesProblem(); err != nil {
		t.Errorf("what --set wrote is a problem: %v", err)
	}

	// What --set would not have accepted is not read: the key keeps the value of its file, the
	// values beside it hold, and the process is told what it was handed.
	settingstest.WriteFiles(t, root, map[string]string{"pipeline": "dispatch:\n  workers: 2\n"})
	t.Setenv(settings.RunOverridesEnv, `{"pipeline.dispatch.workers":"banana","pipeline.not.a.key":"1","pipeline.repository.protected_paths":"[a]","agents.limits.retries":"4"}`)
	notes.Reset()
	r = &settings.Reader{Root: root, Stderr: &notes}
	if got := r.Int(settings.Workers); got != 2 || notes.Len() != 0 {
		t.Errorf("a value --set could not have accepted: read %d, said %q", got, notes.String())
	}
	if got := r.Int(settings.AgentRetries); got != 4 {
		t.Errorf("the value beside it: limits.retries = %d, want 4", got)
	}
	if got := strings.Join(r.Strings(settings.ProtectedPaths), " "); got == "a" {
		t.Error("a list was read from what --set does not take")
	}
	if got := strings.Join(settings.RunOverrides(), " "); got != "agents.limits.retries=4" {
		t.Errorf("RunOverrides() = %q", got)
	}
	err := settings.RunOverridesProblem()
	if err == nil || !strings.Contains(err.Error(), "FOREMANKIT_SET holds what foreman-kit --set did not write (--set pipeline.dispatch.workers: must be") ||
		!strings.Contains(err.Error(), "unset it, and give the values of this run with --set") {
		t.Errorf("the problem of what was handed: %v", err)
	}

	t.Setenv(settings.RunOverridesEnv, "workers=9")
	if got := (&settings.Reader{Root: root}).Int(settings.Workers); got != 2 {
		t.Errorf("text that --set does not write was read: dispatch.workers = %d", got)
	}
	if err := settings.RunOverridesProblem(); err == nil || !strings.Contains(err.Error(), "it is not a list of values") {
		t.Errorf("the problem of text --set does not write: %v", err)
	}
	t.Setenv(settings.RunOverridesEnv, "")
	if err := settings.RunOverridesProblem(); err != nil {
		t.Errorf("no value handed is a problem: %v", err)
	}
}

// TestSetTakesTheKeysConfigSetTakes: over the whole catalog, --set refuses exactly the keys that
// `config set` refuses as not one value, and each of those says how it is changed.
func TestSetTakesTheKeysConfigSetTakes(t *testing.T) {
	for _, s := range settings.All() {
		_, _, err := settings.ParseOverride(string(s.Key) + "=x")
		refused := err != nil && strings.Contains(err.Error(), "it is not one value")
		if refused == s.Settable() {
			t.Errorf("%s: settable %v, refused as not one value %v (%v)", s.Key, s.Settable(), refused, err)
		}
		if !s.Settable() && s.Change == "" {
			t.Errorf("%s is not one value and does not say how it is changed", s.Key)
		}
	}
}
