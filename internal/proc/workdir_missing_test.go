package proc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestRunBoundedNamesAWorkdirThatIsMissing runs a command that exists in a directory that does not,
// and in a path that is a file: the error names the directory and the exit code is not 127, which
// would read as a command that is not there.
func TestRunBoundedNamesAWorkdirThatIsMissing(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "worker-typo")
	file := filepath.Join(base, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, dir, want string }{
		{"missing", missing, "does not exist"},
		{"file", file, "is not a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			res, err := RunBounded(context.Background(), RunOptions{Dir: tc.dir, Stdout: &out, Stderr: &out}, "sh", "-c", "echo ran")
			if err == nil || res == nil {
				t.Fatalf("expected an error and a result, got res=%+v err=%v", res, err)
			}
			if !strings.Contains(err.Error(), tc.dir) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the error does not name the directory and why: %v", err)
			}
			if res.ExitCode == 127 || res.ExitCode == 0 {
				t.Fatalf("exit code %d for a work directory that cannot be used", res.ExitCode)
			}
			if out.Len() != 0 {
				t.Fatalf("the command ran: %q", out.String())
			}
		})
	}
}

// TestAgentExecWithAMissingWorkdirRecordsNoEngineHealth calls an installed agent in a directory that
// is gone. The call does not start, the output names the directory, and no engine health row is
// written: a not_installed row would hold every agent of the role off for an hour.
func TestAgentExecWithAMissingWorkdirRecordsNoEngineHealth(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	settingstest.Give(t, settings.AgentRetries, "0")

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/usr/bin/env bash\necho pong\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(root, ".worktrees", "worker-typo")
	var out, errOut bytes.Buffer
	res, err := AgentExec(context.Background(), AgentExecOptions{
		Workdir:   missing,
		AppRoot:   root,
		Prompt:    "noop",
		Engines:   "claude",
		Store:     st,
		SkipEvent: true,
		Stdout:    &out,
		Stderr:    &errOut,
	})
	if res == nil || res.ExitCode == 0 || res.ExitCode == ExitNotInstalled {
		t.Fatalf("expected a failed call that is not exit 127, got res=%+v err=%v", res, err)
	}
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("the error does not name the directory: %v", err)
	}
	for _, text := range []string{res.Output, errOut.String()} {
		if !strings.Contains(text, missing) || !strings.Contains(text, "does not exist") {
			t.Fatalf("the output does not name the missing directory: %q", text)
		}
	}
	if strings.Contains(res.Output, "not installed") || out.Len() != 0 {
		t.Fatalf("the call was taken for a missing agent or ran: %q / %q", res.Output, out.String())
	}
	health, herr := st.ListEngineHealth(context.Background())
	if herr != nil {
		t.Fatal(herr)
	}
	if rec, ok := health["claude"]; ok {
		t.Fatalf("an engine health row was written for a call that never started: %+v", rec)
	}
	for name, rec := range health {
		if rec.Status == "not_installed" {
			t.Fatalf("%s is recorded not_installed: %+v", name, rec)
		}
	}
}

// TestAgentExecStillRecordsAMissingBinaryAsNotInstalled keeps the other half: an agent whose binary
// is not there is exit 127 and a not_installed row, in a work directory that exists.
func TestAgentExecStillRecordsAMissingBinaryAsNotInstalled(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	settingstest.Give(t, settings.AgentRetries, "0")

	var out, errOut bytes.Buffer
	res, _ := AgentExec(context.Background(), AgentExecOptions{
		Workdir:   root,
		AppRoot:   root,
		Prompt:    "noop",
		Engines:   "foreman-no-such-agent",
		Store:     st,
		SkipEvent: true,
		Stdout:    &out,
		Stderr:    &errOut,
	})
	if res == nil || res.ExitCode != ExitNotInstalled {
		t.Fatalf("expected exit 127 for a missing binary, got %+v", res)
	}
	health, err := st.ListEngineHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := health["foreman-no-such-agent"]
	if !ok || rec.Status != "not_installed" || rec.CooldownUntil == 0 {
		t.Fatalf("a missing binary is not recorded not_installed with a cooldown: %+v (found=%v)", rec, ok)
	}
}
