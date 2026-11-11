package cycle

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// TestVisualQAEndBringsTheMainCheckoutUpToDate: while a visual QA session holds the main checkout,
// nothing moves it, so what a cycle published meanwhile stayed out of it, and out of the queue the
// kit reads from it, until the next dispatcher pass, the next cycle or a sync. Ending the session
// brings it up to date and reads the queue again.
func TestVisualQAEndBringsTheMainCheckoutUpToDate(t *testing.T) {
	stopTestGit(t)
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", bare)
	root := filepath.Join(base, "root")
	gitIn(t, base, "clone", "-q", bare, root)
	commitFile(t, root, BacklogFile, `[{"change_name":"before-the-session"}]`+"\n", "backlog: one")
	gitIn(t, root, "branch", "-q", "--set-upstream-to=origin/main")
	st := openTestStore(t, root)
	ctx := context.Background()
	if err := st.SyncJSONToSQLite(ctx, root); err != nil {
		t.Fatal(err)
	}

	// A cycle publishes a change while the session runs.
	mate := filepath.Join(base, "mate")
	gitIn(t, base, "clone", "-q", bare, mate)
	commitFile(t, mate, BacklogFile, `[{"change_name":"before-the-session"},{"change_name":"queued-during-the-session"}]`+"\n", "chore(memory): scheduled cycle")

	var out bytes.Buffer
	if err := VisualQAEnd(ctx, root, st, VisualQAOptions{Stamp: "20261111T150000Z"}, &out); err != nil {
		t.Fatalf("VisualQAEnd: %v\n%s", err, out.String())
	}
	if at, want := headOf(t, root), headOf(t, mate); at != want {
		t.Errorf("after the session the main checkout is at %s, origin at %s", at, want)
	}
	if got := backlogStatusIn(t, st, "queued-during-the-session"); got != "queued" {
		t.Errorf("the change a cycle queued during the session is %q in the registry, want queued", got)
	}
	if !strings.Contains(out.String(), "session 20261111T150000Z closed") {
		t.Errorf("the session did not close:\n%s", out.String())
	}
}

func backlogStatusIn(t *testing.T, st *store.Store, change string) string {
	t.Helper()
	var status string
	if err := st.DB().QueryRowContext(context.Background(), `SELECT backlog_status FROM changes WHERE change_name = ?`, change).Scan(&status); err != nil {
		return "not on record (" + err.Error() + ")"
	}
	return status
}
