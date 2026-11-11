package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestEvaluatorRenameKeepsWhatWasAsked: a database written while the evaluator was named after its
// provider keeps every evaluation, cached answer and event under the new names, and the manifests
// that point at an evaluation still do.
func TestEvaluatorRenameKeepsWhatWasAsked(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) < 3 {
		t.Fatalf("got %d migrations, want the rename as the third", len(migrations))
	}

	// The database as schema version 2 left it.
	old, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:2] {
		if _, err := old.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
	}
	for _, stmt := range []string{
		`PRAGMA user_version = 2`,
		`INSERT INTO jev_evaluations (id, gate, mode, status, model, created_at) VALUES (7, 'plan_review', 'enforce', 'failed', 'jev-1.14.0', 1000)`,
		`INSERT INTO jev_cache (cache_key, gate, model, state_json, response_json, created_at) VALUES ('k1', 'plan_review', 'jev-1.14.0', '{}', '{"answers":{}}', 1000)`,
		`INSERT INTO events (ts, event, pid) VALUES (1000, 'jev.answer', 1)`,
		`INSERT INTO events (ts, event, pid) VALUES (1001, 'jev.route', 1)`,
		`INSERT INTO events (ts, event, pid) VALUES (1002, 'jev.unavailable', 1)`,
		`INSERT INTO events (ts, event, pid) VALUES (1003, 'change.retry', 1)`,
	} {
		if _, err := old.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := OpenAt(dbPath, "rename-test", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var version int
	if err := st.DB().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion() {
		t.Fatalf("user_version = %d, want %d", version, SchemaVersion())
	}

	var leftover int
	if err := st.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master WHERE name LIKE '%jev%' OR sql LIKE '%jev\_%' ESCAPE '\'
	`).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Fatalf("%d tables or indexes still carry the provider's name", leftover)
	}

	evs, err := st.ListEvaluations(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].ID != 7 || evs[0].Gate != "plan_review" || evs[0].Status != "failed" {
		t.Fatalf("the evaluation did not survive the rename: %+v", evs)
	}
	if resp, ok, err := st.GetEvaluationCache(ctx, "k1"); err != nil || !ok || resp != `{"answers":{}}` {
		t.Fatalf("the cached answer did not survive the rename: %q %v %v", resp, ok, err)
	}

	counts := map[string]int{}
	rows, err := st.DB().QueryContext(ctx, `SELECT event, COUNT(*) FROM events GROUP BY event`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			t.Fatal(err)
		}
		counts[name] = n
	}
	_ = rows.Close()
	want := map[string]int{"evaluator.answer": 1, "evaluator.route": 1, "evaluator.unavailable": 1, "change.retry": 1}
	for name, n := range want {
		if counts[name] != n {
			t.Fatalf("events after the rename: %v, want %v", counts, want)
		}
	}
	if len(counts) != len(want) {
		t.Fatalf("events after the rename: %v, want %v", counts, want)
	}

	// A manifest names the evaluation that assessed its risk: that relation follows the table.
	var target string
	if err := st.DB().QueryRowContext(ctx, `
		SELECT "table" FROM pragma_foreign_key_list('ready_manifests') WHERE "from" = 'risk_evaluation_id'
	`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != "evaluations" {
		t.Fatalf("ready_manifests.risk_evaluation_id points at %q, want evaluations", target)
	}
}
