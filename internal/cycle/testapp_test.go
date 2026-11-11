package cycle

import (
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// bareApp returns a new directory that holds nothing but the seven configuration files as the kit
// ships them, with main as its base branch: the least an application needs for a cycle, a visual
// session or a daemon to run. None of them starts on a file that lacks a key.
func bareApp(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	nameBaseBranch(t, root, "main")
	return root
}

// nameBaseBranch writes the configuration files of root as the kit ships them, with branch as the
// base branch.
func nameBaseBranch(t *testing.T, root, branch string) {
	t.Helper()
	settingstest.WriteFiles(t, root, map[string]string{"pipeline": "repository:\n  base_branch: " + branch + "\n"})
}
