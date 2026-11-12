package portal

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// post sends an action and returns its status, its body and the code of its error, if it is one.
func post(t *testing.T, srv http.Handler, path string, payload map[string]any) (int, map[string]any, string) {
	t.Helper()
	if payload == nil {
		payload = map[string]any{}
	}
	code, body, _, _ := doReq(t, srv, http.MethodPost, path, payload)
	errCode, _ := errorOf(body)
	return code, body, errCode
}

// refuses fails unless the action answers with the status and the code of the problem expected.
func refuses(t *testing.T, srv http.Handler, path string, payload map[string]any, status int, problem string) {
	t.Helper()
	code, body, errCode := post(t, srv, path, payload)
	_, message := errorOf(body)
	if code != status || errCode != problem || message == "" {
		t.Errorf("POST %s = %d %v; want %d %s with a message", path, code, body, status, problem)
	}
}

// TestTheActionsOfAChange: each action about a change does one thing, answers with the change as it
// is recorded afterwards, refuses what it cannot read (400), says what does not exist (404) and
// what the state of the change does not allow (409), and then changes nothing.
func TestTheActionsOfAChange(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedChanges(t, st, root)
	ctx := context.Background()
	quarantine := func(change string) *store.QuarantineRecord {
		t.Helper()
		q, err := st.GetQuarantine(ctx, change)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}

	// What every action of a change refuses the same way.
	for _, action := range []string{"unblock", "risk-approval", "reclaim", "validate", "gates", "reset", "prompts"} {
		payload := map[string]any{}
		if action == "prompts" {
			payload = map[string]any{"engine": "claude", "prompt": "x"}
		}
		refuses(t, srv, "/api/changes/no-such-change/"+action, payload, http.StatusNotFound, "not_found")
		refuses(t, srv, "/api/changes/Not_A_Name/"+action, payload, http.StatusBadRequest, "invalid_input")
		refuses(t, srv, "/api/changes/add-billing/"+action, map[string]any{"force": true}, http.StatusBadRequest, "invalid_input")
		if code, _, _, hdr := doReq(t, srv, http.MethodGet, "/api/changes/add-billing/"+action, nil); code != http.StatusMethodNotAllowed || hdr.Get("Allow") != "POST" {
			t.Errorf("GET /api/changes/add-billing/%s = %d (Allow %q), want 405 with Allow: POST", action, code, hdr.Get("Allow"))
		}
	}
	if q := quarantine("add-billing"); q == nil || q.Status != "blocked" {
		t.Fatalf("a refused action changed add-billing: %+v", q)
	}

	// 17. Validate: the specs of the worktree, by the checks of the dispatcher. It changes nothing.
	code, body, _ := post(t, srv, "/api/changes/add-search/validate", nil)
	if code != http.StatusOK || body["ok"] != true || len(body["problems"].([]any)) != 0 || !strings.Contains(asStr(body["output"]), "are valid") {
		t.Errorf("validating a change whose specs are valid = %d %v", code, body)
	}
	code, body, _ = post(t, srv, "/api/changes/add-billing/validate", nil)
	problems, _ := body["problems"].([]any)
	if code != http.StatusOK || body["ok"] != false || len(problems) != 1 || !strings.Contains(asStr(problems[0]), "tasks.md is missing") {
		t.Errorf("validating a change with no task list = %d %v", code, body)
	}
	spec := filepath.Join(root, ".worktrees", "worker-add-search", "openspec", "changes", "add-search", "specs", "search", "spec.md")
	if err := os.WriteFile(spec, []byte(strings.Replace(searchSpec, "#### Scenario:", "### Scenario:", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, body, _ = post(t, srv, "/api/changes/add-search/validate", nil); code != http.StatusOK || body["ok"] != false || !strings.Contains(asStr(body["output"]), "search") {
		t.Errorf("validating a spec whose scenario has three hashes = %d %v", code, body)
	}
	if err := os.WriteFile(spec, []byte(searchSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	refuses(t, srv, "/api/changes/add-ui/validate", nil, http.StatusConflict, "conflict")

	// 20. A prompt to an agent in the worktree, and 18, the gates in the worktree.
	var ranIn string
	kitOf(srv).agent = func(_ context.Context, workdir, engine, prompt string) (string, int, error) {
		ranIn = workdir
		return engine + " did: " + prompt, 0, nil
	}
	code, body, raw, _ := doReq(t, srv, http.MethodPost, "/api/changes/add-search/prompts", map[string]any{"engine": "hermes", "prompt": "fix the failing test"})
	promptID, ex := startedOf(t, code, body, raw)
	if ex["change_name"] != "add-search" || ex["kind"] != "portal_run" || !strings.HasSuffix(asStr(ex["workdir"]), "worker-add-search") {
		t.Errorf("the prompt to the worktree started %v", ex)
	}
	waitExecution(t, st, promptID)
	waitWork(t, srv.exec, promptID)
	if !strings.HasSuffix(ranIn, "worker-add-search") {
		t.Errorf("the agent ran in %q, want the worktree of add-search", ranIn)
	}
	refuses(t, srv, "/api/changes/add-search/prompts", map[string]any{"engine": "hermes", "prompt": "  "}, http.StatusBadRequest, "invalid_input")
	refuses(t, srv, "/api/changes/add-search/prompts", map[string]any{"engine": "hermes", "prompt": strings.Repeat("x", maxPromptBytes+1)}, http.StatusBadRequest, "invalid_input")
	refuses(t, srv, "/api/changes/add-search/prompts", map[string]any{"engine": "no-such-engine", "prompt": "x"}, http.StatusBadRequest, "invalid_input")
	refuses(t, srv, "/api/changes/add-ui/prompts", map[string]any{"engine": "hermes", "prompt": "x"}, http.StatusConflict, "conflict")

	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/changes/add-search/gates", map[string]any{})
	gatesID, ex := startedOf(t, code, body, raw)
	if ex["job_kind"] != "gates-add-search" || ex["change_name"] != "add-search" || !strings.HasSuffix(asStr(ex["workdir"]), "worker-add-search") {
		t.Errorf("the gates of the worktree started %v", ex)
	}
	waitExecution(t, st, gatesID)
	waitWork(t, srv.exec, gatesID)
	refuses(t, srv, "/api/changes/add-ui/gates", nil, http.StatusConflict, "conflict")
	// The story of the change has both.
	_, story, _, _ := doReq(t, srv, http.MethodGet, "/api/changes/add-search", nil)
	sent := 0
	for _, it := range story["items"].([]any) {
		if ep := it.(map[string]any); ep["kind"] == "execution" {
			sent++
		}
	}
	if sent != 2 {
		t.Errorf("the story of add-search has %d executions a person started, want the prompt and the gates", sent)
	}

	// 15. Accepting the risk: it is recorded, and a block for another reason stays.
	code, body, _ = post(t, srv, "/api/changes/add-billing/risk-approval", nil)
	if code != http.StatusOK || body["state"] != "blocked" || body["change_name"] != "add-billing" {
		t.Errorf("accepting the risk of a change blocked by a gate = %d %v; want it still blocked", code, body)
	}
	if q := quarantine("add-billing"); q == nil || !q.ApprovedRisk || q.Status != "blocked" {
		t.Errorf("after the risk of add-billing was accepted: %+v", q)
	}
	if err := st.BlockChange(ctx, store.QuarantineRecord{ChangeName: "add-ui", Reason: "risk_review", Details: "it touches a migration"}); err != nil {
		t.Fatal(err)
	}
	code, body, _ = post(t, srv, "/api/changes/add-ui/risk-approval", nil)
	if code != http.StatusOK || body["state"] != "queued" || body["block"] != nil {
		t.Errorf("accepting the risk a change was blocked for = %d %v; want its block lifted", code, body)
	}
	if err := st.CreateExecution(ctx, store.Execution{ExecID: "old-plan", Kind: "worker", ChangeName: "add-done", Phase: "plan", Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(root, ".foreman-kit", "agent_history.json")
	if err := os.WriteFile(history, []byte(`[{"change_name":"add-done","integrated_at":"2026-11-01T10:00:00Z","commit":"c0ffee1","description":"Done"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncJSONToSQLite(ctx, root); err != nil {
		t.Fatal(err)
	}
	refuses(t, srv, "/api/changes/add-done/risk-approval", nil, http.StatusConflict, "conflict")

	// 14. Unblock: the block in force and the question of the agent, with the counters kept or not.
	question := filepath.Join(root, ".worktrees", "worker-add-billing", "openspec", "changes", "add-billing", ".needs-human")
	code, body, _ = post(t, srv, "/api/changes/add-billing/unblock", nil)
	if code != http.StatusOK || body["state"] == "blocked" || body["block"] != nil || asStr(body["needs_human"]) != "" {
		t.Errorf("unblocking add-billing = %d %v", code, body)
	}
	if q := quarantine("add-billing"); q == nil || q.Status == "blocked" || !q.ApprovedRisk {
		t.Errorf("after add-billing was unblocked with its counters kept: %+v; want the block lifted and the accepted risk kept", q)
	}
	if isFile(question) {
		t.Errorf("unblocking left the question of the agent in the worktree")
	}
	refuses(t, srv, "/api/changes/add-billing/unblock", nil, http.StatusConflict, "conflict")
	refuses(t, srv, "/api/changes/add-search/unblock", map[string]any{"reset_retries": true}, http.StatusConflict, "conflict")
	// A question alone stops a change too, and is lifted the same way.
	if err := os.WriteFile(question, []byte("And the taxes?\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, body, _ = post(t, srv, "/api/changes/add-billing/unblock", map[string]any{"reset_retries": true}); code != http.StatusOK || isFile(question) {
		t.Errorf("unblocking a change an agent asked a question about = %d %v", code, body)
	}
	if q := quarantine("add-billing"); q != nil && (q.ApprovedRisk || q.Retries != 0) {
		t.Errorf("after the counters of add-billing started over: %+v; want no accepted risk and no retries", q)
	}

	// 16. Reclaim: the lease of a worker that is gone.
	refuses(t, srv, "/api/changes/add-export/reclaim", nil, http.StatusConflict, "conflict")
	key := store.WorkerLeaseKey("add-export")
	if _, err := st.AcquireLock(ctx, key, store.LockOptions{OwnerType: "worker", PID: os.Getpid(), TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	refuses(t, srv, "/api/changes/add-export/reclaim", nil, http.StatusConflict, "conflict")
	if lease, _ := st.GetLock(ctx, key); lease == nil {
		t.Fatal("the lease of a live worker was given back")
	}
	if err := st.ReleaseLock(ctx, key, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireLock(ctx, key, store.LockOptions{OwnerType: "worker", PID: gone.Process.Pid, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if code, body, _ = post(t, srv, "/api/changes/add-export/reclaim", nil); code != http.StatusOK || body["change_name"] != "add-export" {
		t.Errorf("giving back the lease of a dead worker = %d %v", code, body)
	}
	if lease, _ := st.GetLock(ctx, key); lease != nil {
		t.Errorf("the lease of a dead worker is still held: %+v", lease)
	}

	// 19. Reset: what the worker built is removed; never under a live worker.
	searchKey := store.WorkerLeaseKey("add-search")
	if _, err := st.AcquireLock(ctx, searchKey, store.LockOptions{OwnerType: "worker", PID: os.Getpid(), TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	refuses(t, srv, "/api/changes/add-search/reset", nil, http.StatusConflict, "conflict")
	if !isDir(filepath.Join(root, ".worktrees", "worker-add-search")) {
		t.Fatal("a reset that was refused removed the worktree")
	}
	if err := st.ReleaseLock(ctx, searchKey, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if code, body, _ = post(t, srv, "/api/changes/add-search/reset", nil); code != http.StatusOK || body["change_name"] != "add-search" || body["has_worktree"] == true {
		t.Errorf("resetting add-search = %d %v", code, body)
	}
	if isDir(filepath.Join(root, ".worktrees", "worker-add-search")) {
		t.Errorf("the reset left the worktree of add-search")
	}
	if lease, _ := st.GetLock(ctx, searchKey); lease != nil {
		t.Errorf("the reset kept the lease it took: %+v", lease)
	}
}

// TestAcceptingABrokenBase: POST /api/bases/accept accepts the base of a repository that fails on
// its own, once, and says so when there is nothing to accept.
func TestAcceptingABrokenBase(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	ctx := context.Background()
	if _, err := st.RecordBaseVerification(ctx, store.BaseVerification{RepoPath: "repos/core", BaseSHA: "1a2b3c4d5e6f", CheckName: "regression",
		Status: store.BaseVerdictFailed, ExitCode: 1, OutsideCommits: 2}); err != nil {
		t.Fatal(err)
	}

	refuses(t, srv, "/api/bases/accept", map[string]any{}, http.StatusBadRequest, "invalid_input")
	refuses(t, srv, "/api/bases/accept", map[string]any{"repo": "repos/core", "sha": "1a2b3c4d", "why": "because"}, http.StatusBadRequest, "invalid_input")
	refuses(t, srv, "/api/bases/accept", map[string]any{"repo": "repos/other"}, http.StatusConflict, "conflict")
	refuses(t, srv, "/api/bases/accept", map[string]any{"repo": "repos/core", "sha": "ffff"}, http.StatusConflict, "conflict")
	if broken, _ := st.ListBrokenBases(ctx); len(broken) != 1 {
		t.Fatalf("a refused request accepted the base: %+v", broken)
	}

	code, body, _ := post(t, srv, "/api/bases/accept", map[string]any{"repo": "repos/core", "sha": "1a2b3c4d"})
	if code != http.StatusOK || body["repo"] != "repos/core" || body["sha"] != "1a2b3c4d5e6f" || body["verdicts"] != float64(1) {
		t.Errorf("accepting the broken base = %d %v", code, body)
	}
	if broken, _ := st.ListBrokenBases(ctx); len(broken) != 0 {
		t.Errorf("the base is still broken after it was accepted: %+v", broken)
	}
	refuses(t, srv, "/api/bases/accept", map[string]any{"repo": "repos/core"}, http.StatusConflict, "conflict")
	if code, _, _, hdr := doReq(t, srv, http.MethodGet, "/api/bases/accept", nil); code != http.StatusMethodNotAllowed || hdr.Get("Allow") != "POST" {
		t.Errorf("GET /api/bases/accept = %d (Allow %q), want 405", code, hdr.Get("Allow"))
	}
}

func TestChangePromptLengthLimit(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedChanges(t, st, root)
	ctx := context.Background()
	called := false
	kitOf(srv).agent = func(_ context.Context, _, _, _ string) (string, int, error) {
		called = true
		return "ok", 0, nil
	}

	refuses(t, srv, "/api/changes/add-search/prompts", map[string]any{
		"engine": "hermes",
		"prompt": strings.Repeat("x", maxPromptBytes+1),
	}, http.StatusBadRequest, "invalid_input")
	if runs, err := st.ListExecutions(ctx, "portal_run", "add-search", 50); err != nil || len(runs) != 0 {
		t.Fatalf("oversized prompt to add-search left runs = %+v (err %v), want none", runs, err)
	}
	if called {
		t.Fatal("oversized prompt to add-search started the agent")
	}
}
