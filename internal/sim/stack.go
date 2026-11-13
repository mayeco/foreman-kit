package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/orchestrator"
	"github.com/mayeco/foreman-kit/internal/proc"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// Stack Layout in <StackDir>:
//
//	<StackDir>/bin/           -> simulated agent CLI binaries (sim-agent-clean, sim-agent-quota, etc.)
//	<StackDir>/remotes/       -> bare Git remotes (umbrella.git, backend.git)
//	<StackDir>/workspace/     -> umbrella checkout with repos/backend submodule and .foreman-kit/agents.yaml
//	<StackDir>/state/sim.db   -> SQLite state database for this simulation stack
type Stack struct {
	RootDir      string `json:"root_dir"`
	BinDir       string `json:"bin_dir"`
	RemotesDir   string `json:"remotes_dir"`
	WorkspaceDir string `json:"workspace_dir"`
	DBPath       string `json:"db_path"`
}

// ScenarioResult summarizes the verified outcome of running one simulated scenario.
type ScenarioResult struct {
	Scenario       string   `json:"scenario"`
	Passed         bool     `json:"passed"`
	DurationMS     int64    `json:"duration_ms"`
	Integrated     []string `json:"integrated,omitempty"`
	Blocked        []string `json:"blocked,omitempty"`
	AgentsUsed     []string `json:"agents_used,omitempty"`
	VerifiedChecks []string `json:"verified_checks"`
	Details        string   `json:"details,omitempty"`
}

// OpenStack returns the Stack struct for stackDir.
func OpenStack(stackDir string) (*Stack, error) {
	abs, err := filepath.Abs(stackDir)
	if err != nil {
		return nil, err
	}
	return &Stack{
		RootDir:      abs,
		BinDir:       filepath.Join(abs, "bin"),
		RemotesDir:   filepath.Join(abs, "remotes"),
		WorkspaceDir: filepath.Join(abs, "workspace"),
		DBPath:       filepath.Join(abs, "state", "sim.db"),
	}, nil
}

// InitStack creates or resets a complete repeatable simulation environment in stackDir.
func InitStack(ctx context.Context, stackDir string, out io.Writer) (*Stack, error) {
	if out == nil {
		out = io.Discard
	}
	st, err := OpenStack(stackDir)
	if err != nil {
		return nil, err
	}

	// Clean previous remotes, workspace, and state DB while recreating bin/
	_ = os.RemoveAll(st.RemotesDir)
	_ = os.RemoveAll(st.WorkspaceDir)
	_ = os.RemoveAll(filepath.Dir(st.DBPath))
	if err := os.MkdirAll(filepath.Dir(st.DBPath), 0o755); err != nil {
		return nil, err
	}

	if err := InstallSimulatedAgentBinaries(st.BinDir); err != nil {
		return nil, fmt.Errorf("install simulated agents: %w", err)
	}

	umbrellaBare := filepath.Join(st.RemotesDir, "umbrella.git")
	backendBare := filepath.Join(st.RemotesDir, "backend.git")
	if err := os.MkdirAll(st.RemotesDir, 0o755); err != nil {
		return nil, err
	}
	if _, err := gitx.RunGit(ctx, st.RemotesDir, "init", "--bare", "-b", "main", backendBare); err != nil {
		return nil, err
	}
	if _, err := gitx.RunGit(ctx, st.RemotesDir, "init", "--bare", "-b", "master", umbrellaBare); err != nil {
		return nil, err
	}

	// Seed backend repository with a real Go module, shared route file, and repo.config.yaml
	backendSeed := filepath.Join(st.RootDir, "tmp-backend-seed")
	_ = os.RemoveAll(backendSeed)
	if err := os.MkdirAll(backendSeed, 0o755); err != nil {
		return nil, err
	}
	defer os.RemoveAll(backendSeed)

	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "sim-bot"},
		{"config", "user.email", "sim-bot@example.com"},
		{"remote", "add", "origin", backendBare},
	} {
		if _, err := gitx.RunGit(ctx, backendSeed, args...); err != nil {
			return nil, err
		}
	}

	_ = os.WriteFile(filepath.Join(backendSeed, "go.mod"), []byte("module example.com/sim/backend\n\ngo 1.25\n"), 0o644)
	_ = os.WriteFile(filepath.Join(backendSeed, "routes_shared.go"), []byte("package backend\n\nvar RouteRegistry = []string{\n\t\"baseline\",\n}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(backendSeed, "routes_shared_test.go"), []byte("package backend\n\nimport \"testing\"\n\nfunc TestBaselineRoute(t *testing.T) {\n\tif len(RouteRegistry) == 0 {\n\t\tt.Fatal(\"empty registry\")\n\t}\n}\n"), 0o644)
	repoCfg := `kind: cli
base_branch: main
gates:
  lint: "go vet ./..."
  test: "go test ./..."
`
	_ = os.WriteFile(filepath.Join(backendSeed, "repo.config.yaml"), []byte(repoCfg), 0o644)
	_, _ = gitx.RunGit(ctx, backendSeed, "add", "-A")
	_, _ = gitx.RunGit(ctx, backendSeed, "commit", "-m", "chore: initial backend")
	if _, err := gitx.RunGit(ctx, backendSeed, "push", "-u", "origin", "main"); err != nil {
		return nil, err
	}

	// Initialize umbrella workspace with foreman-kit init
	if err := os.MkdirAll(st.WorkspaceDir, 0o755); err != nil {
		return nil, err
	}
	for _, args := range [][]string{
		{"init", "-b", "master"},
		{"config", "user.name", "sim-bot"},
		{"config", "user.email", "sim-bot@example.com"},
		{"config", "protocol.file.allow", "always"},
		{"remote", "add", "origin", umbrellaBare},
	} {
		if _, err := gitx.RunGit(ctx, st.WorkspaceDir, args...); err != nil {
			return nil, err
		}
	}

	if err := cycle.Init(ctx, st.WorkspaceDir, cycle.KitOptions{
		Agents: "claude,hermes",
		Roles: map[string]string{
			"plan":  "claude",
			"build": "claude",
			"cycle": "hermes",
		},
		Force: true,
	}, io.Discard); err != nil {
		return nil, fmt.Errorf("cycle.Init: %w", err)
	}

	// Configure .foreman-kit/agents.yaml with our custom fake agent binaries!
	WriteAgentsYAML(st.WorkspaceDir,
		[]string{
			"sim-agent-clean",
			"sim-agent-quota",
			"sim-agent-bad-spec",
			"sim-agent-flaky-code",
			"sim-agent-conflict",
			"sim-agent-rogue",
			"sim-agent-needs-human",
			"sim-agent-always-fail",
			"sim-agent-cycle",
			"sim-agent-router",
		},
		map[string][]string{
			"plan":  {"sim-agent-router"},
			"build": {"sim-agent-router"},
			"cycle": {"sim-agent-cycle"},
		},
		"in_order",
		map[string]string{
			"sim-agent-clean":  "--persona=clean",
			"sim-agent-router": "--mode=auto-route",
		},
	)

	// Short limits and no waits, for rapid repeatable simulation runs.
	if err := writeSimConfig(st.WorkspaceDir, simLimits{}); err != nil {
		return nil, fmt.Errorf("configure the simulation: %w", err)
	}
	_ = os.WriteFile(filepath.Join(st.WorkspaceDir, "openspec", "config.yaml"), []byte("schema: spec-driven\n"), 0o644)

	// Add repos/backend submodule
	if _, err := gitx.RunGit(ctx, st.WorkspaceDir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", backendBare, "repos/backend"); err != nil {
		return nil, fmt.Errorf("submodule add: %w", err)
	}

	_, _ = gitx.RunGit(ctx, st.WorkspaceDir, "add", "-A")
	_, _ = gitx.RunGit(ctx, st.WorkspaceDir, "commit", "-m", "chore: initialize simulation stack")
	if _, err := gitx.RunGit(ctx, st.WorkspaceDir, "push", "-u", "origin", "master"); err != nil {
		return nil, err
	}

	// Open SQLite store to initialize schema
	dbStore, err := store.OpenAt(st.DBPath, "sim-stack", st.WorkspaceDir)
	if err != nil {
		return nil, err
	}
	_ = dbStore.Close()

	fmt.Fprintf(out, "sim: initialized stack at %s\n", st.RootDir)
	fmt.Fprintf(out, "  workspace: %s\n", st.WorkspaceDir)
	fmt.Fprintf(out, "  binaries:  %s (%s)\n", st.BinDir, strings.Join(SimulatedAgents, ", "))
	fmt.Fprintf(out, "  sqlite db: %s\n", st.DBPath)
	return st, nil
}

// simLimits is what a simulation changes in the limits the kit ships, so that a run takes seconds.
type simLimits struct {
	// agentTimeout limits one PLAN or BUILD call and idleTimeout the time an agent may show no work
	// ("": two minutes and one).
	agentTimeout string
	idleTimeout  string
	// conflictRebuilds is merge.conflict_rebuilds (0: two).
	conflictRebuilds int
	// more sets other keys, each written the way `foreman-kit config set` takes it.
	more map[settings.Key]string
}

// simAgentLimits are the limits of .foreman-kit/agents.yaml a simulation runs with. WriteAgentsYAML writes
// them again after every change of agents, which rewrites that file.
var simAgentLimits = map[settings.Key]string{}

// writeSimConfig writes the configuration of a simulated workspace: .foreman-kit/pipeline.yaml with short
// limits and no waits, and the limits of an agent call in .foreman-kit/agents.yaml.
func writeSimConfig(root string, o simLimits) error {
	if o.agentTimeout == "" {
		o.agentTimeout = "2m"
	}
	if o.idleTimeout == "" {
		o.idleTimeout = "1m"
	}
	if o.conflictRebuilds == 0 {
		o.conflictRebuilds = 2
	}
	text, err := config.RenderSeed(settings.BaseBranch.File(), map[settings.Key]any{
		settings.BaseBranch:        "master",
		settings.Workers:           2,
		settings.PriorityBumpEvery: "1h",
		settings.BlockedRetries:    2,
		settings.BlockedRetryAfter: "1h",
		settings.GateTimeout:       "2m",
		settings.ConflictRebuilds:  o.conflictRebuilds,
		settings.LocksStaleAfter:   "5m",
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(settings.BaseBranch.FilePath())), text, 0o644); err != nil {
		return err
	}
	simAgentLimits = map[settings.Key]string{
		settings.RoleTimeout("plan"):  o.agentTimeout,
		settings.RoleTimeout("build"): o.agentTimeout,
		settings.IdleTimeout:          o.idleTimeout,
		settings.AgentRetries:         "0",
		settings.AgentRetryAfter:      "0s",
	}
	if err := applySimSettings(root, simAgentLimits); err != nil {
		return err
	}
	return applySimSettings(root, o.more)
}

// applySimSettings sets each key of values in the workspace at root.
func applySimSettings(root string, values map[settings.Key]string) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := config.Set(root, key, values[settings.Key(key)], false, io.Discard); err != nil {
			return fmt.Errorf("config set %s: %w", key, err)
		}
	}
	return nil
}

// WriteAgentsYAML writes .foreman-kit/agents.yaml for simulated agents: the commands that run each one,
// the agents of each role, how a call picks among them, and the arguments an agent gets in every
// role. The limits of the simulation are kept.
func WriteAgentsYAML(root string, agents []string, roles map[string][]string, selection string, args map[string]string) {
	if selection == "" {
		selection = settings.SelectionInOrder
	}
	// The file is written with what config.Write keeps as it finds it: the commands of agents the
	// kit does not know, and the arguments of each role.
	named := append([]string(nil), agents...)
	for _, role := range settings.Roles {
		named = append(named, roles[role]...)
	}
	var withArgs []string
	for name := range args {
		withArgs = append(withArgs, fmt.Sprintf("%s: %q", name, args[name]))
	}
	sort.Strings(withArgs)
	var b strings.Builder
	b.WriteString("roles:\n")
	for _, role := range settings.Roles {
		fmt.Fprintf(&b, "  %s:\n    args: {%s}\n", role, strings.Join(withArgs, ", "))
	}
	b.WriteString("commands:\n")
	seen := map[string]bool{}
	for _, name := range named {
		if seen[name] || name == "" {
			continue
		}
		seen[name] = true
		if _, known := config.SUPPORTED[name]; known {
			continue
		}
		fmt.Fprintf(&b, "  %s:\n    run: %s {{args}} -p {{prompt}}\n    restricted: %s {{args}} -p {{prompt}}\n    ping: %s {{args}} -p {{prompt}}\n", name, name, name, name)
	}
	p := filepath.Join(root, ".foreman-kit", "agents.yaml")
	_ = os.WriteFile(p, []byte(b.String()), 0o644)
	if err := config.Write(root, agents, roles, selection); err != nil {
		fmt.Fprintf(os.Stderr, "sim: .foreman-kit/agents.yaml was not written: %v\n", err)
	}
	if err := applySimSettings(root, simAgentLimits); err != nil {
		fmt.Fprintf(os.Stderr, "sim: the limits of .foreman-kit/agents.yaml were not written: %v\n", err)
	}

	lockPath := filepath.Join(root, ".foreman-kit/kit.lock")
	if rawLock, err := os.ReadFile(lockPath); err == nil {
		var lockMap map[string]any
		if json.Unmarshal(rawLock, &lockMap) == nil && lockMap != nil {
			lockMap["agents"] = agents
			if updated, err := json.MarshalIndent(lockMap, "", "  "); err == nil {
				_ = os.WriteFile(lockPath, append(updated, '\n'), 0o644)
			}
		}
	}
}

// RunScenario executes one named scenario ("happy-path", "quota-continuation", "plan-build-repairs",
// "rebase-conflict-rebuild", "guardrails-quarantine", "auto-retry-quarantine", "autonomous-cycle",
// "cycle-on-demand", "queue-full", or "all")
// against the stack in stackDir, resetting between scenarios when running "all".
func RunScenario(ctx context.Context, stackDir, scenario string, out io.Writer) ([]ScenarioResult, error) {
	if out == nil {
		out = os.Stdout
	}
	if scenario == "" || scenario == "all" || scenario == "faults" || scenario == "everything" {
		names := []string{
			"happy-path",
			"quota-continuation",
			"plan-build-repairs",
			"rebase-conflict-rebuild",
			"guardrails-quarantine",
			"auto-retry-quarantine",
			"autonomous-cycle",
			"merger-idempotent-push-and-stale-heal",
			"native-spec-validator-and-unchecked-tasks",
			"spread-circuit-breaker-and-jev-budget",
			"batch-merge-and-visual-qa-backend-filter",
			"legacy-files-migration-and-git-sync",
			"cycle-on-demand",
			"queue-full",
		}
		switch scenario {
		case "faults":
			names = FaultScenarios
		case "everything":
			names = append(names, FaultScenarios...)
		}
		var results []ScenarioResult
		for _, name := range names {
			if _, err := InitStack(ctx, stackDir, io.Discard); err != nil {
				return results, err
			}
			res, err := runSingleScenario(ctx, stackDir, name, out)
			results = append(results, res)
			if err != nil {
				return results, err
			}
		}
		return results, nil
	}

	if _, err := os.Stat(filepath.Join(stackDir, "workspace", ".git")); err != nil {
		if _, err := InitStack(ctx, stackDir, io.Discard); err != nil {
			return nil, err
		}
	}
	res, err := runSingleScenario(ctx, stackDir, scenario, out)
	return []ScenarioResult{res}, err
}

// inheritedAgentEnv lists the variables a pipeline agent receives that describe its own run; a
// simulation started from inside that agent must not see them.
var inheritedAgentEnv = []string{
	"FOREMANKIT_WORKDIR",
	"FOREMANKIT_MAIN_ROOT",
	"FOREMANKIT_CHANGE",
	"FOREMANKIT_PHASE",
	"FOREMANKIT_ROLE",
	"FOREMANKIT_ENGINE",
	"FOREMANKIT_PROMPT_FILE",
	"FOREMANKIT_SET",
}

// scrubInheritedAgentEnv unsets inheritedAgentEnv and returns the function that restores it.
func scrubInheritedAgentEnv() (restore func()) {
	inherited := map[string]string{}
	for _, k := range inheritedAgentEnv {
		if v, ok := os.LookupEnv(k); ok {
			inherited[k] = v
			_ = os.Unsetenv(k)
		}
	}
	return func() {
		for k, v := range inherited {
			_ = os.Setenv(k, v)
		}
	}
}

func runSingleScenario(ctx context.Context, stackDir, name string, out io.Writer) (ScenarioResult, error) {
	start := time.Now()
	st, err := OpenStack(stackDir)
	if err != nil {
		return ScenarioResult{Scenario: name}, err
	}

	// Put <stack>/bin first on PATH and configure SQLite DB & built-in openspec
	origPath := os.Getenv("PATH")
	origDB := os.Getenv("FOREMANKIT_DB_PATH")
	origApp := os.Getenv("FOREMANKIT_APP_ROOT")
	origGitAllow := os.Getenv("GIT_CONFIG_COUNT")
	_ = os.Setenv("PATH", st.BinDir+string(os.PathListSeparator)+origPath)
	_ = os.Setenv("FOREMANKIT_DB_PATH", st.DBPath)
	_ = os.Setenv("FOREMANKIT_APP_ROOT", st.WorkspaceDir)
	_ = os.Setenv("GIT_CONFIG_COUNT", "1")
	_ = os.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	_ = os.Setenv("GIT_CONFIG_VALUE_0", "always")
	// A simulation run from inside a pipeline agent (a build's gates) inherits that agent's
	// variables; they name the real worktree, change and phase, so the stack's agents would
	// write into the real umbrella and record the wrong change. The stack runs without them.
	defer scrubInheritedAgentEnv()()
	defer func() {
		_ = os.Setenv("PATH", origPath)
		_ = os.Setenv("FOREMANKIT_DB_PATH", origDB)
		_ = os.Setenv("FOREMANKIT_APP_ROOT", origApp)
		_ = os.Setenv("GIT_CONFIG_COUNT", origGitAllow)
	}()

	// Start an in-process mock Jev server so calibrated gates also work during simulation
	resetFaults()
	jevSrv := startSimulatedJevServer()
	defer jevSrv.Close()
	_ = os.Setenv("TYPESAFE_API_KEY", "sim-jev-key")
	// The scenario asks the stand-in evaluator: its endpoint is given to this run, and to what it
	// starts, the way --set gives a value. Whether the evaluator is on stays with the files.
	restoreEvaluator, err := settings.GiveForThisRun(map[settings.Key]string{
		settings.EvaluatorBaseURL: jevSrv.URL,
	})
	if err != nil {
		return ScenarioResult{Scenario: name}, err
	}
	defer restoreEvaluator()

	dbStore, err := store.OpenAt(st.DBPath, "sim-stack", st.WorkspaceDir)
	if err != nil {
		return ScenarioResult{Scenario: name}, err
	}
	defer dbStore.Close()

	res := ScenarioResult{Scenario: name}
	check := func(desc string, cond bool) error {
		if !cond {
			qs, _ := dbStore.ListQuarantine(ctx, "")
			evts, _ := dbStore.QueryEvents(ctx, store.EventFilter{})
			return fmt.Errorf("assertion failed: %s\nquarantine=%+v\nevents=%+v", desc, qs, evts)
		}
		res.VerifiedChecks = append(res.VerifiedChecks, desc)
		return nil
	}

	switch name {
	case "happy-path":
		WriteAgentsYAML(st.WorkspaceDir,
			[]string{"sim-agent-clean"},
			map[string][]string{
				"plan":  {"sim-agent-clean"},
				"build": {"sim-agent-clean"},
				"cycle": {"sim-agent-clean"},
			},
			"in_order",
			map[string]string{"sim-agent-clean": "--persona=clean"},
		)
		commitWorkspaceConfig(ctx, st.WorkspaceDir, "configure sim-agent-clean")

		// Verify preflight probes custom binary sim-agent-clean in .foreman-kit/agents.yaml
		pfRes, err := proc.RunPreflight(ctx, proc.PreflightOptions{
			Root:   st.WorkspaceDir,
			Store:  dbStore,
			Stdout: io.Discard,
			Stderr: io.Discard,
		})
		if err != nil {
			return res, err
		}
		if err := check("preflight pings custom binary sim-agent-clean from agents.yaml", strings.Contains(pfRes.Output, "sim-agent-clean: OK")); err != nil {
			return res, err
		}

		writeBacklog(st.WorkspaceDir, []map[string]any{
			{"change_name": "add-payments-api", "priority": "high", "description": "Add payments endpoint"},
		})
		if err := runDispatchAndMerge(ctx, st.WorkspaceDir, dbStore, 1); err != nil {
			return res, err
		}
		if err := check("add-payments-api integrated on origin/master", isIntegrated(ctx, st.WorkspaceDir, "add-payments-api")); err != nil {
			return res, err
		}
		if err := check("Go code and test committed in repos/backend", fileExists(filepath.Join(st.WorkspaceDir, "repos", "backend", "feature_add_payments_api.go"))); err != nil {
			return res, err
		}

	case "quota-continuation":
		// Configure role fallback: sim-agent-quota first (leaves partial file & hits quota), then sim-agent-clean
		WriteAgentsYAML(st.WorkspaceDir,
			[]string{"sim-agent-quota", "sim-agent-clean"},
			map[string][]string{
				"plan":  {"sim-agent-clean"},
				"build": {"sim-agent-quota", "sim-agent-clean"},
				"cycle": {"sim-agent-clean"},
			},
			"in_order",
			nil,
		)
		commitWorkspaceConfig(ctx, st.WorkspaceDir, "configure quota fallback")
		writeBacklog(st.WorkspaceDir, []map[string]any{
			{"change_name": "resume-after-quota", "priority": "high", "description": "Test quota fallback and continuation note"},
		})
		if err := runDispatchAndMerge(ctx, st.WorkspaceDir, dbStore, 1); err != nil {
			return res, err
		}
		if err := check("resume-after-quota integrated via fallback engine sim-agent-clean", isIntegrated(ctx, st.WorkspaceDir, "resume-after-quota")); err != nil {
			return res, err
		}
		evts, _ := dbStore.QueryEvents(ctx, store.EventFilter{Event: "agent.exit", ChangeName: "resume-after-quota"})
		usedClean := false
		for _, e := range evts {
			if e.Phase == "apply" && e.Engine == "sim-agent-clean" {
				usedClean = true
			}
		}
		if err := check("agent.exit recorded fallback engine sim-agent-clean after sim-agent-quota failed", usedClean); err != nil {
			return res, err
		}

	case "plan-build-repairs":
		// Configure sim-agent-bad-spec for PLAN and sim-agent-flaky-code for BUILD
		WriteAgentsYAML(st.WorkspaceDir,
			[]string{"sim-agent-bad-spec", "sim-agent-flaky-code", "sim-agent-clean"},
			map[string][]string{
				"plan":  {"sim-agent-bad-spec"},
				"build": {"sim-agent-flaky-code"},
				"cycle": {"sim-agent-clean"},
			},
			"in_order",
			nil,
		)
		commitWorkspaceConfig(ctx, st.WorkspaceDir, "configure repair personas")
		writeBacklog(st.WorkspaceDir, []map[string]any{
			{"change_name": "self-healing-feature", "priority": "high", "description": "Fails first plan and first build, then repairs both"},
		})
		if err := runDispatchAndMerge(ctx, st.WorkspaceDir, dbStore, 1); err != nil {
			return res, err
		}
		if err := check("self-healing-feature integrated after plan-repair-1 and build repair-1", isIntegrated(ctx, st.WorkspaceDir, "self-healing-feature")); err != nil {
			return res, err
		}
		repairsContent := findArchivedRepairs(st.WorkspaceDir, "self-healing-feature")
		if err := check("repairs.md archived with both plan-repair-1 and repair-1 rounds",
			strings.Contains(repairsContent, "plan-repair-1") && strings.Contains(repairsContent, "repair-1")); err != nil {
			return res, err
		}

	case "rebase-conflict-rebuild":
		// Configure sim-agent-conflict for BUILD so two parallel changes edit routes_shared.go on the same lines
		WriteAgentsYAML(st.WorkspaceDir,
			[]string{"sim-agent-clean", "sim-agent-conflict"},
			map[string][]string{
				"plan":  {"sim-agent-clean"},
				"build": {"sim-agent-conflict"},
				"cycle": {"sim-agent-clean"},
			},
			"in_order",
			nil,
		)
		commitWorkspaceConfig(ctx, st.WorkspaceDir, "configure conflict persona")
		writeBacklog(st.WorkspaceDir, []map[string]any{
			{"change_name": "conflict-alpha", "priority": "high", "description": "First route change"},
			{"change_name": "conflict-beta", "priority": "high", "description": "Second conflicting route change"},
		})
		// Round 1: both build concurrently on initial base; Merger integrates the first and sends the second back for rebuild
		if err := runDispatchAndMerge(ctx, st.WorkspaceDir, dbStore, 2); err != nil {
			return res, err
		}
		// Round 2: Dispatcher picks up rebuild_requested, rebuilds on new base, Merger integrates
		if err := runDispatchAndMerge(ctx, st.WorkspaceDir, dbStore, 2); err != nil {
			return res, err
		}
		if err := check("both conflict-alpha and conflict-beta integrated after rebase conflict rebuild",
			isIntegrated(ctx, st.WorkspaceDir, "conflict-alpha") && isIntegrated(ctx, st.WorkspaceDir, "conflict-beta")); err != nil {
			return res, err
		}
		routesBytes, _ := os.ReadFile(filepath.Join(st.WorkspaceDir, "repos", "backend", "routes_shared.go"))
		if err := check("routes_shared.go contains both conflict-alpha and conflict-beta routes",
			strings.Contains(string(routesBytes), "conflict-alpha") && strings.Contains(string(routesBytes), "conflict-beta")); err != nil {
			return res, err
		}

	case "guardrails-quarantine":
		// Use sim-agent-router so rogue-tamper, ambiguous-spec, and broken-gate each trigger their guardrail
		WriteAgentsYAML(st.WorkspaceDir,
			[]string{"sim-agent-router"},
			map[string][]string{
				"plan":  {"sim-agent-router"},
				"build": {"sim-agent-router"},
				"cycle": {"sim-agent-router"},
			},
			"in_order",
			nil,
		)
		commitWorkspaceConfig(ctx, st.WorkspaceDir, "configure router persona")
		writeBacklog(st.WorkspaceDir, []map[string]any{
			{"change_name": "rogue-tamper", "priority": "high", "description": "Attempts to edit repo.config.yaml"},
			{"change_name": "ambiguous-spec", "priority": "high", "description": "Writes .needs-human"},
			{"change_name": "broken-gate", "priority": "normal", "description": "Always fails unit test gate"},
			{"change_name": "clean-survivor", "priority": "low", "description": "Builds and merges cleanly despite sibling failures"},
		})
		if err := runDispatchAndMerge(ctx, st.WorkspaceDir, dbStore, 2); err != nil {
			return res, err
		}
		qRogue, _ := dbStore.GetQuarantine(ctx, "rogue-tamper")
		qAmb, _ := dbStore.GetQuarantine(ctx, "ambiguous-spec")
		qBrok, _ := dbStore.GetQuarantine(ctx, "broken-gate")
		if err := check("rogue-tamper quarantined as policy_violation", qRogue != nil && qRogue.Reason == "policy_violation"); err != nil {
			return res, err
		}
		if err := check("ambiguous-spec quarantined as spec_ambiguity", qAmb != nil && qAmb.Reason == "spec_ambiguity"); err != nil {
			return res, err
		}
		if err := check("broken-gate quarantined as gate_failure", qBrok != nil && qBrok.Reason == "gate_failure"); err != nil {
			return res, err
		}
		if err := check("clean-survivor still integrated cleanly while faulty changes were quarantined", isIntegrated(ctx, st.WorkspaceDir, "clean-survivor")); err != nil {
			return res, err
		}

	case "auto-retry-quarantine":
		// First run quarantines retry-me with sim-agent-always-fail; then switch build to sim-agent-clean and advance clock past blocked_retry_hours
		WriteAgentsYAML(st.WorkspaceDir,
			[]string{"sim-agent-clean", "sim-agent-always-fail"},
			map[string][]string{
				"plan":  {"sim-agent-clean"},
				"build": {"sim-agent-always-fail"},
				"cycle": {"sim-agent-clean"},
			},
			"in_order",
			nil,
		)
		commitWorkspaceConfig(ctx, st.WorkspaceDir, "configure always-fail for retry")
		writeBacklog(st.WorkspaceDir, []map[string]any{
			{"change_name": "retry-me", "priority": "high", "description": "Fails first dispatch, heals on automatic retry"},
		})
		if err := runDispatchAndMerge(ctx, st.WorkspaceDir, dbStore, 1); err != nil {
			return res, err
		}
		q1, _ := dbStore.GetQuarantine(ctx, "retry-me")
		if err := check("retry-me initially quarantined as gate_failure", q1 != nil && q1.Status == "blocked" && q1.Reason == "gate_failure"); err != nil {
			return res, err
		}
		// Switch build role to sim-agent-clean and dispatch with clock 2 hours in the future
		WriteAgentsYAML(st.WorkspaceDir,
			[]string{"sim-agent-clean"},
			map[string][]string{
				"plan":  {"sim-agent-clean"},
				"build": {"sim-agent-clean"},
				"cycle": {"sim-agent-clean"},
			},
			"in_order",
			nil,
		)
		commitWorkspaceConfig(ctx, st.WorkspaceDir, "heal build agent for retry")
		disp, err := orchestrator.NewDispatcher(orchestrator.DispatcherOptions{
			Root:       st.WorkspaceDir,
			Store:      dbStore,
			MaxWorkers: 1,
			NowFunc:    func() time.Time { return time.Now().UTC().Add(2 * time.Hour) },
		})
		if err != nil {
			return res, err
		}
		_ = disp.Run(ctx)
		_ = disp.Close()
		merger, err := orchestrator.NewMerger(orchestrator.MergerOptions{
			Root:  st.WorkspaceDir,
			Store: dbStore,
		})
		if err != nil {
			return res, err
		}
		_ = merger.Run(ctx)
		_ = merger.Close()
		if err := check("retry-me automatically retried after backoff and integrated", isIntegrated(ctx, st.WorkspaceDir, "retry-me")); err != nil {
			return res, err
		}

	case "autonomous-cycle":
		WriteAgentsYAML(st.WorkspaceDir,
			[]string{"sim-agent-clean", "sim-agent-cycle"},
			map[string][]string{
				"plan":  {"sim-agent-clean"},
				"build": {"sim-agent-clean"},
				"cycle": {"sim-agent-cycle"},
			},
			"in_order",
			nil,
		)
		commitWorkspaceConfig(ctx, st.WorkspaceDir, "configure cycle agent")
		var cycleOut bytes.Buffer
		err := cycle.RunCycle(ctx, st.WorkspaceDir, dbStore, cycle.CycleOptions{
			SkipWorktreePrep: true,
			PreflightFunc: func(ctx context.Context, r string, w io.Writer) error {
				_, err := proc.RunPreflight(ctx, proc.PreflightOptions{Root: r, Store: dbStore, Stdout: w, Stderr: w})
				return err
			},
			MergeFunc: func(ctx context.Context, r string, w io.Writer) error {
				m, err := orchestrator.NewMerger(orchestrator.MergerOptions{Root: r, Store: dbStore})
				if err != nil {
					return err
				}
				defer m.Close()
				return m.Run(ctx)
			},
			AgentExecFunc: func(ctx context.Context, workdir, pf, engine string, timeout time.Duration, w io.Writer) error {
				r, err := proc.AgentExec(ctx, proc.AgentExecOptions{
					Workdir:    workdir,
					AppRoot:    st.WorkspaceDir,
					PromptFile: pf,
					Engines:    engine,
					Role:       "cycle",
					Phase:      "cycle",
					Store:      dbStore,
					Stdout:     w,
					Stderr:     w,
				})
				if err != nil || r.ExitCode != 0 {
					return fmt.Errorf("cycle agent exited %d", r.ExitCode)
				}
				return nil
			},
			DispatchFunc: func(ctx context.Context, r string, w io.Writer) error {
				return runDispatchAndMerge(ctx, r, dbStore, 1)
			},
		}, &cycleOut)
		if err != nil {
			return res, err
		}
		if err := check("cycle ran sim-agent-cycle, approved proposal add-audit-trail, and integrated it",
			isIntegrated(ctx, st.WorkspaceDir, "add-audit-trail")); err != nil {
			return res, err
		}
		memBytes, _ := os.ReadFile(filepath.Join(st.WorkspaceDir, filepath.FromSlash(memory.PitfallsFile)))
		if err := check(memory.PitfallsFile+" written by sim-agent-cycle with agent-memory and recorded on origin/master",
			strings.Contains(string(memBytes), "Simulated cycle lesson")); err != nil {
			return res, err
		}
		// A commit of the memory is one any checkout can search without being changed by it.
		_ = memory.Normalize(ctx, st.WorkspaceDir)
		memStatus, _ := gitx.RunGit(ctx, st.WorkspaceDir, "status", "--porcelain", "--", memory.Dir)
		if err := check("the main checkout holds the recorded memory in the form agent-memory writes it (nothing to change after rebuild-index)",
			strings.TrimSpace(memStatus) == ""); err != nil {
			return res, err
		}

	case "merger-idempotent-push-and-stale-heal":
		// Verifies:
		// - Auto-healing ready_manifests.head_sha when worker branch tip advances by 1 commit
		// - Merger passing BaseRef == current origin/master SHA (never stale fork_point)
		// - When PUSHING_CHILDREN succeeds and PUSHING_ROOT fails due to a concurrent push to origin/master,
		//   retry recovers from children_pushed_json WITHOUT re-running GateRunner (gateCalls == 1)
		// - O(1) worktree and log/tmp/<change> cleanup
		WriteAgentsYAML(st.WorkspaceDir,
			[]string{"sim-agent-clean"},
			map[string][]string{
				"plan":  {"sim-agent-clean"},
				"build": {"sim-agent-clean"},
				"cycle": {"sim-agent-clean"},
			},
			"in_order",
			nil,
		)
		commitWorkspaceConfig(ctx, st.WorkspaceDir, "configure clean agent for merger resilience")
		writeBacklog(st.WorkspaceDir, []map[string]any{
			{"change_name": "merge-resilience-check", "priority": "high", "description": "Verify idempotent push, base diff, and stale heal in Merger"},
		})

		disp, err := orchestrator.NewDispatcher(orchestrator.DispatcherOptions{
			Root:       st.WorkspaceDir,
			Store:      dbStore,
			MaxWorkers: 1,
		})
		if err != nil {
			return res, err
		}
		if err := disp.Run(ctx); err != nil {
			_ = disp.Close()
			return res, err
		}
		_ = disp.Close()

		// Advance worker branch by 1 commit without updating ready_manifests.head_sha
		workerWT := filepath.Join(st.WorkspaceDir, ".worktrees", "worker-merge-resilience-check")
		_ = os.WriteFile(filepath.Join(workerWT, "openspec", "changes", "merge-resilience-check", "design.md"),
			[]byte("# Design\nAmended after ready enqueue\n"), 0o644)
		_, _ = gitx.RunGit(ctx, workerWT, "add", "openspec/changes/merge-resilience-check/design.md")
		_, _ = gitx.RunGit(ctx, workerWT, "commit", "-m", "chore(spec): amend design after ready")

		// Advance origin/master by 1 commit BEFORE Merger runs so fork_point != origin/master
		sideClone := filepath.Join(st.RootDir, "tmp-side-umbrella")
		_ = os.RemoveAll(sideClone)
		_, _ = gitx.RunGit(ctx, st.RootDir, "clone", "-b", "master", filepath.Join(st.RemotesDir, "umbrella.git"), sideClone)
		_, _ = gitx.RunGit(ctx, sideClone, "config", "user.name", "sim-bot")
		_, _ = gitx.RunGit(ctx, sideClone, "config", "user.email", "sim-bot@example.com")
		appendPitfall(sideClone, "- prior cycle note")
		_, _ = gitx.RunGit(ctx, sideClone, "add", memory.Dir)
		_, _ = gitx.RunGit(ctx, sideClone, "commit", "-m", "chore(memory): advance base before merge")
		_, _ = gitx.RunGit(ctx, sideClone, "push", "origin", "master")
		expectedBaseSHA, _ := gitx.RunGit(ctx, sideClone, "rev-parse", "HEAD")
		expectedBaseSHA = strings.TrimSpace(expectedBaseSHA)

		// Push another commit to origin/master WHILE GateRunner runs on attempt 1
		gateCalls := 0
		observedBaseRef := ""
		merger, err := orchestrator.NewMerger(orchestrator.MergerOptions{
			Root:  st.WorkspaceDir,
			Store: dbStore,
			GateRunner: func(ctx context.Context, req orchestrator.GateRunRequest) (orchestrator.GateRunResult, error) {
				gateCalls++
				observedBaseRef = req.BaseRef
				if gateCalls == 1 {
					appendPitfall(sideClone, "- concurrent cycle push during gates")
					_, _ = gitx.RunGit(ctx, sideClone, "add", memory.Dir)
					_, _ = gitx.RunGit(ctx, sideClone, "commit", "-m", "chore(memory): concurrent push during gates")
					_, _ = gitx.RunGit(ctx, sideClone, "push", "origin", "master")
				}
				return orchestrator.DefaultGateRunner(dbStore)(ctx, req)
			},
		})
		if err != nil {
			return res, err
		}
		if err := merger.Run(ctx); err != nil {
			_ = merger.Close()
			return res, err
		}
		_ = merger.Close()
		_ = os.RemoveAll(sideClone)

		if err := check("GateRunner ran only ONCE across PUSHING_ROOT non-fast-forward retry", gateCalls == 1); err != nil {
			return res, err
		}
		if err := check("GateRunner received current origin/master SHA as BaseRef", observedBaseRef == expectedBaseSHA); err != nil {
			return res, err
		}
		headEvts, _ := dbStore.QueryEvents(ctx, store.EventFilter{Event: "merge.head_updated", ChangeName: "merge-resilience-check"})
		if err := check("Merger auto-healed stale head_sha and emitted merge.head_updated", len(headEvts) == 1); err != nil {
			return res, err
		}
		if err := check("merge-resilience-check integrated on origin/master", isIntegrated(ctx, st.WorkspaceDir, "merge-resilience-check")); err != nil {
			return res, err
		}

	case "native-spec-validator-and-unchecked-tasks":
		// Verifies:
		// - sim-agent-overlong-spec writes a >500-char requirement paragraph on attempt 1; ValidateOpenSpecNative rejects it in PLAN and plan-repair-1 fixes it.
		// - sim-agent-unchecked-task leaves Line 4 (- [ ] 1.2 ...) unchecked on attempt 1; UncheckedTasksDetails reports exact line number and repair-1 checks it.
		// - gate_runs and repair_rounds tables in SQLite record the structured runs.
		WriteAgentsYAML(st.WorkspaceDir,
			[]string{"sim-agent-overlong-spec", "sim-agent-unchecked-task", "sim-agent-clean"},
			map[string][]string{
				"plan":  {"sim-agent-overlong-spec"},
				"build": {"sim-agent-unchecked-task"},
				"cycle": {"sim-agent-clean"},
			},
			"in_order",
			nil,
		)
		commitWorkspaceConfig(ctx, st.WorkspaceDir, "configure validator and task check personas")
		writeBacklog(st.WorkspaceDir, []map[string]any{
			{"change_name": "spec-and-tasks-check", "priority": "high", "description": "Verify native spec validator and unchecked task line reporting"},
		})
		if err := runDispatchAndMerge(ctx, st.WorkspaceDir, dbStore, 1); err != nil {
			return res, err
		}
		if err := check("spec-and-tasks-check integrated after native spec validation repair and unchecked task repair",
			isIntegrated(ctx, st.WorkspaceDir, "spec-and-tasks-check")); err != nil {
			return res, err
		}
		repairs, _ := dbStore.ListRepairRounds(ctx, "spec-and-tasks-check")
		hasPlanSpecInvalid := false
		hasBuildIncompleteTasks := false
		for _, r := range repairs {
			if r.AskedReason == "spec_invalid" && strings.Contains(r.AskedDetails, "exceeds 500 characters") {
				hasPlanSpecInvalid = true
			}
			if r.AskedReason == "incomplete_tasks" && strings.Contains(r.AskedDetails, "Line 4: - [ ] 1.2") {
				hasBuildIncompleteTasks = true
			}
		}
		if err := check("repair_rounds recorded >500-char requirement caught by ValidateOpenSpecNative", hasPlanSpecInvalid); err != nil {
			return res, err
		}
		if err := check("repair_rounds recorded exact unchecked task Line 4 from UncheckedTasksDetails", hasBuildIncompleteTasks); err != nil {
			return res, err
		}
		gateRuns, _ := dbStore.ListGateRuns(ctx, "spec-and-tasks-check")
		if err := check("gate_runs table recorded individual gate executions in SQLite", len(gateRuns) > 0); err != nil {
			return res, err
		}

	case "spread-circuit-breaker-and-jev-budget":
		// Verifies:
		// - selection: least_busy with engine cooldown (engines.cooldown_until) circuit breaker skips sim-agent-auth-broken and sim-agent-quota even when in_use=0.
		// - ClipState enforces DefaultMaxStateTotalBytes (60KB) and Judge.Ask auto-retries on size-question without pausing advisory gates.
		WriteAgentsYAML(st.WorkspaceDir,
			[]string{"sim-agent-auth-broken", "sim-agent-quota", "sim-agent-clean"},
			map[string][]string{
				"plan":  {"sim-agent-auth-broken", "sim-agent-quota", "sim-agent-clean"},
				"build": {"sim-agent-auth-broken", "sim-agent-quota", "sim-agent-clean"},
				"cycle": {"sim-agent-clean"},
			},
			"least_busy",
			nil,
		)
		commitWorkspaceConfig(ctx, st.WorkspaceDir, "configure spread with broken and quota engines")

		// Dispatch a change: first worker tries broken/quota engines, which record cooldowns in the engines table, and falls back to sim-agent-clean
		writeBacklog(st.WorkspaceDir, []map[string]any{
			{"change_name": "circuit-breaker-check", "priority": "high", "description": "Trigger engine cooldown (engines.cooldown_until)s and verify spread"},
		})
		// Spread breaks ties between unused engines at random: claim sim-agent-clean for the duration of
		// the dispatch so the first pick is always a broken engine and both get tried (and recorded)
		// before the fallback, instead of the scenario failing whenever sim-agent-clean is picked first.
		_ = dbStore.ClaimAgent(ctx, os.Getpid(), "sim-agent-clean", "build", "pinned-for-spread-check")
		dispatchErr := runDispatchAndMerge(ctx, st.WorkspaceDir, dbStore, 1)
		_ = dbStore.ReleaseAgent(ctx, os.Getpid())
		if dispatchErr != nil {
			return res, dispatchErr
		}
		healthMap, _ := dbStore.ListEngineHealth(ctx)
		if err := check("the engines table recorded auth_failed cooldown for sim-agent-auth-broken and quota_exceeded for sim-agent-quota",
			healthMap["sim-agent-auth-broken"].CooldownUntil > time.Now().UTC().Unix() &&
				healthMap["sim-agent-quota"].CooldownUntil > time.Now().UTC().Unix()); err != nil {
			return res, err
		}

		// Even if sim-agent-clean is currently claimed (in_use=1), ArrangeAgents in spread mode MUST still put sim-agent-clean FIRST ahead of cooldown engines!
		_ = dbStore.ClaimAgent(ctx, os.Getpid(), "sim-agent-clean", "build", "active-change")
		arranged, err := dbStore.ArrangeAgents(ctx, []string{"sim-agent-auth-broken", "sim-agent-quota", "sim-agent-clean"}, "least_busy", 0, nil)
		_ = dbStore.ReleaseAgent(ctx, os.Getpid())
		if err != nil {
			return res, err
		}
		if err := check("ArrangeAgents(spread) selects healthy sim-agent-clean first despite in_use=1, pushing cooldown engines to the end",
			len(arranged) == 3 && arranged[0] == "sim-agent-clean"); err != nil {
			return res, err
		}

		// Verify ClipState global byte budget and size-question retry
		hugeState := map[string]any{
			"cap1": strings.Repeat("A", 24000),
			"cap2": strings.Repeat("B", 24000),
			"cap3": strings.Repeat("C", 24000),
			"cap4": strings.Repeat("D", 24000),
		}
		clipped := evaluator.ClipState(hugeState, 24000)
		clippedBytes, _ := json.Marshal(clipped)
		if err := check("ClipState enforces DefaultMaxStateTotalBytes <= 60000 bytes", len(clippedBytes) <= evaluator.DefaultMaxStateTotalBytes); err != nil {
			return res, err
		}

	case "batch-merge-and-visual-qa-backend-filter":
		// Verifies:
		// - Local git-ignored config (repos/backend/config/application.yml) linked into worker worktrees
		// - Multiple ready changes integrated in a single Merger pass
		// - VisualQAEnqueueIntegrated skips backend-only commits in kind:web repos while enqueuing UI commits
		WriteAgentsYAML(st.WorkspaceDir,
			[]string{"sim-agent-clean"},
			map[string][]string{
				"plan":  {"sim-agent-clean"},
				"build": {"sim-agent-clean"},
				"cycle": {"sim-agent-clean"},
			},
			"in_order",
			nil,
		)
		commitWorkspaceConfig(ctx, st.WorkspaceDir, "configure batch merge and visual qa filter")

		// Create git-ignored config/application.yml in repos/backend
		_ = os.MkdirAll(filepath.Join(st.WorkspaceDir, "repos", "backend", "config"), 0o755)
		_ = os.WriteFile(filepath.Join(st.WorkspaceDir, "repos", "backend", "config", "application.yml"), []byte("LOCAL_SECRET: test\n"), 0o644)

		writeBacklog(st.WorkspaceDir, []map[string]any{
			{"change_name": "batch-item-one", "priority": "high", "description": "First batch item"},
			{"change_name": "batch-item-two", "priority": "normal", "description": "Second batch item"},
		})

		disp, err := orchestrator.NewDispatcher(orchestrator.DispatcherOptions{
			Root:       st.WorkspaceDir,
			Store:      dbStore,
			MaxWorkers: 2,
		})
		if err != nil {
			return res, err
		}
		if err := disp.Run(ctx); err != nil {
			_ = disp.Close()
			return res, err
		}
		_ = disp.Close()

		if err := check("local git-ignored repos/backend/config/application.yml linked into worker worktree",
			fileExists(filepath.Join(st.WorkspaceDir, ".worktrees", "worker-batch-item-one", "repos", "backend", "config", "application.yml"))); err != nil {
			return res, err
		}
		_ = os.RemoveAll(filepath.Join(st.WorkspaceDir, "repos", "backend", "config"))

		// Run Merger draining all ready changes in a single pass
		merger, err := orchestrator.NewMerger(orchestrator.MergerOptions{
			Root:  st.WorkspaceDir,
			Store: dbStore,
		})
		if err != nil {
			return res, err
		}
		if err := merger.Run(ctx); err != nil {
			_ = merger.Close()
			return res, err
		}
		_ = merger.Close()

		if err := check("both batch-item-one and batch-item-two integrated via Merger",
			isIntegrated(ctx, st.WorkspaceDir, "batch-item-one") && isIntegrated(ctx, st.WorkspaceDir, "batch-item-two")); err != nil {
			return res, err
		}

		// Set repos/backend to kind: web, create 1 backend-only change and 1 UI change, and test VisualQAEnqueueIntegrated
		_, _ = gitx.RunGit(ctx, st.WorkspaceDir, "fetch", "origin")
		_, _ = gitx.RunGit(ctx, st.WorkspaceDir, "reset", "--hard", "origin/master")
		_, _ = gitx.RunGit(ctx, st.WorkspaceDir, "submodule", "update", "--init", "--checkout")
		webRepoCfg := "kind: web\nbase_branch: main\ngates:\n  lint: \"go vet ./...\"\n  test: \"go test ./...\"\n"
		_ = os.WriteFile(filepath.Join(st.WorkspaceDir, "repos", "backend", "repo.config.yaml"), []byte(webRepoCfg), 0o644)
		_, _ = gitx.RunGit(ctx, filepath.Join(st.WorkspaceDir, "repos", "backend"), "add", "repo.config.yaml")
		_, _ = gitx.RunGit(ctx, filepath.Join(st.WorkspaceDir, "repos", "backend"), "commit", "-m", "chore: switch to kind web")
		_, _ = gitx.RunGit(ctx, filepath.Join(st.WorkspaceDir, "repos", "backend"), "push", "origin", "main")
		_, _ = gitx.RunGit(ctx, st.WorkspaceDir, "add", "repos/backend")
		_, _ = gitx.RunGit(ctx, st.WorkspaceDir, "commit", "-m", "chore: update backend submodule to kind web")
		// Record baseline SHA for VisualQAEnqueueIntegrated
		_ = cycle.VisualQAEnqueueIntegrated(ctx, st.WorkspaceDir, dbStore, io.Discard)

		// Commit 1 in backend: pure backend file (db/migrate/001.sql)
		_ = os.MkdirAll(filepath.Join(st.WorkspaceDir, "repos", "backend", "db", "migrate"), 0o755)
		_ = os.WriteFile(filepath.Join(st.WorkspaceDir, "repos", "backend", "db", "migrate", "001.sql"), []byte("SELECT 1;\n"), 0o644)
		_, _ = gitx.RunGit(ctx, filepath.Join(st.WorkspaceDir, "repos", "backend"), "add", "-A")
		_, _ = gitx.RunGit(ctx, filepath.Join(st.WorkspaceDir, "repos", "backend"), "commit", "-m", "feat: backend migration")
		_, _ = gitx.RunGit(ctx, st.WorkspaceDir, "add", "repos/backend")
		_, _ = gitx.RunGit(ctx, st.WorkspaceDir, "commit", "-m", "chore(merge): integrate pure-db-migration\n\nOpenSpec-Change: pure-db-migration")

		// Commit 2 in backend: UI view file (app/views/index.html)
		_ = os.MkdirAll(filepath.Join(st.WorkspaceDir, "repos", "backend", "app", "views"), 0o755)
		_ = os.WriteFile(filepath.Join(st.WorkspaceDir, "repos", "backend", "app", "views", "index.html"), []byte("<h1>Hello</h1>\n"), 0o644)
		_, _ = gitx.RunGit(ctx, filepath.Join(st.WorkspaceDir, "repos", "backend"), "add", "-A")
		_, _ = gitx.RunGit(ctx, filepath.Join(st.WorkspaceDir, "repos", "backend"), "commit", "-m", "feat: add view")
		_, _ = gitx.RunGit(ctx, st.WorkspaceDir, "add", "repos/backend")
		_, _ = gitx.RunGit(ctx, st.WorkspaceDir, "commit", "-m", "chore(merge): integrate ui-landing-view\n\nOpenSpec-Change: ui-landing-view")
		_, _ = gitx.RunGit(ctx, st.WorkspaceDir, "push", "origin", "master")

		_ = cycle.VisualQAEnqueueIntegrated(ctx, st.WorkspaceDir, dbStore, io.Discard)
		vqItems, _ := dbStore.ListVisualQA(ctx, "pending")
		hasDB := false
		hasUI := false
		for _, it := range vqItems {
			if it.ChangeName == "pure-db-migration" {
				hasDB = true
			}
			if it.ChangeName == "ui-landing-view" {
				hasUI = true
			}
		}
		if err := check("VisualQAEnqueueIntegrated skipped pure-db-migration and enqueued ui-landing-view", !hasDB && hasUI); err != nil {
			return res, err
		}

	case "legacy-files-migration-and-git-sync":
		// Seed legacy v3.x files in config/ and log/ and run MigrateFromFiles + SyncJSONToSQLite
		_ = cycle.WriteJSONList(filepath.Join(st.WorkspaceDir, ".foreman-kit", "agent_history.json"), []map[string]any{
			{"change_name": "legacy-integrated-1", "priority": "high", "description": "Legacy history item", "completed_at": "2026-11-01T00:00:00Z"},
		})
		_ = cycle.WriteJSONList(filepath.Join(st.WorkspaceDir, ".foreman-kit", "agent_backlog.json"), []map[string]any{
			{"change_name": "legacy-queued-1", "priority": "normal", "description": "Legacy backlog item"},
		})
		_ = cycle.WriteJSONList(filepath.Join(st.WorkspaceDir, ".foreman-kit", "agent_proposals.json"), []map[string]any{
			{"change_name": "legacy-proposal-1", "status": "proposed", "source": "exploration", "description": "Legacy proposal"},
		})
		_ = os.MkdirAll(filepath.Join(st.WorkspaceDir, "log", "blocked", "history"), 0o755)
		_ = os.WriteFile(filepath.Join(st.WorkspaceDir, "log", "blocked", "legacy-blocked-1.json"),
			[]byte(`{"change_name":"legacy-blocked-1","reason":"gate_failure","stage":"BUILD","details":"rspec failed","blocked_at":"2026-11-02T00:00:00Z"}`), 0o644)
		_ = os.WriteFile(filepath.Join(st.WorkspaceDir, "log", "events.jsonl"),
			[]byte(`{"ts":"2026-11-01T01:00:00Z","event":"agent.exit","change":"legacy-integrated-1","phase":"apply","engine":"hermes","code":0,"seconds":42}`+"\n"), 0o644)
		archRepDir := filepath.Join(st.WorkspaceDir, "openspec", "changes", "archive", "2026-11-01-legacy-integrated-1")
		_ = os.MkdirAll(archRepDir, 0o755)
		_ = os.WriteFile(filepath.Join(archRepDir, "repairs.md"),
			[]byte("## repair-1, 2026-11-01T00:30:00Z, hermes\n\nAsked to fix (gate_failure):\n```\nfail\n```\n\nResult: passed.\n"), 0o644)

		migRep, err := dbStore.MigrateFromFiles(ctx, st.WorkspaceDir, io.Discard)
		if err != nil {
			return res, err
		}
		if err := check("MigrateFromFiles imported history, backlog, proposals, blocked, events, and repairs.md into SQLite",
			migRep.HistoryItems >= 1 && migRep.BacklogItems >= 1 && migRep.Proposals >= 1 &&
				migRep.BlockedChanges >= 1 && migRep.EventsImported >= 1 && migRep.RepairRounds >= 1); err != nil {
			return res, err
		}

	case "cycle-on-demand":
		if err := runCycleOnDemand(ctx, st, dbStore, check); err != nil {
			return res, err
		}

	case "queue-full":
		if err := runQueueFull(ctx, st, dbStore, check); err != nil {
			return res, err
		}

	default:
		if !isFaultScenario(name) {
			return res, fmt.Errorf("unknown scenario %q", name)
		}
		fe := &faultEnv{ctx: ctx, st: st, db: dbStore, res: &res, check: check}
		if err := fe.run(name); err != nil {
			return res, err
		}
	}

	// What the scenario left is read back through every read the portal has.
	if err := checkPortalReads(ctx, dbStore); err != nil {
		return res, fmt.Errorf("%s: the portal's reads do not agree on what the scenario left: %w", name, err)
	}
	res.VerifiedChecks = append(res.VerifiedChecks, "the portal's reads agree on what the scenario left")

	// Collect summary from SQLite & git
	if repo, err := gitx.Open(st.WorkspaceDir); err == nil {
		if intList, err := repo.CompletedSince("", "HEAD"); err == nil {
			for _, ic := range intList {
				res.Integrated = append(res.Integrated, ic.ChangeName)
			}
		}
	}
	if qList, err := dbStore.ListQuarantine(ctx, "blocked"); err == nil {
		for _, q := range qList {
			res.Blocked = append(res.Blocked, q.ChangeName+":"+q.Reason)
		}
	}
	if evts, err := dbStore.QueryEvents(ctx, store.EventFilter{Event: "agent.exit"}); err == nil {
		seen := make(map[string]bool)
		for _, e := range evts {
			if e.Engine != "" && !seen[e.Engine] {
				seen[e.Engine] = true
				res.AgentsUsed = append(res.AgentsUsed, e.Engine)
			}
		}
	}

	res.Passed = true
	res.DurationMS = time.Since(start).Milliseconds()
	fmt.Fprintf(out, "  PASS  %-26s (%dms) integrated=%v blocked=%v agents=%v\n",
		res.Scenario, res.DurationMS, res.Integrated, res.Blocked, res.AgentsUsed)
	for _, c := range res.VerifiedChecks {
		fmt.Fprintf(out, "        ok: %s\n", c)
	}
	if res.Details != "" {
		fmt.Fprintf(out, "        notes: %s\n", res.Details)
	}
	return res, nil
}

func runDispatchAndMerge(ctx context.Context, root string, st *store.Store, workers int) error {
	disp, err := orchestrator.NewDispatcher(orchestrator.DispatcherOptions{
		Root:           root,
		Store:          st,
		MaxWorkers:     workers,
		RepairAttempts: 2,
	})
	if err != nil {
		return err
	}
	if err := disp.Run(ctx); err != nil {
		_ = disp.Close()
		return err
	}
	_ = disp.Close()

	merger, err := orchestrator.NewMerger(orchestrator.MergerOptions{
		Root:  root,
		Store: st,
	})
	if err != nil {
		return err
	}
	defer merger.Close()
	if err := merger.Run(ctx); err != nil {
		return err
	}
	_, _ = gitx.RunGit(ctx, root, "fetch", "origin")
	_, _ = gitx.RunGit(ctx, root, "reset", "--hard", "origin/master")
	_, _ = gitx.RunGit(ctx, root, "submodule", "update", "--init", "--checkout")
	return nil
}

func writeBacklog(root string, items []map[string]any) {
	b, _ := json.MarshalIndent(items, "", "  ")
	_ = os.WriteFile(filepath.Join(root, ".foreman-kit", "agent_backlog.json"), append(b, '\n'), 0o644)
	_, _ = gitx.RunGit(context.Background(), root, "add", ".foreman-kit/agent_backlog.json")
	_, _ = gitx.RunGit(context.Background(), root, "commit", "-m", "chore: update backlog")
	_, _ = gitx.RunGit(context.Background(), root, "push", "origin", "master")
}

func commitWorkspaceConfig(ctx context.Context, root, msg string) {
	_, _ = gitx.RunGit(ctx, root, "add", ".foreman-kit/agents.yaml", ".foreman-kit/pipeline.yaml", ".foreman-kit/kit.lock")
	_, _ = gitx.RunGit(ctx, root, "commit", "-m", "chore: "+msg)
	_, _ = gitx.RunGit(ctx, root, "push", "origin", "master")
}

func isIntegrated(ctx context.Context, root, change string) bool {
	_, _ = gitx.RunGit(ctx, root, "fetch", "origin")
	repo, err := gitx.Open(root)
	if err != nil {
		return false
	}
	ok, _ := repo.HasCommitWithTrailer("origin/master", change)
	if ok {
		return true
	}
	ok, _ = repo.HasCommitWithTrailer("HEAD", change)
	return ok
}

func findArchivedRepairs(root, change string) string {
	archDir := filepath.Join(root, "openspec", "changes", "archive")
	entries, _ := os.ReadDir(archDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-"+change) || e.Name() == change {
			b, err := os.ReadFile(filepath.Join(archDir, e.Name(), "repairs.md"))
			if err == nil {
				return string(b)
			}
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func startSimulatedJevServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			State     json.RawMessage `json:"state"`
			Questions map[string]struct {
				Type string `json:"type"`
			} `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if serveJevFault(w, r) {
			return
		}
		answers := make(map[string]any)
		for qID, q := range body.Questions {
			switch q.Type {
			case "choice":
				if qID == "risk" {
					answers[qID] = map[string]any{"type": "choice", "choice": "low", "confidence": 0.92}
				} else if qID == "importance" {
					answers[qID] = map[string]any{"type": "choice", "choice": simulatedLossImportance(body.State), "confidence": 0.9}
				} else {
					answers[qID] = map[string]any{"type": "choice", "choice": simulatedChoice(qID, body.State), "confidence": 0.90}
				}
			default:
				val := 0.92
				if qID == "reports_failure" || qID == "security_sensitive" || qID == "irreversible_data_change" || qID == "superseded_by_change" {
					val = 0.05
				}
				answers[qID] = map[string]any{"type": "noul", "noul": val}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "jev-1.14.0",
			"answers": answers,
		})
	}))
}

// simulatedChoice answers the routing gates' choice questions from the state, the way their criteria in
// .foreman-kit/evaluator.yaml describe (keyword stand-ins for the model's judgment).
func simulatedChoice(qID string, state json.RawMessage) string {
	var st map[string]any
	_ = json.Unmarshal(state, &st)
	field := func(k string) string {
		b, _ := json.Marshal(st[k])
		return strings.ToLower(string(b))
	}
	has := func(text string, words ...string) bool {
		for _, w := range words {
			if strings.Contains(text, w) {
				return true
			}
		}
		return false
	}
	switch qID {
	case "cause": // failure_triage
		if has(field("gate_output"), "file size limit", "no space left", "connection refused", "signal: file size", "exit status 153") {
			return "environment_or_flaky"
		}
		return "implementation_defect"
	case "scope": // system_error_triage
		e := field("error")
		switch {
		case has(e, "permission denied", "no space left", "read-only file system", "not our ref", "upload-pack"):
			return "workspace_wide"
		case has(e, "index.lock", "timed out", "connection reset", "temporarily"):
			return "transient"
		case has(e, "already exists", "not a directory", "worker-"):
			return "change_specific"
		}
		return "cannot_tell"
	case "nature": // policy_violation_triage
		o := field("outside")
		switch {
		case has(o, "escaped by", "curl ", "| sh"):
			return "tampering"
		case has(o, ".ds_store", ".log", "coverage.out", ".tmp", "__pycache__"):
			return "stray_artifact"
		}
		return "cannot_tell"
	case "outcome": // agent_outcome
		a := field("agent_output")
		switch {
		case has(a, "no space left", "read-only file system", "permission denied"):
			return "environment"
		case has(a, "cannot implement", "giving up", "i refuse", "contradicts itself"):
			return "agent_gave_up"
		case has(a, "429", "rate limit", "too many requests"):
			return "quota_or_rate_limit"
		case has(a, "401", "unauthorized"):
			return "auth"
		}
		return "provider_outage"
	case "relation": // semantic_merge_conflict
		if has(field("concurrent"), "invariant") {
			return "contradicts"
		}
		return "compatible"
	case "kind": // push_rejection
		o := field("remote_output")
		switch {
		case has(o, "non-fast-forward", "fetch first", "stale info"):
			return "non_fast_forward"
		case has(o, "hook declined", "protected", "permission to", "denied"):
			return "policy"
		}
		return "transient"
	}
	return "implementation_defect"
}

// simulatedLossImportance judges lost concurrent work the way the concurrent_work_loss criteria ask:
// by the lost commits' messages first.
func simulatedLossImportance(state json.RawMessage) string {
	var st struct {
		LostWork []struct {
			Message string `json:"message"`
		} `json:"lost_work"`
	}
	_ = json.Unmarshal(state, &st)
	verdict := "cannot_tell"
	for _, lw := range st.LostWork {
		msg := strings.ToLower(lw.Message)
		switch {
		case strings.Contains(msg, "hotfix") || strings.Contains(msg, "production") || strings.Contains(msg, "outage"):
			return "critical"
		case strings.HasPrefix(msg, "chore") || strings.HasPrefix(msg, "style") || strings.Contains(msg, "newline") || strings.Contains(msg, "whitespace"):
			if verdict == "cannot_tell" {
				verdict = "trivial"
			}
		default:
			verdict = "significant"
		}
	}
	return verdict
}

// RunSimCLI handles `foreman-kit sim <init|reset|run|status|agents> [dir] [scenario]`.
func RunSimCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: foreman-kit sim <init|reset|run|status|agents> [stack-dir] [scenario]")
		fmt.Fprintln(stderr, "Scenarios: happy-path | quota-continuation | plan-build-repairs | rebase-conflict-rebuild | guardrails-quarantine | auto-retry-quarantine | autonomous-cycle | merger-idempotent-push-and-stale-heal | native-spec-validator-and-unchecked-tasks | spread-circuit-breaker-and-jev-budget | batch-merge-and-visual-qa-backend-filter | legacy-files-migration-and-git-sync | cycle-on-demand | queue-full | all | faults | everything | <fault scenario>")
		return 2
	}

	sub := args[0]
	stackDir := "/tmp/openspec-sim-stack"
	if len(args) >= 2 && !strings.HasPrefix(args[1], "-") {
		stackDir = args[1]
	}

	switch sub {
	case "init", "reset":
		if _, err := InitStack(ctx, stackDir, stdout); err != nil {
			fmt.Fprintf(stderr, "sim %s: %v\n", sub, err)
			return 1
		}
		return 0

	case "agents":
		for _, a := range SimulatedAgents {
			fmt.Fprintln(stdout, a)
		}
		return 0

	case "run":
		scenario := "all"
		if len(args) >= 3 {
			scenario = args[2]
		}
		fmt.Fprintf(stdout, "== Running simulation stack at %s (scenario: %s)\n", stackDir, scenario)
		results, err := RunScenario(ctx, stackDir, scenario, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "sim run: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "== Simulation complete: %d/%d scenario(s) PASSED\n", len(results), len(results))
		return 0

	case "status":
		st, err := OpenStack(stackDir)
		if err != nil {
			fmt.Fprintf(stderr, "sim status: %v\n", err)
			return 1
		}
		dbStore, err := store.OpenAt(st.DBPath, "sim-stack", st.WorkspaceDir)
		if err != nil {
			fmt.Fprintf(stderr, "sim status: %v\n", err)
			return 1
		}
		defer dbStore.Close()
		_ = config.AppRoot(st.WorkspaceDir)
		_ = cycle.Status(ctx, st.WorkspaceDir, dbStore, stdout)
		return 0

	default:
		fmt.Fprintf(stderr, "Unknown sim subcommand %q (expected init, reset, run, status, agents)\n", sub)
		return 2
	}
}

// appendPitfall adds one line to the lessons of the memory in the checkout at dir, the way another
// session's record would have: it stands for a commit somebody else pushed.
func appendPitfall(dir, line string) {
	p := filepath.Join(dir, filepath.FromSlash(memory.PitfallsFile))
	raw, _ := os.ReadFile(p)
	text := string(raw)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(text+line+"\n"), 0o644)
}
