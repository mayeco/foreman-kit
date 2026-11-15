package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// everySetOfFiles names the files each process of the kit reads, as the readers ask for them.
var everySetOfFiles = map[string][]string{
	"dispatcher":     settings.DispatcherFiles,
	"merger":         settings.MergerFiles,
	"cycle":          settings.CycleFiles,
	"visual session": settings.VisualSessionFiles,
	"daemon":         settings.DaemonFiles,
	"agent call":     settings.AgentCallFiles,
}

// A file `config check` rejects keeps what reads it from starting new work, whatever the rule that
// rejects it lives in: the catalog or the checks of this package.
func TestAFileThatConfigCheckRejectsKeepsItsReadersFromStartingWork(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		file      string
		want      string
	}{
		{"an abstain band out of order", map[string]string{settings.EvaluatorFile: "abstain_band: [0.9, 0.1]\n"},
			settings.EvaluatorFile, "abstain_band: must be [low, high] between 0 and 1"},
		{"a role that names an agent with no commands", map[string]string{"agents": "roles:\n  plan:\n    agents: [ghost-agent]\n"},
			"agents", "ghost-agent"},
		{"a command with a placeholder that does not exist", map[string]string{"agents": "commands:\n  claude:\n    run: \"claude --model {{model}} -p {{prompt}}\"\n"},
			"agents", "{{model}} is not a placeholder"},
		{"a visual session slot a worker takes", map[string]string{"pipeline": "dispatch:\n  workers: 4\n", "visual_qa": "slot: 3\n"},
			"visual_qa", "3 is taken"},
		{"a strategy that cannot run", map[string]string{"exploration": "strategies:\n  - slug: probe\n    kind: bogus\n    directive: look\n    session: cycle\n"},
			"exploration", "kind must be one of"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			settingstest.WriteFiles(t, root, tc.overrides)
			rel := configFile(tc.file)
			if errs := Check(root)[rel].Errors; len(errs) == 0 {
				t.Fatalf("config check reports no error about %s", rel)
			}
			for who, files := range everySetOfFiles {
				why := Unusable(root, files...)
				if reads := slices.Contains(files, tc.file); reads {
					if !strings.Contains(why, rel+": ") || !strings.Contains(why, tc.want) {
						t.Errorf("the %s reads %s and is told %q, not %q", who, rel, why, tc.want)
					}
				} else if why != "" {
					t.Errorf("the %s does not read %s and is told %q", who, rel, why)
				}
			}
		})
	}
}

// The answer names the first error and says how many more there are, as the catalog's does.
func TestUnusableCountsTheErrorsOfConfigCheck(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	root := t.TempDir()
	settingstest.WriteFiles(t, root, map[string]string{"agents": "roles:\n  plan:\n    agents: [ghost-agent]\n"})
	errs := Check(root)[AgentsFile].Errors
	if len(errs) < 2 {
		t.Fatalf("expected the ghost agent to cost several errors, config check says %v", errs)
	}
	want := fmt.Sprintf("%s: %s (and %d more: foreman-kit config check lists them)", AgentsFile, errs[0], len(errs)-1)
	if len(errs) == 2 {
		want = AgentsFile + ": " + errs[0] + " (and 1 more: foreman-kit config check lists them)"
	}
	if got := Unusable(root, settings.AgentCallFiles...); got != want {
		t.Fatalf("Unusable = %q, want %q", got, want)
	}
	settingstest.WriteFiles(t, root, map[string]string{"evaluator": "abstain_band: [2, 3]\n"})
	if got, want := Unusable(root, settings.MergerFiles...), configFile(settings.EvaluatorFile)+": "; !strings.HasPrefix(got, want) || !strings.HasSuffix(got, " (foreman-kit config check)") {
		t.Fatalf("one error: %q", got)
	}
}

// A file the catalog finds a problem in keeps the catalog's words, and a warning keeps nothing.
func TestUnusableKeepsTheCatalogWordsAndIgnoresWarnings(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	root := t.TempDir()
	settingstest.WriteFiles(t, root, map[string]string{"evaluator": "model: latest\n"})
	if rep := Check(root)[configFile(settings.EvaluatorFile)]; len(rep.Warnings) == 0 {
		t.Fatalf("the test needs a warning, config check says none: %+v", rep)
	}
	if why := Unusable(root, settings.DispatcherFiles...); why != "" {
		t.Fatalf("a warning keeps work from starting: %s", why)
	}
	path := filepath.Join(root, filepath.FromSlash(AgentsFile))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := regexp.MustCompile(`(?m)^\s*idle_cpu_percent:.*\n`).ReplaceAllString(string(raw), "")
	if cut == string(raw) {
		t.Fatal("the agents file has no idle_cpu_percent line to remove")
	}
	if err := os.WriteFile(path, []byte(cut), 0o644); err != nil {
		t.Fatal(err)
	}
	want := settings.Unusable(root, settings.AgentCallFiles...)
	if want == "" || Unusable(root, settings.AgentCallFiles...) != want {
		t.Fatalf("a missing key: config.Unusable %q, settings.Unusable %q", Unusable(root, settings.AgentCallFiles...), want)
	}
}

// A freshly seeded application is usable for every process.
func TestASeededApplicationIsUsableForEveryProcess(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	root := t.TempDir()
	settingstest.WriteFiles(t, root, nil)
	for who, files := range everySetOfFiles {
		if why := Unusable(root, files...); why != "" {
			t.Errorf("the %s is told %q on the files the kit ships", who, why)
		}
	}
}
