package cycle

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

var expectedKitSkills = []string{
	"foreman-kit-config",
	"foreman-kit-cycle",
	"foreman-kit-init",
	"foreman-kit-qa",
	"foreman-kit-status",
	"foreman-kit-visual-qa",
}

func assertInstalledSkills(t *testing.T, appDir, skillsRel string) {
	t.Helper()
	for _, sk := range expectedKitSkills {
		p := filepath.Join(appDir, filepath.FromSlash(skillsRel), sk, "SKILL.md")
		if !fileExists(p) {
			t.Fatalf("expected skill %s at %s, but missing", sk, p)
		}
	}
}

func assertNoSkillsDir(t *testing.T, appDir, skillsRel string) {
	t.Helper()
	for _, sk := range expectedKitSkills {
		p := filepath.Join(appDir, filepath.FromSlash(skillsRel), sk, "SKILL.md")
		if fileExists(p) {
			t.Fatalf("expected skill %s NOT to exist at %s", sk, p)
		}
	}
}

// TestStressInstallScenarios_I1_to_I35 exhaustively covers install.sh scenarios I1-I35:
// - 1, 2, and multiple agents across all 13 supported agents
// - Role assignment (plan, build, cycle), fallback lists, and the agents given to one run
// - Rejection of unknown agents, empty agents, and roles outside selection
// - Upgrade changing agents on a kit-agents-... branch, pruning dropped agent skills
// - Drift detection in Verify (agents.yaml vs lock, modified skill files, missing files)
// - Preservation of user-modified seed files and writing <file>.new on template change
// - Rotation and random order of strategies in exploration.yaml skipping browser strategies in headless mode
// - Automatic stack detection in SetupRepo / SetupAddRepo (Ruby/Rails, Go web/CLI, Node/TS, Python, Rust)
// - Updating .gitignore without log/.keep
func TestStressInstallScenarios_I1_to_I35(t *testing.T) {
	ctx := context.Background()

	// =========================================================================
	// I1: Two agents (hermes,opencode), explicit roles, seeds, .gitignore without log/.keep
	// =========================================================================
	t.Run("I1_TwoAgentsAndSeedsAndGitignore", func(t *testing.T) {
		twoDir := t.TempDir()
		var out bytes.Buffer
		err := Init(ctx, twoDir, KitOptions{
			Agents: "hermes,opencode",
			Roles: map[string]string{
				"plan":  "hermes",
				"build": "opencode",
				"cycle": "hermes",
			},
			Version: "4.0.0-go",
		}, &out)
		if err != nil {
			t.Fatalf("I1 Init failed: %v", err)
		}

		assertInstalledSkills(t, twoDir, ".agents/skills")
		assertInstalledSkills(t, twoDir, ".hermes/skills")
		assertInstalledSkills(t, twoDir, ".opencode/skills")
		assertNoSkillsDir(t, twoDir, ".claude/skills")
		assertNoSkillsDir(t, twoDir, ".github/skills")

		if exists(filepath.Join(twoDir, "bin")) || exists(filepath.Join(twoDir, "INSTALL.md")) ||
			exists(filepath.Join(twoDir, "SCHEDULING.md")) || exists(filepath.Join(twoDir, ".foreman-kit/templates")) {
			t.Fatalf("I1: release-only files should not be copied into installed application")
		}

		cfg := LoadAgentsConfig(twoDir)
		if !equalStringSlices(cfg.Agents, []string{"hermes", "opencode"}) {
			t.Fatalf("I1: unexpected agents: %v", cfg.Agents)
		}
		if !equalStringSlices(cfg.Roles["plan"], []string{"hermes"}) ||
			!equalStringSlices(cfg.Roles["build"], []string{"opencode"}) ||
			!equalStringSlices(cfg.Roles["cycle"], []string{"hermes"}) {
			t.Fatalf("I1: unexpected roles: %+v", cfg.Roles)
		}
		if cfg.Permissions != "full" || cfg.Order != "in_order" {
			t.Fatalf("I1: expected permissions=full, selection=in_order; got permissions=%s selection=%s", cfg.Permissions, cfg.Order)
		}

		lock, err := ReadKitLock(twoDir)
		if err != nil {
			t.Fatalf("I1: ReadKitLock failed: %v", err)
		}
		if !equalStringSlices(lock.Agents, []string{"hermes", "opencode"}) {
			t.Fatalf("I1: lock agents mismatch: %v", lock.Agents)
		}
		if lock.Files[".foreman-kit/agents.yaml"].Class != "seed" {
			t.Fatalf("I1: expected .foreman-kit/agents.yaml to be class=seed in lock")
		}

		// Verify seeds exist and log/.keep does NOT exist
		for _, seed := range []string{
			".foreman-kit/pipeline.yaml",
			".foreman-kit/exploration.yaml",
			".foreman-kit/cycle.yaml",
			".foreman-kit/evaluator.yaml",
			".foreman-kit/agent_proposals.json",
			".foreman-kit/agent_history.json",
			".foreman-kit/agent_backlog.json",
			".foreman-kit/prompts/plan.md",
			".foreman-kit/prompts/build.md",
			".foreman-kit/prompts/cycle.md",
			"ROADMAP.md",
			"README.md",
			"AGENTS.md",
			"CLAUDE.md",
			".gitignore",
		} {
			if !fileExists(filepath.Join(twoDir, filepath.FromSlash(seed))) {
				t.Fatalf("I1: missing seed %s", seed)
			}
		}
		// The memory is no seed: agent-memory created it, and the kit's conventions are in it.
		if !memory.Present(twoDir) || exists(filepath.Join(twoDir, memory.LegacyFile)) {
			t.Fatalf("I1: init must leave %s/ made by agent-memory and no %s", memory.Dir, memory.LegacyFile)
		}
		if conv, _ := os.ReadFile(filepath.Join(twoDir, filepath.FromSlash(memory.ConventionsFile))); !strings.Contains(string(conv), "**Topology**") {
			t.Fatalf("I1: %s lacks the kit's conventions:\n%s", memory.ConventionsFile, conv)
		}
		if exists(filepath.Join(twoDir, "log", ".keep")) {
			t.Fatalf("I1: log/.keep must not be created")
		}
		if _, hasKeep := lock.Files["log/.keep"]; hasKeep {
			t.Fatalf("I1: log/.keep must not be recorded in .foreman-kit/kit.lock")
		}

		giBytes, _ := os.ReadFile(filepath.Join(twoDir, ".gitignore"))
		giText := string(giBytes)
		if !strings.Contains(giText, "/log/*") || !strings.Contains(giText, "/.worktrees/") || !strings.Contains(giText, "*.new") {
			t.Fatalf("I1: .gitignore missing required patterns:\n%s", giText)
		}
		if strings.Contains(giText, "log/.keep") {
			t.Fatalf("I1: .gitignore must not reference log/.keep:\n%s", giText)
		}

		claudeBytes, _ := os.ReadFile(filepath.Join(twoDir, "CLAUDE.md"))
		if strings.TrimSpace(string(claudeBytes)) != "@AGENTS.md" {
			t.Fatalf("I1: expected CLAUDE.md to be @AGENTS.md, got %q", string(claudeBytes))
		}

		var verOut bytes.Buffer
		ok, err := Verify(twoDir, &verOut)
		if err != nil || !ok {
			t.Fatalf("I1: Verify failed: ok=%v err=%v out=%s", ok, err, verOut.String())
		}

		// Calling Init again on an already-initialized app must fail
		if err := Init(ctx, twoDir, KitOptions{Agents: "claude"}, io.Discard); err == nil || !strings.Contains(err.Error(), "already has .foreman-kit/kit.lock") {
			t.Fatalf("I1: expected second Init to fail with already has .foreman-kit/kit.lock, got %v", err)
		}
	})

	// =========================================================================
	// I2, I17, I19, I21, I22, I23, I34: All 13 supported agents (1, 2, 4, and all 13)
	// =========================================================================
	t.Run("I2_I17_I19_I21_I22_I23_I34_All13AgentsAndRoleLists", func(t *testing.T) {
		if len(SupportedAgents) != 13 || len(SupportedAgentOrder) != 13 {
			t.Fatalf("expected 13 supported agents, got %d", len(SupportedAgents))
		}

		// Single agent installs for every supported agent: assigns all 3 roles to that agent
		for _, agent := range SupportedAgentOrder {
			dir := t.TempDir()
			if err := Init(ctx, dir, KitOptions{Agents: agent}, io.Discard); err != nil {
				t.Fatalf("single-agent Init(%s) failed: %v", agent, err)
			}
			cfg := LoadAgentsConfig(dir)
			for _, role := range []string{"plan", "build", "cycle"} {
				if !equalStringSlices(cfg.Roles[role], []string{agent}) {
					t.Fatalf("single-agent %s: expected role %s=[%s], got %v", agent, role, agent, cfg.Roles[role])
				}
			}
			assertInstalledSkills(t, dir, ".agents/skills")
			assertInstalledSkills(t, dir, SupportedAgents[agent].Skills)
			ok, err := Verify(dir, io.Discard)
			if err != nil || !ok {
				t.Fatalf("single-agent %s: Verify failed: ok=%v err=%v", agent, ok, err)
			}
		}

		// Multi-agent combinations with fallback role lists and order=random / spread
		combos := []struct {
			name   string
			agents string
			roles  map[string]string
			order  string
		}{
			{
				name:   "lists_claude_hermes",
				agents: "claude,hermes",
				roles:  map[string]string{"plan": "claude", "build": "claude,hermes", "cycle": "hermes"},
				order:  "in_order",
			},
			{
				name:   "kilo4_random",
				agents: "claude,opencode,hermes,kilo",
				roles:  map[string]string{"plan": "claude,opencode,hermes,kilo", "build": "claude,opencode,hermes,kilo", "cycle": "claude,opencode,hermes,kilo"},
				order:  "random",
			},
			{
				name:   "cline_pair",
				agents: "claude,cline",
				roles:  map[string]string{"plan": "cline", "build": "claude,cline", "cycle": "claude"},
				order:  "in_order",
			},
			{
				name:   "pi_omp_pair",
				agents: "pi,omp",
				roles:  map[string]string{"plan": "pi", "build": "pi,omp", "cycle": "omp"},
				order:  "in_order",
			},
			{
				name:   "crush_pair",
				agents: "crush,claude",
				roles:  map[string]string{"plan": "crush", "build": "crush,claude", "cycle": "claude"},
				order:  "in_order",
			},
			{
				name:   "cursor_zcode_triple",
				agents: "cursor-agent,zcode,claude",
				roles:  map[string]string{"plan": "cursor-agent", "build": "cursor-agent,zcode", "cycle": "zcode,claude"},
				order:  "least_busy",
			},
			{
				name:   "all_13_agents",
				agents: strings.Join(SupportedAgentOrder, ","),
				roles: map[string]string{
					"plan":  "claude,codex,copilot,agy",
					"build": "hermes,opencode,kilo,cline,pi,omp",
					"cycle": "crush,cursor-agent,zcode,claude",
				},
				order: "least_busy",
			},
		}

		for _, tc := range combos {
			dir := t.TempDir()
			err := Init(ctx, dir, KitOptions{
				Agents:    tc.agents,
				Roles:     tc.roles,
				Selection: tc.order,
			}, io.Discard)
			if err != nil {
				t.Fatalf("%s: Init failed: %v", tc.name, err)
			}
			cfg := LoadAgentsConfig(dir)
			if cfg.Order != tc.order {
				t.Fatalf("%s: expected order=%s, got %s", tc.name, tc.order, cfg.Order)
			}
			for _, role := range []string{"plan", "build", "cycle"} {
				want := strings.Split(tc.roles[role], ",")
				got := RoleEngines(dir, role)
				if !equalStringSlices(got, want) {
					t.Fatalf("%s: role %s got %v, want %v", tc.name, role, got, want)
				}
			}
			for _, ag := range strings.Split(tc.agents, ",") {
				assertInstalledSkills(t, dir, SupportedAgents[ag].Skills)
			}
			ok, err := Verify(dir, io.Discard)
			if err != nil || !ok {
				t.Fatalf("%s: Verify failed: ok=%v err=%v", tc.name, ok, err)
			}
		}

		// I5: the agents of a role given to one run, as --set gives them
		dir := t.TempDir()
		_ = Init(ctx, dir, KitOptions{
			Agents: "hermes,opencode",
			Roles:  map[string]string{"plan": "hermes", "build": "opencode", "cycle": "hermes"},
		}, io.Discard)
		settingstest.Give(t, settings.RoleAgents("cycle"), "[claude, codex]")
		if got := RoleEngines(dir, "cycle"); !equalStringSlices(got, []string{"claude", "codex"}) {
			t.Fatalf("expected the agents the run was given for cycle, got %v", got)
		}
		settingstest.Take(t, settings.RoleAgents("cycle"))
		if got := RoleEngines(dir, "cycle"); !equalStringSlices(got, []string{"hermes"}) {
			t.Fatalf("expected the agents of the file again, got %v", got)
		}
	})

	// =========================================================================
	// I3: Invalid agent selections install nothing
	// =========================================================================
	t.Run("I3_InvalidSelectionsRefused", func(t *testing.T) {
		baseTmp := t.TempDir()

		// Unknown agent refused
		bad1 := filepath.Join(baseTmp, "bad-unknown")
		if err := Init(ctx, bad1, KitOptions{Agents: "claude,gemini"}, io.Discard); err == nil || !strings.Contains(err.Error(), "unknown agent gemini") {
			t.Fatalf("expected unknown agent gemini error, got %v", err)
		}
		if exists(bad1) {
			t.Fatalf("expected %s not to be created on invalid selection", bad1)
		}

		// Role outside selection refused
		bad2 := filepath.Join(baseTmp, "bad-role")
		err := Init(ctx, bad2, KitOptions{
			Agents: "claude,codex",
			Roles:  map[string]string{"plan": "hermes", "build": "claude", "cycle": "claude"},
		}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "--plan hermes is not one of the agents") {
			t.Fatalf("expected role outside selection error, got %v", err)
		}
		if exists(bad2) {
			t.Fatalf("expected %s not to be created on invalid role", bad2)
		}

		// No --agents refused
		bad3 := filepath.Join(baseTmp, "bad-empty")
		if err := Init(ctx, bad3, KitOptions{}, io.Discard); err == nil || !strings.Contains(err.Error(), "pass --agents") {
			t.Fatalf("expected missing --agents error, got %v", err)
		}
		if exists(bad3) {
			t.Fatalf("expected %s not to be created when --agents is empty", bad3)
		}

		// Unknown selection refused
		bad4 := filepath.Join(baseTmp, "bad-order")
		if err := Init(ctx, bad4, KitOptions{Agents: "claude", Selection: "sideways"}, io.Discard); err == nil || !strings.Contains(err.Error(), "unknown selection sideways") {
			t.Fatalf("expected unknown selection error, got %v", err)
		}
	})

	// =========================================================================
	// I8 & I10: Upgrade changes agents on branch kit-agents-..., removes dropped skills,
	// preserves permissions & the args of a role, and Verify catches drift
	// =========================================================================
	t.Run("I8_I10_I27_UpgradeChangesAgentsAndVerifyDrift", func(t *testing.T) {
		appDir := t.TempDir()
		if err := Init(ctx, appDir, KitOptions{
			Agents:  "hermes,opencode",
			Roles:   map[string]string{"plan": "hermes", "build": "opencode", "cycle": "hermes"},
			Version: "4.0.0-go",
		}, io.Discard); err != nil {
			t.Fatalf("Init failed: %v", err)
		}

		// Set permissions: restricted and the args of the plan role in .foreman-kit/agents.yaml (I7, I27)
		agPath := filepath.Join(appDir, AgentsConfig)
		agBytes, _ := os.ReadFile(agPath)
		agMod := strings.Replace(string(agBytes), "permissions: full", "permissions: restricted", 1)
		agMod = strings.Replace(agMod, "    args: {} # per agent", "    args: {claude: \"--model x-model\"} # per agent", 1)
		if strings.Count(agMod, "permissions: restricted") != 1 || strings.Count(agMod, "x-model") != 1 {
			t.Fatalf(".foreman-kit/agents.yaml does not read as the test expects:\n%s", agBytes)
		}
		_ = os.WriteFile(agPath, []byte(agMod), 0o644)

		initGitRepo(t, appDir)
		commitAll(t, appDir, "init app")

		// Same release and selection: nothing to do
		var upOut bytes.Buffer
		if err := Upgrade(ctx, appDir, KitOptions{Version: "4.0.0-go"}, &upOut); err != nil {
			t.Fatalf("Upgrade no-op failed: %v", err)
		}
		if !strings.Contains(upOut.String(), "nothing to do") {
			t.Fatalf("expected 'nothing to do', got: %s", upOut.String())
		}

		// Upgrade refuses --selection flag (I19)
		if err := Upgrade(ctx, appDir, KitOptions{Selection: "random"}, io.Discard); err == nil || !strings.Contains(err.Error(), "upgrade takes no --selection") {
			t.Fatalf("expected upgrade --selection to fail, got %v", err)
		}

		// Upgrade changing agents to claude,opencode
		upOut.Reset()
		err := Upgrade(ctx, appDir, KitOptions{
			Agents:  "claude,opencode",
			Roles:   map[string]string{"plan": "claude", "cycle": "claude"},
			Version: "4.0.0-go",
		}, &upOut)
		if err != nil {
			t.Fatalf("Upgrade changing agents failed: %v", err)
		}

		// Dropped agent (.hermes) skills removed & directory pruned; new agent (.claude) skills installed
		assertNoSkillsDir(t, appDir, ".hermes/skills")
		if exists(filepath.Join(appDir, ".hermes")) {
			t.Fatalf("expected empty .hermes directory to be pruned")
		}
		assertInstalledSkills(t, appDir, ".claude/skills")
		assertInstalledSkills(t, appDir, ".opencode/skills")

		// Kept role (build: opencode) stays; dropped agent's roles replaced with claude
		cfg := LoadAgentsConfig(appDir)
		if !equalStringSlices(cfg.Roles["plan"], []string{"claude"}) ||
			!equalStringSlices(cfg.Roles["build"], []string{"opencode"}) ||
			!equalStringSlices(cfg.Roles["cycle"], []string{"claude"}) {
			t.Fatalf("unexpected roles after upgrade: %+v", cfg.Roles)
		}
		if cfg.Permissions != "restricted" {
			t.Fatalf("expected permissions: restricted to survive upgrade, got %s", cfg.Permissions)
		}
		if !equalStringSlices(cfg.ExtraArgs["claude"], []string{"--model", "x-model"}) {
			t.Fatalf("expected the args of claude to survive upgrade, got %v", cfg.ExtraArgs)
		}

		// Committed on branch kit-agents-claude-opencode with clean tree
		brOut, _ := gitx.RunGit(ctx, appDir, "branch", "--show-current")
		if strings.TrimSpace(brOut) != "kit-agents-claude-opencode" {
			t.Fatalf("expected branch kit-agents-claude-opencode, got %q", strings.TrimSpace(brOut))
		}
		stOut, _ := gitx.RunGit(ctx, appDir, "status", "--porcelain")
		if strings.TrimSpace(stOut) != "" {
			t.Fatalf("expected clean git status after upgrade, got:\n%s", stOut)
		}

		// Verify passes after upgrade
		ok, err := Verify(appDir, io.Discard)
		if err != nil || !ok {
			t.Fatalf("Verify failed after upgrade: ok=%v err=%v", ok, err)
		}

		// I10: Verify catches drift when .foreman-kit/agents.yaml is edited by hand
		agBytes, _ = os.ReadFile(agPath)
		_ = os.WriteFile(agPath, []byte(strings.Replace(string(agBytes), "agents: [claude, opencode]", "agents: [claude, opencode, codex]", 1)), 0o644)
		var driftOut bytes.Buffer
		ok, _ = Verify(appDir, &driftOut)
		if ok || !strings.Contains(driftOut.String(), "DRIFT") || !strings.Contains(driftOut.String(), ".foreman-kit/agents.yaml") {
			t.Fatalf("expected Verify to catch agents.yaml drift, got ok=%v out=%s", ok, driftOut.String())
		}
		_, _ = gitx.RunGit(ctx, appDir, "checkout", "-q", "--", ".foreman-kit/agents.yaml")

		// I10: Verify catches drift when a kit skill copy is edited
		skillFile := filepath.Join(appDir, ".claude/skills/foreman-kit-cycle/SKILL.md")
		origSkill, _ := os.ReadFile(skillFile)
		_ = os.WriteFile(skillFile, append(origSkill, []byte("\n# edited by hand\n")...), 0o644)
		driftOut.Reset()
		ok, _ = Verify(appDir, &driftOut)
		if ok || !strings.Contains(driftOut.String(), "DRIFT") || !strings.Contains(driftOut.String(), ".claude/skills/foreman-kit-cycle/SKILL.md (modified)") {
			t.Fatalf("expected Verify to catch skill modification drift, got ok=%v out=%s", ok, driftOut.String())
		}
	})

	// =========================================================================
	// I15 & I28: Existing .gitignore preservation, user-edited seeds preserved,
	// changed seed templates written as <file>.new and untracked from git
	// =========================================================================
	t.Run("I15_I28_ExistingGitignoreAndSeedDotNewOnUpgrade", func(t *testing.T) {
		// I15: Existing .gitignore keeps its lines and gets only what it lacks
		legacyDir := t.TempDir()
		_ = os.WriteFile(filepath.Join(legacyDir, ".gitignore"), []byte("node_modules/\nlog/\n"), 0o644)
		if err := Init(ctx, legacyDir, KitOptions{Agents: "claude"}, io.Discard); err != nil {
			t.Fatalf("legacy Init failed: %v", err)
		}
		giBytes, _ := os.ReadFile(filepath.Join(legacyDir, ".gitignore"))
		giLines := strings.Split(strings.TrimSpace(string(giBytes)), "\n")
		hasNodeModules, hasWorktrees, hasDotNew := false, false, false
		logRuleCount := 0
		for _, l := range giLines {
			l = strings.TrimSpace(l)
			if l == "node_modules/" {
				hasNodeModules = true
			}
			if l == "/.worktrees/" {
				hasWorktrees = true
			}
			if l == "*.new" {
				hasDotNew = true
			}
			if strings.HasPrefix(l, "log") || strings.HasPrefix(l, "/log") || strings.HasPrefix(l, "!/log") {
				logRuleCount++
			}
		}
		if !hasNodeModules || !hasWorktrees || !hasDotNew || logRuleCount != 1 {
			t.Fatalf("I15: unexpected legacy .gitignore (logRuleCount=%d):\n%s", logRuleCount, string(giBytes))
		}

		// I28: Upgrade proposes a changed seed as <file>.new, preserves user-modified seed, untracks old.new
		newApp := t.TempDir()
		if err := Init(ctx, newApp, KitOptions{Agents: "claude", Version: "4.0.0-go"}, io.Discard); err != nil {
			t.Fatalf("newApp Init failed: %v", err)
		}
		// User customizes README.md and removes *.new from .gitignore, then commits old.new
		userReadme := "# My Custom Application README\nUser customized content.\n"
		_ = os.WriteFile(filepath.Join(newApp, "README.md"), []byte(userReadme), 0o644)
		giRaw, _ := os.ReadFile(filepath.Join(newApp, ".gitignore"))
		_ = os.WriteFile(filepath.Join(newApp, ".gitignore"), []byte(strings.ReplaceAll(string(giRaw), "*.new\n", "")), 0o644)
		_ = os.WriteFile(filepath.Join(newApp, "old.new"), []byte("an old proposal\n"), 0o644)

		initGitRepo(t, newApp)
		commitAll(t, newApp, "initial commit with old.new")

		newReadmeTpl := []byte("# Template v99.0.0\nA line the next release adds.\n")
		var upOut bytes.Buffer
		err := Upgrade(ctx, newApp, KitOptions{
			Version: "99.0.0",
			SeedOverrides: map[string][]byte{
				"README.md": newReadmeTpl,
			},
		}, &upOut)
		if err != nil {
			t.Fatalf("I28 Upgrade failed: %v", err)
		}

		// User's README.md is preserved untouched
		gotReadme, _ := os.ReadFile(filepath.Join(newApp, "README.md"))
		if string(gotReadme) != userReadme {
			t.Fatalf("I28: user's README.md was overwritten! got:\n%s", string(gotReadme))
		}
		// Changed template written as README.md.new
		gotReadmeNew, err := os.ReadFile(filepath.Join(newApp, "README.md.new"))
		if err != nil || string(gotReadmeNew) != string(newReadmeTpl) {
			t.Fatalf("I28: expected README.md.new with new template, err=%v got=%q", err, string(gotReadmeNew))
		}
		// Branch is kit-upgrade-v99.0.0, no *.new files tracked in git, old.new still on disk, git status clean
		brOut, _ := gitx.RunGit(ctx, newApp, "branch", "--show-current")
		if strings.TrimSpace(brOut) != "kit-upgrade-v99.0.0" {
			t.Fatalf("I28: expected branch kit-upgrade-v99.0.0, got %q", strings.TrimSpace(brOut))
		}
		lsNew, _ := gitx.RunGit(ctx, newApp, "ls-files", "--", "*.new")
		if strings.TrimSpace(lsNew) != "" {
			t.Fatalf("I28: expected no *.new tracked in git, got %q", lsNew)
		}
		if !fileExists(filepath.Join(newApp, "old.new")) {
			t.Fatalf("I28: expected old.new to remain on disk")
		}
		stPorcelain, _ := gitx.RunGit(ctx, newApp, "status", "--porcelain")
		if strings.TrimSpace(stPorcelain) != "" {
			t.Fatalf("I28: expected clean git status (with *.new ignored), got:\n%s", stPorcelain)
		}
	})

	// =========================================================================
	// I6: Exploration strategies rotation and random mode, skipping browser strategies in headless mode
	// =========================================================================
	t.Run("I6_ExplorationStrategiesRotationAndRandomHeadlessFilter", func(t *testing.T) {
		appDir := t.TempDir()
		if err := Init(ctx, appDir, KitOptions{Agents: "claude"}, io.Discard); err != nil {
			t.Fatalf("Init failed: %v", err)
		}

		browserSlugs := map[string]bool{
			"missing-capabilities":           true,
			"errors-and-logs":                true,
			"user-flows-and-copy":            true,
			"accessibility-and-localization": true,
		}

		// 1. Rotation mode across 8 cycles
		var pickedRotation []string
		for i := 1; i <= 8; i++ {
			outFile := filepath.Join(appDir, "log", "cycle", "p.md")
			args := []string{outFile}
			if i == 2 {
				args = append(args, "--dispatch-running", "4242")
			}
			var out, errBuf bytes.Buffer
			if rc := RunCyclePromptCLI(ctx, appDir, args, &out, &errBuf); rc != 0 {
				t.Fatalf("RunCyclePromptCLI #%d failed: %s", i, errBuf.String())
			}
			bodyBytes, _ := os.ReadFile(outFile)
			body := string(bodyBytes)
			for bSlug := range browserSlugs {
				if strings.Contains(body, "(`"+bSlug+"`") {
					t.Fatalf("headless rotation #%d picked browser strategy %s", i, bSlug)
				}
			}
			if i == 1 {
				if !strings.Contains(body, "(`security-and-access-control`, kind `defects`)") {
					t.Fatalf("cycle 1 expected security-and-access-control, got:\n%s", body)
				}
				if !strings.Contains(body, "still has `{{...}}` placeholders") {
					t.Fatalf("cycle 1 expected roadmap placeholder notice")
				}
				if !strings.Contains(body, "approval `evaluator`") {
					t.Fatalf("cycle 1 expected approval `evaluator`")
				}
				if !strings.Contains(body, "foreman-kit visual-qa request") {
					t.Fatalf("cycle 1 expected visual-qa request instruction")
				}
			}
			if i == 2 {
				if !strings.Contains(body, "(`specs-versus-code`, kind `defects`)") {
					t.Fatalf("cycle 2 expected specs-versus-code")
				}
				if !strings.Contains(body, "a dispatch is running (pid 4242)") {
					t.Fatalf("cycle 2 expected dispatch running pid 4242")
				}
			}
			m := regexp.MustCompile(`^strategy: ([a-z0-9-]+)`).FindStringSubmatch(strings.TrimSpace(out.String()))
			if len(m) == 2 {
				pickedRotation = append(pickedRotation, m[1])
			}
		}
		if len(pickedRotation) != 8 {
			t.Fatalf("expected 8 strategies picked in rotation, got %v", pickedRotation)
		}

		// 2. Random mode in .foreman-kit/exploration.yaml
		expPath := filepath.Join(appDir, ExplorationFile)
		expBytes, _ := os.ReadFile(expPath)
		_ = os.WriteFile(expPath, []byte(strings.Replace(string(expBytes), "\nselection: in_order\n", "\nselection: random\n", 1)), 0o644)
		if got := LoadExplorationSelection(appDir); got != "random" {
			t.Fatalf("expected LoadExplorationSelection=random, got %s", got)
		}

		headless := SessionStrategies(LoadExplorationStrategies(appDir), settings.SessionCycle)
		stateFile := filepath.Join(appDir, "log", "cycle", "random_state.json")
		seenRandom := make(map[string]bool)
		for i := 0; i < 30; i++ {
			st, err := PickExplorationStrategy(headless, "random", "", stateFile)
			if err != nil || st == nil {
				t.Fatalf("PickExplorationStrategy random failed: %v", err)
			}
			if browserSlugs[st.Slug] {
				t.Fatalf("random mode picked browser strategy %s in headless mode", st.Slug)
			}
			seenRandom[st.Slug] = true
		}
		if len(seenRandom) < 3 {
			t.Fatalf("expected random strategy selection to vary across >=3 strategies, got %v", seenRandom)
		}
	})

	// =========================================================================
	// I11 & I16: Automatic stack detection in SetupRepo / SetupAddRepo for
	// Node/TS (Next.js), Go (Gin web & Cobra CLI), Ruby/Rails, Python (FastAPI/Django), and Rust (Axum)
	// =========================================================================
	t.Run("I11_I16_SetupRepoStackDetectionAllLanguages", func(t *testing.T) {
		appDir := t.TempDir()
		if err := Init(ctx, appDir, KitOptions{Agents: "claude"}, io.Discard); err != nil {
			t.Fatalf("Init failed: %v", err)
		}
		initGitRepo(t, appDir)
		commitAll(t, appDir, "init umbrella")

		workTmp := t.TempDir()
		createBareRemote := func(name, branch string, files map[string]string) string {
			src := filepath.Join(workTmp, name)
			_ = os.MkdirAll(src, 0o755)
			for _, args := range [][]string{
				{"init", "-b", branch},
				{"config", "user.name", "Bot"},
				{"config", "user.email", "bot@example.com"},
			} {
				_, _ = gitx.RunGit(ctx, src, args...)
			}
			for rel, content := range files {
				full := filepath.Join(src, filepath.FromSlash(rel))
				_ = os.MkdirAll(filepath.Dir(full), 0o755)
				_ = os.WriteFile(full, []byte(content), 0o644)
			}
			commitAll(t, src, "init "+name)
			bare := filepath.Join(workTmp, name+".git")
			if _, err := gitx.RunGit(ctx, workTmp, "clone", "--bare", src, bare); err != nil {
				t.Fatalf("clone bare %s failed: %v", name, err)
			}
			return bare
		}

		// 1. Node/TypeScript Next.js repo on branch trunk
		webBare := createBareRemote("web", "trunk", map[string]string{
			".nvmrc":         "22\n",
			"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
			"tsconfig.json":  "{}\n",
			"package.json":   `{"scripts":{"dev":"next dev","lint":"next lint","test":"vitest run","build":"next build"},"dependencies":{"next":"15"}}`,
		})
		if err := SetupAddRepo(ctx, appDir, webBare, "web", "", io.Discard); err != nil {
			t.Fatalf("SetupAddRepo web failed: %v", err)
		}

		// 2. Go Gin web server
		gowebBare := createBareRemote("goweb", "main", map[string]string{
			".go-version":        "1.27.2\n",
			"go.mod":             "module example.com/shop\n\ngo 1.27\n\nrequire github.com/gin-gonic/gin v1.10.0\n",
			"cmd/server/main.go": "package main\nfunc main() { r := gin.Default(); r.GET(\"/healthz\", nil); r.Run(\":9090\") }\n",
		})
		if err := SetupAddRepo(ctx, appDir, gowebBare, "goweb", "", io.Discard); err != nil {
			t.Fatalf("SetupAddRepo goweb failed: %v", err)
		}

		// 3. Go Cobra CLI
		gocliBare := createBareRemote("gocli", "main", map[string]string{
			".go-version":        "1.27.2\n",
			"go.mod":             "module example.com/mytool\n\ngo 1.27\n\nrequire github.com/spf13/cobra v1.8.0\n",
			"cmd/mytool/main.go": "package main\nfunc main() {}\n",
		})
		if err := SetupAddRepo(ctx, appDir, gocliBare, "gocli", "", io.Discard); err != nil {
			t.Fatalf("SetupAddRepo gocli failed: %v", err)
		}

		// 4. Ruby on Rails repo
		railsBare := createBareRemote("railsapp", "main", map[string]string{
			".ruby-version":         "3.4.1\n",
			"Gemfile":               "source 'https://rubygems.org'\ngem 'rails'\ngem 'rubocop'\ngem 'sorbet'\ngem 'rspec'\n",
			"config/application.rb": "module RailsApp; end\n",
			"config/routes.rb":      "Rails.application.routes.draw { get 'up' => 'rails/health#show' }\n",
			"spec/spec_helper.rb":   "# rspec\n",
			"bin/dev":               "#!/bin/sh\n",
		})
		if err := SetupAddRepo(ctx, appDir, railsBare, "railsapp", "", io.Discard); err != nil {
			t.Fatalf("SetupAddRepo railsapp failed: %v", err)
		}

		// 5. Python FastAPI repo with uv.lock
		pyBare := createBareRemote("pyapi", "main", map[string]string{
			".python-version": "3.13\n",
			"uv.lock":         "version = 1\n",
			"pyproject.toml":  "[project]\ndependencies = [\"fastapi\", \"ruff\", \"mypy\", \"pytest\"]\n",
		})
		if err := SetupAddRepo(ctx, appDir, pyBare, "pyapi", "", io.Discard); err != nil {
			t.Fatalf("SetupAddRepo pyapi failed: %v", err)
		}

		// 6. Rust Axum web repo
		rustBare := createBareRemote("rustsvc", "main", map[string]string{
			"Cargo.toml":  "[package]\nname = \"rustsvc\"\n[dependencies]\naxum = \"0.7\"\n",
			"src/main.rs": "fn main() {}\n",
		})
		if err := SetupAddRepo(ctx, appDir, rustBare, "rustsvc", "", io.Discard); err != nil {
			t.Fatalf("SetupAddRepo rustsvc failed: %v", err)
		}

		readRepoCfg := func(repoName string) map[string]any {
			b, err := os.ReadFile(filepath.Join(appDir, "repos", repoName, "repo.config.yaml"))
			if err != nil {
				t.Fatalf("missing repo.config.yaml for %s: %v", repoName, err)
			}
			var m map[string]any
			if err := yaml.Unmarshal(b, &m); err != nil {
				t.Fatalf("invalid yaml in %s/repo.config.yaml: %v", repoName, err)
			}
			return m
		}

		// Verify Next.js
		webCfg := readRepoCfg("web")
		webSvc, _ := webCfg["service"].(map[string]any)
		webGates, _ := webCfg["gates"].(map[string]any)
		if webCfg["base_branch"] != "trunk" || webCfg["stack"] != "nextjs-typescript" || webCfg["kind"] != "web" ||
			webCfg["env_script"] != "script/env.sh" || webSvc["dev_command"] != "pnpm dev" ||
			webGates["test"] != "pnpm test" || webGates["typecheck"] != "pnpm exec tsc --noEmit" {
			t.Fatalf("unexpected web repo.config.yaml: %+v", webCfg)
		}
		if !fileExists(filepath.Join(appDir, "repos/web/script/env.sh")) {
			t.Fatalf("expected script/env.sh in repos/web")
		}

		// Verify Go Gin web
		gowebCfg := readRepoCfg("goweb")
		gowebSvc, _ := gowebCfg["service"].(map[string]any)
		if gowebCfg["stack"] != "go-web-gin" || gowebCfg["kind"] != "web" ||
			gowebSvc["port"] != "9090" || gowebSvc["health_endpoint"] != "http://localhost:9090/healthz" ||
			gowebSvc["dev_command"] != "go run ./cmd/server" {
			t.Fatalf("unexpected goweb repo.config.yaml: %+v", gowebCfg)
		}

		// Verify Go Cobra CLI
		gocliCfg := readRepoCfg("gocli")
		gocliSvc, _ := gocliCfg["service"].(map[string]any)
		gocliReg, _ := gocliCfg["regression"].(map[string]any)
		gocliRegCommands, _ := gocliReg["commands"].(map[string]any)
		if gocliCfg["stack"] != "go-cli-cobra" || gocliCfg["kind"] != "cli" ||
			gocliSvc["port"] != "" || gocliSvc["health_endpoint"] != "" ||
			gocliSvc["dev_command"] != "go run ./cmd/mytool" || gocliRegCommands["test"] != "go test ./..." {
			t.Fatalf("unexpected gocli repo.config.yaml: %+v", gocliCfg)
		}

		// Verify Ruby on Rails
		railsCfg := readRepoCfg("railsapp")
		railsSvc, _ := railsCfg["service"].(map[string]any)
		railsGates, _ := railsCfg["gates"].(map[string]any)
		if railsCfg["stack"] != "rails" || railsCfg["kind"] != "web" ||
			railsSvc["port"] != "3000" || railsSvc["health_endpoint"] != "http://localhost:3000/up" ||
			railsSvc["dev_command"] != "bin/dev" || railsGates["test"] != "bundle exec rspec" ||
			railsGates["lint"] != "bundle exec rubocop" || railsGates["typecheck"] != "bundle exec srb tc" ||
			railsCfg["setup"] != "RAILS_ENV=test bin/rails db:prepare" || railsGates["setup"] != nil {
			t.Fatalf("unexpected railsapp repo.config.yaml: %+v", railsCfg)
		}

		// Verify Python FastAPI
		pyCfg := readRepoCfg("pyapi")
		pyGates, _ := pyCfg["gates"].(map[string]any)
		if pyCfg["stack"] != "python-fastapi" || pyCfg["kind"] != "web" ||
			pyGates["lint"] != "uv run ruff check ." || pyGates["typecheck"] != "uv run mypy ." ||
			pyGates["test"] != "uv run pytest" {
			t.Fatalf("unexpected pyapi repo.config.yaml: %+v", pyCfg)
		}

		// Verify Rust Axum
		rustCfg := readRepoCfg("rustsvc")
		rustGates, _ := rustCfg["gates"].(map[string]any)
		if rustCfg["stack"] != "rust" || rustCfg["kind"] != "web" ||
			rustGates["lint"] != "cargo clippy -- -D warnings" || rustGates["typecheck"] != "cargo check" ||
			rustGates["test"] != "cargo test" || rustGates["build"] != "cargo build" {
			t.Fatalf("unexpected rustsvc repo.config.yaml: %+v", rustCfg)
		}

		// SetupRepo without --force keeps user customizations
		webCfgFile := filepath.Join(appDir, "repos/web/repo.config.yaml")
		origWebBytes, _ := os.ReadFile(webCfgFile)
		_ = os.WriteFile(webCfgFile, append(origWebBytes, []byte("\n# mine\n")...), 0o644)
		_ = SetupRepo(appDir, "web", false, io.Discard)
		afterWebBytes, _ := os.ReadFile(webCfgFile)
		if !strings.Contains(string(afterWebBytes), "# mine") {
			t.Fatalf("expected SetupRepo without --force to preserve existing repo.config.yaml")
		}
	})

	// =========================================================================
	// I18, I20, I29, I30: Scoped memory caps, prompt versions, prompt proposals,
	// and exact backlog archiving
	// =========================================================================
	t.Run("I18_I20_I29_I30_MemoryAndPromptProposalsAndArchiveMatching", func(t *testing.T) {
		appDir := t.TempDir()
		if err := Init(ctx, appDir, KitOptions{Agents: "claude"}, io.Discard); err != nil {
			t.Fatalf("Init failed: %v", err)
		}
		initGitRepo(t, appDir)
		st := openTestStore(t, appDir)

		// Memory caps check (I20)
		if over := memory.OverCap(appDir); len(over) != 0 {
			t.Fatalf("expected the memory of a new application within its caps, got %v", over)
		}
		_ = os.MkdirAll(filepath.Join(appDir, filepath.FromSlash(memory.ModulesDir)), 0o755)
		_ = os.WriteFile(filepath.Join(appDir, filepath.FromSlash(memory.RepoModule("web"))), []byte("# Repository web\n<!-- @id: repo-web -->\n\n- Run via foreman-kit exec\n"), 0o644)
		_ = os.WriteFile(filepath.Join(appDir, filepath.FromSlash(memory.SpecModule("auth"))), []byte("# Capability auth\n<!-- @id: spec-auth -->\n\n"+strings.Repeat("- Rotate session token after login (add-mfa)\n", 150)), 0o644)

		block := MemoryPromptBlock(appDir, []string{"auth"})
		if !strings.Contains(block, "for the capabilities this change modifies: .agent-memory/modules/spec-auth.md") ||
			!strings.Contains(block, "open the ones of the repositories you change: .agent-memory/modules/repo-web.md") {
			t.Fatalf("unexpected MemoryPromptBlock:\n%s", block)
		}
		planLine := MemoryPlanLine(appDir)
		if !strings.Contains(planLine, "Over the cap, trim first: `.agent-memory/modules/spec-auth.md`") {
			t.Fatalf("expected over-cap warning in MemoryPlanLine, got:\n%s", planLine)
		}

		// Prompt proposals (I20): valid edit, stale old text, and edit that breaks {{feedback}}
		buildVBefore, _ := PromptVersion(appDir, "build")
		_ = WriteProposals(appDir, []map[string]any{
			{
				"change_name": "prompt-build-src",
				"kind":        "prompt",
				"prompt":      "build",
				"status":      "proposed",
				"source":      "repairs",
				"edits": []any{
					map[string]any{
						"old": "- Run `foreman-kit gates` before you finish and fix every failure.",
						"new": "- Run `foreman-kit gates` before you finish and fix every failure; the gates need src/ present.",
					},
				},
			},
			{
				"change_name": "prompt-build-stale",
				"kind":        "prompt",
				"prompt":      "build",
				"status":      "proposed",
				"source":      "repairs",
				"edits": []any{
					map[string]any{
						"old": "a sentence the prompt does not have",
						"new": "anything",
					},
				},
			},
			{
				"change_name": "prompt-plan-break",
				"kind":        "prompt",
				"prompt":      "plan",
				"status":      "proposed",
				"source":      "repairs",
				"edits": []any{
					map[string]any{
						"old": "{{feedback}}",
						"new": "",
					},
				},
			},
		})

		// By default prompt proposals wait for a human
		var revOut bytes.Buffer
		_ = ReviewProposals(ctx, appDir, st, nil, &revOut)
		if !strings.Contains(revOut.String(), "3 prompt edit(s) waiting for a human") {
			t.Fatalf("expected 3 prompt edits waiting for a human, got: %s", revOut.String())
		}

		// Approve all 3 and review
		for _, n := range []string{"prompt-build-src", "prompt-build-stale", "prompt-plan-break"} {
			if err := ApproveProposal(appDir, n, io.Discard); err != nil {
				t.Fatalf("ApproveProposal(%s) failed: %v", n, err)
			}
		}
		_ = ReviewProposals(ctx, appDir, st, nil, io.Discard)
		buildVAfter, _ := PromptVersion(appDir, "build")
		if buildVAfter == buildVBefore {
			t.Fatalf("expected build prompt version to change after applying prompt-build-src")
		}
		props, _ := ReadProposals(appDir)
		propMap := make(map[string]map[string]any)
		for _, p := range props {
			propMap[asString(p["change_name"])] = p
		}
		if asString(propMap["prompt-build-src"]["status"]) != "integrated" {
			t.Fatalf("expected prompt-build-src integrated, got %+v", propMap["prompt-build-src"])
		}
		if asString(propMap["prompt-build-stale"]["status"]) != "rejected" {
			t.Fatalf("expected prompt-build-stale rejected, got %+v", propMap["prompt-build-stale"])
		}
		if asString(propMap["prompt-plan-break"]["status"]) != "rejected" {
			t.Fatalf("expected prompt-plan-break rejected, got %+v", propMap["prompt-plan-break"])
		}
		if errs, _ := CheckPrompts(appDir); len(errs) != 0 {
			t.Fatalf("expected plan.md to remain valid after rejected break edit, got %v", errs)
		}

		// I29 & I30: Exact change name matching on archive
		_ = os.MkdirAll(filepath.Join(appDir, "openspec/changes/archive/2026-10-20-add-thing"), 0o755)
		_ = WriteProposals(appDir, []map[string]any{
			{
				"change_name": "thing",
				"priority":    "low",
				"description": "Not add-thing",
				"status":      "approved",
				"source":      "exploration",
			},
		})
		_ = ReviewProposals(ctx, appDir, st, nil, io.Discard)
		props, _ = ReadProposals(appDir)
		if asString(props[0]["status"]) != "queued" {
			t.Fatalf("I30: expected 'thing' to stay queued while only 'add-thing' is archived, got %v", props[0]["status"])
		}
		bl, _ := ReadJSONList(filepath.Join(appDir, BacklogFile))
		if len(bl) != 1 || asString(bl[0]["change_name"]) != "thing" {
			t.Fatalf("I30: expected 'thing' in backlog, got %+v", bl)
		}

		// Once 2026-10-21-thing is archived, 'thing' becomes integrated and moves to agent_history.json
		_ = os.MkdirAll(filepath.Join(appDir, "openspec/changes/archive/2026-10-21-thing"), 0o755)
		_ = ReviewProposals(ctx, appDir, st, nil, io.Discard)
		props, _ = ReadProposals(appDir)
		if asString(props[0]["status"]) != "integrated" {
			t.Fatalf("I30: expected 'thing' to be integrated once archived, got %v", props[0]["status"])
		}
		bl, _ = ReadJSONList(filepath.Join(appDir, BacklogFile))
		if len(bl) != 0 {
			t.Fatalf("I30: expected empty backlog after archiving 'thing', got %+v", bl)
		}
		hist, _ := ReadJSONList(filepath.Join(appDir, HistoryFile))
		if len(hist) != 1 || asString(hist[0]["change_name"]) != "thing" || asString(hist[0]["integrated_at"]) == "" {
			t.Fatalf("I30: expected 'thing' in agent_history.json with integrated_at, got %+v", hist)
		}
	})
}

// TestStressScenarioS_FullReconciliationAndGitFailures covers Scenario S (S1-S22) of acceptance.sh:
//   - 3-way in-memory merge with go-git without data loss across the files of .agent-memory/,
//     agent_backlog.json, agent_history.json, and agent_proposals.json when origin/master advances in parallel
//   - CarryProposals from a dirty main checkout behind origin/master + ReleaseScopeEdits (S9, S10, S11)
//   - SyncRepos restoring a backlog holding a version origin published since HEAD (S12)
//   - WithRecordLock reclaiming dead PID locks and timing out behind live locks (S14)
//   - Push retry on transient pre-receive rejection (S18)
//   - Non-JSON backlog fallback to 3-way text merge (S20)
//   - SaveUnpublishedCommit under refs/openspec/unpublished/... when commit (index.lock) or push fails,
//     and stale index.lock cleanup (>10m) vs young index.lock refusal (S6, S7, S21, S22)
//   - ScopeGuard pausing the workspace in SQLite and log/PAUSE on out-of-scope edits
func TestStressScenarioS_FullReconciliationAndGitFailures(t *testing.T) {
	ctx := context.Background()
	workRoot := t.TempDir()

	originBare := filepath.Join(workRoot, "umbrella.git")
	_ = os.MkdirAll(originBare, 0o755)
	if _, err := gitx.RunGit(ctx, originBare, "init", "--bare", "-b", "master"); err != nil {
		t.Fatalf("init bare failed: %v", err)
	}

	ws := filepath.Join(workRoot, "ws")
	if err := Init(ctx, ws, KitOptions{Agents: "claude"}, io.Discard); err != nil {
		t.Fatalf("Init ws failed: %v", err)
	}
	// These scenarios pile up backlog items that are never built, to see how they are reconciled:
	// the cap on the queue is not what they test.
	raiseQueueCap(t, ws)
	initGitRepo(t, ws)
	_, _ = gitx.RunGit(ctx, ws, "remote", "add", "origin", originBare)
	st := openTestStore(t, ws)

	// Seed initial state (S setup)
	_ = WriteJSONList(filepath.Join(ws, BacklogFile), []map[string]any{
		{"change_name": "shared-item", "priority": "low", "description": "Edited by the session", "depends_on": []string{"never-built"}},
		{"change_name": "to-delete", "priority": "low", "description": "Deleted by the session", "depends_on": []string{"never-built"}},
		{"change_name": "remote-keep", "priority": "low", "description": "Edited on origin", "depends_on": []string{"never-built"}},
	})
	_ = WriteJSONList(filepath.Join(ws, HistoryFile), []map[string]any{
		{"change_name": "hist-seed", "priority": "normal", "integrated_at": "2026-10-01T00:00:00Z"},
	})
	_ = WriteProposals(ws, []map[string]any{
		{"change_name": "prop-x", "priority": "low", "description": "Proposed", "status": "proposed", "source": "exploration"},
	})
	_ = os.WriteFile(memPath(ws), []byte("# MEMORY\n- Seed lesson.\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(ws, filepath.FromSlash(memory.ModulesDir)), 0o755)
	_ = os.WriteFile(filepath.Join(ws, ".agent-memory/modules/repo-backend.md"), []byte("# backend\n- Base backend lesson.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(ws, ".agent-memory/modules/spec-auth.md"), []byte("# auth\n- Base auth lesson.\n"), 0o644)

	commitAll(t, ws, "s: initial backlog, proposals, memory")
	if _, err := gitx.RunGit(ctx, ws, "push", "-u", "origin", "master"); err != nil {
		t.Fatalf("initial push failed: %v", err)
	}

	// Clone 'other' checkout to simulate concurrent merger/other session pushing to origin/master
	other := filepath.Join(workRoot, "other")
	if _, err := gitx.RunGit(ctx, workRoot, "clone", originBare, other); err != nil {
		t.Fatalf("clone other failed: %v", err)
	}
	_, _ = gitx.RunGit(ctx, other, "config", "user.name", "Other Writer")
	_, _ = gitx.RunGit(ctx, other, "config", "user.email", "other@example.com")

	otherSync := func() {
		if _, err := gitx.RunGit(ctx, other, "pull", "--rebase", "origin", "master"); err != nil {
			t.Fatalf("other pull failed: %v", err)
		}
	}
	otherPush := func(msg string) {
		commitAll(t, other, msg)
		if _, err := gitx.RunGit(ctx, other, "push", "origin", "master"); err != nil {
			t.Fatalf("other push (%s) failed: %v", msg, err)
		}
	}
	originBlob := func(relPath string) string {
		_, _ = gitx.RunGit(ctx, ws, "fetch", "-q", "origin")
		repo, err := gitx.Open(ws)
		if err != nil {
			t.Fatalf("gitx.Open failed: %v", err)
		}
		b, err := repo.ReadBlob("origin/master", relPath)
		if err != nil {
			t.Fatalf("ReadBlob(origin/master, %s) failed: %v", relPath, err)
		}
		return string(b)
	}

	cw := filepath.Join(ws, ".worktrees", "_cycle")

	// =========================================================================
	// S1 & S2: 3-way merge of the files of .agent-memory/,
	// agent_backlog.json, agent_history.json, and agent_proposals.json
	// =========================================================================
	baseRef, _ := gitx.RunGit(ctx, ws, "rev-parse", "origin/master")
	baseRef = strings.TrimSpace(baseRef)
	if err := PrepareSessionWorktree(ctx, ws, cw, baseRef, "master", io.Discard); err != nil {
		t.Fatalf("S1 PrepareSessionWorktree failed: %v", err)
	}

	// Remote writer modifies the lessons of the memory, .agent-memory/modules/repo-backend.md, .agent-memory/modules/spec-auth.md, backlog, history, and proposals
	otherSync()
	_ = os.WriteFile(memPath(other), []byte("# MEMORY\n- Seed lesson.\n- Remote lesson.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(other, ".agent-memory/modules/repo-backend.md"), []byte("# backend\n- Base backend lesson.\n- Remote backend lesson.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(other, ".agent-memory/modules/spec-auth.md"), []byte("# auth\n- Base auth lesson.\n- Remote auth lesson.\n"), 0o644)
	_ = WriteJSONList(filepath.Join(other, BacklogFile), []map[string]any{
		{"change_name": "shared-item", "priority": "low", "description": "Edited by the session", "depends_on": []string{"never-built"}},
		{"change_name": "to-delete", "priority": "low", "description": "Deleted by the session", "depends_on": []string{"never-built"}},
		{"change_name": "remote-keep", "priority": "low", "description": "Edited on origin, kept", "depends_on": []string{"never-built"}},
		{"change_name": "remote-new", "priority": "low", "depends_on": []string{"never-built"}},
	})
	_ = WriteJSONList(filepath.Join(other, HistoryFile), []map[string]any{
		{"change_name": "hist-seed", "priority": "normal", "integrated_at": "2026-10-01T00:00:00Z"},
		{"change_name": "hist-remote", "priority": "high", "integrated_at": "2026-10-02T00:00:00Z"},
	})
	_ = WriteProposals(other, []map[string]any{
		{
			"change_name": "prop-x",
			"priority":    "low",
			"description": "Proposed",
			"status":      "queued",
			"source":      "exploration",
			"review":      map[string]any{"by": "jev"},
			"queued_at":   "2026-10-21T00:00:00Z",
		},
	})
	otherPush("other: remote edits")

	// Local session in .worktrees/_cycle modifies the same files concurrently
	_ = os.WriteFile(memPath(cw), []byte("# MEMORY\n- Seed lesson.\n- Local lesson.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(cw, ".agent-memory/modules/repo-backend.md"), []byte("# backend\n- Base backend lesson.\n- Local backend lesson.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(cw, ".agent-memory/modules/spec-auth.md"), []byte("# auth\n- Base auth lesson.\n- Local auth lesson.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(cw, ".agent-memory/modules/repo-frontend.md"), []byte("# frontend\n- New frontend lesson.\n"), 0o644)
	_ = WriteJSONList(filepath.Join(cw, BacklogFile), []map[string]any{
		{"change_name": "shared-item", "priority": "high", "description": "Edited by the session", "depends_on": []string{"never-built"}},
		{"change_name": "remote-keep", "priority": "low", "description": "Edited on origin", "depends_on": []string{"never-built"}},
		{"change_name": "local-new", "priority": "low", "depends_on": []string{"never-built"}},
	})
	_ = WriteJSONList(filepath.Join(cw, HistoryFile), []map[string]any{
		{"change_name": "hist-seed", "priority": "normal", "integrated_at": "2026-10-01T00:00:00Z"},
		{"change_name": "hist-local", "priority": "low", "integrated_at": "2026-10-03T00:00:00Z"},
	})
	_ = WriteProposals(cw, []map[string]any{
		{"change_name": "prop-x", "priority": "low", "description": "Proposed", "status": "approved", "source": "exploration"},
		{"change_name": "prop-y", "priority": "low", "description": "New", "status": "proposed", "source": "exploration"},
	})

	if err := RecordSession(ctx, cw, "master", "chore(memory): s1 and s2 record", st, io.Discard); err != nil {
		t.Fatalf("S1/S2 RecordSession failed: %v", err)
	}

	// Verify S1: the files of the memory
	memOrigin := originBlob(memRel)
	if !strings.Contains(memOrigin, "Remote lesson.") || !strings.Contains(memOrigin, "Local lesson.") ||
		strings.Count(memOrigin, "Seed lesson.") != 1 || !strings.HasSuffix(memOrigin, "\n") {
		t.Fatalf("S1: unexpected merged memory file:\n%s", memOrigin)
	}
	beOrigin := originBlob(".agent-memory/modules/repo-backend.md")
	if !strings.Contains(beOrigin, "Remote backend lesson.") || !strings.Contains(beOrigin, "Local backend lesson.") {
		t.Fatalf("S1: unexpected merged .agent-memory/modules/repo-backend.md:\n%s", beOrigin)
	}
	authOrigin := originBlob(".agent-memory/modules/spec-auth.md")
	if !strings.Contains(authOrigin, "Remote auth lesson.") || !strings.Contains(authOrigin, "Local auth lesson.") {
		t.Fatalf("S1: unexpected merged .agent-memory/modules/spec-auth.md:\n%s", authOrigin)
	}
	if !strings.Contains(originBlob(".agent-memory/modules/repo-frontend.md"), "New frontend lesson.") {
		t.Fatalf("S1: missing new file .agent-memory/modules/repo-frontend.md on origin")
	}

	// Verify S1 backlog (to-delete deleted, local-new and remote-new added, shared-item=high, remote-keep='Edited on origin, kept')
	blOrigin, _ := parseJSONListBytes([]byte(originBlob(BacklogFile)))
	var blNames []string
	blBy := make(map[string]map[string]any)
	for _, it := range blOrigin {
		n := asString(it["change_name"])
		blNames = append(blNames, n)
		blBy[n] = it
	}
	sort.Strings(blNames)
	if !equalStringSlices(blNames, []string{"local-new", "remote-keep", "remote-new", "shared-item"}) {
		t.Fatalf("S1: unexpected backlog names after 3-way merge: %v", blNames)
	}
	if asString(blBy["shared-item"]["priority"]) != "high" || asString(blBy["remote-keep"]["description"]) != "Edited on origin, kept" {
		t.Fatalf("S1: unexpected backlog item fields: %+v", blBy)
	}

	// Verify S1 history
	histOrigin, _ := parseJSONListBytes([]byte(originBlob(HistoryFile)))
	if len(histOrigin) != 3 {
		t.Fatalf("S1: expected 3 history items (hist-seed, hist-remote, hist-local), got %+v", histOrigin)
	}

	// Verify S2 proposals (prop-x keeps status=queued and review.by=jev; prop-y added)
	propsOrigin, _ := parseJSONListBytes([]byte(originBlob(ProposalsFile)))
	propBy := make(map[string]map[string]any)
	for _, p := range propsOrigin {
		propBy[asString(p["change_name"])] = p
	}
	revX, _ := propBy["prop-x"]["review"].(map[string]any)
	if asString(propBy["prop-x"]["status"]) != "queued" || asString(revX["by"]) != "jev" || propBy["prop-y"] == nil {
		t.Fatalf("S2: unexpected merged proposals: %+v", propsOrigin)
	}

	// =========================================================================
	// S3: RebaseProposals refuses two local commits and leaves HEAD untouched
	// =========================================================================
	baseRef, _ = gitx.RunGit(ctx, ws, "rev-parse", "origin/master")
	_ = PrepareSessionWorktree(ctx, ws, cw, strings.TrimSpace(baseRef), "master", io.Discard)
	_, _ = gitx.RunGit(ctx, cw, "commit", "--allow-empty", "-m", "chore(memory): one")
	_, _ = gitx.RunGit(ctx, cw, "commit", "--allow-empty", "-m", "chore(memory): two")
	twoSHA, _ := gitx.RunGit(ctx, cw, "rev-parse", "HEAD")
	twoSHA = strings.TrimSpace(twoSHA)

	otherSync()
	_ = os.WriteFile(memPath(other), []byte(originBlob(memRel)+"- other s3\n"), 0o644)
	otherPush("other: s3")

	if err := RebaseProposals(ctx, cw, "master", io.Discard); err == nil {
		t.Fatalf("S3: expected RebaseProposals to refuse 2 local commits")
	}
	afterTwoSHA, _ := gitx.RunGit(ctx, cw, "rev-parse", "HEAD")
	if strings.TrimSpace(afterTwoSHA) != twoSHA {
		t.Fatalf("S3: expected HEAD to remain at %s, got %s", twoSHA, strings.TrimSpace(afterTwoSHA))
	}

	// =========================================================================
	// S4: Worktree behind origin with no record of its own is fast-forwarded
	// =========================================================================
	_, _ = gitx.RunGit(ctx, ws, "fetch", "-q", "origin")
	prevOrigin, _ := gitx.RunGit(ctx, ws, "rev-parse", "origin/master~1")
	_ = PrepareSessionWorktree(ctx, ws, cw, strings.TrimSpace(prevOrigin), "master", io.Discard)
	if err := RebaseProposals(ctx, cw, "master", io.Discard); err != nil {
		t.Fatalf("S4: RebaseProposals fast-forward failed: %v", err)
	}
	cwHead, _ := gitx.RunGit(ctx, cw, "rev-parse", "HEAD")
	origHead, _ := gitx.RunGit(ctx, ws, "rev-parse", "origin/master")
	if strings.TrimSpace(cwHead) != strings.TrimSpace(origHead) {
		t.Fatalf("S4: expected fast-forward to %s, got %s", strings.TrimSpace(origHead), strings.TrimSpace(cwHead))
	}

	// =========================================================================
	// S5: Rebase failure mid-way rolls back to the record commit, and the next
	// PrepareSessionWorktree publishes it rebased onto origin/master
	// =========================================================================
	_ = PrepareSessionWorktree(ctx, ws, cw, strings.TrimSpace(origHead), "master", io.Discard)
	_ = os.WriteFile(memPath(cw), []byte(originBlob(memRel)+"- S5 lesson.\n"), 0o644)
	s5SHA := commitAll(t, cw, "chore(memory): s5 record")

	otherSync()
	_ = os.WriteFile(memPath(other), []byte(originBlob(memRel)+"- other s5\n"), 0o644)
	otherPush("other: s5")

	// Configure a failing pre-commit hook via git config in cw
	hooksDir := filepath.Join(workRoot, "hooks-s5")
	_ = os.MkdirAll(hooksDir, 0o755)
	_ = os.WriteFile(filepath.Join(hooksDir, "pre-commit"), []byte("#!/bin/sh\necho 'pre-commit refuses' >&2\nexit 1\n"), 0o755)
	_, _ = gitx.RunGit(ctx, cw, "config", "core.hooksPath", hooksDir)

	err := RebaseProposals(ctx, cw, "master", io.Discard)
	_, _ = gitx.RunGit(ctx, cw, "config", "--unset", "core.hooksPath")
	if err == nil {
		t.Fatalf("S5: expected RebaseProposals to fail when pre-commit hook rejects commit")
	}
	cwHeadAfterFail, _ := gitx.RunGit(ctx, cw, "rev-parse", "HEAD")
	if strings.TrimSpace(cwHeadAfterFail) != s5SHA {
		t.Fatalf("S5: expected rollback to %s, got %s", s5SHA, strings.TrimSpace(cwHeadAfterFail))
	}
	cwMemBytes, _ := os.ReadFile(memPath(cw))
	if !strings.Contains(string(cwMemBytes), "S5 lesson.") {
		t.Fatalf("S5: expected S5 lesson preserved after rollback")
	}

	// Next PrepareSessionWorktree publishes the leftover record
	_, _ = gitx.RunGit(ctx, ws, "fetch", "-q", "origin")
	origHead, _ = gitx.RunGit(ctx, ws, "rev-parse", "origin/master")
	var s5bOut bytes.Buffer
	if err := PrepareSessionWorktree(ctx, ws, cw, strings.TrimSpace(origHead), "master", &s5bOut); err != nil {
		t.Fatalf("S5b: PrepareSessionWorktree failed: %v", err)
	}
	if !strings.Contains(s5bOut.String(), "published the record an earlier session left") {
		t.Fatalf("S5b: expected leftover record to be published, got: %s", s5bOut.String())
	}
	memS5 := originBlob(memRel)
	if !strings.Contains(memS5, "S5 lesson.") || !strings.Contains(memS5, "other s5") {
		t.Fatalf("S5b: expected both S5 lesson and other s5 on origin, got:\n%s", memS5)
	}

	// =========================================================================
	// S6: A whole cycle with origin unreachable keeps its record in .worktrees/_cycle;
	// the next cycle publishes it before judging, then records its own
	// =========================================================================
	_, _ = gitx.RunGit(ctx, ws, "remote", "set-url", "origin", filepath.Join(workRoot, "missing.git"))
	var s6aOut bytes.Buffer
	_ = RunCycle(ctx, ws, st, CycleOptions{
		Stamp: "20261108T100000Z",
		AgentExecFunc: func(ctx context.Context, workdir, pf, engine string, timeout time.Duration, out io.Writer) error {
			bl, _ := ReadJSONList(filepath.Join(workdir, BacklogFile))
			bl = append(bl, map[string]any{"change_name": "offline-item", "priority": "low", "depends_on": []string{"never-built"}})
			return WriteJSONList(filepath.Join(workdir, BacklogFile), bl)
		},
	}, &s6aOut)
	_, _ = gitx.RunGit(ctx, ws, "remote", "set-url", "origin", originBare)

	if !strings.Contains(s6aOut.String(), "record: not published") {
		t.Fatalf("S6a: expected 'record: not published', got:\n%s", s6aOut.String())
	}
	if strings.Contains(originBlob(BacklogFile), "offline-item") {
		t.Fatalf("S6a: offline-item should not be on origin while origin was unreachable")
	}

	var s6bOut bytes.Buffer
	if err := RunCycle(ctx, ws, st, CycleOptions{
		Stamp: "20261108T100500Z",
		AgentExecFunc: func(ctx context.Context, workdir, pf, engine string, timeout time.Duration, out io.Writer) error {
			bl, _ := ReadJSONList(filepath.Join(workdir, BacklogFile))
			bl = append(bl, map[string]any{"change_name": "online-item", "priority": "low", "depends_on": []string{"never-built"}})
			return WriteJSONList(filepath.Join(workdir, BacklogFile), bl)
		},
	}, &s6bOut); err != nil {
		t.Fatalf("S6b: RunCycle failed: %v", err)
	}
	if !strings.Contains(s6bOut.String(), "published the record an earlier session left") {
		t.Fatalf("S6b: expected leftover record published before judging, got:\n%s", s6bOut.String())
	}
	blS6 := originBlob(BacklogFile)
	if !strings.Contains(blS6, "offline-item") || !strings.Contains(blS6, "online-item") {
		t.Fatalf("S6b: expected both offline-item and online-item on origin, got:\n%s", blS6)
	}

	// =========================================================================
	// S7: A leftover that still cannot be published is saved under refs/openspec/unpublished/
	// =========================================================================
	_ = os.WriteFile(memPath(cw), []byte(originBlob(memRel)+"- Kept S7 lesson.\n"), 0o644)
	keptSHA := commitAll(t, cw, "chore(memory): s7 leftover")
	_, _ = gitx.RunGit(ctx, ws, "remote", "set-url", "origin", filepath.Join(workRoot, "missing.git"))
	origHead, _ = gitx.RunGit(ctx, ws, "rev-parse", "origin/master")
	var s7Out bytes.Buffer
	_ = PrepareSessionWorktree(ctx, ws, cw, strings.TrimSpace(origHead), "master", &s7Out)
	_, _ = gitx.RunGit(ctx, ws, "remote", "set-url", "origin", originBare)

	if !strings.Contains(s7Out.String(), "could not publish the record an earlier session left") ||
		!strings.Contains(s7Out.String(), "refs/openspec/unpublished/_cycle-") {
		t.Fatalf("S7: expected unpublished ref message, got:\n%s", s7Out.String())
	}
	mRef := regexp.MustCompile(`refs/openspec/unpublished/_cycle-[^\s)]+`).FindString(s7Out.String())
	refSHA, err := gitx.RunGit(ctx, ws, "rev-parse", mRef)
	if err != nil || strings.TrimSpace(refSHA) != keptSHA {
		t.Fatalf("S7: expected ref %s to point to %s, got %s (err=%v)", mRef, keptSHA, strings.TrimSpace(refSHA), err)
	}

	// =========================================================================
	// S8: A session worktree without its .git or deleted is re-created without touching main checkout
	// =========================================================================
	_ = SyncRepos(ctx, ws, st, false, io.Discard)
	_ = os.Remove(filepath.Join(cw, ".git"))
	_ = os.WriteFile(memPath(ws), []byte(originBlob(memRel)+"- Operator lesson in progress.\n"), 0o644)
	wsHeadBefore, _ := gitx.RunGit(ctx, ws, "rev-parse", "HEAD")
	origHead, _ = gitx.RunGit(ctx, ws, "rev-parse", "origin/master")
	if err := PrepareSessionWorktree(ctx, ws, cw, strings.TrimSpace(origHead), "master", io.Discard); err != nil {
		t.Fatalf("S8: PrepareSessionWorktree failed: %v", err)
	}
	wsHeadAfter, _ := gitx.RunGit(ctx, ws, "rev-parse", "HEAD")
	wsMemAfter, _ := os.ReadFile(memPath(ws))
	if strings.TrimSpace(wsHeadAfter) != strings.TrimSpace(wsHeadBefore) || !strings.Contains(string(wsMemAfter), "Operator lesson in progress.") {
		t.Fatalf("S8: main checkout was disturbed!")
	}
	_, _ = gitx.RunGit(ctx, ws, "checkout", "-q", "--", memRel)

	// =========================================================================
	// S9, S10, S11: Operator's uncommitted edits in a main checkout behind origin
	// are carried 3-way by RunCycle, published, and dropped once recorded
	// =========================================================================
	_ = SyncRepos(ctx, ws, st, false, io.Discard)
	otherSync()
	_ = os.WriteFile(memPath(other), []byte(originBlob(memRel)+"- Remote lesson 2.\n"), 0o644)
	otherBl, _ := ReadJSONList(filepath.Join(other, BacklogFile))
	otherBl = append(otherBl, map[string]any{"change_name": "remote-item-2", "priority": "low", "depends_on": []string{"never-built"}})
	_ = WriteJSONList(filepath.Join(other, BacklogFile), otherBl)
	otherPush("other: while main checkout is behind")

	// Operator makes uncommitted edits in ws (which is now behind origin/master)
	wsMemCur, _ := os.ReadFile(memPath(ws))
	_ = os.WriteFile(memPath(ws), append(wsMemCur, []byte("- Operator lesson.\n")...), 0o644)
	wsBl, _ := ReadJSONList(filepath.Join(ws, BacklogFile))
	wsBl = append(wsBl, map[string]any{"change_name": "operator-item", "priority": "low", "depends_on": []string{"never-built"}})
	_ = WriteJSONList(filepath.Join(ws, BacklogFile), wsBl)
	_ = os.WriteFile(filepath.Join(ws, ".agent-memory/modules/spec-billing.md"), []byte("# billing\n- Billing lesson.\n"), 0o644)

	if err := RunCycle(ctx, ws, st, CycleOptions{
		Stamp: "20261108T110000Z",
		AgentExecFunc: func(ctx context.Context, workdir, pf, engine string, timeout time.Duration, out io.Writer) error {
			bl, _ := ReadJSONList(filepath.Join(workdir, BacklogFile))
			bl = append(bl, map[string]any{"change_name": "cycle-item", "priority": "low", "depends_on": []string{"never-built"}})
			return WriteJSONList(filepath.Join(workdir, BacklogFile), bl)
		},
	}, io.Discard); err != nil {
		t.Fatalf("S9: RunCycle failed: %v", err)
	}

	memS9 := originBlob(memRel)
	if !strings.Contains(memS9, "Remote lesson 2.") || !strings.Contains(memS9, "Operator lesson.") {
		t.Fatalf("S9: expected both Remote lesson 2 and Operator lesson on origin:\n%s", memS9)
	}
	blS9 := originBlob(BacklogFile)
	for _, wantName := range []string{"remote-item-2", "operator-item", "cycle-item"} {
		if !strings.Contains(blS9, wantName) {
			t.Fatalf("S9: expected %s in origin backlog", wantName)
		}
	}
	if !strings.Contains(originBlob(".agent-memory/modules/spec-billing.md"), "Billing lesson.") {
		t.Fatalf("S9: expected .agent-memory/modules/spec-billing.md on origin")
	}
	// Main checkout dropped carried edits and caught up with origin
	wsDirty, _ := gitx.RunGit(ctx, ws, "status", "--porcelain")
	if strings.TrimSpace(wsDirty) != "" {
		t.Fatalf("S9: expected clean main checkout after cycle carried and published edits, got:\n%s", wsDirty)
	}

	// S10: A cycle stopped by ScopeGuard after carry leaves carried edit in main checkout; next cycle records it once
	wsMemCur, _ = os.ReadFile(memPath(ws))
	_ = os.WriteFile(memPath(ws), append(wsMemCur, []byte("- Operator lesson 2.\n")...), 0o644)
	err = RunCycle(ctx, ws, st, CycleOptions{
		Stamp: "20261108T110500Z",
		AgentExecFunc: func(ctx context.Context, workdir, pf, engine string, timeout time.Duration, out io.Writer) error {
			// Escape scope!
			_ = os.MkdirAll(filepath.Join(workdir, "openspec"), 0o755)
			return os.WriteFile(filepath.Join(workdir, "openspec/config.yaml"), []byte("oops: escaped\n"), 0o644)
		},
	}, io.Discard)
	if err == nil {
		t.Fatalf("S10: expected RunCycle to fail on ScopeGuard violation")
	}
	wsState, _ := st.GetWorkspaceState(ctx)
	if !wsState.Paused {
		t.Fatalf("S10: expected workspace paused in store")
	}
	if strings.Contains(originBlob(memRel), "Operator lesson 2.") {
		t.Fatalf("S10: Operator lesson 2 must not be published when ScopeGuard fails")
	}
	wsMemStill, _ := os.ReadFile(memPath(ws))
	if !strings.Contains(string(wsMemStill), "Operator lesson 2.") {
		t.Fatalf("S10: Operator lesson 2 must remain in main checkout")
	}

	// Unpause and run next cycle -> records Operator lesson 2 once and cleans main checkout
	_ = st.SetPaused(ctx, false, "")
	_ = os.Remove(filepath.Join(ws, "log", "PAUSE"))
	if err := RunCycle(ctx, ws, st, CycleOptions{
		Stamp: "20261108T111000Z",
		AgentExecFunc: func(ctx context.Context, workdir, pf, engine string, timeout time.Duration, out io.Writer) error {
			return nil
		},
	}, io.Discard); err != nil {
		t.Fatalf("S10b: RunCycle failed: %v", err)
	}
	if strings.Count(originBlob(memRel), "Operator lesson 2.") != 1 {
		t.Fatalf("S10b: expected Operator lesson 2 recorded exactly once on origin")
	}

	// S11: ReleaseScopeEdits keeps an edit modified again after carry, drops an unchanged one
	wsMemCur, _ = os.ReadFile(memPath(ws))
	_ = os.WriteFile(memPath(ws), append(wsMemCur, []byte("- Edited once.\n")...), 0o644)
	snapBefore, _ := ScopeEdits(ctx, ws)
	_ = os.WriteFile(memPath(ws), append(wsMemCur, []byte("- Edited once.\n- Edited twice.\n")...), 0o644)
	_ = ReleaseScopeEdits(ctx, ws, snapBefore)
	wsMemCheck, _ := os.ReadFile(memPath(ws))
	if !strings.Contains(string(wsMemCheck), "Edited twice.") {
		t.Fatalf("S11: ReleaseScopeEdits must keep file modified after snapshot")
	}
	snapBefore2, _ := ScopeEdits(ctx, ws)
	_ = ReleaseScopeEdits(ctx, ws, snapBefore2)
	wsDirty, _ = gitx.RunGit(ctx, ws, "status", "--porcelain")
	if strings.TrimSpace(wsDirty) != "" {
		t.Fatalf("S11: ReleaseScopeEdits must drop unchanged edits, got:\n%s", wsDirty)
	}

	// =========================================================================
	// S12: SyncRepos restores a backlog holding a version origin published since HEAD
	// =========================================================================
	otherSync()
	otherBl, _ = ReadJSONList(filepath.Join(other, BacklogFile))
	otherBl = append(otherBl, map[string]any{"change_name": "v1-item", "depends_on": []string{"never-built"}})
	_ = WriteJSONList(filepath.Join(other, BacklogFile), otherBl)
	otherPush("other: backlog v1")
	v1SHA, _ := gitx.RunGit(ctx, other, "rev-parse", "HEAD")
	v1SHA = strings.TrimSpace(v1SHA)

	otherBl = append(otherBl, map[string]any{"change_name": "v2-item", "depends_on": []string{"never-built"}})
	_ = WriteJSONList(filepath.Join(other, BacklogFile), otherBl)
	otherPush("other: backlog v2")

	_, _ = gitx.RunGit(ctx, ws, "fetch", "-q", "origin")
	v1Backlog, _ := gitx.RunGit(ctx, ws, "show", v1SHA+":"+BacklogFile)
	_ = os.WriteFile(filepath.Join(ws, BacklogFile), []byte(v1Backlog), 0o644)

	var s12Out bytes.Buffer
	if err := SyncRepos(ctx, ws, st, false, &s12Out); err != nil {
		t.Fatalf("S12a: SyncRepos failed: %v", err)
	}
	wsDirty, _ = gitx.RunGit(ctx, ws, "status", "--porcelain")
	wsHead, _ := gitx.RunGit(ctx, ws, "rev-parse", "HEAD")
	origHead, _ = gitx.RunGit(ctx, ws, "rev-parse", "origin/master")
	if strings.TrimSpace(wsDirty) != "" || strings.TrimSpace(wsHead) != strings.TrimSpace(origHead) {
		t.Fatalf("S12a: expected clean fast-forwarded checkout, dirty=%q head=%s orig=%s", wsDirty, wsHead, origHead)
	}

	// =========================================================================
	// S14: WithRecordLock reclaims dead lock immediately and times out behind live lock
	// =========================================================================
	_, _ = st.AcquireLock(ctx, "record", store.LockOptions{
		OwnerType: "record",
		PID:       999998, // dead PID
		TTL:       10 * time.Minute,
	})
	if err := WithRecordLock(ctx, st, 5*time.Second, func() error { return nil }); err != nil {
		t.Fatalf("S14: expected dead PID record lock to be reclaimed immediately: %v", err)
	}

	// Live PID lock held -> WithRecordLock times out
	_, _ = st.AcquireLock(ctx, "record", store.LockOptions{
		OwnerType: "record",
		PID:       os.Getpid(),
		TTL:       10 * time.Minute,
	})
	err = WithRecordLock(ctx, st, 1*time.Second, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "record lock: held") {
		t.Fatalf("S14: expected WithRecordLock to time out behind live lock, got %v", err)
	}
	_ = st.ReleaseLock(ctx, "record", os.Getpid())

	// =========================================================================
	// S18, S20, S21, S22: Push retry on pre-receive rejection, non-JSON backlog text merge,
	// index.lock SaveUnpublishedCommit + stale index.lock cleanup, and rewritten origin base
	// =========================================================================
	// S18: Push rejected once by pre-receive hook succeeds on retry
	rejectOnceFile := filepath.Join(workRoot, "reject.once")
	_ = os.WriteFile(rejectOnceFile, []byte("1"), 0o644)
	preReceiveHook := filepath.Join(originBare, "hooks", "pre-receive")
	_ = os.MkdirAll(filepath.Dir(preReceiveHook), 0o755)
	_ = os.WriteFile(preReceiveHook, []byte("#!/bin/sh\nif [ -e \""+rejectOnceFile+"\" ]; then rm -f \""+rejectOnceFile+"\"; echo 'rejected once' >&2; exit 1; fi\n"), 0o755)

	_, _ = gitx.RunGit(ctx, ws, "fetch", "-q", "origin")
	origHead, _ = gitx.RunGit(ctx, ws, "rev-parse", "origin/master")
	_ = PrepareSessionWorktree(ctx, ws, cw, strings.TrimSpace(origHead), "master", io.Discard)
	_ = os.WriteFile(memPath(cw), []byte(originBlob(memRel)+"- Rejected once lesson.\n"), 0o644)
	if err := RecordSession(ctx, cw, "master", "chore(memory): s18 record", st, io.Discard); err != nil {
		t.Fatalf("S18: expected RecordSession retry to succeed after single pre-receive rejection: %v", err)
	}
	_ = os.Remove(preReceiveHook)
	if !strings.Contains(originBlob(memRel), "Rejected once lesson.") {
		t.Fatalf("S18: expected Rejected once lesson on origin")
	}

	// S20: Non-JSON backlog merges as text without losing or blocking the record
	_, _ = gitx.RunGit(ctx, ws, "fetch", "-q", "origin")
	origHead, _ = gitx.RunGit(ctx, ws, "rev-parse", "origin/master")
	_ = PrepareSessionWorktree(ctx, ws, cw, strings.TrimSpace(origHead), "master", io.Discard)
	otherSync()
	_ = os.WriteFile(memPath(other), []byte(originBlob(memRel)+"- late remote s20\n"), 0o644)
	otherPush("other: late s20")
	_ = os.WriteFile(filepath.Join(cw, BacklogFile), []byte("[\n  not valid json\n"), 0o644)
	if err := RecordSession(ctx, cw, "master", "chore(memory): s20 record", st, io.Discard); err != nil {
		t.Fatalf("S20: expected non-JSON backlog to merge as text and publish: %v", err)
	}
	if !strings.Contains(originBlob(BacklogFile), "not valid json") {
		t.Fatalf("S20: expected non-JSON backlog content on origin")
	}
	// Restore valid JSON backlog on origin
	otherSync()
	_ = WriteJSONList(filepath.Join(other, BacklogFile), []map[string]any{})
	otherPush("other: restore valid backlog")

	// S21: index.lock refuses commit -> SaveUnpublishedCommit saves under refs/openspec/unpublished/;
	// young index.lock stops PrepareSessionWorktree; >10m old index.lock is removed and worktree reset
	_, _ = gitx.RunGit(ctx, ws, "fetch", "-q", "origin")
	origHead, _ = gitx.RunGit(ctx, ws, "rev-parse", "origin/master")
	_ = PrepareSessionWorktree(ctx, ws, cw, strings.TrimSpace(origHead), "master", io.Discard)
	absGitDir, _ := gitx.RunGit(ctx, cw, "rev-parse", "--absolute-git-dir")
	indexLock := filepath.Join(strings.TrimSpace(absGitDir), "index.lock")
	_ = os.WriteFile(memPath(cw), []byte(originBlob(memRel)+"- Locked S21 lesson.\n"), 0o644)
	_ = os.WriteFile(indexLock, []byte("locked"), 0o644)

	var s21Out bytes.Buffer
	err = RecordSession(ctx, cw, "master", "chore(memory): s21 record", st, &s21Out)
	if err == nil || !strings.Contains(s21Out.String(), "record: git refused the commit; kept as refs/openspec/unpublished/_cycle-") {
		t.Fatalf("S21: expected SaveUnpublishedCommit fallback when index.lock is present, err=%v out=%s", err, s21Out.String())
	}
	s21Ref := regexp.MustCompile(`refs/openspec/unpublished/_cycle-[^\s(]+`).FindString(s21Out.String())
	s21Blob, err := gitx.RunGit(ctx, ws, "show", s21Ref+":"+memRel)
	if err != nil || !strings.Contains(s21Blob, "Locked S21 lesson.") {
		t.Fatalf("S21: expected Locked S21 lesson in saved ref %s, got err=%v blob=%s", s21Ref, err, s21Blob)
	}

	// Young index.lock (<10m) stops PrepareSessionWorktree
	if err := PrepareSessionWorktree(ctx, ws, cw, strings.TrimSpace(origHead), "master", io.Discard); err == nil {
		t.Fatalf("S21b: expected young index.lock to stop PrepareSessionWorktree")
	}

	// Old index.lock (>10m) is removed and PrepareSessionWorktree succeeds
	oldTime := time.Now().Add(-20 * time.Minute)
	_ = os.Chtimes(indexLock, oldTime, oldTime)
	var s21cOut bytes.Buffer
	if err := PrepareSessionWorktree(ctx, ws, cw, strings.TrimSpace(origHead), "master", &s21cOut); err != nil {
		t.Fatalf("S21c: expected stale index.lock to be removed: %v", err)
	}
	if !strings.Contains(s21cOut.String(), "which no git had touched for 10 minutes") || fileExists(indexLock) {
		t.Fatalf("S21c: expected stale index.lock removal message, got: %s", s21cOut.String())
	}

	// S22: Leftover record on a base origin rewrote (force-pushed) is kept under refs/openspec/unpublished/
	_ = os.WriteFile(memPath(cw), []byte(originBlob(memRel)+"- Rewritten S22 lesson.\n"), 0o644)
	leftoverS22 := commitAll(t, cw, "chore(memory): s22 leftover")
	otherSync()
	_, _ = gitx.RunGit(ctx, other, "reset", "--hard", "HEAD~1")
	_ = os.WriteFile(memPath(other), []byte("# MEMORY\n- rewritten history\n"), 0o644)
	commitAll(t, other, "other: rewritten history")
	_, _ = gitx.RunGit(ctx, other, "push", "--force", "origin", "master")
	_, _ = gitx.RunGit(ctx, ws, "fetch", "-q", "origin")
	origHead, _ = gitx.RunGit(ctx, ws, "rev-parse", "origin/master")

	var s22Out bytes.Buffer
	if err := PrepareSessionWorktree(ctx, ws, cw, strings.TrimSpace(origHead), "master", &s22Out); err != nil {
		t.Fatalf("S22: PrepareSessionWorktree failed: %v", err)
	}
	if !strings.Contains(s22Out.String(), "sits on a base origin no longer has; kept as refs/openspec/unpublished/_cycle-") {
		t.Fatalf("S22: expected rewritten base message, got:\n%s", s22Out.String())
	}
	s22Ref := regexp.MustCompile(`refs/openspec/unpublished/_cycle-[^\s)]+`).FindString(s22Out.String())
	s22RefSHA, _ := gitx.RunGit(ctx, ws, "rev-parse", s22Ref)
	if strings.TrimSpace(s22RefSHA) != leftoverS22 {
		t.Fatalf("S22: expected %s to point to %s, got %s", s22Ref, leftoverS22, strings.TrimSpace(s22RefSHA))
	}
	if strings.Contains(originBlob(memRel), "Rewritten S22 lesson.") {
		t.Fatalf("S22: rewritten base leftover must not be pushed to origin")
	}
}

// TestStressScenarioV_ParallelVisualQAAndHeadlessCycle covers Scenario V and I16/P visual queue integration:
//   - VisualQAEnqueueIntegrated skipping CLI-only changes and queuing web changes
//   - VisualQABegin opening a session in .worktrees/_visual_qa, picking browser-only strategies,
//     running ServiceCheckFunc and ServicePrepareFunc with configured visual_qa.slot
//   - Parallel headless RunCycle executing in .worktrees/_cycle without touching the main checkout
//   - VisualQAEnd carrying and 3-way reconciling the visual session's findings with what the parallel
//     headless cycle pushed to origin/master without collision or data loss
//   - S19 fallback when .worktrees/_visual_qa vanishes mid-session
//   - ScopeGuard during VisualQAEnd pausing the workspace when an out-of-scope file is touched
func TestStressScenarioV_ParallelVisualQAAndHeadlessCycle(t *testing.T) {
	ctx := context.Background()
	workRoot := t.TempDir()

	originBare := filepath.Join(workRoot, "umbrella.git")
	_ = os.MkdirAll(originBare, 0o755)
	_, _ = gitx.RunGit(ctx, originBare, "init", "--bare", "-b", "master")

	ws := filepath.Join(workRoot, "ws")
	if err := Init(ctx, ws, KitOptions{Agents: "claude"}, io.Discard); err != nil {
		t.Fatalf("Init ws failed: %v", err)
	}
	initGitRepo(t, ws)
	_, _ = gitx.RunGit(ctx, ws, "remote", "add", "origin", originBare)
	st := openTestStore(t, ws)

	// Set up two child repos:repos/gocli (kind: cli) and repos/web (kind: web)
	_ = os.MkdirAll(filepath.Join(ws, "repos/gocli"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, "repos/gocli/repo.config.yaml"), []byte("name: gocli\nkind: cli\nstack: go-cli\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(ws, "repos/web"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, "repos/web/repo.config.yaml"), []byte("name: web\nkind: web\nstack: go-web-gin\n"), 0o644)
	_ = os.WriteFile(memPath(ws), []byte("# MEMORY\n- Seed lesson.\n"), 0o644)
	commitAll(t, ws, "initial setup")
	_, _ = gitx.RunGit(ctx, ws, "push", "-u", "origin", "master")

	// Baseline VisualQAEnqueueIntegrated records current HEAD
	if err := VisualQAEnqueueIntegrated(ctx, ws, st, io.Discard); err != nil {
		t.Fatalf("initial VisualQAEnqueueIntegrated failed: %v", err)
	}

	// 1. Commit a change that only touches repos/gocli (terminal program) -> skipped by VisualQAEnqueueIntegrated
	_ = os.WriteFile(filepath.Join(ws, "repos/gocli/cli.txt"), []byte("cli tweak\n"), 0o644)
	commitAll(t, ws, "chore(merge): integrate cli-tweak\n\nOpenSpec-Change: cli-tweak")
	var enqOut bytes.Buffer
	_ = VisualQAEnqueueIntegrated(ctx, ws, st, &enqOut)
	if !strings.Contains(enqOut.String(), "0 integrated change(s) queued; 1 only touched terminal programs (cli-tweak)") {
		t.Fatalf("expected cli-tweak to be skipped as terminal program, got: %s", enqOut.String())
	}

	// 2. Commit a change that touches repos/web -> queued for visual QA!
	_ = os.WriteFile(filepath.Join(ws, "repos/web/web.txt"), []byte("health page\n"), 0o644)
	commitAll(t, ws, "chore(merge): integrate add-health-check\n\nOpenSpec-Change: add-health-check")
	_, _ = gitx.RunGit(ctx, ws, "push", "origin", "master")
	enqOut.Reset()
	_ = VisualQAEnqueueIntegrated(ctx, ws, st, &enqOut)
	if !strings.Contains(enqOut.String(), "visual-qa: 1 integrated change(s) queued") {
		t.Fatalf("expected add-health-check to be queued for visual QA, got: %s", enqOut.String())
	}

	// Also add an explicit VisualQARequest
	reqItem, err := VisualQARequest(ctx, ws, st, "Verify mobile navbar collapse", "add-health-check", io.Discard)
	if err != nil || reqItem == nil {
		t.Fatalf("VisualQARequest failed: %v", err)
	}

	// 3. Configure slot: 97 in .foreman-kit/visual_qa.yaml and commit
	visualPath := filepath.Join(ws, filepath.FromSlash(settings.VisualQASlot.FilePath()))
	visualRaw, _ := os.ReadFile(visualPath)
	_ = os.WriteFile(visualPath, []byte(strings.Replace(string(visualRaw), "slot: none", "slot: 97", 1)), 0o644)
	commitAll(t, ws, "set the slot of the visual session to 97")
	_, _ = gitx.RunGit(ctx, ws, "push", "origin", "master")

	// 4. VisualQABegin opens session in .worktrees/_visual_qa, checks services, prepares on slot 97, claims items
	vStamp := "20261108T150000Z"
	var preparedWT string
	var preparedSlot int
	var beginOut bytes.Buffer
	err = VisualQABegin(ctx, ws, st, VisualQAOptions{
		Stamp: vStamp,
		ServiceCheckFunc: func(ctx context.Context, root string) error {
			return nil
		},
		ServicePrepareFunc: func(ctx context.Context, wt string, slot int) error {
			preparedWT = wt
			preparedSlot = slot
			return nil
		},
	}, &beginOut)
	if err != nil {
		t.Fatalf("VisualQABegin failed: %v", err)
	}
	if preparedSlot != 97 || !strings.HasSuffix(preparedWT, ".worktrees/_visual_qa") {
		t.Fatalf("expected ServicePrepareFunc on .worktrees/_visual_qa with slot=97, got wt=%s slot=%d", preparedWT, preparedSlot)
	}

	// Verify plan file lists add-health-check and explores the first browser strategy (missing-capabilities)
	planFile := store.RuntimePath(ws, "cycle", "visual-"+vStamp+".md")
	planBytes, err := os.ReadFile(planFile)
	if err != nil {
		t.Fatalf("missing visual plan file: %v", err)
	}
	planText := string(planBytes)
	if !strings.Contains(planText, "change `add-health-check`") ||
		!strings.Contains(planText, "(`missing-capabilities`, kind `features`)") {
		t.Fatalf("unexpected visual plan content:\n%s", planText)
	}

	// Calling VisualQABegin while session is active reports 'still running'
	var secondBegin bytes.Buffer
	_ = VisualQABegin(ctx, ws, st, VisualQAOptions{Stamp: "20261108T150500Z"}, &secondBegin)
	if !strings.Contains(secondBegin.String(), "is still running") {
		t.Fatalf("expected second VisualQABegin to report still running, got: %s", secondBegin.String())
	}

	// 5. Simulate visual QA agent in the main checkout:
	// - Close item 1 as verified (leaving item 2 in_progress so VisualQAEnd returns it to pending)
	// - Write a visual lesson to the memory, a defect to .foreman-kit/agent_backlog.json, and a proposal to .foreman-kit/agent_proposals.json
	if err := VisualQADone(ctx, ws, st, "1", "verified", "the health page renders cleanly", io.Discard); err != nil {
		t.Fatalf("VisualQADone failed: %v", err)
	}
	wsMem, _ := os.ReadFile(memPath(ws))
	_ = os.WriteFile(memPath(ws), append(wsMem, []byte("- Visual QA lesson: check the save button copy.\n")...), 0o644)
	_ = WriteJSONList(filepath.Join(ws, BacklogFile), []map[string]any{
		{"change_name": "fix-button-label", "priority": "normal", "description": "The save button reads Sumbit"},
	})
	_ = WriteProposals(ws, []map[string]any{
		{"change_name": "visual-dark-mode", "priority": "low", "description": "Dark mode contrast", "status": "proposed", "source": "visual-qa"},
	})

	// 6. While the visual QA session has uncommitted findings in root, a headless cycle runs in .worktrees/_cycle!
	var vCycleOut bytes.Buffer
	err = RunCycle(ctx, ws, st, CycleOptions{
		Stamp: "20261108T151000Z",
		AgentExecFunc: func(ctx context.Context, workdir, pf, engine string, timeout time.Duration, out io.Writer) error {
			if !strings.HasSuffix(workdir, ".worktrees/_cycle") {
				t.Fatalf("expected parallel cycle to judge in .worktrees/_cycle, got %s", workdir)
			}
			cMem, _ := os.ReadFile(memPath(workdir))
			_ = os.WriteFile(memPath(workdir), append(cMem, []byte("- Headless cycle lesson during visual QA.\n")...), 0o644)
			cBl, _ := ReadJSONList(filepath.Join(workdir, BacklogFile))
			cBl = append(cBl, map[string]any{"change_name": "fix-typo", "priority": "high", "description": "Fix typo found by headless cycle"})
			_ = WriteJSONList(filepath.Join(workdir, BacklogFile), cBl)
			cProps, _ := ReadProposals(workdir)
			cProps = append(cProps, map[string]any{"change_name": "headless-prop", "priority": "normal", "description": "Headless proposal", "status": "proposed", "source": "exploration"})
			return WriteProposals(workdir, cProps)
		},
	}, &vCycleOut)
	if err != nil {
		t.Fatalf("parallel headless RunCycle during Visual QA failed: %v", err)
	}
	if !strings.Contains(vCycleOut.String(), "visual QA session "+vStamp+" in progress: leaving main checkout untouched; judging in .worktrees/_cycle") {
		t.Fatalf("expected parallel cycle to log visual QA session in progress, got:\n%s", vCycleOut.String())
	}

	// Verify root's uncommitted visual QA edits are untouched by the headless cycle
	rootBl, _ := ReadJSONList(filepath.Join(ws, BacklogFile))
	if len(rootBl) != 1 || asString(rootBl[0]["change_name"]) != "fix-button-label" {
		t.Fatalf("expected root backlog to still have only fix-button-label during visual session, got %+v", rootBl)
	}

	// 7. Close the Visual QA session with VisualQAEnd -> carries and 3-way reconciles with origin/master!
	var endOut bytes.Buffer
	if err := VisualQAEnd(ctx, ws, st, VisualQAOptions{Stamp: vStamp}, &endOut); err != nil {
		t.Fatalf("VisualQAEnd failed: %v", err)
	}
	if ActiveVisualSession(ctx, st) != "" {
		t.Fatalf("expected visual_qa lock to be released after VisualQAEnd")
	}

	// Verify item 1 is verified and unfinished item 2 returned to pending
	allItems, _ := st.ListVisualQA(ctx, "")
	itemStatus := make(map[string]string)
	for _, it := range allItems {
		itemStatus[it.ItemID] = it.Status
	}
	if itemStatus["1"] != "verified" || itemStatus[reqItem.ItemID] != "pending" {
		t.Fatalf("expected item 1=verified and item %s=pending, got %v", reqItem.ItemID, itemStatus)
	}

	// Verify origin/master has BOTH the parallel headless cycle's findings AND the visual QA session's findings!
	_, _ = gitx.RunGit(ctx, ws, "fetch", "-q", "origin")
	repo, _ := gitx.Open(ws)
	memMergedBytes, _ := repo.ReadBlob("origin/master", memRel)
	memMerged := string(memMergedBytes)
	if !strings.Contains(memMerged, "Visual QA lesson: check the save button copy.") ||
		!strings.Contains(memMerged, "Headless cycle lesson during visual QA.") {
		t.Fatalf("expected both visual and headless lessons in the memory on origin/master, got:\n%s", memMerged)
	}

	blMergedBytes, _ := repo.ReadBlob("origin/master", BacklogFile)
	blMerged := string(blMergedBytes)
	if !strings.Contains(blMerged, "fix-button-label") || !strings.Contains(blMerged, "fix-typo") {
		t.Fatalf("expected both fix-button-label and fix-typo in origin/master:%s, got:\n%s", BacklogFile, blMerged)
	}

	propsMergedBytes, _ := repo.ReadBlob("origin/master", ProposalsFile)
	propsMerged := string(propsMergedBytes)
	if !strings.Contains(propsMerged, "visual-dark-mode") || !strings.Contains(propsMerged, "headless-prop") {
		t.Fatalf("expected both visual-dark-mode and headless-prop in origin/master:%s, got:\n%s", ProposalsFile, propsMerged)
	}

	// Main checkout is clean after VisualQAEnd released carried edits
	wsDirty, _ := gitx.RunGit(ctx, ws, "status", "--porcelain")
	if strings.TrimSpace(wsDirty) != "" {
		t.Fatalf("expected clean main checkout after VisualQAEnd, got:\n%s", wsDirty)
	}

	// 8. S19: Visual session whose .worktrees/_visual_qa vanished records from main checkout and closes
	_ = SyncRepos(ctx, ws, st, false, io.Discard)
	if err := VisualQABegin(ctx, ws, st, VisualQAOptions{Stamp: "20261108T160000Z"}, io.Discard); err != nil {
		t.Fatalf("S19 VisualQABegin failed: %v", err)
	}
	_ = os.RemoveAll(filepath.Join(ws, ".worktrees", "_visual_qa"))
	_, _ = gitx.RunGit(ctx, ws, "worktree", "prune")
	wsMem, _ = os.ReadFile(memPath(ws))
	_ = os.WriteFile(memPath(ws), append(wsMem, []byte("- Visual lesson without its worktree.\n")...), 0o644)
	if err := VisualQAEnd(ctx, ws, st, VisualQAOptions{Stamp: "20261108T160000Z"}, io.Discard); err != nil {
		t.Fatalf("S19 VisualQAEnd failed: %v", err)
	}
	_, _ = gitx.RunGit(ctx, ws, "fetch", "-q", "origin")
	memS19, _ := repo.ReadBlob("origin/master", memRel)
	if !strings.Contains(string(memS19), "Visual lesson without its worktree.") {
		t.Fatalf("S19: expected visual lesson recorded from main checkout on origin")
	}

	// 9. Visual session that writes outside its scope pauses the system and records nothing
	_ = SyncRepos(ctx, ws, st, false, io.Discard)
	_ = VisualQABegin(ctx, ws, st, VisualQAOptions{Stamp: "20261108T161000Z"}, io.Discard)
	_ = os.MkdirAll(filepath.Join(ws, "openspec"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, "openspec/config.yaml"), []byte("oops: outside scope\n"), 0o644)
	if err := VisualQAEnd(ctx, ws, st, VisualQAOptions{Stamp: "20261108T161000Z"}, io.Discard); err == nil {
		t.Fatalf("expected VisualQAEnd to fail when agent wrote outside scope")
	}
	wsState, _ := st.GetWorkspaceState(ctx)
	if !wsState.Paused || ActiveVisualSession(ctx, st) != "" {
		t.Fatalf("expected workspace paused and visual_qa lock released on scope violation")
	}
}

// TestStressScenarios_G_H_P covers Scenarios G, H, and P of acceptance.sh:
//   - Scenario G: Detached cycle launch, reading judgment engine from .foreman-kit/agents.yaml,
//     prompt proposal waiting for human then applied on next cycle and recorded on origin
//   - Scenario H: Cycle running during a long dispatch judges and records on origin without starting
//     a second dispatch; approved_by: nobody queues proposals into agent_backlog.json
//   - Scenario P: Required service down stops cycle at preflight without pausing workspace,
//     and stops VisualQABegin before taking any check
func TestStressScenarios_G_H_P(t *testing.T) {
	ctx := context.Background()
	workRoot := t.TempDir()

	originBare := filepath.Join(workRoot, "umbrella.git")
	_ = os.MkdirAll(originBare, 0o755)
	_, _ = gitx.RunGit(ctx, originBare, "init", "--bare", "-b", "master")

	ws := filepath.Join(workRoot, "ws")
	if err := Init(ctx, ws, KitOptions{
		Agents: "claude,hermes",
		Roles:  map[string]string{"plan": "claude", "build": "claude", "cycle": "hermes"},
	}, io.Discard); err != nil {
		t.Fatalf("Init ws failed: %v", err)
	}
	initGitRepo(t, ws)
	_, _ = gitx.RunGit(ctx, ws, "remote", "add", "origin", originBare)
	st := openTestStore(t, ws)

	commitAll(t, ws, "initial")
	_, _ = gitx.RunGit(ctx, ws, "push", "-u", "origin", "master")

	// G1: --detach returns immediately with PID
	var detachOut bytes.Buffer
	if err := RunCycle(ctx, ws, st, CycleOptions{
		Detach: true,
		Stamp:  "20261108T170000Z",
		DetachSpawnFunc: func(ctx context.Context, root, stamp string) (int, error) {
			return 42424, nil
		},
	}, &detachOut); err != nil {
		t.Fatalf("G1 Detach RunCycle failed: %v", err)
	}
	if !strings.Contains(detachOut.String(), "pid=42424") {
		t.Fatalf("G1: expected pid=42424 in detach output, got: %s", detachOut.String())
	}

	// G2: Cycle proposes a prompt edit -> waits for human; once approved, next cycle applies and records on origin
	var g2Out bytes.Buffer
	err := RunCycle(ctx, ws, st, CycleOptions{
		Stamp: "20261108T170500Z",
		AgentExecFunc: func(ctx context.Context, workdir, pf, engine string, timeout time.Duration, out io.Writer) error {
			if engine != "hermes" {
				t.Fatalf("G2: expected cycle engine=hermes from .foreman-kit/agents.yaml, got %s", engine)
			}
			return WriteProposals(workdir, []map[string]any{
				{
					"change_name": "prompt-build-src",
					"kind":        "prompt",
					"prompt":      "build",
					"status":      "proposed",
					"source":      "repairs",
					"edits": []any{
						map[string]any{
							"old": "- Run `foreman-kit gates` before you finish and fix every failure.",
							"new": "- Run `foreman-kit gates` before you finish and fix every failure; the gates need src/ present.",
						},
					},
				},
			})
		},
	}, &g2Out)
	if err != nil {
		t.Fatalf("G2 RunCycle failed: %v", err)
	}
	_, _ = gitx.RunGit(ctx, ws, "fetch", "-q", "origin")
	repo, _ := gitx.Open(ws)
	buildBlobBefore, _ := repo.ReadBlob("origin/master", ".foreman-kit/prompts/build.md")
	if strings.Contains(string(buildBlobBefore), "the gates need src/ present") {
		t.Fatalf("G2: prompt proposal should wait for human approval before modifying build.md")
	}

	// Approve prompt-build-src and run next cycle -> applies and records on origin
	if err := ApproveProposal(ws, "prompt-build-src", io.Discard); err != nil {
		t.Fatalf("ApproveProposal failed: %v", err)
	}
	if err := RunCycle(ctx, ws, st, CycleOptions{
		Stamp: "20261108T171000Z",
		AgentExecFunc: func(ctx context.Context, workdir, pf, engine string, timeout time.Duration, out io.Writer) error {
			return nil
		},
	}, io.Discard); err != nil {
		t.Fatalf("G3 RunCycle failed: %v", err)
	}
	_, _ = gitx.RunGit(ctx, ws, "fetch", "-q", "origin")
	buildBlobAfter, _ := repo.ReadBlob("origin/master", ".foreman-kit/prompts/build.md")
	if !strings.Contains(string(buildBlobAfter), "the gates need src/ present") {
		t.Fatalf("G3: expected approved prompt proposal recorded in origin/master:.foreman-kit/prompts/build.md")
	}

	// H: Cycle during a running dispatch judges, rotates strategy, auto-approves proposals, and does NOT start a second dispatch
	cycleCfgPath := filepath.Join(ws, CycleFile)
	cycleCfgRaw, _ := os.ReadFile(cycleCfgPath)
	_ = os.WriteFile(cycleCfgPath, []byte(strings.Replace(string(cycleCfgRaw), "approved_by: evaluator", "approved_by: nobody", 1)), 0o644)
	commitAll(t, ws, "set approved_by: nobody")
	_, _ = gitx.RunGit(ctx, ws, "push", "origin", "master")

	// Simulate active dispatch holding "dispatch" lock
	_, _ = st.AcquireLock(ctx, "dispatch", store.LockOptions{
		OwnerType: "dispatch",
		PID:       os.Getpid(),
		TTL:       30 * time.Minute,
	})
	dispatchCalls := 0
	var hOut bytes.Buffer
	err = RunCycle(ctx, ws, st, CycleOptions{
		Stamp: "20261108T171500Z",
		AgentExecFunc: func(ctx context.Context, workdir, pf, engine string, timeout time.Duration, out io.Writer) error {
			pBytes, _ := os.ReadFile(pf)
			if !strings.Contains(string(pBytes), "a dispatch is running") {
				t.Fatalf("H: expected prompt to say a dispatch is running")
			}
			cBl, _ := ReadJSONList(filepath.Join(workdir, BacklogFile))
			cBl = append(cBl, map[string]any{"change_name": "fix-typo", "priority": "normal"})
			_ = WriteJSONList(filepath.Join(workdir, BacklogFile), cBl)

			cProps, _ := ReadProposals(workdir)
			cProps = append(cProps, map[string]any{
				"change_name": "add-request-counts",
				"priority":    "normal",
				"description": "Add request counts",
				"status":      "proposed",
				"source":      "exploration",
			})
			return WriteProposals(workdir, cProps)
		},
		DispatchFunc: func(ctx context.Context, root string, out io.Writer) error {
			dispatchCalls++
			return nil
		},
	}, &hOut)
	_ = st.ReleaseLock(ctx, "dispatch", os.Getpid())
	if err != nil {
		t.Fatalf("H RunCycle failed: %v", err)
	}
	if dispatchCalls != 0 || !strings.Contains(hOut.String(), "dispatch: already running") {
		t.Fatalf("H: expected second dispatch not to start, dispatchCalls=%d out=%s", dispatchCalls, hOut.String())
	}
	_, _ = gitx.RunGit(ctx, ws, "fetch", "-q", "origin")
	blHBytes, _ := repo.ReadBlob("origin/master", BacklogFile)
	if !strings.Contains(string(blHBytes), "fix-typo") || !strings.Contains(string(blHBytes), "add-request-counts") {
		t.Fatalf("H: expected both fix-typo and auto-approved add-request-counts in origin backlog:\n%s", string(blHBytes))
	}

	// P: Required service down stops cycle at preflight without pausing workspace, and stops VisualQABegin before taking any check
	var pOut bytes.Buffer
	err = RunCycle(ctx, ws, st, CycleOptions{
		Stamp: "20261108T172000Z",
		PreflightFunc: func(ctx context.Context, root string, out io.Writer) error {
			fmt.Fprintln(out, "preflight: FAIL (a service a repository requires is down: repos/backend:flag)")
			return fmt.Errorf("required service down")
		},
	}, &pOut)
	if err == nil || !strings.Contains(pOut.String(), "preflight: FAIL") {
		t.Fatalf("P: expected preflight failure to stop RunCycle, got err=%v out=%s", err, pOut.String())
	}
	wsState, _ := st.GetWorkspaceState(ctx)
	if wsState.Paused {
		t.Fatalf("P: preflight service failure must not pause workspace")
	}

	// Enqueue a visual QA item and verify failing ServicePrepareFunc stops VisualQABegin before claiming items
	_, _ = VisualQARequest(ctx, ws, st, "Check login header", "fix-typo", io.Discard)
	err = VisualQABegin(ctx, ws, st, VisualQAOptions{
		Stamp: "20261108T172500Z",
		ServicePrepareFunc: func(ctx context.Context, wt string, slot int) error {
			return fmt.Errorf("visual-qa: service.prepare of repos/backend failed")
		},
	}, io.Discard)
	if err == nil || ActiveVisualSession(ctx, st) != "" {
		t.Fatalf("P: expected failing ServicePrepareFunc to stop VisualQABegin and release lock, err=%v", err)
	}
	inProg, _ := st.ListVisualQA(ctx, "in_progress")
	if len(inProg) != 0 {
		t.Fatalf("P: expected 0 in_progress visual QA items when ServicePrepareFunc fails, got %d", len(inProg))
	}
}
