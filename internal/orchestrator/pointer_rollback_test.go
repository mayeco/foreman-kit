package orchestrator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/gitx"
)

// TestAChangeBuildsOnTheGitlinkFromBeforeARollback reproduces a real run: a person committed to the
// umbrella from a clone whose core submodule was still checked out at an old commit (git commit -a
// after a pull), moving core's gitlink back past a change the pipeline had integrated. The next core
// change was built on that old gitlink, without the integrated code. It is now built on the gitlink
// from before the rollback, its gates count only its own files, it is reported once, and its merge
// records core forward again.
func TestAChangeBuildsOnTheGitlinkFromBeforeARollback(t *testing.T) {
	sb := newMCFSandbox(t, "verify")
	ctx := context.Background()

	sb.runCoreDispatch(t, "add-a", map[string]string{"a.txt": "a\n"})
	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-a")
	mcfGit(t, sb.umbrella, "fetch", "-q", "origin", "master")
	integrated := mcfGit(t, sb.umbrella, "rev-parse", "origin/master:repos/core")

	// The teammate's clone: core checked out at its first commit, then an unrelated commit with -a.
	mate := filepath.Join(sb.tmp, "mate-umbrella")
	mcfGit(t, sb.tmp, "clone", "-q", "-b", "master", sb.umbrellaBare, mate)
	mcfGit(t, mate, "submodule", "update", "--init", "-q", "repos/core")
	first := mcfGit(t, filepath.Join(mate, "repos", "core"), "rev-list", "--max-parents=0", "HEAD")
	mcfGit(t, filepath.Join(mate, "repos", "core"), "checkout", "-q", first)
	mustWriteFile(t, filepath.Join(mate, "MEMORY.md"), "# Memory\n\nA note.\n")
	mcfGit(t, mate, "commit", "-qam", "docs: a note")
	mcfGit(t, mate, "push", "-q", "origin", "master")
	rollback := mcfGit(t, mate, "rev-parse", "HEAD")

	rb := gitx.FindPointerRollback(ctx, mate, "HEAD", "repos/core", 200)
	if rb == nil || rb.Commit != rollback || rb.From != integrated || rb.To != first || rb.Lost != 1 {
		t.Fatalf("rollback = %+v, want %s moving %s back to %s, 1 commit lost", rb, rollback, integrated, first)
	}

	// A change that only touches mobile is built with core at the gitlink from before the rollback,
	// but does not record core: its merge must not bring core's checks in.
	sb.runCoreDispatch(t, "add-m", map[string]string{"repos/mobile/m.txt": "m\n"})
	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-m")
	mcfGit(t, sb.umbrella, "fetch", "-q", "origin", "master")
	if got := mcfGit(t, sb.umbrella, "rev-parse", "origin/master:repos/core"); got != first {
		t.Fatalf("the mobile-only change recorded core at %s, want it left at %s", got, first)
	}
	var moved int
	_ = sb.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE event = 'merge.integrated' AND change_name = 'add-m' AND json_extract(payload_json, '$.repos') = 'repos/mobile'`).Scan(&moved)
	if moved != 1 {
		t.Fatal("add-m's merge should list repos/mobile only")
	}

	sb.runCoreDispatch(t, "add-b", map[string]string{"b.txt": "b\n"})
	workerCore := filepath.Join(sb.umbrella, ".worktrees", "worker-add-b", "repos", "core")
	if _, err := gitx.RunGit(ctx, workerCore, "merge-base", "--is-ancestor", integrated, "HEAD"); err != nil {
		t.Fatalf("add-b was not built on the gitlink from before the rollback (%s)", integrated)
	}
	var reported int
	_ = sb.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE event = 'repo.pointer_rolled_back'`).Scan(&reported)
	if reported != 2 {
		t.Fatalf("repo.pointer_rolled_back events = %d, want 2 (once per dispatch: add-m's and add-b's)", reported)
	}
	var last string
	for _, l := range sb.marker("core.ran") {
		if strings.HasPrefix(l, "slot=1 ") {
			last = l
		}
	}
	if last != "slot=1 files=b.txt" {
		t.Fatalf("add-b's worker gates saw %q, want only b.txt", last)
	}

	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-b")
	mcfGit(t, sb.umbrella, "fetch", "-q", "origin", "master")
	now := mcfGit(t, sb.umbrella, "rev-parse", "origin/master:repos/core")
	core := filepath.Join(sb.umbrella, "repos", "core")
	mcfGit(t, core, "fetch", "-q", "origin")
	if _, err := gitx.RunGit(ctx, core, "merge-base", "--is-ancestor", integrated, now); err != nil {
		t.Fatalf("the merge did not record core forward past %s (now %s)", integrated, now)
	}
	if gitx.FindPointerRollback(ctx, sb.umbrella, "origin/master", "repos/core", 200) != nil {
		t.Fatal("the rollback is still reported after the merge recorded core forward")
	}
}
