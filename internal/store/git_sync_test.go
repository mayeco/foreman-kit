package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMirroredFilesSignatureMovesWhenAMirroredFileChanges: the signature stays as it was while the
// files SyncJSONToSQLite mirrors are left alone, and moves when one of them is written, added or
// removed: the backlog, the proposals, the history and a rotated file of the history.
func TestMirroredFilesSignatureMovesWhenAMirroredFileChanges(t *testing.T) {
	st := openTestStore(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".foreman-kit")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cfg, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	last := st.MirroredFilesSignature(dir)
	moved := func(what string) {
		t.Helper()
		sig := st.MirroredFilesSignature(dir)
		if sig == last {
			t.Errorf("the signature did not move after %s", what)
		}
		if again := st.MirroredFilesSignature(dir); again != sig {
			t.Errorf("the signature moved with nothing written, after %s", what)
		}
		last = sig
	}

	write("agent_backlog.json", `[{"change_name":"add-a"}]`)
	moved("the backlog was written")
	write("agent_proposals.json", `[]`)
	moved("the proposals were written")
	write("agent_history.json", `[]`)
	moved("the history was written")
	write("agent_history.2026-10.json", `[]`)
	moved("a rotated file of the history was added")

	// Written again with the same size: the time of modification tells.
	write("agent_backlog.json", `[{"change_name":"add-b"}]`)
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(cfg, "agent_backlog.json"), later, later); err != nil {
		t.Fatal(err)
	}
	moved("the backlog was written again with the same size")

	if err := os.Remove(filepath.Join(cfg, "agent_proposals.json")); err != nil {
		t.Fatal(err)
	}
	moved("the proposals were removed")

	// A file the mirror does not read leaves the signature alone.
	write("agents.yaml", "engines: []\n")
	if sig := st.MirroredFilesSignature(dir); sig != last {
		t.Errorf("the signature moved when a file the mirror does not read was written")
	}
}

// TestAChangeThatLeftTheBacklogIsNotQueuedAnyMore: the registry follows the backlog file. An item that
// leaves it without being integrated or abandoned stops being queued, and is queued again, from that
// moment, when the file lists it again. A file that cannot be read whole takes nothing out of the
// queue.
func TestAChangeThatLeftTheBacklogIsNotQueuedAnyMore(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	backlog := filepath.Join(dir, ".foreman-kit", "agent_backlog.json")
	sync := func() {
		t.Helper()
		if err := st.SyncJSONToSQLite(ctx, dir); err != nil {
			t.Fatal(err)
		}
	}
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(backlog, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		sync()
	}
	want := func(when string, states map[string]string) {
		t.Helper()
		for change, state := range states {
			if got, err := st.ChangeState(ctx, change); err != nil || got != state {
				t.Errorf("%s: %s is %q (err %v), want %q", when, change, got, err, state)
			}
		}
	}
	queuedAt := func(change string) (ms sql.NullInt64) {
		t.Helper()
		if err := st.DB().QueryRowContext(ctx, `SELECT queued_at FROM changes WHERE change_name = ?`, change).Scan(&ms); err != nil {
			t.Fatal(err)
		}
		return ms
	}

	write(`[{"change_name":"stays"},{"change_name":"leaves","queued_at":"2026-11-01T11:00:00Z"},{"change_name":"lands"},{"change_name":"goes"},{"change_name":"builds"}]`)
	if err := st.MarkChangeIntegrated(ctx, "lands", "abc123"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkChangeDiscarded(ctx, "goes"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginChangeRun(ctx, "builds", 1, "/wt/builds", "feat/spec-builds"); err != nil {
		t.Fatal(err)
	}
	all := map[string]string{"stays": ChangeQueued, "leaves": ChangeQueued, "lands": ChangeIntegrated, "goes": ChangeDiscarded, "builds": ChangeBuilding}
	want("with every item in the backlog", all)
	first := queuedAt("leaves")
	if !first.Valid {
		t.Fatal("a queued change has no time of being queued")
	}

	// A file that says nothing whole takes nothing out: missing, empty, and cut in the middle of a
	// write, where only the lines that are whole are read.
	if err := os.Remove(backlog); err != nil {
		t.Fatal(err)
	}
	sync()
	want("with no backlog file", all)
	write(``)
	want("with an empty backlog file", all)
	write("[\n{\"change_name\":\"stays\"},\n{\"change_na")
	want("with a backlog file cut in the middle of a write", all)

	// The file without them: what was integrated or discarded stays so, and a change in progress
	// goes on, as one the backlog does not list.
	write(`[{"change_name":"stays"}]`)
	want("after four items left the backlog", map[string]string{
		"stays": ChangeQueued, "leaves": ChangeUnknown, "lands": ChangeIntegrated, "goes": ChangeDiscarded, "builds": ChangeBuilding,
	})
	var status string
	if err := st.DB().QueryRowContext(ctx, `SELECT backlog_status FROM changes WHERE change_name = 'builds'`).Scan(&status); err != nil || status != "unknown" {
		t.Errorf("the change in progress that left the backlog is %q there (err %v), want unknown", status, err)
	}
	if queuedAt("leaves").Valid {
		t.Error("a change that left the backlog still has the time it was queued")
	}

	// Listed again, it is queued from now, not from the first time.
	write(`[{"change_name":"stays"},{"change_name":"leaves"}]`)
	want("after the item came back", map[string]string{"stays": ChangeQueued, "leaves": ChangeQueued})
	if again := queuedAt("leaves"); !again.Valid || again.Int64 <= first.Int64 {
		t.Errorf("the item that came back is queued since %v, want later than the first time (%v)", again, first)
	}

	// An empty list is a backlog with nothing in it.
	write(`[]`)
	want("with an empty list", map[string]string{"stays": ChangeUnknown, "leaves": ChangeUnknown})
}
