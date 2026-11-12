package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// configFile returns the relative path of the configuration file called section.
func configFile(section string) string {
	f, _ := settings.FileNamed(section)
	return f.Path
}

// configSections returns the names of the configuration files, in display order.
func configSections() []string {
	var names []string
	for _, f := range settings.Files() {
		names = append(names, f.Name)
	}
	return names
}

// quietReader reads the settings of root from its files alone, without the notes a process prints
// about values it cannot use: show and check report those themselves.
func quietReader(root string) *settings.Reader {
	return &settings.Reader{Root: root, FilesAlone: true, Stderr: io.Discard}
}

// ScopeWritePaths lists paths that the cycle's judgment and visual session may write.
var ScopeWritePaths = []string{
	memory.Dir,
	".foreman-kit/agent_backlog.json",
	".foreman-kit/agent_history.json",
	".foreman-kit/agent_proposals.json",
}

var (
	changeNameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	plainScalarRe = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)
)

var (
	ExplorationKinds       = []string{"defects", "improvements", "features"}
	BacklogPriorities      = []string{"critical", "high", "normal", "low"}
	ProposalStatuses       = []string{"proposed", "approved", "queued", "integrated", "rejected"}
	StandardGateOrder      = []string{"lint", "typecheck", "test", "build"}
	PromptRoleDescriptions = map[string]string{
		"plan":  "the PLAN agent: writes the OpenSpec change of each backlog item (foreman-kit dispatch)",
		"build": "the BUILD agent: implements its tasks in the child repositories (foreman-kit dispatch)",
		"cycle": "the cycle's judgment; foreman-kit cycle appends \"This cycle\" (foreman-kit cycle)",
	}
)

// FileProblems holds errors and warnings for one checked file or section.
type FileProblems struct {
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// loadConfigYAML returns the contents of the configuration file called section, whether the file
// exists, and why it cannot be read.
func loadConfigYAML(root, section string) (map[string]any, bool, error) {
	if _, ok := settings.FileNamed(section); !ok {
		return nil, false, nil
	}
	data, present, err := quietReader(root).Data(section)
	if !present {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	return data, true, nil
}

func childPathsInRoot(root string) []string {
	if repo, err := gitx.Open(root); err == nil {
		if paths, err := repo.ChildPaths(); err == nil {
			return paths
		}
	}
	// Fallback to git config if not a full git repo
	gmPath := filepath.Join(root, ".gitmodules")
	if !fileExists(gmPath) {
		return []string{}
	}
	out, err := exec.Command("git", "-C", root, "config", "-f", ".gitmodules", "--get-regexp", `^submodule\..*\.path$`).Output()
	if err != nil {
		return []string{}
	}
	var list []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[1], "repos/") {
			list = append(list, fields[1])
		}
	}
	sort.Strings(list)
	return list
}

func hasPlaceholder(val any) bool {
	if val == nil {
		return false
	}
	return strings.Contains(fmt.Sprint(val), "{{")
}

// ==============================================================================
// Show
// ==============================================================================

// Show returns the structured configuration view for topic ("" for every section, the name of one
// configuration file, or "repos", "prompts" or "env").
func Show(root, topic string) (map[string]any, error) {
	if root == "" {
		root = AppRoot("")
	}
	validTopics := map[string]bool{"": true, "repos": true, "prompts": true, "env": true}
	for _, section := range configSections() {
		validTopics[section] = true
	}
	if !validTopics[topic] {
		return nil, fmt.Errorf("unknown topic %q", topic)
	}

	// The values come from the files; what --set gave this run shows as what replaces them here.
	r := &settings.Reader{Root: root, Stderr: io.Discard}
	out := make(map[string]any)
	for _, f := range settings.Files() {
		section := f.Name
		if topic != "" && topic != section {
			continue
		}
		_, present, _ := r.Data(section)
		var settingsList []map[string]any
		for _, s := range settings.In(section) {
			settingsList = append(settingsList, describeSetting(r.Describe(s.Key)))
		}
		secMap := map[string]any{
			"file":     f.Path,
			"about":    f.About,
			"read_by":  f.ReadBy,
			"present":  present,
			"settings": settingsList,
		}
		if section == settings.ExplorationSelection.File() {
			for k, v := range strategiesView(root, quietReader(root)) {
				secMap[k] = v
			}
		}
		if section == settings.EvaluatorFile {
			cliInstalled := "not installed"
			if _, err := exec.LookPath("jev"); err == nil || evaluator.ResolveAPIKey() != "" {
				cliInstalled = "installed"
			}
			secMap["jev_cli"] = cliInstalled
			secMap["off_for_this_run"] = settings.EvaluatorOffForThisRun()
		}
		out[section] = secMap
	}

	if topic == "" || topic == "repos" {
		children := childPathsInRoot(root)
		repos := make([]map[string]any, 0, len(children))
		for _, rel := range children {
			repos = append(repos, repoView(root, rel))
		}
		out["repos"] = repos
	}
	if topic == "" || topic == "prompts" {
		out["prompts"] = promptsView(root)
	}
	if topic == "" || topic == "env" {
		out["env"] = envView()
	}
	return out, nil
}

// describeSetting gives a setting the shape `foreman-kit config show --json` prints.
func describeSetting(d settings.Described) map[string]any {
	var valuesVal, changeVal, overridden, problem any
	if len(d.Values) > 0 {
		valuesVal = d.Values
	}
	if d.Problem != "" {
		problem = d.Problem
	}
	if d.Change != "" {
		changeVal = d.Change
	}
	if d.OverriddenBy != "" {
		overridden = d.OverriddenBy
	}
	return map[string]any{
		"key":           string(d.Key),
		"file":          d.File,
		"value":         d.Value,
		"default":       d.Default,
		"set":           d.Set,
		"problem":       problem,
		"values":        valuesVal,
		"means":         d.Means,
		"read_by":       d.ReadBy,
		"change":        changeVal,
		"overridden_by": overridden,
		"kind":          string(d.Kind),
		"accepts":       d.Accepts,
		"in_effect":     d.InEffect,
	}
}

// StrategySession returns the session that runs strategy s of .foreman-kit/exploration.yaml: the cycle or
// the visual session. A strategy that does not say (a file written before the key) runs in the cycle.
func StrategySession(s map[string]any) string {
	if session, _ := s["session"].(string); session == settings.SessionVisualQA {
		return settings.SessionVisualQA
	}
	return settings.SessionCycle
}

func strategiesView(root string, r *settings.Reader) map[string]any {
	expData, _, _ := r.Data(settings.ExplorationStrategies.File())
	var rawList []map[string]any
	if expData != nil {
		if arr, ok := expData["strategies"].([]any); ok {
			for _, it := range arr {
				if m, ok := it.(map[string]any); ok {
					rawList = append(rawList, m)
				}
			}
		}
	}

	visualEnabled := r.Bool(settings.VisualQAEnabled)
	pools := map[string][]map[string]any{}
	for _, s := range rawList {
		if slug, _ := s["slug"].(string); slug != "" && s["enabled"] != false {
			session := StrategySession(s)
			pools[session] = append(pools[session], s)
		}
	}

	selection := r.String(settings.ExplorationSelection)
	nextOf := func(pool []map[string]any, stateFile string) any {
		if len(pool) == 0 {
			return nil
		}
		if selection == settings.ExplorationRandom {
			return settings.ExplorationRandom
		}
		idx := 0
		if b, err := os.ReadFile(store.RuntimePath(root, "cycle", stateFile)); err == nil {
			var st map[string]any
			if json.Unmarshal(b, &st) == nil {
				if n, ok := toInt(st["next"]); ok && n >= 0 {
					idx = n
				}
			}
		}
		return pool[idx%len(pool)]["slug"]
	}

	var nextVisual any
	if visualEnabled {
		nextVisual = nextOf(pools[settings.SessionVisualQA], "visual_strategy_state.json")
	}

	strategies := make([]map[string]any, 0, len(rawList))
	for _, s := range rawList {
		en := s["enabled"] != false
		session := StrategySession(s)
		runsIn := "the cycle"
		switch {
		case !en:
			runsIn = "nowhere (disabled)"
		case session == settings.SessionVisualQA && visualEnabled:
			runsIn = "the visual session"
		case session == settings.SessionVisualQA:
			runsIn = "nowhere (the visual session is off)"
		}
		strategies = append(strategies, map[string]any{
			"slug":    s["slug"],
			"name":    s["name"],
			"kind":    s["kind"],
			"session": session,
			"enabled": en,
			"runs_in": runsIn,
		})
	}

	return map[string]any{
		"next_cycle":  nextOf(pools[settings.SessionCycle], "strategy_state.json"),
		"next_visual": nextVisual,
		"strategies":  strategies,
	}
}

func repoView(root, rel string) map[string]any {
	dir := filepath.Join(root, filepath.FromSlash(rel))
	cfgFile := filepath.Join(dir, "repo.config.yaml")
	if !fileExists(cfgFile) {
		return map[string]any{
			"repo":        rel,
			"config":      nil,
			"kind":        nil,
			"base_branch": nil,
			"env_script":  "",
			"dev_command": nil,
			"gates":       map[string]string{},
			"regression":  map[string]string{},
		}
	}
	raw, err := os.ReadFile(cfgFile)
	if err != nil {
		return map[string]any{"repo": rel, "error": err.Error()}
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return map[string]any{"repo": rel, "error": "repo.config.yaml is not valid YAML: " + err.Error()}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}

	var baseBranch any
	if b, ok := cfg["base_branch"].(string); ok && b != "" && !hasPlaceholder(b) {
		baseBranch = b
	}
	envScript := ""
	if es, ok := cfg["env_script"].(string); ok {
		envScript = strings.TrimSpace(es)
	}
	gatesOut := make(map[string]string)
	if gMap, ok := cfg["gates"].(map[string]any); ok {
		for k, v := range gMap {
			cs := strings.TrimSpace(fmt.Sprint(v))
			if v == nil || cs == "" || hasPlaceholder(cs) {
				gatesOut[k] = "skipped"
			} else {
				gatesOut[k] = cs
			}
		}
	}
	regOut := make(map[string]string)
	if rMap, ok := digMap(cfg, "regression", "commands").(map[string]any); ok {
		for k, v := range rMap {
			cs := strings.TrimSpace(fmt.Sprint(v))
			if v != nil && cs != "" && !hasPlaceholder(cs) {
				regOut[k] = cs
			}
		}
	}
	var setup any
	if cs, ok := cfg["setup"].(string); ok && strings.TrimSpace(cs) != "" && !hasPlaceholder(cs) {
		setup = strings.TrimSpace(cs)
	}

	out := map[string]any{
		"repo":        rel,
		"config":      RepoConfigFile,
		"kind":        cfg["kind"],
		"base_branch": baseBranch,
		"env_script":  envScript,
		"dev_command": digMap(cfg, "service", "dev_command"),
		"setup":       setup,
		"gates":       gatesOut,
		"regression":  regOut,
	}
	// A file an older foreman-kit wrote: its keys are not read, and what is shown is not what runs.
	if older := RepoOfAnOlderKit(cfg); older != "" {
		out["error"] = RepoConfigFile + " " + older
	}
	return out
}

func promptsView(root string) []map[string]any {
	var lock map[string]any
	if b, err := os.ReadFile(filepath.Join(root, ".foreman-kit/kit.lock")); err == nil {
		_ = json.Unmarshal(b, &lock)
	}
	var out []map[string]any
	for _, name := range RolesOrder {
		rel := fmt.Sprintf("%s/%s.md", PromptsDir, name)
		p := filepath.Join(root, filepath.FromSlash(rel))
		present := fileExists(p)
		phs := []string{}
		if present {
			phs, _ = Placeholders(root, name)
		}
		edited := false
		if present && lock != nil {
			if lockSha, ok := digMap(lock, "files", rel, "sha256").(string); ok && lockSha != "" {
				if raw, err := os.ReadFile(p); err == nil {
					sum := sha256.Sum256(raw)
					edited = hex.EncodeToString(sum[:]) != lockSha
				}
			}
		}
		out = append(out, map[string]any{
			"file":         rel,
			"for":          PromptRoleDescriptions[name],
			"present":      present,
			"placeholders": phs,
			"edited":       edited,
		})
	}
	return out
}

// environmentVariable is one line of what the environment says to foreman-kit, or of what
// foreman-kit says through it to what it runs.
type environmentVariable struct {
	names  string // separated by ", "
	def    any
	means  string
	secret bool
	// Each line says what foreman-kit does with its variables, and at least one of the three:
	// read, it reads them from the environment it is started in; handed, it sets them for what it
	// runs; removed, it takes them out of the environment of what it runs.
	read, handed, removed bool
}

// environmentVariables is everything the environment says to foreman-kit, none of which changes a
// setting: the secrets, where things are on this machine, and what the kit hands to what it runs.
// A test reads the sources of the kit against it: a variable the code reads or hands and no line
// names fails it, and so does a line naming a variable the code no longer reads or hands.
func environmentVariables() []environmentVariable {
	return []environmentVariable{
		{names: "TYPESAFE_API_KEY", means: "the key of the evaluator", secret: true, read: true},
		{names: "FOREMANKIT_DIAGNOSTICS_API_KEY, OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY, GOOGLE_API_KEY, OPENROUTER_API_KEY",
			means: "the key of the diagnostics assistant: the kit's own name for it, or the provider's", secret: true, read: true},
		{names: "FOREMANKIT_PORTAL_TOKEN", means: "the token of the Web Portal's API, to call it from outside this machine", secret: true, read: true},
		{names: "FOREMANKIT_HOME", def: "~/.foreman-kit", means: "where the state of the applications of this machine lives", read: true},
		{names: "FOREMANKIT_STATE_DIR, FOREMANKIT_DB_PATH", means: "the directory of that state in place of $FOREMANKIT_HOME/state, and the database of this application in place of the one found there", read: true},
		{names: "FOREMANKIT_APP_ROOT", means: "the application a command works on, whatever directory it runs in (foreman-kit <directory> <command> says the same); the kit sets it for each agent it runs, to the worktree of the agent", read: true, handed: true},
		{names: "FOREMANKIT_PATH_PREFIX", def: "$HOME/.local/share/mise/shims:$HOME/.local/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/opt/homebrew/bin",
			means: "prepended to PATH for what the kit runs: where the tools are on this machine", read: true},
		{names: "FOREMANKIT_SSH_CONFIG, FOREMANKIT_SANITIZE_GIT_ENV", means: "the SSH configuration to read in place of ~/.ssh/config, and 1 to keep the kit from looking for an SSH agent when SSH_AUTH_SOCK is not set", read: true},
		{names: settings.RunOverridesEnv, means: "what foreman-kit --set gave this run, for the processes the run starts: --set writes it, nobody exports it, and a process that finds in it what --set does not write does not start", read: true, handed: true},
		{names: "FOREMANKIT_CHANGE, FOREMANKIT_PHASE, FOREMANKIT_SLOT", means: "set by the kit for the agents and the gates it runs, and read back by the commands they run: the change and the phase in work, and the slot of the worker", read: true, handed: true},
		{names: "FOREMANKIT_ROLE, FOREMANKIT_ENGINE, FOREMANKIT_PROMPT_FILE, FOREMANKIT_WORKDIR, FOREMANKIT_MAIN_ROOT, TMPDIR",
			means: "set by the kit for each agent it runs: its role, the agent it is, the file of its prompt, its worktree, the main checkout, and for the agents of a worker a scratch directory of their own", handed: true},
		{names: "TEST_ENV_NUMBER, PORT, FOREMANKIT_CHANGED_FILES, CI, NONINTERACTIVE, GIT_TERMINAL_PROMPT, GIT_MERGE_AUTOEDIT, LANG, LC_ALL",
			means: "set by the kit for the gates and the commands of a repository: the number and the port of the slot (3000 plus 10 for each slot), the files the change touched, and what keeps a command from waiting for a person or writing in another encoding", handed: true},
		{names: "OPENSPEC_SLOT, OPENSPEC_CHANGED_FILES, OPENSPEC_MAIN_ROOT",
			means: "the same as FOREMANKIT_SLOT, FOREMANKIT_CHANGED_FILES and FOREMANKIT_MAIN_ROOT, under the names the kit gave them before it was called Foreman, for the scripts of a repository written then. Any OPENSPEC_<name> set in the environment is read as FOREMANKIT_<name> when that one is not set", handed: true},
		{names: "CYCLE_STAMP", means: "the name of a cycle, set by the kit for the process that runs it", read: true, handed: true},
		{names: "SSH_AUTH_SOCK, GH_TOKEN, GITHUB_TOKEN, GITLAB_TOKEN, GIT_ASKPASS", means: "taken out of the environment of the agents a worker runs: what reaches a remote is the merger's to push", secret: true, removed: true},
		{names: "TERM, COLORTERM, NO_COLOR", means: "how the banner of foreman-kit alone is drawn on a terminal: TERM=dumb leaves it out, NO_COLOR set draws it without color, COLORTERM=truecolor or 24bit gives it 24-bit color", read: true},
		{names: "AGENT_STUB, FOREMANKIT_FORCE_BUILTIN, FOREMANKIT_SIM_STRICT",
			means: "for tests and simulations: the script the agent named stub runs, 1 to check a change without the openspec CLI, and 1 to make foreman-kit sim fail on a known gap", read: true},
	}
}

// envView lists what the environment still says to foreman-kit (environmentVariables), with what
// of it is set now: the variables the kit reads or takes out, since a variable it only hands is
// written over for what it runs and says nothing here. A variable an older foreman-kit read, when
// it is set, is listed with what does its job now.
func envView() []map[string]any {
	variables := environmentVariables()
	out := make([]map[string]any, 0, len(variables))
	for _, v := range variables {
		setNow := []string{}
		if v.read || v.removed {
			for _, name := range strings.Split(v.names, ", ") {
				val, ok := os.LookupEnv(name)
				if !ok || val == "" {
					continue
				}
				if v.secret {
					setNow = append(setNow, name+" is set")
				} else {
					setNow = append(setNow, fmt.Sprintf("%s=%s", name, val))
				}
			}
		}
		out = append(out, map[string]any{
			"variables": v.names,
			"default":   v.def,
			"means":     v.means,
			"set_now":   setNow,
		})
	}
	for _, v := range settings.FormerVariables() {
		val := strings.TrimSpace(os.Getenv(v.Name))
		if val == "" {
			continue
		}
		setNow := fmt.Sprintf("%s=%s", v.Name, val)
		if v.Secret {
			setNow = v.Name + " is set"
		}
		out = append(out, map[string]any{
			"variables": v.Name,
			"default":   nil,
			"means":     "no longer read: " + v.Instead(val),
			"set_now":   []string{setNow},
		})
	}
	return out
}

func formatConfigValue(value any) string {
	if value == nil {
		return "(none)"
	}
	switch v := value.(type) {
	case string:
		if v == "" {
			return `""`
		}
		return v
	case []string:
		return "[" + strings.Join(v, ", ") + "]"
	case []any:
		if len(v) > 0 {
			allHash := true
			allStr := true
			var strs []string
			for _, item := range v {
				if _, ok := item.(map[string]any); !ok {
					allHash = false
				}
				if s, ok := item.(string); ok {
					strs = append(strs, s)
				} else {
					allStr = false
				}
			}
			if allHash {
				return fmt.Sprintf("%d item(s)", len(v))
			}
			if allStr {
				return "[" + strings.Join(strs, ", ") + "]"
			}
		}
		b, _ := json.Marshal(v)
		return string(b)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// FormatShowText formats the output of Show into human-readable text matching foreman-kit config show.
func FormatShowText(view map[string]any) string {
	var lines []string
	for _, file := range configSections() {
		v, ok := view[file].(map[string]any)
		if !ok {
			continue
		}
		present, _ := v["present"].(bool)
		missingTag := ""
		if !present {
			missingTag = " (MISSING)"
		}
		lines = append(lines, fmt.Sprintf("== %v: %v; read by %v%s", v["file"], v["about"], v["read_by"], missingTag))
		if settings, ok := v["settings"].([]map[string]any); ok {
			for _, s := range settings {
				name := strings.TrimPrefix(fmt.Sprint(s["key"]), file+".")
				choices := ""
				if vals, ok := s["values"].([]string); ok && len(vals) > 0 {
					choices = fmt.Sprintf("  [%s]", strings.Join(vals, " | "))
				}
				override := ""
				if ov, ok := s["overridden_by"].(string); ok && ov != "" {
					override = fmt.Sprintf("  (%s replaces it for this run)", ov)
				}
				defTag := ""
				if isSet, _ := s["set"].(bool); !isSet {
					defTag = fmt.Sprintf(" (MISSING; the kit ships %s)", formatConfigValue(s["default"]))
				} else if problem, _ := s["problem"].(string); problem != "" {
					defTag = fmt.Sprintf(" (INVALID: %s)", problem)
				}
				lines = append(lines, fmt.Sprintf("  %s: %s%s%s%s", name, formatConfigValue(s["value"]), defTag, choices, override))
				lines = append(lines, fmt.Sprintf("      %v; read by %v", s["means"], s["read_by"]))
				if chg, ok := s["change"].(string); ok && chg != "" {
					lines = append(lines, fmt.Sprintf("      change: %s", chg))
				}
			}
		}
		if file == "exploration" {
			nc := v["next_cycle"]
			if nc == nil {
				nc = "none"
			}
			nv := v["next_visual"]
			if nv == nil {
				nv = "none"
			}
			lines = append(lines, fmt.Sprintf("  next strategy: cycle %v, visual session %v", nc, nv))
			if strats, ok := v["strategies"].([]map[string]any); ok {
				width := 0
				for _, st := range strats {
					if l := len(fmt.Sprint(st["slug"])); l > width {
						width = l
					}
				}
				for _, st := range strats {
					lines = append(lines, fmt.Sprintf("    %-*s  %-12s  %v: %v",
						width, fmt.Sprint(st["slug"]), fmt.Sprint(st["kind"]), st["runs_in"], st["name"]))
				}
			}
		}
	}
	if evaluatorSec, ok := view[settings.EvaluatorFile].(map[string]any); ok {
		disTag := ""
		if off, _ := evaluatorSec["off_for_this_run"].(bool); off {
			disTag = fmt.Sprintf("; --set %s=false: every gate off for this run", settings.EvaluatorEnabled)
		}
		lines = append(lines, fmt.Sprintf("  jev CLI: %v%s", evaluatorSec["jev_cli"], disTag))
	}
	if repos, ok := view["repos"].([]map[string]any); ok {
		lines = append(lines, "== repos/<name>/repo.config.yaml: each child repository, in its own repository")
		if len(repos) == 0 {
			lines = append(lines, "  no child repository yet (the foreman-kit-init skill adds them)")
		}
		for _, r := range repos {
			if errStr, ok := r["error"].(string); ok && errStr != "" {
				lines = append(lines, fmt.Sprintf("  %v: %s", r["repo"], errStr))
			} else if r["config"] == nil {
				lines = append(lines, fmt.Sprintf("  %v: no repo.config.yaml (no gates; base branch from .gitmodules or main)", r["repo"]))
			} else {
				kindStr := "kind unset"
				if k, ok := r["kind"].(string); ok && k != "" {
					kindStr = k
				}
				baseStr := "(from .gitmodules or main)"
				if b, ok := r["base_branch"].(string); ok && b != "" {
					baseStr = b
				}
				esStr := "none"
				if es, ok := r["env_script"].(string); ok && es != "" {
					esStr = es
				}
				var gateParts []string
				if gMap, ok := r["gates"].(map[string]string); ok {
					var gKeys []string
					for k := range gMap {
						gKeys = append(gKeys, k)
					}
					sort.Strings(gKeys)
					for _, k := range gKeys {
						if gMap[k] == "skipped" {
							gateParts = append(gateParts, k+" (skipped)")
						} else {
							gateParts = append(gateParts, k)
						}
					}
				}
				gatesDesc := "none"
				if len(gateParts) > 0 {
					gatesDesc = strings.Join(gateParts, ", ")
				}
				regDesc := "none"
				if rMap, ok := r["regression"].(map[string]string); ok && len(rMap) > 0 {
					var rKeys []string
					for k := range rMap {
						rKeys = append(rKeys, k)
					}
					sort.Strings(rKeys)
					regDesc = strings.Join(rKeys, ", ")
				}
				setupDesc := "none"
				if cs, ok := r["setup"].(string); ok && cs != "" {
					setupDesc = "set"
				}
				lines = append(lines, fmt.Sprintf("  %v (%s): base %s, env_script %s, dev %s, setup %s, gates %s, regression %s",
					r["repo"], kindStr, baseStr, esStr, formatConfigValue(r["dev_command"]), setupDesc, gatesDesc, regDesc))
			}
		}
	}
	if prompts, ok := view["prompts"].([]map[string]any); ok {
		lines = append(lines, fmt.Sprintf("== %s/: the agents' prompts, this application's own (edit, then commit)", PromptsDir))
		for _, p := range prompts {
			present, _ := p["present"].(bool)
			phs, _ := p["placeholders"].([]string)
			uses := "MISSING"
			if present {
				if len(phs) == 0 {
					uses = "no placeholders"
				} else {
					var wrapped []string
					for _, ph := range phs {
						wrapped = append(wrapped, "{{"+ph+"}}")
					}
					uses = strings.Join(wrapped, ", ")
				}
			}
			edTag := ""
			if ed, _ := p["edited"].(bool); ed {
				edTag = " (edited)"
			}
			lines = append(lines, fmt.Sprintf("  %v: %s%s", p["file"], uses, edTag))
			lines = append(lines, fmt.Sprintf("    %v", p["for"]))
		}
	}
	if envs, ok := view["env"].([]map[string]any); ok {
		if given := settings.RunOverrides(); len(given) > 0 {
			lines = append(lines, "== This run: --set "+strings.Join(given, " --set "))
		}
		lines = append(lines, "== Environment: secrets, this machine, and what the kit hands to what it runs. No variable changes a setting: a value for one run is --set <file>.<key>=<value>")
		for _, e := range envs {
			defStr := ""
			if e["default"] != nil {
				defStr = fmt.Sprintf(" (default %v)", e["default"])
			}
			nowStr := ""
			if sn, ok := e["set_now"].([]string); ok && len(sn) > 0 {
				nowStr = "  NOW: " + strings.Join(sn, " ")
			}
			lines = append(lines, fmt.Sprintf("  %v%s: %v%s", e["variables"], defStr, e["means"], nowStr))
		}
	}
	return strings.Join(lines, "\n")
}

// ==============================================================================
// Check
// ==============================================================================

// checkArgsValue reports why the args of one agent (a string or a list) are invalid, or "".
func checkArgsValue(label string, args any) string {
	switch av := args.(type) {
	case []any:
		for _, item := range av {
			switch item.(type) {
			case string, int, int64, float64:
			default:
				return fmt.Sprintf("%s: must be a string or a list of strings", label)
			}
		}
	case []string:
	case string:
		if _, err := SplitShellwords(av); err != nil {
			return fmt.Sprintf("%s: cannot be split as a shell would (%v)", label, err)
		}
	default:
		return fmt.Sprintf("%s: must be a string or a list of strings", label)
	}
	return ""
}

// CommandPlaceholders lists what a command of .foreman-kit/agents.yaml may say in double braces.
var CommandPlaceholders = []string{"prompt", "prompt_file", "workdir", "timeout", "args"}

// commandPlaceholderRe matches a placeholder of a command.
var commandPlaceholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]*)\s*\}\}`)

// checkCommand reports why one command of an agent is invalid, or "".
func checkCommand(label string, command any) string {
	words, err := CommandWords(command)
	if err != nil {
		return fmt.Sprintf("%s: cannot be split as a shell would (%v)", label, err)
	}
	if len(words) == 0 || words[0] == "" {
		return fmt.Sprintf("%s: is empty", label)
	}
	for _, word := range words {
		for _, m := range commandPlaceholderRe.FindAllStringSubmatch(word, -1) {
			if !slices.Contains(CommandPlaceholders, m[1]) {
				return fmt.Sprintf("%s: {{%s}} is not a placeholder (%s); what it stood for is written in the command itself", label, m[1], strings.Join(CommandPlaceholders, ", "))
			}
		}
	}
	return ""
}

// agentCanBeNamed reports whether a role may name agent: one the kit knows, or one the
// .foreman-kit/agents.yaml of root gives commands. Whether its CLI is installed is the preflight's to say.
func agentCanBeNamed(root, agent string) bool {
	if _, known := SUPPORTED[agent]; known || agent == "stub" {
		return true
	}
	_, written, _ := CommandsFor(root, agent)
	return written
}

func checkSettingTypeProblem(root string, setting settings.Setting, value any) string {
	return checkSettingTypeProblemWith(quietReader(root), root, setting, value)
}

// checkSettingTypeProblemWith is checkSettingTypeProblem asking r for the keys the value is checked
// against: the files alone for a value about to be written, what the run reads for a value given
// to the run.
func checkSettingTypeProblemWith(r *settings.Reader, root string, setting settings.Setting, value any) string {
	switch setting.Kind {
	case settings.KindSlot:
		slot, ok := settings.ParseSlot(value)
		if !ok {
			return setting.Problem(value)
		}
		if slot > 0 {
			workers := r.Int(settings.Workers)
			if slot <= workers || slot == 96 || slot == 98 || slot == 99 {
				return fmt.Sprintf("%d is taken (workers 1 to %s, the merger's checks of a base alone 96, the cycle's judgment 98, the merger 99): %s for the main checkout, or %d", slot, settings.Workers.Path(), settings.SlotNone, settings.SlotFree)
			}
		}
	case settings.KindAgents:
		if prob := setting.Problem(value); prob != "" {
			return prob
		}
		list := Names(value)
		if len(list) == 0 {
			return "lists no agent: the role does not run"
		}
		for _, a := range list {
			if !agentCanBeNamed(root, a) {
				return fmt.Sprintf("must list agents among %s, or one %s gives commands (%s is neither)", strings.Join(SupportedOrder, ", "), AgentsFile, a)
			}
		}
	case settings.KindCommands:
		m, ok := value.(map[string]any)
		if !ok {
			return "must map each agent to its commands (run, restricted, ping)"
		}
		agents := make([]string, 0, len(m))
		for agent := range m {
			agents = append(agents, agent)
		}
		sort.Strings(agents)
		for _, agent := range agents {
			spec, ok := ParseCommandSpec(m[agent])
			if !ok {
				return fmt.Sprintf("%s: must be a mapping with run, restricted and ping", agent)
			}
			for _, entry := range []struct {
				name    string
				command any
			}{{"run", spec.Run}, {"restricted", spec.Restricted}, {"ping", spec.Ping}} {
				if entry.command == nil {
					continue
				}
				if problem := checkCommand(agent+"."+entry.name, entry.command); problem != "" {
					return problem
				}
			}
		}
	case settings.KindArgs:
		m, ok := value.(map[string]any)
		if !ok {
			return "must map agents to their arguments (a string or a list)"
		}
		agents := make([]string, 0, len(m))
		for agent := range m {
			agents = append(agents, agent)
		}
		sort.Strings(agents)
		for _, agent := range agents {
			if !agentCanBeNamed(root, agent) {
				return fmt.Sprintf("%s: not an agent the kit knows (%s) or %s gives commands", agent, strings.Join(SupportedOrder, ", "), AgentsFile)
			}
			if msg := checkArgsValue(agent, m[agent]); msg != "" {
				return msg
			}
		}
	default:
		return setting.Problem(value)
	}
	return ""
}

func leafKeysOfMap(data any, prefix string) []string {
	m, ok := data.(map[string]any)
	if !ok || len(m) == 0 {
		return []string{prefix}
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, leafKeysOfMap(m[k], prefix+"."+k)...)
	}
	return out
}

// checkSectionSettings reports, as errors, every key of the file called section that cannot be used:
// one the catalog defines and data does not write, one that holds something it does not accept, and
// one the catalog does not know (a key an older foreman-kit read, or a typo).
func checkSectionSettings(root, section string, data map[string]any, rep *FileProblems) {
	reported := map[settings.Key]bool{}
	for _, p := range quietReader(root).Use(section, data).Problems(section) {
		rep.Errors = append(rep.Errors, p.Text)
		reported[p.Key] = true
	}
	for _, s := range settings.In(section) {
		val := digMap(data, s.Key.Parts()...)
		if val == nil || reported[s.Key] {
			continue
		}
		if prob := checkSettingTypeProblem(root, s, val); prob != "" {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %s (is %v)", s.Key.Path(), prob, formatInspect(val)))
		}
	}
}

func checkAgentsSection(root string, data map[string]any, rep *FileProblems) {
	agents := Names(data["agents"])
	if data["agents"] != nil && len(agents) == 0 {
		rep.Errors = append(rep.Errors, settings.Agents.Path()+": none listed")
	}
	cfg := agentsConfigOf(data)
	var unknown []string
	agentSet := make(map[string]bool)
	for _, a := range agents {
		agentSet[a] = true
		if _, defined := cfg.Commands[a]; !defined && a != "stub" {
			if _, known := SUPPORTED[a]; !known {
				unknown = append(unknown, a)
			}
		}
	}
	if len(unknown) > 0 {
		rep.Errors = append(rep.Errors, fmt.Sprintf("%s: unknown %s", settings.Agents.Path(), strings.Join(unknown, ", ")))
	}

	restricted := cfg.Permissions == settings.PermissionsRestricted
	checked := map[string]bool{}
	for _, role := range RolesOrder {
		list := cfg.Roles[role]
		var outside []string
		for _, a := range list {
			if _, sup := SUPPORTED[a]; sup && !agentSet[a] {
				outside = append(outside, a)
			}
		}
		if len(outside) > 0 {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %s not among the agents, so without skills here: foreman-kit update --agents ... adds it", settings.RoleAgents(role).Path(), strings.Join(outside, ", ")))
		}
		for _, a := range list {
			if a == "stub" {
				continue
			}
			at := settings.AgentCommands.Path() + "." + a
			spec, written := cfg.Commands[a]
			switch {
			case !written && !checked[a]:
				if _, shipped := SUPPORTED[a]; shipped {
					rep.Errors = append(rep.Errors, fmt.Sprintf("%s: missing: foreman-kit update --agents %s writes the commands the kit ships for %s", at, strings.Join(agentsWith(agents, a), ","), a))
				} else {
					rep.Errors = append(rep.Errors, fmt.Sprintf("%s: missing: write the commands that run %s (run, restricted, ping)", at, a))
				}
			case written && spec.Run == nil && !checked[a]:
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s.run: missing: %s cannot run", at, a))
			case written && spec.Ping == nil && !checked[a]:
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s.ping: missing: the preflight pings %s with its run command", at, a))
			}
			if written && restricted && spec.Restricted == nil && !checked[a+"\x00restricted"] {
				checked[a+"\x00restricted"] = true
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s.restricted: missing: %s cannot be used while %s is %s (%s lists it)", at, a, settings.AgentPermissions.Path(), settings.PermissionsRestricted, settings.RoleAgents(role).Path()))
			}
			checked[a] = true
		}
		args := make([]string, 0, len(cfg.RoleArgs[role]))
		for agent := range cfg.RoleArgs[role] {
			args = append(args, agent)
		}
		sort.Strings(args)
		for _, agent := range args {
			if !slices.Contains(list, agent) {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s.%s: %s is not an agent of this role; nothing reads these arguments", settings.RoleArgs(role).Path(), agent, agent))
			}
		}
	}
}

// agentsWith returns the agents of an application with one more, for the command that adds it.
func agentsWith(agents []string, one string) []string {
	if slices.Contains(agents, one) {
		return agents
	}
	return append(append([]string(nil), agents...), one)
}

func checkCycleSection(root string, data map[string]any, rep *FileProblems, evaluatorData map[string]any) {
	for _, m := range memory.OverCap(root) {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("memory: %s holds %d characters, over its cap of %d; the next cycle is told to trim it first", m.File, m.Chars, m.Cap))
	}
	r := quietReader(root).Use(settings.RoadmapFile.File(), data)
	rmFile := r.String(settings.RoadmapFile)
	if rmFile != "" {
		full := filepath.Join(root, filepath.FromSlash(rmFile))
		if !fileExists(full) {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %s does not exist; the cycle proposes nothing from a roadmap", settings.RoadmapFile.Path(), rmFile))
		} else if raw, err := os.ReadFile(full); err == nil && strings.Contains(string(raw), "{{") {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %s still has {{...}} placeholders; the cycle proposes nothing from it until they are filled", settings.RoadmapFile.Path(), rmFile))
		}
	}
	// A cap on the queue below the number of workers leaves some of them without work for good.
	if maxOpen, workers := r.Int(settings.QueueMaxOpen), r.Int(settings.Workers); maxOpen < workers {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %d, below %s (%d): the cycle stops queuing before every worker has a change to build",
			settings.QueueMaxOpen.Path(), maxOpen, settings.Workers.Where(), workers))
	}
	// No value comes out of another: the kit only says when two of them do not fit together.
	budget, exploring := r.Duration(settings.CycleBudget), r.Duration(settings.CycleExploring)
	if exploring > budget {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %s, above %s (%s): the strategy cannot get more time than the judgment has",
			settings.CycleExploring.Path(), settings.FormatDuration(exploring), settings.CycleBudget.Path(), settings.FormatDuration(budget)))
	}
	if limit := r.Duration(settings.RoleTimeout("cycle")); budget > limit {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %s does not fit in %s (%s): the judgment is stopped before it has spent its budget",
			settings.CycleBudget.Path(), settings.FormatDuration(budget), settings.RoleTimeout("cycle").Where(), settings.FormatDuration(limit)))
	}

	approvalKey := settings.ProposalsApprovedBy
	approval := r.String(approvalKey)
	if approval != settings.ApprovalEvaluator {
		return
	}
	mode := settings.GateOff
	if evaluatorData != nil {
		er := quietReader(root).Use(settings.EvaluatorFile, evaluatorData)
		if er.Bool(settings.EvaluatorEnabled) {
			mode = er.String(settings.GateMode("proposal_review"))
		}
	}
	if mode != settings.GateEnforce {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %s, but gate proposal_review is %s: every proposal waits for a person", approvalKey.Path(), approval, mode))
	} else if _, err := exec.LookPath("jev"); err != nil && evaluator.ResolveAPIKey() == "" {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %s, but the jev CLI is not installed: every proposal waits for a person", approvalKey.Path(), approval))
	}
}

// checkDaemonSection warns about two values of .foreman-kit/daemon.yaml that do not fit together.
func checkDaemonSection(root string, data map[string]any, rep *FileProblems) {
	r := quietReader(root).Use(settings.CycleEvery.File(), data)
	every, early := r.Duration(settings.CycleEvery), r.Duration(settings.CycleEveryWhenOutOfWork)
	if early > 0 && every > 0 && early >= every {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %s is not below %s (%s): the cycle never starts early; write %s to say so",
			settings.CycleEveryWhenOutOfWork.Path(), settings.FormatDuration(early), settings.CycleEvery.Path(), settings.FormatDuration(every), settings.Never))
	}
}

// checkVisualQASection warns about two values of .foreman-kit/visual_qa.yaml that do not fit together.
func checkVisualQASection(root string, data map[string]any, rep *FileProblems) {
	r := quietReader(root).Use(settings.VisualQAEnabled.File(), data)
	budget, expires := r.Duration(settings.VisualQATimeBudget), r.Duration(settings.VisualQAExpiresAfter)
	if budget > 0 && expires > 0 && expires < budget {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %s is below %s (%s): the lock of a session frees while the session may still be running",
			settings.VisualQAExpiresAfter.Path(), settings.FormatDuration(expires), settings.VisualQATimeBudget.Path(), settings.FormatDuration(budget)))
	}
}

func checkExplorationSection(root string, data map[string]any, rep *FileProblems) {
	list, ok := data["strategies"].([]any)
	if !ok {
		if data["strategies"] != nil {
			rep.Errors = append(rep.Errors, "strategies: must be a list")
		}
		return
	}
	seen := make(map[string]bool)
	enabled := map[string][]string{}
	total := 0
	for i, raw := range list {
		s, ok := raw.(map[string]any)
		at := fmt.Sprintf("strategies[%d]", i)
		if ok {
			if sl, ok := s["slug"].(string); ok && sl != "" {
				at = fmt.Sprintf("strategies[%d] (%s)", i, sl)
			}
		}
		if !ok {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: must be a mapping", at))
			continue
		}
		slug, _ := s["slug"].(string)
		if slug == "" {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: no slug; it is skipped", at))
		} else if seen[slug] {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: slug used twice", at))
		} else if !changeNameRe.MatchString(slug) {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: slug should be kebab-case", at))
		}
		seen[slug] = true

		kindStr, _ := s["kind"].(string)
		if !slices.Contains(ExplorationKinds, kindStr) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: kind must be one of %s", at, strings.Join(ExplorationKinds, ", ")))
		}
		dirStr, _ := s["directive"].(string)
		if strings.TrimSpace(dirStr) == "" {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: no directive", at))
		}
		nameStr, _ := s["name"].(string)
		if strings.TrimSpace(nameStr) == "" {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: no name", at))
		}
		if s["needs"] != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: needs is no longer read: session says who runs the strategy, %s (foreman-kit update rewrites the file)", at, strings.Join(settings.Sessions, " or ")))
		}
		if session, _ := s["session"].(string); !slices.Contains(settings.Sessions, session) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: session must be %s", at, strings.Join(settings.Sessions, " or ")))
		}
		if s["enabled"] != nil && s["enabled"] != true && s["enabled"] != false {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: enabled must be true or false", at))
		}
		if slug != "" && s["enabled"] != false {
			session := StrategySession(s)
			enabled[session] = append(enabled[session], slug)
			total++
		}
	}
	if total == 0 {
		rep.Warnings = append(rep.Warnings, "strategies: none enabled; the cycle only runs its regression sweep")
		return
	}
	if len(enabled[settings.SessionCycle]) == 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("strategies: none enabled with session %s; the cycle explores none", settings.SessionCycle))
	}
	if visual := enabled[settings.SessionVisualQA]; len(visual) > 0 && !quietReader(root).Bool(settings.VisualQAEnabled) {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("strategies: %s run in the visual session, which is off (%s): nothing explores them",
			strings.Join(visual, ", "), settings.VisualQAEnabled.Where()))
	}
}

func checkEvaluatorSection(root string, data map[string]any, rep *FileProblems) {
	model, _ := data["model"].(string)
	if model == "" || strings.Contains(model, "latest") {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: pin a versioned id (for example %v), not latest", settings.EvaluatorModel.Path(), settings.Of(settings.EvaluatorModel).Default))
	}
	if bandRaw, exists := data["abstain_band"]; exists && bandRaw != nil {
		validBand := false
		if arr, ok := bandRaw.([]any); ok && len(arr) == 2 {
			f0, ok0 := toFloat(arr[0])
			f1, ok1 := toFloat(arr[1])
			if ok0 && ok1 && f0 < f1 && f0 >= 0 && f0 <= 1 && f1 >= 0 && f1 <= 1 {
				validBand = true
			}
		}
		if !validBand {
			rep.Errors = append(rep.Errors, settings.EvaluatorAbstainBand.Path()+": must be [low, high] between 0 and 1")
		}
	}
	gates, _ := data["gates"].(map[string]any)
	activeCount := 0
	var gateNames []string
	// A gate the file does not list at all is reported as a missing key (its mode).
	for name := range gates {
		gateNames = append(gateNames, name)
	}
	sort.Strings(gateNames)
	for _, name := range gateNames {
		rawCfg := gates[name]
		if !settings.IsGate(name) {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("gates.%s: nothing asks this gate", name))
			continue
		}
		cfg, ok := rawCfg.(map[string]any)
		if !ok {
			rep.Errors = append(rep.Errors, fmt.Sprintf("gates.%s: must be a mapping", name))
			continue
		}
		mode := settings.GateOff
		if m, ok := cfg["mode"].(string); ok && m != "" {
			mode = m
		}
		fileStr, _ := cfg["file"].(string)
		hasQuestions := cfg["questions"] != nil
		if mode != settings.GateOff {
			activeCount++
			if fileStr != "" {
				if _, _, err := evaluator.LoadTemplate(root, fileStr); err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("gates.%s.file: %s does not exist or is invalid (%v)", name, fileStr, err))
				}
			} else if !hasQuestions {
				if _, _, err := evaluator.LoadGateTemplate(root, name, nil); err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("gates.%s.questions: missing or invalid (%v)", name, err))
				}
			} else {
				if _, _, err := evaluator.LoadGateTemplate(root, name, nil); err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("gates.%s.questions: %v", name, err))
				}
			}
		}
	}
	if activeCount == 0 {
		return
	}
	r := quietReader(root).Use(settings.EvaluatorFile, data)
	switch {
	case !r.Bool(settings.EvaluatorEnabled):
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: false: every gate is off, whatever its mode", settings.EvaluatorEnabled.Path()))
	default:
		if _, err := exec.LookPath("jev"); err != nil && evaluator.ResolveAPIKey() == "" {
			whenUnavailable := r.String(settings.EvaluatorWhenUnavailable)
			outcome := "the gates pass"
			switch whenUnavailable {
			case settings.UnavailableBlock:
				outcome = "enforce gates block changes and pause the system"
			case settings.UnavailableStop:
				outcome = "enforce gates block changes, and the preflight fails: no cycle judges and no worker starts"
			}
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("jev CLI not installed: %s (%s: %s)", outcome, settings.EvaluatorWhenUnavailable.Path(), whenUnavailable))
		}
	}
}

func checkBacklogFile(root string, rep *FileProblems) {
	raw, err := os.ReadFile(filepath.Join(root, ".foreman-kit", "agent_backlog.json"))
	if err != nil {
		rep.Errors = append(rep.Errors, "missing or not valid JSON")
		return
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		rep.Errors = append(rep.Errors, "missing or not valid JSON")
		return
	}
	items, ok := parsed.([]any)
	if !ok {
		rep.Errors = append(rep.Errors, "must be a JSON list")
		return
	}

	done := make(map[string]bool)
	histFiles := []string{filepath.Join(root, ".foreman-kit", "agent_history.json")}
	if rotMatches, err := filepath.Glob(filepath.Join(root, ".foreman-kit", "agent_history.*.json")); err == nil {
		histFiles = append(histFiles, rotMatches...)
	}
	for _, hf := range histFiles {
		if hRaw, err := os.ReadFile(hf); err == nil {
			var hList []any
			if json.Unmarshal(hRaw, &hList) == nil {
				for _, h := range hList {
					if hm, ok := h.(map[string]any); ok {
						if cn, ok := hm["change_name"].(string); ok && cn != "" {
							done[cn] = true
						}
					}
				}
			}
		}
	}

	names := make([]string, len(items))
	nameCount := make(map[string]int)
	firstIdx := make(map[string]int)
	for i, it := range items {
		if m, ok := it.(map[string]any); ok {
			n, _ := m["change_name"].(string)
			names[i] = n
			nameCount[n]++
			if _, exists := firstIdx[n]; !exists {
				firstIdx[n] = i
			}
		}
	}

	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			rep.Errors = append(rep.Errors, fmt.Sprintf("item %d: must be an object", i))
			continue
		}
		name := names[i]
		at := name
		if at == "" {
			at = fmt.Sprintf("item %d", i)
		}
		if !changeNameRe.MatchString(name) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: change_name must be kebab-case (a-z, 0-9, -); the dispatcher skips it", at))
		}
		if name != "" && nameCount[name] > 1 && firstIdx[name] == i {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: listed twice", at))
		}
		if prio := m["priority"]; prio != nil {
			pStr, ok := prio.(string)
			validP := false
			for _, vp := range BacklogPriorities {
				if ok && pStr == vp {
					validP = true
					break
				}
			}
			if !validP {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: priority %v is read as normal", at, formatInspect(prio)))
			}
		}
		desc, _ := m["description"].(string)
		if strings.TrimSpace(desc) == "" {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: no description", at))
		}
		if deps, ok := m["depends_on"].([]any); ok {
			var missing []string
			for _, d := range deps {
				ds := fmt.Sprint(d)
				if nameCount[ds] == 0 && !done[ds] {
					missing = append(missing, ds)
				}
			}
			if len(missing) > 0 {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: depends on %s, not in the backlog (fine if already archived)", at, strings.Join(missing, ", ")))
			}
		}
	}
}

func checkHistoryFile(root string, rep *FileProblems) {
	raw, err := os.ReadFile(filepath.Join(root, ".foreman-kit", "agent_history.json"))
	if err != nil {
		rep.Errors = append(rep.Errors, "missing or not valid JSON")
		return
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		rep.Errors = append(rep.Errors, "missing or not valid JSON")
		return
	}
	items, ok := parsed.([]any)
	if !ok {
		rep.Errors = append(rep.Errors, "must be a JSON list")
		return
	}
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			rep.Errors = append(rep.Errors, fmt.Sprintf("item %d: must be an object", i))
			continue
		}
		name, _ := m["change_name"].(string)
		at := name
		if at == "" {
			at = fmt.Sprintf("item %d", i)
		}
		if !changeNameRe.MatchString(name) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: change_name must be kebab-case", at))
		}
	}
}

func checkProposalsFile(root string, rep *FileProblems) {
	raw, err := os.ReadFile(filepath.Join(root, ".foreman-kit", "agent_proposals.json"))
	if err != nil {
		rep.Errors = append(rep.Errors, "missing or not valid JSON")
		return
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		rep.Errors = append(rep.Errors, "missing or not valid JSON")
		return
	}
	items, ok := parsed.([]any)
	if !ok {
		rep.Errors = append(rep.Errors, "must be a JSON list")
		return
	}
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			rep.Errors = append(rep.Errors, fmt.Sprintf("item %d: must be an object", i))
			continue
		}
		name, _ := m["change_name"].(string)
		at := name
		if at == "" {
			at = fmt.Sprintf("item %d", i)
		}
		if !changeNameRe.MatchString(name) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: change_name must be kebab-case", at))
		}
		stStr, _ := m["status"].(string)
		validSt := false
		for _, vs := range ProposalStatuses {
			if stStr == vs {
				validSt = true
				break
			}
		}
		if !validSt {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: status must be one of %s", at, strings.Join(ProposalStatuses, ", ")))
		}
		if m["kind"] == "prompt" {
			prName, _ := m["prompt"].(string)
			if _, ok := PLACEHOLDERS[prName]; !ok {
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s: prompt must be one of %s", at, strings.Join(RolesOrder, ", ")))
			}
			edits, ok := m["edits"].([]any)
			validEdits := ok && len(edits) > 0
			for _, ed := range edits {
				em, ok := ed.(map[string]any)
				if !ok {
					validEdits = false
					break
				}
				oldStr, ok1 := em["old"].(string)
				_, ok2 := em["new"].(string)
				if !ok1 || oldStr == "" || !ok2 {
					validEdits = false
					break
				}
			}
			if !validEdits {
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s: edits must be a list of {old, new} texts", at))
			}
			if stStr == "proposed" {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: a prompt edit waits for a human: foreman-kit proposals show %s", at, at))
			}
		}
	}
}

// RepoProblems checks a child repository's repo.config.yaml.
func RepoProblems(root, rel string) FileProblems {
	rep := FileProblems{Errors: []string{}, Warnings: []string{}}
	dir := filepath.Join(root, filepath.FromSlash(rel))
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		rep.Warnings = append(rep.Warnings, "not checked out: git submodule update --init")
		return rep
	}
	cfgFile := filepath.Join(dir, "repo.config.yaml")
	if !fileExists(cfgFile) {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("no repo.config.yaml: no gates run, base branch from .gitmodules or main (foreman-kit setup repo %s writes one)", filepath.Base(rel)))
		return rep
	}
	raw, err := os.ReadFile(cfgFile)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return rep
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		rep.Errors = append(rep.Errors, "repo.config.yaml is not valid YAML: "+err.Error())
		return rep
	}
	if cfg == nil {
		cfg = map[string]any{}
	}

	// The keys an older foreman-kit read are not read any more: each one is named with the key that
	// holds its value now. The rest of the file is checked once it is rewritten.
	if former := RepoFormerKeys(cfg); len(former) > 0 {
		for _, f := range former {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: no longer read: it is %s, because %s (foreman-kit update rewrites the file)", f.Was, f.Is, f.Why))
		}
		return rep
	}

	var left []string
	for _, lk := range leafKeysOfMap(cfg, "cfg") {
		parts := strings.Split(lk, ".")[1:]
		if hasPlaceholder(digMap(cfg, parts...)) {
			left = append(left, strings.TrimPrefix(lk, "cfg."))
		}
	}
	if len(left) > 0 {
		rep.Warnings = append(rep.Warnings, "placeholders left: "+strings.Join(left, ", "))
	}

	if kind := cfg["kind"]; kind != nil && !hasPlaceholder(kind) && kind != "web" && kind != "cli" {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("kind %v: web or cli (QA opens a web application in a browser and runs a cli)", formatInspect(kind)))
	}
	script := ""
	if es, ok := cfg["env_script"].(string); ok {
		script = strings.TrimSpace(es)
	}
	if script != "" && !fileExists(filepath.Join(dir, filepath.FromSlash(script))) {
		rep.Errors = append(rep.Errors, fmt.Sprintf("env_script %s does not exist: every command of the repository fails", script))
	}

	usableGates := make(map[string]string)
	if gMap, ok := cfg["gates"].(map[string]any); ok {
		for k, v := range gMap {
			cs := strings.TrimSpace(fmt.Sprint(v))
			if v != nil && cs != "" && !hasPlaceholder(cs) {
				usableGates[k] = cs
			}
		}
	}
	gateKeys := make([]string, 0, len(usableGates))
	for k := range usableGates {
		gateKeys = append(gateKeys, k)
	}
	sort.Strings(gateKeys)
	for _, k := range gateKeys {
		if doubleQuotedDollarHash(usableGates[k]) {
			// The Go test gate foreman-kit setup used to write: in double quotes the shell
			// expands the "$#" of ".go$#" to 0, sed fails, and the gate tests every package every time.
			rep.Warnings = append(rep.Warnings, fmt.Sprintf(`gates.%s: "$#" inside double quotes is expanded by the shell (to 0), so its sed program fails and the gate runs everything on every change; single-quote that program, or rewrite the gate with foreman-kit setup repo <name> --force`, k))
		}
	}
	if len(usableGates) == 0 {
		rep.Warnings = append(rep.Warnings, "no gate set: changes to it merge untested")
	} else if _, hasTest := usableGates["test"]; !hasTest {
		rep.Warnings = append(rep.Warnings, "no test gate")
	}

	if reqs := cfg["required_services"]; reqs != nil {
		rm, ok := reqs.(map[string]any)
		validReq := ok
		for _, v := range rm {
			vs, isStr := v.(string)
			if !isStr || strings.TrimSpace(vs) == "" {
				validReq = false
				break
			}
		}
		if !validReq {
			rep.Errors = append(rep.Errors, `required_services: must map a name to the command that checks it, e.g. postgres: "pg_isready -p 5432"`)
		}
	}
	if setup := cfg["setup"]; setup != nil {
		if _, isStr := setup.(string); !isStr {
			rep.Errors = append(rep.Errors, "setup: must be a command (text)")
		}
	}
	regression, _ := cfg["regression"].(map[string]any)
	if cfg["regression"] != nil && regression == nil {
		rep.Errors = append(rep.Errors, "regression: must be a section with commands and when")
	}
	hasRegCmd := false
	if commands := regression["commands"]; commands != nil {
		rMap, ok := commands.(map[string]any)
		if !ok {
			rep.Errors = append(rep.Errors, `regression.commands: must map a name to its command, e.g. test: "bundle exec rspec"`)
		}
		for _, v := range rMap {
			vs, isStr := v.(string)
			if isStr && strings.TrimSpace(vs) != "" && !hasPlaceholder(vs) {
				hasRegCmd = true
				break
			}
		}
	}
	if !hasRegCmd && len(usableGates) > 0 {
		// Nothing stands in for a regression that has no command: the gates do not run in its place.
		rep.Warnings = append(rep.Warnings, "no regression command (regression.commands): the cycle's sweep runs nothing in this repository, and a base that breaks on its own is not checked with its full suite")
	}
	if globs := regression["when"]; globs != nil {
		gArr, ok := globs.([]any)
		validGlobs := ok
		for _, g := range gArr {
			gs, isStr := g.(string)
			if !isStr || strings.TrimSpace(gs) == "" {
				validGlobs = false
				break
			}
		}
		positives := 0
		if validGlobs {
			for _, g := range gArr {
				gs := strings.TrimSpace(g.(string))
				if !strings.HasPrefix(gs, "!") {
					positives++
					continue
				}
				if rest := strings.TrimSpace(gs[1:]); rest == "" || strings.HasPrefix(rest, "!") {
					rep.Errors = append(rep.Errors, fmt.Sprintf(`regression.when: %q is not an exclusion: "!" must be followed by one path glob, e.g. "!config/locales/**"`, gs))
				}
			}
		}
		if tagged := unquotedExclusions(raw); len(tagged) > 0 {
			for _, x := range tagged {
				rep.Errors = append(rep.Errors, fmt.Sprintf(`regression.when: write the exclusion %s in quotes ("%s"): unquoted, YAML reads it as a tag and the entry is empty`, x, x))
			}
		} else if !validGlobs {
			rep.Errors = append(rep.Errors, `regression.when: must be a list of path globs, e.g. ["app/models/**"]`)
		} else if len(gArr) > 0 && positives == 0 {
			rep.Warnings = append(rep.Warnings, "regression.when has only exclusions: nothing ever triggers the regression")
		} else if len(gArr) > 0 && !hasRegCmd {
			rep.Warnings = append(rep.Warnings, "regression.when: set, but no regression command: a change there runs nothing more than its gates")
		}
	}
	if session, ok := cfg["visual_qa"].(map[string]any); ok {
		if prep := session["prepare"]; prep != nil {
			if _, isStr := prep.(string); !isStr {
				rep.Errors = append(rep.Errors, "visual_qa.prepare: must be a command (text)")
			}
		}
	}

	return rep
}

// CheckKitDrift checks if any kit file recorded in .foreman-kit/kit.lock has been modified or deleted.
func CheckKitDrift(root string) string {
	lockPath := filepath.Join(root, ".foreman-kit/kit.lock")
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		return ""
	}
	var lock map[string]any
	if err := json.Unmarshal(raw, &lock); err != nil {
		return ""
	}
	version := fmt.Sprint(lock["version"])
	edited := make(map[string]string)
	if filesMap, ok := lock["files"].(map[string]any); ok {
		var rels []string
		for r := range filesMap {
			rels = append(rels, r)
		}
		sort.Strings(rels)
		for _, rel := range rels {
			fm, ok := filesMap[rel].(map[string]any)
			if !ok || fm["class"] != "kit" {
				continue
			}
			wantSha, _ := fm["sha256"].(string)
			p := filepath.Join(root, filepath.FromSlash(rel))
			b, err := os.ReadFile(p)
			if err != nil {
				edited[rel] = "missing"
			} else if assets.SHA256Bytes(b) != wantSha {
				edited[rel] = "modified"
			}
		}
	}
	if lockAgentsRaw, ok := lock["agents"].([]any); ok {
		var lockAgents []string
		for _, a := range lockAgentsRaw {
			lockAgents = append(lockAgents, fmt.Sprint(a))
		}
		if cfg := Load(root); cfg != nil && len(cfg.Agents) > 0 && !reflect.DeepEqual(cfg.Agents, lockAgents) {
			edited[".foreman-kit/agents.yaml"] = fmt.Sprintf("lists %s, skills installed for %s: run foreman-kit upgrade --agents ...", strings.Join(cfg.Agents, ", "), strings.Join(lockAgents, ", "))
		}
	}
	if len(edited) == 0 {
		return ""
	}
	var keys []string
	for k := range edited {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s (%s)", k, edited[k]))
	}
	return fmt.Sprintf("kit: DRIFT from v%s: %s", version, strings.Join(parts, ", "))
}

func checkApplicationProblems(root string) FileProblems {
	rep := FileProblems{Errors: []string{}, Warnings: []string{}}
	if drift := CheckKitDrift(root); drift != "" {
		rep.Errors = append(rep.Errors, fmt.Sprintf("kit files edited in place (%s): foreman-kit verify", drift))
	}

	// The memory is kept with agent-memory. An application installed before it has none, or still
	// holds the files it kept its memory in.
	switch {
	case !memory.Present(root):
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("the application has no %s/: foreman-kit update creates it with %s (the preflight fails until then)", memory.Dir, memory.Binary))
	case memory.LegacyPresent(root):
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s and %s/ are no longer read: foreman-kit update takes them into %s/", memory.LegacyFile, memory.LegacyDir, memory.Dir))
	}

	var pendingNew []string
	for _, pat := range []string{"*.md.new", ".foreman-kit/*.new", ".foreman-kit/prompts/*.new"} {
		matches, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(pat)))
		for _, m := range matches {
			if rel, err := filepath.Rel(root, m); err == nil {
				pendingNew = append(pendingNew, filepath.ToSlash(rel))
			}
		}
	}
	sort.Strings(pendingNew)
	if len(pendingNew) > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("an upgrade left %s: merge what you need into the file, then delete it", strings.Join(pendingNew, ", ")))
	}

	ctx := context.Background()
	statusArgs := []string{"status", "--porcelain", "--ignore-submodules=all", "--", "."}
	for _, sp := range ScopeWritePaths {
		statusArgs = append(statusArgs, ":!"+sp)
	}
	if out, err := gitx.RunGit(ctx, root, statusArgs...); err == nil && strings.TrimSpace(out) != "" {
		var files []string
		for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			if len(line) > 3 {
				files = append(files, strings.TrimSpace(line[3:]))
			}
		}
		shown := files
		suffix := ""
		if len(shown) > 8 {
			shown = shown[:8]
			suffix = ", ..."
		}
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("uncommitted outside the cycle's write scope: %s%s; the next cycle or visual session pauses the system (scope guard) until they are committed", strings.Join(shown, ", "), suffix))
	}

	base := quietReader(root).String(settings.BaseBranch)
	if remOut, err := gitx.RunGit(ctx, root, "remote"); err == nil {
		hasOrigin := false
		for _, r := range strings.Fields(remOut) {
			if r == "origin" {
				hasOrigin = true
				break
			}
		}
		if hasOrigin {
			if _, err := gitx.RunGit(ctx, root, "rev-parse", "--verify", "--quiet", "origin/"+base); err != nil {
				curBranch := "<branch>"
				if sym, ok := gitx.CurrentBranch(ctx, root); ok {
					curBranch = sym
				}
				if curBranch == base {
					// The setting already names this branch: setting it again is no remedy.
					rep.Warnings = append(rep.Warnings, fmt.Sprintf("origin has no branch %s (%s): push it (git push -u origin %s)", base, settings.BaseBranch, base))
				} else {
					rep.Warnings = append(rep.Warnings, fmt.Sprintf("origin has no branch %s (%s): push it, or foreman-kit config set %s %s", base, settings.BaseBranch, settings.BaseBranch, curBranch))
				}
			}
		}
	}

	// A variable an older foreman-kit read changes nothing any more, and whoever exports it expects
	// that it does: what read it starts nothing until it is out of the environment.
	for _, p := range settings.FormerVariablesSet() {
		rep.Errors = append(rep.Errors, p.Text)
	}
	return rep
}

// Check validates all application configuration files, prompts, backlog, proposals, child repos, and workspace state.
func Check(root string) map[string]FileProblems {
	if root == "" {
		root = AppRoot("")
	}
	files := make(map[string]FileProblems)
	evaluatorData, _, _ := loadConfigYAML(root, settings.EvaluatorFile)

	for _, section := range configSections() {
		rel := configFile(section)
		rep := FileProblems{Errors: []string{}, Warnings: []string{}}
		data, present, err := loadConfigYAML(root, section)
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
			files[rel] = rep
			continue
		}
		if !present {
			missing := "missing (foreman-kit update writes it)"
			if section == settings.EvaluatorFile && fileExists(filepath.Join(root, filepath.FromSlash(evaluator.LegacyPolicyFile))) {
				// An application installed before the file was renamed: its gates are still there.
				missing = fmt.Sprintf("missing: %s has not been renamed to it (foreman-kit update)", evaluator.LegacyPolicyFile)
			}
			rep.Errors = append(rep.Errors, missing)
			files[rel] = rep
			continue
		}
		checkSectionSettings(root, section, data, &rep)
		switch section {
		case "agents":
			checkAgentsSection(root, data, &rep)
		case "cycle":
			checkCycleSection(root, data, &rep, evaluatorData)
		case "daemon":
			checkDaemonSection(root, data, &rep)
		case "visual_qa":
			checkVisualQASection(root, data, &rep)
		case "exploration":
			checkExplorationSection(root, data, &rep)
		case settings.EvaluatorFile:
			checkEvaluatorSection(root, data, &rep)
		}
		files[rel] = rep
	}

	backlogRep := FileProblems{Errors: []string{}, Warnings: []string{}}
	checkBacklogFile(root, &backlogRep)
	files[".foreman-kit/agent_backlog.json"] = backlogRep

	if fileExists(filepath.Join(root, ".foreman-kit", "agent_history.json")) {
		histRep := FileProblems{Errors: []string{}, Warnings: []string{}}
		checkHistoryFile(root, &histRep)
		files[".foreman-kit/agent_history.json"] = histRep
	}

	propRep := FileProblems{Errors: []string{}, Warnings: []string{}}
	checkProposalsFile(root, &propRep)
	files[".foreman-kit/agent_proposals.json"] = propRep

	pProb := Problems(root)
	files[PromptsDir+"/"] = FileProblems{Errors: pProb.Errors, Warnings: pProb.Warnings}

	for _, rel := range childPathsInRoot(root) {
		files[rel+"/repo.config.yaml"] = RepoProblems(root, rel)
	}
	files["application"] = checkApplicationProblems(root)
	return files
}

// CheckOrder returns the deterministic display order of keys in Check(root).
func CheckOrder(root string, files map[string]FileProblems) []string {
	var order []string
	seen := make(map[string]bool)
	add := func(k string) {
		if _, ok := files[k]; ok && !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
	}
	for _, sec := range configSections() {
		add(configFile(sec))
	}
	add(".foreman-kit/agent_backlog.json")
	add(".foreman-kit/agent_history.json")
	add(".foreman-kit/agent_proposals.json")
	add(PromptsDir + "/")
	for _, rel := range childPathsInRoot(root) {
		add(rel + "/repo.config.yaml")
	}
	add("application")
	for k := range files {
		add(k)
	}
	return order
}

// FormatCheckText formats the output of Check into human-readable text matching foreman-kit config check.
func FormatCheckText(root string, files map[string]FileProblems) string {
	var lines []string
	totalErrs := 0
	totalWarns := 0
	for _, name := range CheckOrder(root, files) {
		r := files[name]
		state := "ok"
		if len(r.Errors) > 0 {
			state = fmt.Sprintf("%d error(s)", len(r.Errors))
		} else if len(r.Warnings) > 0 {
			state = fmt.Sprintf("%d warning(s)", len(r.Warnings))
		}
		lines = append(lines, fmt.Sprintf("%s: %s", name, state))
		for _, e := range r.Errors {
			lines = append(lines, "  ERROR "+e)
		}
		for _, w := range r.Warnings {
			lines = append(lines, "  WARN  "+w)
		}
		totalErrs += len(r.Errors)
		totalWarns += len(r.Warnings)
	}
	if totalErrs == 0 {
		lines = append(lines, fmt.Sprintf("config: OK (%d warning(s))", totalWarns))
	} else {
		lines = append(lines, fmt.Sprintf("config: FAIL (%d error(s), %d warning(s))", totalErrs, totalWarns))
	}
	return strings.Join(lines, "\n")
}

// ==============================================================================
// Set (using gopkg.in/yaml.v3 yaml.Node AST preserving comments and indentation)
// ==============================================================================

// parseValueForSet turns the text given to `foreman-kit config set` into a value of the setting.
func parseValueForSet(root string, setting settings.Setting, text string, data map[string]any) (any, error) {
	return parseValueWith(quietReader(root), root, setting, text, data)
}

// parseValueWith is parseValueForSet asking r for the keys the value is checked against.
func parseValueWith(r *settings.Reader, root string, setting settings.Setting, text string, data map[string]any) (any, error) {
	var value any = text
	trimmed := strings.TrimSpace(text)
	switch setting.Kind {
	case settings.KindSlot:
		value = trimmed
		if n, err := strconv.Atoi(trimmed); err == nil {
			value = n
		}
	case settings.KindInt:
		value = trimmed
		if n, err := strconv.Atoi(trimmed); err == nil {
			value = n
		}
	case settings.KindDuration, settings.KindEnum, settings.KindBranch, settings.KindText, settings.KindPath:
		value = trimmed
	case settings.KindBool:
		switch trimmed {
		case "true":
			value = true
		case "false":
			value = false
		}
	case settings.KindAgents:
		value = Names(agentsAsWritten(trimmed))
	}

	if prob := checkSettingTypeProblemWith(r, root, setting, value); prob != "" {
		if setting.Scalar() && strings.HasPrefix(prob, "must be ") {
			return nil, fmt.Errorf("%s %s, not %q", setting.Key, prob, text)
		}
		return nil, fmt.Errorf("%s %s", setting.Key, prob)
	}

	if setting.Kind == settings.KindAgents {
		listed := Names(data["agents"])
		listedSet := make(map[string]bool, len(listed))
		for _, a := range listed {
			listedSet[a] = true
		}
		var stray []string
		for _, a := range value.([]string) {
			if !listedSet[a] {
				stray = append(stray, a)
			}
		}
		if len(stray) > 0 {
			return nil, fmt.Errorf("%s not among the agents of %s (%s); add it first with foreman-kit update --agents ..., which installs its skills",
				strings.Join(stray, ", "), settings.Agents.FilePath(), strings.Join(listed, ", "))
		}
	}
	return value, nil
}

// agentsAsWritten reads a list of agents the two ways it is written: as a person types it
// (claude,codex) and as its file holds it ([claude, codex]).
func agentsAsWritten(text string) any {
	if strings.HasPrefix(text, "[") {
		var list []any
		if yaml.Unmarshal([]byte(text), &list) == nil {
			return list
		}
	}
	return text
}

// CheckRunOverrides says why a value `foreman-kit --set` gave this run cannot be used in the
// application at root, in the words `foreman-kit config set` has for the same value there: an
// agent the application does not list, a slot that is taken. Each value was already checked
// against its key when it was given; this is what only the application can say, and it is said
// before the run starts anything. nil: every value can be used, or root holds no file to ask.
func CheckRunOverrides(root string) error {
	given := settings.DecodeOverrides(os.Getenv(settings.RunOverridesEnv))
	if len(given) == 0 {
		return nil
	}
	keys := make([]string, 0, len(given))
	for key := range given {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	inEffect := &settings.Reader{Root: root, Stderr: io.Discard}
	// The files asked are the ones the run reads: the main checkout's, for a worktree cut before
	// `foreman-kit update` rewrote them.
	files := settings.FilesRoot(root)
	for _, name := range keys {
		key := settings.Key(name)
		setting, known := settings.Lookup(key)
		if !known {
			continue
		}
		// A file that is missing or cannot be read is said by the checks of the files.
		raw, err := os.ReadFile(filepath.Join(files, filepath.FromSlash(key.FilePath())))
		if err != nil {
			continue
		}
		var data map[string]any
		if yaml.Unmarshal(raw, &data) != nil {
			continue
		}
		if _, err := parseValueWith(inEffect, files, setting, given[key], data); err != nil {
			return fmt.Errorf("--set %s=%s: %s", key, given[key], strings.TrimPrefix(err.Error(), name+" "))
		}
	}
	return nil
}

// valueToYAMLNode returns the node of value as inlineYAML writes it, with the comments of the node
// it replaces.
func valueToYAMLNode(value any, existing *yaml.Node) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprint(value)}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(inlineYAML(value, existing)), &doc); err == nil && len(doc.Content) == 1 {
		n = doc.Content[0]
		n.Line, n.Column = 0, 0
	}
	if existing != nil {
		n.HeadComment = existing.HeadComment
		n.LineComment = existing.LineComment
		n.FootComment = existing.FootComment
	}
	return n
}

// astLocateResult records the AST target found via gopkg.in/yaml.v3 yaml.Node traversal.
type astLocateResult struct {
	keyNode     *yaml.Node
	valNode     *yaml.Node
	parentMap   *yaml.Node
	insertLine  int // 0-indexed line after which to insert if key was missing in parentMap
	childIndent int
	// newPath lists the keys to write, one line each, when the key was missing: the sections the file
	// lacks, then the key (["merge", "outside_commits"] in a pipeline.yaml that has no merge: section).
	newPath []string
}

func navigateAndMutateAST(doc *yaml.Node, keys []string, value any) (*astLocateResult, error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("root YAML node is not a mapping")
	}
	cur := doc.Content[0]
	parentIndent := -1

	for depth, k := range keys {
		if cur.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("cannot traverse non-mapping node at %q", k)
		}
		var foundKey, foundVal *yaml.Node
		var foundIdx int
		childIndent := -1
		lastLine := cur.Line // 1-indexed

		for i := 0; i+1 < len(cur.Content); i += 2 {
			kn := cur.Content[i]
			vn := cur.Content[i+1]
			if childIndent == -1 && kn.Column > 0 {
				childIndent = kn.Column - 1
			}
			if maxL := maxNodeLine(vn); maxL > lastLine {
				lastLine = maxL
			}
			if kn.Value == k {
				foundKey = kn
				foundVal = vn
				foundIdx = i + 1
				break
			}
		}
		if childIndent == -1 {
			childIndent = parentIndent + 2
		}

		if foundKey == nil {
			// Add the key and, when sections above it are missing too (a file written before the
			// setting existed), those sections: Set writes one line per level after the last line of cur.
			newPath := keys[depth:]
			parent := cur
			var leafKey, leafVal *yaml.Node
			for i, name := range newPath {
				kn := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}
				vn := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				if i == len(newPath)-1 {
					vn = valueToYAMLNode(value, nil)
					leafKey, leafVal = kn, vn
				}
				parent.Content = append(parent.Content, kn, vn)
				parent = vn
			}
			return &astLocateResult{
				keyNode:     leafKey,
				valNode:     leafVal,
				parentMap:   cur,
				insertLine:  lastLine - 1,
				childIndent: childIndent,
				newPath:     newPath,
			}, nil
		}

		if depth == len(keys)-1 {
			updatedVal := valueToYAMLNode(value, foundVal)
			updatedVal.Line = foundVal.Line
			updatedVal.Column = foundVal.Column
			cur.Content[foundIdx] = updatedVal
			return &astLocateResult{
				keyNode:   foundKey,
				valNode:   foundVal,
				parentMap: cur,
			}, nil
		}

		parentIndent = foundKey.Column - 1
		cur = foundVal
	}
	return nil, fmt.Errorf("empty key path")
}

func maxNodeLine(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	maxL := n.Line
	for _, c := range n.Content {
		if cl := maxNodeLine(c); cl > maxL {
			maxL = cl
		}
	}
	return maxL
}

var lineKeyValueRe = regexp.MustCompile(`^( *[^:#]+:[ \t]*)(.*?)([ \t]+#.*)?(\r?\n)?$`)

// Refusal is the error Set returns when the change asked for is one it does not make: the key is not
// a setting, the value is not one the setting accepts, the file is missing or is not valid YAML, or
// the file would not pass its checks with the value. The files are as they were. Any other error of
// Set is a failure to read or to write the file.
type Refusal struct {
	Reason string
}

func (r *Refusal) Error() string { return r.Reason }

// refused returns a Refusal that gives the reason.
func refused(format string, args ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// Settable reports whether `foreman-kit config set` changes s: it does when the key holds one
// value. A list, the arguments of a role and the commands of the agents are edited in their file,
// and Set refuses them as "not one value".
func Settable(s settings.Setting) bool { return s.Settable() }

// Set updates one scalar (or role agent list) setting in place: the value is replaced on the line of
// its key, so the comments and every other line stay, and the file is checked with it. A change it
// does not make is a *Refusal.
func Set(root, key, text string, dryRun bool, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	if root == "" {
		root = AppRoot("")
	}
	setting, ok := settings.Lookup(settings.Key(key))
	if !ok {
		return refused("%s is not a setting; foreman-kit config show lists them", key)
	}
	if !Settable(setting) {
		return refused("%s is not one value: %s", key, setting.Change)
	}

	parts := strings.Split(key, ".")
	section := setting.Key.File()
	relFile := setting.Key.FilePath()
	fullPath := filepath.Join(root, filepath.FromSlash(relFile))
	if !fileExists(fullPath) {
		return refused("%s does not exist (foreman-kit update writes it)", relFile)
	}

	rawBytes, err := os.ReadFile(fullPath)
	if err != nil {
		return err
	}
	var before map[string]any
	if err := yaml.Unmarshal(rawBytes, &before); err != nil {
		return refused("%s is not valid YAML (%v); fix it by hand", relFile, err)
	}
	if before == nil {
		before = map[string]any{}
	}

	value, err := parseValueForSet(root, setting, text, before)
	if err != nil {
		return &Refusal{Reason: err.Error()}
	}

	old := digMap(before, setting.Key.Parts()...)
	if old != nil && valuesEqual(old, value) {
		fmt.Fprintf(out, "%s: %s is already %s\n", relFile, strings.TrimPrefix(key, section+"."), formatConfigValue(value))
		return nil
	}

	// The value is replaced on the line of its key: the comments and every other line stay.
	afterBytes, err := setInText(rawBytes, parts[1:], value)
	if err != nil {
		return refused("cannot set %s in %s (%v); edit it by hand", key, relFile, err)
	}
	afterText := string(afterBytes)
	var after map[string]any
	if err := yaml.Unmarshal(afterBytes, &after); err != nil {
		return refused("editing %s would break %s; edit it by hand", key, relFile)
	}
	if after == nil {
		after = map[string]any{}
	}

	// The change may not bring a problem the file did not have: what it already lacked stays its
	// own, said by check, and can be mended one key at a time.
	problemsOf := func(data map[string]any) []string {
		rep := FileProblems{Errors: []string{}, Warnings: []string{}}
		checkSectionSettings(root, section, data, &rep)
		if section == "agents" {
			checkAgentsSection(root, data, &rep)
		}
		return rep.Errors
	}
	had := map[string]bool{}
	for _, problem := range problemsOf(before) {
		had[problem] = true
	}
	var brought []string
	for _, problem := range problemsOf(after) {
		if !had[problem] {
			brought = append(brought, problem)
		}
	}
	if len(brought) > 0 {
		return refused("%s = %s would make %s invalid: %s", key, formatConfigValue(value), relFile, strings.Join(brought, "; "))
	}

	oldDisplay := "nothing (the key was missing)"
	if old != nil {
		oldDisplay = formatConfigValue(old)
	}
	changeDesc := fmt.Sprintf("%s: %s %s -> %s",
		relFile, strings.TrimPrefix(key, section+"."), oldDisplay, formatConfigValue(value))
	if dryRun {
		fmt.Fprintf(out, "would change %s\n", changeDesc)
		return nil
	}

	if err := WriteFileAtomic(fullPath, []byte(afterText), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "changed %s\n", changeDesc)
	fmt.Fprintf(out, "takes effect on the next run of %s. Commit and push it before the next cycle: an uncommitted change outside the cycle's write scope pauses the system (git add %s && git commit && git push).\n",
		setting.ReadBy, relFile)
	return nil
}

// RunConfigCLI implements `foreman-kit config show | check | set`. `foreman-kit config recommend`
// reads the state database and is implemented beside the cycle (cycle.RunRecommendCLI); its usage is
// listed here with the others.
func RunConfigCLI(root string, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if root == "" {
		root = AppRoot("")
	}
	usage := "Usage: foreman-kit config show [" + strings.Join(configSections(), "|") + "|repos|prompts|env] [--json]\n" +
		"       foreman-kit config check [--json]\n" +
		"       foreman-kit config set <file>.<key> <value> [--dry-run]\n" +
		"       foreman-kit config recommend [--window 24h|7d|15d|30d] [--evidence] [--json]"

	var filtered []string
	asJSON := false
	dryRun := false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "--dry-run":
			dryRun = true
		default:
			filtered = append(filtered, a)
		}
	}

	if len(filtered) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	switch filtered[0] {
	case "show":
		if len(filtered) > 2 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		topic := ""
		if len(filtered) == 2 {
			topic = filtered[1]
		}
		view, err := Show(root, topic)
		if err != nil {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		if asJSON {
			b, _ := json.MarshalIndent(view, "", "  ")
			fmt.Fprintln(stdout, string(b))
		} else {
			fmt.Fprintln(stdout, FormatShowText(view))
		}
		return 0
	case "check":
		if len(filtered) != 1 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		files := Check(root)
		if asJSON {
			b, _ := json.MarshalIndent(files, "", "  ")
			fmt.Fprintln(stdout, string(b))
		} else {
			fmt.Fprintln(stdout, FormatCheckText(root, files))
		}
		for _, r := range files {
			if len(r.Errors) > 0 {
				return 1
			}
		}
		return 0
	case "set":
		if len(filtered) != 3 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		if err := Set(root, filtered[1], filtered[2], dryRun, stdout); err != nil {
			fmt.Fprintf(stderr, "kit_config: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintln(stderr, usage)
		return 2
	}
}

func splitKeepEOL(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func valuesEqual(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func mapsEquivalent(a, b map[string]any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func formatInspect(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// unquotedExclusions returns the regression.when entries of a repo.config.yaml written as unquoted
// exclusions (- !config/locales/**): YAML reads a leading "!" as a local tag, so the entry decodes as an
// empty string and the exclusion silently does nothing.
func unquotedExclusions(raw []byte) []string {
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	root := doc.Content[0]
	var out []string
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "regression" || root.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		section := root.Content[i+1]
		for j := 0; j+1 < len(section.Content); j += 2 {
			if section.Content[j].Value != "when" || section.Content[j+1].Kind != yaml.SequenceNode {
				continue
			}
			for _, n := range section.Content[j+1].Content {
				if strings.HasPrefix(n.Tag, "!") && !strings.HasPrefix(n.Tag, "!!") {
					out = append(out, n.Tag+n.Value)
				}
			}
		}
	}
	return out
}

// doubleQuotedDollarHash reports whether a shell command has "$#" inside a double-quoted string
// (where the shell expands it), as in sed -nE "s#^(.+)/[^/]+\.go$#./\1#p".
func doubleQuotedDollarHash(cmd string) bool {
	inSingle, inDouble := false, false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '\\' && !inSingle && i+1 < len(cmd):
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '$' && inDouble && i+1 < len(cmd) && cmd[i+1] == '#':
			return true
		}
	}
	return false
}
