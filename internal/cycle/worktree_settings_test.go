package cycle

import (
	"path/filepath"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestStrategiesOfAWorktreeCutBeforeTheUpdate: a worktree cut before `foreman-kit update` rewrote
// .foreman-kit/exploration.yaml holds the strategies as an older kit wrote them, without the session that
// runs each one. What reads them from that worktree reads the strategies of the main checkout.
func TestStrategiesOfAWorktreeCutBeforeTheUpdate(t *testing.T) {
	main := t.TempDir()
	settingstest.WriteFiles(t, main, map[string]string{"exploration": `strategies:
  - slug: only-ours
    name: Only ours
    kind: defects
    directive: Look at what only this application has.
    session: visual_qa
`})
	worktree := filepath.Join(main, ".worktrees", "worker-add-search")
	settingstest.WriteFormerFiles(t, worktree)

	for _, root := range []string{main, worktree} {
		list := LoadExplorationStrategies(root)
		if len(list) != 1 || list[0].Slug != "only-ours" || list[0].RunsIn() != settings.SessionVisualQA {
			t.Errorf("LoadExplorationStrategies(%s) = %+v, want the one strategy of the main checkout", root, list)
		}
	}
}
