package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A cycle says what started it, keeps saying it when the same process records the cycle again, and
// says nothing rather than something the kit does not know.
func TestCycleSaysWhatStartedIt(t *testing.T) {
	st := openMovementStore(t)
	ctx := context.Background()

	for i, trigger := range []string{CycleTriggerStartup, CycleTriggerInterval, CycleTriggerOutOfWork, CycleTriggerManual} {
		stamp := "20261110T0" + string(rune('1'+i)) + "0000Z"
		if err := st.BeginCycle(ctx, Cycle{Stamp: stamp, Trigger: trigger}); err != nil {
			t.Fatalf("%s: %v", trigger, err)
		}
		c, err := st.GetCycle(ctx, stamp)
		if err != nil || c == nil || c.Trigger != trigger {
			t.Fatalf("cycle started by %q: got %+v (%v)", trigger, c, err)
		}
	}

	const stamp = "20261110T051820Z"
	if err := st.BeginCycle(ctx, Cycle{Stamp: stamp, Trigger: CycleTriggerOutOfWork}); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginCycle(ctx, Cycle{Stamp: stamp, LogPath: "/state/cycle.log"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.GetCycle(ctx, stamp); c == nil || c.Trigger != CycleTriggerOutOfWork || c.LogPath != "/state/cycle.log" {
		t.Fatalf("recording the cycle again without a trigger keeps the one it has, got %+v", c)
	}

	if err := st.BeginCycle(ctx, Cycle{Stamp: "20261110T061820Z", Trigger: "timer"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.GetCycle(ctx, "20261110T061820Z"); c == nil || c.Trigger != "" {
		t.Fatalf("a trigger the kit does not know is not recorded, got %+v", c)
	}
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO cycles (stamp, outcome, started_at, trigger) VALUES ('raw', 'running', 1, 'timer')`); err == nil {
		t.Fatal("the schema refuses a trigger that is not one of the four")
	}

	listed, err := st.ListCycles(ctx, 1)
	if err != nil || len(listed) != 1 || listed[0].Stamp != "20261110T061820Z" {
		t.Fatalf("the latest cycle: got %+v (%v)", listed, err)
	}
}

// A database written before cycles said what started them keeps its cycles, with no trigger.
func TestCycleTriggerMigrationKeepsEarlierCycles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	before := -1
	for i, m := range migrations {
		if strings.HasSuffix(m.name, "_cycle_trigger.sql") {
			before = i
		}
	}
	if before < 1 {
		t.Fatalf("the cycle trigger migration is not among %d migrations", len(migrations))
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
		"PRAGMA user_version = " + string(rune('0'+before)),
		`INSERT INTO cycles (stamp, outcome, started_at, finished_at) VALUES ('20261110T041820Z', 'done', 1000, 2000)`,
	} {
		if _, err := old.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := OpenAt(dbPath, "cycle-trigger-test", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	c, err := st.GetCycle(ctx, "20261110T041820Z")
	if err != nil || c == nil || c.Trigger != "" || c.Outcome != "done" {
		t.Fatalf("the earlier cycle: got %+v (%v)", c, err)
	}
	if err := st.BeginCycle(ctx, Cycle{Stamp: "20261110T051820Z", Trigger: CycleTriggerInterval}); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.GetCycle(ctx, "20261110T051820Z"); c == nil || c.Trigger != CycleTriggerInterval {
		t.Fatalf("a cycle begun after the migration says what started it, got %+v", c)
	}
}

// The daemon's schedule lives with its lock: it is merged into what the lock already says, read back
// as it was written, replaced whole by the next one, and gone with the daemon.
func TestCycleScheduleLivesWithTheDaemonLock(t *testing.T) {
	st := openMovementStore(t)
	ctx := context.Background()
	pid := os.Getpid()

	if sched, err := st.CycleSchedule(ctx); err != nil || sched != nil {
		t.Fatalf("no daemon runs: got %+v (%v)", sched, err)
	}
	if _, err := st.AcquireLock(ctx, "daemon", LockOptions{
		OwnerType: "daemon", PID: pid, TTL: time.Hour,
		Metadata: map[string]any{"web": true, "interval": "1h0m0s"},
	}); err != nil {
		t.Fatal(err)
	}
	if sched, err := st.CycleSchedule(ctx); err != nil || sched != nil {
		t.Fatalf("the daemon has said nothing yet: got %+v (%v)", sched, err)
	}

	latest := time.Date(2026, 11, 10, 6, 18, 20, 0, time.UTC)
	want := CycleSchedule{
		Interval: time.Hour, IdleDelay: 15 * time.Minute,
		Latest: latest, Earliest: latest.Add(-45 * time.Minute), Waits: "busy",
	}
	if err := st.SetCycleSchedule(ctx, pid, want); err != nil {
		t.Fatal(err)
	}
	got, err := st.CycleSchedule(ctx)
	if err != nil || got == nil || *got != want {
		t.Fatalf("the schedule read back: got %+v (%v), want %+v", got, err, want)
	}
	lock, _ := st.GetLock(ctx, "daemon")
	var meta map[string]any
	if err := json.Unmarshal([]byte(lock.MetadataJSON), &meta); err != nil || meta["web"] != true || meta["interval"] != "1h0m0s" {
		t.Fatalf("the rest of what the lock says is kept, got %s (%v)", lock.MetadataJSON, err)
	}

	// Another process cannot write the daemon's schedule.
	if err := st.SetCycleSchedule(ctx, pid+1, CycleSchedule{Interval: time.Minute, Latest: latest}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.CycleSchedule(ctx); got == nil || *got != want {
		t.Fatalf("a process that does not hold the lock changed the schedule: %+v", got)
	}

	// With the advance off, the next schedule says no earliest time and waits for nothing.
	off := CycleSchedule{Interval: time.Hour, Latest: latest.Add(time.Hour), Waits: "ignored without a delay"}
	if err := st.SetCycleSchedule(ctx, pid, off); err != nil {
		t.Fatal(err)
	}
	got, err = st.CycleSchedule(ctx)
	if err != nil || got == nil {
		t.Fatalf("the schedule with the advance off: %+v (%v)", got, err)
	}
	if got.IdleDelay != 0 || !got.Earliest.IsZero() || got.Waits != "" || !got.Latest.Equal(latest.Add(time.Hour)) {
		t.Fatalf("the earlier schedule shows through the new one: %+v", got)
	}

	if err := st.ReleaseLock(ctx, "daemon", pid); err != nil {
		t.Fatal(err)
	}
	if sched, err := st.CycleSchedule(ctx); err != nil || sched != nil {
		t.Fatalf("the daemon stopped: got %+v (%v)", sched, err)
	}
}
