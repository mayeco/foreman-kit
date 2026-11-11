package config

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

func setupTestUmbrella(t *testing.T) (string, *store.Store) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FOREMANKIT_APP_ROOT", root)

	// Seed config files from embedded seeds
	seeds, err := assets.WalkEmbedded("seeds")
	if err != nil {
		t.Fatalf("WalkEmbedded(seeds): %v", err)
	}
	for rel, b := range seeds {
		targetRel := rel
		if rel == "gitignore" {
			targetRel = ".gitignore"
		}
		dst := filepath.Join(root, filepath.FromSlash(targetRel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Write .foreman-kit/agents.yaml
	err = Write(root, []string{"claude", "hermes", "codex"}, map[string][]string{
		"plan":  {"claude", "hermes"},
		"build": {"codex", "claude"},
		"cycle": {"claude"},
	}, settings.SelectionInOrder)
	if err != nil {
		t.Fatalf("Write agents.yaml: %v", err)
	}

	dbPath := filepath.Join(root, "state.db")
	t.Setenv("FOREMANKIT_DB_PATH", dbPath)
	st, err := store.OpenAt(dbPath, "test-1234567890ab", root)
	if err != nil {
		t.Fatalf("store.OpenAt: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	return root, st
}

func TestSettingsGetAndParse(t *testing.T) {
	root, _ := setupTestUmbrella(t)

	if got := pipelineString(root, "repository.base_branch"); got != "main" {
		t.Fatalf("expected the seeded repository.base_branch=main, got %q", got)
	}
	if got := pipelineInt(root, "dispatch.workers"); got != 2 {
		t.Fatalf("expected the seeded dispatch.workers=2, got %d", got)
	}
	if got := settings.Duration(root, settings.GateTimeout); got != 45*time.Minute {
		t.Fatalf("expected the seeded gates.timeout=45m, got %v", got)
	}

	// A value the run was handed that the key does not accept (nothing --set would have taken) is
	// not read: the key keeps what its file holds, and the process is told what it was handed
	// (foreman-kit does not start with it).
	var stderr bytes.Buffer
	t.Setenv(settings.RunOverridesEnv, `{"pipeline.dispatch.workers":"0"}`)
	val, err := pipelineValue(root, "dispatch.workers", &stderr)
	if err != nil || val != 2 || stderr.Len() != 0 {
		t.Fatalf("expected the 2 of the file on a handed 0 for dispatch.workers, got %v (err=%v, said %q)", val, err, stderr.String())
	}
	if err := settings.RunOverridesProblem(); err == nil || !strings.Contains(err.Error(), "--set pipeline.dispatch.workers: must be a whole number above 0") {
		t.Fatalf("expected the handed value to be named, got %v", err)
	}
	t.Setenv(settings.RunOverridesEnv, "")

	// A value for one run, as --set gives it
	settingstest.Give(t, settings.BaseBranch, "develop")
	settingstest.Give(t, settings.Workers, "4")
	settingstest.Give(t, settings.GateTimeout, "90s")
	if got := pipelineString(root, "repository.base_branch"); got != "develop" {
		t.Fatalf("expected the run's repository.base_branch=develop, got %q", got)
	}
	if got := pipelineInt(root, "dispatch.workers"); got != 4 {
		t.Fatalf("expected the run's dispatch.workers=4, got %d", got)
	}
	if got := settings.Duration(root, settings.GateTimeout); got != 90*time.Second {
		t.Fatalf("expected the run's gates.timeout=90s, got %v", got)
	}

	// A setting that accepts 0
	settingstest.Give(t, settings.BlockedRetries, "0")
	val, err = pipelineValue(root, "blocked_changes.retries", &stderr)
	if err != nil || val != 0 {
		t.Fatalf("expected 0 for blocked_changes.retries, got %v (err=%v)", val, err)
	}

	// CLI handler: the value as the file writes it
	var outBuf, errBuf bytes.Buffer
	code := RunSettingsCLI(root, []string{"get", "repository.base_branch"}, &outBuf, &errBuf)
	if code != 0 || strings.TrimSpace(outBuf.String()) != "develop" {
		t.Fatalf("RunSettingsCLI failed: code=%d out=%q err=%q", code, outBuf.String(), errBuf.String())
	}
	outBuf.Reset()
	if code := RunSettingsCLI(root, []string{"get", "gates.timeout"}, &outBuf, &errBuf); code != 0 || strings.TrimSpace(outBuf.String()) != "1m30s" {
		t.Fatalf("a time is not printed with its unit: code=%d out=%q", code, outBuf.String())
	}
	outBuf.Reset()
	if code := RunSettingsCLI(root, []string{"get", "dispatch.priority_bump_every"}, &outBuf, &errBuf); code != 0 || strings.TrimSpace(outBuf.String()) != "6h" {
		t.Fatalf("code=%d out=%q", code, outBuf.String())
	}
	code = RunSettingsCLI(root, []string{"get", "nonexistent.key"}, &outBuf, &errBuf)
	if code != 2 {
		t.Fatalf("expected exit 2 on unknown setting, got %d", code)
	}
}

func TestOutsideCommitsSetting(t *testing.T) {
	root, _ := setupTestUmbrella(t)

	if got := pipelineString(root, "merge.outside_commits"); got != settings.OutsideCommitsVerify {
		t.Fatalf("expected the seeded merge.outside_commits=verify, got %q", got)
	}

	var stderr bytes.Buffer
	t.Setenv(settings.RunOverridesEnv, `{"pipeline.merge.outside_commits":"everything"}`)
	val, err := pipelineValue(root, "merge.outside_commits", &stderr)
	if err != nil || val != settings.OutsideCommitsVerify || stderr.Len() != 0 {
		t.Fatalf("expected the verify of the file for a handed value the key does not take, got %v (err=%v, said %q)", val, err, stderr.String())
	}
	if err := settings.RunOverridesProblem(); err == nil || !strings.Contains(err.Error(), "must be verify, ignore or include") {
		t.Fatalf("expected the handed value to be named with the valid ones, got %v", err)
	}
	t.Setenv(settings.RunOverridesEnv, "")
	settingstest.Give(t, settings.OutsideCommits, "include")
	if got := pipelineString(root, "merge.outside_commits"); got != settings.OutsideCommitsInclude {
		t.Fatalf("expected the run's include, got %q", got)
	}
	settingstest.Take(t, settings.OutsideCommits)

	var out bytes.Buffer
	if err := Set(root, "pipeline.merge.outside_commits", "include", false, &out); err != nil {
		t.Fatalf("Set pipeline.merge.outside_commits=include failed: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".foreman-kit", "pipeline.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "outside_commits: include") || !strings.Contains(string(raw), "# Commits pushed to a child repository outside the pipeline") {
		t.Fatalf("expected the value changed in place with its comments kept:\n%s", raw)
	}
	if got := pipelineString(root, "merge.outside_commits"); got != settings.OutsideCommitsInclude {
		t.Fatalf("expected include after Set, got %q", got)
	}
	if rep := Check(root)[settings.BaseBranch.FilePath()]; len(rep.Errors) > 0 || len(rep.Warnings) > 0 {
		t.Fatalf("expected .foreman-kit/pipeline.yaml to check clean, got %+v", rep)
	}
	if err := Set(root, "pipeline.merge.outside_commits", "everything", false, &out); err == nil || !strings.Contains(err.Error(), "must be verify, ignore or include") {
		t.Fatalf("expected Set to reject an unknown value, got %v", err)
	}
	// A word of before is refused like any other, and a time is written with its unit.
	if err := Set(root, "pipeline.merge.outside_commits", "integrated", false, &out); err == nil {
		t.Fatal("a word of before was accepted")
	}
	if err := Set(root, "pipeline.gates.timeout", "2700", false, &out); err == nil || !strings.Contains(err.Error(), "a time with its unit") {
		t.Fatalf("a bare number was accepted as a time: %v", err)
	}
	if err := Set(root, "pipeline.gates.timeout", "30m", false, &out); err != nil {
		t.Fatal(err)
	}
	if err := Set(root, "pipeline.dispatch.priority_bump_every", "never", false, &out); err != nil {
		t.Fatal(err)
	}
	if got := settings.Duration(root, settings.GateTimeout); got != 30*time.Minute {
		t.Fatalf("gates.timeout after Set: %v", got)
	}
	if got := settings.Duration(root, settings.PriorityBumpEvery); got != 0 {
		t.Fatalf("priority_bump_every: never reads as %v", got)
	}
}

func TestAgentsConfigAndStoreRegistry(t *testing.T) {
	root, st := setupTestUmbrella(t)

	// Verify all 13 supported engines
	if len(SUPPORTED) != 13 || len(SupportedOrder) != 13 {
		t.Fatalf("expected 13 supported engines, got %d", len(SUPPORTED))
	}
	parsed, err := ParseAgents("claude, hermes, cursor-agent, zcode, claude")
	if err != nil || len(parsed) != 4 {
		t.Fatalf("ParseAgents failed: %v, err=%v", parsed, err)
	}
	if _, err := ParseAgents("unknown-engine"); err == nil {
		t.Fatalf("expected error for unknown engine")
	}

	dirs := SkillsDirs([]string{"claude", "codex", "hermes"})
	expectedDirs := []string{".agents/skills", ".claude/skills", ".hermes/skills"}
	if strings.Join(dirs, ",") != strings.Join(expectedDirs, ",") {
		t.Fatalf("SkillsDirs mismatch: got %v, want %v", dirs, expectedDirs)
	}

	tools := Tools([]string{"copilot", "omp", "cursor-agent"})
	expectedTools := []string{"agents", "github-copilot", "oh-my-pi", "cursor"}
	if strings.Join(tools, ",") != strings.Join(expectedTools, ",") {
		t.Fatalf("Tools mismatch: got %v, want %v", tools, expectedTools)
	}

	eng, err := Engine(root, "plan")
	if err != nil || eng != "claude,hermes" {
		t.Fatalf("Engine(plan) got %q, err=%v", eng, err)
	}

	// Test Arrange / Claim / Release / InUse backed by SQLite store
	ctx := context.Background()
	pid := os.Getpid()
	settingstest.Give(t, settings.AgentSelection, settings.SelectionLeastBusy)

	// Pre-claim claude with current pid, then arrange for another alive pid (ppid)
	if err := Claim(ctx, root, st, "claude", pid); err != nil {
		t.Fatalf("Claim failed: %v", err)
	}
	inUse, err := InUse(ctx, root, st)
	if err != nil || inUse["claude"] != 1 {
		t.Fatalf("expected claude=1 in use, got %v (err=%v)", inUse, err)
	}

	ppid := os.Getppid()
	arranged, err := Arrange(ctx, root, st, "claude,hermes", ppid)
	if err != nil {
		t.Fatalf("Arrange failed: %v", err)
	}
	// Because claude is in use (1) and hermes is free (0), least_busy must pick hermes first
	if len(arranged) != 2 || arranged[0] != "hermes" || arranged[1] != "claude" {
		t.Fatalf("expected least_busy to pick [hermes claude], got %v", arranged)
	}

	_ = Release(ctx, root, st, pid)
	_ = Release(ctx, root, st, ppid)
	inUseAfter, _ := InUse(ctx, root, st)
	if len(inUseAfter) != 0 {
		t.Fatalf("expected 0 in use after Release, got %v", inUseAfter)
	}

	// Test CLI
	var outBuf, errBuf bytes.Buffer
	if rc := RunAgentsCLI(ctx, root, st, []string{"list"}, &outBuf, &errBuf); rc != 0 || strings.TrimSpace(outBuf.String()) != "claude hermes codex" {
		t.Fatalf("RunAgentsCLI list failed: rc=%d out=%q", rc, outBuf.String())
	}
}

func TestPromptsAndPromptReport(t *testing.T) {
	root, st := setupTestUmbrella(t)

	probs := Problems(root)
	if len(probs.Errors) != 0 {
		t.Fatalf("expected 0 prompt problems on seeded prompts, got %v", probs.Errors)
	}

	verPlan, err := Version(root, "plan")
	if err != nil || len(verPlan) != 12 {
		t.Fatalf("Version(plan) failed: %q, err=%v", verPlan, err)
	}

	rendered, err := Render(root, "plan", map[string]string{
		"change":   "add-oauth",
		"goal":     "Support OAuth2 login",
		"feedback": "none",
	})
	if err != nil {
		t.Fatalf("Render(plan) failed: %v", err)
	}
	if strings.Contains(rendered, "<!--") || !strings.Contains(rendered, "add-oauth") || !strings.Contains(rendered, "Support OAuth2 login") {
		t.Fatalf("unexpected rendered prompt:\n%s", rendered)
	}

	// Emit agent.exit events in SQLite store and run PromptReport
	ctx := context.Background()
	memVer := memory.Fingerprint(root)
	_ = st.EmitEvent(ctx, store.EventInput{
		TS:     time.Now().Add(-2 * time.Hour),
		Event:  "agent.exit",
		Change: "add-oauth",
		Engine: "claude",
		Phase:  "plan",
		Payload: map[string]any{
			"code":           0,
			"seconds":        45,
			"prompt_version": verPlan,
			"memory_version": memVer,
		},
	})
	_ = st.EmitEvent(ctx, store.EventInput{
		TS:     time.Now().Add(-90 * time.Minute),
		Event:  "agent.exit",
		Change: "add-oauth",
		Engine: "hermes",
		Phase:  "plan-repair-1",
		Payload: map[string]any{
			"code":           0,
			"seconds":        30,
			"prompt_version": verPlan,
			"memory_version": memVer,
		},
	})
	// Exit 75 (ENGINES_DOWN) should be ignored
	_ = st.EmitEvent(ctx, store.EventInput{
		TS:     time.Now().Add(-60 * time.Minute),
		Event:  "agent.exit",
		Change: "ignored-change",
		Engine: "claude",
		Phase:  "plan",
		Payload: map[string]any{
			"code":           75,
			"seconds":        5,
			"prompt_version": verPlan,
			"memory_version": memVer,
		},
	})

	rep, err := PromptReport(ctx, root, st, time.Now().Add(-24*time.Hour), "prompt", func(chg string) string {
		if chg == "add-oauth" {
			return "integrated"
		}
		return "pending or abandoned"
	})
	if err != nil {
		t.Fatalf("PromptReport failed: %v", err)
	}
	if len(rep.Roles) != 2 || len(rep.Roles[0].Versions) != 1 {
		t.Fatalf("unexpected PromptReport roles: %+v", rep.Roles)
	}
	vRow := rep.Roles[0].Versions[0]
	if vRow.Version != verPlan || !vRow.Current || vRow.Changes != 1 || vRow.Calls != 2 || vRow.Repairs != 1 || vRow.Finished != 1 || vRow.FinishedWithoutRepair != 0 {
		t.Fatalf("unexpected PromptVersionReport: %+v", vRow)
	}

	var outBuf, errBuf bytes.Buffer
	if rc := RunPromptReportCLI(ctx, root, st, []string{"--since", "24h", "--json"}, &outBuf, &errBuf); rc != 0 {
		t.Fatalf("RunPromptReportCLI failed: rc=%d err=%q", rc, errBuf.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(outBuf.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON from RunPromptReportCLI: %v", err)
	}
}

func TestKitConfigShowCheckAndSetAST(t *testing.T) {
	root, _ := setupTestUmbrella(t)

	// 1. Show
	view, err := Show(root, "")
	if err != nil {
		t.Fatalf("Show failed: %v", err)
	}
	for _, sec := range []string{"pipeline", "agents", "cycle", "daemon", "visual_qa", "exploration", "evaluator", "repos", "prompts", "env"} {
		if _, ok := view[sec]; !ok {
			t.Fatalf("expected section %q in Show view", sec)
		}
	}

	// 2. Check
	files := Check(root)
	for name, r := range files {
		if len(r.Errors) > 0 {
			t.Fatalf("expected 0 errors in %s on clean seeded umbrella, got: %v", name, r.Errors)
		}
	}

	// 3. Set using yaml.Node AST while preserving comments and indentation
	cyclePath := filepath.Join(root, ".foreman-kit", "cycle.yaml")
	beforeBytes, err := os.ReadFile(cyclePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(beforeBytes), "# ==============================================================================") {
		t.Fatalf("expected header comments in .foreman-kit/cycle.yaml")
	}

	var setOut bytes.Buffer
	if err := Set(root, "cycle.proposals.approved_by", "human", false, &setOut); err != nil {
		t.Fatalf("Set cycle.proposals.approved_by=human failed: %v", err)
	}
	if !strings.Contains(setOut.String(), "changed .foreman-kit/cycle.yaml: proposals.approved_by evaluator -> human") {
		t.Fatalf("unexpected Set output:\n%s", setOut.String())
	}

	afterBytes, err := os.ReadFile(cyclePath)
	if err != nil {
		t.Fatal(err)
	}
	// Verify comments are preserved intact
	if !strings.Contains(string(afterBytes), "# ==============================================================================") {
		t.Fatalf("comments were lost after Set:\n%s", string(afterBytes))
	}
	if !strings.Contains(string(afterBytes), "approved_by: human") {
		t.Fatalf("expected approved_by: human in .foreman-kit/cycle.yaml:\n%s", string(afterBytes))
	}

	// Set agents.roles.cycle.agents to [hermes, claude]
	setOut.Reset()
	if err := Set(root, "agents.roles.cycle.agents", "hermes,claude", false, &setOut); err != nil {
		t.Fatalf("Set agents.roles.cycle.agents failed: %v", err)
	}
	engs, err := Engines(root, "cycle")
	if err != nil || strings.Join(engs, ",") != "hermes,claude" {
		t.Fatalf("expected cycle engines hermes,claude after Set, got %v (err=%v)", engs, err)
	}

	// Reject agent not listed in .foreman-kit/agents.yaml
	if err := Set(root, "agents.roles.cycle.agents", "pi", false, &setOut); err == nil {
		t.Fatalf("expected Set to reject agent 'pi' not listed in agents.agents")
	}
}
