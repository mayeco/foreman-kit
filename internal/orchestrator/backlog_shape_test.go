package orchestrator

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// A .foreman-kit/agent_backlog.json that parses as JSON but is not a backlog (an object without the legacy
// "changes" list: items wrapped under another key, or one item without the list around it; a scalar)
// is treated like a file that does not parse: reported once, and the last backlog that was read keeps
// being dispatched. Read as an empty backlog, the pipeline idled without a word.
func TestMisShapedBacklogIsReportedAndKeepsTheLastValidOne(t *testing.T) {
	ctx := context.Background()
	root, _, _, st := setupTestWorkspaceWithSubmodule(t)
	writeBacklogJSON(t, root, map[string]any{"change_name": "kept-item", "description": "Stays dispatchable", "priority": "high"})
	disp, err := NewDispatcher(DispatcherOptions{Root: root, Store: st, MaxWorkers: 1, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer disp.Close()
	if items, _, _ := disp.loadBacklog(ctx, false); len(items) != 1 || items[0].ChangeName != "kept-item" {
		t.Fatalf("want the valid backlog read first, got %+v", items)
	}

	backlogFile := filepath.Join(root, ".foreman-kit", "agent_backlog.json")
	for _, content := range []string{
		`{"backlog": [{"change_name": "other-item", "description": "Wrapped under another key"}]}`,
		`{"change_name": "other-item", "description": "One item without the list around it"}`,
		`{"changes": {"change_name": "other-item"}}`,
		`"other-item"`,
		`null`,
	} {
		mustWriteFile(t, backlogFile, content)
		items, _, _ := disp.loadBacklog(ctx, false)
		if len(items) != 1 || items[0].ChangeName != "kept-item" {
			t.Errorf("backlog file %s: want the last valid backlog kept, got %+v", content, items)
		}
	}
	evs, err := st.QueryEvents(ctx, store.EventFilter{Event: "backlog.unreadable"})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Errorf("want the mis-shaped backlog reported once as backlog.unreadable, got %d event(s)", len(evs))
	}

	// The legacy {"changes": [...]} wrapper and an empty list are backlogs.
	mustWriteFile(t, backlogFile, `{"changes": [{"change_name": "wrapped-item", "description": "Legacy wrapper"}]}`)
	if items, _, _ := disp.loadBacklog(ctx, false); len(items) != 1 || items[0].ChangeName != "wrapped-item" {
		t.Errorf("want the legacy wrapper read as a backlog, got %+v", items)
	}
	mustWriteFile(t, backlogFile, `[]`)
	if items, _, _ := disp.loadBacklog(ctx, false); len(items) != 0 {
		t.Errorf("want an empty list read as an empty backlog, got %+v", items)
	}
}
