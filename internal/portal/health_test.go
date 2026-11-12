package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/store"
)

func TestHealthEndpoint(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	mux := srv.router()
	ctx := context.Background()

	before, err := st.RowsWritten(ctx)
	if err != nil {
		t.Fatal(err)
	}

	code, body, raw, hdr := doReq(t, mux, http.MethodGet, "/healthz", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /healthz = %d %s; want 200", code, raw)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("GET /healthz Content-Type = %q; want application/json", ct)
	}
	if cc := hdr.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("GET /healthz Cache-Control = %q; want no-store", cc)
	}
	var got health
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("GET /healthz invalid JSON %q: %v", raw, err)
	}
	want := health{
		Status:        "ok",
		Version:       assets.Version,
		Commit:        assets.SourceCommit,
		SchemaVersion: store.SchemaVersion(),
	}
	if got != want {
		t.Errorf("GET /healthz = %+v; want %+v", got, want)
	}
	if body["status"] != "ok" || body["version"] != assets.Version || body["commit"] != assets.SourceCommit || body["schema_version"] != float64(store.SchemaVersion()) {
		t.Errorf("GET /healthz body = %v; want %+v", body, want)
	}

	code, _, _, hdr = doReq(t, mux, http.MethodHead, "/healthz", nil)
	if code != http.StatusOK {
		t.Errorf("HEAD /healthz = %d; want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("HEAD /healthz Content-Type = %q; want application/json", ct)
	}
	if cc := hdr.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("HEAD /healthz Cache-Control = %q; want no-store", cc)
	}

	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		code, body, raw, hdr := doReq(t, mux, method, "/healthz", nil)
		if code != http.StatusMethodNotAllowed {
			t.Errorf("%s /healthz = %d %s; want 405", method, code, raw)
		}
		if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s /healthz Content-Type = %q; want application/json", method, ct)
		}
		if allow := hdr.Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("%s /healthz Allow = %q; want GET, HEAD", method, allow)
		}
		if errCode, errMsg := errorOf(body); errCode != "method_not_allowed" || errMsg == "" {
			t.Errorf("%s /healthz error = (%q, %q) in %s; want method_not_allowed", method, errCode, errMsg, raw)
		}
	}

	after, err := st.RowsWritten(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("/healthz wrote %d row(s) to the database", after-before)
	}
}
