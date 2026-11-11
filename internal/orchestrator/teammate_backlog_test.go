package orchestrator

import (
	"context"
	"path/filepath"
	"testing"
)

// TestDispatcherBuildsABacklogItemATeammatePushed: the dispatcher reads the backlog from the main
// checkout, which nothing moved after a teammate pushed an item from another clone: dispatch said
// "nothing pending" until someone ran foreman-kit sync. It now fast-forwards the main checkout
// first, so the item is built.
func TestDispatcherBuildsABacklogItemATeammatePushed(t *testing.T) {
	sb := newMCFSandbox(t, "")
	ctx := context.Background()

	mate := filepath.Join(sb.tmp, "mate-umbrella")
	mcfGit(t, sb.tmp, "clone", "-q", "-b", "master", sb.umbrellaBare, mate)
	mustWriteFile(t, filepath.Join(mate, ".foreman-kit", "agent_backlog.json"),
		`[{"change_name": "add-greeting", "description": "Greet in Spanish", "priority": "high"}]`)
	mcfGit(t, mate, "commit", "-qam", "chore(backlog): queue add-greeting")
	mcfGit(t, mate, "push", "-q", "origin", "master")

	// The main checkout still has the empty backlog; the dispatch must see the pushed one.
	builds := sb.runCoreDispatchFromCheckout(t, map[string]string{"greeting.txt": "hola\n"})
	if builds != 1 {
		t.Fatalf("BUILD calls = %d, want 1: the item the teammate pushed was not built", builds)
	}
	ready, _ := sb.st.ListReadyQueue(ctx)
	if len(ready) != 1 || ready[0].ChangeName != "add-greeting" {
		t.Fatalf("the merge queue = %+v", ready)
	}
	var ff int
	_ = sb.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE event = 'main.fast_forwarded'`).Scan(&ff)
	if ff != 1 {
		t.Fatalf("main.fast_forwarded events = %d, want 1", ff)
	}
	if head, mateHead := mcfGit(t, sb.umbrella, "rev-parse", "HEAD"), mcfGit(t, mate, "rev-parse", "HEAD"); head != mateHead {
		t.Fatalf("main checkout at %s, want the teammate's %s", head, mateHead)
	}
}
