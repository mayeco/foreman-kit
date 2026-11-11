package cycle

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// TestTheJudgmentIsGivenWhereItsFilesAreAndWhatToVerify reproduces a cycle of a new application: the
// judgment runs in .worktrees/_cycle, so it wrote its report and looked for state.json under the
// worktree's log/, where the script never looks, and on the first cycle it verified nothing, so the
// lessons of the changes integrated before it never reached the memory. The plan now gives the
// absolute paths of both files, and the commit to verify since: the one that installed the kit
// before any cycle ran, the one the last cycle ran on afterwards.
func TestTheJudgmentIsGivenWhereItsFilesAreAndWhatToVerify(t *testing.T) {
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "1")
	ctx := context.Background()
	app := filepath.Join(t.TempDir(), "app")
	if err := Init(ctx, app, KitOptions{Agents: "claude"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, app)
	install := commitAll(t, app, "install the kit")
	if err := os.WriteFile(filepath.Join(app, "feature.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, app, "chore(merge): integrate add-feature")
	st := openTestStore(t, app)

	since, what := verifySince(ctx, app, st, "20261111T193002Z")
	if since != install || !strings.Contains(what, "installed the kit") {
		t.Fatalf("the first cycle verifies since %q (%s), want the install commit %s", since, what, install)
	}

	stateDir := filepath.Join(t.TempDir(), "state", "cycle")
	ensureCycleState(stateDir, io.Discard)
	if raw, err := os.ReadFile(filepath.Join(stateDir, "state.json")); err != nil || !strings.Contains(string(raw), `"umbrella_sha": null`) {
		t.Fatalf("state.json was not created: %v %s", err, raw)
	}

	prompt, err := renderCyclePrompt(app, nil, nil, cyclePlan{QueueOpen: -1, Stamp: "20261111T193002Z", StateDir: stateDir, VerifySince: since, VerifySinceWhat: what})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- **Report**: write it as `" + filepath.Join(stateDir, "reports", "20261111T193002Z.md") + "`",
		"- **State**: `" + filepath.Join(stateDir, "state.json") + "`",
		"- **Verify**: run `foreman-kit completed-since " + install + "`",
		"the first cycle too",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}

	// Once a cycle ended, the next one verifies since the commit it ran on.
	if err := st.BeginCycle(ctx, store.Cycle{Stamp: "20261111T193002Z", UmbrellaSHA: install}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishCycle(ctx, "20261111T193002Z", "done", ""); err != nil {
		t.Fatal(err)
	}
	if since, what := verifySince(ctx, app, st, "20261111T200000Z"); since != install || !strings.Contains(what, "the cycle 20261111T193002Z ran on") {
		t.Errorf("the second cycle verifies since %q (%s)", since, what)
	}
}
