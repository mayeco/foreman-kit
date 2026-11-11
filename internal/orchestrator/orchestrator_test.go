package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

// setupTestWorkspaceWithSubmodule creates a complete temporary umbrella git repository
// with a bare origin remote and a child submodule at repos/backend (also backed by a bare remote).
func setupTestWorkspaceWithSubmodule(t *testing.T) (umbrellaDir string, childBareDir string, umbrellaBareDir string, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(tmp, "state"))
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_APP_ROOT", "")
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "1")
	settingstest.Give(t, settings.EvaluatorEnabled, "false")

	// 1. Create bare remote for child repo (repos/backend)
	childBareDir = filepath.Join(tmp, "remotes", "backend.git")
	if err := os.MkdirAll(childBareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, childBareDir, "init", "--bare", "-b", "main")

	// 2. Create seed checkout for child repo, commit repo.config.yaml,env_script, and initial code, then push to bare remote
	childSeed := filepath.Join(tmp, "seed-backend")
	if err := os.MkdirAll(filepath.Join(childSeed, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, childSeed, "init", "-b", "main")
	mustGit(t, ctx, childSeed, "config", "user.email", "test@example.com")
	mustGit(t, ctx, childSeed, "config", "user.name", "Test")

	repoCfg := `base_branch: main
env_script: scripts/custom_env.sh
gates:
  test: "test -f feature.txt"
`
	if err := os.WriteFile(filepath.Join(childSeed, "repo.config.yaml"), []byte(repoCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(childSeed, "scripts", "custom_env.sh"), []byte("#!/usr/bin/env bash\nexport CHILD_ENV_LOADED=1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(childSeed, "README.md"), []byte("# Backend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, childSeed, "add", "-A")
	mustGit(t, ctx, childSeed, "commit", "-m", "chore: initial backend")
	mustGit(t, ctx, childSeed, "remote", "add", "origin", childBareDir)
	mustGit(t, ctx, childSeed, "push", "-u", "origin", "main")

	// 3. Create bare remote for umbrella repo
	umbrellaBareDir = filepath.Join(tmp, "remotes", "umbrella.git")
	if err := os.MkdirAll(umbrellaBareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, umbrellaBareDir, "init", "--bare", "-b", "master")

	// 4. Create umbrella checkout
	umbrellaDir = filepath.Join(tmp, "umbrella")
	if err := os.MkdirAll(filepath.Join(umbrellaDir, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(umbrellaDir, "openspec", "specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(umbrellaDir, "openspec", "changes", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, umbrellaDir, "init", "-b", "master")
	mustGit(t, ctx, umbrellaDir, "config", "user.email", "test@example.com")
	mustGit(t, ctx, umbrellaDir, "config", "user.name", "Test")

	if err := os.WriteFile(filepath.Join(umbrellaDir, ".foreman-kit/kit.lock"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(umbrellaDir, ".gitignore"), []byte("/.worktrees/\n/log/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(umbrellaDir, "MEMORY.md"), []byte("# Memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(umbrellaDir, "openspec", "config.yaml"), []byte("schema: spec-driven\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pipelineYAML := `repository:
  base_branch: master
dispatch:
  workers: 2
  priority_bump_every: 6h
blocked_changes:
  retries: 1
  retry_after: 1h
gates:
  timeout: 1m
merge:
  conflict_rebuilds: 2
locks:
  stale_after: 5m
`
	settingstest.WriteFiles(t, umbrellaDir, map[string]string{"pipeline": pipelineYAML})
	if err := os.WriteFile(filepath.Join(umbrellaDir, ".foreman-kit", "agent_backlog.json"), []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(umbrellaDir, ".foreman-kit", "agent_history.json"), []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Add submodule repos/backend
	mustGit(t, ctx, umbrellaDir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", childBareDir, "repos/backend")
	mustGit(t, ctx, umbrellaDir, "config", "-f", ".gitmodules", "submodule.repos/backend.branch", "main")
	mustGit(t, ctx, filepath.Join(umbrellaDir, "repos", "backend"), "config", "user.email", "test@example.com")
	mustGit(t, ctx, filepath.Join(umbrellaDir, "repos", "backend"), "config", "user.name", "Test")

	mustGit(t, ctx, umbrellaDir, "add", "-A")
	mustGit(t, ctx, umbrellaDir, "commit", "-m", "chore: initial umbrella")
	mustGit(t, ctx, umbrellaDir, "remote", "add", "origin", umbrellaBareDir)
	mustGit(t, ctx, umbrellaDir, "push", "-u", "origin", "master")

	var err error
	st, err = store.Open(umbrellaDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	return umbrellaDir, childBareDir, umbrellaBareDir, st
}

func mustGit(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.RunGit(ctx, dir, args...)
	if err != nil {
		t.Fatalf("git %s in %s failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(out)
}

func TestDryRunPriorityAgingDependenciesLocksAndQuarantine(t *testing.T) {
	ctx := context.Background()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

	// Configure backlog with items testing:
	// - invalid name
	// - low priority aged 13 hours (with priority_bump_every: 6h -> ages 2 levels: low(3) -> high(1))
	// - normal priority fresh
	// - item waiting on dependency
	// - item sharing lock with an item in the merge queue
	// - item with an open block (change_blocks)
	backlogJSON := `[
  {"change_name": "Invalid_Name!", "priority": "high"},
  {"change_name": "fresh-normal", "priority": "normal", "impact_score": 5, "queued_at": "2026-11-08T11:30:00Z"},
  {"change_name": "aged-low", "priority": "low", "impact_score": 3, "queued_at": "2026-11-07T23:00:00Z"},
  {"change_name": "waits-on-dep", "priority": "critical", "depends_on": ["fresh-normal"], "queued_at": "2026-11-08T11:00:00Z"},
  {"change_name": "shares-lock", "priority": "high", "locks": ["db-schema"], "queued_at": "2026-11-08T11:00:00Z"},
  {"change_name": "quarantined-item", "priority": "high", "queued_at": "2026-11-08T10:00:00Z"},
  {"change_name": "in-ready-queue", "priority": "high", "locks": ["db-schema"], "queued_at": "2026-11-08T09:00:00Z"}
]`
	if err := os.WriteFile(filepath.Join(umbrellaDir, ".foreman-kit", "agent_backlog.json"), []byte(backlogJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// Put "in-ready-queue" into store.EnqueueReady holding lock:db-schema
	if err := st.EnqueueReady(ctx, store.ReadyManifest{
		ChangeName:   "in-ready-queue",
		Branch:       "feat/spec-in-ready-queue",
		Worktree:     filepath.Join(umbrellaDir, ".worktrees", "worker-in-ready-queue"),
		HeadSHA:      "0123456789abcdef0123456789abcdef01234567",
		Capabilities: []string{"lock:db-schema"},
		EnqueuedAt:   now.Add(-10 * time.Minute).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	// Block "quarantined-item" with a retryable reason
	if err := st.BlockChange(ctx, store.QuarantineRecord{
		ChangeName: "quarantined-item",
		Reason:     "gate_failure",
		Details:    "lint failed",
		BlockedAt:  now.Add(-30 * time.Minute).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	disp, err := NewDispatcher(DispatcherOptions{
		Root:       umbrellaDir,
		Store:      st,
		MaxWorkers: 1,
		NowFunc:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	rep, err := disp.DryRun(ctx)
	if err != nil {
		t.Fatalf("DryRun failed: %v", err)
	}

	if rep.FreeWorkers != 1 {
		t.Fatalf("expected FreeWorkers=1, got %d", rep.FreeWorkers)
	}

	byChange := make(map[string]DryRunItem)
	var order []string
	for _, it := range rep.Items {
		byChange[it.Change] = it
		order = append(order, it.Change)
	}

	// 1. Invalid item is reported as state="invalid"
	if inv, ok := byChange["Invalid_Name!"]; !ok || inv.State != "invalid" {
		t.Fatalf("expected Invalid_Name! state=invalid, got %+v", inv)
	}

	// 2. aged-low waited 13h -> counts_as="high" and should be ordered BEFORE fresh-normal ("normal")!
	agedLow := byChange["aged-low"]
	if agedLow.CountsAs != "high" || agedLow.State != "ready" || !agedLow.Next {
		t.Fatalf("expected aged-low counts_as=high, state=ready, next=true; got %+v", agedLow)
	}
	freshNormal := byChange["fresh-normal"]
	if freshNormal.State != "ready" || freshNormal.Next {
		t.Fatalf("expected fresh-normal state=ready, next=false (since max_workers=1); got %+v", freshNormal)
	}

	// 3. waits-on-dep is state="waiting"
	waitsDep := byChange["waits-on-dep"]
	if waitsDep.State != "waiting" || !strings.Contains(waitsDep.Why, "fresh-normal (not built yet)") {
		t.Fatalf("expected waits-on-dep state=waiting on fresh-normal, got %+v", waitsDep)
	}

	// 4. shares-lock is state="held" because in-ready-queue holds lock:db-schema
	sharesLock := byChange["shares-lock"]
	if sharesLock.State != "held" || !strings.Contains(sharesLock.Why, "lock:db-schema") {
		t.Fatalf("expected shares-lock state=held on lock:db-schema, got %+v", sharesLock)
	}

	// 5. in-ready-queue is state="merging"
	mergingItem := byChange["in-ready-queue"]
	if mergingItem.State != "merging" {
		t.Fatalf("expected in-ready-queue state=merging, got %+v", mergingItem)
	}

	// 6. quarantined-item is state="blocked" with retry note
	blockedItem := byChange["quarantined-item"]
	if blockedItem.State != "blocked" || !strings.Contains(blockedItem.Why, "retry 1 of 1") {
		t.Fatalf("expected quarantined-item state=blocked with retry note, got %+v", blockedItem)
	}

	jsonOut, err := rep.FormatJSON()
	if err != nil || !strings.Contains(jsonOut, `"aged-low"`) {
		t.Fatalf("FormatJSON failed: %v", err)
	}
	textOut := rep.FormatText(1, 0)
	if !strings.Contains(textOut, "low>high") {
		t.Fatalf("expected FormatText to include 'low>high', got:\n%s", textOut)
	}
}

func TestPolicyCheckRootAndChildProtectedPaths(t *testing.T) {
	ctx := context.Background()
	umbrellaDir, _, _, _ := setupTestWorkspaceWithSubmodule(t)

	repo, err := gitx.Open(umbrellaDir)
	if err != nil {
		t.Fatal(err)
	}
	forkPoint, err := repo.RevParse("HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Create a branch that touches an unauthorized root path (MEMORY.md)
	wtDir := filepath.Join(umbrellaDir, ".worktrees", "worker-bad-root")
	if err := repo.AddWorktree(ctx, wtDir, "feat/spec-bad-root", forkPoint, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "MEMORY.md"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, wtDir, "add", "MEMORY.md")
	mustGit(t, ctx, wtDir, "commit", "-m", "bad root edit")
	badRootSHA := mustGit(t, ctx, wtDir, "rev-parse", "HEAD")

	wtRepo, err := gitx.Open(wtDir)
	if err != nil {
		t.Fatal(err)
	}
	err = CheckRootPolicy(wtRepo, "bad-root", forkPoint, badRootSHA)
	if err == nil || !strings.Contains(err.Error(), "policy_violation") || !strings.Contains(err.Error(), "MEMORY.md") {
		t.Fatalf("expected CheckRootPolicy to reject MEMORY.md edit, got: %v", err)
	}

	// 2. Test child policy check: touching protected static path (repo.config.yaml, .github, .gitlab-ci.yml)
	// and dynamic env_script (scripts/custom_env.sh read from blob at fork point)
	childDir := filepath.Join(umbrellaDir, "repos", "backend")
	childRepo, err := gitx.Open(childDir)
	if err != nil {
		t.Fatal(err)
	}
	childFork, err := childRepo.RevParse("HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// Verify ProtectedChildPathsFromBlob discovers scripts/custom_env.sh from the git blob at childFork
	protPaths := ProtectedChildPathsFromBlob(childRepo, childFork)
	foundEnvScript := false
	for _, p := range protPaths {
		if p == "scripts/custom_env.sh" {
			foundEnvScript = true
			break
		}
	}
	if !foundEnvScript {
		t.Fatalf("expected ProtectedChildPathsFromBlob to include scripts/custom_env.sh, got %v", protPaths)
	}

	// Allowed change in child repo
	mustGit(t, ctx, childDir, "checkout", "-b", "feat/spec-ok-child", childFork)
	if err := os.WriteFile(filepath.Join(childDir, "feature.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, childDir, "add", "feature.txt")
	mustGit(t, ctx, childDir, "commit", "-m", "feat: add feature.txt")
	okChildSHA := mustGit(t, ctx, childDir, "rev-parse", "HEAD")

	if err := CheckChildPolicy(childRepo, "repos/backend", childFork, okChildSHA); err != nil {
		t.Fatalf("expected CheckChildPolicy to allow feature.txt, got: %v", err)
	}

	// Tamper with scripts/custom_env.sh (the env_script in repo.config.yaml)
	if err := os.WriteFile(filepath.Join(childDir, "scripts", "custom_env.sh"), []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, childDir, "add", "scripts/custom_env.sh")
	mustGit(t, ctx, childDir, "commit", "-m", "tamper env_script")
	badEnvSHA := mustGit(t, ctx, childDir, "rev-parse", "HEAD")

	err = CheckChildPolicy(childRepo, "repos/backend", childFork, badEnvSHA)
	if err == nil || !strings.Contains(err.Error(), "policy_violation") || !strings.Contains(err.Error(), "scripts/custom_env.sh") {
		t.Fatalf("expected CheckChildPolicy to reject env_script modification, got: %v", err)
	}

	// Tamper with .github/workflows/ci.yml
	mustGit(t, ctx, childDir, "checkout", "-B", "feat/spec-bad-ci", okChildSHA)
	if err := os.MkdirAll(filepath.Join(childDir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(childDir, ".github", "workflows", "ci.yml"), []byte("name: ci\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, childDir, "add", ".github/workflows/ci.yml")
	mustGit(t, ctx, childDir, "commit", "-m", "tamper github workflow")
	badCISHA := mustGit(t, ctx, childDir, "rev-parse", "HEAD")

	err = CheckChildPolicy(childRepo, "repos/backend", childFork, badCISHA)
	if err == nil || !strings.Contains(err.Error(), "policy_violation") || !strings.Contains(err.Error(), ".github/workflows/ci.yml") {
		t.Fatalf("expected CheckChildPolicy to reject .github modification, got: %v", err)
	}
	mustGit(t, ctx, childDir, "checkout", "main")
}

func TestFullCycleDispatchReadyQueueAndMergeWithJevAndSubmodule(t *testing.T) {
	ctx := context.Background()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	// Start a mock Jev HTTP server to verify Jev integration across plan_review, scenario_coverage, and change_risk
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"model": "jev-1.14.0",
			"answers": map[string]any{
				"problem_is_concrete":             map[string]any{"type": "noul", "noul": 0.95},
				"scenarios_are_testable":          map[string]any{"type": "noul", "noul": 0.92},
				"tasks_cover_scenarios":           map[string]any{"type": "noul", "noul": 0.94},
				"consistent_with_canonical_specs": map[string]any{"type": "noul", "noul": 0.96},
				"single_concern":                  map[string]any{"type": "noul", "noul": 0.91},
				"every_scenario_tested":           map[string]any{"type": "noul", "noul": 0.93},
				"tests_assert_outcomes":           map[string]any{"type": "noul", "noul": 0.90},
				"blast_radius":                    map[string]any{"type": "choice", "choice": "contained", "confidence": 0.90},
				"touches_sensitive_surface":       map[string]any{"type": "noul", "noul": 0.05},
				"matches_proposal":                map[string]any{"type": "noul", "noul": 0.95},
				"reversible":                      map[string]any{"type": "noul", "noul": 0.95},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	settingstest.Give(t, settings.EvaluatorEnabled, "true")
	t.Setenv("TYPESAFE_API_KEY", "ts-mock-key")
	settingstest.Give(t, settings.EvaluatorBaseURL, srv.URL)

	// Write backlog item
	backlog := `[{"change_name": "add-greeting", "description": "Add greeting feature to backend", "priority": "high", "impact_score": 8}]`
	if err := os.WriteFile(filepath.Join(umbrellaDir, ".foreman-kit", "agent_backlog.json"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}

	var seenSlots []int
	var mu sync.Mutex

	agentRunner := func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		mu.Lock()
		seenSlots = append(seenSlots, req.Slot)
		mu.Unlock()

		// Verify no log/ symlink was created in the worker worktree
		if _, err := os.Lstat(filepath.Join(req.Worktree, "log")); err == nil {
			t.Errorf("unexpected log/ directory or symlink in worker worktree %s", req.Worktree)
		}

		chgDir := filepath.Join(req.Worktree, "openspec", "changes", req.Change)
		switch req.Role {
		case "plan":
			_ = os.MkdirAll(filepath.Join(chgDir, "specs", "greeting"), 0o755)
			_ = os.WriteFile(filepath.Join(chgDir, "proposal.md"), []byte("# Proposal\n\n## Why\nAdd greeting.\n\n## What Changes\n- Add feature.txt in backend.\n"), 0o644)
			_ = os.WriteFile(filepath.Join(chgDir, "design.md"), []byte("# Design\n\n## Goals\n- Clean greeting implementation.\n"), 0o644)
			_ = os.WriteFile(filepath.Join(chgDir, "tasks.md"), []byte("# Tasks\n\n## 1. Implementation\n- [ ] 1.1 Create feature.txt in repos/backend\n"), 0o644)
			specContent := `## Purpose
This capability provides the greeting feature in the backend service with deterministic verification.

## ADDED Requirements

### Requirement: Greeting output
The backend SHALL provide feature.txt with greeting content.

#### Scenario: Greeting file present
- **WHEN** the backend is built
- **THEN** feature.txt contains hello world
`
			_ = os.WriteFile(filepath.Join(chgDir, "specs", "greeting", "spec.md"), []byte(specContent), 0o644)
			return AgentRunResult{Output: "plan artifacts written", ExitCode: 0, UsedEngine: "mock-plan"}, nil

		case "build":
			// Implement code in child repo (repos/backend) and mark task [x] in tasks.md
			childDir := filepath.Join(req.Worktree, "repos", "backend")
			if err := os.WriteFile(filepath.Join(childDir, "feature.txt"), []byte("hello world\n"), 0o644); err != nil {
				return AgentRunResult{ExitCode: 1, Output: err.Error()}, nil
			}
			_ = os.WriteFile(filepath.Join(chgDir, "tasks.md"), []byte("# Tasks\n\n## 1. Implementation\n- [x] 1.1 Create feature.txt in repos/backend\n"), 0o644)
			return AgentRunResult{Output: "build completed", ExitCode: 0, UsedEngine: "mock-build"}, nil
		}
		return AgentRunResult{ExitCode: 1, Output: "unknown role"}, nil
	}

	judge, err := evaluator.NewJudge(umbrellaDir, st)
	if err != nil {
		t.Fatal(err)
	}

	// Step 1: Run Dispatcher
	disp, err := NewDispatcher(DispatcherOptions{
		Root:        umbrellaDir,
		Store:       st,
		Judge:       judge,
		MaxWorkers:  2,
		AgentRunner: agentRunner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp.Run(ctx); err != nil {
		t.Fatalf("Dispatcher.Run failed: %v", err)
	}

	if len(seenSlots) < 2 || seenSlots[0] != 1 {
		qs, _ := st.ListQuarantine(ctx, "")
		evts, _ := st.QueryEvents(ctx, store.EventFilter{})
		t.Fatalf("expected worker slot 1 to be used across plan & build, got %v\nquarantine=%+v\nevents=%+v", seenSlots, qs, evts)
	}

	// Verify ReadyQueue has the manifest
	readyList, err := st.ListReadyQueue(ctx)
	if err != nil || len(readyList) != 1 {
		qs, _ := st.ListQuarantine(ctx, "")
		evts, _ := st.QueryEvents(ctx, store.EventFilter{})
		t.Fatalf("expected 1 manifest in the merge queue, got %v (err=%v)\nquarantine=%+v\nevents=%+v", readyList, err, qs, evts)
	}
	if readyList[0].ChangeName != "add-greeting" || len(readyList[0].Capabilities) != 1 || readyList[0].Capabilities[0] != "greeting" {
		t.Fatalf("unexpected manifest in the merge queue: %+v", readyList[0])
	}
	if readyList[0].Assessment == nil || readyList[0].Assessment["model"] != "jev-1.14.0" {
		t.Fatalf("expected Jev assessment in manifest, got %+v", readyList[0].Assessment)
	}

	// Step 2: Run Merger to integrate the ready manifest
	merger, err := NewMerger(MergerOptions{
		Root:  umbrellaDir,
		Store: st,
		Watch: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := merger.Run(ctx); err != nil {
		t.Fatalf("Merger.Run failed: %v", err)
	}

	// Verify ReadyQueue is now empty
	readyAfter, err := st.ListReadyQueue(ctx)
	if err != nil || len(readyAfter) != 0 {
		t.Fatalf("expected an empty merge queue after merge, got %v", readyAfter)
	}

	// Verify worker worktree was cleaned up
	workerWT := filepath.Join(umbrellaDir, ".worktrees", "worker-add-greeting")
	if dirExists(workerWT) {
		t.Fatalf("expected worker worktree %s to be removed after merge", workerWT)
	}

	// Verify origin/master has the commit with trailer OpenSpec-Change: add-greeting and Risk-Assessment
	rootRepo, err := gitx.Open(umbrellaDir)
	if err != nil {
		t.Fatal(err)
	}
	hasTrailer, err := rootRepo.HasCommitWithTrailer("origin/master", "add-greeting")
	if err != nil || !hasTrailer {
		t.Fatalf("expected origin/master to have commit with trailer OpenSpec-Change: add-greeting, got %v (err=%v)", hasTrailer, err)
	}
	commitLog := mustGit(t, ctx, umbrellaDir, "log", "-n", "1", "origin/master")
	if !strings.Contains(commitLog, "Risk-Assessment:") {
		t.Fatalf("expected commit message on origin/master to carry Risk-Assessment trailer, got:\n%s", commitLog)
	}

	// Verify canonical spec was created in origin/master
	specBlob, err := rootRepo.ReadBlob("origin/master", "openspec/specs/greeting/spec.md")
	if err != nil || !strings.Contains(string(specBlob), "Greeting output") {
		t.Fatalf("expected canonical spec in origin/master, got err=%v, content=%q", err, string(specBlob))
	}

	// Verify item moved from .foreman-kit/agent_backlog.json to .foreman-kit/agent_history.json
	backlogAfter := readBacklogItemsFromFile(filepath.Join(umbrellaDir, ".foreman-kit", "agent_backlog.json"))
	if len(backlogAfter) != 0 {
		t.Fatalf("expected empty .foreman-kit/agent_backlog.json after merge, got %+v", backlogAfter)
	}
	historyAfter := readBacklogItemsFromFile(filepath.Join(umbrellaDir, ".foreman-kit", "agent_history.json"))
	if len(historyAfter) != 1 || historyAfter[0].ChangeName != "add-greeting" || historyAfter[0].IntegratedAt == "" {
		t.Fatalf("expected add-greeting in .foreman-kit/agent_history.json, got %+v", historyAfter)
	}

	// Verify merge_attempts.state returned to IDLE
	ws, err := st.GetWorkspaceState(ctx)
	if err != nil || ws.MergerState != MergerStateIdle {
		t.Fatalf("expected merger_state=IDLE after completion, got %+v (err=%v)", ws, err)
	}
}

func TestQuarantineAutomaticRetryAndConflictRebuildInCoordinatedRun(t *testing.T) {
	ctx := context.Background()
	umbrellaDir, childBareDir, _, st := setupTestWorkspaceWithSubmodule(t)

	// 1. Test Quarantine + Automatic Retry (AdvanceQuarantineRetry) + repairs.md recording
	backlog := `[{"change_name": "retry-demo", "description": "Test quarantine and retry", "priority": "high"}]`
	if err := os.WriteFile(filepath.Join(umbrellaDir, ".foreman-kit", "agent_backlog.json"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-populate a blocked quarantine record from 2 hours ago (blocked_changes.retry_after is 1h, blocked_changes.retries is 1)
	twoHoursAgo := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	if err := st.BlockChange(ctx, store.QuarantineRecord{
		ChangeName: "retry-demo",
		Reason:     "gate_failure",
		Details:    "previous gate failed with secret_token=supersecret123",
		BlockedAt:  twoHoursAgo,
	}); err != nil {
		t.Fatal(err)
	}

	// Also test conflict rebuilds: push a conflicting commit to childBareDir while worker builds,
	// or test that a repair round writes repairs.md with redacted secrets
	buildCallCount := 0
	var seenRetryPrompt bool

	agentRunner := func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		chgDir := filepath.Join(req.Worktree, "openspec", "changes", req.Change)
		if strings.Contains(req.Prompt, "retried automatically (retry 1 of 1)") {
			seenRetryPrompt = true
			if strings.Contains(req.Prompt, "supersecret123") {
				t.Errorf("expected secret in retry prompt to be redacted, got: %s", req.Prompt)
			}
		}

		if req.Role == "plan" {
			_ = os.MkdirAll(filepath.Join(chgDir, "specs", "retrycap"), 0o755)
			_ = os.WriteFile(filepath.Join(chgDir, "proposal.md"), []byte("# Proposal\n\n## Why\nRetry demo.\n"), 0o644)
			_ = os.WriteFile(filepath.Join(chgDir, "design.md"), []byte("# Design\n\n## Goals\n- Retry demo.\n"), 0o644)
			_ = os.WriteFile(filepath.Join(chgDir, "tasks.md"), []byte("# Tasks\n\n## 1. Impl\n- [ ] 1.1 Build feature\n"), 0o644)
			specContent := `## Purpose
This capability tests automatic quarantine retry, repair rounds, and conflict rebuilds in the orchestrator.

## ADDED Requirements

### Requirement: Retry behavior
The system SHALL recover after retry.

#### Scenario: Retried build
- **WHEN** retried
- **THEN** it succeeds
`
			_ = os.WriteFile(filepath.Join(chgDir, "specs", "retrycap", "spec.md"), []byte(specContent), 0o644)
			return AgentRunResult{Output: "planned", ExitCode: 0, UsedEngine: "mock"}, nil
		}

		buildCallCount++
		childDir := filepath.Join(req.Worktree, "repos", "backend")
		if buildCallCount == 1 {
			// First build attempt: touch repos/backend without feature.txt so gate fails and triggers repair-1!
			_ = os.WriteFile(filepath.Join(childDir, "incomplete.txt"), []byte("wip\n"), 0o644)
			_ = os.WriteFile(filepath.Join(chgDir, "tasks.md"), []byte("# Tasks\n\n## 1. Impl\n- [x] 1.1 Build feature\n"), 0o644)
			return AgentRunResult{Output: "first build attempt without feature.txt", ExitCode: 0, UsedEngine: "mock"}, nil
		}

		// Second build attempt (repair-1): create feature.txt so gate passes!
		_ = os.WriteFile(filepath.Join(childDir, "feature.txt"), []byte("fixed in repair round\n"), 0o644)
		_ = os.WriteFile(filepath.Join(chgDir, "tasks.md"), []byte("# Tasks\n\n## 1. Impl\n- [x] 1.1 Build feature\n"), 0o644)
		return AgentRunResult{Output: "second build attempt fixed feature.txt", ExitCode: 0, UsedEngine: "mock"}, nil
	}

	_ = childBareDir
	if err := RunCoordinated(ctx, CoordinatedOptions{
		Root:           umbrellaDir,
		Store:          st,
		MaxWorkers:     1,
		RepairAttempts: 2,
		AgentRunner:    agentRunner,
	}); err != nil {
		t.Fatalf("RunCoordinated failed: %v", err)
	}

	if !seenRetryPrompt {
		qs, _ := st.ListQuarantine(ctx, "")
		evts, _ := st.QueryEvents(ctx, store.EventFilter{})
		t.Fatalf("expected first agent call after automatic retry to receive retry feedback in prompt\nquarantine=%+v\nevents=%+v", qs, evts)
	}
	if buildCallCount != 2 {
		qs, _ := st.ListQuarantine(ctx, "")
		evts, _ := st.QueryEvents(ctx, store.EventFilter{})
		t.Fatalf("expected 2 build calls (initial + 1 repair round), got %d\nquarantine=%+v\nevents=%+v", buildCallCount, qs, evts)
	}

	// Verify the change was integrated and repairs.md was archived with the change on origin/master
	rootRepo, err := gitx.Open(umbrellaDir)
	if err != nil {
		t.Fatal(err)
	}
	hasTrailer, err := rootRepo.HasCommitWithTrailer("origin/master", "retry-demo")
	if err != nil || !hasTrailer {
		t.Fatalf("expected retry-demo to be integrated on origin/master, got %v (err=%v)", hasTrailer, err)
	}

	// Verify quarantine was cleared after integration
	qAfter, err := st.GetQuarantine(ctx, "retry-demo")
	if err != nil || qAfter != nil {
		t.Fatalf("expected quarantine record to be forgotten after integration, got %+v (err=%v)", qAfter, err)
	}
}

func TestMergerRebaseConflictSendBackAndRebuild(t *testing.T) {
	ctx := context.Background()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	// Two parallel changes (change-a and change-b) that touch different capabilities so both build in parallel,
	// but both edit README.md in repos/backend on the same lines so the second one conflicts in Merger and is sent back for rebuild!
	backlog := `[
  {"change_name": "change-a", "description": "First README change", "priority": "high", "impact_score": 9},
  {"change_name": "change-b", "description": "Second README change", "priority": "high", "impact_score": 8}
]`
	if err := os.WriteFile(filepath.Join(umbrellaDir, ".foreman-kit", "agent_backlog.json"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var changeBRebuildPromptSeen bool

	agentRunner := func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		chgDir := filepath.Join(req.Worktree, "openspec", "changes", req.Change)
		capName := "cap-" + req.Change
		if req.Role == "plan" {
			_ = os.MkdirAll(filepath.Join(chgDir, "specs", capName), 0o755)
			_ = os.WriteFile(filepath.Join(chgDir, "proposal.md"), []byte("# Proposal\n\n## Why\nUpdate README.\n"), 0o644)
			_ = os.WriteFile(filepath.Join(chgDir, "design.md"), []byte("# Design\n\n## Goals\n- Update README.\n"), 0o644)
			_ = os.WriteFile(filepath.Join(chgDir, "tasks.md"), []byte("# Tasks\n\n## 1. Impl\n- [ ] 1.1 Update README and feature.txt\n"), 0o644)
			specContent := `## Purpose
This capability updates the backend README and feature file for testing automatic conflict rebuilds.

## ADDED Requirements

### Requirement: Update README
The backend SHALL update README.md and feature.txt.

#### Scenario: README updated
- **WHEN** built
- **THEN** README.md and feature.txt are updated
`
			_ = os.WriteFile(filepath.Join(chgDir, "specs", capName, "spec.md"), []byte(specContent), 0o644)
			return AgentRunResult{Output: "planned", ExitCode: 0, UsedEngine: "mock"}, nil
		}

		// BUILD role
		if strings.Contains(req.Prompt, "feat/spec-change-b-conflict-1") {
			mu.Lock()
			changeBRebuildPromptSeen = true
			mu.Unlock()
		}
		childDir := filepath.Join(req.Worktree, "repos", "backend")
		existingReadme, _ := os.ReadFile(filepath.Join(childDir, "README.md"))
		// Append or replace line 1 to cause conflict on first pass, then merge cleanly on rebuild
		var newReadme string
		if strings.Contains(string(existingReadme), "change-a line") {
			newReadme = "# Backend\nchange-a line\nchange-b line\n"
		} else {
			newReadme = "# Backend (" + req.Change + " line)\n"
		}
		_ = os.WriteFile(filepath.Join(childDir, "README.md"), []byte(newReadme), 0o644)
		_ = os.WriteFile(filepath.Join(childDir, "feature.txt"), []byte("ok-"+req.Change+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(chgDir, "tasks.md"), []byte("# Tasks\n\n## 1. Impl\n- [x] 1.1 Update README and feature.txt\n"), 0o644)
		return AgentRunResult{Output: "built", ExitCode: 0, UsedEngine: "mock"}, nil
	}

	// Step 1: Run Dispatcher once (both change-a and change-b build from the initial base and enter ReadyQueue)
	disp1, err := NewDispatcher(DispatcherOptions{
		Root:        umbrellaDir,
		Store:       st,
		MaxWorkers:  2,
		AgentRunner: agentRunner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp1.Run(ctx); err != nil {
		t.Fatal(err)
	}

	ready1, _ := st.ListReadyQueue(ctx)
	if len(ready1) != 2 {
		t.Fatalf("expected 2 items in the merge queue, got %d", len(ready1))
	}

	// Step 2: Run Merger once: first item integrates, second item conflicts on README.md and calls store.RequestRebuild!
	merger1, err := NewMerger(MergerOptions{
		Root:  umbrellaDir,
		Store: st,
		Watch: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := merger1.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// Check which change was sent back with status="rebuild_requested"
	rebuilds, err := st.ListQuarantine(ctx, "rebuild_requested")
	if err != nil || len(rebuilds) != 1 || rebuilds[0].Rebuilds != 1 {
		t.Fatalf("expected 1 change with status=rebuild_requested and rebuilds=1, got %+v (err=%v)", rebuilds, err)
	}

	// Step 3: RunCoordinated to rebuild the conflicted change on the new base and merge it!
	if err := RunCoordinated(ctx, CoordinatedOptions{
		Root:        umbrellaDir,
		Store:       st,
		MaxWorkers:  1,
		AgentRunner: agentRunner,
	}); err != nil {
		t.Fatalf("RunCoordinated for rebuild failed: %v", err)
	}

	if !changeBRebuildPromptSeen {
		// Note: either change-a or change-b could have been the second one depending on enqueue order
		evts, _ := st.QueryEvents(ctx, store.EventFilter{Event: "change.rebuild"})
		if len(evts) != 1 {
			allEvts, _ := st.QueryEvents(ctx, store.EventFilter{})
			qs, _ := st.ListQuarantine(ctx, "")
			t.Fatalf("expected 1 change.rebuild event, got %+v\nquarantine=%+v\nallEvents=%+v", evts, qs, allEvts)
		}
	}

	rootRepo, err := gitx.Open(umbrellaDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, chg := range []string{"change-a", "change-b"} {
		ok, err := rootRepo.HasCommitWithTrailer("origin/master", chg)
		if err != nil || !ok {
			t.Fatalf("expected %s to be integrated on origin/master, got %v (err=%v)", chg, ok, err)
		}
	}
}

func TestMergerIdempotentPushBaseDiffAndStaleHeal(t *testing.T) {
	ctx := context.Background()
	umbrellaDir, _, umbrellaBareDir, st := setupTestWorkspaceWithSubmodule(t)

	backlog := `[{"change_name": "merge-resilience-one", "description": "Verify idempotent push, base diff, and stale manifest healing", "priority": "high"}]`
	if err := os.WriteFile(filepath.Join(umbrellaDir, ".foreman-kit", "agent_backlog.json"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}

	disp, err := NewDispatcher(DispatcherOptions{
		Root:       umbrellaDir,
		Store:      st,
		MaxWorkers: 1,
		AgentRunner: func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
			chgDir := filepath.Join(req.Worktree, "openspec", "changes", req.Change)
			if req.Role == "plan" {
				_ = os.MkdirAll(filepath.Join(chgDir, "specs", "resilience-cap"), 0o755)
				_ = os.WriteFile(filepath.Join(chgDir, "proposal.md"), []byte("# Proposal\n\n## Why\nVerify merge resilience.\n\n## What Changes\n- Add feature.txt.\n"), 0o644)
				_ = os.WriteFile(filepath.Join(chgDir, "design.md"), []byte("# Design\n\n## Goals\n- Idempotent push.\n"), 0o644)
				_ = os.WriteFile(filepath.Join(chgDir, "tasks.md"), []byte("# Tasks\n\n- [ ] 1.1 Create feature.txt in repos/backend\n"), 0o644)
				specContent := "## Purpose\nVerifies that idempotent push, base diff, and stale manifest healing work.\n\n## ADDED Requirements\n\n### Requirement: Resilience verification\nThe backend SHALL provide feature.txt.\n\n#### Scenario: File present\n- **WHEN** built\n- **THEN** feature.txt exists\n"
				_ = os.WriteFile(filepath.Join(chgDir, "specs", "resilience-cap", "spec.md"), []byte(specContent), 0o644)
				return AgentRunResult{ExitCode: 0, UsedEngine: "claude"}, nil
			}
			childFile := filepath.Join(req.Worktree, "repos", "backend", "feature.txt")
			_ = os.WriteFile(childFile, []byte("resilience-payload\n"), 0o644)
			_ = os.WriteFile(filepath.Join(chgDir, "tasks.md"), []byte("# Tasks\n\n- [x] 1.1 Create feature.txt in repos/backend\n"), 0o644)
			return AgentRunResult{ExitCode: 0, UsedEngine: "claude"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp.Run(ctx); err != nil {
		t.Fatal(err)
	}
	_ = disp.Close()

	// Advance the worker branch by 1 commit without updating ready_manifests.head_sha,
	// and verify the Merger auto-heals head_sha (merge.head_updated) instead of blocking as manifest_stale,
	// while removing any stale log/approved/<change> marker
	approvedMarker := filepath.Join(umbrellaDir, "log", "approved", "merge-resilience-one")
	_ = os.MkdirAll(filepath.Dir(approvedMarker), 0o755)
	_ = os.WriteFile(approvedMarker, []byte("approved\n"), 0o644)

	workerWT := filepath.Join(umbrellaDir, ".worktrees", "worker-merge-resilience-one")
	_ = os.WriteFile(filepath.Join(workerWT, "openspec", "changes", "merge-resilience-one", "design.md"), []byte("# Design\nUpdated after ready enqueue\n"), 0o644)
	mustGit(t, ctx, workerWT, "add", "openspec/changes/merge-resilience-one/design.md")
	mustGit(t, ctx, workerWT, "commit", "-m", "chore(spec): amend design")

	// Advance origin/master by 1 commit BEFORE Merger runs so forkPoint != origin/master,
	// and verify GateRunner receives BaseRef == current origin/master SHA (never stale forkPoint)
	sideClone := filepath.Join(t.TempDir(), "side-umbrella")
	mustGit(t, ctx, t.TempDir(), "clone", "-b", "master", umbrellaBareDir, sideClone)
	mustGit(t, ctx, sideClone, "config", "user.name", "CycleBot")
	mustGit(t, ctx, sideClone, "config", "user.email", "cycle@example.com")
	_ = os.WriteFile(filepath.Join(sideClone, "MEMORY.md"), []byte("# Memory\n- prior cycle note\n"), 0o644)
	mustGit(t, ctx, sideClone, "add", "MEMORY.md")
	mustGit(t, ctx, sideClone, "commit", "-m", "chore(memory): prior cycle")
	mustGit(t, ctx, sideClone, "push", "origin", "master")
	expectedIntegrationBase := strings.TrimSpace(mustGit(t, ctx, sideClone, "rev-parse", "HEAD"))

	// Simulate a concurrent push to origin/master occurring WHILE gates run on attempt 1,
	// so PUSHING_CHILDREN succeeds on attempt 1, PUSHING_ROOT fails with non-fast-forward on attempt 1,
	// and attempt 2 recovers and pushes umbrella WITHOUT re-running GateRunner
	gateCalls := 0
	var observedBaseRef string
	merger, err := NewMerger(MergerOptions{
		Root:  umbrellaDir,
		Store: st,
		GateRunner: func(ctx context.Context, req GateRunRequest) (GateRunResult, error) {
			gateCalls++
			observedBaseRef = req.BaseRef
			if gateCalls == 1 {
				// Push a concurrent commit to umbrellaBareDir while gates are running!
				_ = os.WriteFile(filepath.Join(sideClone, "MEMORY.md"), []byte("# Memory\n- concurrent cycle note during gates\n"), 0o644)
				mustGit(t, ctx, sideClone, "add", "MEMORY.md")
				mustGit(t, ctx, sideClone, "commit", "-m", "chore(memory): concurrent cycle during gates")
				mustGit(t, ctx, sideClone, "push", "origin", "master")
			}
			return DefaultGateRunner(st)(ctx, req)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := merger.Run(ctx); err != nil {
		t.Fatal(err)
	}
	_ = merger.Close()

	if gateCalls != 1 {
		t.Fatalf("expected GateRunner to run ONLY ONCE (1) across PUSHING_ROOT retry, ran %d times", gateCalls)
	}
	if observedBaseRef != expectedIntegrationBase {
		t.Fatalf("expected GateRunner BaseRef=%s (origin/master), got %s", expectedIntegrationBase, observedBaseRef)
	}
	headUpdEvts, _ := st.QueryEvents(ctx, store.EventFilter{Event: "merge.head_updated", ChangeName: "merge-resilience-one"})
	if len(headUpdEvts) != 1 {
		t.Fatalf("expected 1 merge.head_updated event, got %d", len(headUpdEvts))
	}
	if fileExists(approvedMarker) {
		t.Fatalf("expected stale approval marker %s to be removed when head_sha auto-heals", approvedMarker)
	}
	rootRepo, _ := gitx.Open(umbrellaDir)
	_ = rootRepo.Fetch(ctx, "origin", "master")
	if ok, _ := rootRepo.HasCommitWithTrailer("origin/master", "merge-resilience-one"); !ok {
		t.Fatalf("expected merge-resilience-one to be integrated on origin/master after PUSHING_ROOT recovery")
	}
}

func TestSmartGateFailureExtractionAndRegressionSemaphore(t *testing.T) {
	// 1. Verify ExtractOpenSpecInvalidItems extracts only invalid items even when surrounded by 200 valid items
	var manyItems []map[string]any
	for i := 0; i < 100; i++ {
		manyItems = append(manyItems, map[string]any{"id": "valid-early", "valid": true})
	}
	manyItems = append(manyItems, map[string]any{
		"id":     "broken-middle-spec",
		"valid":  false,
		"issues": []map[string]any{{"message": "missing Purpose section"}},
	})
	for i := 0; i < 100; i++ {
		manyItems = append(manyItems, map[string]any{"id": "valid-late", "valid": true})
	}
	rawJSONBytes, _ := json.MarshalIndent(map[string]any{"items": manyItems, "summary": map[string]any{"failed": 1}}, "", "  ")
	extracted := ExtractOpenSpecInvalidItems(string(rawJSONBytes))
	if !strings.Contains(extracted, "broken-middle-spec") || !strings.Contains(extracted, "missing Purpose section") || strings.Contains(extracted, "valid-late") {
		t.Fatalf("expected only broken-middle-spec in extracted JSON, got:\n%s", extracted)
	}

	// 2. Verify ExtractGateFailureExcerpt extracts RSpec Failures block and GATE TIMEOUT banner
	rspecSample := strings.Repeat(".", 500) + "\n\nFailures:\n\n  1) User login fails\n     Failure/Error: expect(res).to eq(200)\n\nFailed examples:\n\nrspec ./spec/user_spec.rb:12\n" + strings.Repeat("Coverage report line\n", 50)
	rspecExcerpt := ExtractGateFailureExcerpt("core:test", rspecSample, 1)
	if !strings.Contains(rspecExcerpt, "1) User login fails") || !strings.Contains(rspecExcerpt, "rspec ./spec/user_spec.rb:12") {
		t.Fatalf("expected RSpec failure block, got:\n%s", rspecExcerpt)
	}
	timeoutExcerpt := ExtractGateFailureExcerpt("core:regression:system", "some output", 124)
	if !strings.HasPrefix(timeoutExcerpt, "GATE TIMEOUT: core:regression:system exceeded wall-clock limit (exit 124).") {
		t.Fatalf("expected GATE TIMEOUT banner, got:\n%s", timeoutExcerpt)
	}
}

func TestDetailedUncheckedTasksAndNativeOpenSpecValidator(t *testing.T) {
	tmp := t.TempDir()
	chg := "validator-check"
	chgDir := filepath.Join(tmp, "openspec", "changes", chg)
	_ = os.MkdirAll(filepath.Join(chgDir, "specs", "overlong-cap"), 0o755)

	// 1. Verify UncheckedTasksDetails reports exact 1-based line numbers and task descriptions
	tasksFile := filepath.Join(chgDir, "tasks.md")
	tasksContent := "# Tasks\n\n- [x] 1.1 Implement model\n- [ ] 1.2 Add controller spec and mark complete\n- [x] 1.3 Run rubocop\n"
	_ = os.WriteFile(tasksFile, []byte(tasksContent), 0o644)
	done, total, details := UncheckedTasksDetails(tasksFile, chg)
	if done != 2 || total != 3 || !strings.Contains(details, "Line 4: - [ ] 1.2 Add controller spec and mark complete") {
		t.Fatalf("expected Line 4 unchecked item in details, got done=%d total=%d details:\n%s", done, total, details)
	}

	// 2. Verify ValidateOpenSpecNative rejects >500-char requirement prose and TBD Purpose
	overlongSpec := "## Purpose\nThis capability has a valid fifty-plus character purpose description for testing.\n\n## ADDED Requirements\n\n### Requirement: Too long prose\n" +
		strings.Repeat("X", 505) + "\n\n#### Scenario: Check limit\n- **WHEN** validated\n- **THEN** it fails\n"
	specFile := filepath.Join(chgDir, "specs", "overlong-cap", "spec.md")
	_ = os.WriteFile(specFile, []byte(overlongSpec), 0o644)
	if ok, msg := ValidateOpenSpecNative(tmp, chg); ok || !strings.Contains(msg, "exceeds 500 characters") {
		t.Fatalf("expected >500-char requirement to fail ValidateOpenSpecNative, got ok=%v msg=%s", ok, msg)
	}
}

func TestValidateOpenSpecNativeAndBuiltinArchiveRemovedAndRenamed(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	// Create canonical spec with two requirements
	canonDir := filepath.Join(tmp, "openspec", "specs", "auth")
	if err := os.MkdirAll(canonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	canonContent := `## Purpose
This capability governs authentication flows, session tokens, and login behavior across services.

## Requirements

### Requirement: Legacy Token
The system SHALL accept legacy static tokens.

#### Scenario: Legacy token used
- **WHEN** a client sends a legacy token
- **THEN** authentication succeeds

### Requirement: Old Login
The system SHALL authenticate users with password.

#### Scenario: Password login
- **WHEN** a user submits valid credentials
- **THEN** a session is established
`
	canonSpecPath := filepath.Join(canonDir, "spec.md")
	if err := os.WriteFile(canonSpecPath, []byte(canonContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a change with REMOVED and RENAMED requirements (no ADDED or MODIFIED, no #### Scenario: blocks)
	chg := "deprecate-legacy-auth"
	chgDir := filepath.Join(tmp, "openspec", "changes", chg)
	if err := os.MkdirAll(filepath.Join(chgDir, "specs", "auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(chgDir, "proposal.md"), []byte("# Proposal\n\n## Why\nRemove legacy token and rename login.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(chgDir, "design.md"), []byte("# Design\n\n## Goals\n- Clean up auth spec.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(chgDir, "tasks.md"), []byte("# Tasks\n\n- [x] 1.1 Remove legacy token\n"), 0o644)

	deltaContent := `## REMOVED Requirements

### Requirement: Legacy Token

## RENAMED Requirements

- FROM: ` + "`### Requirement: Old Login`" + `
- TO: ` + "`### Requirement: Modern Login`" + `
`
	if err := os.WriteFile(filepath.Join(chgDir, "specs", "auth", "spec.md"), []byte(deltaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. ValidateOpenSpecNative must accept REMOVED/RENAMED delta without #### Scenario: blocks
	if ok, msg := ValidateOpenSpecNative(tmp, chg); !ok {
		t.Fatalf("expected ValidateOpenSpecNative to accept REMOVED/RENAMED delta spec, got ok=false msg=%s", msg)
	}

	// 2. BuiltinOpenSpec validate <change> must succeed
	valRes, err := BuiltinOpenSpec(ctx, tmp, "validate", chg, "--strict", "--json")
	if err != nil || !valRes.OK {
		t.Fatalf("expected BuiltinOpenSpec validate to pass for REMOVED/RENAMED delta, got: %+v (err=%v)", valRes, err)
	}

	// 3. BuiltinOpenSpec archive must remove Legacy Token, rename Old Login -> Modern Login,
	// never append raw delta headers, and report totals.removed=1, totals.renamed=1
	archRes, err := BuiltinOpenSpec(ctx, tmp, "archive", chg, "--yes", "--json")
	if err != nil || !archRes.OK {
		t.Fatalf("expected BuiltinOpenSpec archive to succeed, got: %+v (err=%v)", archRes, err)
	}
	var archPayload struct {
		Archive struct {
			Totals map[string]int `json:"totals"`
		} `json:"archive"`
	}
	if err := json.Unmarshal([]byte(archRes.Output), &archPayload); err != nil {
		t.Fatalf("failed to unmarshal archive output: %v (%s)", err, archRes.Output)
	}
	if archPayload.Archive.Totals["removed"] != 1 || archPayload.Archive.Totals["renamed"] != 1 ||
		archPayload.Archive.Totals["added"] != 0 || archPayload.Archive.Totals["modified"] != 0 {
		t.Fatalf("unexpected archive totals: %+v", archPayload.Archive.Totals)
	}

	updatedCanonBytes, err := os.ReadFile(canonSpecPath)
	if err != nil {
		t.Fatal(err)
	}
	updatedCanon := string(updatedCanonBytes)
	if strings.Contains(updatedCanon, "Legacy Token") {
		t.Fatalf("expected Legacy Token requirement to be removed from canonical spec, got:\n%s", updatedCanon)
	}
	if strings.Contains(updatedCanon, "### Requirement: Old Login") || !strings.Contains(updatedCanon, "### Requirement: Modern Login") {
		t.Fatalf("expected Old Login to be renamed to Modern Login in canonical spec, got:\n%s", updatedCanon)
	}
	if strings.Contains(updatedCanon, "## REMOVED Requirements") || strings.Contains(updatedCanon, "## RENAMED Requirements") {
		t.Fatalf("canonical spec must not contain raw delta headers, got:\n%s", updatedCanon)
	}

	// 4. BuiltinOpenSpec validate --specs must pass on the resulting canonical spec
	specsVal, err := BuiltinOpenSpec(ctx, tmp, "validate", "--specs", "--strict", "--json")
	if err != nil || !specsVal.OK {
		t.Fatalf("expected canonical specs after REMOVED/RENAMED archive to pass validation, got: %+v (err=%v)", specsVal, err)
	}
}

func TestMatchRepoGateGlobAndMissingEnvScript(t *testing.T) {
	// 1. Verify matchRepoGateGlob delegates to proc.MatchRegressionGlob (intermediate ** and brace expansion)
	cases := []struct {
		pattern string
		target  string
		want    bool
	}{
		{"db/**/*.rb", "db/migrate/20261108_create_users.rb", true},
		{"db/**/*.rb", "db/seeds.rb", true},
		{"db/**/*.rb", "app/models/user.rb", false},
		{"{Gemfile,Gemfile.lock}", "Gemfile", true},
		{"{Gemfile,Gemfile.lock}", "Gemfile.lock", true},
		{"{Gemfile,Gemfile.lock}", "package.json", false},
		{"app/**/models/*.rb", "app/domains/billing/models/invoice.rb", true},
	}
	for _, tc := range cases {
		if got := matchRepoGateGlob(tc.pattern, tc.target); got != tc.want {
			t.Errorf("matchRepoGateGlob(%q, %q) = %v, want %v", tc.pattern, tc.target, got, tc.want)
		}
	}

	// 2. Verify DefaultGateRunner fails immediately with ExitCode: 2 when env_script is missing
	ctx := context.Background()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)
	childDir := filepath.Join(umbrellaDir, "repos", "backend")
	if err := os.Remove(filepath.Join(childDir, "scripts", "custom_env.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "feature.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := DefaultGateRunner(st)(ctx, GateRunRequest{
		Change:   "test-missing-env",
		Worktree: umbrellaDir,
		BaseRef:  "HEAD",
		Slot:     3,
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error from DefaultGateRunner: %v", err)
	}
	if res.OK || res.ExitCode != 2 || !strings.Contains(res.Output, "env_script") {
		t.Fatalf("expected ExitCode=2 and env_script error when env_script is missing, got: %+v", res)
	}
}

func TestBacklogMidWriteRecoveryAndSyncRootBacklogLockGuard(t *testing.T) {
	ctx := context.Background()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)

	backlogPath := filepath.Join(umbrellaDir, ".foreman-kit", "agent_backlog.json")
	validJSON := `[{"change_name": "item-one", "priority": "high"}, {"change_name": "item-two", "priority": "normal"}]` + "\n"
	if err := os.WriteFile(backlogPath, []byte(validJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	disp, err := NewDispatcher(DispatcherOptions{
		Root:  umbrellaDir,
		Store: st,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disp.Close()

	// 1. Initial load populates lastValidBacklog
	items1, _, _ := disp.loadBacklog(ctx, true)
	if len(items1) != 2 || items1[0].ChangeName != "item-one" {
		t.Fatalf("expected 2 items on initial load, got %+v", items1)
	}

	// 2. Simulate partial write (truncated/invalid non-empty JSON) and verify loadBacklog reuses lastValidBacklog
	if err := os.WriteFile(backlogPath, []byte(`[{"change_name": "item-one", `), 0o644); err != nil {
		t.Fatal(err)
	}
	items2, _, _ := disp.loadBacklog(ctx, true)
	if len(items2) != 2 || items2[0].ChangeName != "item-one" {
		t.Fatalf("expected loadBacklog to reuse lastValidBacklog on mid-write JSON error, got %+v", items2)
	}

	// 3. Restore valid backlog and verify ArchiveBacklogInDir does not rewrite agent_backlog.json when no item from it is archived
	if err := os.WriteFile(backlogPath, []byte(validJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	ArchiveBacklogInDir(umbrellaDir, map[string]any{"change_name": "other-archived-change"}, "2026-11-08T12:00:00Z")
	afterBytes, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterBytes) != validJSON {
		t.Fatalf("expected ArchiveBacklogInDir not to rewrite backlogPath when no item was removed from it; got:\n%s", string(afterBytes))
	}

	// 4. Verify syncRootBacklog does not modify agent_backlog.json while a non-stale visual_qa lock is held
	_, err = st.AcquireLock(ctx, "visual_qa", store.LockOptions{
		OwnerType: "visual_qa",
		PID:       os.Getpid(),
		Slot:      97,
		TTL:       30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	merger, err := NewMerger(MergerOptions{
		Root:  umbrellaDir,
		Store: st,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer merger.Close()

	merger.syncRootBacklog(ctx, store.ReadyManifest{
		ChangeName: "item-one",
		Item:       map[string]any{"change_name": "item-one"},
	}, "HEAD", "2026-11-08T12:00:00Z")
	lockedBytes, _ := os.ReadFile(backlogPath)
	if string(lockedBytes) != validJSON {
		t.Fatalf("expected syncRootBacklog not to modify agent_backlog.json while visual_qa lock is active; got:\n%s", string(lockedBytes))
	}
	_ = st.ReleaseLock(ctx, "visual_qa", 0)
}
