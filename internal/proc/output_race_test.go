package proc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// sharedBufferWriter is a distinct writer value around a shared, non-thread-safe buffer.
type sharedBufferWriter struct{ buf *bytes.Buffer }

func (w *sharedBufferWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

const interleavedOutputScript = `for i in $(seq 1 300); do echo "out-$i"; echo "err-$i" >&2; done`

// TestRunBoundedSerializesDistinctStdoutAndStderrWriters verifies that distinct Stdout and Stderr
// writers sharing state are never written concurrently, that Output carries both streams, and that
// log_chunks sequence numbers stay unique for the execution. Run with -race.
func TestRunBoundedSerializesDistinctStdoutAndStderrWriters(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	ctx := context.Background()

	execID := "exec-distinct-writers"
	if err := st.CreateExecution(ctx, store.Execution{ExecID: execID, Kind: "worker", ChangeName: "race-change", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	var shared bytes.Buffer
	res, err := RunBounded(ctx, RunOptions{
		Dir:    root,
		Stdout: &sharedBufferWriter{buf: &shared},
		Stderr: &sharedBufferWriter{buf: &shared},
		Store:  st,
		ExecID: execID,
	}, "bash", "-c", interleavedOutputScript)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("RunBounded failed: res=%+v err=%v", res, err)
	}
	for _, want := range []string{"out-300", "err-300"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("expected Output to contain %q from both streams", want)
		}
		if !strings.Contains(shared.String(), want) {
			t.Fatalf("expected caller writers to receive %q", want)
		}
	}

	chunks, err := st.ReadLogChunks(ctx, execID, 0)
	if err != nil || len(chunks) == 0 {
		t.Fatalf("expected log chunks for %s, got %d (err=%v)", execID, len(chunks), err)
	}
	// Chunks are ordered by their id: each one is recorded once, after the one before it.
	var last int64
	for _, c := range chunks {
		if c.ID <= last {
			t.Fatalf("log chunk %d of %s is not after chunk %d", c.ID, execID, last)
		}
		last = c.ID
	}
}

// TestAgentExecCapturesInterleavedStdoutAndStderr runs an engine writing heavily to both streams
// through AgentExec with one shared caller buffer, as the dispatcher does. Run with -race.
func TestAgentExecCapturesInterleavedStdoutAndStderr(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	ctx := context.Background()

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/usr/bin/env bash\n"+interleavedOutputScript+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var outBuf bytes.Buffer
	res, err := AgentExec(ctx, AgentExecOptions{
		Workdir:   root,
		AppRoot:   root,
		Prompt:    "noop",
		Engines:   "claude",
		Store:     st,
		SkipEvent: true,
		Stdout:    &outBuf,
		Stderr:    &outBuf,
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("AgentExec failed: res=%+v err=%v", res, err)
	}
	for _, want := range []string{"out-300", "err-300"} {
		if !strings.Contains(res.Output, want) || !strings.Contains(outBuf.String(), want) {
			t.Fatalf("expected %q in AgentExec output and caller buffer", want)
		}
	}
}

// TestAgentExecRejectsEngineListWithoutNames verifies that a list of separators fails cleanly
// instead of indexing an empty engine slice.
func TestAgentExecRejectsEngineListWithoutNames(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	var errBuf bytes.Buffer
	res, err := AgentExec(context.Background(), AgentExecOptions{
		Workdir: root,
		AppRoot: root,
		Prompt:  "noop",
		Engines: " , ,",
		Store:   st,
		Stdout:  &bytes.Buffer{},
		Stderr:  &errBuf,
	})
	if err == nil || res == nil || res.ExitCode != ExitNotInstalled {
		t.Fatalf("expected ExitNotInstalled and an error, got res=%+v err=%v", res, err)
	}
	if !strings.Contains(errBuf.String(), "no agent named") {
		t.Fatalf("expected a clear stderr message, got %q", errBuf.String())
	}
}
