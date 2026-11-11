package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openSpecCLIStandIn puts an `openspec` that runs script first on PATH and returns a directory the
// default runner takes for an OpenSpec workspace.
func openSpecCLIStandIn(t *testing.T, script string) string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "openspec"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "")

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "openspec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "openspec", "config.yaml"), []byte("schema: spec-driven\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := openSpecCLIWaitDelay
	openSpecCLIWaitDelay = 300 * time.Millisecond
	t.Cleanup(func() { openSpecCLIWaitDelay = prev })
	return dir
}

// The `openspec` CLI is read through a pipe. A helper it left running with that pipe inherited kept the
// runner waiting after the CLI itself had answered and exited: the worker or the merger that asked sat
// there for as long as the helper lived.
func TestDefaultOpenSpecRunnerDoesNotWaitForAHelperTheCLILeftRunning(t *testing.T) {
	dir := openSpecCLIStandIn(t, `echo '{"valid":true}'
sleep 5 &
exit 0`)

	started := time.Now()
	res, err := DefaultOpenSpecRunner()(context.Background(), dir, "validate", "some-change", "--json")
	took := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if took > 3*time.Second {
		t.Fatalf("the runner waited %s for a helper the CLI left running", took.Round(time.Millisecond))
	}
	if !res.OK || res.ExitCode != 0 || !strings.Contains(res.Output, `{"valid":true}`) {
		t.Fatalf("the CLI answered and exited 0, got %+v", res)
	}
}

// A CLI that fails still reports its own exit status and output when a helper outlives it.
func TestDefaultOpenSpecRunnerKeepsTheFailureOfACLIWhoseHelperOutlivesIt(t *testing.T) {
	dir := openSpecCLIStandIn(t, `echo '{"valid":false}'
sleep 5 &
exit 3`)

	started := time.Now()
	res, err := DefaultOpenSpecRunner()(context.Background(), dir, "validate", "some-change", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("the runner waited %s for a helper the CLI left running", took.Round(time.Millisecond))
	}
	if res.OK || res.ExitCode != 3 || !strings.Contains(res.Output, `{"valid":false}`) {
		t.Fatalf("the CLI exited 3 with its verdict, got %+v", res)
	}
}
