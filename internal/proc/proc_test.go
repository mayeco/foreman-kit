package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

func setupProcTestWorkspace(t *testing.T) (string, *store.Store) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FOREMANKIT_APP_ROOT", root)

	// Initialize git repository in root
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-b", "master"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
	} {
		if _, err := gitx.RunGit(ctx, root, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	seeds, err := assets.WalkEmbedded("seeds")
	if err != nil {
		t.Fatal(err)
	}
	for rel, b := range seeds {
		target := rel
		if rel == "gitignore" {
			target = ".gitignore"
		}
		dst := filepath.Join(root, filepath.FromSlash(target))
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		_ = os.WriteFile(dst, b, 0o644)
	}

	_ = config.Write(root, []string{"claude", "copilot"}, map[string][]string{
		"plan":  {"claude", "copilot"},
		"build": {"claude", "copilot"},
		"cycle": {"claude"},
	}, "in_order")

	_, _ = gitx.RunGit(ctx, root, "add", "-A")
	_, _ = gitx.RunGit(ctx, root, "commit", "-m", "initial")

	dbPath := filepath.Join(root, "state.db")
	t.Setenv("FOREMANKIT_DB_PATH", dbPath)
	st, err := store.OpenAt(dbPath, "proc-test-123456", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	return root, st
}

func TestRunBoundedStreamingAndTimeoutAndWatchdog(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	ctx := context.Background()

	// 1. Streaming capture into store.AppendLogChunk
	execID := "exec-stream-1"
	if err := st.CreateExecution(ctx, store.Execution{
		ExecID:     execID,
		Kind:       "worker",
		ChangeName: "stream-change",
		Status:     "running",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := RunBounded(ctx, RunOptions{
		Dir:    root,
		Store:  st,
		ExecID: execID,
	}, "bash", "-c", "echo line1; echo line2 >&2")
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("RunBounded failed: res=%+v err=%v", res, err)
	}
	fullLog, err := st.ReadFullLog(ctx, execID)
	if err != nil || !strings.Contains(fullLog, "line1") || !strings.Contains(fullLog, "line2") {
		t.Fatalf("expected streamed log chunks in SQLite, got %q (err=%v)", fullLog, err)
	}

	// 2. Wall-clock timeout with SIGTERM to process group (-pid)
	resTimeout, err := RunBounded(ctx, RunOptions{
		Dir:         root,
		Timeout:     150 * time.Millisecond,
		GracePeriod: 100 * time.Millisecond,
	}, "bash", "-c", "sleep 30")
	if err != nil {
		t.Fatalf("RunBounded timeout returned unexpected error: %v", err)
	}
	if !resTimeout.TimedOut || resTimeout.ExitCode != 124 {
		t.Fatalf("expected TimedOut=true and ExitCode=124, got %+v", resTimeout)
	}

	// 3. Idle watchdog kills stalled process
	resIdle, err := RunBounded(ctx, RunOptions{
		Dir:          root,
		Timeout:      10 * time.Second,
		IdleTimeout:  300 * time.Millisecond,
		WatchdogPoll: 100 * time.Millisecond,
		GracePeriod:  100 * time.Millisecond,
	}, "bash", "-c", "echo starting; sleep 30")
	if err != nil {
		t.Fatalf("RunBounded idle watchdog returned error: %v", err)
	}
	if !resIdle.IdleStalled || resIdle.ExitCode != 124 {
		t.Fatalf("expected IdleStalled=true and ExitCode=124, got %+v", resIdle)
	}
}

// Every agent the kit knows runs the command the kit ships for it, exactly as written: the role's
// arguments where the command says {{args}}, the prompt as one argument, and nothing else. An agent
// whose CLI has no way to hold it back has no restricted command, and cannot be used in that mode.
func TestEveryAgentRunsItsCommandsAsWritten(t *testing.T) {
	args := []string{"--model", "x"}
	for _, eng := range config.SupportedOrder {
		binName, argv, _, err := ResolveEngineCommand("", eng, "test prompt", "/tmp/prompt.md", "/tmp/work", "full", 3600, args)
		if err != nil || binName != strings.Fields(config.SUPPORTED[eng].Run)[0] {
			t.Fatalf("%s: bin=%q argv=%v err=%v", eng, binName, argv, err)
		}
		joined := strings.Join(argv, "\x00")
		if !strings.Contains(joined, "test prompt") || !strings.Contains(joined, "--model\x00x") || strings.Contains(joined, "{{") {
			t.Errorf("%s: the prompt, the role's arguments or a placeholder: %q", eng, argv)
		}
		for _, arg := range argv {
			if arg == "" {
				t.Errorf("%s: an empty argument in %q", eng, argv)
			}
		}

		_, restricted, _, err := ResolveEngineCommand("", eng, "test prompt", "/tmp/prompt.md", "/tmp/work", "restricted", 3600, nil)
		if config.SUPPORTED[eng].Restricted == "" {
			if err == nil || !strings.Contains(err.Error(), eng+" has no restricted command") {
				t.Errorf("%s has no restricted command and ran %q (err %v)", eng, restricted, err)
			}
			continue
		}
		if err != nil || len(restricted) == 0 {
			t.Errorf("%s restricted: %v, %v", eng, restricted, err)
		}
		// Without arguments for the role, {{args}} leaves nothing behind.
		for _, arg := range restricted {
			if arg == "" || strings.Contains(arg, "{{") {
				t.Errorf("%s restricted: %q", eng, restricted)
			}
		}
	}

	// What the binary used to add on its own is in the command now, word for word.
	_, claude, _, err := ResolveEngineCommand("", "claude", "do it", "/tmp/p.md", "/tmp/work", "full", 1800, []string{"--model", "opus"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "do it", "--output-format", "json", "--no-session-persistence", "--permission-mode", "bypassPermissions", "--max-budget-usd", "5",
		"--disallowedTools", "Bash(git commit *) Bash(git push *) Bash(git rebase *) Bash(git reset *) Bash(git checkout *) Bash(git switch *) Bash(rm -rf *)", "--model", "opus"}
	if strings.Join(claude, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("claude runs:\n%q\nwant:\n%q", claude, want)
	}
	for eng, wantArgv := range map[string][]string{
		"codex": {"exec", "--dangerously-bypass-approvals-and-sandbox", "--model", "opus", "do it"},
		"pi":    {"-p", "--no-session", "--approve", "--model", "opus", "--", "do it"},
		"agy":   {"-p", "do it", "--print-timeout", "1800s", "--dangerously-skip-permissions", "--model", "opus"},
		"cline": {"--yolo", "--cwd", "/tmp/work", "--model", "opus", "do it"},
	} {
		_, got, _, err := ResolveEngineCommand("", eng, "do it", "/tmp/p.md", "/tmp/work", "full", 1800, []string{"--model", "opus"})
		if err != nil || strings.Join(got, "\x00") != strings.Join(wantArgv, "\x00") {
			t.Errorf("%s runs %q (err %v), want %q", eng, got, err, wantArgv)
		}
	}
	if _, _, closeStdin, _ := ResolveEngineCommand("", "crush", "p", "", "/w", "full", 60, nil); !closeStdin {
		t.Error("crush runs with its standard input open")
	}
	// An agent nobody gave commands cannot run: nothing is made up for it.
	if _, _, _, err := ResolveEngineCommand("", "some-binary", "p", "", "/w", "full", 60, nil); err == nil || !strings.Contains(err.Error(), "some-binary has no commands") {
		t.Errorf("an agent without commands: %v", err)
	}
	// A command without {{args}} gets the role's arguments after its last word.
	_, tail, _, err := ExpandCommand(config.AgentCommandSpec{}, "tool run {{prompt_file}}", "tool", "p", "/tmp/p.md", "/w", 60, []string{"-m", "x"})
	if err != nil || strings.Join(tail, " ") != "run /tmp/p.md -m x" {
		t.Errorf("a command without {{args}}: %q, %v", tail, err)
	}
}

func TestAgentExecQuotaFallbackContinuationAndExit75(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	ctx := context.Background()

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	settingstest.Give(t, settings.AgentRetries, "1")
	settingstest.Give(t, settings.AgentRetryAfter, "0s")

	// Case 1: Both engines fail without touching the worktree -> exit 75 (ENGINES_DOWN)
	writeFakeBin := func(name, body string) {
		p := filepath.Join(binDir, name)
		if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFakeBin("claude", `echo "You have hit your usage limit for today" >&2; exit 1`)
	writeFakeBin("hermes", `echo "Authentication failed: not logged in" >&2; exit 1`)

	var outBuf, errBuf bytes.Buffer
	res75, err := AgentExec(ctx, AgentExecOptions{
		Workdir: root,
		AppRoot: root,
		Prompt:  "Build feature X",
		Engines: "claude,hermes",
		Role:    "plan",
		Change:  "feat-x",
		Phase:   "plan",
		Store:   st,
		Stdout:  &outBuf,
		Stderr:  &errBuf,
	})
	if err != nil {
		t.Fatalf("AgentExec returned error: %v", err)
	}
	if res75.ExitCode != ExitEnginesDown {
		t.Fatalf("expected ExitCode=75 when no engine could work, got %d (stderr=%s)", res75.ExitCode, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "out of quota or over a usage limit") {
		t.Fatalf("expected quota detection message, got:\n%s", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "authentication or provider configuration failure") {
		t.Fatalf("expected auth failure detection message, got:\n%s", errBuf.String())
	}

	// Case 2: First engine modifies worktree then fails; second engine receives continuation_note and succeeds
	promptDump := filepath.Join(t.TempDir(), "hermes_prompt.txt")
	writeFakeBin("claude", `echo "partial work" > partial.txt; echo "hit your usage limit"; exit 1`)
	writeFakeBin("hermes", `printf "%s" "$2" > "`+promptDump+`"; echo "completed"; exit 0`)

	outBuf.Reset()
	errBuf.Reset()
	resOK, err := AgentExec(ctx, AgentExecOptions{
		Workdir: root,
		AppRoot: root,
		Prompt:  "Build feature Y",
		Engines: "claude,hermes",
		Role:    "build",
		Change:  "feat-y",
		Phase:   "apply",
		Store:   st,
		Stdout:  &outBuf,
		Stderr:  &errBuf,
	})
	if err != nil || resOK.ExitCode != 0 || resOK.UsedEngine != "hermes" {
		t.Fatalf("expected fallback to hermes with exit 0, got %+v (err=%v, stderr=%s)", resOK, err, errBuf.String())
	}
	gotPrompt, err := os.ReadFile(promptDump)
	if err != nil {
		t.Fatalf("failed to read hermes prompt dump: %v", err)
	}
	if !strings.Contains(string(gotPrompt), "## A previous attempt stopped before finishing") || !strings.Contains(string(gotPrompt), "partial.txt") {
		t.Fatalf("expected continuation note with partial.txt in fallback prompt, got:\n%s", string(gotPrompt))
	}

	// Verify agent.exit event was recorded in SQLite store
	evts, err := st.QueryEvents(ctx, store.EventFilter{Event: "agent.exit", ChangeName: "feat-y"})
	if err != nil || len(evts) != 1 {
		t.Fatalf("expected 1 agent.exit event for feat-y in store, got %d (err=%v)", len(evts), err)
	}
	if evts[0].Engine != "hermes" || evts[0].Payload["prompt_version"] == nil {
		t.Fatalf("unexpected agent.exit event payload: %+v", evts[0])
	}
}

func TestRepoExecAndRunGatesWithRegressionWhenAndSlot(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	ctx := context.Background()

	// Create a child repository under repos/backend
	backendDir := filepath.Join(root, "repos", "backend")
	if err := os.MkdirAll(backendDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
	} {
		if _, err := gitx.RunGit(ctx, backendDir, args...); err != nil {
			t.Fatal(err)
		}
	}

	// Write env_script and repo.config.yaml
	envScript := `export CUSTOM_REPO_ENV="loaded_ok"
if [ -n "${FOREMANKIT_SLOT:-}" ]; then
  export TEST_ENV_NUMBER="$FOREMANKIT_SLOT"
  export PORT="$((3000 + FOREMANKIT_SLOT * 10))"
fi
`
	_ = os.WriteFile(filepath.Join(backendDir, "env.sh"), []byte(envScript), 0o644)
	repoCfg := `kind: web
base_branch: main
env_script: env.sh
setup: "echo setup_slot=$FOREMANKIT_SLOT port=$PORT env=$CUSTOM_REPO_ENV"
gates:
  lint: "echo lint_changed=$FOREMANKIT_CHANGED_FILES"
  test: "echo test_ok"
regression:
  commands:
    full: "echo regression_full_ran"
  when:
    - "app/models/**"
`
	_ = os.WriteFile(filepath.Join(backendDir, "repo.config.yaml"), []byte(repoCfg), 0o644)
	_ = os.MkdirAll(filepath.Join(backendDir, "app", "models"), 0o755)
	_ = os.WriteFile(filepath.Join(backendDir, "app", "models", "user.rb"), []byte("class User; end\n"), 0o644)
	_, _ = gitx.RunGit(ctx, backendDir, "add", "-A")
	_, _ = gitx.RunGit(ctx, backendDir, "commit", "-m", "init backend")

	// Register repos/backend in .gitmodules of umbrella root
	gitmodules := `[submodule "backend"]
	path = repos/backend
	url = ./repos/backend
	branch = main
`
	_ = os.WriteFile(filepath.Join(root, ".gitmodules"), []byte(gitmodules), 0o644)
	_, _ = gitx.RunGit(ctx, root, "add", ".gitmodules")
	_, _ = gitx.RunGit(ctx, root, "commit", "-m", "add gitmodules")

	// Modify a file matching regression.when ("app/models/**") in backend
	_ = os.WriteFile(filepath.Join(backendDir, "app", "models", "account.rb"), []byte("class Account; end\n"), 0o644)

	var outBuf, errBuf bytes.Buffer
	gatesRes, err := RunGates(ctx, GatesOptions{
		Root:   root,
		Repos:  []string{"backend"},
		Slot:   3,
		Store:  st,
		Stdout: &outBuf,
		Stderr: &errBuf,
	})
	if err != nil || gatesRes.ExitCode != 0 {
		t.Fatalf("RunGates failed: res=%+v err=%v\nout:\n%s\nerr:\n%s", gatesRes, err, outBuf.String(), errBuf.String())
	}
	if gatesRes.Passed != 4 { // setup, lint, test, regression:full
		t.Fatalf("expected 4 gates executed (including regression:full), got %d:\n%s", gatesRes.Passed, outBuf.String())
	}
	if !strings.Contains(outBuf.String(), "setup_slot=3 port=3030 env=loaded_ok") {
		t.Fatalf("expected slot/env vars in setup output:\n%s", outBuf.String())
	}
	if !strings.Contains(outBuf.String(), "lint_changed=app/models/account.rb") {
		t.Fatalf("expected FOREMANKIT_CHANGED_FILES in lint output:\n%s", outBuf.String())
	}
	if !strings.Contains(outBuf.String(), "regression_full_ran") || !strings.Contains(errBuf.String(), "matches regression.when") {
		t.Fatalf("expected regression.when trigger and execution:\nout:\n%s\nerr:\n%s", outBuf.String(), errBuf.String())
	}
}

func TestPreflightWithMockJevAndConcurrentAgentPings(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	ctx := context.Background()

	// Create fake git, openspec, claude, hermes on PATH
	binDir := filepath.Join(t.TempDir(), "bin")
	_ = os.MkdirAll(binDir, 0o755)
	for _, name := range []string{"openspec", "claude", "hermes", "copilot"} {
		script := "#!/usr/bin/env bash\necho '" + name + " 1.0.0 pong'\nexit 0\n"
		_ = os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	// Write a valid .foreman-kit/kit.lock with 1 embedded skill file
	skills, err := assets.WalkEmbedded("skills")
	if err != nil {
		t.Fatal(err)
	}
	var firstSkillRel string
	var firstSkillBytes []byte
	for k, v := range skills {
		firstSkillRel = ".agents/skills/" + k
		firstSkillBytes = v
		break
	}
	skillDiskPath := filepath.Join(root, filepath.FromSlash(firstSkillRel))
	_ = os.MkdirAll(filepath.Dir(skillDiskPath), 0o755)
	_ = os.WriteFile(skillDiskPath, firstSkillBytes, 0o644)

	lockData := map[string]any{
		"version": assets.Version,
		"agents":  []string{"claude", "copilot"},
		"files": map[string]any{
			firstSkillRel: map[string]any{
				"class":  "kit",
				"sha256": assets.SHA256Bytes(firstSkillBytes),
			},
		},
	}
	lockBytes, _ := json.MarshalIndent(lockData, "", "  ")
	_ = os.WriteFile(filepath.Join(root, ".foreman-kit/kit.lock"), lockBytes, 0o644)

	// Mock Jev server for live canary
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-1.14.0",
			"answers": map[string]any{
				"all_gates_passed": map[string]any{"type": "noul", "noul": 0.96},
				"reports_failure":  map[string]any{"type": "noul", "noul": 0.02},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("TYPESAFE_API_KEY", "ts-canary-key")
	settingstest.Give(t, settings.EvaluatorBaseURL, srv.URL)
	settingstest.Give(t, settings.EvaluatorWhenUnavailable, settings.UnavailableStop)

	var outBuf, errBuf bytes.Buffer

	// An application without a memory was installed before agent-memory kept it: nothing starts.
	resNoMemory, _ := RunPreflight(ctx, PreflightOptions{Root: root, Store: st, Stdout: &outBuf, Stderr: &errBuf})
	if resNoMemory.OK || !strings.Contains(outBuf.String(), ".agent-memory: MISSING") ||
		!strings.Contains(outBuf.String(), "the application has no .agent-memory/: foreman-kit update creates it") {
		t.Fatalf("expected the preflight to fail on an application without a memory, got:\n%s", outBuf.String())
	}
	// The memory is made by the agent-memory on PATH, the real one: the kit holds no copy of it.
	if _, err := memory.Init(ctx, root, "demo"); err != nil {
		t.Fatal(err)
	}

	outBuf.Reset()
	res, err := RunPreflight(ctx, PreflightOptions{
		Root:   root,
		Store:  st,
		Stdout: &outBuf,
		Stderr: &errBuf,
	})
	if err != nil || !res.OK || res.ExitCode != 0 {
		t.Fatalf("RunPreflight failed: res=%+v err=%v\nout:\n%s", res, err, outBuf.String())
	}
	if res.JudgeStatus != "ok" || !strings.Contains(outBuf.String(), "preflight: OK") {
		t.Fatalf("unexpected preflight output:\n%s", outBuf.String())
	}
	// The toolchain names agent-memory with where it was found, and the memory is counted.
	agentMemoryPath, _ := exec.LookPath(memory.Binary)
	if !strings.Contains(outBuf.String(), "agent-memory: v") || !strings.Contains(outBuf.String(), "("+agentMemoryPath+")") ||
		!strings.Contains(outBuf.String(), ".agent-memory: OK (3 files)") {
		t.Fatalf("the preflight does not report agent-memory and the memory:\n%s", outBuf.String())
	}

	// Without agent-memory on PATH the toolchain is incomplete, and the preflight says how to
	// install it.
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitOnly := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(gitOnly, "git")); err != nil {
		t.Fatal(err)
	}
	fullPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+":"+gitOnly+":/usr/bin:/bin")
	if _, lookErr := exec.LookPath(memory.Binary); lookErr == nil {
		t.Fatal("agent-memory is installed in a system directory: this check cannot take it off PATH")
	}
	outBuf.Reset()
	resNoTool, _ := RunPreflight(ctx, PreflightOptions{Root: root, Store: st, Stdout: &outBuf, Stderr: &errBuf})
	if resNoTool.OK || !strings.Contains(outBuf.String(), "agent-memory: NOT FOUND") ||
		!strings.Contains(outBuf.String(), "go install github.com/xChuCx/agent-memory/cmd/agent-memory@latest") ||
		!strings.Contains(outBuf.String(), "preflight: FAIL (toolchain incomplete)") {
		t.Fatalf("expected the preflight to fail without agent-memory and to say how to install it, got:\n%s", outBuf.String())
	}
	t.Setenv("PATH", fullPath)

	// Modify kit skill file and verify drift fails preflight
	_ = os.WriteFile(skillDiskPath, []byte("tampered skill content"), 0o644)
	outBuf.Reset()
	resDrift, _ := RunPreflight(ctx, PreflightOptions{
		Root:   root,
		Store:  st,
		Stdout: &outBuf,
		Stderr: &errBuf,
	})
	if resDrift.OK || resDrift.ExitCode == 0 || !strings.Contains(outBuf.String(), "kit differs from .foreman-kit/kit.lock") {
		t.Fatalf("expected preflight to fail on kit drift, got:\n%s", outBuf.String())
	}
	// The files were whole in both runs, and each one was said to be.
	if !strings.Contains(outBuf.String(), "== Configuration\n  .foreman-kit/pipeline.yaml: ok\n  .foreman-kit/agents.yaml: ok\n") {
		t.Fatalf("the preflight does not list the configuration files:\n%s", outBuf.String())
	}

	// A configuration file that lacks a key fails the preflight, which names the file and the key:
	// what reads that file starts nothing new, so nothing would run as the files say.
	_ = os.WriteFile(skillDiskPath, firstSkillBytes, 0o644)
	daemonYAML := filepath.Join(root, ".foreman-kit", "daemon.yaml")
	whole, err := os.ReadFile(daemonYAML)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(whole), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "every_when_out_of_work:") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(daemonYAML, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	outBuf.Reset()
	resConfig, _ := RunPreflight(ctx, PreflightOptions{Root: root, Store: st, Stdout: &outBuf, Stderr: &errBuf})
	if resConfig.OK || resConfig.ExitCode == 0 ||
		!strings.Contains(outBuf.String(), "  .foreman-kit/daemon.yaml: FAILED (cycle.every_when_out_of_work: missing (the kit ships 15m))") ||
		!strings.Contains(outBuf.String(), "preflight: FAIL (.foreman-kit/daemon.yaml cannot be used as written") {
		t.Fatalf("expected preflight to fail on a configuration file that lacks a key, got:\n%s", outBuf.String())
	}

	// So does the repo.config.yaml of a repository that still holds the keys an older kit read: its
	// gates would not run as it says.
	if err := os.WriteFile(daemonYAML, whole, 0o644); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(root, "repos", "backend")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "repo.config.yaml"), []byte("base_branch: main\nrequires: {}\ngates:\n  test: \"true\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outBuf.Reset()
	resRepo, _ := RunPreflight(ctx, PreflightOptions{Root: root, Store: st, Stdout: &outBuf, Stderr: &errBuf})
	if resRepo.OK || resRepo.ExitCode == 0 ||
		!strings.Contains(outBuf.String(), "  repos/backend/repo.config.yaml: FAILED (written for an older foreman-kit: foreman-kit update rewrites it)") ||
		!strings.Contains(outBuf.String(), "preflight: FAIL (repos/backend/repo.config.yaml cannot be used as written") {
		t.Fatalf("expected preflight to fail on a repo.config.yaml of an older kit, got:\n%s", outBuf.String())
	}

	// And so does a variable an older kit read, while it is set.
	if err := os.Remove(filepath.Join(repoDir, "repo.config.yaml")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_PERMISSIONS", "restricted")
	outBuf.Reset()
	resVariable, _ := RunPreflight(ctx, PreflightOptions{Root: root, Store: st, Stdout: &outBuf, Stderr: &errBuf})
	if resVariable.OK || resVariable.ExitCode == 0 ||
		!strings.Contains(outBuf.String(), "  the environment: FAILED (AGENT_PERMISSIONS=restricted is set and no longer read: write permissions in .foreman-kit/agents.yaml") ||
		!strings.Contains(outBuf.String(), "preflight: FAIL (AGENT_PERMISSIONS set in this environment and no longer read") {
		t.Fatalf("expected preflight to fail on a variable an older kit read, got:\n%s", outBuf.String())
	}
}

func TestDynamicCommandsInAgentsYAML(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	ctx := context.Background()

	fakeBinDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	argsLog := filepath.Join(t.TempDir(), "invoked_args.txt")
	customRunner := filepath.Join(fakeBinDir, "my-custom-runner")
	script := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > " + argsLog + "\necho \"pong\"\n"
	if err := os.WriteFile(customRunner, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Write .foreman-kit/agents.yaml with commands of the application's own for claude and for a custom profile
	agentsYAML := `agents: [claude, custom-profile]
roles:
  plan:
    agents: [custom-profile]
    timeout: 30m
    args: {claude: "--model plan-only"}
  build:
    agents: [claude]
    timeout: 20m
    args: {claude: "--model sonnet-custom"}
  cycle:
    agents: [custom-profile]
    timeout: 1h
    args: {}
selection: in_order
permissions: full
commands:
  claude:
    run: "my-custom-runner --custom-claude-flag --cwd={{workdir}} --timeout={{timeout}} {{args}} {{prompt}}"
  custom-profile:
    run: "my-custom-runner --profile=fast {{prompt}}"
    restricted: "my-custom-runner --profile=sandboxed {{prompt}}"
    ping: "my-custom-runner --ping-check {{prompt}}"
`
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "agents.yaml"), []byte(agentsYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	var outBuf, errBuf bytes.Buffer
	res, err := AgentExec(ctx, AgentExecOptions{
		Workdir: root,
		AppRoot: root,
		Prompt:  "Verify dynamic commands",
		Engines: "claude",
		Role:    "build",
		Store:   st,
		Stdout:  &outBuf,
		Stderr:  &errBuf,
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("expected custom command for claude to succeed, got res=%+v err=%v stderr=%s", res, err, errBuf.String())
	}
	gotArgs, _ := os.ReadFile(argsLog)
	gotStr := string(gotArgs)
	// The command ran exactly as written: the role's own limit and arguments in their places, and
	// nothing the file does not say.
	if want := "--custom-claude-flag\n--cwd=" + root + "\n--timeout=1200\n--model\nsonnet-custom\nVerify dynamic commands\n"; gotStr != want {
		t.Fatalf("the command did not run as written; arguments:\n%s\nwant:\n%s", gotStr, want)
	}

	// Also verify ResolvePingArgs uses commands.custom-profile.ping
	pingBin, pingArgs, _ := ResolvePingArgs(root, "custom-profile", nil)
	if pingBin != "my-custom-runner" || len(pingArgs) < 2 || pingArgs[0] != "--ping-check" {
		t.Fatalf("expected ResolvePingArgs to use custom ping command, got bin=%s args=%v", pingBin, pingArgs)
	}
}

func TestEngineHealthCircuitBreakerInSpreadArrange(t *testing.T) {
	_, st := setupProcTestWorkspace(t)
	ctx := context.Background()

	// Mark claude and crush as in cooldown (auth_failed / config_error), while hermes is healthy and already has 1 active PID
	future := time.Now().UTC().Add(30 * time.Minute).Unix()
	_ = st.RecordEngineHealth(ctx, store.EngineHealthRecord{Engine: "claude", Installed: true, Status: "auth_failed", CooldownUntil: future})
	_ = st.RecordEngineHealth(ctx, store.EngineHealthRecord{Engine: "crush", Installed: true, Status: "config_error", CooldownUntil: future})
	_ = st.RecordEngineHealth(ctx, store.EngineHealthRecord{Engine: "hermes", Installed: true, Status: "ok", CooldownUntil: 0})
	_ = st.ClaimAgent(ctx, os.Getpid(), "hermes", "build", "active-change")

	// Even though hermes has in_use=1 and claude/crush have in_use=0, spread MUST pick healthy hermes first!
	arranged, err := st.ArrangeAgents(ctx, []string{"claude", "crush", "hermes"}, "least_busy", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(arranged) != 3 || arranged[0] != "hermes" {
		t.Fatalf("expected healthy hermes to be chosen first despite in_use=1, got %v", arranged)
	}
}

func TestPreflightGitVersionCheck(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	ctx := context.Background()

	if _, err := memory.Init(ctx, root, "demo"); err != nil {
		t.Fatal(err)
	}
	settingstest.Give(t, settings.EvaluatorEnabled, "false")

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"openspec", "claude", "hermes", "copilot"} {
		script := "#!/usr/bin/env bash\necho '" + name + " 1.0.0 pong'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fakeGit := filepath.Join(binDir, "git")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cases := []struct {
		name     string
		verLine  string
		wantOK   bool
		wantLine string
		wantFail string
	}{
		{
			name:     "2.16.6 below minimum",
			verLine:  "git version 2.16.6",
			wantOK:   false,
			wantLine: fmt.Sprintf("  git: 2.16.6 (minimum 2.17.0) (%s)", fakeGit),
			wantFail: fmt.Sprintf("preflight: FAIL (git 2.16.6 at %s is older than the minimum 2.17.0)", fakeGit),
		},
		{
			name:     "2.17.0 minimum",
			verLine:  "git version 2.17.0",
			wantOK:   true,
			wantLine: fmt.Sprintf("  git: 2.17.0 (minimum 2.17.0) (%s)", fakeGit),
		},
		{
			name:     "Apple vendor string",
			verLine:  "git version 2.39.5 (Apple Git-154)",
			wantOK:   true,
			wantLine: fmt.Sprintf("  git: 2.39.5 (minimum 2.17.0) (%s)", fakeGit),
		},
		{
			name:     "unreadable version",
			verLine:  "git version custom-unreadable",
			wantOK:   false,
			wantLine: fmt.Sprintf("  git: unreadable version (minimum 2.17.0) (%s)", fakeGit),
			wantFail: fmt.Sprintf("preflight: FAIL (git version at %s cannot be read (minimum 2.17.0))", fakeGit),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := "#!/bin/sh\necho '" + tc.verLine + "'\nexit 0\n"
			if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			var outBuf, errBuf bytes.Buffer
			res, err := RunPreflight(ctx, PreflightOptions{
				Root:   root,
				Store:  st,
				Stdout: &outBuf,
				Stderr: &errBuf,
			})
			if err != nil {
				t.Fatalf("RunPreflight error: %v", err)
			}
			out := outBuf.String()
			if !strings.Contains(out, tc.wantLine) {
				t.Fatalf("expected output to contain %q, got:\n%s", tc.wantLine, out)
			}
			if tc.wantOK {
				if !res.OK || res.ExitCode != 0 {
					t.Fatalf("expected preflight OK, got res=%+v\nout:\n%s", res, out)
				}
			} else {
				if res.OK || res.ExitCode == 0 {
					t.Fatalf("expected preflight to fail, got res=%+v\nout:\n%s", res, out)
				}
				if !strings.Contains(out, tc.wantFail) {
					t.Fatalf("expected failure line %q, got:\n%s", tc.wantFail, out)
				}
			}
		})
	}
}
