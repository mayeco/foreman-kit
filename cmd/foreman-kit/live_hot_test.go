//go:build live

package main

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

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/proc"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

// setupHotEnv prepares PATH with real agent CLIs ($HOME/.local/share/mise/shims, $HOME/.local/bin, Linuxbrew/Homebrew)
// plus a freshly built foreman-kit binary in a temp bin directory, configures git identity
// and protocol.file.allow=always for submodules, and sets isolated FOREMANKIT_STATE_DIR.
func setupHotEnv(t *testing.T) (tmpRoot string, stateDir string) {
	t.Helper()
	tmpRoot = t.TempDir()
	stateDir = filepath.Join(tmpRoot, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(tmpRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Build the current foreman-kit binary so any subcommand or agent calling `foreman-kit` uses the Go binary.
	kitBin := filepath.Join(binDir, "foreman-kit")
	buildCmd := exec.Command("go", "build", "-o", kitBin, ".")
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build foreman-kit binary: %v\n%s", err, string(out))
	}

	home, _ := os.UserHomeDir()
	origPath := os.Getenv("PATH")
	newPath := fmt.Sprintf("%s:%s/.local/share/mise/shims:%s/.local/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/opt/homebrew/bin:/usr/bin:/bin:%s", binDir, home, home, origPath)
	t.Setenv("PATH", newPath)
	// Unset PWD and OLDPWD so Bun/Node-based agent CLIs (like opencode) use actual getcwd() (cmd.Dir)
	// when spawned with an explicit cmd.Env slice in worker worktrees.
	_ = os.Unsetenv("PWD")
	_ = os.Unsetenv("OLDPWD")
	t.Setenv("FOREMANKIT_STATE_DIR", stateDir)
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "0")
	// Fast flags for real hermes and opencode CLIs during hot tests
	t.Setenv("EXTRA_ARGS_HERMES", "--ignore-rules --reasoning none -t file")
	t.Setenv("EXTRA_ARGS_OPENCODE", "--standalone --auto")
	settingstest.Give(t, settings.AgentRetries, "0")
	settingstest.Give(t, settings.AgentRetryAfter, "0s")

	// Ensure git commits and local file:// submodule clones work in every child worktree/process
	t.Setenv("GIT_AUTHOR_NAME", "OpenSpec Hot Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "hot-test@openspec.local")
	t.Setenv("GIT_COMMITTER_NAME", "OpenSpec Hot Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "hot-test@openspec.local")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	for _, bin := range []string{"hermes", "opencode", "claude", "codex", "openspec", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("required live binary %q not found on PATH (%s): %v", bin, newPath, err)
		}
	}
	return tmpRoot, stateDir
}

// startCalibratedJevServer starts a live HTTP Jev server answering canary, plan_review,
// scenario_coverage, change_risk, proposal_review, failure_triage, and repair_progress.
func startCalibratedJevServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"model": "jev-1.14.0",
			"answers": map[string]any{
				// canary.yaml
				"all_gates_passed": map[string]any{"type": "noul", "noul": 0.96},
				"reports_failure":  map[string]any{"type": "noul", "noul": 0.02},
				// plan_review.yaml
				"problem_is_concrete":             map[string]any{"type": "noul", "noul": 0.95},
				"scenarios_are_testable":          map[string]any{"type": "noul", "noul": 0.94},
				"tasks_cover_scenarios":           map[string]any{"type": "noul", "noul": 0.95},
				"consistent_with_canonical_specs": map[string]any{"type": "noul", "noul": 0.96},
				"single_concern":                  map[string]any{"type": "noul", "noul": 0.93},
				// scenario_coverage.yaml
				"every_scenario_tested": map[string]any{"type": "noul", "noul": 0.95},
				"tests_assert_outcomes": map[string]any{"type": "noul", "noul": 0.94},
				// change_risk.yaml
				"blast_radius":              map[string]any{"type": "choice", "choice": "contained", "confidence": 0.92},
				"touches_sensitive_surface": map[string]any{"type": "noul", "noul": 0.04},
				"matches_proposal":          map[string]any{"type": "noul", "noul": 0.96},
				"reversible":                map[string]any{"type": "noul", "noul": 0.95},
				// proposal_review.yaml
				"advances_the_product": map[string]any{"type": "noul", "noul": 0.95},
				"well_scoped":          map[string]any{"type": "noul", "noul": 0.93},
				"not_duplicate":        map[string]any{"type": "noul", "noul": 0.94},
				"safe_to_automate":     map[string]any{"type": "noul", "noul": 0.92},
			},
			"usage": map[string]any{
				"input_tokens":  180,
				"output_tokens": 32,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	settingstest.Give(t, settings.EvaluatorEnabled, "true")
	t.Setenv("TYPESAFE_API_KEY", "ts-live-calibrated-key")
	settingstest.Give(t, settings.EvaluatorBaseURL, srv.URL)
	return srv
}

func mustGitHot(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.RunGit(ctx, dir, args...)
	if err != nil {
		t.Fatalf("git %s in %s failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(out)
}

// 1. TestHotPreflightAndFallbackWithRealAgents:
// - Initializes a temporary app with foreman-kit init using --agents claude,hermes,opencode --plan claude,hermes --build opencode,hermes --cycle hermes.
// - Runs live preflight against real binaries: verifies that hermes and opencode respond with live pong, claude fails fast without hanging, and the plan role passes with WARN using its real fallback (hermes).
func TestHotPreflightAndFallbackWithRealAgents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	tmpRoot, _ := setupHotEnv(t)
	startCalibratedJevServer(t)
	settingstest.Give(t, settings.EvaluatorWhenUnavailable, settings.UnavailableStop)

	appDir := filepath.Join(tmpRoot, "preflight-app")
	t.Setenv("FOREMANKIT_APP_ROOT", appDir)

	var out, errBuf bytes.Buffer
	initArgs := []string{
		"init", appDir,
		"--agents", "claude,hermes,opencode",
		"--plan", "claude,hermes",
		"--build", "opencode,hermes",
		"--cycle", "hermes",
	}
	if code := Run(ctx, initArgs, &out, &errBuf); code != 0 {
		t.Fatalf("foreman-kit init failed (%d):\nstdout:\n%s\nstderr:\n%s", code, out.String(), errBuf.String())
	}

	out.Reset()
	errBuf.Reset()
	start := time.Now()
	code := Run(ctx, []string{"preflight"}, &out, &errBuf)
	elapsed := time.Since(start)
	preflightOut := out.String()
	t.Logf("preflight finished in %s (exit=%d):\n%s", elapsed, code, preflightOut)

	if code != 0 {
		t.Fatalf("expected preflight exit code 0, got %d:\nstdout:\n%s\nstderr:\n%s", code, preflightOut, errBuf.String())
	}
	if !strings.Contains(preflightOut, "  claude: FAILED (authentication)") {
		t.Fatalf("expected claude to fail authentication quickly in preflight, got:\n%s", preflightOut)
	}
	if !strings.Contains(preflightOut, "  hermes: OK") {
		t.Fatalf("expected hermes: OK from live pong in preflight, got:\n%s", preflightOut)
	}
	if !strings.Contains(preflightOut, "  opencode: OK") {
		t.Fatalf("expected opencode: OK from live pong in preflight, got:\n%s", preflightOut)
	}
	if !strings.Contains(preflightOut, "  plan: WARN runs on hermes; not working in its list: claude") {
		t.Fatalf("expected plan role WARN fallback to hermes in preflight, got:\n%s", preflightOut)
	}
	if !strings.Contains(preflightOut, "preflight: OK (engines: hermes opencode; roles: plan:claude,hermes build:opencode,hermes cycle:hermes; judge: ok)") {
		t.Fatalf("expected final preflight: OK summary with hermes & opencode, got:\n%s", preflightOut)
	}
}

// 2. TestHotAgentExecFallbackAndContinuationWithRealLLM:
//   - Runs agent-exec in a real git worktree with the claude,opencode and codex,hermes engine lists.
//   - Verifies that after claude/codex authentication failure, the real fallback (opencode and hermes) takes over,
//     receives the continuation_note when prior worktree changes exist, creates/modifies a real file requested by the prompt,
//     streams log_chunks, and emits the agent.exit event in SQLite.
func TestHotAgentExecFallbackAndContinuationWithRealLLM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	tmpRoot, _ := setupHotEnv(t)
	appDir := filepath.Join(tmpRoot, "exec-app")
	t.Setenv("FOREMANKIT_APP_ROOT", appDir)

	var out, errBuf bytes.Buffer
	if code := Run(ctx, []string{
		"init", appDir,
		"--agents", "claude,codex,hermes,opencode",
		"--plan", "claude,opencode",
		"--build", "codex,hermes",
		"--cycle", "hermes",
	}, &out, &errBuf); code != 0 {
		t.Fatalf("init failed (%d): %s\n%s", code, out.String(), errBuf.String())
	}

	mustGitHot(t, ctx, appDir, "init", "-b", "master")
	mustGitHot(t, ctx, appDir, "add", "-A")
	mustGitHot(t, ctx, appDir, "commit", "-m", "chore: init workspace")

	wtDir := filepath.Join(appDir, ".worktrees", "worker-hot-fallback")
	if err := os.MkdirAll(filepath.Dir(wtDir), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitHot(t, ctx, appDir, "worktree", "add", "-b", "feat/spec-hot-fallback", wtDir, "HEAD")

	st, err := store.Open(appDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Case A: claude,opencode fallback with continuation_note, streaming log_chunks, and agent.exit event
	execID1 := "hot-exec-claude-opencode"
	if err := st.CreateExecution(ctx, store.Execution{
		ExecID:     execID1,
		Kind:       "worker",
		ChangeName: "hot-opencode-change",
		Agent:      "claude",
		Phase:      "apply",
		Status:     "running",
		PID:        os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}

	prompt1 := `Do NOT run shell commands. Use your file write tool to write the file "opencode_result.txt" in the current working directory.
If your prompt contains the heading "## A previous attempt stopped before finishing" and mentions "partial_claude.txt", write the exact text "OPENCODE_CONTINUED_OK" into "opencode_result.txt".
Otherwise write "OPENCODE_NO_CONTINUATION" into "opencode_result.txt". Stop immediately after writing the file.`
	promptFile1 := filepath.Join(tmpRoot, "prompt-claude-opencode.md")
	if err := os.WriteFile(promptFile1, []byte(prompt1), 0o644); err != nil {
		t.Fatal(err)
	}

	// While claude is running (takes ~1.2s) and after startState is captured (~5ms), write partial_claude.txt
	// so TreeState(ctx, wtDir) != startState triggers ContinuationNote for the fallback engine (opencode).
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(wtDir, "partial_claude.txt"), []byte("partial work before claude exited\n"), 0o644)
	}()

	out.Reset()
	errBuf.Reset()
	res1, err := proc.AgentExec(ctx, proc.AgentExecOptions{
		Workdir:    wtDir,
		AppRoot:    appDir,
		PromptFile: promptFile1,
		Engines:    "claude,opencode",
		Role:       "plan",
		Change:     "hot-opencode-change",
		Phase:      "plan",
		Store:      st,
		ExecID:     execID1,
		Stdout:     &out,
		Stderr:     &errBuf,
	})
	if err != nil {
		t.Fatalf("AgentExec(claude,opencode) returned error: %v", err)
	}
	_ = st.FinishExecution(ctx, execID1, "succeeded", res1.ExitCode)
	t.Logf("AgentExec(claude,opencode) exit=%d used=%s stderr:\n%s", res1.ExitCode, res1.UsedEngine, errBuf.String())

	if res1.ExitCode != 0 || res1.UsedEngine != "opencode" {
		t.Fatalf("expected ExitCode=0 and UsedEngine=opencode, got %+v\nstderr:\n%s", res1, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "claude has an authentication or provider configuration failure") ||
		!strings.Contains(errBuf.String(), "claude failed (exit 1); trying opencode") {
		t.Fatalf("expected claude auth failure and fallback to opencode in stderr, got:\n%s", errBuf.String())
	}

	gotFile1, err := os.ReadFile(filepath.Join(wtDir, "opencode_result.txt"))
	if err != nil {
		t.Fatalf("expected opencode_result.txt to be created by opencode: %v", err)
	}
	if !strings.Contains(string(gotFile1), "OPENCODE_CONTINUED_OK") {
		t.Fatalf("expected opencode_result.txt to contain OPENCODE_CONTINUED_OK (proving continuation_note was received), got %q", string(gotFile1))
	}

	chunks1, err := st.ReadLogChunks(ctx, execID1, 0)
	if err != nil || len(chunks1) == 0 {
		t.Fatalf("expected streaming log_chunks in SQLite for %s, got %d (err=%v)", execID1, len(chunks1), err)
	}

	evts1, err := st.QueryEvents(ctx, store.EventFilter{Event: "agent.exit", ChangeName: "hot-opencode-change"})
	if err != nil || len(evts1) != 1 || evts1[0].Engine != "opencode" {
		t.Fatalf("expected agent.exit event for hot-opencode-change with engine=opencode, got %+v (err=%v)", evts1, err)
	}

	// Commit worktree state before Case B so startState for Case B is clean
	mustGitHot(t, ctx, wtDir, "add", "-A")
	mustGitHot(t, ctx, wtDir, "commit", "-m", "chore: after opencode fallback")

	// Case B: codex,hermes fallback via CLI `foreman-kit agent-exec` + SQLite log_chunks & agent.exit verification
	execID2 := "hot-exec-codex-hermes"
	if err := st.CreateExecution(ctx, store.Execution{
		ExecID:     execID2,
		Kind:       "worker",
		ChangeName: "hot-hermes-change",
		Agent:      "codex",
		Phase:      "apply",
		Status:     "running",
		PID:        os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}

	prompt2 := `Do NOT run shell commands. Use your file write tool to write the file "hermes_result.txt" in the current working directory.
If your prompt contains the heading "## A previous attempt stopped before finishing" and mentions "partial_codex.txt", write the exact text "HERMES_CONTINUED_OK" into "hermes_result.txt".
Otherwise write "HERMES_NO_CONTINUATION" into "hermes_result.txt". Stop immediately after writing the file.`
	promptFile2 := filepath.Join(tmpRoot, "prompt-codex-hermes.md")
	if err := os.WriteFile(promptFile2, []byte(prompt2), 0o644); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(250 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(wtDir, "partial_codex.txt"), []byte("partial work before codex exited\n"), 0o644)
	}()

	out.Reset()
	errBuf.Reset()
	res2, err := proc.AgentExec(ctx, proc.AgentExecOptions{
		Workdir:    wtDir,
		AppRoot:    appDir,
		PromptFile: promptFile2,
		Engines:    "codex,hermes",
		Role:       "build",
		Change:     "hot-hermes-change",
		Phase:      "apply",
		Store:      st,
		ExecID:     execID2,
		Stdout:     &out,
		Stderr:     &errBuf,
	})
	if err != nil {
		t.Fatalf("AgentExec(codex,hermes) returned error: %v", err)
	}
	_ = st.FinishExecution(ctx, execID2, "succeeded", res2.ExitCode)
	t.Logf("AgentExec(codex,hermes) exit=%d used=%s stderr:\n%s", res2.ExitCode, res2.UsedEngine, errBuf.String())

	if res2.ExitCode != 0 || res2.UsedEngine != "hermes" {
		t.Fatalf("expected ExitCode=0 and UsedEngine=hermes, got %+v\nstderr:\n%s", res2, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "codex has an authentication or provider configuration failure") ||
		!strings.Contains(errBuf.String(), "codex failed (exit 1); trying hermes") {
		t.Fatalf("expected codex auth failure and fallback to hermes in stderr, got:\n%s", errBuf.String())
	}

	gotFile2, err := os.ReadFile(filepath.Join(wtDir, "hermes_result.txt"))
	if err != nil {
		t.Fatalf("expected hermes_result.txt to be created by hermes: %v", err)
	}
	if !strings.Contains(string(gotFile2), "HERMES_CONTINUED_OK") {
		t.Fatalf("expected hermes_result.txt to contain HERMES_CONTINUED_OK (proving continuation_note was received), got %q", string(gotFile2))
	}

	chunks2, err := st.ReadLogChunks(ctx, execID2, 0)
	if err != nil || len(chunks2) == 0 {
		t.Fatalf("expected streaming log_chunks in SQLite for %s, got %d (err=%v)", execID2, len(chunks2), err)
	}

	evts2, err := st.QueryEvents(ctx, store.EventFilter{Event: "agent.exit", ChangeName: "hot-hermes-change"})
	if err != nil || len(evts2) != 1 || evts2[0].Engine != "hermes" {
		t.Fatalf("expected agent.exit event for hot-hermes-change with engine=hermes, got %+v (err=%v)", evts2, err)
	}
}

// 3. TestHotEndToEndPipelineDispatchAndMergeWithRealLLM:
//   - Creates a full git sandbox with bare remotes (umbrella.git and backend.git), initializes the kit with foreman-kit init, and adds the repos/backend submodule with its repo.config.yaml (with real lint and test gates).
//   - Queues a real change in .foreman-kit/agent_backlog.json (add-health-endpoint) and runs the full pipeline
//     (Dispatcher PLAN + openspec validate --strict + Dispatcher BUILD + gates + the merge queue + Merger rebase + gates + openspec archive + push to origin)
//     using a real LLM agent (opencode,hermes) and a live calibrated Jev HTTP server.
//   - Verifies that the real LLM generates valid proposal.md, design.md, tasks.md, and specs/health/spec.md, implements the code in repos/backend,
//     checks off tasks - [x], passes real gates, and the Merger archives and integrates the commit with the OpenSpec-Change trailer into origin/master and moves the item to .foreman-kit/agent_history.json.
func TestHotEndToEndPipelineDispatchAndMergeWithRealLLM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	tmpRoot, _ := setupHotEnv(t)
	startCalibratedJevServer(t)

	// 1. Bare remote for backend.git
	backendBareDir := filepath.Join(tmpRoot, "remotes", "backend.git")
	if err := os.MkdirAll(backendBareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitHot(t, ctx, backendBareDir, "init", "--bare", "-b", "main")

	// Seed backend repository with a real Go package and real lint (go vet ./...) + test (go test ./...) gates
	backendSeed := filepath.Join(tmpRoot, "seed-backend")
	if err := os.MkdirAll(backendSeed, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitHot(t, ctx, backendSeed, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(backendSeed, "go.mod"), []byte("module example.com/backend\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backendSeed, "backend.go"), []byte("package backend\n\nfunc ServiceName() string {\n\treturn \"backend\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backendSeed, "backend_test.go"), []byte("package backend\n\nimport \"testing\"\n\nfunc TestServiceName(t *testing.T) {\n\tif ServiceName() != \"backend\" {\n\t\tt.Fatal(\"unexpected service name\")\n\t}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repoCfg := `kind: lib
base_branch: main
gates:
  lint: "go vet ./..."
  test: "go test -v ./..."
`
	if err := os.WriteFile(filepath.Join(backendSeed, "repo.config.yaml"), []byte(repoCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitHot(t, ctx, backendSeed, "add", "-A")
	mustGitHot(t, ctx, backendSeed, "commit", "-m", "chore: initial backend")
	mustGitHot(t, ctx, backendSeed, "remote", "add", "origin", backendBareDir)
	mustGitHot(t, ctx, backendSeed, "push", "-u", "origin", "main")

	// 2. Bare remote for umbrella.git
	umbrellaBareDir := filepath.Join(tmpRoot, "remotes", "umbrella.git")
	if err := os.MkdirAll(umbrellaBareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitHot(t, ctx, umbrellaBareDir, "init", "--bare", "-b", "master")

	// 3. Umbrella checkout + foreman-kit init
	umbrellaDir := filepath.Join(tmpRoot, "umbrella")
	if err := os.MkdirAll(umbrellaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitHot(t, ctx, umbrellaDir, "init", "-b", "master")
	t.Setenv("FOREMANKIT_APP_ROOT", umbrellaDir)

	var out, errBuf bytes.Buffer
	if code := Run(ctx, []string{
		"init", umbrellaDir,
		"--agents", "opencode,hermes",
		"--plan", "opencode,hermes",
		"--build", "opencode,hermes",
		"--cycle", "hermes",
	}, &out, &errBuf); code != 0 {
		t.Fatalf("foreman-kit init failed (%d): %s\n%s", code, out.String(), errBuf.String())
	}

	// Add submodule repos/backend
	mustGitHot(t, ctx, umbrellaDir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", backendBareDir, "repos/backend")
	mustGitHot(t, ctx, umbrellaDir, "config", "-f", ".gitmodules", "submodule.repos/backend.branch", "main")

	// Tailor application seed prompts (.foreman-kit/prompts/plan.md & build.md) while keeping required placeholders
	// ({{change}}, {{goal}}, {{feedback}}, {{branch}}, {{rebuild}}) so `foreman-kit config check` and `verify` pass 100%
	// and the real LLM generates deterministic, strict-valid OpenSpec artifacts and Go code in seconds.
	planPromptSeed := `You are the PLANNING agent for OpenSpec change "{{change}}" in this git worktree.
Goal: {{goal}}

Do NOT run any shell commands. Use your file write tool to create these 4 files inside openspec/changes/{{change}}/ immediately and then stop:

1. openspec/changes/{{change}}/proposal.md:
# Proposal: Add Health Endpoint

## Why
Provide a deterministic health check status function in repos/backend.

## What Changes
- Add HealthStatus() string in repos/backend/health.go and unit test in repos/backend/health_test.go.

## Capabilities
- New Capability: health

## Impact
- Touches repos/backend.

2. openspec/changes/{{change}}/design.md:
# Design: Add Health Endpoint

## Goals
- Provide HealthStatus() string returning "ok" in package backend under repos/backend.

## Non-Goals
- External service dependencies.

3. openspec/changes/{{change}}/tasks.md:
# Tasks

## 1. Implementation
- [ ] 1.1 Create repos/backend/health.go with HealthStatus() string returning "ok" and repos/backend/health_test.go testing it

4. openspec/changes/{{change}}/specs/health/spec.md:
## Purpose
This capability provides a deterministic health status check function in the backend package so callers and readiness probes can verify service health.

## ADDED Requirements

### Requirement: Health status response
The backend package SHALL provide a HealthStatus() function returning "ok".

#### Scenario: Health check returns ok
- **WHEN** HealthStatus() is called
- **THEN** it returns "ok"

Hard rules: only write files inside openspec/changes/{{change}}/; never commit or switch branches.
{{feedback}}
`
	buildPromptSeed := `You are the IMPLEMENTATION agent for OpenSpec change "{{change}}" in this git worktree (child repos on branch {{branch}}).

Do NOT run git commit, git checkout, or git switch. Use your file write tool to create/update these 3 files immediately and then stop:

1. repos/backend/health.go:
package backend

func HealthStatus() string {
	return "ok"
}

2. repos/backend/health_test.go:
package backend

import "testing"

func TestHealthStatus(t *testing.T) {
	if got := HealthStatus(); got != "ok" {
		t.Fatalf("expected ok, got %q", got)
	}
}

3. openspec/changes/{{change}}/tasks.md:
# Tasks

## 1. Implementation
- [x] 1.1 Create repos/backend/health.go with HealthStatus() string returning "ok" and repos/backend/health_test.go testing it

Hard rules: only modify files under repos/backend/ and openspec/changes/{{change}}/tasks.md.
{{feedback}}
{{rebuild}}
`
	if err := os.WriteFile(filepath.Join(umbrellaDir, ".foreman-kit", "prompts", "plan.md"), []byte(planPromptSeed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(umbrellaDir, ".foreman-kit", "prompts", "build.md"), []byte(buildPromptSeed), 0o644); err != nil {
		t.Fatal(err)
	}

	// Verify `foreman-kit config check` and `foreman-kit verify` pass before committing
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"config", "check"}, &out, &errBuf); code != 0 {
		t.Fatalf("config check failed (%d): %s\n%s", code, out.String(), errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"verify", umbrellaDir}, &out, &errBuf); code != 0 {
		t.Fatalf("verify failed (%d): %s\n%s", code, out.String(), errBuf.String())
	}

	// Enqueue add-health-endpoint in .foreman-kit/agent_backlog.json
	backlogJSON := `[
  {
    "change_name": "add-health-endpoint",
    "description": "Add HealthStatus() returning ok and unit tests in repos/backend",
    "priority": "high",
    "impact_score": 9
  }
]
`
	if err := os.WriteFile(filepath.Join(umbrellaDir, ".foreman-kit", "agent_backlog.json"), []byte(backlogJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	mustGitHot(t, ctx, umbrellaDir, "add", "-A")
	mustGitHot(t, ctx, umbrellaDir, "commit", "-m", "chore: initialize umbrella with backend submodule and backlog")
	mustGitHot(t, ctx, umbrellaDir, "remote", "add", "origin", umbrellaBareDir)
	mustGitHot(t, ctx, umbrellaDir, "push", "-u", "origin", "master")

	// Step A: Run `foreman-kit dispatch --workers 1` (PLAN + openspec validate --strict + BUILD + real gates + the merge queue)
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"dispatch", "--workers", "1"}, &out, &errBuf); code != 0 {
		t.Fatalf("foreman-kit dispatch failed (%d):\nstdout:\n%s\nstderr:\n%s", code, out.String(), errBuf.String())
	}

	st, err := store.Open(umbrellaDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Inspect worker worktree before merge to confirm the real LLM generated all OpenSpec artifacts and Go code
	readyList, err := st.ListReadyQueue(ctx)
	if err != nil || len(readyList) != 1 {
		qs, _ := st.ListQuarantine(ctx, "")
		evts, _ := st.QueryEvents(ctx, store.EventFilter{})
		execs, _ := st.ListExecutions(ctx, "", "", 10)
		for _, ex := range execs {
			lg, _ := st.ReadFullLog(ctx, ex.ExecID)
			t.Logf("EXEC %s (%s/%s): status=%s code=%d\nLOG:\n%s", ex.ExecID, ex.ChangeName, ex.Phase, ex.Status, ex.ExitCode, lg)
		}
		t.Fatalf("expected 1 item in the merge queue after dispatch, got %+v (err=%v)\nquarantine=%+v\nevents=%+v", readyList, err, qs, evts)
	}
	manifest := readyList[0]
	if manifest.ChangeName != "add-health-endpoint" {
		t.Fatalf("expected the merge queue change_name=add-health-endpoint, got %+v", manifest)
	}
	if len(manifest.Capabilities) != 1 || manifest.Capabilities[0] != "health" {
		t.Fatalf("expected capability [health], got %+v", manifest.Capabilities)
	}

	workerWT := manifest.Worktree
	for _, rel := range []string{
		"openspec/changes/add-health-endpoint/proposal.md",
		"openspec/changes/add-health-endpoint/design.md",
		"openspec/changes/add-health-endpoint/tasks.md",
		"openspec/changes/add-health-endpoint/specs/health/spec.md",
		"repos/backend/health.go",
		"repos/backend/health_test.go",
	} {
		full := filepath.Join(workerWT, filepath.FromSlash(rel))
		data, err := os.ReadFile(full)
		if err != nil || len(strings.TrimSpace(string(data))) == 0 {
			t.Fatalf("expected non-empty file %s generated by real LLM, err=%v", rel, err)
		}
	}
	tasksContent, _ := os.ReadFile(filepath.Join(workerWT, "openspec/changes/add-health-endpoint/tasks.md"))
	if !strings.Contains(string(tasksContent), "- [x]") {
		t.Fatalf("expected tasks.md to have completed checkbox - [x], got:\n%s", string(tasksContent))
	}

	// Step B: Run `foreman-kit merge` (Merger rebase + gates + openspec archive + push to origin/main and origin/master)
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"merge"}, &out, &errBuf); code != 0 {
		t.Fatalf("foreman-kit merge failed (%d):\nstdout:\n%s\nstderr:\n%s", code, out.String(), errBuf.String())
	}

	// Verify the merge queue is now drained
	readyAfter, err := st.ListReadyQueue(ctx)
	if err != nil || len(readyAfter) != 0 {
		t.Fatalf("expected the merge queue to be empty after merge, got %+v (err=%v)", readyAfter, err)
	}

	// Verify origin/master has the commit with trailer OpenSpec-Change: add-health-endpoint and Risk-Assessment
	rootRepo, err := gitx.Open(umbrellaDir)
	if err != nil {
		t.Fatal(err)
	}
	hasTrailer, err := rootRepo.HasCommitWithTrailer("origin/master", "add-health-endpoint")
	if err != nil || !hasTrailer {
		t.Fatalf("expected origin/master to have commit with trailer OpenSpec-Change: add-health-endpoint, got %v (err=%v)", hasTrailer, err)
	}
	commitLog := mustGitHot(t, ctx, umbrellaDir, "log", "-n", "1", "origin/master")
	if !strings.Contains(commitLog, "OpenSpec-Change: add-health-endpoint") || !strings.Contains(commitLog, "Risk-Assessment:") {
		t.Fatalf("expected OpenSpec-Change and Risk-Assessment trailers in origin/master commit, got:\n%s", commitLog)
	}

	// Verify canonical spec was archived onto origin/master
	specBlob, err := rootRepo.ReadBlob("origin/master", "openspec/specs/health/spec.md")
	if err != nil || !strings.Contains(string(specBlob), "Health status response") {
		t.Fatalf("expected canonical spec openspec/specs/health/spec.md on origin/master, err=%v content=%q", err, string(specBlob))
	}

	// Verify child repository commit was pushed to backend.git (origin/main)
	backendRemoteLog := mustGitHot(t, ctx, backendBareDir, "log", "--oneline", "-n", "5", "main")
	if !strings.Contains(backendRemoteLog, "add-health-endpoint") {
		t.Fatalf("expected backend.git main branch to contain add-health-endpoint implementation commit, got:\n%s", backendRemoteLog)
	}

	// Verify item moved from .foreman-kit/agent_backlog.json to .foreman-kit/agent_history.json
	backlogAfter, err := cycle.ReadJSONList(filepath.Join(umbrellaDir, ".foreman-kit", "agent_backlog.json"))
	if err != nil || len(backlogAfter) != 0 {
		t.Fatalf("expected empty .foreman-kit/agent_backlog.json after integration, got %+v (err=%v)", backlogAfter, err)
	}
	historyAfter, err := cycle.ReadJSONList(filepath.Join(umbrellaDir, ".foreman-kit", "agent_history.json"))
	if err != nil || len(historyAfter) != 1 || fmt.Sprint(historyAfter[0]["change_name"]) != "add-health-endpoint" {
		t.Fatalf("expected add-health-endpoint in .foreman-kit/agent_history.json, got %+v (err=%v)", historyAfter, err)
	}
}

// 4. TestHotCycleAndProposalsWithRealLLM:
// - Runs cycle-prompt, proposals review, and status/progress --json, verifying that the full cycle and SQLite inspection flow works without error.
func TestHotCycleAndProposalsWithRealLLM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	tmpRoot, _ := setupHotEnv(t)
	startCalibratedJevServer(t)

	appDir := filepath.Join(tmpRoot, "cycle-app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitHot(t, ctx, appDir, "init", "-b", "master")
	t.Setenv("FOREMANKIT_APP_ROOT", appDir)

	var out, errBuf bytes.Buffer
	if code := Run(ctx, []string{
		"init", appDir,
		"--agents", "opencode,hermes",
		"--plan", "opencode,hermes",
		"--build", "opencode,hermes",
		"--cycle", "opencode,hermes",
	}, &out, &errBuf); code != 0 {
		t.Fatalf("init failed (%d): %s\n%s", code, out.String(), errBuf.String())
	}

	// Enable prompt_approval: auto in .foreman-kit/cycle.yaml so both feature proposals (via Jev) and prompt proposals are reviewed
	cycleYAMLPath := filepath.Join(appDir, ".foreman-kit", "cycle.yaml")
	cycleBytes, err := os.ReadFile(cycleYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	updatedCycleYAML := strings.Replace(string(cycleBytes), "prompt_approval: human", "prompt_approval: auto", 1)
	if err := os.WriteFile(cycleYAMLPath, []byte(updatedCycleYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	mustGitHot(t, ctx, appDir, "add", "-A")
	mustGitHot(t, ctx, appDir, "commit", "-m", "chore: initial cycle workspace")

	// 1. Run `foreman-kit cycle-prompt`
	promptOutPath := filepath.Join(appDir, "log", "cycle", "prompt-hot.md")
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"cycle-prompt", promptOutPath}, &out, &errBuf); code != 0 {
		t.Fatalf("cycle-prompt failed (%d): %s\n%s", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "strategy:") {
		t.Fatalf("expected strategy output from cycle-prompt, got: %s", out.String())
	}
	renderedPrompt, err := os.ReadFile(promptOutPath)
	if err != nil || !strings.Contains(string(renderedPrompt), "## This cycle (decided by the script)") {
		t.Fatalf("expected rendered cycle prompt with '## This cycle (decided by the script)', err=%v", err)
	}

	// 2. Run a real LLM (opencode,hermes) via `foreman-kit agent-exec` in the cycle workspace to author a proposal in .foreman-kit/agent_proposals.json and update MEMORY.md
	llmCyclePromptFile := filepath.Join(tmpRoot, "llm-cycle-task.md")
	llmCyclePrompt := `Do NOT run any shell commands. Use your file write tool to write these 2 files in the current working directory and then stop immediately:

1. MEMORY.md:
# Memory

- Keep health endpoints fast and dependency-free.

2. .foreman-kit/agent_proposals.json:
[
  {
    "change_name": "add-readiness-probe",
    "status": "proposed",
    "source": "exploration",
    "priority": "high",
    "impact_score": 8,
    "description": "Add readiness probe endpoint to backend service"
  },
  {
    "change_name": "clarify-plan-goal",
    "kind": "prompt",
    "prompt": "plan",
    "status": "proposed",
    "source": "cycle",
    "evidence": "Clarify goal line in plan prompt",
    "edits": [
      {
        "old": "Goal: {{goal}}",
        "new": "Goal: {{goal}}\nKeep every scenario concise and testable."
      }
    ]
  }
]
`
	if err := os.WriteFile(llmCyclePromptFile, []byte(llmCyclePrompt), 0o644); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"agent-exec", appDir, llmCyclePromptFile, "opencode,hermes"}, &out, &errBuf); code != 0 {
		t.Fatalf("agent-exec for cycle proposals failed (%d): stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}

	// Verify ScopeGuard accepts the real LLM's cycle edits
	st, err := store.Open(appDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var sgOut bytes.Buffer
	if err := cycle.ScopeGuard(ctx, appDir, "hot-cycle", st, &sgOut); err != nil {
		t.Fatalf("ScopeGuard failed after real LLM cycle edits: %v\n%s", err, sgOut.String())
	}

	// 3. Run `foreman-kit proposals review`
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"proposals", "review"}, &out, &errBuf); code != 0 {
		t.Fatalf("proposals review failed (%d): stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}

	// Verify add-readiness-probe was approved by Jev and queued into .foreman-kit/agent_backlog.json
	backlog, err := cycle.ReadJSONList(filepath.Join(appDir, ".foreman-kit", "agent_backlog.json"))
	if err != nil || len(backlog) != 1 || fmt.Sprint(backlog[0]["change_name"]) != "add-readiness-probe" {
		t.Fatalf("expected add-readiness-probe queued in .foreman-kit/agent_backlog.json, got %+v (err=%v)", backlog, err)
	}

	// Verify clarify-plan-goal prompt edit was applied and marked integrated
	proposalsAfter, err := cycle.ReadProposals(appDir)
	if err != nil {
		t.Fatal(err)
	}
	statusByName := make(map[string]string)
	for _, p := range proposalsAfter {
		statusByName[fmt.Sprint(p["change_name"])] = fmt.Sprint(p["status"])
	}
	if statusByName["add-readiness-probe"] != "queued" {
		t.Fatalf("expected add-readiness-probe status=queued, got %q", statusByName["add-readiness-probe"])
	}
	if statusByName["clarify-plan-goal"] != "integrated" {
		t.Fatalf("expected clarify-plan-goal status=integrated, got %q", statusByName["clarify-plan-goal"])
	}

	// 4. Run `foreman-kit status` and `foreman-kit progress --since 24h --json`
	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"status"}, &out, &errBuf); code != 0 {
		t.Fatalf("status failed (%d): stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}
	for _, expectedSection := range []string{"== Guards", "== Processes", "== Queues", "== Worktrees", "== Repositories"} {
		if !strings.Contains(out.String(), expectedSection) {
			t.Fatalf("expected %q in status output, got:\n%s", expectedSection, out.String())
		}
	}

	out.Reset()
	errBuf.Reset()
	if code := Run(ctx, []string{"progress", "--since", "24h", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("progress --json failed (%d): stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}
	var progData map[string]any
	if err := json.Unmarshal(out.Bytes(), &progData); err != nil {
		t.Fatalf("failed to parse progress --json output: %v\nraw:\n%s", err, out.String())
	}
	for _, k := range []string{"dashboard", "period", "health", "pending", "needs_human"} {
		if progData[k] == nil {
			t.Fatalf("missing key %q in progress --json output: %+v", k, progData)
		}
	}

	// Verify SQLite events and evaluation_cache were populated without error
	evts, err := st.QueryEvents(ctx, store.EventFilter{})
	if err != nil || len(evts) == 0 {
		t.Fatalf("expected events in SQLite store, got %d (err=%v)", len(evts), err)
	}
}
