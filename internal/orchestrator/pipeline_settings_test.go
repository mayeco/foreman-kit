package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

func writePipelineYAML(t *testing.T, yamlText string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "pipeline.yaml"), []byte(yamlText), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestLoadPipelineSettingsKeepsValidKeysWhenOneHasTheWrongType verifies that one mistyped value does
// not discard the whole pipeline.yaml: the keys that are right are read as written, and the one that
// is not is read as the value the kit ships by whoever only shows it. The dispatcher and the merger
// do not work with it: both say which key keeps them.
func TestLoadPipelineSettingsKeepsValidKeysWhenOneHasTheWrongType(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, map[string]string{"pipeline": "repository:\n  base_branch: main\ndispatch:\n  workers: \"3\"\n  priority_bump_every: never\ngates:\n  timeout: soon\nmerge:\n  conflict_rebuilds: 0\n  base_recheck_after: never\nlocks:\n  stale_after: 10m\n"})
	cfg := LoadPipelineSettings(root)
	if cfg.BaseBranch != "main" {
		t.Fatalf("BaseBranch = %q, want main", cfg.BaseBranch)
	}
	for who, why := range map[string]string{"dispatcher": cfg.DispatcherWaits(), "merger": cfg.MergerWaits()} {
		if !strings.Contains(why, ".foreman-kit/pipeline.yaml: gates.timeout: must be a time with its unit (30s, 15m, 2h) (is soon)") {
			t.Fatalf("the %s does not say which key keeps it: %q", who, why)
		}
	}
	if cfg.Workers != 3 || cfg.LocksStaleAfter != 10*time.Minute || cfg.ConflictRebuilds != 0 || cfg.PriorityBumpEvery != 0 || cfg.BaseRecheckAfter != 0 {
		t.Fatalf("expected numeric strings and valid values to apply, got %+v", cfg)
	}
	if cfg.GateTimeout != 45*time.Minute {
		t.Fatalf("expected an invalid gates.timeout to read as the 45m the kit ships, got %s", cfg.GateTimeout)
	}
	// The limits of an agent call come from .foreman-kit/agents.yaml, one per role.
	if cfg.PlanTimeout != 30*time.Minute || cfg.BuildTimeout != 2*time.Hour || cfg.RepairRounds != 2 || cfg.OutageGiveUpAfter != 3 || cfg.OutageMaxWait != 15*time.Minute {
		t.Fatalf("the values the kit ships for what the files do not write: %+v", cfg)
	}
}

// TestLoadPipelineSettingsHasNoBaseBranchWhenTheFileDoesNotNameOne: the base branch has no value of
// the kit's own. A file that does not write it, or that is not valid YAML, names none, and a
// dispatcher and a merger then refuse to work: reading it as another branch made the merger publish
// to that branch, creating it on origin when it did not exist.
func TestLoadPipelineSettingsHasNoBaseBranchWhenTheFileDoesNotNameOne(t *testing.T) {
	for name, yamlText := range map[string]string{
		"a section given twice":      "repository:\n  base_branch: main\ndispatch:\n  workers: 4\ngates:\n  timeout: 10m\ndispatch:\n  workers: 5\n",
		"the key is not written":     "dispatch:\n  workers: 4\n",
		"the key of an older kit":    "base_branch: main\ndispatch:\n  workers: 4\n",
		"not the name of a branch":   "repository:\n  base_branch: main branch\n",
		"written with nothing in it": "repository:\n  base_branch:\n",
	} {
		root := writePipelineYAML(t, yamlText)
		cfg := LoadPipelineSettings(root)
		if cfg.BaseBranch != "" {
			t.Errorf("%s: BaseBranch = %q, want none", name, cfg.BaseBranch)
		}
		for who, why := range map[string]string{"dispatcher": cfg.DispatcherWaits(), "merger": cfg.MergerWaits()} {
			if !strings.Contains(why, "repository.base_branch in .foreman-kit/pipeline.yaml is missing or not a branch name") {
				t.Errorf("%s: the %s does not say why it cannot work: %q", name, who, why)
			}
		}
	}

	root := writePipelineYAML(t, "dispatch:\n  workers: 4\n")
	settingstest.Give(t, settings.EvaluatorEnabled, "false")
	st, err := store.OpenAt(filepath.Join(t.TempDir(), "state.db"), "ws-no-base-branch", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	disp, err := NewDispatcher(DispatcherOptions{Root: root, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "dispatcher: repository.base_branch in .foreman-kit/pipeline.yaml is missing") {
		t.Errorf("a dispatcher ran without a base branch: %v", err)
	}
	if err := disp.Serve(context.Background()); err == nil || !strings.Contains(err.Error(), "foreman-kit config set pipeline.repository.base_branch <branch>") {
		t.Errorf("a continuous dispatcher ran without a base branch: %v", err)
	}
	merger, err := NewMerger(MergerOptions{Root: root, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if err := merger.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "merger: repository.base_branch in .foreman-kit/pipeline.yaml is missing") {
		t.Errorf("a merger ran without a base branch: %v", err)
	}
	if err := merger.Serve(context.Background()); err == nil || !strings.Contains(err.Error(), "nothing is integrated") {
		t.Errorf("a continuous merger ran without a base branch: %v", err)
	}

	// --set names another branch for one run, over files that are whole.
	whole := t.TempDir()
	settingstest.WriteFiles(t, whole, nil)
	settingstest.Give(t, settings.BaseBranch, "release")
	if cfg := LoadPipelineSettings(whole); cfg.BaseBranch != "release" || cfg.DispatcherWaits() != "" || cfg.MergerWaits() != "" {
		t.Errorf("the base branch the run was given: %q (%s) (%s)", cfg.BaseBranch, cfg.DispatcherWaits(), cfg.MergerWaits())
	}
}

// TestDispatcherAndMergerWaitForTheFilesEachReads: each of the two starts no new work while a file
// it reads lacks a key, holds a value the key does not accept or writes a key nothing reads, and
// neither waits for a file it does not read. The dispatcher also waits for a role that lists no
// agent: nothing runs in the place of an agent nobody named.
func TestDispatcherAndMergerWaitForTheFilesEachReads(t *testing.T) {
	without := func(t *testing.T, file, key string) string {
		t.Helper()
		root := t.TempDir()
		settingstest.WriteFiles(t, root, nil)
		path := filepath.Join(root, ".foreman-kit", file+".yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var kept []string
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) != key && !strings.HasPrefix(strings.TrimSpace(line), key+" ") {
				kept = append(kept, line)
			}
		}
		if len(kept) == len(strings.Split(string(raw), "\n")) {
			t.Fatalf("%s has no line %q to remove", path, key)
		}
		if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}

	whole := t.TempDir()
	settingstest.WriteFiles(t, whole, nil)
	if cfg := LoadPipelineSettings(whole); cfg.DispatcherWaits() != "" || cfg.MergerWaits() != "" {
		t.Fatalf("on the files the kit ships: dispatcher %q, merger %q", cfg.DispatcherWaits(), cfg.MergerWaits())
	}

	for _, c := range []struct {
		file, line         string
		dispatcher, merger bool
		names              string
	}{
		{"pipeline", "timeout: 45m", true, true, ".foreman-kit/pipeline.yaml: gates.timeout: missing"},
		{"agents", "idle_cpu_percent: 5", true, false, ".foreman-kit/agents.yaml: limits.idle_cpu_percent: missing"},
		{"evaluator", "when_unavailable: pass", true, true, ".foreman-kit/evaluator.yaml: when_unavailable: missing"},
		{"daemon", "every: 60m", false, false, ""},
		{"cycle", "max_open: 8", false, false, ""},
	} {
		cfg := LoadPipelineSettings(without(t, c.file, c.line))
		if got := cfg.DispatcherWaits(); (got != "") != c.dispatcher || !strings.Contains(got, c.names) {
			t.Errorf("config/%s.yaml without %q: the dispatcher says %q", c.file, c.line, got)
		}
		if got := cfg.MergerWaits(); (got != "") != c.merger || (c.merger && !strings.Contains(got, c.names)) {
			t.Errorf("config/%s.yaml without %q: the merger says %q", c.file, c.line, got)
		}
	}

	// A role that lists no agent: the dispatcher starts nothing and says which role.
	settingstest.WriteFile(t, whole, "agents", "roles:\n  build:\n    agents: []\n")
	settingstest.Give(t, settings.EvaluatorEnabled, "false")
	st, err := store.OpenAt(filepath.Join(t.TempDir(), "state.db"), "ws-role-without-agents", whole)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	disp, err := NewDispatcher(DispatcherOptions{Root: whole, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "dispatcher: roles.build.agents in .foreman-kit/agents.yaml lists no agent") {
		t.Errorf("a dispatcher ran with a role that lists no agent: %v", err)
	}
}
