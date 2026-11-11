package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// seedPortal writes a backlog, a proposal, a worktree with artifacts, a quarantine record, a ready
// change, a cycle, an event, an agent run and a job, so every page has something to render.
func seedPortal(t *testing.T, srv *Server, st *store.Store, root string) (runID, jobID string) {
	t.Helper()
	ctx := context.Background()
	_ = cycle.WriteJSONList(filepath.Join(root, cycle.BacklogFile), []map[string]any{
		{"change_name": "feat-alpha", "priority": "high", "description": "Alpha feature", "depends_on": []string{"feat-zero"}},
	})
	_ = cycle.WriteProposals(root, []map[string]any{
		{"change_name": "prop-beta", "status": "proposed", "source": "roadmap", "description": "Beta proposal"},
	})
	// The pages read the mirror of these files, which the portal brings up to date when it starts.
	srv.syncBacklog(ctx)
	wtChangeDir := filepath.Join(root, ".worktrees", "worker-feat-alpha", "openspec", "changes", "feat-alpha")
	_ = os.MkdirAll(wtChangeDir, 0o755)
	_ = os.WriteFile(filepath.Join(wtChangeDir, "proposal.md"), []byte("# Proposal feat-alpha\n\n- [x] done\n- [ ] todo\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wtChangeDir, "tasks.md"), []byte("- [x] 1.1 Task one\n- [ ] 1.2 Task two\n"), 0o644)
	_ = st.BlockChange(ctx, store.QuarantineRecord{ChangeName: "blocked-gamma", Reason: "gate_failure", Details: "lint failed"})
	_ = st.EnqueueReady(ctx, store.ReadyManifest{
		ChangeName: "ready-delta",
		Branch:     "feat/spec-ready-delta",
		Worktree:   filepath.Join(root, ".worktrees", "worker-ready-delta"),
		HeadSHA:    "1234567890abcdef1234567890abcdef12345678",
	})
	_ = st.CreateExecution(ctx, store.Execution{ExecID: "cycle-20261108T120000Z", Kind: "cycle", Agent: "claude", Status: "succeeded", StartedAt: "2026-11-08T12:00:00Z"})
	_ = st.AppendLogChunk(ctx, "cycle-20261108T120000Z", "combined", "== cycle 20261108T120000Z\npreflight: OK\nstrategy: api-contracts (defects)\n== done\n")
	_ = st.EmitEvent(ctx, store.EventInput{Event: "worker.started", Change: "feat-alpha", Engine: "claude", Payload: map[string]any{"slot": 1}})
	for i, phase := range []string{"plan", "build", "build-repair-1"} {
		id := "worker-feat-alpha-" + phase
		_ = st.CreateExecution(ctx, store.Execution{ExecID: id, Kind: "worker", ChangeName: "feat-alpha", Agent: "claude", Phase: phase,
			Status: "succeeded", PromptText: "# " + phase + " prompt\n\nBuild **feat-alpha**.\n", StartedAt: time.Date(2026, 11, 8, 10, i, 0, 0, time.UTC).Format(time.RFC3339)})
		_ = st.AppendLogChunk(ctx, id, "combined", phase+" output line\n")
	}

	run, err := srv.exec.StartAgent(ctx, AgentRun{Engine: "claude", Prompt: "Summarize the open changes"})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	job, err := srv.exec.StartOperation(ctx, "dispatch", "")
	if err != nil {
		t.Fatalf("StartOperation: %v", err)
	}
	for _, id := range []string{run.ExecID, job.ExecID} {
		waitExecution(t, st, id)
		waitWork(t, srv.exec, id)
	}
	return run.ExecID, job.ExecID
}

func getPage(t *testing.T, srv http.Handler, path string, cookies ...*http.Cookie) (int, string, http.Header) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4567"+path, nil)
	req.Host = "127.0.0.1:4567"
	req.RemoteAddr = "127.0.0.1:12345"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), rec.Header()
}

func TestPortalPagesRender(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	runID, jobID := seedPortal(t, srv, st, root)

	pages := []string{
		"/", "/recent", "/inbox", "/inbox?kind=proposals",
		"/board",
		"/changes", "/changes?state=building", "/changes?state=blocked&q=gamma", "/changes?page=2",
		"/changes/feat-alpha", "/changes/feat-alpha?limit=1", "/changes/feat-alpha/files", "/changes/feat-alpha/files/proposal.md",
		"/changes/feat-alpha/files/tasks.md?whole=1", "/changes/feat-alpha/events", "/changes/feat-alpha/events?event=worker.started",
		"/changes/feat-alpha/intervene", "/changes/feat-alpha/intervene?from=plan", "/changes/feat-alpha/intervene?from=build",
		"/changes/feat-alpha/intervene?from=worker-feat-alpha-plan",
		"/changes/blocked-gamma", "/changes/blocked-gamma/files", "/changes/blocked-gamma/intervene", "/changes/ready-delta",
		"/executions", "/executions?kind=cycle", "/executions?change=feat-alpha&status=succeeded", "/executions/new",
		"/executions/cycle-20261108T120000Z", "/executions/worker-feat-alpha-build", "/executions/" + url.PathEscape(runID), "/executions/" + url.PathEscape(jobID),
		"/backlog", "/backlog/proposals", "/backlog/proposals?status=rejected", "/merge",
		"/cycles", "/cycles/20261108T120000Z",
		"/visual", "/visual?status=pending", "/visual/sessions",
		"/events", "/events?event=worker.started", "/events?change=feat-alpha", "/events/1",
		"/agents",
		"/quality", "/quality?since=24h", "/quality?since=all",
		"/system/health", "/system/settings", "/system/settings?file=cycle", "/system/recommendations", "/system/setup",
		"/system/prompts", "/system/prompts/plan", "/system/prompts/build?view=sample", "/system/processes",
		"/system/appearance", "/system/appearance?compare=dracula",
	}
	for _, p := range pages {
		code, body, hdr := getPage(t, srv, p)
		if code != http.StatusOK {
			t.Fatalf("GET %s: status %d\n%s", p, code, body)
		}
		if !strings.Contains(hdr.Get("Content-Type"), "text/html") || !strings.HasPrefix(body, "<!doctype html>") {
			t.Fatalf("GET %s: not an HTML page (%s)", p, hdr.Get("Content-Type"))
		}
		if strings.Contains(body, "could not be rendered") || strings.Contains(body, "Something broke") {
			t.Fatalf("GET %s: render error\n%s", p, body)
		}
		if !strings.Contains(body, `data-theme="openspec"`) || !strings.Contains(body, `id="nav-menu"`) {
			t.Fatalf("GET %s: missing layout", p)
		}
	}
}

func TestPortalPageRoutingAndThemes(t *testing.T) {
	srv, _, _ := setupTestPortal(t)

	// Unknown pages and unknown records render the 404 page inside the layout.
	for _, p := range []string{"/nope", "/changes/does-not-exist", "/executions/missing", "/nope/again", "/cycles/20200101T000000Z",
		"/cycles/20261108T120000Z/log", "/events/999999", "/events/first", "/system/prompts/zzz"} {
		code, body, _ := getPage(t, srv, p)
		if code != http.StatusNotFound || !strings.Contains(body, "Signal lost") {
			t.Fatalf("GET %s: want the 404 page, got %d", p, code)
		}
	}

	// Section roots and trailing slashes redirect to a canonical address.
	for p, want := range map[string]string{"/system": "/system/health", "/changes/?q=a": "/changes?q=a", "/go?q=x": "/changes?q=x", "/go": "/changes"} {
		code, _, hdr := getPage(t, srv, p)
		if code != http.StatusSeeOther && code != http.StatusMovedPermanently {
			t.Fatalf("GET %s: want a redirect, got %d", p, code)
		}
		if got := hdr.Get("Location"); got != want {
			t.Fatalf("GET %s: redirect to %q, want %q", p, got, want)
		}
	}

	// The theme cookie picks the theme; an unknown one falls back to the default.
	_, body, _ := getPage(t, srv, "/", &http.Cookie{Name: themeCookie, Value: "dracula"})
	if !strings.Contains(body, `data-theme="dracula"`) || !strings.Contains(body, `value="dracula" checked`) {
		t.Fatalf("theme cookie dracula was not applied")
	}
	_, body, _ = getPage(t, srv, "/", &http.Cookie{Name: themeCookie, Value: `x" onload="alert(1)`})
	if !strings.Contains(body, `data-theme="openspec"`) {
		t.Fatalf("an invalid theme cookie must fall back to the default theme")
	}

	// Pages follow the API's host rule (DNS rebinding).
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/", nil)
	req.Host = "evil.example"
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a foreign Host must be refused, got %d", rec.Code)
	}

	// Static assets: the icon sprite, the favicon and the fonts are embedded.
	for p, ctype := range map[string]string{
		"/assets/icons.svg":   "image/svg+xml",
		"/assets/favicon.svg": "image/svg+xml",
		"/assets/fonts/chakra-petch-latin-700-normal.woff2": "font/woff2",
	} {
		code, body, hdr := getPage(t, srv, p)
		if code != http.StatusOK || hdr.Get("Content-Type") != ctype || len(body) < 100 {
			t.Fatalf("GET %s: code=%d type=%s len=%d", p, code, hdr.Get("Content-Type"), len(body))
		}
	}
	if code, _, _ := getPage(t, srv, "/assets/fonts/../portal.css"); code == http.StatusOK {
		t.Fatalf("font paths must not escape the fonts directory")
	}
}
