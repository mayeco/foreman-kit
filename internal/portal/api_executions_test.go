package portal

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// errorOf returns the code and the message of an error of the API: {"error": {"code", "message"}}.
func errorOf(body map[string]any) (code, message string) {
	e, _ := body["error"].(map[string]any)
	return asStr(e["code"]), asStr(e["message"])
}

// startedOf reads the answer of an action that launched an execution.
func startedOf(t *testing.T, code int, body map[string]any, raw string) (id string, ex map[string]any) {
	t.Helper()
	ex, _ = body["execution"].(map[string]any)
	id = asStr(body["exec_id"])
	if code != http.StatusAccepted || id == "" || asStr(body["href"]) != "/executions/"+url.PathEscape(id) || asStr(ex["exec_id"]) != id {
		t.Fatalf("an action that launches an execution answers 202 with its id, its page and its row; got %d: %s", code, raw)
	}
	return id, ex
}

// TestTheActionsOfAnExecution: each of the four actions does what it says, refuses what it cannot
// read (400), says what does not exist (404) and what its state does not allow (409).
func TestTheActionsOfAnExecution(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	ctx := context.Background()
	blocked := make(chan struct{})
	kitOf(srv).agent = func(ctx context.Context, _, engine, prompt string) (string, int, error) {
		if strings.Contains(prompt, "wait to be stopped") {
			blocked <- struct{}{}
			<-ctx.Done()
			return "", 143, ctx.Err()
		}
		return "answer of " + engine + " to: " + prompt, 0, nil
	}

	// POST /api/executions
	code, body, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions", map[string]any{"engine": "claude", "prompt": "Check the status"})
	runID, ex := startedOf(t, code, body, raw)
	if ex["kind"] != "portal_run" || ex["prompt_text"] != "Check the status" {
		t.Fatalf("the execution POST /api/executions started = %v", ex)
	}
	waitExecution(t, st, runID)
	waitWork(t, srv.exec, runID)
	for name, payload := range map[string]any{
		"an empty prompt":          map[string]any{"engine": "claude", "prompt": ""},
		"an engine nobody knows":   map[string]any{"engine": "no-such-engine", "prompt": "x"},
		"a field it does not take": map[string]any{"engine": "claude", "prompt": "x", "workdir": "/etc"},
		"a directory to run in":    map[string]any{"engine": "claude", "prompt": "x", "change": "feat-alpha"},
		"a value of another type":  map[string]any{"engine": "claude", "prompt": 12},
	} {
		code, body, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions", payload)
		if c, msg := errorOf(body); code != http.StatusBadRequest || c != "invalid_input" || msg == "" {
			t.Errorf("POST /api/executions with %s = %d %s; want 400 invalid_input with a message", name, code, raw)
		}
	}

	// POST /api/executions/{id}/continue
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/executions/"+runID+"/continue", map[string]any{"prompt": "and then?", "with_context": true})
	contID, ex := startedOf(t, code, body, raw)
	if ex["parent_exec_id"] != runID || ex["parent_relation"] != "continue" || !strings.Contains(asStr(ex["prompt_text"]), "answer of claude to: Check the status") {
		t.Fatalf("the follow-up = %v", ex)
	}
	waitExecution(t, st, contID)
	if code, body, _, _ := doReq(t, srv, http.MethodPost, "/api/executions/"+runID+"/continue", map[string]any{"prompt": " "}); code != http.StatusBadRequest {
		t.Errorf("an empty follow-up = %d %v, want 400", code, body)
	}
	if code, body, _, _ := doReq(t, srv, http.MethodPost, "/api/executions/no-such-execution/continue", map[string]any{"prompt": "x"}); code != http.StatusNotFound {
		t.Errorf("a follow-up to nothing = %d %v, want 404", code, body)
	}

	// POST /api/executions/{id}/rerun
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/executions/"+runID+"/rerun", map[string]any{})
	againID, ex := startedOf(t, code, body, raw)
	if ex["parent_exec_id"] != runID || ex["parent_relation"] != "rerun" || ex["prompt_text"] != "Check the status" {
		t.Fatalf("the rerun = %v", ex)
	}
	waitExecution(t, st, againID)
	if code, _, _, _ := doReq(t, srv, http.MethodPost, "/api/executions/no-such-execution/rerun", map[string]any{}); code != http.StatusNotFound {
		t.Errorf("a rerun of nothing = %d, want 404", code)
	}
	if code, _, _, _ := doReq(t, srv, http.MethodPost, "/api/executions/"+runID+"/rerun", map[string]any{"prompt": "another one"}); code != http.StatusBadRequest {
		t.Errorf("a rerun with a prompt of its own = %d, want 400: it repeats the run", code)
	}

	// An operation of the kit is neither continued nor repeated from here.
	op, err := srv.exec.StartOperation(ctx, "dispatch", "")
	if err != nil {
		t.Fatal(err)
	}
	waitExecution(t, st, op.ExecID)
	if code, body, _, _ := doReq(t, srv, http.MethodPost, "/api/executions/"+op.ExecID+"/continue", map[string]any{"prompt": "x"}); code != http.StatusConflict {
		t.Errorf("a follow-up to an operation of the kit = %d %v, want 409", code, body)
	}
	if code, body, _, _ := doReq(t, srv, http.MethodPost, "/api/executions/"+op.ExecID+"/rerun", map[string]any{}); code != http.StatusConflict {
		t.Errorf("a rerun of an operation of the kit = %d %v, want 409", code, body)
	}

	// POST /api/executions/{id}/stop
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/executions", map[string]any{"engine": "claude", "prompt": "wait to be stopped"})
	longID, _ := startedOf(t, code, body, raw)
	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent never started")
	}
	started := time.Now()
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/executions/"+longID+"/stop", map[string]any{})
	if code != http.StatusOK || body["exec_id"] != longID || body["status"] != "stopped" {
		t.Fatalf("POST stop = %d %s; want 200 with the execution as it is recorded, stopped", code, raw)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("the stop took %v", elapsed)
	}
	if got, _ := st.GetExecution(ctx, longID); got == nil || got.Status != "stopped" || got.ExitCode != stoppedExitCode {
		t.Fatalf("the stopped execution is recorded as %+v", got)
	}
	code, body, _, _ = doReq(t, srv, http.MethodPost, "/api/executions/"+longID+"/stop", map[string]any{})
	if c, msg := errorOf(body); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, "not running") {
		t.Errorf("stopping what is stopped = %d %v, want 409 conflict", code, body)
	}
	if code, body, _, _ := doReq(t, srv, http.MethodPost, "/api/executions/no-such-execution/stop", map[string]any{}); code != http.StatusNotFound {
		t.Errorf("stopping nothing = %d %v, want 404", code, body)
	}
	if code, _, _, _ := doReq(t, srv, http.MethodPost, "/api/executions/"+longID+"/stop", map[string]any{"force": true}); code != http.StatusBadRequest {
		t.Errorf("a stop with a field it does not take = %d, want 400", code)
	}

	// An action takes its input from a JSON body that says it is one, and from nowhere else.
	for name, send := range map[string]func(*http.Request){
		"a body with no content type": func(r *http.Request) { r.Header.Del("Content-Type") },
		"a body declared as text":     func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
	} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4567/api/executions", strings.NewReader(`{"engine":"claude","prompt":"x"}`))
		req.Host, req.RemoteAddr = "127.0.0.1:4567", "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		send(req)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden {
			t.Errorf("%s started an execution: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if runs, _ := st.ListExecutions(ctx, "portal_run", "", 50); len(runs) != 4 {
		t.Errorf("%d agent runs on record, want the 4 that were started: a refused request starts nothing", len(runs))
	}

	// A method an address does not take.
	if code, _, _, hdr := doReq(t, srv, http.MethodGet, "/api/executions/"+runID+"/stop", nil); code != http.StatusMethodNotAllowed || hdr.Get("Allow") != "POST" {
		t.Errorf("GET on an action = %d (Allow %q), want 405 with Allow: POST", code, hdr.Get("Allow"))
	}
	if code, _, _, hdr := doReq(t, srv, http.MethodDelete, "/api/executions/"+runID, nil); code != http.StatusMethodNotAllowed || hdr.Get("Allow") != "GET" {
		t.Errorf("DELETE on an execution = %d (Allow %q), want 405 with Allow: GET", code, hdr.Get("Allow"))
	}
}

// seedExecutions records executions of every kind: a worker run with a PLAN, a BUILD stopped by the
// idle watchdog after a change of engine, and a repair verified by a round of gates; a cycle; an
// agent run with a follow-up; an operation of the kit; and a BUILD that runs now with an idle agent.
func seedExecutions(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	const change = "add-api"
	runID, err := st.BeginChangeRun(ctx, change, 1, "/tmp/worker-add-api", "feat/spec-add-api")
	must(err)
	run := store.WithChangeRun(ctx, runID, change)

	must(st.CreateExecution(run, store.Execution{ExecID: "plan-1", Kind: "worker", ChangeName: change, Phase: "plan", Role: "plan",
		EnginesRequested: "claude", PromptText: "# PLAN\n\nWrite the proposal of add-api.\n", PromptVersion: "aaaa1111", MemoryVersion: "bbbb2222"}))
	at, err := st.BeginAttempt(store.WithExecution(run, "plan-1"), "plan-1", "claude", 0)
	must(err)
	must(st.AppendLogChunk(ctx, "plan-1", "combined", "the plan is written\n"))
	must(st.FinishAttempt(ctx, at, 0, store.AttemptOK, 0))
	_, err = st.EndExecution(run, "plan-1", store.ExecutionEnd{Status: "succeeded", EngineUsed: "claude", AgentExit: true})
	must(err)

	// The BUILD: claude stalls and is stopped by the idle watchdog, codex takes over and fails.
	must(st.CreateExecution(run, store.Execution{ExecID: "build-1", Kind: "worker", ChangeName: change, Phase: "apply", Role: "build",
		EnginesRequested: "claude,codex", PromptText: "# BUILD\n\nImplement add-api.\n"}))
	at, err = st.BeginAttempt(store.WithExecution(run, "build-1"), "build-1", "claude", 0)
	must(err)
	must(st.BeginIdlePeriod(ctx, at, time.Now().Add(-10*time.Minute), 300*time.Second))
	must(st.FinishAttempt(ctx, at, 143, store.AttemptIdleStalled, 300))
	at, err = st.BeginAttempt(store.WithExecution(run, "build-1"), "build-1", "codex", 0)
	must(err)
	must(st.AppendLogChunk(ctx, "build-1", "combined", "codex: the tests fail\n"))
	must(st.FinishAttempt(ctx, at, 1, store.AttemptError, 0))
	_, err = st.EndExecution(run, "build-1", store.ExecutionEnd{Status: "failed", ExitCode: 1, EngineUsed: "codex", AgentExit: true})
	must(err)

	// The gates fail on the BUILD, a repair is asked for, and the gates pass on the repair.
	failed, err := st.BeginGateRound(run, store.GateRound{Context: "worker_verify", ChangeName: change, AfterExecID: "build-1", Slot: 1})
	must(err)
	must(st.RecordGateRun(store.WithGateRound(run, failed), store.GateRunRecord{ChangeName: change, Context: "worker_verify", RepoName: "backend",
		GateName: "unit-tests", Command: "go test ./...", Slot: 1, ExitCode: 1, DurationMS: 4200, FailureExcerpt: "--- FAIL: TestCreateWidget"}))
	must(st.FinishGateRound(ctx, failed, "failed"))

	must(st.CreateExecution(run, store.Execution{ExecID: "repair-1", Kind: "worker", ChangeName: change, Phase: "repair-1", Role: "build",
		EnginesRequested: "codex", PromptText: "# BUILD\n\nFix exactly this: TestCreateWidget.\n"}))
	at, err = st.BeginAttempt(store.WithExecution(run, "repair-1"), "repair-1", "codex", 0)
	must(err)
	must(st.AppendLogChunk(ctx, "repair-1", "combined", "codex: fixed TestCreateWidget\n"))
	must(st.FinishAttempt(ctx, at, 0, store.AttemptOK, 0))
	_, err = st.EndExecution(run, "repair-1", store.ExecutionEnd{Status: "succeeded", EngineUsed: "codex", AgentExit: true})
	must(err)
	passed, err := st.BeginGateRound(run, store.GateRound{Context: "worker_verify", ChangeName: change, AfterExecID: "repair-1", Slot: 1})
	must(err)
	must(st.RecordGateRun(store.WithGateRound(run, passed), store.GateRunRecord{ChangeName: change, Context: "worker_verify", RepoName: "backend",
		GateName: "unit-tests", Command: "go test ./...", Slot: 1, ExitCode: 0, DurationMS: 3900}))
	must(st.FinishGateRound(ctx, passed, "passed"))
	must(st.RecordRepairRound(run, store.RepairRoundRecord{ChangeName: change, Phase: "repair-1", ExecID: "repair-1", AskedReason: "gate_failure",
		AskedDetails: "--- FAIL: TestCreateWidget", AskedGateRoundID: failed, ChangedDiffstat: " api/widget.go | 4 ++--", ResultPassed: true, ResultGateRoundID: passed}))
	must(st.FinishChangeRun(ctx, runID, "ready"))

	// A cycle.
	must(st.BeginCycle(ctx, store.Cycle{Stamp: "20261108T120000Z"}))
	must(st.CreateExecution(ctx, store.Execution{ExecID: "cycle-20261108T120000Z", Kind: "cycle", CycleStamp: "20261108T120000Z", EnginesRequested: "claude", Phase: "cycle"}))
	must(st.AppendLogChunk(ctx, "cycle-20261108T120000Z", "combined", "== cycle 20261108T120000Z\n== done\n"))
	must(st.FinishExecution(ctx, "cycle-20261108T120000Z", "succeeded", 0))

	// An agent run, a follow-up to it, and an operation of the kit.
	must(st.CreateExecution(ctx, store.Execution{ExecID: "run-1", Kind: "portal_run", EnginesRequested: "hermes", PromptText: "Summarize the open changes\nand list the risky ones", Workdir: "/tmp/workspace"}))
	must(st.AppendLogChunk(ctx, "run-1", "combined", "three changes are open\n"))
	must(st.FinishExecution(ctx, "run-1", "succeeded", 0))
	must(st.CreateExecution(ctx, store.Execution{ExecID: "run-2", Kind: "portal_run", EnginesRequested: "hermes", PromptText: "context…\n\nwhich one first?", Summary: "which one first?",
		ParentExecID: "run-1", ParentRelation: "continue", Workdir: "/tmp/workspace"}))
	must(st.FinishExecution(ctx, "run-2", "stopped", 143))
	must(st.CreateExecution(ctx, store.Execution{ExecID: "job-1", Kind: "portal_job", Phase: "dispatch", JobKind: "dispatch", CommandText: "foreman-kit dispatch", Workdir: "/tmp/workspace"}))
	must(st.AppendLogChunk(ctx, "job-1", "combined", "dispatcher: nothing to do\n"))
	must(st.FinishExecution(ctx, "job-1", "failed", 2))

	// A BUILD that runs now, whose agent has shown no activity for a while.
	liveRun, err := st.BeginChangeRun(ctx, "add-search", 2, "/tmp/worker-add-search", "feat/spec-add-search")
	must(err)
	live := store.WithChangeRun(ctx, liveRun, "add-search")
	must(st.CreateExecution(live, store.Execution{ExecID: "build-live", Kind: "worker", ChangeName: "add-search", Phase: "apply", Role: "build",
		EnginesRequested: "claude", PromptText: "# BUILD\n\nImplement add-search.\n"}))
	at, err = st.BeginAttempt(store.WithExecution(live, "build-live"), "build-live", "claude", 0)
	must(err)
	must(st.AppendLogChunk(ctx, "build-live", "combined", "reading the specs\n"))
	must(st.BeginIdlePeriod(ctx, at, time.Now().Add(-7*time.Minute), 1200*time.Second))
}

func TestTheReadsOfTheExecutions(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	seedExecutions(t, st)

	list := func(query string) []map[string]any {
		t.Helper()
		code, body, raw, _ := doReq(t, srv, http.MethodGet, "/api/executions"+query, nil)
		if code != http.StatusOK {
			t.Fatalf("GET /api/executions%s = %d: %s", query, code, raw)
		}
		items, ok := body["items"].([]any)
		if !ok {
			t.Fatalf("GET /api/executions%s has no list of items: %s", query, raw)
		}
		out := make([]map[string]any, 0, len(items))
		for _, it := range items {
			out = append(out, it.(map[string]any))
		}
		return out
	}
	ids := func(items []map[string]any) string {
		var out []string
		for _, it := range items {
			out = append(out, asStr(it["exec_id"]))
		}
		return strings.Join(out, " ")
	}

	// 9. The list: every kind, the latest first, filtered by the columns of an execution.
	if all := list(""); len(all) != 8 {
		t.Fatalf("the list has %d executions (%s), want the 8 on record", len(all), ids(all))
	}
	for query, want := range map[string]string{
		"?kind=worker":                     "build-live repair-1 build-1 plan-1",
		"?kind=cycle":                      "cycle-20261108T120000Z",
		"?kind=portal_run":                 "run-2 run-1",
		"?kind=portal_job":                 "job-1",
		"?status=running":                  "build-live",
		"?status=failed":                   "job-1 build-1",
		"?engine=codex":                    "repair-1 build-1",
		"?change=add-api&status=succeeded": "repair-1 plan-1",
		"?cycle=20261108T120000Z":          "cycle-20261108T120000Z",
		"?kind=portal_run&status=failed":   "",
	} {
		if got := ids(list(query)); got != want {
			t.Errorf("GET /api/executions%s = [%s], want [%s]", query, got, want)
		}
	}
	for _, it := range list("?status=running") {
		idle, _ := it["idle"].(map[string]any)
		if it["name"] != "BUILD" || it["about"] != "add-search" || idle == nil || idle["engine"] != "claude" {
			t.Errorf("the running BUILD in the list = %v, want its name, its change and its idle agent", it)
		}
	}
	// It pages by cursor: every execution once, none twice.
	seen := map[string]int{}
	next := ""
	for page := 0; page < 10; page++ {
		query := "?limit=3"
		if next != "" {
			query += "&before=" + url.QueryEscape(next)
		}
		_, body, _, _ := doReq(t, srv, http.MethodGet, "/api/executions"+query, nil)
		items, _ := body["items"].([]any)
		for _, it := range items {
			seen[asStr(it.(map[string]any)["exec_id"])]++
		}
		if next = asStr(body["next"]); next == "" {
			break
		}
		if len(items) != 3 {
			t.Errorf("a page that has a next one holds %d executions, want 3", len(items))
		}
	}
	if len(seen) != 8 {
		t.Errorf("paging by cursor reached %d executions, want 8: %v", len(seen), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("paging by cursor returned %s %d times", id, n)
		}
	}
	for _, bad := range []string{"?kind=job", "?status=ok", "?limit=0", "?limit=many", "?before=not-a-cursor"} {
		code, body, _, _ := doReq(t, srv, http.MethodGet, "/api/executions"+bad, nil)
		if c, _ := errorOf(body); code != http.StatusBadRequest || c != "invalid_input" {
			t.Errorf("GET /api/executions%s = %d %v, want 400 invalid_input", bad, code, body)
		}
	}

	// 10. One execution: its attempts with their idle periods, where it comes from, what verified it.
	code, body, raw, _ := doReq(t, srv, http.MethodGet, "/api/executions/build-1", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/executions/build-1 = %d: %s", code, raw)
	}
	attempts, _ := body["attempts"].([]any)
	if len(attempts) != 2 {
		t.Fatalf("build-1 has %d attempts, want 2: %s", len(attempts), raw)
	}
	first, second := attempts[0].(map[string]any), attempts[1].(map[string]any)
	periods, _ := first["idle_periods"].([]any)
	if first["engine"] != "claude" || first["outcome"] != "idle_stalled" || len(periods) != 1 || periods[0].(map[string]any)["ended_by"] != "stalled" ||
		second["engine"] != "codex" || second["outcome"] != "error" {
		t.Errorf("the attempts of build-1 = %v", attempts)
	}
	runOf, _ := body["run"].(map[string]any)
	rounds, _ := body["gate_rounds"].([]any)
	events, _ := body["events"].([]any)
	outcome, _ := body["outcome"].(map[string]any)
	if body["name"] != "BUILD" || body["about"] != "add-api" || runOf["result"] != "ready" || len(rounds) != 1 || len(events) == 0 ||
		outcome["tone"] != "error" || body["can_stop"] != false || body["can_continue"] != false {
		t.Errorf("GET /api/executions/build-1 = %s", raw)
	}
	if next, _ := body["next"].(map[string]any); next["exec_id"] != "repair-1" {
		t.Errorf("what ran next for the change = %v, want repair-1", body["next"])
	}

	_, body, raw, _ = doReq(t, srv, http.MethodGet, "/api/executions/repair-1", nil)
	repair, _ := body["repair"].(map[string]any)
	if repair["asked_reason"] != "gate_failure" || repair["result_passed"] != true || body["name"] != "BUILD repair 1" {
		t.Errorf("GET /api/executions/repair-1 = %s", raw)
	}
	_, body, raw, _ = doReq(t, srv, http.MethodGet, "/api/executions/run-1", nil)
	conts, _ := body["continuations"].([]any)
	if len(conts) != 1 || conts[0].(map[string]any)["exec_id"] != "run-2" || body["can_continue"] != true || body["can_rerun"] != true || body["can_stop"] != false ||
		body["about"] != "Summarize the open changes" {
		t.Errorf("GET /api/executions/run-1 = %s", raw)
	}
	_, body, raw, _ = doReq(t, srv, http.MethodGet, "/api/executions/run-2", nil)
	if parent, _ := body["parent"].(map[string]any); parent["exec_id"] != "run-1" || body["about"] != "which one first?" {
		t.Errorf("GET /api/executions/run-2 = %s", raw)
	}
	_, body, raw, _ = doReq(t, srv, http.MethodGet, "/api/executions/build-live", nil)
	idle, _ := body["idle"].(map[string]any)
	outcome, _ = body["outcome"].(map[string]any)
	if idle == nil || outcome["tone"] != "warning" || !strings.Contains(asStr(outcome["text"]), "no activity") {
		t.Errorf("GET /api/executions/build-live = %s", raw)
	}
	if _, has := body["Log"]; has || strings.Contains(raw, "reading the specs") {
		t.Errorf("the read of an execution carries its output; it is read at /log: %s", raw)
	}
	code, body, _, _ = doReq(t, srv, http.MethodGet, "/api/executions/no-such-execution", nil)
	if c, _ := errorOf(body); code != http.StatusNotFound || c != "not_found" {
		t.Errorf("GET /api/executions/no-such-execution = %d %v, want 404 not_found", code, body)
	}

	// 11. The output, by chunks in the order they were written.
	ctx := context.Background()
	for i := 2; i <= 5; i++ {
		if err := st.AppendLogChunk(ctx, "run-1", "combined", fmt.Sprintf("line %d\n", i)); err != nil {
			t.Fatal(err)
		}
	}
	_, body, raw, _ = doReq(t, srv, http.MethodGet, "/api/executions/run-1/log?limit=2", nil)
	chunks, _ := body["items"].([]any)
	if len(chunks) != 2 || chunks[0].(map[string]any)["content"] != "three changes are open\n" || asStr(body["next"]) == "" {
		t.Fatalf("GET /api/executions/run-1/log?limit=2 = %s", raw)
	}
	var text strings.Builder
	after := ""
	for page := 0; page < 10; page++ {
		_, body, _, _ := doReq(t, srv, http.MethodGet, "/api/executions/run-1/log?limit=2&after="+after, nil)
		items, _ := body["items"].([]any)
		for _, c := range items {
			text.WriteString(asStr(c.(map[string]any)["content"]))
		}
		if after = asStr(body["next"]); after == "" {
			break
		}
	}
	const whole = "three changes are open\nline 2\nline 3\nline 4\nline 5\n"
	if text.String() != whole {
		t.Errorf("the output read by chunks = %q, want %q", text.String(), whole)
	}
	code, _, raw, hdr := doReq(t, srv, http.MethodGet, "/api/executions/run-1/log?format=text", nil)
	if code != http.StatusOK || raw != whole || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") || hdr.Get("Content-Disposition") != "" {
		t.Errorf("the output as text = %d %q (%s)", code, raw, hdr.Get("Content-Type"))
	}
	if _, _, _, hdr := doReq(t, srv, http.MethodGet, "/api/executions/run-1/log?format=text&download=1", nil); !strings.Contains(hdr.Get("Content-Disposition"), `filename="run-1.log"`) {
		t.Errorf("the output to save has the header %q", hdr.Get("Content-Disposition"))
	}
	for _, path := range []string{"/api/executions/no-such-execution/log", "/api/executions/no-such-execution/log?format=text", "/api/executions/no-such-execution/log/stream"} {
		if code, _, _, _ := doReq(t, srv, http.MethodGet, path, nil); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
	if code, _, _, _ := doReq(t, srv, http.MethodGet, "/api/executions/run-1/log?after=x", nil); code != http.StatusBadRequest {
		t.Errorf("a cursor of the output that is no cursor = %d, want 400", code)
	}
}

// sseMessage is one message of a stream of server-sent events.
type sseMessage struct{ id, event, data string }

// readSSE reads the messages of a stream until it ends or until stop says so.
func readSSE(t *testing.T, body *bufio.Scanner, stop func(sseMessage) bool) []sseMessage {
	t.Helper()
	var out []sseMessage
	var cur sseMessage
	for body.Scan() {
		line := body.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		case line == "" && cur.event != "":
			out = append(out, cur)
			if stop != nil && stop(cur) {
				return out
			}
			cur = sseMessage{}
		}
	}
	return out
}

// TestTheStreamOfAnOutput: the stream sends each chunk of the output as it is written, with the id of
// the chunk, and says when the execution ended. A connection that comes back with the last id it got
// continues after it.
func TestTheStreamOfAnOutput(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	ctx := context.Background()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	const id = "run-streamed"
	if err := st.CreateExecution(ctx, store.Execution{ExecID: id, Kind: "portal_run", EnginesRequested: "claude", PromptText: "stream"}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if err := st.AppendLogChunk(ctx, id, "combined", fmt.Sprintf("chunk %d\n", i)); err != nil {
			t.Fatal(err)
		}
	}
	open := func(query string, lastEventID string) *http.Response {
		t.Helper()
		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		t.Cleanup(cancel)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, ts.URL+"/api/executions/"+id+"/log/stream"+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		if lastEventID != "" {
			req.Header.Set("Last-Event-ID", lastEventID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("the stream answered %d (%s)", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		return resp
	}
	contentOf := func(m sseMessage) string {
		var chunk store.LogChunk
		if err := json.Unmarshal([]byte(m.data), &chunk); err != nil {
			t.Fatalf("a log message that is not a chunk: %q", m.data)
		}
		if strconv.FormatInt(chunk.ID, 10) != m.id {
			t.Errorf("the message has the id %q and carries the chunk %d", m.id, chunk.ID)
		}
		return chunk.Content
	}

	// A first connection gets what is written, then what is written while it listens.
	resp := open("", "")
	scanner := bufio.NewScanner(resp.Body)
	got := readSSE(t, scanner, func(m sseMessage) bool { return strings.Contains(m.data, "chunk 3") })
	if len(got) != 3 || contentOf(got[0]) != "chunk 1\n" || contentOf(got[2]) != "chunk 3\n" {
		t.Fatalf("the stream sent %+v, want the three chunks on record", got)
	}
	if err := st.AppendLogChunk(ctx, id, "combined", "chunk 4\n"); err != nil {
		t.Fatal(err)
	}
	more := readSSE(t, scanner, func(m sseMessage) bool { return true })
	if len(more) != 1 || more[0].event != "log" || contentOf(more[0]) != "chunk 4\n" {
		t.Fatalf("a chunk written while the stream is open arrived as %+v", more)
	}
	lastID := more[0].id
	_ = resp.Body.Close()

	// The connection dropped. The browser comes back with the last id it got, and the address it
	// first asked for: the stream continues after that id, not after the one in the address.
	if err := st.AppendLogChunk(ctx, id, "combined", "chunk 5\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishExecution(ctx, id, "succeeded", 0); err != nil {
		t.Fatal(err)
	}
	rest := readSSE(t, bufio.NewScanner(open("?after="+got[0].id, lastID).Body), nil)
	if len(rest) != 2 || rest[0].event != "log" || contentOf(rest[0]) != "chunk 5\n" || rest[1].event != "done" || !strings.Contains(rest[1].data, `"status":"succeeded"`) {
		t.Fatalf("the stream that came back sent %+v, want chunk 5 and the end", rest)
	}

	// A page that already shows the output says where it ends.
	tail := readSSE(t, bufio.NewScanner(open("?after="+got[1].id, "").Body), nil)
	if len(tail) != 4 || contentOf(tail[0]) != "chunk 3\n" || tail[3].event != "done" {
		t.Fatalf("the stream after the second chunk sent %+v", tail)
	}
	if code, _, _, _ := doReq(t, srv, http.MethodGet, "/api/executions/"+id+"/log/stream?after=soon", nil); code != http.StatusBadRequest {
		t.Errorf("a stream asked to start at no chunk = %d, want 400", code)
	}
}

// TestAutoRunsOnTheAgentsOfTheApplication: the engine "auto" asks for the agents the application
// uses, as the application picks among them: the kit is given their names. Nothing is picked in the
// place of an agent the application does not name, so an application that names none starts
// nothing and says where its agents are named.
func TestAutoRunsOnTheAgentsOfTheApplication(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	given := make(chan string, 2)
	kitOf(srv).agent = func(_ context.Context, _, engine, _ string) (string, int, error) {
		given <- engine
		return "done", 0, nil
	}

	code, body, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions", map[string]any{"engine": "auto", "prompt": "Check the status"})
	runID, ex := startedOf(t, code, body, raw)
	if got := <-given; got != "claude,hermes" {
		t.Errorf("the kit was given %q to run auto on, want the agents of the application: claude,hermes", got)
	}
	// The execution records what was asked for: repeated, it asks for the agents of that moment.
	if ex["engines_requested"] != "auto" {
		t.Errorf("the execution of an auto run asked for %v, want auto", ex["engines_requested"])
	}
	waitExecution(t, st, runID)
	waitWork(t, srv.exec, runID)
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/executions/"+runID+"/rerun", nil)
	rerunID, _ := startedOf(t, code, body, raw)
	if got := <-given; got != "claude,hermes" {
		t.Errorf("the kit was given %q to repeat an auto run on, want claude,hermes", got)
	}
	waitExecution(t, st, rerunID)
	waitWork(t, srv.exec, rerunID)

	// An application that names no agent.
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "agents.yaml"), []byte("agents: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/executions", map[string]any{"engine": "auto", "prompt": "Check the status"})
	if c, msg := errorOf(body); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, ".foreman-kit/agents.yaml lists none") {
		t.Errorf("POST /api/executions with auto and no agent = %d %s; want 409 naming the file of the agents", code, raw)
	}
	// With no agent named, the form offers auto, and auto says the same.
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/executions", map[string]any{"prompt": "Check the status"})
	if c, _ := errorOf(body); code != http.StatusConflict || c != "conflict" {
		t.Errorf("POST /api/executions with no engine and no agent = %d %s; want 409", code, raw)
	}
	select {
	case got := <-given:
		t.Errorf("the kit was asked to run %q for an application that names no agent", got)
	default:
	}
}
