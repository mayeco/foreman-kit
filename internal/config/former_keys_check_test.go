package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

func hasLine(lines []string, parts ...string) bool {
	for _, line := range lines {
		all := true
		for _, part := range parts {
			if !strings.Contains(line, part) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// A key an older foreman-kit read is an error, not an alias: check says where its value went. So
// is a key of today the file does not write, and a key nothing ever read.
func TestCheckRefusesAFormerKeyAndSaysWhereItWent(t *testing.T) {
	root, _ := setupTestUmbrella(t)
	pipeline := filepath.Join(root, filepath.FromSlash(settings.GateTimeout.FilePath()))
	raw, err := os.ReadFile(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	if rep := Check(root)[settings.GateTimeout.FilePath()]; len(rep.Errors) > 0 {
		t.Fatalf("the seed does not check clean: %v", rep.Errors)
	}

	edited := strings.Replace(string(raw), "  timeout: 45m", "  timout: 45m", 1) + "limits:\n  gate_seconds: 2700\n"
	if err := os.WriteFile(pipeline, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := Check(root)[settings.GateTimeout.FilePath()]
	for _, want := range [][]string{
		{"gates.timeout: missing", "the kit ships 45m"},
		{"gates.timout: not a setting of this file"},
		{"limits.gate_seconds: no longer read", "gates.timeout in .foreman-kit/pipeline.yaml", "foreman-kit update"},
	} {
		if !hasLine(rep.Errors, want...) {
			t.Errorf("check does not say %q:\n%s", want, strings.Join(rep.Errors, "\n"))
		}
	}

	// show marks the key the file lacks, with the value the kit ships.
	view, err := Show(root, "pipeline")
	if err != nil {
		t.Fatal(err)
	}
	if text := FormatShowText(view); !strings.Contains(text, "gates.timeout: (none) (MISSING; the kit ships 45m)") {
		t.Errorf("show does not mark the missing key:\n%s", text)
	}
}

// What an application wrote in the layout of before is refused file by file, and a file of today
// that is not there is missing, not read as the seed.
func TestCheckOfAnApplicationWrittenForAnOlderKit(t *testing.T) {
	root, _ := setupTestUmbrella(t)
	former, err := filepath.Abs(filepath.Join("testdata", "former", "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pipeline", "agents", "cycle", "exploration", "evaluator"} {
		raw, err := os.ReadFile(filepath.Join(former, name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".foreman-kit", name+".yaml"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"daemon", "visual_qa"} {
		if err := os.Remove(filepath.Join(root, ".foreman-kit", name+".yaml")); err != nil {
			t.Fatal(err)
		}
	}
	files := Check(root)
	for file, want := range map[string][]string{
		".foreman-kit/pipeline.yaml":    {"base_branch: no longer read", "repository.base_branch"},
		".foreman-kit/agents.yaml":      {"order: no longer read", "selection in .foreman-kit/agents.yaml"},
		".foreman-kit/cycle.yaml":       {"daemon.interval_minutes: no longer read", "cycle.every in .foreman-kit/daemon.yaml"},
		".foreman-kit/daemon.yaml":      {"missing (foreman-kit update writes it)"},
		".foreman-kit/visual_qa.yaml":   {"missing (foreman-kit update writes it)"},
		".foreman-kit/exploration.yaml": {"needs is no longer read", "session"},
		".foreman-kit/evaluator.yaml":   {"on_unavailable: no longer read", "when_unavailable"},
	} {
		if !hasLine(files[file].Errors, want...) {
			t.Errorf("%s: check does not say %q:\n%s", file, want, strings.Join(files[file].Errors, "\n"))
		}
	}
	if !ConfigNeedsRewrite(root) {
		t.Error("an application written for an older kit has nothing to rewrite")
	}
}

// Two values that do not fit together are said, never worked out of each other.
func TestCheckWarnsWhenTwoValuesDoNotFitTogether(t *testing.T) {
	root, _ := setupTestUmbrella(t)
	set := func(key settings.Key, value string) {
		t.Helper()
		if err := Set(root, string(key), value, false, nil); err != nil {
			t.Fatalf("set %s %s: %v", key, value, err)
		}
	}
	if rep := Check(root); len(rep[".foreman-kit/daemon.yaml"].Warnings)+len(rep[".foreman-kit/visual_qa.yaml"].Warnings) > 0 {
		t.Fatalf("the seeds warn: %+v %+v", rep[".foreman-kit/daemon.yaml"], rep[".foreman-kit/visual_qa.yaml"])
	}
	set(settings.CycleEveryWhenOutOfWork, "60m")
	set(settings.VisualQAExpiresAfter, "30m")
	set(settings.CycleBudget, "90m")
	set(settings.CycleExploring, "2h")
	files := Check(root)
	for file, want := range map[string][]string{
		".foreman-kit/daemon.yaml":    {"cycle.every_when_out_of_work: 1h is not below cycle.every (1h)", "never starts early"},
		".foreman-kit/visual_qa.yaml": {"expires_after: 30m is below time_budget (40m)"},
		".foreman-kit/cycle.yaml":     {"budget.total: 1h30m does not fit in roles.cycle.timeout in .foreman-kit/agents.yaml (1h)"},
	} {
		if !hasLine(files[file].Warnings, want...) {
			t.Errorf("%s: check does not warn %q:\n%s", file, want, strings.Join(files[file].Warnings, "\n"))
		}
		if len(files[file].Errors) > 0 {
			t.Errorf("%s: a warning came out as an error: %v", file, files[file].Errors)
		}
	}
	if !hasLine(files[".foreman-kit/cycle.yaml"].Warnings, "budget.exploring: 2h, above budget.total (1h30m)") {
		t.Errorf("check does not warn about the time of the strategy:\n%s", strings.Join(files[".foreman-kit/cycle.yaml"].Warnings, "\n"))
	}
	// A wait of the cycle shorter than a minute would judge without rest: it is not a value.
	if err := Set(root, string(settings.CycleEveryWhenOutOfWork), "10s", false, nil); err == nil || !strings.Contains(err.Error(), "1m or more, or never") {
		t.Errorf("a wait of ten seconds was accepted: %v", err)
	}
	set(settings.CycleEveryWhenOutOfWork, "never")
	if rep := Check(root)[".foreman-kit/daemon.yaml"]; len(rep.Warnings)+len(rep.Errors) > 0 {
		t.Errorf("never is not a problem: %+v", rep)
	}
}

// permissions: restricted holds for every agent: one of a role that has no restricted command cannot
// be used, and check says so.
func TestCheckRefusesAnAgentWithoutRestrictedCommandWhileRestricted(t *testing.T) {
	root, _ := setupTestUmbrella(t)
	if rep := Check(root)[".foreman-kit/agents.yaml"]; len(rep.Errors) > 0 {
		t.Fatalf("the agents of the test umbrella do not check clean: %v", rep.Errors)
	}
	if err := Set(root, string(settings.AgentPermissions), settings.PermissionsRestricted, false, nil); err == nil || !strings.Contains(err.Error(), "commands.copilot.restricted: missing") {
		t.Fatalf("restricted was accepted with copilot in a role (it has no restricted command): %v", err)
	}
	// Without copilot in a role, every agent left has one.
	if err := Set(root, string(settings.RoleAgents("plan")), "claude", false, nil); err != nil {
		t.Fatal(err)
	}
	if err := Set(root, string(settings.AgentPermissions), settings.PermissionsRestricted, false, nil); err != nil {
		t.Fatalf("restricted refused although every agent of a role has a restricted command: %v", err)
	}
	if got := Permissions(root); got != settings.PermissionsRestricted {
		t.Fatalf("permissions = %q", got)
	}
}

// TestCheckNamesAFormerVariableThatIsStillSet: a variable an older foreman-kit read to override a
// key is an error of `foreman-kit config check` while it is set, with what does its job now, and
// `foreman-kit config show env` lists it under what the environment still carries. Neither prints
// a key that was a secret.
func TestCheckNamesAFormerVariableThatIsStillSet(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, nil)
	hasEnvironmentError := func() []string {
		t.Helper()
		var found []string
		for _, text := range Check(root)["application"].Errors {
			if strings.Contains(text, "is set and no longer read") {
				found = append(found, text)
			}
		}
		return found
	}
	if found := hasEnvironmentError(); len(found) != 0 {
		t.Fatalf("a clean environment: %v", found)
	}

	t.Setenv("OPENSPEC_WORKERS", "8")
	t.Setenv("OPENSPEC_LLM_API_KEY", "sk-very-secret")
	t.Setenv("TYPESAFE_API_KEY", "ts-very-secret")
	found := hasEnvironmentError()
	if len(found) != 2 {
		t.Fatalf("errors about the environment: %v", found)
	}
	all := strings.Join(found, "\n")
	for _, want := range []string{
		"OPENSPEC_WORKERS=8 is set and no longer read: write dispatch.workers in .foreman-kit/pipeline.yaml, or give it to one run with --set pipeline.dispatch.workers=<value>, and take it out of the environment (unset it, or delete its line from .env)",
		"OPENSPEC_LLM_API_KEY is set and no longer read: the key of the diagnostics assistant is read from FOREMANKIT_DIAGNOSTICS_API_KEY",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("config check does not say %q:\n%s", want, all)
		}
	}

	view, err := Show(root, "env")
	if err != nil {
		t.Fatal(err)
	}
	text := FormatShowText(view)
	for _, want := range []string{
		"== Environment: secrets, this machine, and what the kit hands to what it runs. No variable changes a setting",
		"TYPESAFE_API_KEY: the key of the evaluator  NOW: TYPESAFE_API_KEY is set",
		"FOREMANKIT_HOME (default ~/.foreman-kit): where the state of the applications of this machine lives",
		"OPENSPEC_WORKERS: no longer read: write dispatch.workers in .foreman-kit/pipeline.yaml",
		"NOW: OPENSPEC_WORKERS=8",
		"NOW: OPENSPEC_LLM_API_KEY is set",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("config show env does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(all+text, "very-secret") {
		t.Fatalf("a secret was printed:\n%s\n%s", all, text)
	}

	// What a run was given is shown, with the key it replaced.
	settingstest.Give(t, settings.Workers, "5")
	view, err = Show(root, "")
	if err != nil {
		t.Fatal(err)
	}
	text = FormatShowText(view)
	if !strings.Contains(text, "== This run: --set pipeline.dispatch.workers=5") {
		t.Errorf("config show does not say what the run was given:\n%s", text)
	}
	// The line of the key shows what its file holds and what replaces it, for this run and no
	// longer: nothing was exported, and no shell holds it.
	if !strings.Contains(text, "dispatch.workers: 2  (--set pipeline.dispatch.workers=5 replaces it for this run)") {
		t.Errorf("the line of the key does not say what replaces its value for this run:\n%s", text)
	}
}
