package config

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestAgentsOfAWorktreeCutBeforeTheUpdate: the worktree of a change that was being built when
// `foreman-kit update` rewrote .foreman-kit/agents.yaml still holds the file an older kit read, where a
// role was a list and the arguments of an agent sat under extra_args. A call made there reads the
// agents, the arguments and the commands of the main checkout: the ones the application has now,
// not the ones the kit ships.
func TestAgentsOfAWorktreeCutBeforeTheUpdate(t *testing.T) {
	main := t.TempDir()
	settingstest.WriteFiles(t, main, map[string]string{"agents": `agents: [claude, reviewer]
roles:
  plan:
    agents: [reviewer]
  build:
    agents: [claude, reviewer]
    args: {claude: "--model our-model"}
commands:
  reviewer:
    run: "reviewer-cli {{args}} -p {{prompt}}"
    restricted: "reviewer-cli --safe {{args}} -p {{prompt}}"
    ping: "reviewer-cli -p {{prompt}}"
`})
	worktree := filepath.Join(main, ".worktrees", "worker-add-search")
	settingstest.WriteFormerFiles(t, worktree)

	for _, root := range []string{main, worktree} {
		cfg := Load(root)
		if cfg == nil || strings.Join(cfg.Agents, " ") != "claude reviewer" || strings.Join(cfg.Roles["build"], " ") != "claude reviewer" {
			t.Fatalf("Load(%s) = %+v, want the agents of the main checkout", root, cfg)
		}
		if engines, err := Engine(root, "plan"); err != nil || engines != "reviewer" {
			t.Errorf("the agents of plan read from %s: %q (%v)", root, engines, err)
		}
		if args := ExtraArgsForRole(root, "claude", "build", io.Discard); strings.Join(args, " ") != "--model our-model" {
			t.Errorf("the arguments of claude in build read from %s: %v", root, args)
		}
		spec, written, known := CommandsFor(root, "reviewer")
		if !written || !known || spec.Run == nil {
			t.Errorf("the commands of an agent the kit does not ship, read from %s: written %v, known %v", root, written, known)
		}
	}
}
