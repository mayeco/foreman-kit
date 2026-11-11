package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ffRotating is what the dispatcher lets FastForwardCheckout restore: the backlog, the history, and the
// files a rotation of the history creates.
var ffRotating = []string{".foreman-kit/agent_backlog.json", ".foreman-kit/agent_history.json", ".foreman-kit/agent_history.*.json"}

// The merger writes the backlog and history it published into the main checkout, which is fast-forwarded
// to them later. When the history is rotated its older entries move to .foreman-kit/agent_history.1.json, a
// file HEAD does not have yet: it is untracked in the main checkout, which counted as a local edit, so
// the checkout was never fast-forwarded again and the dispatcher kept reading a stale backlog from it.
func TestFastForwardCheckoutPullsAHistoryRotatedOnTheBase(t *testing.T) {
	main, mate := ffSetup(t)
	ctx := context.Background()

	ffWrite(t, filepath.Join(mate, ".foreman-kit", "agent_history.json"), `[{"change_name":"a"}]`+"\n")
	ffGit(t, mate, "add", "-A")
	ffGit(t, mate, "commit", "-qm", "chore(merge): integrate a")
	ffGit(t, mate, "push", "-q", "origin", "main")
	ffGit(t, main, "pull", "-q", "--ff-only")

	// The next integration rotates the history: on the base...
	rotated, current := `[{"change_name":"a"}]`+"\n", `[{"change_name":"b"}]`+"\n"
	ffWrite(t, filepath.Join(mate, ".foreman-kit", "agent_history.1.json"), rotated)
	ffWrite(t, filepath.Join(mate, ".foreman-kit", "agent_history.json"), current)
	ffGit(t, mate, "add", "-A")
	ffGit(t, mate, "commit", "-qm", "chore(merge): integrate b")
	ffGit(t, mate, "push", "-q", "origin", "main")
	// ...and, written by the merger, in the main checkout.
	ffWrite(t, filepath.Join(main, ".foreman-kit", "agent_history.1.json"), rotated)
	ffWrite(t, filepath.Join(main, ".foreman-kit", "agent_history.json"), current)

	ffGit(t, main, "fetch", "-q", "origin")
	res := FastForwardCheckout(ctx, main, "origin", "main", ffRotating)
	if res.Pulled != 1 || res.Skipped != "" {
		t.Fatalf("result = %+v, want 1 commit pulled", res)
	}
	if st := ffGit(t, main, "status", "--porcelain"); st != "" {
		t.Fatalf("the main checkout is not clean after the fast-forward:\n%s", st)
	}
	if got, err := os.ReadFile(filepath.Join(main, ".foreman-kit", "agent_history.1.json")); err != nil || string(got) != rotated {
		t.Fatalf(".foreman-kit/agent_history.1.json = %q (err %v), want the published rotation", got, err)
	}
}

// A rotated history file whose content no commit about to be pulled holds is local work: it stays, and
// so does the checkout.
func TestFastForwardCheckoutKeepsARotatedHistoryThatWasNotPublished(t *testing.T) {
	main, mate := ffSetup(t)
	ctx := context.Background()
	pushBacklog(t, mate, `[{"change_name":"x"}]`+"\n")

	local := `[{"change_name":"only-here"}]` + "\n"
	ffWrite(t, filepath.Join(main, ".foreman-kit", "agent_history.1.json"), local)

	ffGit(t, main, "fetch", "-q", "origin")
	res := FastForwardCheckout(ctx, main, "origin", "main", ffRotating)
	if res.Pulled != 0 || !strings.Contains(res.Skipped, "unpublished edits in .foreman-kit/agent_history.1.json") {
		t.Fatalf("result = %+v, want the checkout left as it is for its unpublished history file", res)
	}
	if got, err := os.ReadFile(filepath.Join(main, ".foreman-kit", "agent_history.1.json")); err != nil || string(got) != local {
		t.Fatalf("the unpublished history file was touched: %q (err %v)", got, err)
	}
}
