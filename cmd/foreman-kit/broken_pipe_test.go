package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// brokenPipeHelperEnv makes the test binary run main() as `foreman-kit agent-exec` instead of the tests.
const brokenPipeHelperEnv = "FOREMANKIT_KIT_BROKEN_PIPE_HELPER"

// brokenPipeStub is the agent the helper runs: it reports that it started, and answers SIGTERM with a
// line of output, as real agent CLIs do, before it exits.
const brokenPipeStub = `#!/bin/bash
echo $$ > "$BROKEN_PIPE_DIR/stub.pid"
trap 'echo "stub: stopping"; exit 0' TERM
echo "stub: started"
while :; do sleep 0.05; done
`

// brokenPipeOutput collects the helper's stderr; the test reads it while the helper may still write.
type brokenPipeOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *brokenPipeOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *brokenPipeOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// TestBrokenStdoutPipeDoesNotAbortShutdown reproduces Ctrl-C on `foreman-kit agent-exec ... | tee log`:
// the reader of the pipe dies first, then foreman-kit gets the signal and stops its agent, whose last
// words it relays to a stdout nobody reads any more. That write used to kill foreman-kit with SIGPIPE
// before it had finished stopping the agent's process group and recording the outcome.
func TestBrokenStdoutPipeDoesNotAbortShutdown(t *testing.T) {
	if os.Getenv(brokenPipeHelperEnv) == "1" {
		dir := os.Getenv("BROKEN_PIPE_DIR")
		os.Args = []string{"foreman-kit", "agent-exec", filepath.Join(dir, "work"), filepath.Join(dir, "prompt.md"), "stub"}
		main() // exits the process
		return
	}

	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	// An agent call does not start on a .foreman-kit/agents.yaml that lacks a key.
	settingstest.WriteFiles(t, work, nil)
	stub := filepath.Join(dir, "stub.sh")
	if err := os.WriteFile(stub, []byte(brokenPipeStub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("do the work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutR.Close()
	var stderr brokenPipeOutput
	kit := exec.Command(os.Args[0], "-test.run=^TestBrokenStdoutPipeDoesNotAbortShutdown$")
	kit.Dir = work
	kit.Env = append(os.Environ(),
		brokenPipeHelperEnv+"=1",
		"BROKEN_PIPE_DIR="+dir,
		"AGENT_STUB="+stub,
		settings.RunOverridesEnv+"="+settings.EncodeOverrides(map[settings.Key]string{settings.AgentRetries: "0"}),
		"FOREMANKIT_STATE_DIR="+filepath.Join(dir, "state"),
		"FOREMANKIT_DB_PATH=",
		"FOREMANKIT_APP_ROOT=",
	)
	kit.Stdout = stdoutW
	kit.Stderr = &stderr
	if err := kit.Start(); err != nil {
		t.Fatal(err)
	}
	_ = stdoutW.Close()
	exited := make(chan error, 1)
	go func() { exited <- kit.Wait() }()
	t.Cleanup(func() {
		_ = kit.Process.Kill()
		if raw, err := os.ReadFile(filepath.Join(dir, "stub.pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	// The agent is running once foreman-kit relays its first line.
	started := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(stdoutR)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "stub: started") {
				started <- true
				return
			}
		}
		started <- false
	}()
	select {
	case ok := <-started:
		if !ok {
			t.Fatalf("foreman-kit exited before its agent started; stderr:\n%s", stderr.String())
		}
	case err := <-exited:
		t.Fatalf("foreman-kit exited before its agent started: %v; stderr:\n%s", err, stderr.String())
	case <-time.After(60 * time.Second):
		t.Fatal("the agent did not start")
	}

	// Ctrl-C: the reader of the pipe goes first, then foreman-kit is interrupted.
	_ = stdoutR.Close()
	if err := kit.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}

	select {
	case err = <-exited:
	case <-time.After(90 * time.Second):
		t.Fatalf("foreman-kit did not exit after SIGINT; stderr:\n%s", stderr.String())
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			t.Fatalf("foreman-kit was killed by %v while stopping its agent; stderr:\n%s", ws.Signal(), stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "agent_exec: engine stub exited") {
		t.Fatalf("foreman-kit exited (%v) without finishing the agent call; stderr:\n%s", err, stderr.String())
	}
}
