package config

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestSetReadsTheAgentsOfARoleAsTheirFileWritesThem: `foreman-kit config set` takes the agents of
// a role the way a person types them (claude,codex) and the way .foreman-kit/agents.yaml holds them
// ([claude, codex]). Either way an agent the application does not list is refused, in the same words.
func TestSetReadsTheAgentsOfARoleAsTheirFileWritesThem(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, nil)
	key := string(settings.RoleAgents("build"))
	var out bytes.Buffer
	for _, text := range []string{"claude", "[claude]", " [ claude ] "} {
		if err := Set(root, key, text, true, &out); err != nil {
			t.Errorf("config set %s %q --dry-run: %v", key, text, err)
		}
	}
	typed := Set(root, key, "claude,codex", true, &out)
	written := Set(root, key, "[claude, codex]", true, &out)
	if typed == nil || written == nil || typed.Error() != written.Error() ||
		!strings.Contains(typed.Error(), "codex not among the agents of .foreman-kit/agents.yaml (claude)") {
		t.Errorf("an agent the application does not list: typed %v, as the file writes it %v", typed, written)
	}
	if err := Set(root, key, "[claude", true, &out); err == nil || !strings.Contains(err.Error(), "must list agents among") {
		t.Errorf("a list that is not closed: %v", err)
	}
	if err := Set(root, key, "[]", true, &out); err == nil || !strings.Contains(err.Error(), "lists no agent") {
		t.Errorf("a list of no agent: %v", err)
	}
}

// TestAValueGivenToARunIsAskedOfTheApplication: --set checks a value against its key when it is
// given. What only the application can say of it (an agent it does not list, a slot one of its
// workers takes) is said by CheckRunOverrides in the words `config set` has for the same value,
// so that the run stops before it starts anything instead of failing at its first agent.
func TestAValueGivenToARunIsAskedOfTheApplication(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, map[string]string{"pipeline": "dispatch:\n  workers: 2\n"})
	if err := CheckRunOverrides(root); err != nil {
		t.Fatalf("a run given nothing: %v", err)
	}
	build := settings.RoleAgents("build")

	settingstest.Give(t, settings.GateTimeout, "50m")
	settingstest.Give(t, build, "[claude]")
	if err := CheckRunOverrides(root); err != nil {
		t.Fatalf("values the application can use: %v", err)
	}

	for text, want := range map[string]string{
		"nonsense":     "--set agents.roles.build.agents=nonsense: must list agents among ",
		"codex":        "--set agents.roles.build.agents=codex: codex not among the agents of .foreman-kit/agents.yaml (claude)",
		"claude,codex": "--set agents.roles.build.agents=claude,codex: codex not among the agents of .foreman-kit/agents.yaml (claude)",
	} {
		settingstest.Give(t, build, text)
		if err := CheckRunOverrides(root); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("the build role given %q: %v, want %q", text, err, want)
		}
	}
	settingstest.Take(t, build)

	// A slot is free or taken by the workers the run reads, and --set may have raised them.
	settingstest.Give(t, settings.VisualQASlot, "3")
	if err := CheckRunOverrides(root); err != nil {
		t.Errorf("slot 3 with the 2 workers of the file: %v", err)
	}
	settingstest.Give(t, settings.Workers, "4")
	if err := CheckRunOverrides(root); err == nil || !strings.HasPrefix(err.Error(), "--set visual_qa.slot=3: 3 is taken") {
		t.Errorf("slot 3 with the 4 workers the run was given: %v", err)
	}

	// Where there is no file to ask, nothing is said here: the checks of the files say it.
	if err := CheckRunOverrides(t.TempDir()); err != nil {
		t.Errorf("a directory that holds no application: %v", err)
	}
}

// TestAValueGivenToARunInAWorktreeCutBeforeTheUpdate: the worktree of a change that was being built
// when `foreman-kit update` rewrote the files reads the settings of the main checkout, and a
// value given to a run there is asked of those files: the agents the application has now.
func TestAValueGivenToARunInAWorktreeCutBeforeTheUpdate(t *testing.T) {
	main := t.TempDir()
	settingstest.WriteFiles(t, main, map[string]string{"agents": `agents: [claude, reviewer]
commands:
  reviewer:
    run: "reviewer-cli {{args}} -p {{prompt}}"
    restricted: "reviewer-cli --safe {{args}} -p {{prompt}}"
    ping: "reviewer-cli -p {{prompt}}"
`})
	worktree := filepath.Join(main, ".worktrees", "worker-add-search")
	settingstest.WriteFormerFiles(t, worktree)
	build := settings.RoleAgents("build")

	for _, root := range []string{main, worktree} {
		settingstest.Give(t, build, "reviewer,claude")
		if err := CheckRunOverrides(root); err != nil {
			t.Errorf("agents the application lists, asked from %s: %v", root, err)
		}
		settingstest.Give(t, build, "codex")
		if err := CheckRunOverrides(root); err == nil || !strings.Contains(err.Error(), "codex not among the agents of .foreman-kit/agents.yaml (claude, reviewer)") {
			t.Errorf("an agent the application does not list, asked from %s: %v", root, err)
		}
	}
}
