package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/store"
)

func setupTestPortal(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	if err := cycle.Init(ctx, root, cycle.KitOptions{Agents: "claude,hermes"}, io.Discard); err != nil {
		t.Fatalf("cycle.Init failed: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-b", "master"},
		{"config", "user.name", "Portal Test"},
		{"config", "user.email", "portal@example.com"},
		{"add", "-A"},
		{"commit", "-m", "initial commit"},
	} {
		if _, err := gitx.RunGit(ctx, root, args...); err != nil {
			t.Fatalf("git %v failed: %v", args, err)
		}
	}

	dbPath := filepath.Join(t.TempDir(), "portal.db")
	wsID, canon := store.WorkspaceID(root)
	st, err := store.OpenAt(dbPath, wsID, canon)
	if err != nil {
		t.Fatalf("store.OpenAt failed: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv, kit := newTestServer(t, root, st)
	kit.agent = func(ctx context.Context, workdir, agent, prompt string) (string, int, error) {
		return "executed prompt: " + prompt, 0, nil
	}
	kit.operation = func(ctx context.Context, dir, operation string) (string, int, error) {
		return "job " + operation + " ok", 0, nil
	}
	return srv, st, root
}

// kitOf returns the fakeKit a test Server was built on.
func kitOf(srv *Server) *fakeKit { return srv.kit.(*fakeKit) }

// backlogItems reads the backlog of the application at root as its file holds it.
func backlogItems(t *testing.T, root string) []map[string]any {
	t.Helper()
	items, err := cycle.ReadJSONList(filepath.Join(root, cycle.BacklogFile))
	if err != nil {
		t.Fatalf("reading the backlog: %v", err)
	}
	return items
}

func doReq(t *testing.T, srv http.Handler, method, path string, body any) (int, map[string]any, string, http.Header) {
	t.Helper()
	var rBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rBody = bytes.NewReader(b)
	}
	target := path
	if strings.HasPrefix(path, "/") {
		target = "http://127.0.0.1:4567" + path
	}
	req := httptest.NewRequest(method, target, rBody)
	req.Host = "127.0.0.1:4567"
	req.RemoteAddr = "127.0.0.1:12345"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	raw := rec.Body.String()
	var parsed map[string]any
	_ = json.Unmarshal([]byte(raw), &parsed)
	return rec.Code, parsed, raw, rec.Header()
}

func TestPortalStaticAndPageRoutes(t *testing.T) {
	srv, _, _ := setupTestPortal(t)

	// Root /
	code, _, raw, hdr := doReq(t, srv, http.MethodGet, "/", nil)
	if code != http.StatusOK || !strings.Contains(hdr.Get("Content-Type"), "text/html") || !strings.Contains(raw, "<!doctype html>") {
		t.Fatalf("unexpected GET /: code=%d type=%s", code, hdr.Get("Content-Type"))
	}

	// A page of its own: /changes is rendered on the server with its own title.
	code, _, raw, hdr = doReq(t, srv, http.MethodGet, "/changes", nil)
	if code != http.StatusOK || !strings.Contains(hdr.Get("Content-Type"), "text/html") || !strings.Contains(raw, "<title>Changes · Foreman</title>") {
		t.Fatalf("unexpected page /changes: code=%d", code)
	}

	// /assets/portal.css
	code, _, raw, hdr = doReq(t, srv, http.MethodGet, "/assets/portal.css", nil)
	if code != http.StatusOK || !strings.Contains(hdr.Get("Content-Type"), "text/css") || len(raw) < 100 {
		t.Fatalf("unexpected /assets/portal.css: code=%d len=%d", code, len(raw))
	}

	// /assets/portal.js (verify alphabetical concatenation order)
	code, _, raw, hdr = doReq(t, srv, http.MethodGet, "/assets/portal.js", nil)
	if code != http.StatusOK || !strings.Contains(hdr.Get("Content-Type"), "application/javascript") {
		t.Fatalf("unexpected /assets/portal.js: code=%d type=%s", code, hdr.Get("Content-Type"))
	}
	idx00 := strings.Index(raw, "// 00_core.js")
	idx10 := strings.Index(raw, "// 10_viewer.js")
	idx20 := strings.Index(raw, "// 20_live.js")
	idx30 := strings.Index(raw, "// 30_recent.js")
	if idx00 < 0 || idx10 <= idx00 || idx20 <= idx10 || idx30 <= idx20 {
		t.Fatalf("expected portal.js scripts in alphabetical order, indices: %d, %d, %d, %d", idx00, idx10, idx20, idx30)
	}
}

func TestPortalRESTEndpoints(t *testing.T) {
	ctx := context.Background()
	srv, st, root := setupTestPortal(t)

	// Seed backlog, proposals, worktree artifacts, quarantine, the merge queue, executions, events
	_ = cycle.WriteJSONList(filepath.Join(root, cycle.BacklogFile), []map[string]any{
		{"change_name": "feat-alpha", "priority": "high", "description": "Alpha feature"},
	})
	_ = cycle.WriteProposals(root, []map[string]any{
		{"change_name": "prop-beta", "status": "proposed", "source": "roadmap", "description": "Beta proposal"},
	})
	wtChangeDir := filepath.Join(root, ".worktrees", "worker-feat-alpha", "openspec", "changes", "feat-alpha")
	_ = os.MkdirAll(wtChangeDir, 0o755)
	_ = os.WriteFile(filepath.Join(wtChangeDir, "proposal.md"), []byte("# Proposal feat-alpha\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wtChangeDir, "tasks.md"), []byte("- [x] 1.1 Task one\n- [ ] 1.2 Task two\n"), 0o644)

	_ = st.BlockChange(ctx, store.QuarantineRecord{
		ChangeName: "blocked-gamma",
		Reason:     "gate_failure",
		Details:    "lint failed",
	})
	_ = st.EnqueueReady(ctx, store.ReadyManifest{
		ChangeName:   "ready-delta",
		Branch:       "feat/spec-ready-delta",
		Worktree:     filepath.Join(root, ".worktrees", "worker-ready-delta"),
		HeadSHA:      "1234567890abcdef1234567890abcdef12345678",
		Capabilities: []string{"auth"},
	})
	_ = st.CreateExecution(ctx, store.Execution{
		ExecID:    "cycle-20261108T120000Z",
		Kind:      "cycle",
		Agent:     "claude",
		Status:    "succeeded",
		StartedAt: "2026-11-08T12:00:00Z",
	})
	_ = st.AppendLogChunk(ctx, "cycle-20261108T120000Z", "combined", "== cycle 20261108T120000Z\npreflight: OK\nstrategy: api-contracts (defects)\n== done\n")
	_ = st.EmitEvent(ctx, store.EventInput{
		Event:   "worker.started",
		Change:  "feat-alpha",
		Engine:  "claude",
		Payload: map[string]any{"slot": 1},
	})

	endpoints := []string{
		"/api/dashboard",
		"/api/inbox",
		"/api/inbox?kind=blocked",
		"/api/system/processes",
		"/api/agents",
		"/api/changes",
		"/api/changes/feat-alpha",
		"/api/changes/feat-alpha/events",
		"/api/changes/feat-alpha/files/proposal.md",
		"/api/executions",
		"/api/executions/cycle-20261108T120000Z",
		"/api/executions/cycle-20261108T120000Z/log",
		"/api/cycles",
		"/api/cycles/20261108T120000Z",
		"/api/events",
		"/api/events/1",
		"/api/backlog",
		"/api/backlog?status=rejected",
		"/api/merge",
		"/api/system/settings",
		"/api/system/recommendations",
		"/api/system/setup",
		"/api/quality",
		"/api/visual",
		"/api/system/prompts",
		"/api/system/prompts/plan",
		"/api/system/health",
	}

	for _, ep := range endpoints {
		code, data, raw, _ := doReq(t, srv, http.MethodGet, ep, nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s returned %d: %s", ep, code, raw)
		}
		if len(data) == 0 {
			t.Fatalf("GET %s returned empty JSON object: %s", ep, raw)
		}
	}
}

func TestPortalSSEStreams(t *testing.T) {
	ctx := context.Background()
	srv, st, _ := setupTestPortal(t)

	// 1. Create an execution and append log chunks
	execID := "worker-test-stream"
	if err := st.CreateExecution(ctx, store.Execution{
		ExecID:     execID,
		Kind:       "worker",
		ChangeName: "feat-stream",
		Agent:      "claude",
		Status:     "running",
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.AppendLogChunk(ctx, execID, "combined", "chunk one\n")
	_ = st.AppendLogChunk(ctx, execID, "combined", "chunk two\n")
	_ = st.FinishExecution(ctx, execID, "succeeded", 0)

	code, _, raw, hdr := doReq(t, srv, http.MethodGet, "/api/executions/"+execID+"/log/stream", nil)
	if code != http.StatusOK {
		t.Fatalf("SSE logs returned %d: %s", code, raw)
	}
	if !strings.Contains(hdr.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %s", hdr.Get("Content-Type"))
	}
	if !strings.Contains(raw, "chunk one") || !strings.Contains(raw, "chunk two") || !strings.Contains(raw, "event: done") {
		t.Fatalf("unexpected SSE log stream body:\n%s", raw)
	}
}

func TestPortalActionsPOSTAndDELETE(t *testing.T) {
	ctx := context.Background()
	srv, st, root := setupTestPortal(t)

	// 1. Pause & Resume
	code, data, _, _ := doReq(t, srv, http.MethodPost, "/api/workspace/pause", map[string]any{"reason": "maintenance window"})
	if code != http.StatusOK || data["reason"] != "maintenance window" || data["paused_by"] != "portal" {
		t.Fatalf("pause failed: %+v", data)
	}
	ws, _ := st.GetWorkspaceState(ctx)
	if !ws.Paused || ws.PauseReason != "maintenance window" {
		t.Fatalf("expected store paused=true, got %+v", ws)
	}

	code, data, _, _ = doReq(t, srv, http.MethodPost, "/api/workspace/resume", nil)
	if code != http.StatusOK || data["paused"] != false {
		t.Fatalf("resume failed: %+v", data)
	}
	ws, _ = st.GetWorkspaceState(ctx)
	if ws.Paused {
		t.Fatalf("expected store paused=false after resume")
	}

	// 2. Quarantine & Unblock
	_ = st.BlockChange(ctx, store.QuarantineRecord{
		ChangeName: "quarantined-one",
		Reason:     "risk_review",
		Details:    "touches migration",
	})
	// Accepting the risk of a change blocked for its risk lifts the block: one action, one effect.
	var raw string
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/changes/quarantined-one/risk-approval", map[string]any{})
	if code != http.StatusOK || data["state"] == "blocked" || data["block"] != nil {
		t.Fatalf("risk-approval failed (%d): %s", code, raw)
	}
	qRec, _ := st.GetQuarantine(ctx, "quarantined-one")
	if qRec == nil || !qRec.ApprovedRisk || qRec.Status == "blocked" {
		t.Fatalf("expected the block lifted and the risk approved, got: %+v", qRec)
	}
	// A block for another reason stays when the risk is accepted, and is lifted by unblocking.
	_ = st.BlockChange(ctx, store.QuarantineRecord{ChangeName: "quarantined-two", Reason: "gate_failure", Details: "lint failed"})
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/changes/quarantined-two/risk-approval", map[string]any{})
	if code != http.StatusOK || data["state"] != "blocked" {
		t.Fatalf("accepting the risk of a change blocked by a gate lifted its block (%d): %s", code, raw)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/changes/quarantined-two/unblock", map[string]any{"reset_retries": false})
	if code != http.StatusOK || data["state"] == "blocked" {
		t.Fatalf("unblock failed (%d): %s", code, raw)
	}
	if q, _ := st.GetQuarantine(ctx, "quarantined-two"); q == nil || q.Status == "blocked" || !q.ApprovedRisk {
		t.Fatalf("expected the block lifted and the accepted risk kept, got: %+v", q)
	}

	// 3. Approve a proposal
	_ = cycle.WriteProposals(root, []map[string]any{
		{"change_name": "prop-approve-me", "status": "proposed", "source": "cycle", "description": "Approve test"},
	})
	code, data, _, _ = doReq(t, srv, http.MethodPost, "/api/proposals/prop-approve-me/approve", map[string]any{})
	if code != http.StatusOK || data["status"] != "approved" {
		t.Fatalf("approve failed: %+v", data)
	}
	props, _ := cycle.ReadProposals(root)
	if len(props) != 1 || props[0]["status"] != "approved" {
		t.Fatalf("expected proposal status=approved, got %+v", props)
	}

	// 4. Add & Delete backlog item
	code, data, _, _ = doReq(t, srv, http.MethodPost, "/api/backlog", map[string]any{
		"change_name": "new-backlog-item",
		"priority":    "high",
		"description": "Added via portal API",
	})
	if code != http.StatusCreated || data["change_name"] != "new-backlog-item" || data["priority"] != "high" {
		t.Fatalf("POST /api/backlog failed: %+v", data)
	}
	code, data, _, _ = doReq(t, srv, http.MethodDelete, "/api/backlog/new-backlog-item", nil)
	if code != http.StatusOK || data["in_backlog"] != false {
		t.Fatalf("DELETE /api/backlog/new-backlog-item failed: %+v", data)
	}

	// 5. An agent run and a follow-up to it
	code, runData, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions", map[string]any{
		"engine": "claude",
		"prompt": "Check system status",
	})
	runID, _ := startedOf(t, code, runData, raw)
	waitExecution(t, st, runID)
	waitWork(t, srv.exec, runID)

	code, contData, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions/"+runID+"/continue", map[string]any{
		"prompt": "Follow up question",
	})
	contID, contEx := startedOf(t, code, contData, raw)
	if contEx["parent_exec_id"] != runID {
		t.Fatalf("POST /api/executions/:id/continue failed: %+v", contData)
	}
	waitExecution(t, st, contID)

	// 6. An operation of the kit
	code, jobData, raw, _ := doReq(t, srv, http.MethodPost, "/api/gates/runs", map[string]any{})
	jobID, jobEx := startedOf(t, code, jobData, raw)
	if jobEx["kind"] != "portal_job" || jobEx["job_kind"] != "gates" {
		t.Fatalf("POST /api/gates/runs started %+v", jobEx)
	}
	waitExecution(t, st, jobID)

	// 7. Settings update
	code, cfgData, _, _ := doReq(t, srv, http.MethodPost, "/api/system/settings", map[string]any{
		"key":   "visual_qa.time_budget",
		"value": "60m",
	})
	if code != http.StatusOK || !strings.Contains(asStr(cfgData["output"]), "changed") {
		t.Fatalf("POST /api/system/settings failed: %+v", cfgData)
	}
}

func TestPortalEngineHealthGatesAndRepairsEndpoints(t *testing.T) {
	ctx := context.Background()
	srv, st, _ := setupTestPortal(t)

	_ = st.RecordEngineHealth(ctx, store.EngineHealthRecord{
		Engine:        "codex",
		Installed:     true,
		Status:        "quota_exceeded",
		LastError:     "hit your usage limit",
		CooldownUntil: time.Now().UTC().Add(30 * time.Minute).Unix(),
	})
	_ = st.RecordGateRun(ctx, store.GateRunRecord{
		ChangeName:     "add-billing",
		Context:        "worker",
		RepoName:       "repos/backend",
		GateName:       "test",
		Command:        "go test ./...",
		Slot:           1,
		ExitCode:       1,
		DurationMS:     1200,
		FailureExcerpt: "--- FAIL: TestBilling",
	})
	// A repair round belongs to a worker's run of the change.
	runID, err := st.BeginChangeRun(ctx, "add-billing", 1, "/tmp/worker-add-billing", "feat/spec-add-billing")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.RecordRepairRound(store.WithChangeRun(ctx, runID, "add-billing"), store.RepairRoundRecord{
		ChangeName:   "add-billing",
		Phase:        "build-repair-1",
		Engine:       "claude",
		AskedReason:  "gate_failure",
		AskedDetails: "--- FAIL: TestBilling",
		ResultPassed: true,
	})

	// The health a run recorded for an engine is in the read of the agents.
	code, engData, rawAgents, _ := doReq(t, srv, http.MethodGet, "/api/agents", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/agents failed: code=%d %s", code, rawAgents)
	}
	codexHealth := ""
	for _, it := range engData["agents"].([]any) {
		if a, _ := it.(map[string]any); a["name"] == "codex" {
			codexHealth = asStr(a["health"])
		}
	}
	if codexHealth != "quota_exceeded" {
		t.Fatalf("codex in /api/agents has the health %q, want quota_exceeded: %s", codexHealth, rawAgents)
	}

	// The gates and the repairs of a change are in its story.
	code, story, raw, _ := doReq(t, srv, http.MethodGet, "/api/changes/add-billing", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/changes/add-billing failed: code=%d %s", code, raw)
	}
	items, _ := story["items"].([]any)
	repairs, gateRuns := 0, 0
	for _, it := range items {
		ep, _ := it.(map[string]any)
		if run, _ := ep["run"].(map[string]any); run != nil {
			list, _ := run["repairs"].([]any)
			repairs += len(list)
		}
		if gates, _ := ep["gates"].(map[string]any); gates != nil {
			list, _ := gates["runs"].([]any)
			gateRuns += len(list)
		}
	}
	if repairs != 1 || gateRuns != 1 {
		t.Fatalf("the story of add-billing has %d repair(s) and %d gate run(s) outside a run, want 1 and 1: %s", repairs, gateRuns, raw)
	}
}

func TestPortalSecurityCSRFOriginHostAndWorkdir(t *testing.T) {
	srv, st, root := setupTestPortal(t)

	// 1. Cross-origin POST /api/executions with Origin: http://evil.example.com -> 403 Forbidden
	{
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4567/api/executions", strings.NewReader(`{"engine":"claude","prompt":"hi"}`))
		req.Host = "127.0.0.1:4567"
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://evil.example.com")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for cross-origin POST /api/executions, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 2. Cross-port Origin and Referer on localhost -> 403 Forbidden; matching port -> 200 OK
	{
		// POST /api/workspace/pause with Origin: http://localhost:3000 to Host: localhost:4567 -> 403 Forbidden
		req := httptest.NewRequest(http.MethodPost, "http://localhost:4567/api/workspace/pause", strings.NewReader(`{"reason":"from port 3000"}`))
		req.Host = "localhost:4567"
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://localhost:3000")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for cross-port Origin http://localhost:3000 -> Host localhost:4567, got %d: %s", rec.Code, rec.Body.String())
		}

		// POST /api/workspace/pause with Referer: http://localhost:3000/app (Origin absent) -> 403 Forbidden
		reqRef := httptest.NewRequest(http.MethodPost, "http://localhost:4567/api/workspace/pause", strings.NewReader(`{"reason":"from referer 3000"}`))
		reqRef.Host = "localhost:4567"
		reqRef.RemoteAddr = "127.0.0.1:12345"
		reqRef.Header.Set("Content-Type", "application/json")
		reqRef.Header.Set("Referer", "http://localhost:3000/app")
		recRef := httptest.NewRecorder()
		srv.ServeHTTP(recRef, reqRef)
		if recRef.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for cross-port Referer http://localhost:3000/app -> Host localhost:4567, got %d: %s", recRef.Code, recRef.Body.String())
		}

		// POST /api/workspace/pause with Origin: http://localhost:4567 to Host: localhost:4567 -> 200 OK
		reqOK := httptest.NewRequest(http.MethodPost, "http://localhost:4567/api/workspace/pause", strings.NewReader(`{"reason":"same port"}`))
		reqOK.Host = "localhost:4567"
		reqOK.RemoteAddr = "127.0.0.1:12345"
		reqOK.Header.Set("Content-Type", "application/json")
		reqOK.Header.Set("Origin", "http://localhost:4567")
		recOK := httptest.NewRecorder()
		srv.ServeHTTP(recOK, reqOK)
		if recOK.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for same-port Origin http://localhost:4567 -> Host localhost:4567, got %d: %s", recOK.Code, recOK.Body.String())
		}
		_, _, _, _ = doReq(t, srv, http.MethodPost, "/api/workspace/resume", nil)
	}

	// 3. Cross-site POST /api/executions with Sec-Fetch-Site: cross-site -> 403 Forbidden
	{
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4567/api/executions", strings.NewReader(`{"engine":"claude","prompt":"hi"}`))
		req.Host = "127.0.0.1:4567"
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for Sec-Fetch-Site: cross-site POST /api/executions, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 4. DNS rebinding Host header -> 403 Forbidden
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req := httptest.NewRequest(method, "http://127.0.0.1:4567/api/dashboard", nil)
		req.Host = "evil.com:4567"
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for Host: evil.com:4567 (%s), got %d: %s", method, rec.Code, rec.Body.String())
		}
	}

	// 5. POST /api/executions?engine=claude&prompt=pwn without Content-Type: application/json -> 403 Forbidden
	for _, ct := range []string{"", "text/plain;charset=UTF-8", "application/x-www-form-urlencoded", "multipart/form-data; boundary=---"} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4567/api/executions?engine=claude&prompt=pwn", nil)
		req.Host = "127.0.0.1:4567"
		req.RemoteAddr = "127.0.0.1:12345"
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for no-cors query-param POST /api/executions (Content-Type=%q), got %d: %s", ct, rec.Code, rec.Body.String())
		}
	}

	// 5b. The same input in the address with a JSON content type and no body: nothing is read from
	// the address, so the prompt is missing and nothing runs.
	{
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4567/api/executions?engine=claude&prompt=pwn", nil)
		req.Host = "127.0.0.1:4567"
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for an action whose input is in the address, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 6. A request cannot name the directory an agent runs in: the field does not exist -> 400 Bad Request
	for _, badDir := range []string{"/etc", "/tmp", "../../etc"} {
		code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions", map[string]any{
			"engine":  "claude",
			"prompt":  "hi",
			"workdir": badDir,
		})
		if code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for workdir=%q, got %d: %s", badDir, code, raw)
		}
		if c, msg := errorOf(data); c != "invalid_input" || !strings.Contains(msg, "workdir") {
			t.Fatalf("expected the error to name the field that is not taken for %q, got %+v", badDir, data)
		}
	}
	if runs, _ := st.ListExecutions(context.Background(), "portal_run", "", 10); len(runs) != 0 {
		t.Fatalf("a refused request started an agent: %+v", runs)
	}

	// 7. Legitimate same-origin POST /api/executions -> 202 Accepted, and the agent runs at the root
	{
		bodyBytes, _ := json.Marshal(map[string]any{
			"engine": "claude",
			"prompt": "legitimate prompt",
		})
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4567/api/executions", bytes.NewReader(bodyBytes))
		req.Host = "127.0.0.1:4567"
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://127.0.0.1:4567")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted for legitimate same-origin POST /api/executions, got %d: %s", rec.Code, rec.Body.String())
		}
		runs, _ := st.ListExecutions(context.Background(), "portal_run", "", 10)
		if len(runs) != 1 {
			t.Fatalf("expected the one agent run that was asked for, got %+v", runs)
		}
		waitExecution(t, st, runs[0].ExecID)
		waitWork(t, srv.exec, runs[0].ExecID)
		realRoot, _ := filepath.EvalSymlinks(root)
		if runs[0].Workdir != root && runs[0].Workdir != realRoot {
			t.Fatalf("the agent ran in %q, want the root of the workspace", runs[0].Workdir)
		}
	}

	// 8. Non-loopback bind ("0.0.0.0") requires FOREMANKIT_PORTAL_TOKEN even with Host: localhost
	{
		nonLoopSrv, _ := newTestServer(t, root, st, WithBind("0.0.0.0"))
		t.Setenv("FOREMANKIT_PORTAL_TOKEN", "")

		for _, tc := range []struct {
			method string
			path   string
			body   string
		}{
			{http.MethodGet, "/api/dashboard", ""},
			{http.MethodPost, "/api/workspace/pause", `{"reason":"unauth"}`},
		} {
			var bodyReader io.Reader
			if tc.body != "" {
				bodyReader = strings.NewReader(tc.body)
			}
			req := httptest.NewRequest(tc.method, "http://localhost:4567"+tc.path, bodyReader)
			req.Host = "localhost"
			req.RemoteAddr = "127.0.0.1:12345"
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			nonLoopSrv.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "needs the token of the portal") {
				t.Fatalf("expected 403 authentication token required for %s %s on 0.0.0.0 without token, got %d: %s", tc.method, tc.path, rec.Code, rec.Body.String())
			}
		}

		// Configure FOREMANKIT_PORTAL_TOKEN; missing or wrong token still gets 403, matching Bearer token succeeds
		t.Setenv("FOREMANKIT_PORTAL_TOKEN", "portal-secret-token-123")

		reqMissing := httptest.NewRequest(http.MethodGet, "http://localhost:4567/api/dashboard", nil)
		reqMissing.Host = "localhost"
		reqMissing.RemoteAddr = "127.0.0.1:12345"
		recMissing := httptest.NewRecorder()
		nonLoopSrv.ServeHTTP(recMissing, reqMissing)
		if recMissing.Code != http.StatusForbidden {
			t.Fatalf("expected 403 when token env is set but header is missing, got %d", recMissing.Code)
		}

		reqGetOK := httptest.NewRequest(http.MethodGet, "http://localhost:4567/api/dashboard", nil)
		reqGetOK.Host = "localhost"
		reqGetOK.RemoteAddr = "127.0.0.1:12345"
		reqGetOK.Header.Set("Authorization", "Bearer portal-secret-token-123")
		recGetOK := httptest.NewRecorder()
		nonLoopSrv.ServeHTTP(recGetOK, reqGetOK)
		if recGetOK.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for GET /api/dashboard on 0.0.0.0 with valid Bearer token, got %d: %s", recGetOK.Code, recGetOK.Body.String())
		}

		reqPostOK := httptest.NewRequest(http.MethodPost, "http://localhost:4567/api/workspace/pause", strings.NewReader(`{"reason":"authed"}`))
		reqPostOK.Host = "localhost:4567"
		reqPostOK.RemoteAddr = "192.168.1.50:54321"
		reqPostOK.Header.Set("Content-Type", "application/json")
		reqPostOK.Header.Set("Origin", "http://localhost:4567")
		reqPostOK.Header.Set("Authorization", "Bearer portal-secret-token-123")
		recPostOK := httptest.NewRecorder()
		nonLoopSrv.ServeHTTP(recPostOK, reqPostOK)
		if recPostOK.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for POST /api/workspace/pause on 0.0.0.0 with valid Bearer token, got %d: %s", recPostOK.Code, recPostOK.Body.String())
		}
		_, _, _, _ = doReq(t, srv, http.MethodPost, "/api/workspace/resume", nil)
	}
}
