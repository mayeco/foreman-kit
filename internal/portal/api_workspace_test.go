package portal

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/orchestrator"
	"github.com/mayeco/foreman-kit/internal/store"
)

// TestPausingAndResumingTheKit: a pause is opened with its reason and closed by resuming, one action
// each. Pausing what is paused and resuming what is not are refused as a conflict, and change nothing.
func TestPausingAndResumingTheKit(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	ctx := context.Background()
	paused := func() bool {
		t.Helper()
		ws, err := st.GetWorkspaceState(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return ws.Paused
	}

	code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/workspace/resume", map[string]any{})
	if c, msg := errorOf(data); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, "not paused") {
		t.Errorf("resuming a kit that is not paused = %d %s", code, raw)
	}

	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/workspace/pause", map[string]any{"reason": "  a gate is being looked at  "})
	if code != http.StatusOK || data["reason"] != "a gate is being looked at" || data["paused_by"] != "portal" || data["paused_at"] == nil {
		t.Fatalf("pausing = %d %s", code, raw)
	}
	if !paused() {
		t.Fatal("the kit is not paused after the pause was opened")
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/workspace/pause", map[string]any{"reason": "again"})
	if c, _ := errorOf(data); code != http.StatusConflict || c != "conflict" {
		t.Errorf("pausing a kit that is paused = %d %s", code, raw)
	}
	if ws, _ := st.GetWorkspaceState(ctx); ws.PauseReason != "a gate is being looked at" {
		t.Errorf("a second pause replaced the reason of the first: %q", ws.PauseReason)
	}

	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/workspace/resume", map[string]any{})
	if code != http.StatusOK || data["paused"] != false || paused() {
		t.Fatalf("resuming = %d %s", code, raw)
	}

	// A pause with no reason says where it was put from.
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/workspace/pause", nil)
	if code != http.StatusOK || data["reason"] != "paused from the portal" {
		t.Errorf("a pause with no reason = %d %s", code, raw)
	}
	// An action takes the fields it declares and no other.
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/workspace/resume", map[string]any{"drain_merge": true})
	if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" || !paused() {
		t.Errorf("resuming with a field the action does not take = %d %s", code, raw)
	}
}

// TestStartingTheWorkOfTheKit: the dispatcher, the merger and the gates start as an execution a
// person can follow; a cycle starts and goes on by itself; a simulation of the dispatch answers with
// what the dispatcher says and starts nothing.
func TestStartingTheWorkOfTheKit(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	kit := srv.kit.(*fakeKit)

	for address, operation := range map[string]string{"/api/dispatcher/runs": "dispatch", "/api/merger/runs": "merge", "/api/gates/runs": "gates"} {
		code, data, raw, _ := doReq(t, srv, http.MethodPost, address, map[string]any{})
		id, ex := startedOf(t, code, data, raw)
		if ex["kind"] != "portal_job" || ex["job_kind"] != operation {
			t.Errorf("POST %s started %v, want the operation %s", address, ex, operation)
		}
		waitExecution(t, st, id)
		waitWork(t, srv.exec, id)
		if code, data, raw, _ := doReq(t, srv, http.MethodPost, address, map[string]any{"change": "add-api"}); code != http.StatusBadRequest {
			t.Errorf("POST %s with a field it does not take = %d %s %v", address, code, raw, data)
		}
	}

	// A simulation: what the dispatcher says, with the instant it said it.
	asked := 0
	kit.simulate = func(context.Context, string) (*orchestrator.DryRunReport, string, error) {
		asked++
		return &orchestrator.DryRunReport{FreeWorkers: 1, Items: []orchestrator.DryRunItem{
			{Change: "add-ui", Priority: "normal", CountsAs: "normal", State: "waiting", Why: "waiting on dependencies: add-api"},
			{Change: "add-search", Priority: "critical", CountsAs: "critical", State: "ready", Why: "ready to start", Next: true},
		}}, "dispatch: not running\n  add-search  ready    ready to start  <- next\n  add-ui      waiting  waiting on dependencies: add-api", nil
	}
	before, err := st.RowsWritten(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/dispatcher/simulations", map[string]any{})
	report, _ := data["report"].(map[string]any)
	items, _ := report["items"].([]any)
	output, _ := data["output"].(string)
	if code != http.StatusOK || asked != 1 || len(items) != 2 || data["computed_at"] == nil ||
		!strings.HasPrefix(output, "computed at ") || !strings.Contains(output, "add-search  ready") {
		t.Fatalf("POST /api/dispatcher/simulations = %d %s", code, raw)
	}
	if after, err := st.RowsWritten(context.Background()); err != nil || after != before {
		t.Errorf("a simulation of the dispatch wrote %d row(s) (err %v)", after-before, err)
	}
	// A dispatcher that cannot say is a conflict with what it answered, not a failure of the portal.
	kit.simulate = func(context.Context, string) (*orchestrator.DryRunReport, string, error) {
		return nil, "", errors.New("the evaluator has no API key for the gates that need one")
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/dispatcher/simulations", map[string]any{})
	if c, msg := errorOf(data); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, "API key") {
		t.Errorf("a simulation the dispatcher cannot make = %d %s", code, raw)
	}

	// A cycle: started, and on its own from there.
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/cycles", map[string]any{})
	if _, said := data["output"].(string); code != http.StatusAccepted || !said {
		t.Errorf("POST /api/cycles = %d %s", code, raw)
	}
}

// TestTheBacklogIsEditedThroughItsFile: adding a change writes it to the backlog file and the pages
// know it at once; taking it out leaves it as a change that is in no queue. What cannot be added or
// taken out is refused with the reason, and the file stays as it was.
func TestTheBacklogIsEditedThroughItsFile(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	ctx := context.Background()
	file := filepath.Join(root, cycle.BacklogFile)
	inFile := func(name string) bool {
		t.Helper()
		items, err := cycle.ReadJSONList(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if asStr(it["change_name"]) == name {
				return true
			}
		}
		return false
	}
	state := func(name string) string {
		t.Helper()
		s, err := st.ChangeState(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	// What cannot be added, each with its reason.
	for _, tc := range []struct {
		why    string
		in     map[string]any
		status int
		says   string
	}{
		{"a name that is not one", map[string]any{"change_name": "Add Cart", "description": "A cart"}, http.StatusBadRequest, "not a name for a change"},
		{"no description", map[string]any{"change_name": "add-cart"}, http.StatusBadRequest, "needs a description"},
		{"a priority that is not one", map[string]any{"change_name": "add-cart", "description": "A cart", "priority": "urgent"}, http.StatusBadRequest, "not a priority"},
		{"a dependency on itself", map[string]any{"change_name": "add-cart", "description": "A cart", "depends_on": "add-cart"}, http.StatusBadRequest, "depend on itself"},
		{"a field the action does not take", map[string]any{"change_name": "add-cart", "description": "A cart", "workdir": "/tmp"}, http.StatusBadRequest, "not the JSON"},
		{"a change that is already queued", map[string]any{"change_name": "add-ui", "description": "Again"}, http.StatusConflict, "already in the backlog"},
	} {
		code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/backlog", tc.in)
		if _, msg := errorOf(data); code != tc.status || !strings.Contains(msg, tc.says) {
			t.Errorf("adding %s = %d %s; want %d saying %q", tc.why, code, raw, tc.status, tc.says)
		}
	}
	if now, _ := os.ReadFile(file); string(now) != string(before) {
		t.Fatalf("a refused edit rewrote the backlog:\n%s", now)
	}

	// Added: in the file, queued in the registry, and on the page, with what it depends on.
	code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/backlog", map[string]any{
		"change_name": "add-cart", "priority": "high", "description": "  A cart for the widgets  ", "depends_on": "add-api, add-ui,add-api",
	})
	deps, _ := data["depends_on"].([]any)
	if code != http.StatusCreated || data["change_name"] != "add-cart" || data["description"] != "A cart for the widgets" || data["queued_at"] == nil ||
		len(deps) != 2 || deps[0] != "add-api" || deps[1] != "add-ui" {
		t.Fatalf("POST /api/backlog = %d %s", code, raw)
	}
	if !inFile("add-cart") || state("add-cart") != store.ChangeQueued {
		t.Fatalf("after adding it, add-cart is in the file: %v, and its state is %q", inFile("add-cart"), state("add-cart"))
	}
	_, body := overviewPage(t, srv, "/backlog")
	wantOnPage(t, "the backlog after a change was added", body, `href="/changes/add-cart"`, "A cart for the widgets", `data-post="/api/backlog/add-cart"`)
	// A list of names is taken as a list too.
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/backlog", map[string]any{"change_name": "add-wishlist", "description": "A wishlist", "depends_on": []string{"add-cart"}})
	if deps, _ := data["depends_on"].([]any); code != http.StatusCreated || len(deps) != 1 || deps[0] != "add-cart" {
		t.Errorf("POST /api/backlog with a list of dependencies = %d %s", code, raw)
	}

	// Taken out: only a change that waits with nothing open.
	code, data, raw, _ = doReq(t, srv, http.MethodDelete, "/api/backlog/no-such-change", nil)
	if c, _ := errorOf(data); code != http.StatusNotFound || c != "not_found" {
		t.Errorf("taking out a change the backlog does not list = %d %s", code, raw)
	}
	for _, busy := range []string{"add-search", "add-billing", "add-api"} {
		code, data, raw, _ = doReq(t, srv, http.MethodDelete, "/api/backlog/"+busy, nil)
		if c, msg := errorOf(data); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, "nothing open") || !inFile(busy) {
			t.Errorf("taking out %s, which is %s = %d %s", busy, state(busy), code, raw)
		}
	}
	code, data, raw, _ = doReq(t, srv, http.MethodDelete, "/api/backlog/add-cart", nil)
	if code != http.StatusOK || data["change_name"] != "add-cart" || data["in_backlog"] != false {
		t.Fatalf("DELETE /api/backlog/add-cart = %d %s", code, raw)
	}
	if inFile("add-cart") || state("add-cart") != store.ChangeUnknown {
		t.Fatalf("after taking it out, add-cart is in the file: %v, and its state is %q, want it out of the queue", inFile("add-cart"), state("add-cart"))
	}
	// It is off the queue; the change that depends on it says what became of it.
	_, body = overviewPage(t, srv, "/backlog")
	wantNotOnPage(t, "the backlog after the change was taken out", body, `data-post="/api/backlog/add-cart"`, "A cart for the widgets")
	wantOnPage(t, "the backlog after the change was taken out", body, `href="/changes/add-wishlist"`, "It depends on", "which is not in the backlog.")
	// It is a change the kit knows and that is in no queue, and it can be queued again.
	_, body = overviewPage(t, srv, "/changes?q=add-cart")
	wantOnPage(t, "the list of changes after the change was taken out", body, `href="/changes/add-cart"`, "not in the backlog")
	if code, _, raw, _ := doReq(t, srv, http.MethodPost, "/api/backlog", map[string]any{"change_name": "add-cart", "description": "A cart, again"}); code != http.StatusCreated || state("add-cart") != store.ChangeQueued {
		t.Errorf("queueing again a change that was taken out = %d %s, state %q", code, raw, state("add-cart"))
	}

	// A change the kit finished keeps its name: it is not queued a second time under it.
	if err := st.MarkChangeIntegrated(ctx, "add-done", "abc123"); err != nil {
		t.Fatal(err)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/backlog", map[string]any{"change_name": "add-done", "description": "Again"})
	if c, msg := errorOf(data); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, "integrated") || inFile("add-done") {
		t.Errorf("queueing a change that is integrated = %d %s", code, raw)
	}
}

// TestProposalsAreDecidedOnce: approving or rejecting a proposal records the decision in the
// proposals file, and Needs you stops listing it. A proposal that does not exist, or that is past a
// decision, is refused.
func TestProposalsAreDecidedOnce(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	if err := cycle.WriteProposals(root, []map[string]any{
		{"change_name": "add-cache", "status": "proposed", "priority": "high", "source": "exploration", "description": "Cache the feed of widgets", "proposed_at": "2026-11-04T08:00:00Z"},
		{"change_name": "add-feed", "status": "proposed", "description": "A feed", "proposed_at": "2026-11-04T09:00:00Z"},
		{"change_name": "add-old", "status": "rejected", "description": "An old idea", "proposed_at": "2026-10-01T09:00:00Z"},
		{"change_name": "add-landed", "status": "integrated", "description": "It landed", "proposed_at": "2026-10-02T09:00:00Z"},
	}); err != nil {
		t.Fatal(err)
	}
	srv.syncBacklog(context.Background())
	status := func(name string) string {
		t.Helper()
		items, err := cycle.ReadProposals(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if asStr(it["change_name"]) == name {
				return asStr(it["status"])
			}
		}
		return ""
	}

	code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/proposals/add-cache/approve", map[string]any{})
	if code != http.StatusOK || data["id"] != "add-cache" || data["status"] != "approved" || status("add-cache") != "approved" {
		t.Fatalf("approving add-cache = %d %s, in the file %q", code, raw, status("add-cache"))
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/proposals/add-feed/reject", map[string]any{})
	if code != http.StatusOK || data["status"] != "rejected" || status("add-feed") != "rejected" {
		t.Fatalf("rejecting add-feed = %d %s, in the file %q", code, raw, status("add-feed"))
	}
	// The pages know at once: nothing waits for a decision, and each list has its proposals.
	_, body := overviewPage(t, srv, "/inbox")
	wantNotOnPage(t, "Needs you after the proposals were decided", body, "Proposals to decide", "add-cache", "add-feed")
	_, body = overviewPage(t, srv, "/backlog/proposals?status=approved")
	wantOnPage(t, "the approved proposals", body, "add-cache", "approved", `data-post="/api/proposals/add-cache/reject"`)
	wantNotOnPage(t, "the approved proposals", body, "add-feed", `data-post="/api/proposals/add-cache/approve"`)

	// An approval can still be withdrawn; a decision on what is past one cannot be taken.
	if code, _, raw, _ := doReq(t, srv, http.MethodPost, "/api/proposals/add-cache/reject", map[string]any{}); code != http.StatusOK || status("add-cache") != "rejected" {
		t.Errorf("rejecting an approved proposal = %d %s", code, raw)
	}
	for _, past := range []string{"add-old", "add-landed", "add-feed"} {
		code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/proposals/"+past+"/approve", map[string]any{})
		if c, msg := errorOf(data); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, "not waiting for a decision") {
			t.Errorf("approving %s, which is %s = %d %s", past, status(past), code, raw)
		}
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/proposals/no-such-proposal/approve", map[string]any{})
	if c, _ := errorOf(data); code != http.StatusNotFound || c != "not_found" {
		t.Errorf("approving a proposal that does not exist = %d %s", code, raw)
	}
}

// TestABacklogThatIsMissingIsSaidAndIsNotWrittenOver: the file of the backlog of the main checkout
// reads as an empty queue when it is gone, with no error. Dashboard and Backlog say what the kit
// found in its place, their reads say it too, and the portal changes nothing of a backlog that is
// missing: the edit would write a file with nothing of what the one that is gone held.
func TestABacklogThatIsMissingIsSaidAndIsNotWrittenOver(t *testing.T) {
	srv, _, root := setupTestPortal(t)
	path := filepath.Join(root, filepath.FromSlash(cycle.BacklogFile))
	held, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the portal of the test has no backlog file: %v", err)
	}
	problem := func(t *testing.T) string {
		t.Helper()
		var said []string
		for _, address := range []string{"/api/dashboard", "/api/backlog"} {
			code, data, raw, _ := doReq(t, srv, http.MethodGet, address, nil)
			text, isText := data["backlog_problem"].(string)
			if code != http.StatusOK || !isText {
				t.Fatalf("GET %s = %d, backlog_problem = %v in %s", address, code, data["backlog_problem"], raw)
			}
			said = append(said, text)
		}
		if said[0] != said[1] {
			t.Errorf("Dashboard says the backlog is %q and Backlog says %q", said[0], said[1])
		}
		return said[0]
	}
	notice := "The backlog of the main checkout is"

	// The file is there: nothing is said of it.
	if got := problem(t); got != "" {
		t.Errorf("with its file there the backlog is said to be %q", got)
	}
	for _, address := range []string{"/", "/backlog"} {
		_, body := overviewPage(t, srv, address)
		wantNotOnPage(t, address+" with the backlog there", body, notice)
	}

	// The file is gone.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := problem(t); got != "missing" {
		t.Errorf("a backlog that is gone is said to be %q, want missing", got)
	}
	_, body := overviewPage(t, srv, "/")
	wantOnPage(t, "Dashboard with the backlog gone", body, notice+" missing", cycle.BacklogFile, "the dispatcher starts nothing", `href="/backlog"`)
	_, body = overviewPage(t, srv, "/backlog")
	wantOnPage(t, "Backlog with its file gone", body, notice+" missing", cycle.BacklogFile, "git status", "git restore "+cycle.BacklogFile)
	// Adding a change would write a backlog that holds that change alone.
	code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/backlog", map[string]any{"change_name": "add-late", "description": "Asked while the backlog was gone"})
	if c, m := errorOf(data); code != http.StatusConflict || c != "conflict" || !strings.Contains(m, "missing") || !strings.Contains(m, cycle.BacklogFile) {
		t.Errorf("adding to a backlog that is gone = %d %s", code, raw)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the edit that was refused left a backlog file behind: %v", err)
	}

	// The file holds something that is no list.
	if err := os.WriteFile(path, []byte("{\"change_name\": \"add-cart\""), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := problem(t); !strings.HasPrefix(got, "not a list the kit can read") {
		t.Errorf("a backlog that is no list is said to be %q", got)
	}
	_, body = overviewPage(t, srv, "/backlog")
	wantOnPage(t, "Backlog with a file that is no list", body, notice+" not a list the kit can read")

	// The file is back: nothing is said any more.
	if err := os.WriteFile(path, held, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := problem(t); got != "" {
		t.Errorf("with its file back the backlog is said to be %q", got)
	}
	_, body = overviewPage(t, srv, "/")
	wantNotOnPage(t, "Dashboard with the backlog back", body, notice)
}

// TestNoSyncFromThePortalDuringAVisualQASession: neither a cycle nor the dispatcher moves the main
// checkout while a visual QA session works in it, and the sync itself does not look. The action of
// the portal does: it answers 409 and names the session, alone or with what it publishes, and syncs
// again once the session has ended.
func TestNoSyncFromThePortalDuringAVisualQASession(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	ctx := context.Background()
	const stamp = "20261111T090000Z"
	if _, err := st.AcquireLock(ctx, "visual_qa", store.LockOptions{OwnerType: "visual_qa", PID: os.Getpid(), Stamp: stamp, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	for _, publish := range []bool{false, true} {
		code, body, problem := post(t, srv, "/api/repos/sync", map[string]any{"publish": publish})
		if _, message := errorOf(body); code != http.StatusConflict || problem != "conflict" || !strings.Contains(message, stamp) {
			t.Errorf("a sync during a visual QA session (publish %v) = %d %v, want 409 with the session named", publish, code, body)
		}
	}

	if err := st.ReleaseLock(ctx, "visual_qa", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	code, body, _ := post(t, srv, "/api/repos/sync", map[string]any{"publish": false})
	if said := asStr(body["output"]); code != http.StatusOK || !strings.Contains(said, "Umbrella") {
		t.Errorf("a sync once the session has ended = %d %v, want what the sync said", code, body)
	}
}
