package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCobraCLIInitVerifyStatusConfigAndEvaluatorContract(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	appDir := filepath.Join(tmp, "sample-app")
	t.Setenv("FOREMANKIT_STATE_DIR", stateDir)
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_APP_ROOT", appDir)

	ctx := context.Background()

	// 1. foreman-kit version
	var out, errBuf bytes.Buffer
	if code := Run(ctx, []string{"version"}, &out, &errBuf); code != 0 {
		t.Fatalf("version failed (%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "foreman-kit v") {
		t.Fatalf("unexpected version output: %s", out.String())
	}

	// 2. foreman-kit init <appDir> --agents claude
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"init", appDir, "--agents", "claude"}, &out, &errBuf); code != 0 {
		t.Fatalf("init failed (%d): stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}

	// 3. foreman-kit verify <appDir>
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"verify", appDir}, &out, &errBuf); code != 0 {
		t.Fatalf("verify failed (%d): stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}

	// 4. foreman-kit db path
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"db", "path", appDir}, &out, &errBuf); code != 0 {
		t.Fatalf("db path failed (%d): %s", code, errBuf.String())
	}
	if !strings.HasSuffix(strings.TrimSpace(out.String()), ".db") {
		t.Fatalf("expected .db path, got: %s", out.String())
	}

	// 5. foreman-kit settings get dispatch.workers
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"settings", "get", "dispatch.workers"}, &out, &errBuf); code != 0 {
		t.Fatalf("settings get failed (%d): %s", code, errBuf.String())
	}
	if strings.TrimSpace(out.String()) != "2" {
		t.Fatalf("expected dispatch.workers=2, got %q", strings.TrimSpace(out.String()))
	}

	// 6. foreman-kit config set pipeline.dispatch.workers 4
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"config", "set", "pipeline.dispatch.workers", "4"}, &out, &errBuf); code != 0 {
		t.Fatalf("config set failed (%d): stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}
	out.Reset()
	if code := Run(ctx, []string{"settings", "get", "dispatch.workers"}, &out, &errBuf); code != 0 || strings.TrimSpace(out.String()) != "4" {
		t.Fatalf("expected updated dispatch.workers=4, got %q", strings.TrimSpace(out.String()))
	}

	// 7. foreman-kit evaluator-contract (offline, without the external jev binary)
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"evaluator-contract"}, &out, &errBuf); code != 0 {
		t.Fatalf("evaluator-contract failed (%d): %s\n%s", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "evaluator contract: PASS") {
		t.Fatalf("expected evaluator contract: PASS, got:\n%s", out.String())
	}

	// 8. foreman-kit status & progress --json
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"status"}, &out, &errBuf); code != 0 {
		t.Fatalf("status failed (%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "== Guards") {
		t.Fatalf("expected == Guards in status output, got:\n%s", out.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"progress", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("progress --json failed (%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `"dashboard"`) {
		t.Fatalf("expected JSON progress output, got:\n%s", out.String())
	}
}
