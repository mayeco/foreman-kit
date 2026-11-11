package store

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestViewListsTheEnginesWithTheirHealthAndWhoRunsThem: the engines on record come by name, each
// with its health as the last check left it, the time it is left for last only while that time has
// not passed, and the live processes that run it. The claim a dead process left is not one of them,
// and reading the engines removes nothing.
func TestViewListsTheEnginesWithTheirHealthAndWhoRunsThem(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// One engine answered its ping; one failed and is left for last for an hour; one failed long
	// enough ago for that time to have passed.
	must(st.RecordEngineHealth(ctx, EngineHealthRecord{Engine: "claude", Installed: true, Status: "ok", LastLatencyMS: 420, LastCheckedAt: "2026-11-10T10:00:00Z"}))
	must(st.RecordEngineHealth(ctx, EngineHealthRecord{Engine: "codex", Installed: true, Status: "quota_exceeded", LastError: "429 from the API", CooldownUntil: time.Now().Add(time.Hour).Unix()}))
	must(st.RecordEngineHealth(ctx, EngineHealthRecord{Engine: "opencode", Installed: true, Status: "timeout", LastError: "no answer in 30s", CooldownUntil: time.Now().Add(-time.Hour).Unix()}))

	// This process runs claude for a change and for a cycle, and hermes, which nothing checked yet.
	must(st.ClaimAgent(ctx, AgentClaimID(1, os.Getpid()), "claude", "build", "add-api"))
	must(st.ClaimAgent(ctx, AgentClaimID(2, os.Getpid()), "claude", "cycle", ""))
	must(st.ClaimAgent(ctx, AgentClaimID(3, os.Getpid()), "hermes", "", ""))
	// A process that is gone left its claim on codex: the test binary, run with no test to run.
	gone := exec.Command(os.Args[0], "-test.run=^$")
	must(gone.Run())
	dead := gone.Process.Pid
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO engine_claims (claim_id, owner_pid, host, engine, claimed_at) VALUES (?, ?, 'elsewhere', 'codex', ?)`,
		dead, dead, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	view(t, st, func(v *View) error {
		engines, err := v.ListEngines(ctx)
		if err != nil {
			return err
		}
		if len(engines) != 4 {
			t.Fatalf("ListEngines returns %d engines, want 4: %+v", len(engines), engines)
		}
		claude, codex, hermes, opencode := engines[0], engines[1], engines[2], engines[3]
		if claude.Engine != "claude" || codex.Engine != "codex" || hermes.Engine != "hermes" || opencode.Engine != "opencode" {
			t.Fatalf("the engines do not come by name: %+v", engines)
		}

		if claude.Health != "ok" || claude.ConsecutiveFailures != 0 || claude.LastLatencyMS != 420 || claude.LastCheckedAt != "2026-11-10T10:00:00Z" ||
			claude.LastError != "" || claude.CooldownUntil != "" {
			t.Errorf("the engine that answered its ping = %+v", claude)
		}
		if len(claude.InUse) != 2 {
			t.Fatalf("claude is run by %d processes, want 2: %+v", len(claude.InUse), claude.InUse)
		}
		first, second := claude.InUse[0], claude.InUse[1]
		if first.Role != "build" || first.ChangeName != "add-api" || first.OwnerPID != os.Getpid() || first.ClaimedAt == "" ||
			second.Role != "cycle" || second.ChangeName != "" || second.ClaimID == first.ClaimID {
			t.Errorf("the processes that run claude = %+v", claude.InUse)
		}

		if codex.Health != "quota_exceeded" || codex.ConsecutiveFailures != 1 || codex.LastError != "429 from the API" || codex.LastCheckedAt == "" {
			t.Errorf("the engine out of quota = %+v", codex)
		}
		if until, err := time.Parse(time.RFC3339, codex.CooldownUntil); err != nil || !until.After(time.Now().Add(50*time.Minute)) {
			t.Errorf("codex is left for last until %q, want about an hour from now", codex.CooldownUntil)
		}
		if len(codex.InUse) != 0 {
			t.Errorf("the claim of a process that is gone counts as a process that runs codex: %+v", codex.InUse)
		}

		if opencode.Health != "timeout" || opencode.CooldownUntil != "" {
			t.Errorf("an engine whose time apart has passed = %+v", opencode)
		}
		if hermes.Health != "unknown" || hermes.LastCheckedAt != "" || len(hermes.InUse) != 1 {
			t.Errorf("an engine nothing checked yet = %+v", hermes)
		}
		return nil
	})

	// Reading the engines removed no claim: the one of the dead process is still on record.
	var claims int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM engine_claims`).Scan(&claims); err != nil || claims != 4 {
		t.Errorf("%d claims are on record after the read, want 4 (%v)", claims, err)
	}
}

// TestAPingRecordsTheHealthOfAnEngineAndLeavesItsTimeApartToTheRuns: a ping that answers leaves the
// engine healthy, with no failure and no time apart. A ping that fails says how and counts one more
// failure, and neither shortens nor lengthens the time the runs of the kit set the engine apart for.
func TestAPingRecordsTheHealthOfAnEngineAndLeavesItsTimeApartToTheRuns(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	engine := func(name string) EngineRecord {
		t.Helper()
		var found *EngineRecord
		view(t, st, func(v *View) error {
			engines, err := v.ListEngines(ctx)
			for i := range engines {
				if engines[i].Engine == name {
					found = &engines[i]
				}
			}
			return err
		})
		if found == nil {
			t.Fatalf("the engine %s is not on record", name)
		}
		return *found
	}

	// An engine nothing ran yet: its first ping puts it on record.
	must(st.RecordEnginePing(ctx, "claude", "ok", 380, ""))
	if e := engine("claude"); e.Health != "ok" || e.LastLatencyMS != 380 || e.ConsecutiveFailures != 0 || e.LastCheckedAt == "" || e.CooldownUntil != "" {
		t.Errorf("an engine that answered its first ping = %+v", e)
	}

	// A run found codex out of quota and set it apart for half an hour. A ping that fails says what
	// it found and counts one more failure; the half hour stays.
	apart := time.Now().Add(30 * time.Minute).Unix()
	must(st.RecordEngineHealth(ctx, EngineHealthRecord{Engine: "codex", Installed: true, Status: "quota_exceeded", LastError: "out of quota", CooldownUntil: apart}))
	before := engine("codex").CooldownUntil
	must(st.RecordEnginePing(ctx, "codex", "auth_failed", 950, "exit 1: not signed in"))
	e := engine("codex")
	if e.Health != "auth_failed" || e.LastError != "exit 1: not signed in" || e.LastLatencyMS != 950 || e.ConsecutiveFailures != 2 {
		t.Errorf("an engine whose ping failed = %+v", e)
	}
	if e.CooldownUntil == "" || e.CooldownUntil != before {
		t.Errorf("a ping that failed moved the time codex is set apart for from %q to %q", before, e.CooldownUntil)
	}
	// A failed ping of an engine that was not set apart does not set it apart.
	must(st.RecordEnginePing(ctx, "opencode", "timeout", 90000, "no answer in 90s"))
	if e := engine("opencode"); e.Health != "timeout" || e.CooldownUntil != "" || e.ConsecutiveFailures != 1 {
		t.Errorf("an engine whose first ping timed out = %+v", e)
	}
	// What a ping calls a health the schema does not know is recorded as unknown, not refused.
	must(st.RecordEnginePing(ctx, "hermes", "failed", 10, "exit 2"))
	if e := engine("hermes"); e.Health != "unknown" || e.LastCheckedAt == "" {
		t.Errorf("a ping with a health the schema does not know = %+v", e)
	}

	// The ping that answers brings codex back: healthy, no failure, not set apart.
	must(st.RecordEnginePing(ctx, "codex", "ok", 410, ""))
	if e := engine("codex"); e.Health != "ok" || e.ConsecutiveFailures != 0 || e.CooldownUntil != "" || e.LastError != "" {
		t.Errorf("an engine that answers again = %+v", e)
	}
	if err := st.RecordEnginePing(ctx, " ", "ok", 0, ""); err == nil {
		t.Errorf("a ping of no engine was recorded")
	}
}
