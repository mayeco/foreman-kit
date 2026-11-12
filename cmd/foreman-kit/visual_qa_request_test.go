package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/portal"
	"github.com/mayeco/foreman-kit/internal/store"
)

func TestVisualQARequestChangeNameValidation(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	appDir := filepath.Join(tmp, "sample-app")
	t.Setenv("FOREMANKIT_STATE_DIR", stateDir)
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_APP_ROOT", appDir)

	ctx := context.Background()
	var out, errBuf bytes.Buffer
	if code := Run(ctx, []string{"init", appDir, "--agents", "claude"}, &out, &errBuf); code != 0 {
		t.Fatalf("init failed (%d): stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}

	// Status before the refused request.
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"visual-qa", "status"}, &out, &errBuf); code != 0 {
		t.Fatalf("status before failed (%d): %s", code, errBuf.String())
	}
	statusBefore := out.String()

	// 1. Invalid change name "../etc" is refused by the CLI.
	out.Reset()
	errBuf.Reset()
	code := Run(ctx, []string{"visual-qa", "request", "probe", "--change", "../etc"}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("expected non-zero exit code for invalid change name ../etc, got 0 (stdout=%q stderr=%q)", out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), `"../etc" is not the name of a change`) {
		t.Fatalf("expected stderr refusal naming ../etc, got %q", errBuf.String())
	}
	if strings.Contains(out.String(), "queued") {
		t.Fatalf("expected no queued output on stdout, got %q", out.String())
	}

	// Status after the refused request is unchanged.
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"visual-qa", "status"}, &out, &errBuf); code != 0 {
		t.Fatalf("status after failed (%d): %s", code, errBuf.String())
	}
	if out.String() != statusBefore {
		t.Fatalf("expected status unchanged (%q), got %q", statusBefore, out.String())
	}

	st, err := store.Open(appDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	items, err := st.ListVisualQA(ctx, "")
	if err != nil {
		t.Fatalf("ListVisualQA: %v", err)
	}
	for _, it := range items {
		if it.ChangeName == "../etc" {
			t.Fatalf("store unexpectedly holds visual QA item for ../etc: %+v", it)
		}
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items after refused request, got %d", len(items))
	}

	// Portal and CLI refuse the same invalid change names and leave no row.
	srv, err := portal.New(appDir, st, portalKit(st))
	if err != nil {
		t.Fatalf("portal.New: %v", err)
	}
	for _, bad := range []string{"../etc", "Not A Change"} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4567/api/visual-qa/items",
			strings.NewReader(`{"what":"probe","change_name":"`+bad+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected portal 400 for %q, got %d: %s", bad, rec.Code, rec.Body.String())
		}
		out.Reset()
		errBuf.Reset()
		if c := Run(ctx, []string{"visual-qa", "request", "probe", "--change", bad}, &out, &errBuf); c == 0 {
			t.Fatalf("expected CLI non-zero exit for %q, got 0", bad)
		}
	}
	if after, err := st.ListVisualQA(ctx, ""); err != nil || len(after) != 0 {
		t.Fatalf("expected 0 items after portal and CLI refusals, got %v (%v)", after, err)
	}

	// 2. Valid change name "add-billing" with visual QA enabled queues.
	out.Reset()
	errBuf.Reset()
	code = Run(ctx, []string{"visual-qa", "request", "check the totals", "--change", "add-billing"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("expected exit 0 for valid change name add-billing, got %d (stdout=%q stderr=%q)", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "visual-qa: request 1 queued") {
		t.Fatalf("expected stdout 'visual-qa: request 1 queued', got %q", out.String())
	}
	pending, err := st.ListVisualQA(ctx, "pending")
	if err != nil {
		t.Fatalf("ListVisualQA pending: %v", err)
	}
	if len(pending) != 1 || pending[0].ChangeName != "add-billing" || pending[0].What != "check the totals" {
		t.Fatalf("expected 1 pending item for add-billing, got %+v", pending)
	}

	// 3. Request without --change queues with an empty change name.
	out.Reset()
	errBuf.Reset()
	code = Run(ctx, []string{"visual-qa", "request", "the footer shows the year"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("expected exit 0 for request without --change, got %d (stdout=%q stderr=%q)", code, out.String(), errBuf.String())
	}
	pending, err = st.ListVisualQA(ctx, "pending")
	if err != nil {
		t.Fatalf("ListVisualQA pending: %v", err)
	}
	if len(pending) != 2 || pending[1].ChangeName != "" || pending[1].What != "the footer shows the year" {
		t.Fatalf("expected second pending item with empty change name, got %+v", pending)
	}
}
