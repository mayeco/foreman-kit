package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// portalWorld is a workspace in the middle of its work, written with the store's own functions:
//
//	add-base    integrated
//	add-api     built in one run (a fallback from claude to opencode, an idle stretch, a failed gate
//	            round, a repair, a passed round), queued for the merger, one merge attempt rejected
//	            at the push and a second one in progress
//	add-ui      queued: it depends on add-api and declares a lock add-api holds
//	add-search  building, with an agent idle for six minutes
//	add-export  building as far as its run says, but nothing holds its lease
//	add-billing blocked after a run; a person sent a prompt and ran the gates by hand afterwards
//	add-report  the merger asked to rebuild it, and no engine could take it
//
// plus a paused workspace, a broken base, two proposals nobody decided on and a cycle.
type portalWorld struct {
	st          *Store
	dir         string
	dispatch    int64
	apiRun      int64
	billingRun  int64
	firstMerge  int64
	secondMerge int64
	failedRound int64
	passedRound int64
	riskEval    int64
}

func seedPortalWorld(t *testing.T) portalWorld {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, ".foreman-kit", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("agent_history.json", `[{"change_name":"add-base","integrated_at":"2026-11-01T10:00:00Z","commit":"c0ffee","description":"The base"}]`)
	write("agent_backlog.json", `[
		{"change_name":"add-ui","priority":"high","description":"The UI of the search","depends_on":["add-base","add-api"],"locks":"db-schema, routes","queued_at":"2026-11-02T10:00:00Z"},
		{"change_name":"add-api","description":"The API","depends_on":"add-base","locks":"db-schema","queued_at":"2026-11-02T09:00:00Z"},
		{"change_name":"add-search","priority":"critical","description":"Full-text search","queued_at":"2026-11-03T09:00:00Z"},
		{"change_name":"add-export","priority":"low","description":"CSV export","queued_at":"2026-11-03T10:00:00Z"},
		{"change_name":"add-billing","description":"Billing with 100% coverage_","queued_at":"2026-11-03T11:00:00Z"},
		{"change_name":"add-report","description":"Monthly report","depends_on":"add-export","queued_at":"2026-11-03T12:00:00Z"}
	]`)
	write("agent_proposals.json", `[
		{"change_name":"add-ui","status":"queued","kind":"change","description":"The UI of the search","proposed_at":"2026-11-01T08:00:00Z"},
		{"change_name":"add-cache","status":"proposed","kind":"change","priority":"high","description":"Cache the feed","source":"exploration","proposed_at":"2026-11-04T08:00:00Z"},
		{"change_name":"add-feed","status":"proposed","kind":"change","description":"RSS feed","review":{"answers":{"well_scoped":0.9}},"proposed_at":"2026-11-04T09:00:00Z"},
		{"change_name":"add-old","status":"rejected","kind":"change","description":"An old idea","proposed_at":"2026-10-01T09:00:00Z"}
	]`)

	st, err := OpenAt(filepath.Join(dir, "state.db"), "view-test", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// An idle stretch cannot begin before its attempt: the attempt has been running for half an hour.
	longRunning := func(attempt int64) {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx, `UPDATE execution_attempts SET started_at = started_at - 1800000 WHERE id = ?`, attempt); err != nil {
			t.Fatal(err)
		}
	}
	must(st.SyncJSONToSQLite(ctx, dir))
	w := portalWorld{st: st, dir: dir}

	w.dispatch, err = st.BeginDispatcherRun(ctx, "once", 2, 6)
	must(err)
	dctx := WithDispatcherRun(ctx, w.dispatch)

	// ---- add-api: one run, then the merge queue.
	w.apiRun, err = st.BeginChangeRun(dctx, "add-api", 1, "/wt/worker-add-api", "feat/spec-add-api")
	must(err)
	apiCtx := WithChangeRun(dctx, w.apiRun, "add-api")
	must(st.EmitEvent(apiCtx, EventInput{Event: "change.start", Change: "add-api"}))
	must(st.CreateExecution(apiCtx, Execution{ExecID: "api-plan", Kind: "worker", ChangeName: "add-api", Phase: "plan", Role: "plan",
		EnginesRequested: "claude,opencode", PromptText: strings.Repeat("plan the API. ", 60)}))
	planCtx := WithExecution(apiCtx, "api-plan")
	first, err := st.BeginAttempt(planCtx, "api-plan", "claude", 0)
	must(err)
	must(st.FinishAttempt(planCtx, first, 1, AttemptQuotaExceeded, 0))
	second, err := st.BeginAttempt(planCtx, "api-plan", "opencode", 0)
	must(err)
	must(st.FinishAttempt(planCtx, second, 0, AttemptOK, 0))
	if _, err := st.EndExecution(apiCtx, "api-plan", ExecutionEnd{Status: "succeeded", EngineUsed: "opencode"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordEvaluation(apiCtx, Evaluation{Gate: "plan_review", Mode: "enforce", Status: "passed", Model: "jev-1", CostUSD: 0.02, ChangeName: "add-api"}, nil); err != nil {
		t.Fatal(err)
	}

	must(st.CreateExecution(apiCtx, Execution{ExecID: "api-apply", Kind: "worker", ChangeName: "add-api", Phase: "apply", Role: "build",
		EnginesRequested: "claude", PromptText: "build the API"}))
	applyCtx := WithExecution(apiCtx, "api-apply")
	apply, err := st.BeginAttempt(applyCtx, "api-apply", "claude", 0)
	must(err)
	longRunning(apply)
	must(st.BeginIdlePeriod(applyCtx, apply, time.Now().Add(-6*time.Minute), 20*time.Minute))
	must(st.EndIdlePeriod(applyCtx, apply))
	must(st.FinishAttempt(applyCtx, apply, 0, AttemptOK, 0))
	if _, err := st.EndExecution(apiCtx, "api-apply", ExecutionEnd{Status: "succeeded", EngineUsed: "claude"}); err != nil {
		t.Fatal(err)
	}

	w.failedRound, err = st.BeginGateRound(apiCtx, GateRound{Context: "worker_verify", ChangeName: "add-api", Slot: 1})
	must(err)
	must(st.RecordGateRun(WithGateRound(apiCtx, w.failedRound), GateRunRecord{ChangeName: "add-api", RepoName: "api", RepoPath: "repos/api",
		GateName: "test", Command: "go test ./...", ExitCode: 1, DurationMS: 900, FailureExcerpt: "--- FAIL: TestList"}))
	must(st.RecordGateRun(WithGateRound(apiCtx, w.failedRound), GateRunRecord{ChangeName: "add-api", RepoName: "api", RepoPath: "repos/api",
		GateName: "lint", Command: "golangci-lint run", DurationMS: 300}))
	must(st.FinishGateRound(ctx, w.failedRound, "failed"))

	must(st.CreateExecution(apiCtx, Execution{ExecID: "api-repair", Kind: "worker", ChangeName: "add-api", Phase: "repair-1", Role: "build",
		EnginesRequested: "claude", PromptText: "fix TestList"}))
	repairCtx := WithExecution(apiCtx, "api-repair")
	repair, err := st.BeginAttempt(repairCtx, "api-repair", "claude", 0)
	must(err)
	must(st.FinishAttempt(repairCtx, repair, 0, AttemptOK, 0))
	if _, err := st.EndExecution(apiCtx, "api-repair", ExecutionEnd{Status: "succeeded", EngineUsed: "claude"}); err != nil {
		t.Fatal(err)
	}
	w.passedRound, err = st.BeginGateRound(apiCtx, GateRound{Context: "worker_verify", ChangeName: "add-api", Slot: 1})
	must(err)
	must(st.RecordGateRun(WithGateRound(apiCtx, w.passedRound), GateRunRecord{ChangeName: "add-api", RepoName: "api", RepoPath: "repos/api",
		GateName: "test", Command: "go test ./...", DurationMS: 700}))
	must(st.FinishGateRound(ctx, w.passedRound, "passed"))
	must(st.RecordRepairRound(apiCtx, RepairRoundRecord{ChangeName: "add-api", Phase: "repair-1", ExecID: "api-repair",
		AskedReason: "gate_failure", AskedDetails: "--- FAIL: TestList", AskedGateRoundID: w.failedRound, ResultPassed: true}))

	w.riskEval, err = st.RecordEvaluation(apiCtx, Evaluation{Gate: "change_risk", Mode: "enforce", Status: "passed", Model: "jev-1", CostUSD: 0.05, ChangeName: "add-api"}, nil)
	must(err)
	must(st.EnqueueReady(apiCtx, ReadyManifest{ChangeName: "add-api", Branch: "feat/spec-add-api", Worktree: "/wt/worker-add-api", HeadSHA: "abc123",
		Capabilities: []string{"api", "lock:db-schema"}, Assessment: map[string]any{"evaluation_id": w.riskEval}}))
	must(st.EmitEvent(apiCtx, EventInput{Event: "change.ready", Change: "add-api"}))
	must(st.EmitEvent(apiCtx, EventInput{Event: "change.finished", Change: "add-api", Payload: map[string]any{"result": "ready"}}))
	must(st.FinishChangeRun(ctx, w.apiRun, "ready"))

	w.firstMerge, err = st.BeginMergeAttempt(ctx, "add-api")
	must(err)
	mergeCtx := WithMergeAttempt(ctx, w.firstMerge, "add-api")
	must(st.SetMergeAttemptState(ctx, w.firstMerge, "RUNNING_GATES"))
	mergeRound, err := st.BeginGateRound(mergeCtx, GateRound{Context: "merger", ChangeName: "add-api"})
	must(err)
	must(st.RecordGateRun(WithGateRound(mergeCtx, mergeRound), GateRunRecord{ChangeName: "add-api", RepoName: "api", RepoPath: "repos/api",
		GateName: "test", Command: "go test ./...", DurationMS: 650}))
	must(st.FinishGateRound(ctx, mergeRound, "passed"))
	if _, err := st.RecordEvaluation(mergeCtx, Evaluation{Gate: "merge_review", Status: "failed", Model: "jev-1", CostUSD: 0.01, ChangeName: "add-api"}, nil); err != nil {
		t.Fatal(err)
	}
	must(st.SetMergeAttemptState(ctx, w.firstMerge, "PUSHING_CHILDREN"))
	must(st.RecordChildPushed(mergeCtx, "add-api", "repos/api", "sha-api"))
	must(st.EmitEvent(mergeCtx, EventInput{Event: "merge.push_rejected", Change: "add-api", RepoPath: "repos/api"}))
	must(st.FinishMergeAttempt(ctx, w.firstMerge, MergePushRejected, "repos/api: the remote closed the connection"))
	w.secondMerge, err = st.BeginMergeAttempt(ctx, "add-api")
	must(err)

	// ---- add-search: building, with an agent that went idle.
	searchRun, err := st.BeginChangeRun(dctx, "add-search", 2, "/wt/worker-add-search", "feat/spec-add-search")
	must(err)
	searchCtx := WithChangeRun(dctx, searchRun, "add-search")
	if _, err := st.AcquireLock(ctx, WorkerLeaseKey("add-search"), LockOptions{OwnerType: "worker", PID: os.Getpid(), Slot: 2, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	must(st.CreateExecution(searchCtx, Execution{ExecID: "search-apply", Kind: "worker", ChangeName: "add-search", Phase: "apply", Role: "build",
		EnginesRequested: "claude", PID: os.Getpid()}))
	idle, err := st.BeginAttempt(WithExecution(searchCtx, "search-apply"), "search-apply", "claude", 0)
	must(err)
	longRunning(idle)
	must(st.BeginIdlePeriod(WithExecution(searchCtx, "search-apply"), idle, time.Now().Add(-6*time.Minute), 20*time.Minute))

	// ---- add-export: an open run whose lease ran out an hour ago.
	if _, err := st.BeginChangeRun(dctx, "add-export", 1, "/wt/worker-add-export", "feat/spec-add-export"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireLock(ctx, WorkerLeaseKey("add-export"), LockOptions{OwnerType: "worker", PID: os.Getpid(), Slot: 1, TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE locks SET acquired_at = acquired_at - 3600000, heartbeat_at = heartbeat_at - 3600000 WHERE lock_key = ?`,
		WorkerLeaseKey("add-export")); err != nil {
		t.Fatal(err)
	}

	// ---- add-billing: blocked by its run, then touched by a person.
	w.billingRun, err = st.BeginChangeRun(dctx, "add-billing", 2, "/wt/worker-add-billing", "feat/spec-add-billing")
	must(err)
	billingCtx := WithChangeRun(dctx, w.billingRun, "add-billing")
	must(st.RecordDeferral(billingCtx, "add-billing", "build", true, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "claude: quota"))
	must(st.BlockChange(billingCtx, QuarantineRecord{ChangeName: "add-billing", Reason: "gate_failure", Details: "coverage 91% < 100%", Stage: "verify"}))
	must(st.FinishChangeRun(ctx, w.billingRun, "blocked"))
	must(st.ClearDeferrals(ctx, "add-billing"))
	must(st.CreateExecution(ctx, Execution{ExecID: "billing-prompt", Kind: "portal_run", ChangeName: "add-billing", EnginesRequested: "claude",
		PromptText: "raise the coverage", Workdir: "/wt/worker-add-billing"}))
	handRound, err := st.BeginGateRound(ctx, GateRound{Context: "worker_verify", ChangeName: "add-billing"})
	must(err)
	must(st.RecordGateRun(WithGateRound(ctx, handRound), GateRunRecord{ChangeName: "add-billing", RepoName: "api", RepoPath: "repos/api",
		GateName: "test", Command: "go test ./...", ExitCode: 1, DurationMS: 500}))
	must(st.FinishGateRound(ctx, handRound, "failed"))
	if _, err := st.RecordEvaluation(ctx, Evaluation{Gate: "plan_review", Status: "abstained", ChangeName: "add-billing"}, nil); err != nil {
		t.Fatal(err)
	}

	// ---- add-report: a rebuild the merger asked for, and a deferral outside any run.
	if _, err := st.RequestRebuild(ctx, "add-report", "feat/spec-add-report", "CONFLICT in report.go"); err != nil {
		t.Fatal(err)
	}
	must(st.RecordDeferral(ctx, "add-report", "plan", true, time.Now().Add(-10*time.Minute), time.Now().Add(20*time.Minute), "every engine is out"))

	// ---- the workspace.
	if _, err := st.RecordBaseVerification(ctx, BaseVerification{RepoPath: "repos/core", BaseSHA: "1a2b3c4d", CheckName: "regression",
		Status: BaseVerdictFailed, ExitCode: 1, OutsideCommits: 2}); err != nil {
		t.Fatal(err)
	}
	must(st.CreateExecution(ctx, Execution{ExecID: "cycle-20261105T090000Z", Kind: "cycle", EnginesRequested: "claude", Status: "succeeded"}))
	must(st.CreateExecution(ctx, Execution{ExecID: "job-gates", Kind: "portal_job", JobKind: "gates", CommandText: "foreman-kit gates", Status: "failed",
		ExitCode: 1}))
	if _, err := st.Pause(ctx, "maintenance window", "portal", ""); err != nil {
		t.Fatal(err)
	}
	return w
}

func view(t *testing.T, st *Store, fn func(v *View) error) {
	t.Helper()
	if err := st.View(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

// TestViewShowsOneInstant: what a View reads does not change while it is open, whatever is written
// meanwhile, and a View opened afterwards sees it.
func TestViewShowsOneInstant(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if _, err := st.Pause(ctx, "first", "cli", ""); err != nil {
		t.Fatal(err)
	}
	view(t, st, func(v *View) error {
		before, err := v.ListPauses(ctx, "", 0)
		if err != nil {
			return err
		}
		if _, err := st.Resume(ctx, "cli"); err != nil {
			return err
		}
		if _, err := st.Pause(ctx, "second", "cli", ""); err != nil {
			return err
		}
		after, err := v.ListPauses(ctx, "", 0)
		if err != nil {
			return err
		}
		open, err := v.OpenPause(ctx)
		if err != nil {
			return err
		}
		if len(before.Items) != 1 || len(after.Items) != 1 || after.Items[0].ResumedAt != "" || open == nil || open.Reason != "first" {
			t.Fatalf("a view changed under its reader: before %+v, after %+v, open %+v", before.Items, after.Items, open)
		}
		if v.At().IsZero() || time.Since(v.At()) > time.Minute {
			t.Fatalf("At() = %v", v.At())
		}
		return nil
	})
	view(t, st, func(v *View) error {
		page, err := v.ListPauses(ctx, "", 0)
		if err != nil {
			return err
		}
		if len(page.Items) != 2 || page.Items[0].Reason != "second" || page.Items[0].ResumedAt != "" ||
			page.Items[1].Reason != "first" || page.Items[1].ResumedBy != "cli" {
			t.Fatalf("pauses, the latest first = %+v", page.Items)
		}
		return nil
	})
	if err := st.View(ctx, func(*View) error { return errors.New("stop") }); err == nil || err.Error() != "stop" {
		t.Fatalf("View returns the error of its function, got %v", err)
	}
}

// TestChangeOverviewsAreFilteredCountedAndPagedInSQL.
func TestChangeOverviewsAreFilteredCountedAndPagedInSQL(t *testing.T) {
	w := seedPortalWorld(t)
	ctx := context.Background()
	view(t, w.st, func(v *View) error {
		counts, err := v.CountChangesByState(ctx, "")
		if err != nil {
			return err
		}
		want := map[string]int{"queued": 1, "building": 2, "merging": 1, "blocked": 1, "rebuild_requested": 1, "integrated": 1, "ready": 0, "discarded": 0, "unknown": 0}
		for state, n := range want {
			if counts[state] != n {
				t.Errorf("changes in state %s = %d, want %d (all: %v)", state, counts[state], n, counts)
			}
		}
		if len(counts) != len(ChangeStates) {
			t.Errorf("the counts name %d states, want the %d of ChangeStates: %v", len(counts), len(ChangeStates), counts)
		}

		all, err := v.ListChangeOverviews(ctx, ChangeFilter{Per: 4})
		if err != nil {
			return err
		}
		if all.Total != 7 || all.Pages != 2 || all.Page != 1 || len(all.Items) != 4 {
			t.Fatalf("the first page of every change: total %d, pages %d, page %d, %d items", all.Total, all.Pages, all.Page, len(all.Items))
		}
		rest, err := v.ListChangeOverviews(ctx, ChangeFilter{Per: 4, Page: 2})
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		last := ""
		for _, c := range append(all.Items, rest.Items...) {
			if seen[c.ChangeName] {
				t.Errorf("%s is on two pages", c.ChangeName)
			}
			seen[c.ChangeName] = true
			if last != "" && c.LastActivityAt > last {
				t.Errorf("%s (%s) comes after a change with an earlier activity (%s)", c.ChangeName, c.LastActivityAt, last)
			}
			last = c.LastActivityAt
		}
		if len(seen) != 7 {
			t.Errorf("two pages of four hold %d changes, want 7", len(seen))
		}
		// A page past the end is the last page.
		if far, _ := v.ListChangeOverviews(ctx, ChangeFilter{Per: 4, Page: 9}); far.Page != 2 || len(far.Items) != 3 {
			t.Errorf("a page past the end: page %d with %d items", far.Page, len(far.Items))
		}

		building, err := v.ListChangeOverviews(ctx, ChangeFilter{State: ChangeBuilding})
		if err != nil {
			return err
		}
		if building.Total != 2 || len(building.Items) != 2 {
			t.Fatalf("changes building = %+v", building)
		}
		blocked, err := v.ListChangeOverviews(ctx, ChangeFilter{State: ChangeBlocked})
		if err != nil {
			return err
		}
		if len(blocked.Items) != 1 {
			t.Fatalf("blocked changes = %+v", blocked.Items)
		}
		if b := blocked.Items[0]; b.ChangeName != "add-billing" || b.OpenBlockReason != "gate_failure" || b.LastRunResult != "blocked" ||
			b.Runs != 1 || b.Executions != 1 || b.OpenDeferrals != 0 || b.Priority != "normal" || b.BacklogStatus != "queued" || b.QueuedAt != "2026-11-03T11:00:00Z" {
			t.Errorf("the blocked change = %+v", b)
		}
		api, err := v.ListChangeOverviews(ctx, ChangeFilter{Query: "the api"})
		if err != nil {
			return err
		}
		if len(api.Items) != 1 || api.Items[0].ChangeName != "add-api" || api.Items[0].State != ChangeMerging ||
			api.Items[0].Runs != 1 || api.Items[0].Executions != 3 || api.Items[0].Repairs != 1 || api.Items[0].LastRunResult != "ready" {
			t.Errorf("the search for 'the api' = %+v", api.Items)
		}
		// The wildcards of a search are taken literally.
		if pct, _ := v.ListChangeOverviews(ctx, ChangeFilter{Query: "100%"}); len(pct.Items) != 1 || pct.Items[0].ChangeName != "add-billing" {
			t.Errorf("the search for '100%%' = %+v", pct.Items)
		}
		if under, _ := v.ListChangeOverviews(ctx, ChangeFilter{Query: "coverage_"}); len(under.Items) != 1 {
			t.Errorf("the search for 'coverage_' = %+v", under.Items)
		}
		if none, _ := v.ListChangeOverviews(ctx, ChangeFilter{Query: "a%i", Page: 3}); none.Total != 0 || len(none.Items) != 0 || none.Page != 1 || none.Pages != 0 {
			t.Errorf("the search for 'a%%i' matched %+v", none.Items)
		}
		searched, err := v.CountChangesByState(ctx, "search")
		if err != nil {
			return err
		}
		if searched[ChangeBuilding] != 1 || searched[ChangeQueued] != 1 || searched[ChangeIntegrated] != 0 {
			t.Errorf("the counts of the search for 'search' = %v", searched)
		}
		integrated, _ := v.ListChangeOverviews(ctx, ChangeFilter{State: ChangeIntegrated})
		if len(integrated.Items) != 1 || integrated.Items[0].IntegratedCommit != "c0ffee" || integrated.Items[0].IntegratedAt != "2026-11-01T10:00:00Z" {
			t.Errorf("the integrated change = %+v", integrated.Items)
		}
		return nil
	})
}

// TestChangeStoryIsMadeOfRowsNotEvents: the story of a change is its runs, its manifests, its merge
// attempts, its blocks and what each carried, the latest first and paged by cursor.
func TestChangeStoryIsMadeOfRowsNotEvents(t *testing.T) {
	w := seedPortalWorld(t)
	ctx := context.Background()
	kinds := func(p StoryPage) string {
		var out []string
		for _, e := range p.Episodes {
			out = append(out, e.Kind)
		}
		return strings.Join(out, ",")
	}
	view(t, w.st, func(v *View) error {
		story, err := v.ChangeStory(ctx, "add-api", "", 0)
		if err != nil {
			return err
		}
		if got := kinds(story); got != "merge,merge,manifest,run" || story.Next != "" {
			t.Fatalf("the story of add-api = %s (next %q), want merge,merge,manifest,run", got, story.Next)
		}
		current, rejected, manifest, run := story.Episodes[0].Merge, story.Episodes[1].Merge, story.Episodes[2].Manifest, story.Episodes[3].Run
		if current.ID != w.secondMerge || current.Result != "" || current.FinishedAt != "" || current.ResumedFromID != w.firstMerge {
			t.Errorf("the merge attempt in progress = %+v", current.MergeAttempt)
		}
		if rejected.ID != w.firstMerge || rejected.Result != MergePushRejected || !strings.Contains(rejected.ResultReason, "closed the connection") ||
			len(rejected.GateRounds) != 1 || rejected.GateRounds[0].Context != "merger" || len(rejected.GateRounds[0].Runs) != 1 ||
			len(rejected.Evaluations) != 1 || rejected.Evaluations[0].Gate != "merge_review" ||
			len(rejected.Pushes) != 1 || rejected.Pushes[0].RepoPath != "repos/api" || rejected.Pushes[0].PushedSHA != "sha-api" {
			t.Errorf("the rejected merge attempt = %+v", rejected)
		}
		if manifest.HeadSHA != "abc123" || manifest.ChangeRunID != w.apiRun || manifest.RiskEvaluationID != w.riskEval ||
			strings.Join(manifest.Capabilities, ",") != "api,lock:db-schema" || manifest.ClosedAt != "" {
			t.Errorf("the manifest = %+v", manifest)
		}
		if run.ID != w.apiRun || run.Result != "ready" || run.Origin != "backlog" || run.DispatcherRunID != w.dispatch {
			t.Errorf("the run = %+v", run.ChangeRun)
		}
		var phases []string
		for _, e := range run.Executions {
			phases = append(phases, e.Phase)
		}
		if strings.Join(phases, ",") != "plan,apply,repair-1" {
			t.Fatalf("the executions of the run = %v, want plan, apply, repair-1 in that order", phases)
		}
		plan, apply := run.Executions[0], run.Executions[1]
		if len(plan.Attempts) != 2 || plan.Attempts[0].Engine != "claude" || plan.Attempts[0].Outcome != AttemptQuotaExceeded ||
			plan.Attempts[1].Engine != "opencode" || plan.Attempts[1].Outcome != AttemptOK || plan.EngineUsed != "opencode" {
			t.Errorf("the attempts of the plan = %+v", plan.Attempts)
		}
		if len(plan.PromptText) != 400 {
			t.Errorf("a story carries the beginning of a prompt, got %d bytes", len(plan.PromptText))
		}
		if len(apply.Attempts) != 1 || len(apply.Attempts[0].IdlePeriods) != 1 || apply.Attempts[0].IdlePeriods[0].EndedBy != IdleEndedByActivity ||
			apply.Attempts[0].IdlePeriods[0].Seconds < 355 || apply.Attempts[0].IdlePeriods[0].LimitSeconds != 1200 {
			t.Errorf("the idle period of the apply = %+v", apply.Attempts)
		}
		if len(run.GateRounds) != 2 || run.GateRounds[0].ID != w.failedRound || run.GateRounds[0].Result != "failed" || len(run.GateRounds[0].Runs) != 2 ||
			run.GateRounds[0].Runs[0].GateName != "test" || run.GateRounds[0].Runs[0].FailureExcerpt != "--- FAIL: TestList" ||
			run.GateRounds[1].ID != w.passedRound || run.GateRounds[1].Result != "passed" || run.GateRounds[1].AfterExecID != "api-repair" {
			t.Errorf("the gate rounds of the run = %+v", run.GateRounds)
		}
		if len(run.Repairs) != 1 || run.Repairs[0].ExecID != "api-repair" || run.Repairs[0].AskedGateRoundID != w.failedRound ||
			run.Repairs[0].ResultGateRoundID != w.passedRound || !run.Repairs[0].ResultPassed || run.Repairs[0].Engine != "claude" {
			t.Errorf("the repairs of the run = %+v", run.Repairs)
		}
		if len(run.Evaluations) != 2 || run.Evaluations[0].Gate != "plan_review" || run.Evaluations[1].Gate != "change_risk" || run.Evaluations[1].ID != w.riskEval {
			t.Errorf("the evaluations of the run = %+v", run.Evaluations)
		}

		// Two episodes a page: the cursor continues where the page ended, with no episode twice.
		first, err := v.ChangeStory(ctx, "add-api", "", 2)
		if err != nil {
			return err
		}
		if kinds(first) != "merge,merge" || first.Next == "" {
			t.Fatalf("the first page of two = %s (next %q)", kinds(first), first.Next)
		}
		second, err := v.ChangeStory(ctx, "add-api", first.Next, 2)
		if err != nil {
			return err
		}
		if kinds(second) != "manifest,run" || second.Next != "" {
			t.Fatalf("the second page of two = %s (next %q)", kinds(second), second.Next)
		}
		if _, err := v.ChangeStory(ctx, "add-api", "not-a-cursor", 2); !errors.Is(err, ErrBadCursor) {
			t.Errorf("a cursor nobody handed out = %v, want ErrBadCursor", err)
		}

		// What belongs to no run and no merge attempt is an episode of its own.
		billing, err := v.ChangeStory(ctx, "add-billing", "", 0)
		if err != nil {
			return err
		}
		if got := kinds(billing); got != "evaluation,gates,execution,block,run" {
			t.Fatalf("the story of add-billing = %s", got)
		}
		eval, gates, prompt, block, brun := billing.Episodes[0].Evaluation, billing.Episodes[1].Gates, billing.Episodes[2].Execution, billing.Episodes[3].Block, billing.Episodes[4].Run
		if eval.Gate != "plan_review" || eval.Status != "abstained" || eval.ChangeRunID != 0 {
			t.Errorf("the evaluation outside a run = %+v", eval)
		}
		if gates.Context != "cli" || gates.Result != "failed" || len(gates.Runs) != 1 || gates.Runs[0].ExitCode != 1 {
			t.Errorf("the gates run by hand = %+v", gates)
		}
		if prompt.ExecID != "billing-prompt" || prompt.Kind != "portal_run" || prompt.PromptText != "raise the coverage" || prompt.ChangeRunID != 0 {
			t.Errorf("the prompt sent from the portal = %+v", prompt.Execution)
		}
		if block.Reason != "gate_failure" || block.Details != "coverage 91% < 100%" || block.Stage != "verify" || block.ChangeRunID != w.billingRun ||
			block.ResolvedAt != "" || block.RetryNum != 0 {
			t.Errorf("the block = %+v", block)
		}
		if brun.Result != "blocked" || len(brun.Deferrals) != 1 || brun.Deferrals[0].Role != "build" || brun.Deferrals[0].OutputTail != "claude: quota" ||
			brun.Deferrals[0].ClearedAt == "" || len(brun.Executions) != 0 {
			t.Errorf("the run that ended blocked = %+v", brun)
		}

		report, err := v.ChangeStory(ctx, "add-report", "", 0)
		if err != nil {
			return err
		}
		if got := kinds(report); got != "rebuild,deferral" && got != "deferral,rebuild" {
			t.Fatalf("the story of add-report = %s", got)
		}
		for _, e := range report.Episodes {
			switch e.Kind {
			case EpisodeRebuild:
				if e.Rebuild.ConflictDetails != "CONFLICT in report.go" || e.Rebuild.RebuildNum != 1 || e.Rebuild.ConsumedAt != "" {
					t.Errorf("the rebuild request = %+v", e.Rebuild)
				}
			case EpisodeDeferral:
				if e.Deferral.Role != "plan" || e.Deferral.ChangeRunID != 0 || e.Deferral.RetryAt == "" || e.Deferral.ClearedAt != "" {
					t.Errorf("the deferral outside a run = %+v", e.Deferral)
				}
			}
		}
		if none, err := v.ChangeStory(ctx, "no-such-change", "", 0); err != nil || len(none.Episodes) != 0 || none.Next != "" {
			t.Errorf("the story of a change nobody met = %+v, %v", none, err)
		}
		return nil
	})
}

// TestExecutionsAndEventsPageByCursor.
func TestExecutionsAndEventsPageByCursor(t *testing.T) {
	w := seedPortalWorld(t)
	ctx := context.Background()
	ids := func(p ExecutionPage) string {
		var out []string
		for _, e := range p.Items {
			out = append(out, e.ExecID)
		}
		return strings.Join(out, ",")
	}
	view(t, w.st, func(v *View) error {
		all, err := v.ListExecutionsPage(ctx, ExecutionFilter{})
		if err != nil {
			return err
		}
		const want = "job-gates,cycle-20261105T090000Z,billing-prompt,search-apply,api-repair,api-apply,api-plan"
		if ids(all) != want || all.Next != "" {
			t.Fatalf("every execution, the latest first = %s (next %q)", ids(all), all.Next)
		}
		plan, search := all.Items[6], all.Items[3]
		if len(plan.Attempts) != 2 || plan.Attempts[1].Engine != "opencode" || len(search.Attempts) != 1 ||
			len(search.Attempts[0].IdlePeriods) != 1 || search.Attempts[0].IdlePeriods[0].EndedAt != "" {
			t.Errorf("the attempts of the list: plan %+v, search %+v", plan.Attempts, search.Attempts)
		}
		var walked []string
		cursor := ""
		for range 10 {
			page, err := v.ListExecutionsPage(ctx, ExecutionFilter{Before: cursor, Limit: 3})
			if err != nil {
				return err
			}
			walked = append(walked, ids(page))
			if cursor = page.Next; cursor == "" {
				break
			}
		}
		if strings.Join(walked, ",") != want || len(walked) != 3 {
			t.Errorf("three executions a page = %v, want %s in three pages", walked, want)
		}
		for name, f := range map[string]ExecutionFilter{
			"billing-prompt":                {Kind: "portal_run"},
			"job-gates":                     {Kind: "portal_job"},
			"search-apply":                  {Status: "running", Kind: "worker"},
			"api-plan":                      {Engine: "opencode"},
			"api-repair,api-apply,api-plan": {Change: "add-api"},
			"cycle-20261105T090000Z":        {Cycle: "20261105T090000Z"},
			"":                              {Change: "add-api", Status: "failed"},
			// An engine worked on an execution it made an attempt of: the prompt nobody took yet is not one.
			"search-apply,api-repair,api-apply,api-plan": {Engine: "claude"},
		} {
			page, err := v.ListExecutionsPage(ctx, f)
			if err != nil {
				return err
			}
			if ids(page) != name {
				t.Errorf("executions for %+v = %s, want %s", f, ids(page), name)
			}
		}
		if _, err := v.ListExecutionsPage(ctx, ExecutionFilter{Before: "x.y"}); !errors.Is(err, ErrBadCursor) {
			t.Errorf("a cursor nobody handed out = %v, want ErrBadCursor", err)
		}

		// Events: every page is the events below the cursor, and the counts are over every event.
		var total int
		if err := w.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&total); err != nil {
			return err
		}
		var seen []int64
		cursor = ""
		for range 100 {
			page, err := v.ListEventsPage(ctx, EventPageFilter{Before: cursor, Limit: 4})
			if err != nil {
				return err
			}
			for _, e := range page.Items {
				if len(seen) > 0 && e.ID >= seen[len(seen)-1] {
					t.Fatalf("event %d comes after event %d", e.ID, seen[len(seen)-1])
				}
				seen = append(seen, e.ID)
			}
			if cursor = page.Next; cursor == "" {
				break
			}
		}
		if len(seen) != total || total < 8 {
			t.Errorf("paging walked %d events of %d", len(seen), total)
		}
		counts, err := v.CountEventsByType(ctx, EventPageFilter{Event: "agent.idle", Limit: 1})
		if err != nil {
			return err
		}
		sum := 0
		byType := map[string]int{}
		for _, c := range counts {
			sum += c.Count
			byType[c.Event] = c.Count
		}
		if sum != total || byType["agent.idle"] != 2 || byType["system.paused"] != 1 || byType["change.start"] != 1 {
			t.Errorf("the counts by type add up to %d of %d: %v", sum, total, byType)
		}
		idle, err := v.ListEventsPage(ctx, EventPageFilter{Event: "agent.idle", Change: "add-search"})
		if err != nil {
			return err
		}
		if len(idle.Items) != 1 || idle.Items[0].ExecID != "search-apply" || idle.Items[0].Engine != "claude" {
			t.Errorf("agent.idle of add-search = %+v", idle.Items)
		}
		ofExec, _ := v.ListEventsPage(ctx, EventPageFilter{ExecID: "api-apply"})
		ofRun, _ := v.ListEventsPage(ctx, EventPageFilter{ChangeRunID: w.apiRun})
		ofMerge, _ := v.ListEventsPage(ctx, EventPageFilter{MergeAttemptID: w.firstMerge})
		ofChange, _ := v.CountEventsByType(ctx, EventPageFilter{Change: "add-api"})
		if len(ofExec.Items) != 2 || len(ofRun.Items) != 5 || len(ofChange) != 6 ||
			len(ofMerge.Items) != 1 || ofMerge.Items[0].Event != "merge.push_rejected" || ofMerge.Items[0].RepoPath != "repos/api" {
			t.Errorf("events of the execution %d, of its run %d, of the merge attempt %+v, types of its change %+v", len(ofExec.Items), len(ofRun.Items), ofMerge.Items, ofChange)
		}
		for _, e := range ofExec.Items {
			if e.ExecID != "api-apply" {
				t.Errorf("an event of another execution: %+v", e)
			}
		}
		if _, err := v.ListEventsPage(ctx, EventPageFilter{Before: "-3"}); !errors.Is(err, ErrBadCursor) {
			t.Errorf("a cursor nobody handed out = %v, want ErrBadCursor", err)
		}
		return nil
	})
}

// TestInboxBacklogAndMergeQueueComeFromOpenRows.
func TestInboxBacklogAndMergeQueueComeFromOpenRows(t *testing.T) {
	w := seedPortalWorld(t)
	ctx := context.Background()
	view(t, w.st, func(v *View) error {
		lost, err := v.ListLostRuns(ctx)
		if err != nil {
			return err
		}
		if len(lost) != 1 || lost[0].ChangeName != "add-export" || lost[0].Lease == nil || lost[0].LeaseExpiredAt == "" || lost[0].Lease.TTLSeconds != 60 {
			t.Fatalf("lost runs = %+v, want the run of add-export alone: add-search holds its lease", lost)
		}

		in, err := v.ListInbox(ctx)
		if err != nil {
			return err
		}
		if in.Count() != 6 {
			t.Errorf("the inbox counts %d, want 6: a block, a broken base, two proposals, a lost run and the pause", in.Count())
		}
		if len(in.Blocked) != 1 || in.Blocked[0].ChangeName != "add-billing" || in.Blocked[0].Reason != "gate_failure" ||
			in.Blocked[0].ChangeRunID != w.billingRun || in.Blocked[0].Quarantine.Status != "blocked" || in.Blocked[0].Quarantine.Retries != 0 {
			t.Errorf("blocked = %+v", in.Blocked)
		}
		if len(in.BrokenBases) != 1 || in.BrokenBases[0].Repo != "repos/core" || in.BrokenBases[0].OutsideCommits != 2 {
			t.Errorf("broken bases = %+v", in.BrokenBases)
		}
		if len(in.Proposals) != 2 || in.Proposals[0].ID != "add-cache" || in.Proposals[0].Priority != "high" || in.Proposals[0].Source != "exploration" ||
			in.Proposals[1].ID != "add-feed" || !strings.Contains(in.Proposals[1].ReviewJSON, "well_scoped") {
			t.Errorf("proposals nobody decided on = %+v", in.Proposals)
		}
		if len(in.LostRuns) != 1 || in.Pause == nil || in.Pause.Reason != "maintenance window" || in.Pause.PausedBy != "portal" {
			t.Errorf("lost runs %+v, pause %+v", in.LostRuns, in.Pause)
		}
		rejected, err := v.ListProposals(ctx, "rejected", "", 0)
		if err != nil {
			return err
		}
		if len(rejected.Items) != 1 || rejected.Items[0].ID != "add-old" || rejected.Next != "" {
			t.Errorf("rejected proposals = %+v", rejected)
		}
		firstProposed, _ := v.ListProposals(ctx, "proposed", "", 1)
		nextProposed, _ := v.ListProposals(ctx, "proposed", firstProposed.Next, 1)
		if len(firstProposed.Items) != 1 || firstProposed.Items[0].ID != "add-feed" || len(nextProposed.Items) != 1 || nextProposed.Items[0].ID != "add-cache" || nextProposed.Next != "" {
			t.Errorf("proposals, one a page, the latest first = %+v then %+v", firstProposed, nextProposed)
		}

		backlog, err := v.ListWaitFacts(ctx)
		if err != nil {
			return err
		}
		var order []string
		facts := map[string]string{}
		for _, c := range backlog {
			order = append(order, c.ChangeName)
			var fs []string
			for _, f := range c.Facts {
				fs = append(fs, strings.TrimRight(strings.Join([]string{f.Kind, f.OtherChange, f.OtherState, f.LockName, f.Reason}, ":"), ":"))
			}
			facts[c.ChangeName] = strings.Join(fs, " ")
		}
		if strings.Join(order, ",") != "add-search,add-ui,add-api,add-billing,add-report,add-export" {
			t.Errorf("the backlog, by priority and then by age = %v", order)
		}
		for change, want := range map[string]string{
			"add-ui":      "dependency:add-api:merging lock:add-api:merging:db-schema",
			"add-api":     "",
			"add-search":  "",
			"add-billing": "block::::gate_failure",
			"add-report":  "deferral::::plan dependency:add-export:building",
			"add-export":  "",
		} {
			if facts[change] != want {
				t.Errorf("what holds %s back = %q, want %q", change, facts[change], want)
			}
		}
		for _, c := range backlog {
			if c.ChangeName == "add-report" {
				if c.State != ChangeRebuildRequested || c.Facts[0].Since == "" || c.Facts[0].Until == "" || c.OpenDeferrals != 1 {
					t.Errorf("add-report = %+v", c)
				}
			}
		}

		mq, err := v.MergeQueue(ctx, "", 0)
		if err != nil {
			return err
		}
		if mq.Current == nil || mq.Current.ID != w.secondMerge || mq.Current.ChangeName != "add-api" || mq.Current.State != "CLAIMED" {
			t.Errorf("the attempt in progress = %+v", mq.Current)
		}
		if len(mq.Waiting) != 1 {
			t.Fatalf("manifests waiting = %+v", mq.Waiting)
		}
		m := mq.Waiting[0]
		if m.ChangeName != "add-api" || strings.Join(m.Capabilities, ",") != "api,lock:db-schema" || m.Risk == nil || m.Risk.Gate != "change_risk" ||
			m.Attempts != 2 || m.Last == nil || m.Last.ID != w.secondMerge || len(m.Pushes) != 1 || m.Pushes[0].PushedSHA != "sha-api" {
			t.Errorf("the manifest waiting = %+v", m)
		}
		if len(mq.Recent) != 1 || mq.Recent[0].ID != w.firstMerge || mq.Recent[0].Result != MergePushRejected || mq.Next != "" {
			t.Errorf("the attempts that ended = %+v", mq.Recent)
		}
		if len(mq.BrokenBases) != 1 {
			t.Errorf("broken bases of the merge queue = %+v", mq.BrokenBases)
		}

		idle, err := v.ListIdleAgents(ctx)
		if err != nil {
			return err
		}
		if len(idle) != 1 || idle[0].ExecID != "search-apply" || idle[0].ChangeName != "add-search" || idle[0].IdleSeconds < 355 || idle[0].LimitSeconds != 1200 {
			t.Errorf("idle agents = %+v", idle)
		}
		return nil
	})

	// An agent whose orchestrator is gone is not idle: nothing watches it. The filter is the store's.
	if _, err := w.st.DB().ExecContext(ctx, `UPDATE executions SET pid = 2147483646 WHERE exec_id = 'search-apply'`); err != nil {
		t.Fatal(err)
	}
	if idle, err := w.st.ListIdleAgents(ctx); err != nil || len(idle) != 0 {
		t.Fatalf("idle agents of a dead orchestrator = %+v, %v", idle, err)
	}
}

// TestAggregatesAreResolvedInSQLSinceADate.
func TestAggregatesAreResolvedInSQLSinceADate(t *testing.T) {
	w := seedPortalWorld(t)
	ctx := context.Background()
	view(t, w.st, func(v *View) error {
		gates, err := v.GateStats(ctx, time.Time{})
		if err != nil {
			return err
		}
		got := map[string]GateStat{}
		for _, g := range gates {
			got[g.RepoName+"/"+g.GateName] = g
		}
		if test := got["api/test"]; len(gates) != 2 || test.Runs != 4 || test.Failures != 2 || test.MaxMS != 900 || test.TotalMS != 2750 || test.AvgMS != 688 ||
			test.RepoPath != "repos/api" || test.LastRunAt == "" || test.LastFailedAt == "" || test.Changes != 0 {
			t.Errorf("gate api/test = %+v (all: %+v)", test, gates)
		}
		// It failed in two rounds, and the round to open is the later one.
		if test := got["api/test"]; test.LastFailedRoundID <= w.failedRound {
			t.Errorf("the round gate api/test last failed in = %d, want one after %d", test.LastFailedRoundID, w.failedRound)
		}
		if lint := got["api/lint"]; lint.Runs != 1 || lint.Failures != 0 || lint.LastFailedAt != "" || lint.LastFailedRoundID != 0 {
			t.Errorf("gate api/lint = %+v", lint)
		}
		if later, _ := v.GateStats(ctx, time.Now().Add(time.Hour)); len(later) != 0 {
			t.Errorf("gate runs since an hour from now = %+v", later)
		}

		evals, err := v.EvaluationStats(ctx, time.Time{})
		if err != nil {
			return err
		}
		byGate := map[string]EvaluationStat{}
		for _, e := range evals {
			byGate[e.Gate] = e
		}
		if pr := byGate["plan_review"]; len(evals) != 3 || pr.Evaluations != 2 || pr.Passed != 1 || pr.ByStatus["abstained"] != 1 || pr.CostUSD != 0.02 || pr.LastAt == "" ||
			pr.ByMode["enforce"] != 1 || len(pr.ByRoute) != 0 {
			t.Errorf("evaluations of plan_review = %+v (all: %+v)", pr, evals)
		}
		if mr := byGate["merge_review"]; mr.Evaluations != 1 || mr.Passed != 0 || mr.ByStatus["failed"] != 1 {
			t.Errorf("evaluations of merge_review = %+v", mr)
		}

		engines, err := v.EngineStats(ctx, time.Time{})
		if err != nil {
			return err
		}
		byEngine := map[string]EngineStat{}
		for _, e := range engines {
			byEngine[e.Engine] = e
		}
		claude, opencode := byEngine["claude"], byEngine["opencode"]
		if len(engines) != 2 || claude.Attempts != 4 || claude.Running != 1 || claude.ByOutcome[AttemptOK] != 2 || claude.ByOutcome[AttemptQuotaExceeded] != 1 ||
			claude.IdlePeriods != 2 || claude.Stalls != 0 || claude.LongestRecoveredIdleSeconds < 355 || claude.LastUsedAt == "" {
			t.Errorf("engine claude = %+v (all: %+v)", claude, engines)
		}
		if opencode.Attempts != 1 || opencode.ByOutcome[AttemptOK] != 1 || opencode.IdlePeriods != 0 {
			t.Errorf("engine opencode = %+v", opencode)
		}

		dispatches, err := v.ListDispatcherRuns(ctx, "", 0)
		if err != nil {
			return err
		}
		if len(dispatches.Items) != 1 || dispatches.Items[0].ID != w.dispatch || dispatches.Items[0].ChangeRuns != 4 || dispatches.Items[0].Workers != 2 ||
			dispatches.Items[0].Results["ready"] != 1 || dispatches.Items[0].Results["blocked"] != 1 || dispatches.Items[0].Results[""] != 2 {
			t.Errorf("dispatcher runs = %+v", dispatches.Items)
		}
		return nil
	})
}
