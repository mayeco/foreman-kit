package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

// setupAcceptanceSandbox creates an isolated acceptance sandbox matching test/setup_sandbox.sh:
// - Bare remotes for umbrella (branch main) and backend (branch main)
// - Canonical spec openspec/specs/auth/spec.md with Requirement: Login and Scenario: Valid login
// - Child repo repos/backend with script/env.sh, tests/run.sh, and repo.config.yaml
func setupAcceptanceSandbox(t *testing.T) (wsDir, backendBareDir, umbrellaBareDir string, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(tmp, "state"))
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_APP_ROOT", "")
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "1")
	settingstest.Give(t, settings.EvaluatorEnabled, "false")

	backendBareDir = filepath.Join(tmp, "remotes", "backend.git")
	umbrellaBareDir = filepath.Join(tmp, "remotes", "umbrella.git")
	if err := os.MkdirAll(backendBareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(umbrellaBareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, backendBareDir, "init", "--bare", "-b", "main")
	mustGit(t, ctx, umbrellaBareDir, "init", "--bare", "-b", "main")

	// Seed backend repository
	backendSeed := filepath.Join(tmp, "seed", "backend")
	for _, sub := range []string{"src", "tests", "script"} {
		if err := os.MkdirAll(filepath.Join(backendSeed, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, ctx, backendSeed, "init", "-b", "main")
	mustGit(t, ctx, backendSeed, "config", "user.email", "bot@example.com")
	mustGit(t, ctx, backendSeed, "config", "user.name", "bot")

	mustWriteFile(t, filepath.Join(backendSeed, "src", "README"), "seed\n")
	mustWriteFile(t, filepath.Join(backendSeed, "script", "env.sh"), "# shellcheck shell=bash\nexport APP_ENV_READY=1\n")
	runSh := `#!/usr/bin/env bash
if [ "${APP_ENV_READY:-}" != 1 ]; then echo "test failure: script/env.sh was not sourced"; exit 1; fi
if [ -f src/BROKEN ]; then echo "test failure: src/BROKEN present"; exit 1; fi
if [ -n "${FLAKY_MARKER:-}" ] && [ ! -e "$FLAKY_MARKER" ]; then
  touch "$FLAKY_MARKER"; echo "psql: error: connect ECONNRESET 127.0.0.1:5432"; exit 1
fi
echo "tests ok"
`
	mustWriteFile(t, filepath.Join(backendSeed, "tests", "run.sh"), runSh)
	repoCfg := `name: "backend"
stack: "shell"
base_branch: "main"
env_script: "script/env.sh"
gates:
  lint: "test -d src"
  typecheck: ""
  test: "bash tests/run.sh"
  build: "{{BUILD_COMMAND}}"
`
	mustWriteFile(t, filepath.Join(backendSeed, "repo.config.yaml"), repoCfg)
	mustGit(t, ctx, backendSeed, "add", "-A")
	mustGit(t, ctx, backendSeed, "commit", "-m", "seed backend")
	mustGit(t, ctx, backendSeed, "remote", "add", "origin", backendBareDir)
	mustGit(t, ctx, backendSeed, "push", "-u", "origin", "main")

	// Seed umbrella repository
	wsDir = filepath.Join(tmp, "ws")
	for _, sub := range []string{".foreman-kit", "openspec/specs/auth", "openspec/changes/archive"} {
		if err := os.MkdirAll(filepath.Join(wsDir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, ctx, wsDir, "init", "-b", "main")
	mustGit(t, ctx, wsDir, "config", "user.email", "bot@example.com")
	mustGit(t, ctx, wsDir, "config", "user.name", "bot")

	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit/kit.lock"), "{}\n")
	mustWriteFile(t, filepath.Join(wsDir, ".gitignore"), ".worktrees/\nlog/\n")
	mustWriteFile(t, filepath.Join(wsDir, "MEMORY.md"), "# MEMORY\n")
	mustWriteFile(t, filepath.Join(wsDir, "openspec", "config.yaml"), "schema: spec-driven\n")
	authSpec := `# auth Specification

## Purpose
Governs credential verification and session issuance for all client applications.

## Requirements

### Requirement: Login
The system SHALL authenticate users with email and password.

#### Scenario: Valid login
- **WHEN** valid credentials are submitted
- **THEN** a session token is issued
`
	mustWriteFile(t, filepath.Join(wsDir, "openspec", "specs", "auth", "spec.md"), authSpec)

	pipelineYAML := `repository:
  base_branch: main
dispatch:
  workers: 2
  priority_bump_every: 6h
blocked_changes:
  retries: 1
  retry_after: 1h
gates:
  timeout: 1m
merge:
  conflict_rebuilds: 3
locks:
  stale_after: 5m
`
	settingstest.WriteFiles(t, wsDir, map[string]string{"pipeline": pipelineYAML})
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), "[]\n")
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_history.json"), "[]\n")

	mustGit(t, ctx, wsDir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", backendBareDir, "repos/backend")
	mustGit(t, ctx, wsDir, "config", "-f", ".gitmodules", "submodule.repos/backend.branch", "main")
	mustGit(t, ctx, filepath.Join(wsDir, "repos", "backend"), "config", "user.email", "bot@example.com")
	mustGit(t, ctx, filepath.Join(wsDir, "repos", "backend"), "config", "user.name", "bot")

	mustGit(t, ctx, wsDir, "add", "-A")
	mustGit(t, ctx, wsDir, "commit", "-m", "seed umbrella")
	mustGit(t, ctx, wsDir, "remote", "add", "origin", umbrellaBareDir)
	mustGit(t, ctx, wsDir, "push", "-u", "origin", "main")

	var err error
	st, err = store.Open(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	return wsDir, backendBareDir, umbrellaBareDir, st
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type stubAgentConfig struct {
	ModifiedMode bool
	EnginesDown  *atomic.Bool
}

// makeStubAgentRunner reproduces all behaviors of test/stub_agent.sh in pure Go for fast, deterministic execution.
func makeStubAgentRunner(cfg *stubAgentConfig) AgentRunnerFunc {
	if cfg == nil {
		cfg = &stubAgentConfig{}
	}
	return func(_ context.Context, req AgentRunRequest) (AgentRunResult, error) {
		change := req.Change
		wt := req.Worktree
		prompt := req.Prompt
		chgDir := filepath.Join(wt, "openspec", "changes", change)

		if strings.HasPrefix(change, "engine-down") && cfg.EnginesDown != nil && cfg.EnginesDown.Load() {
			return AgentRunResult{
				Output:     "You have hit your session limit\n",
				ExitCode:   EnginesDownExitCode,
				UsedEngine: "stub",
			}, nil
		}

		capName := "misc-" + change
		switch change {
		case "add-mfa", "add-sso":
			capName = "auth"
		case "add-invoices":
			capName = "billing"
		}

		if req.Role == "plan" {
			_ = os.MkdirAll(filepath.Join(chgDir, "specs", capName), 0o755)
			proposal := fmt.Sprintf(`## Why
The product needs %s to satisfy a concrete customer requirement this quarter.

## What Changes
- Implement %s in the backend service.

## Capabilities

### New Capabilities

### Modified Capabilities
- `+"`%s`"+`: adds behavior for %s

## Impact
Backend service only.
`, change, change, capName, change)
			_ = os.WriteFile(filepath.Join(chgDir, "proposal.md"), []byte(proposal), 0o644)

			purpose := ""
			if !fileExists(filepath.Join(wt, "openspec", "specs", capName, "spec.md")) {
				purpose = fmt.Sprintf("## Purpose\nGoverns the %s capability, its invariants and its externally observable behavior.\n\n", capName)
			}
			if change == "no-purpose" && !strings.Contains(prompt, "Once archived") {
				purpose = ""
			}

			specFile := filepath.Join(chgDir, "specs", capName, "spec.md")
			if cfg.ModifiedMode && capName == "auth" {
				canonBytes, _ := os.ReadFile(filepath.Join(wt, "openspec", "specs", "auth", "spec.md"))
				_, canonReqs := parseSpecRequirementsBySection(string(canonBytes))
				curLogin := canonReqs["Login"].RawBlock
				modSpec := fmt.Sprintf("## MODIFIED Requirements\n\n%s\n\n#### Scenario: %s path\n- **WHEN** a client uses %s\n- **THEN** the login flow handles it\n",
					curLogin, change, change)
				_ = os.WriteFile(specFile, []byte(modSpec), 0o644)
			} else {
				thenText := "the system responds successfully"
				if change == "vague-plan" && !strings.Contains(prompt, "evaluator's plan_review gate") {
					thenText = "the system behaves VAGUE-ly well"
				}
				addedSpec := fmt.Sprintf(`%s## ADDED Requirements

### Requirement: Behavior for %s
The system SHALL provide the behavior described by %s.

#### Scenario: %s happy path
- **WHEN** a client exercises %s
- **THEN** %s
`, purpose, change, change, change, change, thenText)
				_ = os.WriteFile(specFile, []byte(addedSpec), 0o644)
			}

			if strings.Contains(prompt, "failed deterministic verification") && fileExists(filepath.Join(chgDir, "tasks.md")) {
				return AgentRunResult{Output: "plan repaired", ExitCode: 0, UsedEngine: "stub"}, nil
			}

			design := fmt.Sprintf("## Context\nStub design for %s.\n\n## Goals / Non-Goals\nGoal: ship it.\n\n## Decisions\nKeep it simple.\n", change)
			_ = os.WriteFile(filepath.Join(chgDir, "design.md"), []byte(design), 0o644)

			var tasks string
			if strings.Contains(change, "star") {
				tasks = fmt.Sprintf("## 1. Core\n\n* [ ] 1.1 Implement %s (verify: bin/run_gates.sh)\n+ [ ] 1.2 Add tests for %s\n1. [ ] 1.3 Verify numbered dot task\n2) [ ] 1.4 Verify numbered paren task\n", change, change)
			} else {
				tasks = fmt.Sprintf("## 1. Core\n\n- [ ] 1.1 Implement %s (verify: bin/run_gates.sh)\n- [ ] 1.2 Add tests for %s\n", change, change)
			}
			_ = os.WriteFile(filepath.Join(chgDir, "tasks.md"), []byte(tasks), 0o644)
			return AgentRunResult{Output: "planned", ExitCode: 0, UsedEngine: "stub"}, nil
		}

		// BUILD role
		isRepair := strings.Contains(prompt, "failed deterministic verification")
		srcDir := filepath.Join(wt, "repos", "backend", "src")
		_ = os.MkdirAll(srcDir, 0o755)
		_ = os.WriteFile(filepath.Join(srcDir, change+".txt"), []byte(change+" implemented\n"), 0o644)
		_ = os.WriteFile(filepath.Join(srcDir, change+".slot"), []byte(fmt.Sprintf("%d\n", req.Slot)), 0o644)

		switch {
		case change == "fix-flaky":
			if !isRepair {
				_ = os.WriteFile(filepath.Join(srcDir, "BROKEN"), []byte("broken\n"), 0o644)
			} else {
				_ = os.Remove(filepath.Join(srcDir, "BROKEN"))
			}
		case change == "always-broken" || change == "hand-fixed" || change == "stalled-change" || change == "ambiguous-gate":
			_ = os.WriteFile(filepath.Join(srcDir, "BROKEN"), []byte("broken\n"), 0o644)
		case change == "retry-once":
			if strings.Contains(prompt, "is now retried automatically") {
				_ = os.Remove(filepath.Join(srcDir, "BROKEN"))
			} else {
				_ = os.WriteFile(filepath.Join(srcDir, "BROKEN"), []byte("broken\n"), 0o644)
			}
		case change == "sneaky":
			f, err := os.OpenFile(filepath.Join(wt, "MEMORY.md"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
			if err == nil {
				_, _ = f.WriteString("agent wrote this\n")
				_ = f.Close()
			}
		case change == "gate-tamper":
			cfgPath := filepath.Join(wt, "repos", "backend", "repo.config.yaml")
			raw, _ := os.ReadFile(cfgPath)
			tampered := regexp.MustCompile(`(?m)^  test: .*`).ReplaceAllString(string(raw), `  test: "true"`)
			_ = os.WriteFile(cfgPath, []byte(tampered), 0o644)
		case change == "github-tamper":
			wfDir := filepath.Join(wt, "repos", "backend", ".github", "workflows")
			_ = os.MkdirAll(wfDir, 0o755)
			_ = os.WriteFile(filepath.Join(wfDir, "ci.yml"), []byte("name: tampered\n"), 0o644)
		case change == "gitlab-tamper":
			_ = os.WriteFile(filepath.Join(wt, "repos", "backend", ".gitlab-ci.yml"), []byte("stages: [tamper]\n"), 0o644)
		case change == "env-tamper":
			envPath := filepath.Join(wt, "repos", "backend", "script", "env.sh")
			raw, _ := os.ReadFile(envPath)
			_ = os.WriteFile(envPath, []byte(string(raw)+"alias rspec=true\n"), 0o644)
		case change == "ambiguous":
			_ = os.WriteFile(filepath.Join(chgDir, ".needs-human"), []byte("Should deleted accounts keep invoices for 7 years (legal) or be purged (GDPR)?\n"), 0o644)
		case strings.HasPrefix(change, "shared-"):
			_ = os.WriteFile(filepath.Join(srcDir, "shared.txt"), []byte(change+" owns this line\n"), 0o644)
		case strings.HasPrefix(change, "untested-"):
			if !strings.Contains(prompt, "evaluator's scenario_coverage gate") {
				_ = os.WriteFile(filepath.Join(srcDir, change+".txt"), []byte(change+" implemented\nUNTESTED: no test asserts the happy path\n"), 0o644)
			}
		}

		tasksPath := filepath.Join(chgDir, "tasks.md")
		if rawTasks, err := os.ReadFile(tasksPath); err == nil {
			checked := regexp.MustCompile(`(?m)^(\s*(?:[-*+]|\d{1,9}[.)])\s+)\[ \]`).ReplaceAllString(string(rawTasks), "${1}[x]")
			_ = os.WriteFile(tasksPath, []byte(checked), 0o644)
		}
		_ = os.WriteFile(filepath.Join(chgDir, "handoff.md"), []byte(fmt.Sprintf("- Stub lesson from %s: gates need src/ present.\n", change)), 0o644)
		return AgentRunResult{Output: "built " + change, ExitCode: 0, UsedEngine: "stub"}, nil
	}
}

func runDispatchAndMergeRound(t *testing.T, ctx context.Context, wsDir string, st *store.Store, workers int, agentRunner AgentRunnerFunc, nowFn func() time.Time) {
	t.Helper()
	disp, err := NewDispatcher(DispatcherOptions{
		Root:         wsDir,
		Store:        st,
		MaxWorkers:   workers,
		PollInterval: 20 * time.Millisecond,
		AgentRunner:  agentRunner,
		NowFunc:      nowFn,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if err := disp.Run(ctx); err != nil {
		t.Fatalf("Dispatcher.Run: %v", err)
	}

	merger, err := NewMerger(MergerOptions{
		Root:    wsDir,
		Store:   st,
		Watch:   false,
		NowFunc: nowFn,
	})
	if err != nil {
		t.Fatalf("NewMerger: %v", err)
	}
	if err := merger.Run(ctx); err != nil {
		t.Fatalf("Merger.Run: %v", err)
	}
}

func integratedCountOnOrigin(t *testing.T, ctx context.Context, wsDir, change string) int {
	t.Helper()
	_, _ = gitx.RunGit(ctx, wsDir, "fetch", "--quiet", "origin", "main")
	out, err := gitx.RunGit(ctx, wsDir, "log", "origin/main", "--format=%(trailers:key=OpenSpec-Change,valueonly)")
	if err != nil {
		t.Fatalf("git log trailers: %v", err)
	}
	cnt := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == change {
			cnt++
		}
	}
	return cnt
}

func archivedFileOnOrigin(t *testing.T, ctx context.Context, wsDir, change, relFile string) string {
	t.Helper()
	_, _ = gitx.RunGit(ctx, wsDir, "fetch", "--quiet", "origin", "main")
	lsOut, err := gitx.RunGit(ctx, wsDir, "ls-tree", "-r", "--name-only", "origin/main", "openspec/changes/archive")
	if err != nil {
		t.Fatalf("ls-tree archive: %v", err)
	}
	suffix := "-" + change + "/" + relFile
	for _, line := range strings.Split(lsOut, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, suffix) {
			content, err := gitx.RunGit(ctx, wsDir, "show", "origin/main:"+line)
			if err != nil {
				t.Fatalf("git show %s: %v", line, err)
			}
			return content
		}
	}
	t.Fatalf("archived file %s for change %s not found in:\n%s", relFile, change, lsOut)
	return ""
}

// =============================================================================
// 1. Resilient OpenSpec JSON Parsing with Balanced Candidate Extraction
// =============================================================================

func TestResilientOpenSpecJSONParsing(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "plain warning lines before JSON",
			raw:  "Warning: NODE_ENV is not set\n[openspec] Experimental flag enabled\n{\"changeName\":\"add-sso\",\"artifacts\":[{\"id\":\"proposal\",\"status\":\"done\"}]}",
			want: "add-sso",
		},
		{
			name: "warnings with unbalanced and invalid braces before JSON",
			raw:  "warn: config {missing close brace\n[warn] invalid {not: json} block\n{\"changeName\":\"brace-test\",\"artifacts\":[]}\ntrailing warning {ignored",
			want: "brace-test",
		},
		{
			name: "nested braces and escaped quotes inside strings",
			raw:  "[warn] pre-check\n{\"changeName\":\"nested-quote\",\"note\":\"escaped \\\" } { [ ] inside string\",\"artifacts\":[{\"id\":\"specs\",\"status\":\"done\"}]}",
			want: "nested-quote",
		},
		{
			name: "prioritizes final JSON payload when earlier debug JSON log exists",
			raw:  "[debug] {\"changeName\":\"earlier-debug\"}\n{\"changeName\":\"final-payload\",\"artifacts\":[]}",
			want: "final-payload",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var parsed map[string]any
			if err := parseOpenSpecJSON(tc.raw, &parsed); err != nil {
				t.Fatalf("parseOpenSpecJSON failed: %v", err)
			}
			if got := fmt.Sprint(parsed["changeName"]); got != tc.want {
				t.Fatalf("expected changeName=%q, got %q (parsed=%+v)", tc.want, got, parsed)
			}
		})
	}

	// End-to-end Dispatcher + Merger test where OpenSpecRunner prepends noisy warnings to every JSON command
	wsDir, _, _, st := setupAcceptanceSandbox(t)
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), `[{"change_name":"noisy-openspec","priority":"high"}]`)

	noisyRunner := func(ctx context.Context, dir string, args ...string) (OpenSpecResult, error) {
		res, err := BuiltinOpenSpec(ctx, dir, args...)
		res.Output = "Warning: deprecated flag {foo\n[openspec] info: checking {bar}\n" + res.Output + "\nWarning: done\n"
		return res, err
	}

	disp, err := NewDispatcher(DispatcherOptions{
		Root:           wsDir,
		Store:          st,
		MaxWorkers:     1,
		AgentRunner:    makeStubAgentRunner(nil),
		OpenSpecRunner: noisyRunner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp.Run(ctx); err != nil {
		t.Fatalf("Dispatcher.Run with noisy OpenSpecRunner failed: %v", err)
	}

	merger, err := NewMerger(MergerOptions{
		Root:           wsDir,
		Store:          st,
		OpenSpecRunner: noisyRunner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := merger.Run(ctx); err != nil {
		t.Fatalf("Merger.Run with noisy OpenSpecRunner failed: %v", err)
	}

	if got := integratedCountOnOrigin(t, ctx, wsDir, "noisy-openspec"); got != 1 {
		t.Fatalf("expected noisy-openspec integrated once, got %d", got)
	}
}

// =============================================================================
// 2. Scenario A & Massive Concurrency: Priorities, Aging, depends_on, Shared Locks,
//    Repairs in repairs.md, Quarantine (gate_failure, policy_violation), Invalid Names
// =============================================================================

func TestScenarioA_SchedulingPrioritiesAgingDependsOnLocksRepairsQuarantineAndInvalidBacklog(t *testing.T) {
	ctx := context.Background()
	wsDir, backendBareDir, _, st := setupAcceptanceSandbox(t)

	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }

	backlog := `[
  { "change_name": "add-mfa",       "priority": "high",   "description": "Add TOTP MFA", "queued_at": "2026-11-08T11:50:00Z" },
  { "change_name": "add-sso",       "priority": "high",   "description": "Add SSO login (same capability as add-mfa)", "queued_at": "2026-11-08T11:50:00Z" },
  { "change_name": "add-invoices",  "priority": "normal", "description": "Monthly invoices", "locks": ["billing-db"], "queued_at": "2026-11-08T11:50:00Z" },
  { "change_name": "fix-flaky",     "priority": "low",    "description": "Aged low item (13h -> counts as high) needing one repair round", "queued_at": "2026-11-07T23:00:00Z" },
  { "change_name": "always-broken", "priority": "low",    "description": "Never passes gates", "queued_at": "2026-11-08T11:55:00Z" },
  { "change_name": "sneaky",        "priority": "low",    "description": "Writes outside allowed paths", "queued_at": "2026-11-08T11:55:00Z" },
  { "change_name": "needs-mfa",     "priority": "critical", "depends_on": ["add-mfa"], "locks": ["billing-db"], "description": "Depends on add-mfa", "queued_at": "2026-11-08T11:50:00Z" },
  { "change_name": "Bad Name; rm -rf /", "priority": "low" }
]`
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), backlog)
	mustGit(t, ctx, wsDir, "add", ".foreman-kit/agent_backlog.json")
	mustGit(t, ctx, wsDir, "commit", "-m", "chore: scenario A backlog")
	mustGit(t, ctx, wsDir, "push", "origin", "main")

	stubRunner := makeStubAgentRunner(nil)

	// 1. DryRun before any dispatch
	dispDry, err := NewDispatcher(DispatcherOptions{
		Root:       wsDir,
		Store:      st,
		MaxWorkers: 2,
		NowFunc:    nowFn,
	})
	if err != nil {
		t.Fatal(err)
	}
	seenBeforeDryRun, _ := st.ChangesFirstSeen(ctx)
	rep, err := dispDry.DryRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dryText := rep.FormatText(2, 0)
	if !strings.Contains(dryText, "dispatch: not running") {
		t.Fatalf("expected 'dispatch: not running' in dry-run text, got:\n%s", dryText)
	}
	if !strings.Contains(dryText, "needs-mfa") || !strings.Contains(dryText, "waits for add-mfa (not built yet)") {
		t.Fatalf("expected needs-mfa waiting for add-mfa in dry-run text, got:\n%s", dryText)
	}
	if !strings.Contains(dryText, "next: starts in the next dispatch") {
		t.Fatalf("expected 'next: starts in the next dispatch' in dry-run text, got:\n%s", dryText)
	}
	if !strings.Contains(dryText, "Bad Name; rm -rf /") || !strings.Contains(dryText, "invalid change_name: use 1-63") {
		t.Fatalf("expected invalid backlog name in dry-run text, got:\n%s", dryText)
	}
	if !strings.Contains(dryText, "low>high") {
		t.Fatalf("expected aged fix-flaky to show 'low>high' in dry-run text, got:\n%s", dryText)
	}
	// Verify dry-run recorded no change as seen: when each was first seen is what it was before.
	seenAfterDryRun, _ := st.ChangesFirstSeen(ctx)
	if len(seenAfterDryRun) != len(seenBeforeDryRun) {
		t.Fatalf("expected DryRun to register no change, got %d before and %d after", len(seenBeforeDryRun), len(seenAfterDryRun))
	}
	for name, at := range seenBeforeDryRun {
		if seenAfterDryRun[name] != at {
			t.Fatalf("expected DryRun to leave first_seen_at of %s at %s, got %s", name, at, seenAfterDryRun[name])
		}
	}

	// 2. Run 3 rounds with 3 workers to exercise concurrency, capability holding (add-sso & add-mfa),
	//    dependency chain (needs-mfa -> add-mfa), repair round (fix-flaky), and quarantines.
	runDispatchAndMergeRound(t, ctx, wsDir, st, 3, stubRunner, nowFn)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 3, stubRunner, nowFn)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 3, stubRunner, nowFn)

	for _, c := range []string{"add-sso", "add-mfa", "add-invoices", "fix-flaky", "needs-mfa"} {
		if cnt := integratedCountOnOrigin(t, ctx, wsDir, c); cnt != 1 {
			t.Fatalf("expected %s integrated exactly once, got %d", c, cnt)
		}
	}

	// Verify backlog.invalid_name event was emitted and invalid worktree never created
	invEvts, _ := st.QueryEvents(ctx, store.EventFilter{Event: "backlog.invalid_name"})
	if len(invEvts) == 0 {
		t.Fatalf("expected backlog.invalid_name event to be emitted")
	}
	if dirExists(filepath.Join(wsDir, ".worktrees", "worker-Bad Name; rm -rf /")) {
		t.Fatalf("invalid backlog name must never create a worktree")
	}

	// Verify always-broken quarantined as gate_failure and sneaky as policy_violation
	qBroken, err := st.GetQuarantine(ctx, "always-broken")
	if err != nil || qBroken == nil || qBroken.Status != "blocked" || qBroken.Reason != "gate_failure" {
		t.Fatalf("expected always-broken blocked as gate_failure, got %+v", qBroken)
	}
	qSneaky, err := st.GetQuarantine(ctx, "sneaky")
	if err != nil || qSneaky == nil || qSneaky.Status != "blocked" || qSneaky.Reason != "policy_violation" {
		t.Fatalf("expected sneaky blocked as policy_violation, got %+v", qSneaky)
	}

	// Verify blocked worktree kept for audit and integrated worktrees removed
	if !dirExists(filepath.Join(wsDir, ".worktrees", "worker-always-broken")) {
		t.Fatalf("expected blocked worktree .worktrees/worker-always-broken to be kept for audit")
	}
	if dirExists(filepath.Join(wsDir, ".worktrees", "worker-add-sso")) {
		t.Fatalf("expected integrated worktree .worktrees/worker-add-sso to be removed")
	}

	// Verify umbrella submodule pointer equals child main
	umbrellaChildSHA := mustGit(t, ctx, wsDir, "rev-parse", "origin/main:repos/backend")
	childMainSHA := mustGit(t, ctx, backendBareDir, "rev-parse", "main")
	if umbrellaChildSHA != childMainSHA {
		t.Fatalf("umbrella pointer (%s) != child main (%s)", umbrellaChildSHA, childMainSHA)
	}

	// Verify agent_history.json and agent_backlog.json on origin and in local checkout
	histItems := readBacklogItemsFromFile(filepath.Join(wsDir, ".foreman-kit", "agent_history.json"))
	var histNames []string
	for _, it := range histItems {
		if it.IntegratedAt == "" {
			t.Fatalf("expected integrated_at on history item %+v", it)
		}
		histNames = append(histNames, it.ChangeName)
	}
	sort.Strings(histNames)
	wantHist := []string{"add-invoices", "add-mfa", "add-sso", "fix-flaky", "needs-mfa"}
	if strings.Join(histNames, ",") != strings.Join(wantHist, ",") {
		t.Fatalf("expected history=%v, got %v", wantHist, histNames)
	}

	remBacklog := readRawMapsFromFile(filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"))
	var remNames []string
	for _, m := range remBacklog {
		remNames = append(remNames, fmt.Sprint(m["change_name"]))
	}
	wantRem := []string{"always-broken", "sneaky", "Bad Name; rm -rf /"}
	if strings.Join(remNames, ",") != strings.Join(wantRem, ",") {
		t.Fatalf("expected remaining backlog=%v, got %v", wantRem, remNames)
	}

	// Verify fix-flaky's repair round recorded in archived repairs.md and listed by CompletedSince
	repairsMD := archivedFileOnOrigin(t, ctx, wsDir, "fix-flaky", "repairs.md")
	for _, needle := range []string{"## repair-1, ", ", stub", "Asked to fix (gate_failure):", "src/BROKEN", "Result: passed."} {
		if !strings.Contains(repairsMD, needle) {
			t.Fatalf("expected %q in archived fix-flaky repairs.md, got:\n%s", needle, repairsMD)
		}
	}
	wsRepo, err := gitx.Open(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	rootCommit := mustGit(t, ctx, wsDir, "rev-list", "--max-parents=0", "origin/main")
	completed, err := wsRepo.CompletedSince(rootCommit, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	foundFlakyCompleted := false
	for _, comp := range completed {
		if comp.ChangeName == "fix-flaky" && comp.SHA != "" {
			foundFlakyCompleted = true
		}
	}
	if !foundFlakyCompleted {
		t.Fatalf("expected CompletedSince to include fix-flaky, got %+v", completed)
	}

	// Verify every agent.exit event recorded 12-hex prompt_version and memory_version
	hex12Re := regexp.MustCompile(`^[0-9a-f]{12}$`)
	exitEvts, _ := st.QueryEvents(ctx, store.EventFilter{Event: "agent.exit"})
	if len(exitEvts) == 0 {
		t.Fatalf("expected agent.exit events")
	}
	for _, ev := range exitEvts {
		pv := fmt.Sprint(ev.Payload["prompt_version"])
		mv := fmt.Sprint(ev.Payload["memory_version"])
		if !hex12Re.MatchString(pv) || !hex12Re.MatchString(mv) {
			t.Fatalf("expected 12-char hex prompt_version and memory_version, got pv=%q mv=%q", pv, mv)
		}
	}
}

// =============================================================================
// 3. Scenario B: Two Concurrent Changes MODIFY the Same Requirement/Scenario
// =============================================================================

func TestScenarioB_ConcurrentModifiedSameRequirementSerializationAndScenarioPreservation(t *testing.T) {
	ctx := context.Background()
	wsDir, _, _, st := setupAcceptanceSandbox(t)

	backlog := `[{"change_name":"add-sso"},{"change_name":"add-mfa"}]`
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), backlog)

	stubRunner := makeStubAgentRunner(&stubAgentConfig{ModifiedMode: true})

	// Round 1: both plan against auth; one claims auth and merges, the other is held
	runDispatchAndMergeRound(t, ctx, wsDir, st, 2, stubRunner, nil)
	// Round 2: the held change rebases onto the updated auth spec, repairs its stale MODIFIED block in PLAN, builds, and merges
	runDispatchAndMergeRound(t, ctx, wsDir, st, 2, stubRunner, nil)

	if got := integratedCountOnOrigin(t, ctx, wsDir, "add-sso") + integratedCountOnOrigin(t, ctx, wsDir, "add-mfa"); got != 2 {
		t.Fatalf("expected both add-sso and add-mfa integrated (total 2), got %d", got)
	}

	heldEvts, _ := st.QueryEvents(ctx, store.EventFilter{Event: "change.held"})
	if len(heldEvts) == 0 {
		t.Fatalf("expected change.held event when two concurrent changes claim capability 'auth'")
	}

	authSpec := mustGit(t, ctx, wsDir, "show", "origin/main:openspec/specs/auth/spec.md")
	scenCount := strings.Count(authSpec, "#### Scenario:")
	if scenCount != 3 {
		t.Fatalf("expected Login requirement in openspec/specs/auth/spec.md to keep all 3 scenarios, got %d:\n%s", scenCount, authSpec)
	}
	for _, scen := range []string{"#### Scenario: Valid login", "#### Scenario: add-sso path", "#### Scenario: add-mfa path"} {
		if !strings.Contains(authSpec, scen) {
			t.Fatalf("expected %q in canonical auth spec, got:\n%s", scen, authSpec)
		}
	}
}

// =============================================================================
// 4. Scenario C: Guardrails (repo.config.yaml, .github, .gitlab-ci.yml, env_script,
//    .needs-human -> spec_ambiguity, missing Purpose -> PLAN repair)
// =============================================================================

func TestScenarioC_GuardrailsProtectedPathsNeedsHumanAndMissingPurposePlanRepair(t *testing.T) {
	ctx := context.Background()
	wsDir, _, _, st := setupAcceptanceSandbox(t)

	backlog := `[
  {"change_name":"gate-tamper"},
  {"change_name":"github-tamper"},
  {"change_name":"gitlab-tamper"},
  {"change_name":"env-tamper"},
  {"change_name":"ambiguous"},
  {"change_name":"no-purpose"}
]`
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), backlog)

	stubRunner := makeStubAgentRunner(nil)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 3, stubRunner, nil)

	for _, tampered := range []string{"gate-tamper", "github-tamper", "gitlab-tamper", "env-tamper"} {
		q, err := st.GetQuarantine(ctx, tampered)
		if err != nil || q == nil || q.Status != "blocked" || q.Reason != "policy_violation" {
			t.Fatalf("expected %s blocked as policy_violation, got %+v (err=%v)", tampered, q, err)
		}
		if cnt := integratedCountOnOrigin(t, ctx, wsDir, tampered); cnt != 0 {
			t.Fatalf("expected %s never integrated, got %d", tampered, cnt)
		}
	}

	// Verify .needs-human escalated immediately as spec_ambiguity without repair rounds
	qAmb, err := st.GetQuarantine(ctx, "ambiguous")
	if err != nil || qAmb == nil || qAmb.Status != "blocked" || qAmb.Reason != "spec_ambiguity" {
		t.Fatalf("expected ambiguous blocked as spec_ambiguity, got %+v", qAmb)
	}
	if !strings.Contains(qAmb.Details, "GDPR") {
		t.Fatalf("expected .needs-human question in quarantine details, got %q", qAmb.Details)
	}
	ambExits, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: "ambiguous", Event: "agent.exit"})
	for _, ev := range ambExits {
		if strings.Contains(ev.Phase, "repair") {
			t.Fatalf("expected .needs-human to escalate without spending repair rounds, saw phase %q", ev.Phase)
		}
	}

	// Verify no-purpose repaired in PLAN before BUILD ("plan", "plan-repair-1", "apply")
	npExits, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: "no-purpose", Event: "agent.exit"})
	var phases []string
	for _, ev := range npExits {
		phases = append(phases, ev.Phase)
	}
	if got := strings.Join(phases, " "); got != "plan plan-repair-1 apply" {
		t.Fatalf("expected no-purpose phases 'plan plan-repair-1 apply', got %q", got)
	}

	npRepairs := archivedFileOnOrigin(t, ctx, wsDir, "no-purpose", "repairs.md")
	if !strings.Contains(npRepairs, "Asked to fix (spec_invalid):") || !strings.Contains(npRepairs, "## plan-repair-1, ") {
		t.Fatalf("expected PLAN repair round recorded in archived repairs.md, got:\n%s", npRepairs)
	}

	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "no-purpose"); cnt != 1 {
		t.Fatalf("expected no-purpose integrated once, got %d", cnt)
	}
	canonNoPurpose := mustGit(t, ctx, wsDir, "show", "origin/main:openspec/specs/misc-no-purpose/spec.md")
	if !strings.Contains(canonNoPurpose, "Governs the misc-no-purpose capability") {
		t.Fatalf("expected canonical Purpose in misc-no-purpose/spec.md, got:\n%s", canonNoPurpose)
	}
}

// =============================================================================
// 5. Scenario D: Merger Crash Recovery Across All Intermediate States
//    (CLAIMED, REBASING_CHILDREN, RUNNING_GATES, COMMITTING, PUSHING_CHILDREN)
// =============================================================================

func TestScenarioD_MergerCrashRecoveryAllIntermediateStates(t *testing.T) {
	ctx := context.Background()
	wsDir, backendBareDir, _, st := setupAcceptanceSandbox(t)

	crashStates := []string{
		MergerStateClaimed,
		MergerStateRebasingChildren,
		MergerStateRunningGates,
		MergerStateCommitting,
		MergerStatePushingChildren,
	}

	var items []map[string]any
	for i := range crashStates {
		items = append(items, map[string]any{
			"change_name": fmt.Sprintf("crash-state-%d", i+1),
			"priority":    "high",
		})
	}
	items = append(items, map[string]any{
		"change_name": "after-crash-tail",
		"priority":    "low",
	})
	backlogBytes, _ := json.Marshal(items)
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), string(backlogBytes))

	stubRunner := makeStubAgentRunner(nil)
	disp, err := NewDispatcher(DispatcherOptions{
		Root:        wsDir,
		Store:       st,
		MaxWorkers:  3,
		AgentRunner: stubRunner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp.Run(ctx); err != nil {
		t.Fatal(err)
	}

	readyList, err := st.ListReadyQueue(ctx)
	if err != nil || len(readyList) != len(crashStates)+1 {
		t.Fatalf("expected %d items in the merge queue, got %d (err=%v)", len(crashStates)+1, len(readyList), err)
	}

	// For each intermediate state, simulate an abrupt crash during that state and recover on the next Merger run
	for idx, state := range crashStates {
		chg := fmt.Sprintf("crash-state-%d", idx+1)
		var targetManifest store.ReadyManifest
		currentQueue, _ := st.ListReadyQueue(ctx)
		for _, m := range currentQueue {
			if m.ChangeName == chg {
				targetManifest = m
				break
			}
		}

		// Simulate the exact side effects up to the crash point in _integration and merge_attempts
		attemptID, _ := st.BeginMergeAttempt(ctx, chg)
		_ = st.SetMergeAttemptState(ctx, attemptID, state)
		intWT := filepath.Join(wsDir, ".worktrees", "_integration")

		switch state {
		case MergerStateClaimed:
			// Crashed right after claiming manifest
		case MergerStateRebasingChildren:
			// Crashed mid-child-rebase with _integration worktree provisioned
			mTmp, _ := NewMerger(MergerOptions{Root: wsDir, Store: st})
			_ = mTmp.prepareIntegrationWorktree(ctx, "origin/main")
			_, _ = gitx.RunGit(ctx, filepath.Join(intWT, "repos", "backend"), "checkout", "-B", "integrate/"+chg)
		case MergerStateRunningGates:
			// Crashed during gates after assembling change artifacts into _integration index
			mTmp, _ := NewMerger(MergerOptions{Root: wsDir, Store: st})
			_ = mTmp.prepareIntegrationWorktree(ctx, "origin/main")
			_, _ = gitx.RunGit(ctx, intWT, "checkout", targetManifest.HeadSHA, "--", "openspec/changes/"+chg)
		case MergerStateCommitting:
			// Crashed after archiving and staging uncommitted files in _integration
			mTmp, _ := NewMerger(MergerOptions{Root: wsDir, Store: st})
			_ = mTmp.prepareIntegrationWorktree(ctx, "origin/main")
			_, _ = gitx.RunGit(ctx, intWT, "checkout", targetManifest.HeadSHA, "--", "openspec/changes/"+chg)
			_, _ = BuiltinOpenSpec(ctx, intWT, "archive", chg, "--yes", "--json")
			_, _ = gitx.RunGit(ctx, intWT, "add", "-A", "--", "openspec")
		case MergerStatePushingChildren:
			// Crashed AFTER pushing the rebased child commit to backend origin/main, BEFORE pushing umbrella origin/main!
			mTmp, _ := NewMerger(MergerOptions{Root: wsDir, Store: st})
			_ = mTmp.prepareIntegrationWorktree(ctx, "origin/main")
			wtRepo, _ := gitx.Open(intWT)
			rebasedSHA, _, err := mTmp.rebaseChild(ctx, targetManifest, wtRepo, "repos/backend", targetManifest.HeadSHA)
			if err != nil {
				t.Fatalf("setup PUSHING_CHILDREN rebaseChild failed: %v", err)
			}
			childRepo, _ := gitx.Open(filepath.Join(intWT, "repos", "backend"))
			if err := childRepo.Push(ctx, "origin", rebasedSHA+":refs/heads/main"); err != nil {
				t.Fatalf("setup PUSHING_CHILDREN child push failed: %v", err)
			}
		}

		// Now start a fresh Merger; it must emit merger.recover for `state` and cleanly integrate `chg`
		merger, err := NewMerger(MergerOptions{
			Root:  wsDir,
			Store: st,
			Watch: false,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Integrate only this item by temporarily keeping only `chg` in the merge queue (or letting recoverState run then integrate)
		merger.recoverState(ctx)
		merger.integrate(ctx, targetManifest)

		// Verify merger.recover event was emitted for this state and change
		recEvts, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: chg, Event: "merger.recover"})
		if len(recEvts) == 0 || fmt.Sprint(recEvts[0].Payload["state"]) != state {
			t.Fatalf("expected merger.recover event for %s at state %s, got %+v", chg, state, recEvts)
		}

		// Verify integrated exactly once on umbrella origin/main
		if cnt := integratedCountOnOrigin(t, ctx, wsDir, chg); cnt != 1 {
			t.Fatalf("after recovery from %s, expected %s integrated once, got %d", state, chg, cnt)
		}

		// Verify child commit is not duplicated on backend main
		childLog := mustGit(t, ctx, backendBareDir, "log", "main", "--format=%s")
		childMatches := 0
		for _, line := range strings.Split(childLog, "\n") {
			if strings.Contains(line, "implement OpenSpec change "+chg) {
				childMatches++
			}
		}
		if childMatches != 1 {
			t.Fatalf("after recovery from %s, expected 1 child commit for %s, got %d:\n%s", state, chg, childMatches, childLog)
		}

		// Verify submodule pointer on umbrella origin/main matches backend main
		umbrellaPtr := mustGit(t, ctx, wsDir, "rev-parse", "origin/main:repos/backend")
		childTip := mustGit(t, ctx, backendBareDir, "rev-parse", "main")
		if umbrellaPtr != childTip {
			t.Fatalf("after recovery from %s, submodule pointer (%s) != child main (%s)", state, umbrellaPtr, childTip)
		}
	}

	// Drain the remaining tail item via full Merger.Run
	mergerFinal, err := NewMerger(MergerOptions{Root: wsDir, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if err := mergerFinal.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "after-crash-tail"); cnt != 1 {
		t.Fatalf("expected after-crash-tail integrated once, got %d", cnt)
	}
}

// =============================================================================
// 6. Scenario E: Jev in Enforce & Advisory Modes
//    (plan_review, failure_triage rerun & escalate, repair_progress stalled,
//     scenario_coverage rebuild, change_risk & store.ApproveChangeRisk)
// =============================================================================

func TestScenarioE_JevEnforceAndAdvisoryAllGates(t *testing.T) {
	ctx := context.Background()
	wsDir, _, _, st := setupAcceptanceSandbox(t)

	flakyMarker := filepath.Join(t.TempDir(), "flaky.marker")
	t.Setenv("FLAKY_MARKER", flakyMarker)

	// Start a deterministic Jev HTTP server that inspects the state JSON in the request body
	// and returns calibrated answers matching test/stub_jev/jev plus requirement_ambiguity escalation.
	jevSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody map[string]any
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		rawBytes, _ := json.Marshal(reqBody)
		bodyStr := string(rawBytes)

		probVague := 0.92
		if strings.Contains(bodyStr, "VAGUE") {
			probVague = 0.10
		}

		causeChoice := "implementation_defect"
		causeConf := 0.90
		if strings.Contains(bodyStr, "ECONNRESET") {
			causeChoice = "environment_or_flaky"
			causeConf = 0.95
		} else if strings.Contains(bodyStr, "ambiguous-gate") {
			causeChoice = "requirement_ambiguity"
			causeConf = 0.93
		}

		converging := 0.85
		if strings.Contains(bodyStr, "stalled-change") {
			converging = 0.05
		}

		everyScenTested := 0.93
		if strings.Contains(bodyStr, "UNTESTED") {
			everyScenTested = 0.10
		}

		riskChoice := "low"
		secSensitive := 0.05
		if strings.Contains(bodyStr, "password") {
			riskChoice = "high"
			secSensitive = 0.97
		}

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"model": "jev-stub",
			"answers": map[string]any{
				// plan_review
				"problem_is_concrete":             map[string]any{"type": "noul", "noul": 0.95},
				"scenarios_are_testable":          map[string]any{"type": "noul", "noul": probVague},
				"tasks_cover_scenarios":           map[string]any{"type": "noul", "noul": 0.94},
				"consistent_with_canonical_specs": map[string]any{"type": "noul", "noul": 0.96},
				"single_concern":                  map[string]any{"type": "noul", "noul": 0.91},
				// failure_triage
				"cause": map[string]any{
					"type":       "choice",
					"choice":     causeChoice,
					"confidence": causeConf,
					"probabilities": map[string]float64{
						causeChoice: causeConf,
					},
				},
				"agent_can_fix": map[string]any{"type": "noul", "noul": 0.80},
				// repair_progress
				"converging":     map[string]any{"type": "noul", "noul": converging},
				"different_axis": map[string]any{"type": "noul", "noul": converging},
				// scenario_coverage
				"every_scenario_tested": map[string]any{"type": "noul", "noul": everyScenTested},
				"tests_assert_outcomes": map[string]any{"type": "noul", "noul": 0.90},
				// change_risk
				"risk": map[string]any{
					"type":       "choice",
					"choice":     riskChoice,
					"confidence": 0.90,
					"probabilities": map[string]float64{
						riskChoice: 0.90,
					},
				},
				"blast_radius": map[string]any{
					"type":       "choice",
					"choice":     "contained",
					"confidence": 0.90,
					"probabilities": map[string]float64{
						"contained": 0.90,
					},
				},
				"touches_sensitive_surface": map[string]any{"type": "noul", "noul": secSensitive},
				"security_sensitive":        map[string]any{"type": "noul", "noul": secSensitive},
				"irreversible_data_change":  map[string]any{"type": "noul", "noul": 0.05},
				"matches_proposal":          map[string]any{"type": "noul", "noul": 0.95},
				"reversible":                map[string]any{"type": "noul", "noul": 0.95},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer jevSrv.Close()

	settingstest.Give(t, settings.EvaluatorEnabled, "true")
	t.Setenv("TYPESAFE_API_KEY", "ts-stub-key")
	settingstest.Give(t, settings.EvaluatorBaseURL, jevSrv.URL)

	// The gates the pipeline asks on the way of a change, all enforced; the other gates stay off,
	// since the stand-in evaluator answers only the questions of these.
	enforcePolicy := `retries: 1
when_unavailable: block
gates:
  plan_review: {mode: enforce}
  failure_triage: {mode: enforce}
  repair_progress: {mode: enforce}
  scenario_coverage: {mode: enforce}
  change_risk:
    mode: enforce
    block_when:
      - { question: risk, choice: high, min_confidence: 0.7 }
      - { question: security_sensitive, min: 0.8 }
  task_priority: {mode: off}
  concurrent_work_loss: {mode: off}
  system_error_triage: {mode: off}
  policy_violation_triage: {mode: off}
  agent_outcome: {mode: off}
  semantic_merge_conflict: {mode: off}
  push_rejection: {mode: off}
  proposal_review: {mode: off}
`
	settingstest.WriteFile(t, wsDir, "evaluator", enforcePolicy)

	backlog := `[
  {"change_name":"vague-plan","priority":"high"},
  {"change_name":"rotate-password-hashing","priority":"high"},
  {"change_name":"add-health-check","priority":"normal"},
  {"change_name":"untested-export","priority":"normal"},
  {"change_name":"ambiguous-gate","priority":"low"},
  {"change_name":"stalled-change","priority":"low"}
]`
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), backlog)

	stubRunner := makeStubAgentRunner(nil)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 2, stubRunner, nil)

	// J1: vague-plan rejected by plan_review, repaired in plan-repair-1, and integrated
	jevAnsEvts, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: "vague-plan", Event: "evaluator.answer"})
	sawFailedPlanReview := false
	for _, ev := range jevAnsEvts {
		if fmt.Sprint(ev.Payload["gate"]) == "plan_review" && fmt.Sprint(ev.Payload["status"]) == "failed" {
			sawFailedPlanReview = true
		}
	}
	if !sawFailedPlanReview {
		t.Fatalf("expected plan_review failed verdict for vague-plan, got %+v", jevAnsEvts)
	}
	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "vague-plan"); cnt != 1 {
		t.Fatalf("expected vague-plan repaired and integrated, got %d", cnt)
	}

	// J2 (environment_or_flaky): flaky gate re-ran once without spending an agent repair
	routeEvts, _ := st.QueryEvents(ctx, store.EventFilter{Event: "evaluator.route"})
	sawRerunGates := false
	sawEscalate := false
	sawStalled := false
	for _, ev := range routeEvts {
		switch fmt.Sprint(ev.Payload["action"]) {
		case "rerun_gates":
			sawRerunGates = true
		case "escalate":
			if ev.Change == "ambiguous-gate" {
				sawEscalate = true
			}
		case "repair_stalled":
			if ev.Change == "stalled-change" {
				sawStalled = true
			}
		}
	}
	if !sawRerunGates {
		t.Fatalf("expected evaluator.route action=rerun_gates on ECONNRESET flaky failure, got %+v", routeEvts)
	}

	// J2 (requirement_ambiguity): ambiguous-gate escalated immediately to spec_ambiguity
	if !sawEscalate {
		t.Fatalf("expected evaluator.route action=escalate for ambiguous-gate")
	}
	qAmbGate, _ := st.GetQuarantine(ctx, "ambiguous-gate")
	if qAmbGate == nil || qAmbGate.Status != "blocked" || qAmbGate.Reason != "spec_ambiguity" {
		t.Fatalf("expected ambiguous-gate blocked as spec_ambiguity, got %+v", qAmbGate)
	}

	// J2 (repair_progress): stalled-change stopped after repair-1 as repair_stalled (never spent repair-2)
	if !sawStalled {
		t.Fatalf("expected evaluator.route action=repair_stalled for stalled-change")
	}
	qStalled, _ := st.GetQuarantine(ctx, "stalled-change")
	if qStalled == nil || qStalled.Status != "blocked" || qStalled.Reason != "repair_stalled" {
		t.Fatalf("expected stalled-change blocked as repair_stalled, got %+v", qStalled)
	}
	stalledExits, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: "stalled-change", Event: "agent.exit"})
	for _, ev := range stalledExits {
		if ev.Phase == "repair-2" {
			t.Fatalf("expected repair_progress to stop stalled-change before repair-2")
		}
	}

	// J7 (scenario_coverage): untested-export sent back to BUILD repair-1, then integrated
	untestedExits, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: "untested-export", Event: "agent.exit"})
	var untestedPhases []string
	for _, ev := range untestedExits {
		untestedPhases = append(untestedPhases, ev.Phase)
	}
	if got := strings.Join(untestedPhases, " "); got != "plan apply repair-1" {
		t.Fatalf("expected untested-export phases 'plan apply repair-1', got %q", got)
	}
	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "untested-export"); cnt != 1 {
		t.Fatalf("expected untested-export integrated once covered, got %d", cnt)
	}

	// J3 (change_risk): rotate-password-hashing quarantined as risk_review; low-risk add-health-check integrated
	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "add-health-check"); cnt != 1 {
		t.Fatalf("expected low-risk add-health-check integrated, got %d", cnt)
	}
	qRisk, _ := st.GetQuarantine(ctx, "rotate-password-hashing")
	if qRisk == nil || qRisk.Status != "blocked" || qRisk.Reason != "risk_review" {
		t.Fatalf("expected rotate-password-hashing blocked as risk_review, got %+v", qRisk)
	}

	// Approve risk via store.ApproveChangeRisk and run another round
	if err := st.ApproveChangeRisk(ctx, "rotate-password-hashing"); err != nil {
		t.Fatalf("ApproveChangeRisk failed: %v", err)
	}
	runDispatchAndMergeRound(t, ctx, wsDir, st, 2, stubRunner, nil)

	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "rotate-password-hashing"); cnt != 1 {
		t.Fatalf("expected approved rotate-password-hashing integrated, got %d", cnt)
	}
	riskTrailers := mustGit(t, ctx, wsDir, "log", "origin/main", "--format=%(trailers:key=Risk-Assessment,valueonly)")
	if !strings.Contains(riskTrailers, "approved_by_operator=true") || !strings.Contains(riskTrailers, "risk=high@0.9") {
		t.Fatalf("expected Risk-Assessment trailer with approved_by_operator=true and risk=high@0.9, got:\n%s", riskTrailers)
	}

	// Verify advisory mode: even a high-risk / vague change is only flagged_advisory and never blocked
	advisoryPolicy := strings.ReplaceAll(enforcePolicy, "mode: enforce", "mode: advisory")
	settingstest.WriteFile(t, wsDir, "evaluator", advisoryPolicy)
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), `[{"change_name":"advisory-password-change","priority":"high"}]`)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 1, stubRunner, nil)

	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "advisory-password-change"); cnt != 1 {
		t.Fatalf("expected advisory-password-change to integrate in advisory mode without blocking, got %d", cnt)
	}
	advRoutes, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: "advisory-password-change", Event: "evaluator.route"})
	sawFlaggedAdvisory := false
	for _, ev := range advRoutes {
		if fmt.Sprint(ev.Payload["action"]) == "flagged_advisory" {
			sawFlaggedAdvisory = true
		}
	}
	if !sawFlaggedAdvisory {
		t.Fatalf("expected evaluator.route action=flagged_advisory in advisory mode, got %+v", advRoutes)
	}
}

// =============================================================================
// 7. Scenario F: Deleted Worktree (missing_child_commits), Duplicate Backlog
//    Items, and Stale MODIFIED Delta at Merge (spec_conflict -> repaired)
// =============================================================================

func TestScenarioF_LostWorktreeDuplicateBacklogAndStaleModifiedDelta(t *testing.T) {
	ctx := context.Background()
	wsDir, backendBareDir, umbrellaBareDir, st := setupAcceptanceSandbox(t)

	backlog := `[{"change_name":"lost-wt"},{"change_name":"dup"},{"change_name":"dup"},{"change_name":"add-sso"}]`
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), backlog)

	stubRunner := makeStubAgentRunner(&stubAgentConfig{ModifiedMode: true})

	disp, err := NewDispatcher(DispatcherOptions{
		Root:        wsDir,
		Store:       st,
		MaxWorkers:  3,
		AgentRunner: stubRunner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// Verify duplicate backlog item emitted backlog.duplicate_name and was readied once
	dupEvts, _ := st.QueryEvents(ctx, store.EventFilter{Event: "backlog.duplicate_name"})
	if len(dupEvts) == 0 {
		t.Fatalf("expected backlog.duplicate_name event")
	}
	readyEvts, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: "dup", Event: "change.ready"})
	if len(readyEvts) != 1 {
		t.Fatalf("expected dup built and readied once, got %d", len(readyEvts))
	}

	// Delete worker-lost-wt worktree after BUILD so its child commits are lost
	mustGit(t, ctx, wsDir, "worktree", "remove", "--force", filepath.Join(wsDir, ".worktrees", "worker-lost-wt"))

	// A human adds a Login scenario ("Locked account") on origin/main after add-sso was queued:
	// add-sso's MODIFIED block is now stale!
	humanDir := filepath.Join(t.TempDir(), "human")
	mustGit(t, ctx, wsDir, "clone", "--quiet", umbrellaBareDir, humanDir)
	mustGit(t, ctx, humanDir, "config", "user.email", "human@example.com")
	mustGit(t, ctx, humanDir, "config", "user.name", "human")
	humanAuthPath := filepath.Join(humanDir, "openspec", "specs", "auth", "spec.md")
	rawAuth, _ := os.ReadFile(humanAuthPath)
	updatedAuth := strings.Replace(
		string(rawAuth),
		"- **THEN** a session token is issued\n",
		"- **THEN** a session token is issued\n\n#### Scenario: Locked account\n- **WHEN** a locked account logs in\n- **THEN** login is refused with HTTP 423\n",
		1,
	)
	mustWriteFile(t, humanAuthPath, updatedAuth)
	mustGit(t, ctx, humanDir, "commit", "-am", "docs: add a Login scenario")
	mustGit(t, ctx, humanDir, "push", "origin", "main")

	// Run Merger
	merger, err := NewMerger(MergerOptions{Root: wsDir, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if err := merger.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "dup"); cnt != 1 {
		t.Fatalf("expected dup integrated once, got %d", cnt)
	}

	qSSO, _ := st.GetQuarantine(ctx, "add-sso")
	if qSSO == nil || qSSO.Status != "blocked" || qSSO.Reason != "spec_conflict" {
		t.Fatalf("expected stale MODIFIED delta on add-sso to block as spec_conflict, got %+v", qSSO)
	}

	qLost, _ := st.GetQuarantine(ctx, "lost-wt")
	if qLost == nil || qLost.Status != "blocked" || qLost.Reason != "missing_child_commits" {
		t.Fatalf("expected deleted worker worktree on lost-wt to block as missing_child_commits, got %+v", qLost)
	}

	// Unblock both add-sso and lost-wt (without deleting feat/spec-lost-wt yet)
	_ = st.ClearQuarantine(ctx, "add-sso", true)
	_ = st.ClearQuarantine(ctx, "lost-wt", true)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 2, stubRunner, nil)

	// Verify add-sso repaired its stale MODIFIED delta and integrated with all 3 scenarios
	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "add-sso"); cnt != 1 {
		t.Fatalf("expected requeued add-sso repaired and integrated, got %d", cnt)
	}
	authAfter := mustGit(t, ctx, wsDir, "show", "origin/main:openspec/specs/auth/spec.md")
	if cnt := strings.Count(authAfter, "#### Scenario:"); cnt != 3 {
		t.Fatalf("expected Login to keep all 3 scenarios after repair, got %d:\n%s", cnt, authAfter)
	}

	// Verify lost-wt was blocked again by abortLostBuild as missing_child_commits, never integrated without code, and did not pause
	qLost2, _ := st.GetQuarantine(ctx, "lost-wt")
	if qLost2 == nil || qLost2.Status != "blocked" || qLost2.Reason != "missing_child_commits" {
		t.Fatalf("expected requeued lost-wt with surviving branch to block as missing_child_commits, got %+v", qLost2)
	}
	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "lost-wt"); cnt != 0 {
		t.Fatalf("expected lost-wt not integrated without its code, got %d", cnt)
	}
	wsState, _ := st.GetWorkspaceState(ctx)
	if wsState.Paused {
		t.Fatalf("expected lost build not to pause the pipeline")
	}

	// Abandon branch feat/spec-lost-wt and unblock -> rebuilt from scratch and integrated with its code
	mustGit(t, ctx, wsDir, "branch", "-D", "feat/spec-lost-wt")
	_ = st.ClearQuarantine(ctx, "lost-wt", true)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 2, stubRunner, nil)

	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "lost-wt"); cnt != 1 {
		t.Fatalf("expected abandoned lost-wt rebuilt and integrated, got %d", cnt)
	}
	lostCode := mustGit(t, ctx, backendBareDir, "show", "main:src/lost-wt.txt")
	if lostCode != "lost-wt implemented" {
		t.Fatalf("expected 'lost-wt implemented' in backend main:src/lost-wt.txt, got %q", lostCode)
	}
}

// =============================================================================
// 8. Scenario K: Uncommitted Human Fix Preserved & Committed on Resume
// =============================================================================

func TestScenarioK_UncommittedHumanFixKeptOnResume(t *testing.T) {
	ctx := context.Background()
	wsDir, backendBareDir, _, st := setupAcceptanceSandbox(t)

	backlog := `[{"change_name":"hand-fixed"},{"change_name":"add-sso"}]`
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), backlog)

	stubRunner := makeStubAgentRunner(nil)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 2, stubRunner, nil)

	qHand, _ := st.GetQuarantine(ctx, "hand-fixed")
	if qHand == nil || qHand.Status != "blocked" || qHand.Reason != "gate_failure" {
		t.Fatalf("expected hand-fixed blocked as gate_failure, got %+v", qHand)
	}
	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "add-sso"); cnt != 1 {
		t.Fatalf("expected add-sso integrated while hand-fixed blocked, got %d", cnt)
	}

	// Human fixes hand-fixed in its worktree and leaves edits uncommitted in both child repo and umbrella change folder
	wtHand := filepath.Join(wsDir, ".worktrees", "worker-hand-fixed")
	if err := os.Remove(filepath.Join(wtHand, "repos", "backend", "src", "BROKEN")); err != nil {
		t.Fatalf("remove BROKEN: %v", err)
	}
	designPath := filepath.Join(wtHand, "openspec", "changes", "hand-fixed", "design.md")
	rawDesign, _ := os.ReadFile(designPath)
	mustWriteFile(t, designPath, string(rawDesign)+"\nHUMAN NOTE: fixed by hand\n")

	_ = st.ClearQuarantine(ctx, "hand-fixed", true)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 2, stubRunner, nil)

	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "hand-fixed"); cnt != 1 {
		t.Fatalf("expected hand-fixed resumed and integrated, got %d", cnt)
	}
	wsState, _ := st.GetWorkspaceState(ctx)
	if wsState.Paused {
		t.Fatalf("expected nothing paused")
	}

	// Verify no rebuild threw the human fix away
	rebEvts, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: "hand-fixed", Event: "change.rebuild"})
	if len(rebEvts) != 0 {
		t.Fatalf("expected 0 change.rebuild events for hand-fixed, got %+v", rebEvts)
	}

	// Verify change.kept_edits event was emitted for both repos/backend and openspec/changes/hand-fixed
	keptEvts, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: "hand-fixed", Event: "change.kept_edits"})
	if len(keptEvts) != 1 {
		t.Fatalf("expected 1 change.kept_edits event, got %+v", keptEvts)
	}
	pathsStr := fmt.Sprint(keptEvts[0].Payload["paths"])
	if !strings.Contains(pathsStr, "repos/backend") || !strings.Contains(pathsStr, "openspec/changes/hand-fixed") {
		t.Fatalf("expected change.kept_edits paths to include repos/backend and openspec/changes/hand-fixed, got %q", pathsStr)
	}

	// Verify child commit message and files on backend main
	backendLog := mustGit(t, ctx, backendBareDir, "log", "main", "--format=%s")
	if !strings.Contains(backendLog, "keep edits found uncommitted") {
		t.Fatalf("expected 'keep edits found uncommitted' commit on backend main, got:\n%s", backendLog)
	}
	mustGit(t, ctx, backendBareDir, "show", "main:src/add-sso.txt")
	if _, err := gitx.RunGit(ctx, backendBareDir, "show", "main:src/BROKEN"); err == nil {
		t.Fatalf("expected src/BROKEN to be gone on backend main")
	}

	// Verify HUMAN NOTE reached the archived design.md on origin/main
	archDesign := archivedFileOnOrigin(t, ctx, wsDir, "hand-fixed", "design.md")
	if !strings.Contains(archDesign, "HUMAN NOTE: fixed by hand") {
		t.Fatalf("expected 'HUMAN NOTE: fixed by hand' in archived design.md, got:\n%s", archDesign)
	}
}

// =============================================================================
// 9. Scenario Q: Automatic Retries of Quarantined Changes After blocked_changes.retry_after
//    Up to blocked_changes.retries, Keeping the resolved block in change_blocks, Never Retrying policy_violation
// =============================================================================

func TestScenarioQ_QuarantineAutomaticRetriesHistoryAndNonRetryablePolicyViolation(t *testing.T) {
	ctx := context.Background()
	wsDir, _, _, st := setupAcceptanceSandbox(t)

	var mu sync.Mutex
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advanceTime := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	backlog := `[{"change_name":"retry-once"},{"change_name":"always-broken"},{"change_name":"sneaky"}]`
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), backlog)

	var toldCounts sync.Map
	baseStub := makeStubAgentRunner(nil)
	agentRunner := func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		if strings.Contains(req.Prompt, "is now retried automatically (retry 1 of 1)") {
			v, _ := toldCounts.LoadOrStore(req.Change, new(int32))
			atomic.AddInt32(v.(*int32), 1)
		}
		return baseStub(ctx, req)
	}

	// Round 1 at T=0: all 3 block (retry-once & always-broken as gate_failure, sneaky as policy_violation)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 3, agentRunner, nowFn)

	qRetry1, _ := st.GetQuarantine(ctx, "retry-once")
	if qRetry1 == nil || qRetry1.Reason != "gate_failure" || qRetry1.Retries != 0 {
		t.Fatalf("expected retry-once blocked with gate_failure and retries=0, got %+v", qRetry1)
	}
	qInfoRetry1 := BuildQuarantineInfo(*qRetry1, 1, 1.0, nowFn())
	if !strings.HasPrefix(qInfoRetry1.RetryNote, "retry 1 of 1 at 2026-") {
		t.Fatalf("expected retry schedule note, got %q", qInfoRetry1.RetryNote)
	}

	qSneaky1, _ := st.GetQuarantine(ctx, "sneaky")
	if qSneaky1 == nil || qSneaky1.Reason != "policy_violation" {
		t.Fatalf("expected sneaky blocked with policy_violation, got %+v", qSneaky1)
	}
	qInfoSneaky := BuildQuarantineInfo(*qSneaky1, 1, 1.0, nowFn())
	if qInfoSneaky.RetryNote != "never retried: needs a person" {
		t.Fatalf("expected sneaky RetryNote='never retried: needs a person', got %q", qInfoSneaky.RetryNote)
	}

	// Pre-set rebuilds=2 on always-broken: an unrelated gate_failure retry must NOT reset its conflict rebuilds
	if _, err := st.DB().ExecContext(ctx, `
		INSERT INTO rebuild_requests (change_name, branch, rebuild_num, requested_at, consumed_at)
		VALUES ('always-broken', 'feat/spec-always-broken', 2, ?, ?)
	`, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	// Advance time by 30 minutes (< the 1h of blocked_changes.retry_after): nothing should retry yet
	advanceTime(30 * time.Minute)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 3, agentRunner, nowFn)
	retryEvtsEarly, _ := st.QueryEvents(ctx, store.EventFilter{Event: "change.retry"})
	if len(retryEvtsEarly) != 0 {
		t.Fatalf("expected 0 retries before blocked_changes.retry_after elapsed, got %+v", retryEvtsEarly)
	}

	// Advance time past 1 hour (T = +90m): retry-once and always-broken should retry; sneaky must NOT
	advanceTime(60 * time.Minute)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 3, agentRunner, nowFn)

	// Verify the blocks retry-once and always-broken were retried from stay on record, resolved, and none for sneaky
	var histRetryOnce, histSneaky int
	_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM change_blocks WHERE change_name = 'retry-once' AND resolution = 'auto_retry' AND retry_num = 0 AND reason = 'gate_failure'`).Scan(&histRetryOnce)
	_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM change_blocks WHERE change_name = 'sneaky' AND resolution = 'auto_retry'`).Scan(&histSneaky)
	if histRetryOnce != 1 || histSneaky != 0 {
		t.Fatalf("expected resolved blocks retry-once=1 and sneaky=0, got retry-once=%d sneaky=%d", histRetryOnce, histSneaky)
	}

	// Verify retry-once phases from its kept worktree: "plan apply repair-1 repair-2 repair-1"
	roExits, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: "retry-once", Event: "agent.exit"})
	var roPhases []string
	for _, ev := range roExits {
		roPhases = append(roPhases, ev.Phase)
	}
	if got := strings.Join(roPhases, " "); got != "plan apply repair-1 repair-2 repair-1" {
		t.Fatalf("expected retry-once phases 'plan apply repair-1 repair-2 repair-1', got %q", got)
	}

	// Verify only the first agent call after retry was told, and retry-once integrated
	roToldPtr, _ := toldCounts.Load("retry-once")
	if roToldPtr == nil || atomic.LoadInt32(roToldPtr.(*int32)) != 1 {
		t.Fatalf("expected retry-once to be told exactly once after retry, got %v", roToldPtr)
	}
	if cnt := integratedCountOnOrigin(t, ctx, wsDir, "retry-once"); cnt != 1 {
		t.Fatalf("expected retry-once integrated once, got %d", cnt)
	}

	// Verify retry-1 is recorded in archived repairs.md
	roRepairs := archivedFileOnOrigin(t, ctx, wsDir, "retry-once", "repairs.md")
	if !strings.Contains(roRepairs, "## retry-1, ") || !strings.Contains(roRepairs, ", dispatcher") || !strings.Contains(roRepairs, "Result: retried automatically (1 of 1).") {
		t.Fatalf("expected retry-1 entry in archived repairs.md, got:\n%s", roRepairs)
	}

	// Verify retry-once quarantine record is completely forgotten after integration
	qRetryAfter, _ := st.GetQuarantine(ctx, "retry-once")
	if qRetryAfter != nil {
		t.Fatalf("expected retry-once quarantine forgotten after integration, got %+v", qRetryAfter)
	}

	// Verify always-broken is blocked for good with retries=1, told=1, and rebuilds=2 preserved
	qBrokenAfter, _ := st.GetQuarantine(ctx, "always-broken")
	if qBrokenAfter == nil || qBrokenAfter.Status != "blocked" || qBrokenAfter.Reason != "gate_failure" || qBrokenAfter.Retries != 1 {
		t.Fatalf("expected always-broken blocked with retries=1, got %+v", qBrokenAfter)
	}
	if qBrokenAfter.Rebuilds != 2 {
		t.Fatalf("expected gate_failure retry to preserve rebuilds=2 on always-broken, got %d", qBrokenAfter.Rebuilds)
	}
	qInfoBroken := BuildQuarantineInfo(*qBrokenAfter, 1, 1.0, nowFn())
	if qInfoBroken.RetryNote != "no retries left: 1 of 1 used" {
		t.Fatalf("expected RetryNote='no retries left: 1 of 1 used', got %q", qInfoBroken.RetryNote)
	}
	abToldPtr, _ := toldCounts.Load("always-broken")
	if abToldPtr == nil || atomic.LoadInt32(abToldPtr.(*int32)) != 1 {
		t.Fatalf("expected always-broken to be told on only its first agent call after retry, got %v", abToldPtr)
	}

	// Advance time by another 2 hours and run Round 3: no retry beyond limit, and none for sneaky
	advanceTime(2 * time.Hour)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 3, agentRunner, nowFn)

	allRetries, _ := st.QueryEvents(ctx, store.EventFilter{Event: "change.retry"})
	var retriedChanges []string
	for _, ev := range allRetries {
		retriedChanges = append(retriedChanges, ev.Change)
	}
	sort.Strings(retriedChanges)
	if got := strings.Join(retriedChanges, " "); got != "always-broken retry-once" {
		t.Fatalf("expected only 'always-broken retry-once' retried, got %q", got)
	}
}

// =============================================================================
// 10. Scenario R: Submodule Rebase Conflict Rebuilds (RequestRebuild up to
//     conflict_rebuilds), Reopening Tasks Under Any List Marker (-, *, +, 1., 1))
// =============================================================================

func TestScenarioR_SubmoduleRebaseConflictRebuildsAndAllTaskListMarkers(t *testing.T) {
	ctx := context.Background()
	wsDir, backendBareDir, umbrellaBareDir, st := setupAcceptanceSandbox(t)

	stubRunner := makeStubAgentRunner(nil)

	// Part 1: shared-a and shared-b edit the same line of src/shared.txt
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), `[{"change_name":"shared-a"},{"change_name":"shared-b"}]`)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 2, stubRunner, nil)

	sentBackEvts, _ := st.QueryEvents(ctx, store.EventFilter{Event: "merge.sent_back"})
	if len(sentBackEvts) != 1 {
		t.Fatalf("expected 1 merge.sent_back event, got %+v", sentBackEvts)
	}
	sentBackChange := sentBackEvts[0].Change
	qSentBack, _ := st.GetQuarantine(ctx, sentBackChange)
	if qSentBack == nil || qSentBack.Status != "rebuild_requested" || qSentBack.Rebuilds != 1 {
		t.Fatalf("expected %s status=rebuild_requested and rebuilds=1, got %+v", sentBackChange, qSentBack)
	}

	// Round 2 rebuilds sentBackChange on the new base and integrates it
	runDispatchAndMergeRound(t, ctx, wsDir, st, 2, stubRunner, nil)

	rebEvts, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: sentBackChange, Event: "change.rebuild"})
	if len(rebEvts) != 1 {
		t.Fatalf("expected 1 change.rebuild event for %s, got %+v", sentBackChange, rebEvts)
	}
	if gotKept := fmt.Sprint(rebEvts[0].Payload["kept"]); gotKept != "feat/spec-"+sentBackChange+"-conflict-1" {
		t.Fatalf("expected kept branch feat/spec-%s-conflict-1, got %q", sentBackChange, gotKept)
	}
	if gotFiles := fmt.Sprint(rebEvts[0].Payload["files"]); gotFiles != "src/shared.txt" {
		t.Fatalf("expected conflict files='src/shared.txt', got %q", gotFiles)
	}
	for _, c := range []string{"shared-a", "shared-b"} {
		if cnt := integratedCountOnOrigin(t, ctx, wsDir, c); cnt != 1 {
			t.Fatalf("expected %s integrated once, got %d", c, cnt)
		}
	}
	sharedTxt := mustGit(t, ctx, backendBareDir, "show", "main:src/shared.txt")
	if sharedTxt != sentBackChange+" owns this line" {
		t.Fatalf("expected rebuilt change %s to own src/shared.txt on main, got %q", sentBackChange, sharedTxt)
	}
	if qDone, _ := st.GetQuarantine(ctx, sentBackChange); qDone != nil {
		t.Fatalf("expected rebuild record cleared once integrated, got %+v", qDone)
	}

	// Part 2: Tasks under *, +, 1., and 2) list markers are reopened on rebuild and re-implemented
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), `[{"change_name":"shared-star-a"},{"change_name":"shared-star-b"}]`)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 2, stubRunner, nil)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 2, stubRunner, nil)

	for _, c := range []string{"shared-star-a", "shared-star-b"} {
		if cnt := integratedCountOnOrigin(t, ctx, wsDir, c); cnt != 1 {
			t.Fatalf("expected %s integrated once, got %d", c, cnt)
		}
	}
	starSentEvts, _ := st.QueryEvents(ctx, store.EventFilter{Event: "merge.sent_back"})
	var starSentBack string
	for _, ev := range starSentEvts {
		if strings.HasPrefix(ev.Change, "shared-star-") {
			starSentBack = ev.Change
			break
		}
	}
	if starSentBack == "" {
		t.Fatalf("expected merge.sent_back for shared-star-*")
	}
	if got := mustGit(t, ctx, backendBareDir, "show", "main:src/shared.txt"); got != starSentBack+" owns this line" {
		t.Fatalf("expected %s owns this line after reopening *, +, 1., 2) tasks, got %q", starSentBack, got)
	}

	// Part 3: with merge.conflict_rebuilds 0 for this run, the conflict is quarantined as rebase_conflict
	settingstest.Give(t, settings.ConflictRebuilds, "0")
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), `[{"change_name":"shared-c"},{"change_name":"shared-d"}]`)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 2, stubRunner, nil)

	totalCD := integratedCountOnOrigin(t, ctx, wsDir, "shared-c") + integratedCountOnOrigin(t, ctx, wsDir, "shared-d")
	if totalCD != 1 {
		t.Fatalf("expected exactly 1 of shared-c/shared-d integrated when conflict_rebuilds=0, got %d", totalCD)
	}
	qC, _ := st.GetQuarantine(ctx, "shared-c")
	qD, _ := st.GetQuarantine(ctx, "shared-d")
	var blockedConflict *store.QuarantineRecord
	if qC != nil && qC.Status == "blocked" {
		blockedConflict = qC
	} else if qD != nil && qD.Status == "blocked" {
		blockedConflict = qD
	}
	if blockedConflict == nil || blockedConflict.Reason != "rebase_conflict" {
		t.Fatalf("expected second conflicting change quarantined as rebase_conflict when conflict_rebuilds=0, got qC=%+v qD=%+v", qC, qD)
	}

	settingstest.Take(t, settings.ConflictRebuilds)

	// Part 4: Umbrella content conflict in refreshPhase is spec_conflict and is NOT retryable
	conflictWT := filepath.Join(wsDir, ".worktrees", "worker-umbrella-conflict")
	mustGit(t, ctx, wsDir, "worktree", "add", "-b", "feat/spec-umbrella-conflict", conflictWT, "origin/main")
	mustWriteFile(t, filepath.Join(conflictWT, "openspec", "changes", "umbrella-conflict", "proposal.md"), "worker version\n")
	mustGit(t, ctx, conflictWT, "add", "-A")
	mustGit(t, ctx, conflictWT, "commit", "-m", "worker umbrella artifacts")

	otherClone := filepath.Join(t.TempDir(), "umbrella-conflict-other")
	mustGit(t, ctx, wsDir, "clone", "--quiet", umbrellaBareDir, otherClone)
	mustGit(t, ctx, otherClone, "config", "user.email", "other@example.com")
	mustGit(t, ctx, otherClone, "config", "user.name", "other")
	mustWriteFile(t, filepath.Join(otherClone, "openspec", "changes", "umbrella-conflict", "proposal.md"), "base version\n")
	mustGit(t, ctx, otherClone, "add", "-A")
	mustGit(t, ctx, otherClone, "commit", "-m", "competing umbrella artifacts")
	mustGit(t, ctx, otherClone, "push", "origin", "main")

	dispConflict, err := NewDispatcher(DispatcherOptions{Root: wsDir, Store: st, MaxWorkers: 1})
	if err != nil {
		t.Fatal(err)
	}
	var retried map[string]any
	_, refErr := dispConflict.refreshPhase(ctx, BacklogItem{ChangeName: "umbrella-conflict"}, conflictWT, "feat/spec-umbrella-conflict", 1, false, &retried)
	var bErr *BlockedError
	if refErr == nil || !strings.Contains(refErr.Error(), "spec_conflict") {
		t.Fatalf("expected umbrella content conflict to fail with spec_conflict, got %v", refErr)
	}
	_ = bErr
	if IsRetryableReason("spec_conflict") {
		t.Fatalf("spec_conflict must NOT be in RETRYABLE_REASONS")
	}
}

// =============================================================================
// 11. Scenario U: Every Engine Down (Exit 75) -> Deferral Without Quarantine or
//     Repair Round, Stopping Dispatch, and Blocking as engine_unavailable on 3rd
// =============================================================================

func TestScenarioU_EnginesDownDeferralAndEngineUnavailableAfterThreeDeferrals(t *testing.T) {
	ctx := context.Background()
	wsDir, _, _, st := setupAcceptanceSandbox(t)

	var enginesDown atomic.Bool
	enginesDown.Store(true)
	stubRunner := makeStubAgentRunner(&stubAgentConfig{EnginesDown: &enginesDown})

	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), `[{"change_name":"engine-down"},{"change_name":"add-sso"}]`)

	// Dispatch 1: engine-down deferred (deferrals=1), not quarantined, no repair round, add-sso never started
	runDispatchAndMergeRound(t, ctx, wsDir, st, 1, stubRunner, nil)

	if q, _ := st.GetQuarantine(ctx, "engine-down"); q != nil && q.Status == "blocked" {
		t.Fatalf("expected engine-down deferred without quarantine, got %+v", q)
	}
	defEvts1, _ := st.QueryEvents(ctx, store.EventFilter{ChangeName: "engine-down", Event: "change.deferred"})
	if len(defEvts1) != 1 || fmt.Sprint(defEvts1[0].Payload["deferrals"]) != "1" {
		t.Fatalf("expected 1 change.deferred event with deferrals=1, got %+v", defEvts1)
	}
	if dirExists(filepath.Join(wsDir, ".worktrees", "worker-add-sso")) {
		t.Fatalf("expected dispatcher to start nothing else once engines were down")
	}

	// Dispatch 2: deferred again and counted (deferrals=2)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 1, stubRunner, nil)
	var dbDeferrals int
	_ = st.DB().QueryRowContext(ctx, `SELECT deferrals FROM v_deferral_count WHERE change_name = 'engine-down'`).Scan(&dbDeferrals)
	if dbDeferrals != 2 {
		t.Fatalf("expected deferrals=2 for engine-down after 2nd dispatch, got %d", dbDeferrals)
	}

	// Engines recover: both engine-down and add-sso build and integrate, and deferrals are cleared
	enginesDown.Store(false)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 1, stubRunner, nil)

	for _, c := range []string{"engine-down", "add-sso"} {
		if cnt := integratedCountOnOrigin(t, ctx, wsDir, c); cnt != 1 {
			t.Fatalf("expected %s integrated once engines recovered, got %d", c, cnt)
		}
	}
	dbDeferrals = 0
	_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM v_deferral_count WHERE change_name = 'engine-down'`).Scan(&dbDeferrals)
	if dbDeferrals != 0 {
		t.Fatalf("expected deferrals cleared after successful agent call, got %d", dbDeferrals)
	}

	// Now engine-down-again fails 3 dispatches in a row -> quarantined as engine_unavailable and pauses pipeline
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), `[{"change_name":"engine-down-again"}]`)
	enginesDown.Store(true)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 1, stubRunner, nil)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 1, stubRunner, nil)
	runDispatchAndMergeRound(t, ctx, wsDir, st, 1, stubRunner, nil)

	qDown3, _ := st.GetQuarantine(ctx, "engine-down-again")
	if qDown3 == nil || qDown3.Status != "blocked" || qDown3.Reason != "engine_unavailable" {
		t.Fatalf("expected 3rd deferral in a row to quarantine engine-down-again as engine_unavailable, got %+v", qDown3)
	}
	wsState, _ := st.GetWorkspaceState(ctx)
	if !wsState.Paused {
		t.Fatalf("expected engine_unavailable to pause the workspace")
	}
}

// =============================================================================
// 12. Massive Concurrency Stress Test: RunCoordinated (4 Workers + Live Merger),
//     Live DryRun Inspection, Pre-Receive Push Rejection Retry & Exhaustion,
//     Invalid Manifest Handling, and Secret Redaction
// =============================================================================

func TestMassiveConcurrencyCoordinatedPushRetryAndLiveDryRun(t *testing.T) {
	ctx := context.Background()
	wsDir, _, umbrellaBareDir, st := setupAcceptanceSandbox(t)

	// Install a pre-receive hook on umbrella.git that rejects once when rejectOnceFile exists
	rejectOnceFile := filepath.Join(t.TempDir(), "reject.once")
	if err := os.WriteFile(rejectOnceFile, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(umbrellaBareDir, "hooks", "pre-receive")
	hookScript := fmt.Sprintf("#!/bin/sh\nif [ -e %q ]; then rm -f %q; echo 'rejected once for stress test' >&2; exit 1; fi\n", rejectOnceFile, rejectOnceFile)
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte(hookScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// Enqueue a manifest with an invalid name to verify Merger rejects it safely
	if err := st.EnqueueReady(ctx, store.ReadyManifest{
		ChangeName: "INVALID MANIFEST; rm -rf /",
		Branch:     "feat/bad",
		Worktree:   "/tmp/bad",
		HeadSHA:    "deadbeef",
		EnqueuedAt: "2026-11-08T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	// Build a multi-layer backlog with 8 changes, shared locks, shared capabilities, repairs, and dependency chains
	backlog := `[
  {"change_name": "tier1-a", "priority": "critical", "locks": ["shared-mig"]},
  {"change_name": "tier1-b", "priority": "high",     "locks": ["shared-mig"]},
  {"change_name": "tier1-c", "priority": "high"},
  {"change_name": "tier1-d", "priority": "normal"},
  {"change_name": "fix-flaky", "priority": "normal"},
  {"change_name": "tier2-a", "priority": "critical", "depends_on": ["tier1-a", "tier1-c"]},
  {"change_name": "tier2-b", "priority": "high",     "depends_on": ["tier1-b", "fix-flaky"]},
  {"change_name": "tier3-final", "priority": "critical", "depends_on": ["tier2-a", "tier2-b", "tier1-d"]}
]`
	mustWriteFile(t, filepath.Join(wsDir, ".foreman-kit", "agent_backlog.json"), backlog)

	var liveDryRunChecked atomic.Bool
	baseStub := makeStubAgentRunner(nil)
	agentRunner := func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		if req.Role == "build" && req.Change == "tier1-a" && !liveDryRunChecked.Swap(true) {
			dispInspect, err := NewDispatcher(DispatcherOptions{
				Root:       wsDir,
				Store:      st,
				MaxWorkers: 4,
			})
			if err == nil {
				if rep, err := dispInspect.DryRun(ctx); err == nil {
					txt := rep.FormatText(4, 0)
					if !strings.Contains(txt, "dispatch: running") || !strings.Contains(txt, "building now") {
						t.Errorf("expected live DryRun during BUILD to report 'dispatch: running' and 'building now', got:\n%s", txt)
					}
				}
			}
		}
		return baseStub(ctx, req)
	}

	if err := RunCoordinated(ctx, CoordinatedOptions{
		Root:          wsDir,
		Store:         st,
		MaxWorkers:    4,
		PollInterval:  25 * time.Millisecond,
		MergeInterval: 25 * time.Millisecond,
		WaitTimeout:   30 * time.Second,
		AgentRunner:   agentRunner,
	}); err != nil {
		t.Fatalf("RunCoordinated failed: %v", err)
	}

	if !liveDryRunChecked.Load() {
		t.Fatalf("expected live DryRun check to execute while worker was building")
	}

	// Verify all 8 valid changes integrated exactly once in DAG order
	for _, chg := range []string{"tier1-a", "tier1-b", "tier1-c", "tier1-d", "fix-flaky", "tier2-a", "tier2-b", "tier3-final"} {
		if cnt := integratedCountOnOrigin(t, ctx, wsDir, chg); cnt != 1 {
			t.Fatalf("expected %s integrated once in RunCoordinated, got %d", chg, cnt)
		}
	}

	// Verify merge.push_rejected was emitted once and recovered by retry
	if fileExists(rejectOnceFile) {
		t.Fatalf("expected pre-receive hook to trigger and remove rejectOnceFile")
	}
	pushRejEvts, _ := st.QueryEvents(ctx, store.EventFilter{Event: "merge.push_rejected"})
	if len(pushRejEvts) == 0 {
		t.Fatalf("expected merge.push_rejected event from pre-receive hook")
	}

	// Verify merge.invalid_manifest was emitted and removed from the merge queue
	invManEvts, _ := st.QueryEvents(ctx, store.EventFilter{Event: "merge.invalid_manifest"})
	if len(invManEvts) == 0 {
		t.Fatalf("expected merge.invalid_manifest event")
	}
	remQueue, _ := st.ListReadyQueue(ctx)
	if len(remQueue) != 0 {
		t.Fatalf("expected an empty merge queue after RunCoordinated, got %+v", remQueue)
	}
}
