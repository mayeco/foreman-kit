package orchestrator

import (
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// The dispatcher waits for what `foreman-kit config check` rejects in the files it reads, and the
// merger only for the files it reads: an agents.yaml whose role names an agent with no commands
// keeps the dispatcher from starting, an evaluator.yaml whose abstain band is out of order keeps both.
func TestDispatcherAndMergerWaitForWhatConfigCheckRejects(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, map[string]string{"pipeline": "repository:\n  base_branch: main\n"})
	if cfg := LoadPipelineSettings(root); cfg.DispatcherWaits() != "" || cfg.MergerWaits() != "" {
		t.Fatalf("on the files the kit ships: dispatcher %q, merger %q", cfg.DispatcherWaits(), cfg.MergerWaits())
	}

	settingstest.WriteFile(t, root, "agents", "roles:\n  plan:\n    agents: [ghost-agent]\n")
	cfg := LoadPipelineSettings(root)
	if got := cfg.DispatcherWaits(); !strings.Contains(got, ".foreman-kit/agents.yaml: ") || !strings.Contains(got, "ghost-agent") {
		t.Fatalf("the dispatcher on an agents.yaml that config check rejects: %q", got)
	}
	if got := cfg.MergerWaits(); got != "" {
		t.Fatalf("the merger does not read agents.yaml: %q", got)
	}

	settingstest.WriteFile(t, root, "agents", "")
	settingstest.WriteFile(t, root, "evaluator", "abstain_band: [0.9, 0.1]\n")
	cfg = LoadPipelineSettings(root)
	for who, got := range map[string]string{"dispatcher": cfg.DispatcherWaits(), "merger": cfg.MergerWaits()} {
		if !strings.Contains(got, ".foreman-kit/evaluator.yaml: abstain_band: must be [low, high] between 0 and 1") {
			t.Fatalf("the %s on an abstain band out of order: %q", who, got)
		}
	}
}
