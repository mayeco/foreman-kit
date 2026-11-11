package proc

import (
	"bytes"
	"context"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAgentExecDoesNotRetryACommandLineTooLargeToStart gives an engine a prompt no execve accepts as
// an argument (over ARG_MAX on macOS, over the per-argument limit on Linux). The start fails the same
// way every time, so AgentExec must not wait out the retry delay to try the same command again.
func TestAgentExecDoesNotRetryACommandLineTooLargeToStart(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	settingstest.Give(t, settings.AgentRetries, "1")
	settingstest.Give(t, settings.AgentRetryAfter, "30s")

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/usr/bin/env bash\necho pong\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	started := time.Now()
	res, err := AgentExec(context.Background(), AgentExecOptions{
		Workdir:   root,
		AppRoot:   root,
		Prompt:    strings.Repeat("describe the change in detail. ", 100_000), // about 3 MB
		Engines:   "claude",
		Store:     st,
		SkipEvent: true,
		Stdout:    &out,
		Stderr:    &errOut,
	})
	elapsed := time.Since(started)
	if err != nil || res == nil || res.ExitCode == 0 {
		t.Fatalf("expected a failed agent call, got res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Output, "could not be started") || !strings.Contains(res.Output, "argument list too long") {
		t.Skipf("this system started the engine with a 3 MB argument: %q", res.Output)
	}
	if strings.Contains(res.Output, "trying claude again") || elapsed > 20*time.Second {
		t.Fatalf("the engine was tried again after %v although its command line can never start: %q", elapsed, res.Output)
	}
	if !strings.Contains(res.Output, "too large to start; not trying it again") {
		t.Fatalf("the output does not say why the engine was not tried again: %q", res.Output)
	}
}
