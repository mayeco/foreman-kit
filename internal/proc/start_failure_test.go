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
)

// TestRepoExecSaysWhyACommandDidNotStart runs `foreman-kit exec <repo> <command>` with a command
// that cannot be executed. The caller must be told why, not only handed an exit code: the same
// happens when the environment is too large for execve (a long FOREMANKIT_CHANGED_FILES), where
// nothing else explains a gate that "fails" without output.
func TestRepoExecSaysWhyACommandDidNotStart(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	repo := filepath.Join(root, "repos", "web")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	res, err := RepoExec(context.Background(), RepoExecOptions{Root: root, Store: st, Stdout: &out, Stderr: &errOut},
		"web", []string{"foreman-no-such-command", "--flag"})
	if err == nil || res == nil || res.ExitCode != 127 {
		t.Fatalf("expected exit 127 and an error for a missing command, got res=%+v err=%v", res, err)
	}
	if !strings.Contains(errOut.String(), "repo_exec: cannot start the command") || !strings.Contains(errOut.String(), "foreman-no-such-command") {
		t.Fatalf("the reason the command did not start was not reported: stderr %q", errOut.String())
	}
}

// TestRepoExecCLIForwardsPipedStdin pipes input into `foreman-kit exec <repo> cat`, with and
// without an env_script: the command must read it, as `exec "$@"` in bin/repo_exec.sh did
// (`foreman-kit exec backend psql < seed.sql`).
func TestRepoExecCLIForwardsPipedStdin(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	for _, tc := range []struct{ name, config string }{
		{"plain", ""},
		{"env_script", "env_script: script/env.sh\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(root, "repos", tc.name)
			if err := os.MkdirAll(filepath.Join(repo, "script"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "script", "env.sh"), []byte("export FROM_ENV=1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "repo.config.yaml"), []byte(tc.config), 0o644); err != nil {
				t.Fatal(err)
			}

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.WriteString("piped input\n"); err != nil {
				t.Fatal(err)
			}
			w.Close()
			defer r.Close()
			oldStdin := os.Stdin
			os.Stdin = r
			defer func() { os.Stdin = oldStdin }()

			var out, errOut bytes.Buffer
			code := RunRepoExecCLI(context.Background(), root, st, []string{tc.name, "cat"}, &out, &errOut)
			if code != 0 {
				t.Fatalf("exit %d, stderr %q", code, errOut.String())
			}
			if out.String() != "piped input\n" {
				t.Fatalf("the command did not read the piped stdin: stdout %q", out.String())
			}
		})
	}
}

// TestForwardableStdinSkipsATerminal keeps a character device (a terminal, /dev/null) from being
// handed on: the command runs in its own process group, where reading the terminal stops it.
func TestForwardableStdinSkipsATerminal(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	if got := forwardableStdin(devNull); got != nil {
		t.Fatalf("a character device was forwarded: %v", got)
	}
	if got := forwardableStdin(nil); got != nil {
		t.Fatalf("a nil file was forwarded: %v", got)
	}
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := forwardableStdin(f); got != f {
		t.Fatalf("a regular file was not forwarded")
	}
}

// TestAgentExecSaysWhyAnEngineDidNotStart gives AgentExec an engine whose file exists on PATH but
// cannot be executed. The output must carry the reason next to the exit code.
func TestAgentExecSaysWhyAnEngineDidNotStart(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	settingstest.Give(t, settings.AgentRetries, "0")

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	// Executable by its mode, but neither a binary nor a script with an interpreter line.
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("\x00\x01\x02 not a program\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	res, err := AgentExec(context.Background(), AgentExecOptions{
		Workdir:   root,
		AppRoot:   root,
		Prompt:    "noop",
		Engines:   "claude",
		Store:     st,
		SkipEvent: true,
		Stdout:    &out,
		Stderr:    &errOut,
	})
	if err != nil || res == nil || res.ExitCode == 0 {
		t.Fatalf("expected a failed agent call, got res=%+v err=%v", res, err)
	}
	for _, text := range []string{res.Output, errOut.String()} {
		if !strings.Contains(text, "agent_exec: claude could not be started") {
			t.Fatalf("the reason the engine did not start was not reported: %q", text)
		}
	}
}
