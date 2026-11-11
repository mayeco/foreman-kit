package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// "End session" must close the visual QA session even when the browser disconnects mid-request.
func TestAuditVisualQAEndSurvivesClientDisconnect(t *testing.T) {
	root := t.TempDir()
	st, err := store.OpenAt(filepath.Join(root, "log", "openspec.db"), "ws-vqa-end-audit", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if _, err := st.AcquireLock(ctx, "visual_qa", store.LockOptions{OwnerType: "visual_qa", Stamp: "20261108T000000Z", TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	srv, _ := newTestServer(t, root, st)

	reqCtx, cancel := context.WithCancel(ctx)
	cancel() // the browser tab was closed while the request was in flight
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4567/api/visual-qa/session/end", strings.NewReader(`{}`)).WithContext(reqCtx)
	req.Host = "127.0.0.1:4567"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if stamp := cycle.ActiveVisualSession(ctx, st); stamp != "" {
		t.Fatalf("visual QA session %s still open after End (HTTP %d: %s)", stamp, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
