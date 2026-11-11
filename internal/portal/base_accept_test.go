package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// POST /api/bases/accept accepts a broken base like foreman-kit base accept, through the portal's
// request validation. Until then the base waits for a person: Needs you lists it, and Dashboard
// counts it.
func TestPortalBaseAcceptAndBrokenBasesInSummary(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	ctx := context.Background()
	t.Setenv("USER", "bob")
	sha := "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"
	if _, err := st.RecordBaseVerification(ctx, store.BaseVerification{RepoPath: "repos/core", BaseSHA: sha, CheckName: "regression", Status: store.BaseVerdictFailed, ExitCode: 1, PointerSHA: "ffff", OutsideCommits: 2}); err != nil {
		t.Fatal(err)
	}

	code, body, raw, _ := doReq(t, srv, http.MethodGet, "/api/inbox", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/inbox: %d %s", code, raw)
	}
	bases, _ := body["broken_bases"].([]any)
	if len(bases) != 1 || body["count"] != float64(1) || body["urgent"] != true {
		t.Fatalf("expected one broken base waiting for a person, got %s", raw)
	}
	if b := bases[0].(map[string]any); b["repo"] != "repos/core" || b["sha"] != sha || b["outside_commits"] != float64(2) {
		t.Fatalf("unexpected broken base %+v", b)
	}
	code, body, raw, _ = doReq(t, srv, http.MethodGet, "/api/dashboard", nil)
	if waits, _ := body["waiting"].(map[string]any); code != http.StatusOK || waits["broken_bases"] != float64(1) || waits["total"] != float64(1) {
		t.Fatalf("expected Dashboard to count the broken base among what waits: %d %s", code, raw)
	}
	// Needs you lists it with its accept action, and Dashboard says that something waits.
	code, _, page, _ := doReq(t, srv, http.MethodGet, "/inbox", nil)
	if code != http.StatusOK || !strings.Contains(page, "No change can merge into repos/core.") || !strings.Contains(page, "Accept the base of repos/core at 1a2b3c4d as it is? ") ||
		!strings.Contains(page, `data-post="/api/bases/accept" data-body='{"repo":"repos/core","sha":"`+sha+`"}'`) || strings.Contains(page, "Nothing waits for you.") {
		t.Fatalf("expected Needs you to show the broken base with its accept action (%d)", code)
	}
	if code, _, page, _ := doReq(t, srv, http.MethodGet, "/", nil); code != http.StatusOK || !strings.Contains(page, "1 base branch that fails on its own") || !strings.Contains(page, `href="/inbox"`) {
		t.Fatalf("expected Dashboard to say that a broken base waits, with the way to it (%d)", code)
	}

	// The action goes through the portal's request validation: a cross-site POST is refused.
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4567/api/bases/accept", strings.NewReader(`{"repo":"repos/core"}`))
	req.Host = "127.0.0.1:4567"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example.com")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a cross-origin POST /api/bases/accept, got %d: %s", rec.Code, rec.Body.String())
	}
	if v, _ := st.GetBaseVerification(ctx, "repos/core", sha, "regression"); v.Status != store.BaseVerdictFailed {
		t.Fatalf("a refused request accepted the base: %+v", v)
	}

	// Nothing to accept is a conflict with what is recorded, and a missing repository a bad request.
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/bases/accept", map[string]any{"repo": "repos/mobile"})
	if c, msg := errorOf(body); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, "repos/mobile is not broken") {
		t.Fatalf("expected 409 for a repository without a broken base, got %d %s", code, raw)
	}
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/bases/accept", map[string]any{"repo": "repos/core", "sha": "ffffffff"})
	if c, _ := errorOf(body); code != http.StatusConflict || c != "conflict" {
		t.Fatalf("expected 409 for a base that is not the broken one, got %d %s", code, raw)
	}
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/bases/accept", map[string]any{"sha": "1a2b3c4d"})
	if c, _ := errorOf(body); code != http.StatusBadRequest || c != "invalid_input" {
		t.Fatalf("expected 400 without a repository, got %d %s", code, raw)
	}
	if v, _ := st.GetBaseVerification(ctx, "repos/core", sha, "regression"); v.Status != store.BaseVerdictFailed {
		t.Fatalf("a refused request accepted the base: %+v", v)
	}

	// The answer is the base that was accepted, and how many of its verdicts.
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/bases/accept", map[string]any{"repo": "core", "sha": "1a2b3c4d"})
	if code != http.StatusOK || body["verdicts"] != float64(1) || body["sha"] != sha || body["repo"] != "repos/core" {
		t.Fatalf("POST /api/bases/accept: %d %s", code, raw)
	}
	// Accepted once: a second time there is nothing left to accept.
	if code, _, raw, _ := doReq(t, srv, http.MethodPost, "/api/bases/accept", map[string]any{"repo": "core", "sha": "1a2b3c4d"}); code != http.StatusConflict {
		t.Fatalf("expected 409 when accepting the same base again, got %d %s", code, raw)
	}
	v, _ := st.GetBaseVerification(ctx, "repos/core", sha, "regression")
	if v.Status != store.BaseVerdictAccepted || v.AcceptedBy != "bob" {
		t.Fatalf("expected the verdict accepted by bob, got %+v", v)
	}
	evs, _ := st.QueryEvents(ctx, store.EventFilter{Event: "merge.base_accepted"})
	if len(evs) != 1 || evs[0].Payload["repo"] != "repos/core" {
		t.Fatalf("expected one merge.base_accepted, got %+v", evs)
	}
	_, body, raw, _ = doReq(t, srv, http.MethodGet, "/api/inbox", nil)
	if bases, _ := body["broken_bases"].([]any); len(bases) != 0 || body["count"] != float64(0) {
		t.Fatalf("expected nothing left for a person, got %s", raw)
	}
}
