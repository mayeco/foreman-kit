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

// TestAgentCallInAWorktreeCutBeforeTheUpdate: a change that was being built when `foreman-kit
// update` rewrote the files keeps the former ones in its worktree. A call made there runs the
// command the application writes for its agent today, with the arguments and the limit of the
// role, and not the command the kit ships for an agent whose file says nothing.
func TestAgentCallInAWorktreeCutBeforeTheUpdate(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	ctx := context.Background()

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	argsLog := filepath.Join(t.TempDir(), "invoked_args.txt")
	ours := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > " + argsLog + "\necho done\n"
	if err := os.WriteFile(filepath.Join(binDir, "our-runner"), []byte(ours), 0o755); err != nil {
		t.Fatal(err)
	}
	// The command the kit ships for claude must not run: a stand-in that says so, in the place of
	// the real CLI.
	shipped := "#!/usr/bin/env bash\necho 'the shipped command ran' >&2\nexit 99\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(shipped), 0o755); err != nil {
		t.Fatal(err)
	}

	settingstest.WriteFile(t, root, "agents", `agents: [claude]
roles:
  build:
    agents: [claude]
    timeout: 20m
    args: {claude: "--model our-model"}
permissions: full
commands:
  claude:
    run: "our-runner --timeout={{timeout}} {{args}} {{prompt}}"
`)
	worktree := filepath.Join(root, ".worktrees", "worker-add-search")
	settingstest.WriteFormerFiles(t, worktree)
	if why := settings.Unusable(worktree, settings.AgentCallFiles...); why != "" {
		t.Fatalf("a call in the worktree would not start: %s", why)
	}

	var out, errOut bytes.Buffer
	res, err := AgentExec(ctx, AgentExecOptions{
		Workdir: worktree,
		Prompt:  "Go on with the change",
		Engines: "claude",
		Role:    "build",
		Store:   st,
		Stdout:  &out,
		Stderr:  &errOut,
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("the call did not run the application's command: res=%+v err=%v stderr=%s", res, err, errOut.String())
	}
	got, _ := os.ReadFile(argsLog)
	if want := "--timeout=1200\n--model\nour-model\nGo on with the change\n"; string(got) != want {
		t.Fatalf("the command did not run as the main checkout writes it; arguments:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(errOut.String(), "reading it as") {
		t.Fatalf("a setting was read as the value the kit ships:\n%s", errOut.String())
	}
}
