package proc

import (
	"bytes"
	"context"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// outputCaptureGoneWriter fails every write the way a stdout does once its reader went away
// (`foreman-kit gates | head`, a full disk behind a redirection, a closed terminal).
type outputCaptureGoneWriter struct{ writes int }

func (w *outputCaptureGoneWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, syscall.EPIPE
}

// TestAgentExecReadsEngineOutputWhenCallerWriterFails runs an engine that reports a usage limit while
// the caller's stdout is gone. AgentExec must still see what the engine printed: it decides from that
// text that the engine is out of quota, and returns it as the call's output.
func TestAgentExecReadsEngineOutputWhenCallerWriterFails(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	settingstest.Give(t, settings.AgentRetries, "0")

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	engine := "#!/usr/bin/env bash\necho 'You have hit your usage limit for today.'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(engine), 0o755); err != nil {
		t.Fatal(err)
	}

	gone := &outputCaptureGoneWriter{}
	var errBuf bytes.Buffer
	res, err := AgentExec(context.Background(), AgentExecOptions{
		Workdir:   root,
		AppRoot:   root,
		Prompt:    "noop",
		Engines:   "claude",
		Store:     st,
		SkipEvent: true,
		Stdout:    gone,
		Stderr:    &errBuf,
	})
	if err != nil || res == nil {
		t.Fatalf("AgentExec failed: res=%+v err=%v", res, err)
	}
	if gone.writes == 0 {
		t.Fatal("the engine's output never reached the caller's writer: the test exercises nothing")
	}
	if !strings.Contains(res.Output, "hit your usage limit") {
		t.Fatalf("the engine's output is missing from the result: %q", res.Output)
	}
	if !strings.Contains(res.Output, "out of quota") {
		t.Fatalf("the usage limit the engine reported was not recognized: %q", res.Output)
	}
}

// TestRunGatesAndPreflightKeepOutputWhenCallerWriterFails checks the same for the two commands that
// return their output: what they print still reaches the result when the caller's writer fails.
func TestRunGatesAndPreflightKeepOutputWhenCallerWriterFails(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	ctx := context.Background()

	gone := &outputCaptureGoneWriter{}
	gates, err := RunGates(ctx, GatesOptions{Root: root, Store: st, Stdout: gone, Stderr: &bytes.Buffer{}})
	if err != nil || gates == nil {
		t.Fatalf("RunGates failed: res=%+v err=%v", gates, err)
	}
	if gone.writes == 0 || !strings.Contains(gates.Output, "gates: PASS") {
		t.Fatalf("RunGates lost its output (writes to the caller: %d): %q", gone.writes, gates.Output)
	}

	gone = &outputCaptureGoneWriter{}
	pre, err := RunPreflight(ctx, PreflightOptions{Root: root, ServicesOnly: true, Store: st, Stdout: gone})
	if err != nil || pre == nil {
		t.Fatalf("RunPreflight failed: res=%+v err=%v", pre, err)
	}
	if gone.writes == 0 || !strings.Contains(pre.Output, "preflight: OK (services)") {
		t.Fatalf("RunPreflight lost its output (writes to the caller: %d): %q", gone.writes, pre.Output)
	}
}
