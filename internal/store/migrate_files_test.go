package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// migrationTableCounts counts the rows of every table of the state database.
func migrationTableCounts(t *testing.T, st *Store) map[string]int {
	t.Helper()
	rows, err := st.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	counts := make(map[string]int, len(tables))
	for _, name := range tables {
		var n int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM ` + name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts[name] = n
	}
	return counts
}

func migrationApp(t *testing.T) (string, *Store) {
	t.Helper()
	tmp := t.TempDir()
	st, err := OpenAt(filepath.Join(tmp, "migrated.db"), "mig-test", tmp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return tmp, st
}

func writeLegacyFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, st *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The reproduction of the defect: a blocked change with a history file, one event and one repair round.
func TestMigrateFromFilesSecondRunRecordsNothingForBlocksEventsAndRepairs(t *testing.T) {
	ctx := context.Background()
	root, st := migrationApp(t)
	writeLegacyFile(t, root, "log/blocked/feat-blocked.json", `{"change":"feat-blocked","reason":"gate_failure","details":"rspec failed"}`)
	writeLegacyFile(t, root, "log/blocked/history/feat-blocked-1.json", `{"change":"feat-blocked","reason":"gate_failure"}`)
	writeLegacyFile(t, root, "log/events.jsonl", `{"ts":"2026-11-07T10:00:00Z","event":"agent.exit","pid":123,"change":"feat-alpha","engine":"hermes","phase":"apply"}`+"\n")
	writeLegacyFile(t, root, "openspec/changes/feat-alpha/repairs.md",
		"## repair-1, 2026-11-07T09:50:00Z, hermes\n\nAsked to fix (gate_failure):\n\n```\nfail\n```\n\nResult: passed.\n")

	first, err := st.MigrateFromFiles(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.EventsImported != 1 || first.BlockHistory != 1 || first.BlockedChanges != 1 || first.RepairRounds != 1 {
		t.Fatalf("first run: %+v", first)
	}
	before := migrationTableCounts(t, st)
	openBefore := countRows(t, st, `SELECT COUNT(*) FROM change_blocks WHERE resolved_at IS NULL`)

	second, err := st.MigrateFromFiles(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (MigrationReport{WorkspaceDir: first.WorkspaceDir, DBPath: first.DBPath}); *second != want {
		t.Fatalf("second run reported %+v, want zeros", *second)
	}
	after := migrationTableCounts(t, st)
	for _, table := range []string{"events", "change_blocks", "change_runs", "executions", "repair_rounds"} {
		if before[table] != after[table] {
			t.Errorf("%s: %d rows after the first run, %d after the second", table, before[table], after[table])
		}
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM change_blocks WHERE resolved_at IS NULL`); n != openBefore || n != 1 {
		t.Errorf("open blocks: %d before the second run, %d after, want 1", openBefore, n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM change_blocks WHERE resolution = ?`, BlockSuperseded); n != 0 {
		t.Errorf("%d blocks superseded: the second run replaced the open block it had imported", n)
	}
}

// Two lines of the file that are the same event are two events, and each is imported once.
func TestMigrateFromFilesKeepsRepeatedEventLinesAndImportsThemOnce(t *testing.T) {
	ctx := context.Background()
	root, st := migrationApp(t)
	line := `{"ts":"2026-11-07T10:00:00Z","event":"agent.exit","pid":123,"change":"feat-alpha","engine":"hermes","phase":"apply"}` + "\n"
	writeLegacyFile(t, root, "log/events.jsonl", line+line+
		`{"event":"merge.integrated","change":"feat-alpha","note":"no ts, no pid"}`+"\n")

	first, err := st.MigrateFromFiles(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.EventsImported != 3 {
		t.Fatalf("first run imported %d events, want 3", first.EventsImported)
	}
	second, err := st.MigrateFromFiles(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.EventsImported != 0 {
		t.Fatalf("second run imported %d events, want 0", second.EventsImported)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM events`); n != 3 {
		t.Fatalf("%d events on record, want 3", n)
	}
	// A line added to the file later is imported on the next run, and only it.
	writeLegacyFile(t, root, "log/events.jsonl", line+line+
		`{"event":"merge.integrated","change":"feat-alpha","note":"no ts, no pid"}`+"\n"+line)
	third, err := st.MigrateFromFiles(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.EventsImported != 1 {
		t.Fatalf("third run imported %d events, want the 1 new line", third.EventsImported)
	}
}

// A history file without a retry number is counted among the blocks of its own change.
func TestMigrateFromFilesNumbersRetriesPerChange(t *testing.T) {
	ctx := context.Background()
	root, st := migrationApp(t)
	for _, chg := range []string{"alpha", "bravo", "charlie"} {
		writeLegacyFile(t, root, "log/blocked/history/"+chg+"-1.json", fmt.Sprintf(`{"change":%q,"reason":"gate_failure"}`, chg))
		writeLegacyFile(t, root, "log/blocked/"+chg+".json", fmt.Sprintf(`{"change":%q,"reason":"gate_failure","details":"still failing"}`, chg))
	}
	// A change retried twice: the files are named so that file order is not the order it happened in.
	writeLegacyFile(t, root, "log/blocked/history/delta-10.json", `{"change":"delta","reason":"gate_failure","blocked_at":"2026-11-07T12:00:00Z"}`)
	writeLegacyFile(t, root, "log/blocked/history/delta-2.json", `{"change":"delta","reason":"rebase_conflict","blocked_at":"2026-11-07T10:00:00Z"}`)
	writeLegacyFile(t, root, "log/blocked/delta.json", `{"change":"delta","reason":"gate_failure","details":"third time"}`)

	rep, err := st.MigrateFromFiles(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.BlockHistory != 5 || rep.BlockedChanges != 4 {
		t.Fatalf("imported %d history blocks and %d open blocks, want 5 and 4", rep.BlockHistory, rep.BlockedChanges)
	}
	want := map[string]int{"alpha": 1, "bravo": 1, "charlie": 1, "delta": 2}
	for chg, retries := range want {
		var retryNum int
		if err := st.db.QueryRow(`SELECT retry_num FROM change_blocks WHERE change_name = ? AND resolved_at IS NULL`, chg).Scan(&retryNum); err != nil {
			t.Fatal(err)
		}
		if retryNum != retries {
			t.Errorf("%s: its open block has retry_num %d, want %d", chg, retryNum, retries)
		}
		if q, err := st.GetQuarantine(ctx, chg); err != nil || q == nil || q.Retries != retries {
			t.Errorf("%s: quarantine %+v (err %v), want %d retries", chg, q, err, retries)
		}
	}
	// The history of delta is in the order it happened: rebase_conflict (10:00) then gate_failure (12:00).
	rows, err := st.db.Query(`SELECT reason FROM change_blocks WHERE change_name = 'delta' AND resolution = ? ORDER BY id`, BlockAutoRetry)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var reasons []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		reasons = append(reasons, r)
	}
	if len(reasons) != 2 || reasons[0] != "rebase_conflict" || reasons[1] != "gate_failure" {
		t.Errorf("delta history imported as %v, want [rebase_conflict gate_failure]", reasons)
	}
}

// What the kit recorded after the first import is not overwritten by the files of the second.
func TestMigrateFromFilesDoesNotRewriteWhatTheKitRecordedSince(t *testing.T) {
	ctx := context.Background()
	root, st := migrationApp(t)
	writeLegacyFile(t, root, "log/blocked/feat-blocked.json", `{"change":"feat-blocked","reason":"risk_review","details":"needs a look"}`)
	writeLegacyFile(t, root, "log/cycle/cycle-20261107T100000Z.log", "cycle")
	writeLegacyFile(t, root, "log/cycle/state.json", `{"visual_qa_last_sha":"old-sha","last_cycle":{"stamp":"20261107T100000Z","outcome":"failed","finished_at":"2026-11-07T10:20:00Z"}}`)
	writeLegacyFile(t, root, "log/cycle/visual_queue.json", `{"items":[{"id":"vqa-a","change":"feat-alpha","detail":"Check alpha UI"},{"change":"feat-beta","detail":"Check beta UI"}]}`)
	writeLegacyFile(t, root, "log/approved/feat-blocked", "")
	writeLegacyFile(t, root, "log/ready_to_merge/feat-ready.json", `{"change":"feat-ready","branch":"feat/spec-feat-ready","head":"deadbeef"}`)

	if _, err := st.MigrateFromFiles(ctx, root, nil); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM cycles WHERE stamp = '20261107T100000Z' AND outcome = 'failed'`); got != 1 {
		t.Fatalf("the first run did not take the outcome of the last cycle from state.json")
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM visual_qa_items`); got != 2 {
		t.Fatalf("%d visual QA items after the first run, want 2", got)
	}

	// The kit goes on: the cycle is corrected, visual QA moves forward, the block is lifted and the
	// risk approval is revoked, the manifest is integrated.
	if err := st.FinishCycle(ctx, "20261107T100000Z", "done", "2026-11-07T11:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVisualQALastSHA(ctx, "new-sha"); err != nil {
		t.Fatal(err)
	}
	if err := st.ResolveQuarantine(ctx, "feat-blocked", QuarantineResolution{Forget: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE ready_manifests SET closed_at = enqueued_at + 1, outcome = 'integrated' WHERE change_name = 'feat-ready'`); err != nil {
		t.Fatal(err)
	}
	before := migrationTableCounts(t, st)

	rep, err := st.MigrateFromFiles(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (MigrationReport{WorkspaceDir: rep.WorkspaceDir, DBPath: rep.DBPath}); *rep != want {
		t.Fatalf("second run reported %+v, want zeros", *rep)
	}
	if after := migrationTableCounts(t, st); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("rows changed:\nbefore %v\nafter  %v", before, after)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM cycles WHERE stamp = '20261107T100000Z' AND outcome = 'done'`); got != 1 {
		t.Errorf("the second run overwrote the outcome of the cycle")
	}
	var sha string
	if err := st.db.QueryRow(`SELECT visual_qa_last_sha FROM workspace WHERE id = 1`).Scan(&sha); err != nil || sha != "new-sha" {
		t.Errorf("visual_qa_last_sha is %q (err %v), want new-sha: the second run went back to the file's", sha, err)
	}
	if q, err := st.GetQuarantine(ctx, "feat-blocked"); err != nil || q != nil && q.Status == "blocked" {
		t.Errorf("the second run blocked the change again: %+v (err %v)", q, err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM ready_manifests WHERE change_name = 'feat-ready' AND outcome = 'integrated'`); got != 1 {
		t.Errorf("the second run queued the integrated manifest again")
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM risk_approvals WHERE change_name = 'feat-blocked' AND revoked_at IS NULL`); got != 0 {
		t.Errorf("the second run approved the risk of the change again")
	}
}

// What a run of the files puts on record is on record whatever order the files are read in: a change
// the registry already holds is not counted again.
func TestMigrateFromFilesCountsOnlyWhatTheRegistryDidNotHold(t *testing.T) {
	ctx := context.Background()
	root, st := migrationApp(t)
	writeLegacyFile(t, root, ".foreman-kit/agent_backlog.json", `[{"change_name":"feat-a","priority":"high"}]`)
	first, err := st.MigrateFromFiles(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.BacklogItems != 1 {
		t.Fatalf("first run: %+v", first)
	}
	writeLegacyFile(t, root, ".foreman-kit/agent_backlog.json", `[{"change_name":"feat-a"},{"change_name":"feat-b"}]`)
	writeLegacyFile(t, root, ".foreman-kit/agent_proposals.json", `[{"change_name":"feat-p","status":"proposed"}]`)
	second, err := st.MigrateFromFiles(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.BacklogItems != 1 || second.Proposals != 1 || second.HistoryItems != 0 {
		t.Fatalf("second run: %+v, want only feat-b and feat-p counted", second)
	}
}
