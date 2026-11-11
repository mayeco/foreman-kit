package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// listedAt returns the hour the registry says the main checkout first listed the change, 0 when it
// says it never did.
func listedAt(t *testing.T, st *Store, change string) int64 {
	t.Helper()
	var at sql.NullInt64
	if err := st.DB().QueryRowContext(context.Background(), `SELECT listed_at FROM changes WHERE change_name = ?`, change).Scan(&at); err != nil {
		t.Fatalf("%s: %v", change, err)
	}
	return at.Int64
}

// A change a cycle queued that the main checkout has not listed, and one somebody took out of its
// backlog, are both unknown and both name the cycle that queued them. The registry tells them apart:
// only the second has the hour the backlog first listed it, and that hour stays as it was.
func TestTheRegistrySaysWhetherTheMainCheckoutEverListedAChange(t *testing.T) {
	st := openMovementStore(t)
	ctx := context.Background()
	main := t.TempDir()
	const stamp = "20261111T120000Z"
	if err := st.BeginCycle(ctx, Cycle{Stamp: stamp}); err != nil {
		t.Fatal(err)
	}
	mirror := func(backlog string) {
		t.Helper()
		writeMirrored(t, main, "agent_backlog.json", backlog)
		if err := st.SyncJSONToSQLite(ctx, main); err != nil {
			t.Fatal(err)
		}
	}
	is := func(when, change, status string, everListed bool) {
		t.Helper()
		if got, at := backlogStatusOf(t, st, change), listedAt(t, st, change); got != status || (at != 0) != everListed {
			t.Errorf("%s, %s is %s and the registry says the main checkout listed it at %d; want %s, ever listed: %v", when, change, got, at, status, everListed)
		}
	}

	// A cycle queued two changes, in its own worktree: the main checkout lists neither.
	work := CycleWork{Queued: []map[string]any{{"change_name": "never-listed"}, {"change_name": "listed-then-removed"}}}
	if err := st.RecordCycleWork(ctx, stamp, work); err != nil {
		t.Fatal(err)
	}
	is("after the cycle", "never-listed", "unknown", false)
	is("after the cycle", "listed-then-removed", "unknown", false)

	// The main checkout comes to list one of them, beside a change a person queued.
	mirror(`[{"change_name": "queued-by-hand"}, {"change_name": "listed-then-removed"}]`)
	is("once the main checkout lists it", "listed-then-removed", "queued", true)
	is("once the main checkout lists it", "queued-by-hand", "queued", true)
	is("while the main checkout does not list it", "never-listed", "unknown", false)
	first := listedAt(t, st, "listed-then-removed")

	// Somebody takes it out of the backlog. It is unknown again, as the change the main checkout
	// never listed is, and still under its cycle: the hour it was first listed is what tells them apart.
	mirror(`[{"change_name": "queued-by-hand"}]`)
	is("taken out of the backlog", "listed-then-removed", "unknown", true)
	is("still not listed", "never-listed", "unknown", false)
	for _, change := range []string{"never-listed", "listed-then-removed"} {
		if got := cycleOf(t, st, change); got != stamp {
			t.Errorf("%s names the cycle %q, want %q", change, got, stamp)
		}
	}

	// Listed again, it keeps the hour of the first time.
	mirror(`[{"change_name": "queued-by-hand"}, {"change_name": "listed-then-removed"}]`)
	if at := listedAt(t, st, "listed-then-removed"); at != first {
		t.Errorf("listed again, the change says it was first listed at %d, want %d still", at, first)
	}
}

// The migration that adds the hour fills it for the changes a registry already holds: one that is
// queued, integrated or discarded was in the backlog, and so was one that has a run. An unknown change
// with no run was never known to be listed.
func TestListedAtIsFilledForTheChangesAlreadyOnRecord(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	before := -1
	for i, m := range migrations {
		if strings.HasSuffix(m.name, "_change_listed.sql") {
			before = i
		}
	}
	if before < 1 {
		t.Fatalf("the migration that adds listed_at is not among %d migrations", len(migrations))
	}

	old, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:before] {
		if _, err := old.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
	}
	for _, stmt := range []string{
		fmt.Sprintf("PRAGMA user_version = %d", before),
		`INSERT INTO cycles (stamp, outcome, started_at, finished_at) VALUES ('20261110T041820Z', 'done', 900, 950)`,
		`INSERT INTO changes (change_name, backlog_status, first_seen_at, queued_at, updated_at) VALUES ('queued-now', 'queued', 1000, 1500, 1500)`,
		`INSERT INTO changes (change_name, backlog_status, first_seen_at, updated_at) VALUES ('integrated-once', 'integrated', 2000, 2500)`,
		`INSERT INTO changes (change_name, backlog_status, first_seen_at, queued_at, updated_at) VALUES ('discarded-once', 'discarded', 3000, 3200, 3500)`,
		`INSERT INTO changes (change_name, backlog_status, first_seen_at, updated_at) VALUES ('built-then-removed', 'unknown', 4000, 4500)`,
		`INSERT INTO change_runs (change_name, pid, host, slot, worktree, branch, started_at) VALUES ('built-then-removed', 1, 'h', 1, '/wt', 'b', 4100)`,
		`INSERT INTO changes (change_name, backlog_status, first_seen_at, cycle_stamp, updated_at) VALUES ('queued-by-a-cycle-never-listed', 'unknown', 5000, '20261110T041820Z', 5000)`,
		`INSERT INTO changes (change_name, backlog_status, first_seen_at, updated_at) VALUES ('met-as-a-dependency', 'unknown', 6000, 6000)`,
	} {
		if _, err := old.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := OpenAt(dbPath, "change-listed-test", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for change, want := range map[string]int64{
		"queued-now":                     1500,
		"integrated-once":                2000,
		"discarded-once":                 3200,
		"built-then-removed":             4000,
		"queued-by-a-cycle-never-listed": 0,
		"met-as-a-dependency":            0,
	} {
		if got := listedAt(t, st, change); got != want {
			t.Errorf("%s: listed_at is %d after the migration, want %d", change, got, want)
		}
	}
}
