package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenAt(filepath.Join(dir, "state.db"), "schema-test", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestSchemaIsVersionedStrictAndRelated: a new database is at the schema version of the binary, every
// table is STRICT, and the relations the model promises are declared as foreign keys.
func TestSchemaIsVersionedStrictAndRelated(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	var version int
	if err := st.DB().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion() || version < 1 {
		t.Fatalf("user_version = %d, want %d", version, SchemaVersion())
	}

	var tables, loose int
	if err := st.DB().QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(strict = 0), 0) FROM pragma_table_list
		WHERE type = 'table' AND schema = 'main' AND name NOT LIKE 'sqlite_%'
	`).Scan(&tables, &loose); err != nil {
		t.Fatal(err)
	}
	if tables != 35 || loose != 0 {
		t.Fatalf("got %d tables, %d of them not STRICT; want 35 and 0", tables, loose)
	}

	var fks int
	if err := st.DB().QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT m.name || '#' || f.id) FROM sqlite_master m, pragma_foreign_key_list(m.name) f
		WHERE m.type = 'table'
	`).Scan(&fks); err != nil {
		t.Fatal(err)
	}
	if fks < 70 {
		t.Fatalf("got %d foreign keys, want at least 70", fks)
	}

	// None of the tables of the unversioned schema is left.
	for _, old := range []string{"workspace_state", "agents_in_use", "ready_queue", "change_quarantine", "quarantine_history",
		"visual_qa_queue", "engine_health", "base_rechecks", "backlog_seen"} {
		var n int
		_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, old).Scan(&n)
		if n != 0 {
			t.Errorf("table %s of the earlier schema still exists", old)
		}
	}

	// Opening it again changes nothing.
	again, err := OpenAt(st.DBPath(), st.WorkspaceID(), st.CanonicalRoot())
	if err != nil {
		t.Fatal(err)
	}
	_ = again.Close()
}

// TestForeignKeysRejectRowsWithoutParent: the database itself refuses a row that names a change, an
// execution or a run that does not exist, and a state outside its list.
func TestForeignKeysRejectRowsWithoutParent(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	for name, stmt := range map[string]string{
		"execution of an unknown change":  `INSERT INTO executions (exec_id, kind, change_name, started_at) VALUES ('e1', 'worker', 'ghost', 1)`,
		"event of an unknown execution":   `INSERT INTO events (ts, event, pid, exec_id) VALUES (1, 'agent.exit', 1, 'nope')`,
		"worker execution with no change": `INSERT INTO executions (exec_id, kind, started_at) VALUES ('e2', 'worker', 1)`,
		"execution status off its list":   `INSERT INTO executions (exec_id, kind, status, started_at) VALUES ('e3', 'portal_run', 'done', 1)`,
		"text in a time column":           `INSERT INTO events (ts, event, pid) VALUES ('yesterday', 'x', 1)`,
		"payload that is not JSON":        `INSERT INTO events (ts, event, pid, payload_json) VALUES (1, 'x', 1, '{bad')`,
	} {
		if _, err := st.DB().ExecContext(ctx, stmt); err == nil {
			t.Errorf("%s: the insert was accepted", name)
		}
	}

	// An execution cannot claim to belong to one change and to the run of another.
	runA, err := st.BeginChangeRun(ctx, "change-a", 1, "/wt/a", "feat/spec-change-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ensureChangeTx(ctx, st.DB(), "change-b", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `
		INSERT INTO executions (exec_id, kind, change_name, change_run_id, started_at) VALUES ('e4', 'worker', 'change-b', ?, 1)
	`, runA); err == nil {
		t.Errorf("an execution of change-b in the run of change-a was accepted")
	}

	// History is not deleted: a change with a run cannot be removed.
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM changes WHERE change_name = 'change-a'`); err == nil {
		t.Errorf("a change with history was deleted")
	}
}

// TestLegacyDatabaseIsSetAsideNotDestroyed: a state database written before schema versions existed
// is kept under another name and a new one is created; while a process of that earlier version still
// holds a live lock in it, Open refuses instead.
func TestLegacyDatabaseIsSetAsideNotDestroyed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	host, _ := os.Hostname()

	plant := func(lockPID int) {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		now := time.Now().Unix()
		if _, err := db.Exec(`
			CREATE TABLE workspace_state (id INTEGER PRIMARY KEY, workspace_id TEXT);
			CREATE TABLE locks (lock_key TEXT PRIMARY KEY, owner_type TEXT, pid INTEGER, host TEXT, acquired_at INTEGER, heartbeat_at INTEGER, ttl_seconds INTEGER);
			CREATE TABLE events (id INTEGER PRIMARY KEY, event TEXT);
			INSERT INTO events (event) VALUES ('kept');
		`); err != nil {
			t.Fatal(err)
		}
		if lockPID > 0 {
			if _, err := db.Exec(`INSERT INTO locks VALUES ('daemon', 'daemon', ?, ?, ?, ?, 7200)`, lockPID, host, now, now); err != nil {
				t.Fatal(err)
			}
		}
	}

	// A live process of the earlier version holds the daemon lock: refuse, and leave the file alone.
	plant(os.Getppid())
	if _, err := OpenAt(dbPath, "legacy-test", dir); !errors.Is(err, ErrLegacyDatabaseInUse) {
		t.Fatalf("OpenAt over a legacy database in use = %v, want ErrLegacyDatabaseInUse", err)
	}
	if matches, _ := filepath.Glob(dbPath + ".legacy-*"); len(matches) != 0 {
		t.Fatalf("the legacy database in use was moved: %v", matches)
	}

	// Nobody holds it: it is set aside whole and a new database takes its place.
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	plant(0)
	st, err := OpenAt(dbPath, "legacy-test", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var version int
	_ = st.DB().QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != SchemaVersion() {
		t.Fatalf("the new database has user_version %d, want %d", version, SchemaVersion())
	}
	matches, _ := filepath.Glob(dbPath + ".legacy-*")
	var aside string
	for _, m := range matches {
		if !strings.HasSuffix(m, "-wal") && !strings.HasSuffix(m, "-shm") {
			aside = m
		}
	}
	if aside == "" {
		t.Fatalf("the legacy database was not kept: %v", matches)
	}
	old, err := sql.Open("sqlite", "file:"+aside)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var kept string
	if err := old.QueryRow(`SELECT event FROM events`).Scan(&kept); err != nil || kept != "kept" {
		t.Fatalf("the legacy database lost its rows: %q %v", kept, err)
	}
}

// TestQuarantineIsDerivedFromBlocksThatAreNeverOverwritten: the record the orchestrator reads follows
// the rows, and every block stays on record after it is resolved.
func TestQuarantineIsDerivedFromBlocksThatAreNeverOverwritten(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const change = "add-login"

	if q, _ := st.GetQuarantine(ctx, change); q != nil {
		t.Fatalf("a change never blocked has a quarantine: %+v", q)
	}
	if err := st.BlockChange(ctx, QuarantineRecord{ChangeName: change, Reason: "gate_failure", Details: "tests failed", Branch: "feat/spec-add-login"}); err != nil {
		t.Fatal(err)
	}
	q, _ := st.GetQuarantine(ctx, change)
	if q == nil || q.Status != "blocked" || q.Reason != "gate_failure" || q.Retries != 0 {
		t.Fatalf("after the first block: %+v", q)
	}

	// The dispatcher retries it: one retry counted, and the block is what the next agent is told.
	q, err := st.AdvanceQuarantineRetry(ctx, change)
	if err != nil || q == nil || q.Status != "cleared" || q.Retries != 1 || q.RetryTold || !strings.Contains(q.LastRetryJSON, "tests failed") {
		t.Fatalf("after the retry: %+v (%v)", q, err)
	}
	if err := st.MarkRetryTold(ctx, change); err != nil {
		t.Fatal(err)
	}
	if q, _ = st.GetQuarantine(ctx, change); !q.RetryTold {
		t.Fatalf("the retry was not marked as told: %+v", q)
	}

	// Blocked again: the count carries on, and the first block is still there.
	if err := st.BlockChange(ctx, QuarantineRecord{ChangeName: change, Reason: "spec_invalid", Details: "second"}); err != nil {
		t.Fatal(err)
	}
	if q, _ = st.GetQuarantine(ctx, change); q.Status != "blocked" || q.Retries != 1 || q.Reason != "spec_invalid" || q.Branch != "feat/spec-add-login" {
		t.Fatalf("after the second block: %+v", q)
	}
	if blocked, _ := st.ListQuarantine(ctx, "blocked"); len(blocked) != 1 {
		t.Fatalf("ListQuarantine(blocked) = %+v", blocked)
	}

	// An operator unblocks it: nothing is in force any more, and both blocks stay on record.
	if err := st.ResolveQuarantine(ctx, change, QuarantineResolution{Resolution: BlockUnblocked, By: "cli", Forget: true}); err != nil {
		t.Fatal(err)
	}
	if q, _ = st.GetQuarantine(ctx, change); q != nil {
		t.Fatalf("after the unblock: %+v", q)
	}
	history, _ := st.ListQuarantineHistory(ctx, change, 0)
	if len(history) != 2 || history[0].Resolution != BlockUnblocked || history[0].ResolvedBy != "cli" || history[1].Resolution != BlockAutoRetry {
		t.Fatalf("history = %+v", history)
	}

	// A block after that starts the count over.
	if err := st.BlockChange(ctx, QuarantineRecord{ChangeName: change, Reason: "gate_failure"}); err != nil {
		t.Fatal(err)
	}
	if q, _ = st.GetQuarantine(ctx, change); q.Retries != 0 {
		t.Fatalf("retries after an unblock = %d, want 0", q.Retries)
	}
}

// TestRebuildRequestsCountUntilARebaseConflictIsRetried: rebuilds the merger asks for keep counting
// across an unrelated retry and start over when the block for running out of them is retried.
func TestRebuildRequestsCountUntilARebaseConflictIsRetried(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const change = "add-cart"

	for want := 1; want <= 2; want++ {
		n, err := st.RequestRebuild(ctx, change, "feat/spec-add-cart", "conflict in cart.go")
		if err != nil || n != want {
			t.Fatalf("RequestRebuild #%d = %d, %v", want, n, err)
		}
		q, _ := st.GetQuarantine(ctx, change)
		if q.Status != "rebuild_requested" || q.Rebuilds != want || q.ConflictDetails != "conflict in cart.go" {
			t.Fatalf("after request #%d: %+v", want, q)
		}
		// A worker consumes it.
		if err := st.ClearQuarantine(ctx, change, false); err != nil {
			t.Fatal(err)
		}
		if q, _ = st.GetQuarantine(ctx, change); q.Status != "cleared" || q.Rebuilds != want {
			t.Fatalf("after consuming request #%d: %+v", want, q)
		}
	}

	// A gate failure is retried: the rebuilds still count.
	_ = st.BlockChange(ctx, QuarantineRecord{ChangeName: change, Reason: "gate_failure"})
	if q, _ := st.AdvanceQuarantineRetry(ctx, change); q.Rebuilds != 2 {
		t.Fatalf("rebuilds after an unrelated retry = %d, want 2", q.Rebuilds)
	}
	// Out of rebuilds, blocked for it, retried: the count starts over.
	_ = st.BlockChange(ctx, QuarantineRecord{ChangeName: change, Reason: "rebase_conflict"})
	if q, _ := st.AdvanceQuarantineRetry(ctx, change); q.Rebuilds != 0 || q.Retries != 2 {
		t.Fatalf("after retrying the rebase_conflict block: %+v", q)
	}
	if n, _ := st.RequestRebuild(ctx, change, "feat/spec-add-cart", "again"); n != 1 {
		t.Fatalf("the first rebuild after the count started over = %d, want 1", n)
	}
}

// TestRiskApprovalResolvesOnlyBlocksAPersonCanAccept.
func TestRiskApprovalResolvesOnlyBlocksAPersonCanAccept(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	_ = st.BlockChange(ctx, QuarantineRecord{ChangeName: "risky", Reason: "risk_review"})
	if err := st.ApproveChangeRiskBy(ctx, "risky", "portal"); err != nil {
		t.Fatal(err)
	}
	if q, _ := st.GetQuarantine(ctx, "risky"); q.Status != "approved" || !q.ApprovedRisk {
		t.Fatalf("a risk_review block that was approved: %+v", q)
	}
	_ = st.BlockChange(ctx, QuarantineRecord{ChangeName: "broken", Reason: "gate_failure"})
	_ = st.ApproveChangeRisk(ctx, "broken")
	if q, _ := st.GetQuarantine(ctx, "broken"); q.Status != "blocked" || !q.ApprovedRisk {
		t.Fatalf("a gate_failure block with an approval: %+v", q)
	}
	_ = st.RevokeRiskApproval(ctx, "risky", "head_updated")
	if q, _ := st.GetQuarantine(ctx, "risky"); q.ApprovedRisk {
		t.Fatalf("the approval was not revoked: %+v", q)
	}
}

// TestExecutionKnowsItsRunItsAttemptsAndItsEvents: the case the model was drawn for. The agent.exit
// event of an execution names it, the execution says which engine did the work and each try of each
// engine is a row.
func TestExecutionKnowsItsRunItsAttemptsAndItsEvents(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const change = "add-search"

	runID, err := st.BeginChangeRun(ctx, change, 2, "/wt/add-search", "feat/spec-add-search")
	if err != nil {
		t.Fatal(err)
	}
	runCtx := WithChangeRun(ctx, runID, change)
	_ = st.EmitEvent(runCtx, EventInput{Event: "change.start", Change: change})

	for _, execID := range []string{"exec-plan", "exec-apply"} {
		if err := st.CreateExecution(runCtx, Execution{
			ExecID: execID, Kind: "worker", ChangeName: change, Phase: strings.TrimPrefix(execID, "exec-"),
			Role: "build", EnginesRequested: "claude,opencode", PromptText: "do it", PromptVersion: "p1", MemoryVersion: "m1",
		}); err != nil {
			t.Fatal(err)
		}
		execCtx := WithExecution(runCtx, execID)
		first, _ := st.BeginAttempt(execCtx, execID, "claude", 0)
		_ = st.AppendLogChunk(WithAttempt(execCtx, first), execID, "combined", "out of quota\n")
		_ = st.FinishAttempt(execCtx, first, 1, AttemptQuotaExceeded, 0)
		second, _ := st.BeginAttempt(execCtx, execID, "opencode", 0)
		_ = st.AddExecutionProcessGroup(WithAttempt(execCtx, second), execID, 4242)
		_ = st.FinishAttempt(execCtx, second, 0, AttemptOK, 0)
		// A gate round of the run, between this execution and the next.
		_ = st.EmitEvent(runCtx, EventInput{Event: "change.gates_accepted", Change: change, Payload: map[string]any{"after": execID}})
		if wrote, err := st.EndExecution(runCtx, execID, ExecutionEnd{Status: "succeeded", EngineUsed: "opencode", AgentExit: true}); err != nil || !wrote {
			t.Fatalf("EndExecution(%s) = %v, %v", execID, wrote, err)
		}
	}

	ex, _ := st.GetExecution(ctx, "exec-plan")
	if ex == nil || ex.ChangeRunID != runID || ex.EngineUsed != "opencode" || ex.EnginesRequested != "claude,opencode" ||
		ex.Agent != "opencode" || ex.Status != "succeeded" || ex.PromptVersion != "p1" || ex.FinishedAt == "" {
		t.Fatalf("execution = %+v", ex)
	}
	attempts, _ := st.ListAttempts(ctx, "exec-plan")
	if len(attempts) != 2 || attempts[0].Engine != "claude" || attempts[0].Outcome != AttemptQuotaExceeded || attempts[1].Outcome != AttemptOK {
		t.Fatalf("attempts = %+v", attempts)
	}

	// The exit event names its execution and its run, and carries what the row says.
	exits, _ := st.QueryEvents(ctx, EventFilter{Event: "agent.exit", ExecID: "exec-plan"})
	if len(exits) != 1 || exits[0].ChangeRunID != runID || exits[0].Engine != "opencode" || exits[0].Phase != "plan" ||
		exits[0].Payload["engines"] != "claude,opencode" || exits[0].Payload["prompt_version"] != "p1" {
		t.Fatalf("agent.exit of exec-plan = %+v", exits)
	}

	// The events of each execution are exact: its own exit, and what its run emitted until the next one started.
	planEvents, _ := st.EventsOfExecution(ctx, "exec-plan")
	applyEvents, _ := st.EventsOfExecution(ctx, "exec-apply")
	names := func(evs []EventRecord) string {
		var out []string
		for _, e := range evs {
			out = append(out, e.Event)
		}
		return strings.Join(out, ",")
	}
	if names(planEvents) != "change.gates_accepted,agent.exit" || planEvents[0].Payload["after"] != "exec-plan" {
		t.Fatalf("events of exec-plan = %s", names(planEvents))
	}
	if names(applyEvents) != "change.gates_accepted,agent.exit" || applyEvents[0].Payload["after"] != "exec-apply" {
		t.Fatalf("events of exec-apply = %s", names(applyEvents))
	}
	if next, _ := st.NextExecution(ctx, "exec-plan"); next == nil || next.ExecID != "exec-apply" {
		t.Fatalf("NextExecution(exec-plan) = %+v", next)
	}
	if next, _ := st.NextExecution(ctx, "exec-apply"); next != nil {
		t.Fatalf("NextExecution(exec-apply) = %+v, want none", next)
	}

	// A running execution with its process groups, for the orphan reaper.
	_ = st.CreateExecution(runCtx, Execution{ExecID: "exec-live", Kind: "worker", ChangeName: change, Phase: "repair-1", PID: os.Getpid()})
	_ = st.AddExecutionProcessGroup(ctx, "exec-live", 777)
	running, _ := st.ListRunningExecutions(ctx)
	if len(running) != 1 || running[0].ExecID != "exec-live" || len(running[0].PGIDs) != 1 || running[0].PGIDRecordedAt[777] == 0 {
		t.Fatalf("running = %+v", running)
	}

	if err := st.FinishChangeRun(ctx, runID, "ready"); err != nil {
		t.Fatal(err)
	}
	runs, _ := st.ListChangeRuns(ctx, change, 0)
	if len(runs) != 1 || runs[0].Result != "ready" || runs[0].Slot != 2 || runs[0].Origin != "backlog" {
		t.Fatalf("runs = %+v", runs)
	}
}

// TestGateRoundsAndRepairRoundsBelongToARun.
func TestGateRoundsAndRepairRoundsBelongToARun(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const change = "add-billing"

	if err := st.RecordRepairRound(ctx, RepairRoundRecord{ChangeName: change, Phase: "repair-1", AskedReason: "gate_failure", AskedDetails: "x"}); !errors.Is(err, ErrNoChangeRun) {
		t.Fatalf("a repair round outside a run = %v, want ErrNoChangeRun", err)
	}

	runID, _ := st.BeginChangeRun(ctx, change, 1, "/wt", "feat/spec-add-billing")
	runCtx := WithChangeRun(ctx, runID, change)
	_ = st.CreateExecution(runCtx, Execution{ExecID: "exec-repair", Kind: "worker", ChangeName: change, Phase: "repair-1", EnginesRequested: "claude"})
	_, _ = st.EndExecution(runCtx, "exec-repair", ExecutionEnd{Status: "succeeded", EngineUsed: "claude"})

	failed, err := st.BeginGateRound(runCtx, GateRound{Context: "worker_verify", ChangeName: change, Slot: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordGateRun(WithGateRound(runCtx, failed), GateRunRecord{ChangeName: change, Context: "worker_verify", RepoName: "api", RepoPath: "repos/api", GateName: "test", Command: "go test ./...", ExitCode: 1, DurationMS: 900, FailureExcerpt: "--- FAIL"}); err != nil {
		t.Fatal(err)
	}
	_ = st.FinishGateRound(ctx, failed, "failed")
	passed, _ := st.BeginGateRound(runCtx, GateRound{Context: "worker_verify", ChangeName: change, Slot: 1})
	_ = st.RecordGateRun(WithGateRound(runCtx, passed), GateRunRecord{ChangeName: change, RepoName: "api", GateName: "test", Command: "go test ./...", DurationMS: 800})
	_ = st.FinishGateRound(ctx, passed, "passed")

	if err := st.RecordRepairRound(runCtx, RepairRoundRecord{
		ChangeName: change, Phase: "repair-1", ExecID: "exec-repair", AskedReason: "gate_failure", AskedDetails: "--- FAIL",
		AskedGateRoundID: failed, ResultPassed: true,
	}); err != nil {
		t.Fatal(err)
	}
	rounds, _ := st.ListRepairRounds(ctx, change)
	if len(rounds) != 1 || rounds[0].Engine != "claude" || rounds[0].AskedGateRoundID != failed || rounds[0].ResultGateRoundID != passed || rounds[0].ChangeRunID != runID {
		t.Fatalf("repair rounds = %+v", rounds)
	}
	gates, _ := st.ListGateRuns(ctx, change)
	if len(gates) != 2 || gates[0].RepoPath != "repos/api" || gates[0].RepoName != "api" || gates[1].RepoPath != "repos/api" ||
		gates[0].Context != "worker_verify" || gates[0].GateRoundID != failed {
		t.Fatalf("gate runs = %+v", gates)
	}
	// A gate run outside any round gets a round of its own, from the command line.
	_ = st.RecordGateRun(ctx, GateRunRecord{Context: "worker_verify", RepoName: "openspec", GateName: "specs", Command: "openspec validate"})
	all, _ := st.ListGateRuns(ctx, "")
	if len(all) != 3 || all[2].Context != "cli" || all[2].RepoPath != UmbrellaRepoPath {
		t.Fatalf("a gate run outside a round = %+v", all[len(all)-1])
	}
}

// TestManifestAndMergeAttemptsAreClosedNotDeleted.
func TestManifestAndMergeAttemptsAreClosedNotDeleted(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const change = "add-export"

	if _, err := st.BeginMergeAttempt(ctx, change); !errors.Is(err, ErrNoReadyManifest) {
		t.Fatalf("a merge attempt with no manifest = %v", err)
	}
	if err := st.EnqueueReady(ctx, ReadyManifest{ChangeName: change, Branch: "feat/spec-add-export", Worktree: "/wt", HeadSHA: "abc123", Capabilities: []string{"exports", "billing"}}); err != nil {
		t.Fatal(err)
	}
	if ws, _ := st.GetWorkspaceState(ctx); ws.MergerState != "IDLE" || ws.MergerChange != "" {
		t.Fatalf("an idle merger: %+v", ws)
	}

	first, err := st.BeginMergeAttempt(ctx, change)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetMergeAttemptState(ctx, first, "PUSHING_CHILDREN")
	if ws, _ := st.GetWorkspaceState(ctx); ws.MergerState != "PUSHING_CHILDREN" || ws.MergerChange != change {
		t.Fatalf("a merger in the middle of a change: %+v", ws)
	}
	mergeCtx := WithMergeAttempt(ctx, first, change)
	if err := st.RecordChildPushed(mergeCtx, change, "repos/api", "sha-api"); err != nil {
		t.Fatal(err)
	}
	_ = st.UpdateReadyHead(mergeCtx, change, "def456")

	// The merger dies; the next one begins its own attempt, which closes the one left open.
	second, err := st.BeginMergeAttempt(ctx, change)
	if err != nil {
		t.Fatal(err)
	}
	queue, _ := st.ListReadyQueue(ctx)
	if len(queue) != 1 || queue[0].HeadSHA != "def456" || queue[0].ChildrenPushed["repos/api"] != "sha-api" || strings.Join(queue[0].Capabilities, ",") != "billing,exports" {
		t.Fatalf("queue = %+v", queue)
	}
	_ = st.MarkMergeAttemptIntegrated(ctx, second, "commit-1", `{"added":1}`)
	_ = st.FinishMergeAttempt(ctx, second, MergeIntegrated, "")
	_ = st.CloseReady(ctx, change, ManifestIntegrated)

	if queue, _ = st.ListReadyQueue(ctx); len(queue) != 0 {
		t.Fatalf("the queue after integrating: %+v", queue)
	}
	attempts, _ := st.ListMergeAttempts(ctx, change, 0)
	if len(attempts) != 2 || attempts[0].Result != MergeIntegrated || attempts[0].IntegratedCommit != "commit-1" || attempts[0].ResumedFromID != first ||
		attempts[1].Result != MergeStopped || attempts[1].BranchTipSHA != "def456" || attempts[1].ManifestHeadSHA != "abc123" {
		t.Fatalf("attempts = %+v", attempts)
	}
	var outcome string
	if err := st.DB().QueryRowContext(ctx, `SELECT outcome FROM ready_manifests WHERE change_name = ?`, change).Scan(&outcome); err != nil || outcome != ManifestIntegrated {
		t.Fatalf("the manifest after integrating: %q %v", outcome, err)
	}
	// The merger says so in the registry at once; the mirror of Git confirms it later.
	if state, _ := st.ChangeState(ctx, change); state != "unknown" {
		t.Fatalf("v_change_state before the merger marks the change = %q", state)
	}
	if err := st.MarkChangeIntegrated(ctx, change, "commit-1"); err != nil {
		t.Fatal(err)
	}
	if state, _ := st.ChangeState(ctx, change); state != "integrated" {
		t.Fatalf("v_change_state of an integrated change = %q", state)
	}
}

// TestPauseIsARowWithWhoPutItAndWhoLiftedIt.
func TestPauseIsARowWithWhoPutItAndWhoLiftedIt(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if paused, err := st.Pause(ctx, "disk full", "dispatcher", ""); err != nil || !paused {
		t.Fatalf("Pause = %v, %v", paused, err)
	}
	if paused, _ := st.Pause(ctx, "another reason", "merger", ""); paused {
		t.Fatalf("a workspace already paused was paused again")
	}
	ws, _ := st.GetWorkspaceState(ctx)
	if !ws.Paused || ws.PauseReason != "disk full" || ws.PausedAt == "" {
		t.Fatalf("workspace = %+v", ws)
	}
	if resumed, err := st.Resume(ctx, "portal"); err != nil || !resumed {
		t.Fatalf("Resume = %v, %v", resumed, err)
	}
	if ws, _ = st.GetWorkspaceState(ctx); ws.Paused {
		t.Fatalf("workspace still paused: %+v", ws)
	}
	var by, resumedBy string
	if err := st.DB().QueryRowContext(ctx, `SELECT paused_by, resumed_by FROM workspace_pauses`).Scan(&by, &resumedBy); err != nil || by != "dispatcher" || resumedBy != "portal" {
		t.Fatalf("pause row: %q %q %v", by, resumedBy, err)
	}
	events, _ := st.QueryEvents(ctx, EventFilter{})
	if len(events) != 2 || events[0].Event != "system.paused" || events[1].Event != "system.resumed" {
		t.Fatalf("events = %+v", events)
	}
}

// TestLogRetentionPrunesWholeExecutionsAndSaysSo.
func TestLogRetentionPrunesWholeExecutionsAndSaysSo(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	budget := LogChunkBudget
	LogChunkBudget = 5
	t.Cleanup(func() { LogChunkBudget = budget })

	for _, id := range []string{"old", "mid", "new"} {
		_ = st.CreateExecution(ctx, Execution{ExecID: id, Kind: "portal_run", Status: "running"})
		for i := 0; i < 4; i++ {
			_ = st.AppendLogChunk(ctx, id, "combined", id+" line\n")
		}
		_ = st.FinishExecution(ctx, id, "succeeded", 0)
	}
	pruned, err := st.PruneLogs(ctx, time.Now())
	if err != nil || pruned != 2 {
		t.Fatalf("PruneLogs = %d, %v; want the two oldest", pruned, err)
	}
	for id, wantLog := range map[string]bool{"old": false, "mid": false, "new": true} {
		ex, _ := st.GetExecution(ctx, id)
		log, _ := st.ReadFullLog(ctx, id)
		if (log != "") != wantLog || (ex.LogPrunedAt == "") != wantLog {
			t.Errorf("%s: log kept = %v, log_pruned_at = %q", id, log != "", ex.LogPrunedAt)
		}
	}
}

// TestSyncJSONMirrorsTheBacklogWithItsDependenciesAndLocks.
func TestSyncJSONMirrorsTheBacklogWithItsDependenciesAndLocks(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".foreman-kit"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".foreman-kit", "agent_history.json"), []byte(`[{"change_name":"add-base","integrated_at":"2026-11-01T10:00:00Z","commit":"c0ffee"}]`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".foreman-kit", "agent_backlog.json"), []byte(`[
		{"change_name":"add-ui","priority":"high","description":"UI","depends_on":["add-base","add-api"],"locks":"db-schema, routes"},
		{"change_name":"add-api","depends_on":"add-base"}
	]`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".foreman-kit", "agent_proposals.json"), []byte(`[{"change_name":"add-ui","status":"queued","kind":"change","description":"UI"}]`), 0o644)

	st, err := OpenAt(filepath.Join(dir, "state.db"), "sync-test", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for i := 0; i < 2; i++ { // twice: the mirror replaces, it does not pile up
		if err := st.SyncJSONToSQLite(ctx, dir); err != nil {
			t.Fatal(err)
		}
	}

	status := func(name string) string {
		var s string
		_ = st.DB().QueryRowContext(ctx, `SELECT backlog_status FROM changes WHERE change_name = ?`, name).Scan(&s)
		return s
	}
	if status("add-base") != "integrated" || status("add-ui") != "queued" || status("add-api") != "queued" {
		t.Fatalf("backlog status: base=%s ui=%s api=%s", status("add-base"), status("add-ui"), status("add-api"))
	}
	var deps, locks, proposal string
	_ = st.DB().QueryRowContext(ctx, `SELECT group_concat(depends_on, ',') FROM (SELECT depends_on FROM change_dependencies WHERE change_name = 'add-ui' ORDER BY depends_on)`).Scan(&deps)
	_ = st.DB().QueryRowContext(ctx, `SELECT group_concat(lock_name, ',') FROM (SELECT lock_name FROM change_locks WHERE change_name = 'add-ui' ORDER BY lock_name)`).Scan(&locks)
	_ = st.DB().QueryRowContext(ctx, `SELECT COALESCE(proposal_id, '') FROM changes WHERE change_name = 'add-ui'`).Scan(&proposal)
	if deps != "add-api,add-base" || locks != "db-schema,routes" || proposal != "add-ui" {
		t.Fatalf("deps=%q locks=%q proposal=%q", deps, locks, proposal)
	}
	if state, _ := st.ChangeState(ctx, "add-base"); state != "integrated" {
		t.Fatalf("v_change_state(add-base) = %q", state)
	}
}
