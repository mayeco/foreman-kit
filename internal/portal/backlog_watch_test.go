package portal

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
)

// TestThePortalMirrorsABacklogNobodyAnnounced: a portal that serves knows the backlog as its files
// hold it when it starts, and learns of an edit nobody announced (the editor of a person, a git
// pull) without a restart and without a page that asks for it. It stops following the files when it
// stops serving.
func TestThePortalMirrorsABacklogNobodyAnnounced(t *testing.T) {
	srv, _, root := setupTestPortal(t)
	srv.backlogEvery = 10 * time.Millisecond
	backlog := func(names ...string) {
		t.Helper()
		var items []map[string]any
		for _, name := range names {
			items = append(items, map[string]any{"change_name": name, "description": "A part"})
		}
		if err := cycle.WriteJSONList(filepath.Join(root, cycle.BacklogFile), items); err != nil {
			t.Fatal(err)
		}
	}
	listed := func() map[string]bool {
		t.Helper()
		code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/changes", nil)
		if code != http.StatusOK {
			t.Fatalf("GET /api/changes = %d: %s", code, raw)
		}
		names := map[string]bool{}
		items, _ := data["items"].([]any)
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				names[m["change_name"].(string)] = true
			}
		}
		return names
	}
	// waitFor asks the list until it holds name, for as long as a slow machine may need.
	waitFor := func(name string) bool {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if listed()[name] {
				return true
			}
		}
		return false
	}

	backlog("add-first")
	if listed()["add-first"] {
		t.Fatal("a page mirrored the backlog by being read")
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	srv.mirrorBacklog(ctx)
	if !listed()["add-first"] {
		t.Fatal("the portal does not know the backlog its files held when it started")
	}

	backlog("add-first", "add-second")
	if !waitFor("add-second") {
		t.Fatal("the portal did not learn of a change added to the backlog file while it served")
	}

	// Stopped, it no longer follows the files.
	stop()
	time.Sleep(50 * time.Millisecond)
	backlog("add-first", "add-second", "add-third")
	time.Sleep(150 * time.Millisecond)
	if listed()["add-third"] {
		t.Error("the portal still follows the files of the backlog after it stopped")
	}
}
