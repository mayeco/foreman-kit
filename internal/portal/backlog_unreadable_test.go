package portal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/cycle"
)

// backlogUnreadableConflict is .foreman-kit/agent_backlog.json in the middle of a merge: two queued changes
// between conflict markers, which no JSON parser reads.
const backlogUnreadableConflict = `[
<<<<<<< HEAD
  {"change_name": "add-invoices", "priority": "high", "description": "Invoices"}
=======
  {"change_name": "add-refunds", "priority": "normal", "description": "Refunds"}
>>>>>>> feat/refunds
]
`

// TestBacklogActionsLeaveAnUnreadableBacklogAlone edits the backlog from the portal while the file
// cannot be parsed. Adding an item used to treat it as empty and write the new item over it: every
// queued change was lost. Neither action may write, and both must say why.
func TestBacklogActionsLeaveAnUnreadableBacklogAlone(t *testing.T) {
	srv, _, root := setupTestPortal(t)
	path := filepath.Join(root, cycle.BacklogFile)
	if err := os.WriteFile(path, []byte(backlogUnreadableConflict), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if item, err := srv.work.AddToBacklog(ctx, "add-exports", "normal", "CSV exports", nil); err == nil {
		t.Errorf("adding a change wrote %v into a backlog that could not be read", item)
	} else if !strings.Contains(err.Error(), "agent_backlog.json") {
		t.Errorf("adding a change does not say the backlog is unreadable: %v", err)
	}
	if err := srv.work.RemoveFromBacklog(ctx, "add-invoices"); err == nil {
		t.Error("taking a change out succeeded on a backlog that could not be read")
	} else if !strings.Contains(err.Error(), "agent_backlog.json") {
		t.Errorf("taking a change out does not say the backlog is unreadable: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != backlogUnreadableConflict {
		t.Fatalf("the unreadable backlog was rewritten, losing its queued changes:\n%s", after)
	}
}
