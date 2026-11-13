package cycle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/history"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

const (
	KitLockFile      = ".foreman-kit/kit.lock"
	AgentsConfig     = ".foreman-kit/agents.yaml"
	DefaultSkillsDir = ".agents/skills"
)

// AgentMeta describes a supported agent CLI.
type AgentMeta struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Tool   string `json:"tool"`
	Skills string `json:"skills"`
}

// SupportedAgents maps agent CLI name to its metadata.
var SupportedAgents = map[string]AgentMeta{
	"claude":       {Name: "claude", Label: "Claude Code", Tool: "claude", Skills: ".claude/skills"},
	"codex":        {Name: "codex", Label: "OpenAI Codex", Tool: "codex", Skills: ".agents/skills"},
	"copilot":      {Name: "copilot", Label: "GitHub Copilot", Tool: "github-copilot", Skills: ".github/skills"},
	"hermes":       {Name: "hermes", Label: "Hermes", Tool: "hermes", Skills: ".hermes/skills"},
	"opencode":     {Name: "opencode", Label: "OpenCode", Tool: "opencode", Skills: ".opencode/skills"},
	"agy":          {Name: "agy", Label: "Google Antigravity", Tool: "antigravity", Skills: ".agents/skills"},
	"kilo":         {Name: "kilo", Label: "Kilo Code", Tool: "kilocode", Skills: ".kilocode/skills"},
	"cline":        {Name: "cline", Label: "Cline", Tool: "cline", Skills: ".cline/skills"},
	"pi":           {Name: "pi", Label: "pi", Tool: "pi", Skills: ".pi/skills"},
	"omp":          {Name: "omp", Label: "oh-my-pi, a fork of pi", Tool: "oh-my-pi", Skills: ".omp/skills"},
	"crush":        {Name: "crush", Label: "Charm's Crush", Tool: "crush", Skills: ".crush/skills"},
	"cursor-agent": {Name: "cursor-agent", Label: "Cursor Agent", Tool: "cursor", Skills: ".cursor/skills"},
	"zcode":        {Name: "zcode", Label: "ZCode", Tool: "zcode", Skills: ".zcode/skills"},
}

var SupportedAgentOrder = []string{
	"claude", "codex", "copilot", "hermes", "opencode", "agy", "kilo",
	"cline", "pi", "omp", "crush", "cursor-agent", "zcode",
}

// RoleDescriptions says what the agent of each role does.
var RoleDescriptions = settings.RoleDoes

var ToolchainPinFiles = []string{
	"mise.toml", ".mise.toml", ".tool-versions",
	".ruby-version", ".nvmrc", ".node-version", ".python-version", ".go-version",
}

// GoFastTestCmd is the test gate written for a Go module: it tests only the packages of the
// changed .go files, and every package when go.mod or go.sum changed or no .go file did. The
// sed program is single-quoted: in double quotes the shell expands the "$#" of ".go$#" to 0,
// sed fails, and the gate silently tests every package on every change.
const GoFastTestCmd = `pkgs=$(printf '%s\n' "$FOREMANKIT_CHANGED_FILES" | sed -nE 's#^(.+)/[^/]+[.]go$#./\1#p; s#^[^/]+[.]go$#.#p' | sort -u | while read -r d; do [ -d "$d" ] && echo "$d"; done); case "$FOREMANKIT_CHANGED_FILES" in *go.mod*|*go.sum*) pkgs="" ;; esac; go test ${pkgs:-./...}`

var (
	repoNameRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	doubleBraceRe  = regexp.MustCompile(`\{\{[A-Z0-9_]+\}\}`)
	goPortListenRe = regexp.MustCompile(`(?:Listen(?:AndServe)?|Run|Addr:?)\s*\(?["'][^"']*:(\d{2,5})["']`)
	// The default of a PORT environment variable: envOr("PORT", "4000"), cmp.Or(os.Getenv("PORT"), "4000").
	goPortEnvDefaultRe = regexp.MustCompile(`"PORT"\s*\)?\s*,\s*"(\d{2,5})"`)
	// A Node server: http.createServer, https.createServer, Bun.serve, Deno.serve.
	nodeServerRe = regexp.MustCompile(`\bcreateServer\s*\(|\bBun\.serve\s*\(|\bDeno\.serve\s*\(`)
	// The port of a Node server: process.env.PORT || 3000, process.env.PORT ?? "3000", .listen(3000.
	nodePortEnvRe    = regexp.MustCompile(`process\.env\.PORT\s*(?:\|\||\?\?)\s*["']?(\d{2,5})`)
	nodePortListenRe = regexp.MustCompile(`\.listen\(\s*(\d{2,5})\b`)
)

// healthPaths are the health endpoints detection looks for in a repository's code, in order.
var healthPaths = []string{"/healthz", "/health", "/up", "/ping"}

// readSources concatenates the repository's source files with one of exts (tests included
// only when withTests), skipping dependency and build directories and files over 200 KB.
func readSources(dir string, exts []string, withTests bool) string {
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", "dist", "build", ".next", "coverage":
				if p != dir {
					return filepath.SkipDir
				}
			}
			return nil
		}
		name := d.Name()
		if !withTests && (strings.HasSuffix(name, "_test.go") || strings.Contains(name, ".test.") || strings.Contains(name, ".spec.")) {
			return nil
		}
		for _, ext := range exts {
			if strings.HasSuffix(name, ext) {
				if c, err := os.ReadFile(p); err == nil && len(c) < 200000 {
					b.Write(c)
					b.WriteByte('\n')
				}
				break
			}
		}
		return nil
	})
	return b.String()
}

// detectHealth returns the first of healthPaths quoted in code (", ' or `), or "".
func detectHealth(code string) string {
	for _, hp := range healthPaths {
		for _, q := range []string{`"`, `'`, "`"} {
			if strings.Contains(code, q+hp+q) {
				return hp
			}
		}
	}
	return ""
}

// yamlQuote renders s as a double-quoted YAML scalar. JSON string syntax is valid YAML, but
// json.Marshal escapes &, < and > for HTML (\u0026): unreadable in a gate a person reviews.
func yamlQuote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

// AgentsFileConfig is .foreman-kit/agents.yaml as a page or a command shows it.
type AgentsFileConfig struct {
	Agents []string            `json:"agents"`
	Roles  map[string][]string `json:"roles"`
	// Order is how each call picks among the agents of its role (agents.selection), and Permissions
	// which command of each agent runs (agents.permissions).
	Order       string `json:"order"`
	Permissions string `json:"permissions"`
	// ExtraArgs holds, per agent, the arguments the preflight's ping gets: those of the first role
	// that lists the agent.
	ExtraArgs map[string][]string `json:"extra_args"`
	// ExtraArgsByRole holds, per agent, the arguments each role gives it (roles.<role>.args).
	ExtraArgsByRole map[string]map[string][]string `json:"extra_args_by_role,omitempty"`
}

// LockFileEntry represents one file entry in .foreman-kit/kit.lock.
type LockFileEntry struct {
	Class  string `json:"class"`
	SHA256 string `json:"sha256"`
}

// KitLock represents the .foreman-kit/kit.lock file structure.
type KitLock struct {
	Name         string                   `json:"name"`
	Version      string                   `json:"version"`
	SourceCommit string                   `json:"source_commit"`
	BuiltAt      string                   `json:"built_at"`
	Agents       []string                 `json:"agents"`
	Files        map[string]LockFileEntry `json:"files"`
	InstalledAt  string                   `json:"installed_at"`
}

// KitOptions configures Init and Upgrade.
type KitOptions struct {
	Agents string
	Roles  map[string]string
	// Selection is how each call picks among the agents of its role ("" for what the file says).
	Selection     string
	Force         bool
	Version       string
	Commit        bool
	SeedOverrides map[string][]byte
}

// SelfUpgradeOptions configures SelfUpgrade.
type SelfUpgradeOptions struct {
	SourceDir  string
	BinaryPath string
	BuildFunc  func(ctx context.Context, sourceDir, destBin string) error
}

// ParseAgentsList parses a comma-separated list of agent names and validates against SupportedAgents.
func ParseAgentsList(list string) ([]string, error) {
	var out []string
	seen := make(map[string]bool)
	for _, raw := range strings.Split(list, ",") {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		if _, ok := SupportedAgents[name]; !ok {
			return nil, fmt.Errorf("unknown agent %s (supported: %s)", name, strings.Join(SupportedAgentOrder, ", "))
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no agent given")
	}
	return out, nil
}

// SkillsDirs returns the unique skill directories for the selected agents, always starting with .agents/skills.
func SkillsDirs(agents []string) []string {
	out := []string{DefaultSkillsDir}
	seen := map[string]bool{DefaultSkillsDir: true}
	for _, a := range agents {
		if meta, ok := SupportedAgents[a]; ok && meta.Skills != "" && !seen[meta.Skills] {
			seen[meta.Skills] = true
			out = append(out, meta.Skills)
		}
	}
	return out
}

// LoadAgentsConfig loads .foreman-kit/agents.yaml from root: the agents and the agents of each role as
// the file writes them (Init keeps them when it writes the file again, so a value for one run must
// not be among them: RoleEngines gives the agents a call of this run uses), and the selection and
// the permissions in effect.
func LoadAgentsConfig(root string) AgentsFileConfig {
	// The selection and the permissions are the ones an agent call of this run reads: what --set
	// gave it, else the file.
	res := AgentsFileConfig{
		Roles:       make(map[string][]string),
		Order:       config.SelectionTo(root, io.Discard),
		Permissions: config.PermissionsTo(root, io.Discard),
		ExtraArgs:   make(map[string][]string),
	}
	cfg := config.Load(root)
	if cfg == nil {
		return res
	}
	res.Agents = append(res.Agents, cfg.Agents...)
	for role, list := range cfg.Roles {
		res.Roles[role] = append([]string(nil), list...)
	}
	for role, byAgent := range cfg.RoleArgs {
		for agent, raw := range byAgent {
			args, err := config.ExtraArgsValue(raw)
			if err != nil || len(args) == 0 {
				continue
			}
			if res.ExtraArgsByRole == nil {
				res.ExtraArgsByRole = make(map[string]map[string][]string)
			}
			if res.ExtraArgsByRole[agent] == nil {
				res.ExtraArgsByRole[agent] = make(map[string][]string)
			}
			res.ExtraArgsByRole[agent][role] = args
		}
	}
	for agent := range res.ExtraArgsByRole {
		if args := config.ExtraArgsWithStderr(root, agent, io.Discard); len(args) > 0 {
			res.ExtraArgs[agent] = args
		}
	}
	return res
}

// RoleEngines returns the agents of role ("plan", "build", "cycle"), in order: the ones --set gave
// this run, else those of .foreman-kit/agents.yaml. A role without agents has none.
func RoleEngines(root, role string) []string {
	list, err := config.Engines(root, role)
	if err != nil {
		return nil
	}
	return list
}

func parseStringList(v any) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(s string) {
		for _, part := range strings.Split(s, ",") {
			p := strings.TrimSpace(part)
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	switch val := v.(type) {
	case string:
		add(val)
	case []string:
		for _, s := range val {
			add(s)
		}
	case []any:
		for _, item := range val {
			add(asString(item))
		}
	}
	return out
}

// WriteAgentsConfig writes .foreman-kit/agents.yaml in root for agents: each role's agents (the first agent
// for a role that names none), how a call picks among them ("" keeps what the file says) and the
// commands of every agent, as the file already holds them or as the kit ships them.
func WriteAgentsConfig(root string, agents []string, roles map[string][]string, selection string) error {
	rolesCopy := make(map[string][]string, len(roles))
	for _, r := range []string{"plan", "build", "cycle"} {
		list := roles[r]
		if len(list) == 0 && len(agents) > 0 {
			list = []string{agents[0]}
		}
		rolesCopy[r] = list
	}
	return config.Write(root, agents, rolesCopy, selection)
}

// resolveSelection returns the agents of an application and the agents of each role: what opts asks
// for, over what cur, its .foreman-kit/agents.yaml, already says.
func resolveSelection(cur AgentsFileConfig, opts KitOptions) ([]string, map[string][]string, error) {
	agentsStr := strings.TrimSpace(opts.Agents)
	if agentsStr == "" && len(cur.Agents) > 0 {
		agentsStr = strings.Join(cur.Agents, ",")
	}
	if agentsStr == "" {
		return nil, nil, fmt.Errorf("pass --agents with the agents this application uses (%s)", strings.Join(SupportedAgentOrder, ", "))
	}
	agents, err := ParseAgentsList(agentsStr)
	if err != nil {
		return nil, nil, err
	}
	agentSet := make(map[string]bool, len(agents))
	for _, a := range agents {
		agentSet[a] = true
	}

	roles := make(map[string][]string, 3)
	for _, role := range []string{"plan", "build", "cycle"} {
		var given []string
		if opts.Roles != nil && opts.Roles[role] != "" {
			given = parseStringList(opts.Roles[role])
		}
		for _, g := range given {
			if !agentSet[g] {
				return nil, nil, fmt.Errorf("--%s %s is not one of the agents (%s)", role, g, strings.Join(agents, ", "))
			}
		}
		var kept []string
		for _, k := range cur.Roles[role] {
			if agentSet[k] {
				kept = append(kept, k)
			}
		}
		var chosen []string
		if len(given) > 0 {
			chosen = given
		} else if len(kept) > 0 && len(agents) > 1 {
			chosen = kept
		} else {
			chosen = []string{firstThatHoldsARole(agents)}
		}
		roles[role] = chosen
	}
	return agents, roles, nil
}

// firstThatHoldsARole returns the first of agents whose shipped commands let it hold a role (not a
// detached agent without a stop command), or the first agent when none does: config check then
// says why it cannot.
func firstThatHoldsARole(agents []string) string {
	for _, a := range agents {
		if spec, known := config.ShippedCommands(a); !known || !spec.HoldsNoRole() {
			return a
		}
	}
	return agents[0]
}

func buildEmbeddedManifest() (skillsMap map[string][]byte, seedsMap map[string][]byte, err error) {
	skillsMap, err = assets.WalkEmbedded("skills")
	if err != nil {
		return nil, nil, err
	}
	rawSeeds, err := assets.WalkEmbedded("seeds")
	if err != nil {
		return nil, nil, err
	}
	seedsMap = make(map[string][]byte, len(rawSeeds))
	for rel, data := range rawSeeds {
		if strings.HasPrefix(rel, "log/") {
			continue
		}
		if rel == "gitignore" {
			rel = ".gitignore"
			cleaned := strings.ReplaceAll(string(data), "(log/.keep keeps the directory) ", "")
			lines := strings.Split(cleaned, "\n")
			var kept []string
			for _, l := range lines {
				if strings.TrimSpace(l) == "!/log/.keep" {
					continue
				}
				kept = append(kept, l)
			}
			data = []byte(strings.Join(kept, "\n"))
		}
		seedsMap[rel] = data
	}
	return skillsMap, seedsMap, nil
}

func buildKitPlan(skillsMap map[string][]byte, agents []string) map[string][]byte {
	plan := make(map[string][]byte)
	dirs := SkillsDirs(agents)
	for rel, data := range skillsMap {
		for _, d := range dirs {
			dest := filepath.ToSlash(filepath.Join(d, rel))
			plan[dest] = data
		}
	}
	return plan
}

func appendGitignore(target string) error {
	p := filepath.Join(target, ".gitignore")
	var present []string
	var raw string
	if b, err := os.ReadFile(p); err == nil {
		raw = string(b)
		for _, line := range strings.Split(raw, "\n") {
			present = append(present, strings.TrimSpace(line))
		}
	}
	presentSet := make(map[string]bool, len(present))
	for _, l := range present {
		presentSet[l] = true
	}

	rules := []struct {
		key       string
		spellings []string
	}{
		{"/.worktrees/", []string{".worktrees/", "/.worktrees/", ".worktrees", "/.worktrees"}},
		{"/log/*", []string{"log/", "/log/", "log", "/log", "log/*", "/log/*"}},
		{"*.new", []string{"*.new", "/*.new", "**/*.new"}},
	}

	var missing []string
	for _, r := range rules {
		found := false
		for _, sp := range r.spellings {
			if presentSet[sp] {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, r.key)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	var buf strings.Builder
	buf.WriteString(raw)
	if len(raw) > 0 && !strings.HasSuffix(raw, "\n") {
		buf.WriteByte('\n')
	}
	if len(raw) > 0 {
		buf.WriteByte('\n')
	}
	buf.WriteString("# Foreman: runtime state, and the <file>.new an upgrade proposes\n")
	for _, m := range missing {
		buf.WriteString(m + "\n")
	}
	return os.WriteFile(p, []byte(buf.String()), 0o644)
}

func writeKitLock(target, version string, agents []string, kitPlan, seedsMap map[string][]byte) error {
	if version == "" {
		version = assets.Version
	}
	files := make(map[string]LockFileEntry, len(kitPlan)+len(seedsMap))
	for rel, b := range seedsMap {
		files[rel] = LockFileEntry{
			Class:  "seed",
			SHA256: assets.SHA256Bytes(b),
		}
	}
	for dest, b := range kitPlan {
		files[dest] = LockFileEntry{
			Class:  "kit",
			SHA256: assets.SHA256Bytes(b),
		}
	}
	lock := KitLock{
		Name:         "foreman-kit",
		Version:      version,
		SourceCommit: assets.SourceCommit,
		BuiltAt:      assets.BuiltAt,
		Agents:       agents,
		Files:        files,
		InstalledAt:  time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(filepath.Join(target, KitLockFile), b, 0o644)
}

// ReadKitLock reads .foreman-kit/kit.lock from target.
func ReadKitLock(target string) (*KitLock, error) {
	b, err := os.ReadFile(filepath.Join(target, KitLockFile))
	if err != nil {
		return nil, err
	}
	var lock KitLock
	if err := json.Unmarshal(b, &lock); err != nil {
		return nil, err
	}
	return &lock, nil
}

// Drift checks all "kit" files in lock against disk and returns a map of relPath -> "missing" | "modified".
func Drift(target string, lock *KitLock) map[string]string {
	out := make(map[string]string)
	if lock == nil {
		return out
	}
	for rel, entry := range lock.Files {
		if entry.Class != "kit" {
			continue
		}
		full := filepath.Join(target, filepath.FromSlash(rel))
		b, err := os.ReadFile(full)
		if err != nil {
			out[rel] = "missing"
			continue
		}
		if assets.SHA256Bytes(b) != entry.SHA256 {
			out[rel] = "modified"
		}
	}
	return out
}

// Init installs the Foreman into target from embedded assets (internal/assets.FS),
// writing seeds, skills, .foreman-kit/agents.yaml, and .foreman-kit/kit.lock without creating log/.keep, and
// runs the two programs an application is kept with: `openspec init` for its specifications and
// `agent-memory init` for its memory. Without either on PATH it says how to install it and writes
// nothing.
func Init(ctx context.Context, target string, opts KitOptions, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	if fileExists(filepath.Join(target, KitLockFile)) {
		return fmt.Errorf("%s already has %s: use upgrade", target, KitLockFile)
	}
	if err := requireTools(); err != nil {
		return err
	}
	agents, roles, err := resolveSelection(LoadAgentsConfig(target), opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}

	skillsMap, seedsMap, err := buildEmbeddedManifest()
	if err != nil {
		return err
	}
	kitPlan := buildKitPlan(skillsMap, agents)

	var taken []string
	for dest, data := range kitPlan {
		full := filepath.Join(target, filepath.FromSlash(dest))
		if b, err := os.ReadFile(full); err == nil {
			if assets.SHA256Bytes(b) != assets.SHA256Bytes(data) {
				taken = append(taken, dest)
			}
		}
	}
	sort.Strings(taken)
	if len(taken) > 0 && !opts.Force {
		return fmt.Errorf("kit files already exist with other content: %s (use --force to replace them)", strings.Join(taken, ", "))
	}

	for dest, data := range kitPlan {
		full := filepath.Join(target, filepath.FromSlash(dest))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return err
		}
	}

	var seeded []string
	for rel, data := range seedsMap {
		// Never create log/.keep
		if strings.HasPrefix(rel, "log/") {
			continue
		}
		full := filepath.Join(target, filepath.FromSlash(rel))
		if !fileExists(full) {
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(full, data, 0o644); err != nil {
				return err
			}
			seeded = append(seeded, rel)
		}
	}

	// The base branch has no value of the kit's own: a checkout that is on a branch names it. An
	// application installed before its repository exists keeps the one the seed writes, to be set.
	if slices.Contains(seeded, settings.BaseBranch.FilePath()) {
		if branch, ok := gitx.CurrentBranch(ctx, target); ok && settings.BranchRe.MatchString(branch) {
			if err := config.Set(target, string(settings.BaseBranch), branch, false, io.Discard); err != nil {
				return err
			}
		}
	}
	if err := WriteAgentsConfig(target, agents, roles, opts.Selection); err != nil {
		return err
	}
	if err := appendGitignore(target); err != nil {
		return err
	}
	if err := ensureOpenSpecInit(ctx, target, agents); err != nil {
		return err
	}
	if err := initMemory(ctx, target, out); err != nil {
		return err
	}
	ver := opts.Version
	if ver == "" {
		ver = assets.Version
	}
	if err := writeKitLock(target, ver, agents, kitPlan, seedsMap); err != nil {
		return err
	}

	fmt.Fprintf(out, "kit: installed v%s into %s (%d kit files, %d seeds created)\n", ver, target, len(kitPlan), len(seeded))
	return nil
}

func ensureOpenSpecInit(ctx context.Context, target string, agents []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := os.MkdirAll(filepath.Join(target, "openspec", "specs"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(target, "openspec", "changes", "archive"), 0o755); err != nil {
		return err
	}
	cfgPath := filepath.Join(target, "openspec", "config.yaml")
	if !fileExists(cfgPath) {
		defaultCfg := "schema: spec-driven\n\n# Project context (optional)\n# This is shown to AI when creating artifacts.\n# Add your tech stack, conventions, style guides, domain knowledge, etc.\n"
		if err := os.WriteFile(cfgPath, []byte(defaultCfg), 0o644); err != nil {
			return err
		}
	}
	// FOREMANKIT_FORCE_BUILTIN=1 is how tests and simulations work without the openspec CLI: the
	// directories above are then all the application gets.
	if os.Getenv("FOREMANKIT_FORCE_BUILTIN") == "1" {
		return nil
	}
	if _, err := exec.LookPath("openspec"); err != nil {
		return fmt.Errorf("openspec is not installed. Install it, then run this again:\n%s", openspecInstallHelp)
	}
	tools := strings.Join(config.Tools(agents), ",")
	cmd := exec.CommandContext(ctx, "openspec", "init", "--tools", tools, "--no-animation", "--no-copilot-cloud", ".")
	cmd.Dir = target
	if err := cmd.Run(); err != nil {
		// An openspec from before those two options.
		cmdFallback := exec.CommandContext(ctx, "openspec", "init", "--tools", tools, ".")
		cmdFallback.Dir = target
		if said, err := cmdFallback.CombinedOutput(); err != nil {
			return fmt.Errorf("openspec init failed in %s: %v\n%s", target, err, strings.TrimSpace(string(said)))
		}
	}
	return nil
}

// Upgrade upgrades an existing kit installation in target from embedded assets.
func Upgrade(ctx context.Context, target string, opts KitOptions, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	if opts.Selection != "" {
		return fmt.Errorf("upgrade takes no --selection: foreman-kit config set %s <selection> in the application", settings.AgentSelection)
	}
	// An application installed before the kit was called Foreman: its files move into .foreman-kit/
	// first, on the branch of this update, so that everything below reads them where they are now.
	var movedLayout []string
	isGitEarly := isGitRepoWithHead(ctx, target)
	if foremanLayoutPending(target) {
		if isGitEarly {
			if dirty, err := gitx.RunGit(ctx, target, "status", "--porcelain", "--ignore-submodules=dirty"); err == nil && strings.TrimSpace(dirty) != "" && !opts.Force {
				return fmt.Errorf("%s has uncommitted changes; commit or stash them first", target)
			}
			earlyVer := opts.Version
			if earlyVer == "" {
				earlyVer = assets.Version
			}
			if _, err := gitx.RunGit(ctx, target, "checkout", "-q", "-B", fmt.Sprintf("kit-upgrade-v%s", earlyVer)); err != nil {
				return err
			}
		}
		fmt.Fprintln(out, "kit: openspec-kit is Foreman now")
		var err error
		if movedLayout, err = moveToForeman(ctx, target, isGitEarly, out); err != nil {
			return err
		}
	}
	oldLock, err := ReadKitLock(target)
	if err != nil {
		return fmt.Errorf("%s has no %s: use init", target, KitLockFile)
	}
	// An application's memory is kept with agent-memory: an update that has to create it, or to take
	// in what the application kept before it, needs the program, and says so before it writes.
	if memoryMigrationPending(target) {
		if err := memory.Require(); err != nil {
			return err
		}
	}
	// The configuration files of an application written for an older foreman-kit are rewritten
	// below. Its agents and the agents of each role are read here the way they will be written, so
	// that an upgrade never mistakes the former layout for a role without agents.
	early, err := config.MigrateConfig(target)
	if err != nil {
		return err
	}
	curAgentsCfg := AgentsFileConfig{Roles: map[string][]string{}}
	if cfg := early.Agents(target); cfg != nil {
		curAgentsCfg.Agents = cfg.Agents
		for role, list := range cfg.Roles {
			curAgentsCfg.Roles[role] = list
		}
	}
	agents, roles, err := resolveSelection(curAgentsCfg, opts)
	if err != nil {
		return err
	}

	skillsMap, seedsMap, err := buildEmbeddedManifest()
	if err != nil {
		return err
	}
	for k, v := range opts.SeedOverrides {
		seedsMap[k] = v
	}
	kitPlan := buildKitPlan(skillsMap, agents)

	var dropped []string
	droppedSet := make(map[string]bool)
	for rel, entry := range oldLock.Files {
		if entry.Class == "kit" {
			if _, ok := kitPlan[rel]; !ok {
				dropped = append(dropped, rel)
				droppedSet[rel] = true
			}
		}
	}
	sort.Strings(dropped)

	edited := Drift(target, oldLock)
	for rel, state := range edited {
		if droppedSet[rel] && state == "missing" {
			delete(edited, rel)
		}
	}

	ver := opts.Version
	if ver == "" {
		ver = assets.Version
	}
	reselected := !equalStringSlices(agents, oldLock.Agents) || !equalRolesMap(roles, curAgentsCfg.Roles)

	seedChanged := false
	for rel, data := range seedsMap {
		if strings.HasPrefix(rel, "log/") {
			continue
		}
		if oldEntry, ok := oldLock.Files[rel]; ok && oldEntry.SHA256 != assets.SHA256Bytes(data) {
			seedChanged = true
			break
		}
	}

	// A skill of the kit that changed is installed even at the same version: during development the
	// version stays while its files change, and comparing seeds alone left such a skill as it was.
	// A file new to an agent the application already had counts (a skill the kit added); the files of
	// an agent chosen now are the reselection's, and leave the update named after the agents.
	kitChanged := false
	oldDirs := SkillsDirs(oldLock.Agents)
	for dest, data := range kitPlan {
		oldEntry, ok := oldLock.Files[dest]
		if ok && oldEntry.SHA256 != assets.SHA256Bytes(data) {
			kitChanged = true
			break
		}
		if !ok && slices.ContainsFunc(oldDirs, func(d string) bool { return strings.HasPrefix(dest, filepath.ToSlash(d)+"/") }) {
			kitChanged = true
			break
		}
	}

	// Names this version no longer reads (see renameEvaluator and config.MigrateConfig) are rewritten
	// even at the same version.
	renamePending := evaluatorRenamePending(target) || history.NeedsCompaction(target) || early.Pending() || config.RepoConfigsNeedRewrite(target) || memoryMigrationPending(target) || len(movedLayout) > 0

	if oldLock.Version == ver && len(edited) == 0 && !reselected && len(dropped) == 0 && !seedChanged && !kitChanged && !renamePending {
		fmt.Fprintf(out, "kit: already at v%s; nothing to do\n", ver)
		return nil
	}
	if len(edited) > 0 && !opts.Force {
		var parts []string
		for p, s := range edited {
			parts = append(parts, fmt.Sprintf("%s (%s)", p, s))
		}
		sort.Strings(parts)
		return fmt.Errorf("kit files were edited in place: %s. Move the edits upstream to the framework, or pass --force to discard them", strings.Join(parts, ", "))
	}

	sameVersion := oldLock.Version == ver && !seedChanged && !kitChanged && !renamePending
	branch := fmt.Sprintf("kit-upgrade-v%s", ver)
	if sameVersion && reselected {
		branch = fmt.Sprintf("kit-agents-%s", strings.Join(agents, "-"))
	}
	isGit := isGitRepoWithHead(ctx, target)
	// Where the checkout was, to return to it when the memory cannot be taken in: that step fails on
	// what the application holds, with nothing written, and leaves it where a person can mend it.
	cameFrom := ""
	if isGit {
		if name, ok := gitx.CurrentBranch(ctx, target); ok {
			cameFrom = name
		}
	}
	if isGit {
		// What a repository holds uncommitted is its own: an upgrade writes there only the
		// repo.config.yaml it rewrites, which is committed in that repository.
		if dirty, err := gitx.RunGit(ctx, target, "status", "--porcelain", "--ignore-submodules=dirty"); err == nil && strings.TrimSpace(dirty) != "" && !opts.Force && len(movedLayout) == 0 {
			return fmt.Errorf("%s has uncommitted changes; commit or stash them first", target)
		}
		if _, err := gitx.RunGit(ctx, target, "checkout", "-q", "-B", branch); err != nil {
			return err
		}
	}

	var renameNotes bytes.Buffer
	renamed, err := renameEvaluator(ctx, target, oldLock, isGit, &renameNotes)
	if err != nil {
		return err
	}
	renamed = append(renamed, movedLayout...)
	// The memory moves before the files that name it are rewritten or proposed: MEMORY.md is a seed
	// of the kit that installed it, and is gone from the lock with the file.
	movedMemory, err := migrateMemory(ctx, target, oldLock, isGit, &renameNotes)
	if err != nil {
		if isGit && cameFrom != "" && cameFrom != branch {
			_, _ = gitx.RunGit(ctx, target, "checkout", "-q", cameFrom)
		}
		return err
	}
	renamed = append(renamed, movedMemory...)
	// The files are rewritten after the rename: the evaluator's gates are then under their new name.
	migration, err := config.MigrateConfig(target)
	if err != nil {
		return err
	}
	migrated := make(map[string]bool, len(migration.Changed))
	for _, rel := range migration.Changed {
		full := filepath.Join(target, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, migration.Files[rel], 0o644); err != nil {
			return err
		}
		migrated[rel] = true
	}
	renamed = append(renamed, migration.Changed...)
	for _, note := range migration.Notes {
		fmt.Fprintf(&renameNotes, "  %s\n", note)
	}
	// The rotated history files an older kit left, one small file per rotation, are regrouped so that
	// each holds history.MaxLines lines.
	compacted, err := history.Compact(target)
	if err != nil {
		fmt.Fprintf(&renameNotes, "  the rotated history was left as it was: %v\n", err)
	} else if len(compacted) > 0 {
		fmt.Fprintf(&renameNotes, "  the rotated history was regrouped into files of %d lines\n", history.MaxLines)
	}
	compactedHistory := len(compacted) > 0
	// The repo.config.yaml of each repository checked out here is rewritten the same way. A
	// repository is a git repository of its own: its file is written, and committing it there is for
	// a person, who is told how once the upgrade is done.
	repoMigration := config.MigrateRepoConfigs(target)
	for rel, data := range repoMigration.Files {
		if err := os.WriteFile(filepath.Join(target, filepath.FromSlash(rel)), data, 0o644); err != nil {
			return err
		}
	}
	for _, note := range repoMigration.Notes {
		fmt.Fprintf(&renameNotes, "%s\n", note)
	}

	for dest, data := range kitPlan {
		full := filepath.Join(target, filepath.FromSlash(dest))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return err
		}
	}
	for _, rel := range dropped {
		_ = os.Remove(filepath.Join(target, filepath.FromSlash(rel)))
	}
	newAgentSet := make(map[string]bool, len(agents))
	for _, a := range agents {
		newAgentSet[a] = true
	}
	var extraPrune []string
	for _, oldAg := range oldLock.Agents {
		if newAgentSet[oldAg] {
			continue
		}
		meta, ok := SupportedAgents[oldAg]
		if !ok || meta.Skills == "" || meta.Skills == DefaultSkillsDir {
			continue
		}
		skillsDir := filepath.Join(target, filepath.FromSlash(meta.Skills))
		if entries, err := os.ReadDir(skillsDir); err == nil {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "openspec-") || strings.HasPrefix(e.Name(), "foreman-") || e.Name() == ".openspec-target" {
					_ = os.RemoveAll(filepath.Join(skillsDir, e.Name()))
				}
			}
		}
		agentRootRel := filepath.ToSlash(filepath.Dir(filepath.FromSlash(meta.Skills)))
		if agentRootRel != "." && agentRootRel != "" {
			cmdDir := filepath.Join(target, filepath.FromSlash(agentRootRel), "commands")
			if entries, err := os.ReadDir(cmdDir); err == nil {
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), "opsx-") || strings.HasPrefix(e.Name(), "openspec-") {
						_ = os.RemoveAll(filepath.Join(cmdDir, e.Name()))
					}
				}
			}
			extraPrune = append(extraPrune, agentRootRel+"/commands/dummy", meta.Skills+"/dummy")
		}
	}
	pruneEmptyDirs(target, append(dropped, extraPrune...))

	var created []string
	var proposed []string
	for rel, data := range seedsMap {
		if strings.HasPrefix(rel, "log/") {
			continue
		}
		full := filepath.Join(target, filepath.FromSlash(rel))
		newSHA := assets.SHA256Bytes(data)
		if !fileExists(full) {
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err == nil {
				_ = os.WriteFile(full, data, 0o644)
				created = append(created, rel)
			}
		} else if migrated[rel] {
			// Rewritten above from this very seed, with the application's values in it.
			continue
		} else if oldEntry, ok := oldLock.Files[rel]; ok && oldEntry.SHA256 != newSHA {
			// A file that already reads like the new template has nothing left to merge.
			if cur, err := os.ReadFile(full); err == nil && assets.SHA256Bytes(cur) == newSHA {
				continue
			}
			newPath := full + ".new"
			_ = os.WriteFile(newPath, data, 0o644)
			proposed = append(proposed, rel+".new")
		}
	}

	if reselected {
		_ = WriteAgentsConfig(target, agents, roles, "")
		_ = ensureOpenSpecInit(ctx, target, agents)
	} else if !fileExists(filepath.Join(target, "openspec", "config.yaml")) {
		_ = ensureOpenSpecInit(ctx, target, agents)
	}
	_ = appendGitignore(target)
	if err := writeKitLock(target, ver, agents, kitPlan, seedsMap); err != nil {
		return err
	}

	verTransition := fmt.Sprintf("v%s -> v%s", oldLock.Version, ver)
	if oldLock.Version == ver {
		verTransition = "v" + ver
	}
	fmt.Fprintf(out, "kit: %s: %d kit files, %d removed, %d seeds created\n",
		verTransition, len(kitPlan), len(dropped), len(created))
	_, _ = out.Write(renameNotes.Bytes())
	for _, p := range proposed {
		fmt.Fprintf(out, "  review %s: the template changed; merge what you need, then delete it\n", p)
	}

	if isGit {
		if trackedNew, err := gitx.RunGit(ctx, target, "ls-files", "--", "*.new"); err == nil && strings.TrimSpace(trackedNew) != "" {
			var toRm []string
			for _, f := range strings.Split(strings.TrimSpace(trackedNew), "\n") {
				if f = strings.TrimSpace(f); f != "" {
					toRm = append(toRm, f)
				}
			}
			if len(toRm) > 0 {
				rmArgs := append([]string{"rm", "--cached", "--quiet", "--"}, toRm...)
				_, _ = gitx.RunGit(ctx, target, rmArgs...)
			}
		}
		if len(dropped) > 0 {
			rmDropped := append([]string{"rm", "--cached", "--ignore-unmatch", "--quiet", "--"}, dropped...)
			_, _ = gitx.RunGit(ctx, target, rmDropped...)
		}
		for _, oldAg := range oldLock.Agents {
			if newAgentSet[oldAg] {
				continue
			}
			if meta, ok := SupportedAgents[oldAg]; ok && meta.Skills != "" && meta.Skills != DefaultSkillsDir {
				_, _ = gitx.RunGit(ctx, target, "rm", "-r", "--cached", "--ignore-unmatch", "--quiet", "--", meta.Skills)
				agentRootRel := filepath.ToSlash(filepath.Dir(filepath.FromSlash(meta.Skills)))
				if agentRootRel != "." && agentRootRel != "" {
					_, _ = gitx.RunGit(ctx, target, "rm", "-r", "--cached", "--ignore-unmatch", "--quiet", "--", agentRootRel+"/commands")
				}
			}
		}
		candidates := []string{KitLockFile, ".gitignore", AgentsConfig}
		candidates = append(candidates, SkillsDirs(agents)...)
		for _, a := range agents {
			if meta, ok := SupportedAgents[a]; ok && meta.Skills != "" {
				agentRootRel := filepath.ToSlash(filepath.Dir(filepath.FromSlash(meta.Skills)))
				if agentRootRel != "." && agentRootRel != "" {
					candidates = append(candidates, agentRootRel+"/commands")
				}
			}
		}
		candidates = append(candidates, created...)
		candidates = append(candidates, renamed...)
		var stagePaths []string
		seenStage := make(map[string]bool)
		for _, p := range candidates {
			if p == "" || seenStage[p] {
				continue
			}
			seenStage[p] = true
			if exists(filepath.Join(target, filepath.FromSlash(p))) {
				stagePaths = append(stagePaths, p)
			}
		}
		if len(stagePaths) > 0 {
			addArgs := append([]string{"add", "-A", "--"}, stagePaths...)
			_, _ = gitx.RunGit(ctx, target, addArgs...)
		}
		if compactedHistory {
			// The regrouped history removes files: a glob pathspec stages their removal too.
			_, _ = gitx.RunGit(ctx, target, "add", "-A", "--", ":(glob).foreman-kit/agent_history.*.json")
		}
		msg := fmt.Sprintf("chore(kit): upgrade the OpenSpec kit to v%s", ver)
		if sameVersion && reselected {
			msg = fmt.Sprintf("chore(kit): use the agents %s", strings.Join(agents, ", "))
		}
		if _, err := gitx.RunGit(ctx, target, "diff", "--cached", "--quiet"); err != nil {
			_, _ = gitx.RunGit(ctx, target, "commit", "--quiet", "-m", msg)
			fmt.Fprintf(out, "committed on branch %s: review it, then merge\n", branch)
		} else {
			// Only the repositories had something to rewrite: the umbrella holds nothing new.
			fmt.Fprintf(out, "nothing to commit in the umbrella: branch %s holds what it held\n", branch)
		}
	}
	if len(repoMigration.Repos) > 0 {
		fmt.Fprintf(out, "%s was rewritten in %s. Each one is its own repository: commit the file there and push it to its base branch.\n",
			config.RepoConfigFile, strings.Join(repoMigration.Repos, ", "))
		for _, rel := range repoMigration.Repos {
			base := "main"
			if repo, err := gitx.Open(target); err == nil {
				base = repo.ChildBaseBranch(rel)
			}
			// A commit made on no branch is left behind when foreman-kit sync returns the repository
			// to the commit the umbrella records, and the file with it: it is made on the base branch.
			childDir := filepath.Join(target, filepath.FromSlash(rel))
			if _, ok := gitx.CurrentBranch(ctx, childDir); isGitRepoWithHead(ctx, childDir) && !ok {
				fmt.Fprintf(out, "  git -C %s switch %s    (it is on no branch: foreman-kit sync would leave a commit made there behind)\n", rel, base)
			}
			fmt.Fprintf(out, "  git -C %s commit -m \"chore: %s with the keys of foreman-kit v%s\" %s && git -C %s push origin HEAD:%s\n",
				rel, config.RepoConfigFile, ver, config.RepoConfigFile, rel, base)
		}
		fmt.Fprintln(out, "A push refused because the base branch moved: pull it with --rebase in that repository, then push again.")
		fmt.Fprintln(out, "Until then every change reads the file of this checkout, so nothing waits for the push; a clone made before it still holds the former keys.")
	}
	return nil
}

func isGitRepoWithHead(ctx context.Context, dir string) bool {
	if !exists(filepath.Join(dir, ".git")) {
		return false
	}
	_, err := gitx.RunGit(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD")
	return err == nil
}

func equalRolesMap(a, b map[string][]string) bool {
	for _, r := range []string{"plan", "build", "cycle"} {
		if !equalStringSlices(a[r], b[r]) {
			return false
		}
	}
	return true
}

func pruneEmptyDirs(target string, relPaths []string) {
	seen := make(map[string]bool)
	var dirs []string
	for _, rel := range relPaths {
		d := filepath.Dir(filepath.FromSlash(rel))
		for d != "." && d != "/" && d != "" {
			if !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
			}
			d = filepath.Dir(d)
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i]) > len(dirs[j])
	})
	for _, d := range dirs {
		full := filepath.Join(target, d)
		entries, err := os.ReadDir(full)
		if err == nil && len(entries) == 0 {
			_ = os.Remove(full)
		}
	}
}

// Verify checks whether installed kit files in target match .foreman-kit/kit.lock.
func Verify(target string, out io.Writer) (bool, error) {
	if out == nil {
		out = os.Stdout
	}
	lock, err := ReadKitLock(target)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "kit: no %s (not installed with init); not verified\n", KitLockFile)
			return true, nil
		}
		return false, err
	}

	edited := Drift(target, lock)
	curAgents := LoadAgentsConfig(target).Agents
	if len(lock.Agents) > 0 && len(curAgents) > 0 && !equalStringSlices(curAgents, lock.Agents) {
		edited[AgentsConfig] = fmt.Sprintf("lists %s, skills installed for %s: run foreman-kit upgrade --agents ...",
			strings.Join(curAgents, ", "), strings.Join(lock.Agents, ", "))
	}

	kitCount := 0
	for _, f := range lock.Files {
		if f.Class == "kit" {
			kitCount++
		}
	}

	if len(edited) == 0 {
		fmt.Fprintf(out, "kit: OK (v%s, %d kit files match %s)\n", lock.Version, kitCount, KitLockFile)
		return true, nil
	}

	var parts []string
	for p, s := range edited {
		parts = append(parts, fmt.Sprintf("%s (%s)", p, s))
	}
	sort.Strings(parts)
	fmt.Fprintf(out, "kit: DRIFT from v%s: %s\n", lock.Version, strings.Join(parts, ", "))
	return false, nil
}

func resolveSelfUpgradeSourceDir(sourceDir string) string {
	isKitCheckout := func(dir string) bool {
		return dir != "" &&
			fileExists(filepath.Join(dir, "go.mod")) &&
			fileExists(filepath.Join(dir, "cmd", "foreman-kit", "main.go"))
	}
	if isKitCheckout(sourceDir) {
		return sourceDir
	}
	if sourceDir != "" && fileExists(filepath.Join(sourceDir, "go.mod")) {
		return sourceDir
	}
	var candidates []string
	if envHome := strings.TrimSpace(os.Getenv("FOREMANKIT_HOME")); envHome != "" {
		candidates = append(candidates, envHome, filepath.Join(envHome, "src"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".foreman-kit"), filepath.Join(home, ".foreman-kit", "src"))
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		for d := cwd; ; {
			candidates = append(candidates, d)
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
	}
	for _, cand := range candidates {
		if isKitCheckout(cand) {
			return cand
		}
	}
	return sourceDir
}

// SelfUpgrade rebuilds or updates the foreman-kit binary.
func SelfUpgrade(ctx context.Context, opts SelfUpgradeOptions, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	destBin := opts.BinaryPath
	if destBin == "" {
		if exe, err := os.Executable(); err == nil {
			destBin = exe
		}
	}
	srcDir := opts.SourceDir
	if srcDir == "" {
		srcDir = resolveSelfUpgradeSourceDir("")
	}
	buildFn := opts.BuildFunc
	if buildFn == nil && srcDir != "" && fileExists(filepath.Join(srcDir, "go.mod")) {
		buildFn = func(ctx context.Context, sourceDir, destBin string) error {
			ver := assets.Version
			sha := assets.SourceCommit
			// The version of the source being built, detected from its git tags.
			if script := filepath.Join(sourceDir, "scripts", "version.sh"); fileExists(script) {
				vcmd := exec.CommandContext(ctx, "sh", script)
				vcmd.Dir = sourceDir
				if vOut, err := vcmd.Output(); err == nil && strings.TrimSpace(string(vOut)) != "" {
					ver = strings.TrimSpace(string(vOut))
				}
			}
			if sOut, err := gitx.RunGit(ctx, sourceDir, "rev-parse", "--short=12", "HEAD"); err == nil && strings.TrimSpace(sOut) != "" {
				sha = strings.TrimSpace(sOut)
			}
			ts := time.Now().UTC().Format(time.RFC3339)
			ldflags := fmt.Sprintf(
				"-X github.com/mayeco/foreman-kit/internal/assets.Version=%s -X github.com/mayeco/foreman-kit/internal/assets.SourceCommit=%s -X github.com/mayeco/foreman-kit/internal/assets.BuiltAt=%s",
				ver, sha, ts,
			)
			tmpBin := destBin + ".new"
			cmd := exec.CommandContext(ctx, "go", "build", "-ldflags", ldflags, "-o", tmpBin, "./cmd/foreman-kit")
			cmd.Dir = sourceDir
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if bOut, err := cmd.CombinedOutput(); err != nil {
				_ = os.Remove(tmpBin)
				return fmt.Errorf("go build failed: %w (%s)", err, strings.TrimSpace(string(bOut)))
			}
			if err := os.Rename(tmpBin, destBin); err != nil {
				_ = os.Remove(tmpBin)
				return err
			}
			return nil
		}
	}
	if buildFn != nil {
		if err := buildFn(ctx, srcDir, destBin); err != nil {
			return err
		}
		fmt.Fprintf(out, "self-upgrade: updated %s to v%s\n", destBin, assets.Version)
		return nil
	}
	fmt.Fprintf(out, "self-upgrade: foreman-kit v%s (%s) is up to date\n", assets.Version, assets.SourceCommit)
	return nil
}

// RepoDetection holds auto-detected stack, kind, commands, and pins for a child repository.
type RepoDetection struct {
	Stack          string            `json:"stack"`
	Kind           string            `json:"kind"`
	Setup          string            `json:"setup"`
	DevCommand     string            `json:"dev_command"`
	Port           string            `json:"port"`
	HealthEndpoint string            `json:"health_endpoint"`
	Gates          map[string]string `json:"gates"`
	Regression     string            `json:"regression"`
	Pins           []string          `json:"pins"`
}

// DetectRepoStack inspects a repository directory (package.json, Gemfile, pyproject.toml, go.mod, Cargo.toml)
// and detects its stack, kind, gates, dev_command, port, and toolchain pins.
func DetectRepoStack(dir string) RepoDetection {
	has := func(rel string) bool {
		return exists(filepath.Join(dir, filepath.FromSlash(rel)))
	}
	found := RepoDetection{
		Gates: make(map[string]string),
	}
	for _, pin := range ToolchainPinFiles {
		if has(pin) {
			found.Pins = append(found.Pins, pin)
		}
	}

	switch {
	case has("package.json"):
		detectNodeRepo(dir, has, &found)
	case has("Gemfile"):
		detectRubyRepo(dir, has, &found)
	case has("pyproject.toml") || has("requirements.txt"):
		detectPythonRepo(dir, has, &found)
	case has("go.mod"):
		detectGoRepo(dir, has, &found)
	case has("Cargo.toml"):
		detectRustRepo(dir, has, &found)
	}
	return found
}

func detectNodeRepo(dir string, has func(string) bool, found *RepoDetection) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		found.Stack = "node"
		return
	}
	var pkg map[string]any
	if err := json.Unmarshal(b, &pkg); err != nil {
		found.Stack = "node"
		return
	}
	scripts, _ := pkg["scripts"].(map[string]any)
	deps := make(map[string]any)
	if d, ok := pkg["dependencies"].(map[string]any); ok {
		for k, v := range d {
			deps[k] = v
		}
	}
	if dd, ok := pkg["devDependencies"].(map[string]any); ok {
		for k, v := range dd {
			deps[k] = v
		}
	}

	pm := "npm"
	switch {
	case has("pnpm-lock.yaml"):
		pm = "pnpm"
	case has("yarn.lock"):
		pm = "yarn"
	case has("bun.lockb") || has("bun.lock"):
		pm = "bun"
	}
	runCmd := func(s string) string {
		if pm == "npm" {
			return "npm run " + s
		}
		return pm + " " + s
	}

	stack := "node"
	if deps["next"] != nil {
		stack = "nextjs"
	} else if deps["@nestjs/core"] != nil {
		stack = "nestjs"
	} else if deps["vite"] != nil {
		stack = "vite"
	}
	ts := deps["typescript"] != nil || has("tsconfig.json")
	if ts {
		found.Stack = stack + "-typescript"
	} else {
		found.Stack = stack
	}

	code := readSources(dir, []string{".js", ".mjs", ".cjs", ".ts", ".mts"}, false)
	if stack != "node" || deps["express"] != nil || deps["fastify"] != nil || deps["koa"] != nil || deps["@hapi/hapi"] != nil {
		found.Kind = "web"
	} else if pkg["bin"] != nil {
		found.Kind = "cli"
	} else if nodeServerRe.MatchString(code) {
		// A dependency-free server on node:http (or Bun/Deno): still an application opened in a browser.
		found.Kind = "web"
	}

	if scripts["lint"] != nil {
		found.Gates["lint"] = runCmd("lint")
	}
	execMap := map[string]string{"npm": "npx", "pnpm": "pnpm exec", "yarn": "yarn", "bun": "bunx"}
	for _, tcName := range []string{"typecheck", "type-check", "check-types", "tsc"} {
		if scripts[tcName] != nil {
			found.Gates["typecheck"] = runCmd(tcName)
			break
		}
	}
	if found.Gates["typecheck"] == "" && ts {
		found.Gates["typecheck"] = execMap[pm] + " tsc --noEmit"
	}
	if tCmd := asString(scripts["test"]); tCmd != "" && !strings.Contains(tCmd, "no test specified") {
		found.Gates["test"] = runCmd("test")
	}
	if scripts["build"] != nil {
		found.Gates["build"] = runCmd("build")
	}
	for _, dName := range []string{"dev", "start", "serve"} {
		if scripts[dName] != nil {
			found.DevCommand = runCmd(dName)
			break
		}
	}
	portMap := map[string]string{"nextjs": "3000", "vite": "5173", "nestjs": "3000"}
	found.Port = portMap[stack]
	if found.Kind == "web" {
		if m := nodePortEnvRe.FindStringSubmatch(code); len(m) == 2 {
			found.Port = m[1]
		} else if m := nodePortListenRe.FindStringSubmatch(code); len(m) == 2 {
			found.Port = m[1]
		}
		if hp := detectHealth(code); hp != "" && found.Port != "" {
			found.HealthEndpoint = "http://localhost:" + found.Port + hp
		}
	}
}

func detectRubyRepo(dir string, has func(string) bool, found *RepoDetection) {
	b1, _ := os.ReadFile(filepath.Join(dir, "Gemfile"))
	b2, _ := os.ReadFile(filepath.Join(dir, "Gemfile.lock"))
	gems := string(b1) + "\n" + string(b2)
	rails := has("config/application.rb") || strings.Contains(gems, "'rails'") || strings.Contains(gems, `"rails"`)
	if rails {
		found.Stack = "rails"
	} else {
		found.Stack = "ruby"
	}
	if rails || strings.Contains(gems, "sinatra") || strings.Contains(gems, "hanami") || strings.Contains(gems, "roda") {
		found.Kind = "web"
	} else if has("exe") || strings.Contains(gems, "thor") || strings.Contains(gems, "dry-cli") {
		found.Kind = "cli"
	}
	if strings.Contains(gems, "rubocop") {
		found.Gates["lint"] = "bundle exec rubocop"
	}
	if strings.Contains(gems, "sorbet") {
		found.Gates["typecheck"] = "bundle exec srb tc"
	}
	if has("spec") && strings.Contains(gems, "rspec") {
		found.Gates["test"] = "bundle exec rspec"
	} else if rails {
		found.Gates["test"] = "bin/rails test"
	} else if has("test") {
		found.Gates["test"] = "bundle exec rake test"
	}
	if rails {
		found.Setup = "RAILS_ENV=test bin/rails db:prepare"
		if has("bin/dev") {
			found.DevCommand = "bin/dev"
		} else {
			found.DevCommand = "bin/rails server"
		}
		found.Port = "3000"
		if routes, err := os.ReadFile(filepath.Join(dir, "config", "routes.rb")); err == nil && strings.Contains(string(routes), "rails/health") {
			found.HealthEndpoint = "http://localhost:3000/up"
		}
	}
}

func detectPythonRepo(dir string, has func(string) bool, found *RepoDetection) {
	var text strings.Builder
	for _, f := range []string{"pyproject.toml", "requirements.txt", "requirements-dev.txt"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			text.Write(b)
			text.WriteByte('\n')
		}
	}
	all := text.String()
	lower := strings.ToLower(all)
	runPrefix := ""
	if has("uv.lock") {
		runPrefix = "uv run "
	} else if has("poetry.lock") {
		runPrefix = "poetry run "
	}
	django := strings.Contains(lower, "django") || has("manage.py")
	switch {
	case django:
		found.Stack = "python-django"
	case strings.Contains(lower, "fastapi"):
		found.Stack = "python-fastapi"
	case strings.Contains(lower, "flask"):
		found.Stack = "python-flask"
	default:
		found.Stack = "python"
	}
	if found.Stack != "python" {
		found.Kind = "web"
	} else if strings.Contains(all, "[project.scripts]") || strings.Contains(lower, "click") || strings.Contains(lower, "typer") {
		found.Kind = "cli"
	}
	if strings.Contains(lower, "ruff") {
		found.Gates["lint"] = runPrefix + "ruff check ."
	}
	if strings.Contains(lower, "mypy") {
		found.Gates["typecheck"] = runPrefix + "mypy ."
	}
	if strings.Contains(lower, "pytest") {
		found.Gates["test"] = runPrefix + "pytest"
	} else if django {
		found.Gates["test"] = runPrefix + "python manage.py test"
	}
	if django {
		found.DevCommand = runPrefix + "python manage.py runserver"
		found.Port = "8000"
	}
}

func detectGoRepo(dir string, has func(string) bool, found *RepoDetection) {
	modBytes, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
	mod := string(modBytes)

	code := readSources(dir, []string{".go"}, false)

	webLibs := []struct{ pkg, label string }{
		{"github.com/gin-gonic/gin", "gin"},
		{"github.com/labstack/echo", "echo"},
		{"github.com/go-chi/chi", "chi"},
		{"github.com/gofiber/fiber", "fiber"},
		{"github.com/gorilla/mux", "gorilla"},
	}
	cliLibs := []struct{ pkg, label string }{
		{"github.com/spf13/cobra", "cobra"},
		{"github.com/urfave/cli", "urfave"},
		{"github.com/charmbracelet/bubbletea", "bubbletea"},
	}

	web := ""
	for _, wl := range webLibs {
		if strings.Contains(mod, wl.pkg) {
			web = wl.label
			break
		}
	}
	if web == "" && (strings.Contains(code, "http.ListenAndServe") || strings.Contains(code, "http.Server{") || strings.Contains(code, "&http.Server{")) {
		web = "net-http"
	}
	cli := ""
	for _, cl := range cliLibs {
		if strings.Contains(mod, cl.pkg) {
			cli = cl.label
			break
		}
	}

	var mains []string
	if entries, err := os.ReadDir(filepath.Join(dir, "cmd")); err == nil {
		for _, e := range entries {
			if e.IsDir() && fileExists(filepath.Join(dir, "cmd", e.Name(), "main.go")) {
				mains = append(mains, "./cmd/"+e.Name())
			}
		}
	}
	sort.Strings(mains)
	mainPkg := ""
	if len(mains) > 0 {
		mainPkg = mains[0]
	} else if has("main.go") {
		mainPkg = "."
	}

	if web != "" {
		found.Kind = "web"
		if web == "net-http" {
			found.Stack = "go-web"
		} else {
			found.Stack = "go-web-" + web
		}
	} else {
		found.Kind = "cli"
		if cli != "" {
			found.Stack = "go-cli-" + cli
		} else {
			found.Stack = "go-cli"
		}
	}

	if web != "" && has(".air.toml") {
		found.DevCommand = "air"
	} else if mainPkg != "" {
		found.DevCommand = "go run " + mainPkg
	}

	if web != "" {
		// No guess: a port the code does not show is left empty for a person to fill.
		stripped := stripGoComments(code)
		port := goFlagPort(dir)
		if port == "" {
			if m := goPortEnvDefaultRe.FindStringSubmatch(stripped); len(m) == 2 {
				port = m[1]
			} else if m := goPortListenRe.FindStringSubmatch(stripped); len(m) == 2 {
				port = m[1]
			}
		}
		found.Port = port
		if port != "" {
			if hp := detectGoHealth(stripped); hp != "" {
				found.HealthEndpoint = "http://localhost:" + port + hp
			}
		}
	}

	hasGolangCI := has(".golangci.yml") || has(".golangci.yaml") || has(".golangci.toml") || has(".golangci.json")
	if hasGolangCI {
		found.Gates["lint"] = "golangci-lint run"
	} else {
		found.Gates["lint"] = `test -z "$(gofmt -l .)" && go vet ./...`
	}
	found.Gates["test"] = GoFastTestCmd
	found.Gates["build"] = "go build ./..."
	found.Regression = "go test ./..."
}

func detectRustRepo(dir string, has func(string) bool, found *RepoDetection) {
	b, _ := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	cargo := string(b)
	found.Stack = "rust"
	if strings.Contains(cargo, "axum") || strings.Contains(cargo, "actix-web") || strings.Contains(cargo, "rocket") {
		found.Kind = "web"
	} else if strings.Contains(cargo, "clap") || has("src/main.rs") {
		found.Kind = "cli"
	}
	found.Gates["lint"] = "cargo clippy -- -D warnings"
	found.Gates["typecheck"] = "cargo check"
	found.Gates["test"] = "cargo test"
	found.Gates["build"] = "cargo build"
}

// RenderRepoConfig renders .foreman-kit/templates/repo.yaml with detected settings.
func RenderRepoConfig(root, name string, found RepoDetection, branch, envScript string) (string, error) {
	tplBytes, err := assets.ReadTemplate(root, "repo.yaml")
	if err != nil {
		return "", err
	}
	text := string(tplBytes)
	regression := found.Regression
	if regression == "" {
		regression = found.Gates["test"]
	}

	type kv struct {
		key string
		val *string
	}
	optStr := func(s string, set bool) *string {
		if !set {
			return nil
		}
		return &s
	}

	// A Go repository's port is never guessed: nothing detected is written as empty.
	isGo := strings.HasPrefix(found.Stack, "go-")
	values := []kv{
		{"REPO_NAME", optStr(name, name != "")},
		{"TECH_STACK", optStr(found.Stack, found.Stack != "")},
		{"BASE_BRANCH", optStr(branch, branch != "")},
		{"DEV_COMMAND", optStr(found.DevCommand, found.DevCommand != "")},
		{"DEV_PORT", optStr(found.Port, found.Port != "" || found.Kind == "cli" || isGo)},
		{"HEALTH_ENDPOINT", optStr(found.HealthEndpoint, found.HealthEndpoint != "" || found.Kind == "cli" || isGo)},
		{"LINT_COMMAND", optStr(found.Gates["lint"], found.Gates["lint"] != "")},
		{"TYPECHECK_COMMAND", optStr(found.Gates["typecheck"], found.Gates["typecheck"] != "")},
		{"TEST_COMMAND", optStr(found.Gates["test"], found.Gates["test"] != "")},
		{"BUILD_COMMAND", optStr(found.Gates["build"], found.Gates["build"] != "")},
		{"REGRESSION_COMMAND", optStr(regression, regression != "")},
		{"APP_KIND", optStr(found.Kind, found.Kind != "")},
	}

	for _, item := range values {
		if item.val == nil {
			continue
		}
		from := fmt.Sprintf(`"{{%s}}"`, item.key)
		text = strings.Replace(text, from, yamlQuote(*item.val), 1)
	}
	if envScript != "" {
		text = strings.Replace(text, `env_script: ""`, "env_script: "+yamlQuote(envScript), 1)
	}
	if found.Setup != "" {
		text = strings.Replace(text, `setup: ""`, "setup: "+yamlQuote(found.Setup), 1)
	}
	var check map[string]any
	if err := yaml.Unmarshal([]byte(text), &check); err != nil {
		return "", fmt.Errorf("rendered repo.config.yaml is invalid YAML: %w", err)
	}
	return text, nil
}

// SetupRepo delivers repo.config.yaml (and script/env.sh when toolchain pins exist) to repos/<name>.
func SetupRepo(root, name string, force bool, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	if !repoNameRe.MatchString(name) {
		return fmt.Errorf("%q is not a repository name (a-z, 0-9, ., _, -)", name)
	}
	relPath := "repos/" + name
	dir := filepath.Join(root, "repos", name)
	if !dirExists(dir) {
		return fmt.Errorf("%s does not exist: foreman-kit setup add-repo <url> --name %s", relPath, name)
	}

	found := DetectRepoStack(dir)
	branch := "main"
	if repo, err := gitx.Open(root); err == nil {
		branch = repo.ChildBaseBranch(relPath)
	}
	if cur, ok := gitx.CurrentBranch(context.Background(), dir); ok {
		branch = cur
	}

	envScript := ""
	if len(found.Pins) > 0 {
		scriptRel := "script/env.sh"
		scriptPath := filepath.Join(dir, "script", "env.sh")
		envTpl, err := assets.ReadTemplate(root, "repo_env.sh")
		if err == nil {
			if !fileExists(scriptPath) {
				_ = os.MkdirAll(filepath.Dir(scriptPath), 0o755)
				_ = os.WriteFile(scriptPath, envTpl, 0o755)
				envScript = scriptRel
				fmt.Fprintf(out, "  wrote %s/%s from .foreman-kit/templates/repo_env.sh\n", relPath, scriptRel)
			} else if existing, err := os.ReadFile(scriptPath); err == nil && string(existing) == string(envTpl) {
				envScript = scriptRel
			}
		}
	}

	cfgPath := filepath.Join(dir, "repo.config.yaml")
	if fileExists(cfgPath) && !force {
		fmt.Fprintf(out, "  kept %s/repo.config.yaml (--force rewrites it from the template)\n", relPath)
	} else {
		rendered, err := RenderRepoConfig(root, name, found, branch, envScript)
		if err != nil {
			return err
		}
		if err := os.WriteFile(cfgPath, []byte(rendered), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "  wrote %s/repo.config.yaml from .foreman-kit/templates/repo.yaml\n", relPath)
	}
	return nil
}

// SetupAddRepo adds a child git repository as a submodule under repos/<name> and runs SetupRepo.
func SetupAddRepo(ctx context.Context, root, url, name, branch string, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	url = strings.TrimSpace(url)
	branch = strings.TrimSpace(branch)
	// git parses an argument starting with "-" as an option: a "URL" such as --upload-pack=<command>
	// would run <command>. The URL also reaches git after "--" below, so this is defense in depth.
	if url == "" || strings.HasPrefix(url, "-") || strings.ContainsAny(url, "\x00\r\n") {
		return fmt.Errorf("%q is not a repository URL", url)
	}
	if branch != "" {
		if strings.HasPrefix(branch, "-") {
			return fmt.Errorf("%q is not a branch name", branch)
		}
		if _, err := gitx.RunGit(ctx, root, "check-ref-format", "--branch", branch); err != nil {
			return fmt.Errorf("%q is not a branch name", branch)
		}
	}
	if name == "" {
		cleaned := strings.TrimSuffix(strings.TrimRight(url, "/"), ".git")
		name = filepath.Base(cleaned)
	}
	if !repoNameRe.MatchString(name) {
		return fmt.Errorf("%q is not a repository name: give --name", name)
	}
	relPath := "repos/" + name
	if exists(filepath.Join(root, filepath.FromSlash(relPath))) {
		return fmt.Errorf("%s already exists: foreman-kit setup repo %s", relPath, name)
	}

	local := !strings.Contains(url, "://") && !strings.Contains(url, "@")
	var baseArgs []string
	if local {
		baseArgs = append(baseArgs, "-c", "protocol.file.allow=always")
	}
	if branch == "" {
		lsArgs := append(append([]string{}, baseArgs...), "ls-remote", "--symref", "--", url, "HEAD")
		if lsOut, err := gitx.RunGit(ctx, root, lsArgs...); err == nil {
			for _, line := range strings.Split(lsOut, "\n") {
				if strings.HasPrefix(line, "ref: refs/heads/") {
					parts := strings.Fields(strings.TrimPrefix(line, "ref: refs/heads/"))
					if len(parts) > 0 {
						branch = parts[0]
						break
					}
				}
			}
		}
		if branch == "" {
			branch = "main"
		}
	}

	subArgs := append(append([]string{}, baseArgs...), "submodule", "add", "-b", branch, "--", url, relPath)
	if _, err := gitx.RunGit(ctx, root, subArgs...); err != nil {
		return fmt.Errorf("git submodule add failed: %w", err)
	}
	fmt.Fprintf(out, "added %s (%s, branch %s)\n", relPath, url, branch)
	return SetupRepo(root, name, false, out)
}

func makeSetupItem(state, what string, fix any) map[string]any {
	return map[string]any{
		"state": state,
		"what":  what,
		"fix":   fix,
	}
}

// SetupStatus computes the full readiness checklist for an application umbrella (bin/app_setup.rb status).
func SetupStatus(ctx context.Context, root string, st *store.Store) (map[string]any, error) {
	lock, _ := ReadKitLock(root)
	baseBranch := ResolveBaseBranch(root)

	// 1. kit section
	var kitItems []map[string]any
	if lock == nil {
		kitItems = append(kitItems, makeSetupItem("todo", "the kit is not installed here", "INSTALL.md: foreman-kit init --agents ..."))
	} else {
		drift := Drift(root, lock)
		if len(drift) > 0 {
			var keys []string
			for k := range drift {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			kitItems = append(kitItems, makeSetupItem("todo", fmt.Sprintf("kit v%s: files edited in place (%s)", lock.Version, strings.Join(keys, ", ")), "foreman-kit verify"))
		} else {
			kitItems = append(kitItems, makeSetupItem("ok", fmt.Sprintf("kit v%s, agents %s", lock.Version, strings.Join(lock.Agents, ", ")), nil))
		}
	}
	// The memory, kept with agent-memory: the program on PATH, and the application's .agent-memory/.
	switch {
	case memory.Require() != nil:
		kitItems = append(kitItems, makeSetupItem("todo", memory.Binary+" is not installed: the agents read and write the memory with it",
			"go install github.com/xChuCx/agent-memory/cmd/agent-memory@latest   # or: npm install -g @xchucx/agent-memory"))
	case !memory.Present(root):
		kitItems = append(kitItems, makeSetupItem("todo", "the application has no "+memory.Dir+"/", "foreman-kit update   # creates it with agent-memory and takes in MEMORY.md and memory/"))
	case memory.LegacyPresent(root):
		kitItems = append(kitItems, makeSetupItem("todo", memory.LegacyFile+" and "+memory.LegacyDir+"/ are no longer read", "foreman-kit update   # takes them into "+memory.Dir+"/"))
	default:
		kitItems = append(kitItems, makeSetupItem("ok", fmt.Sprintf("memory in %s/ (%d files), kept with %s", memory.Dir, len(memory.Files(root)), memory.Binary), nil))
	}

	// 2. project section
	var projectItems []map[string]any
	for _, f := range []string{"README.md", "ROADMAP.md", "AGENTS.md"} {
		b, _ := os.ReadFile(filepath.Join(root, f))
		left := doubleBraceRe.FindAllString(string(b), -1)
		if len(left) == 0 && len(b) > 0 {
			projectItems = append(projectItems, makeSetupItem("ok", f+" filled in", nil))
		} else {
			count := len(left)
			if count == 0 {
				count = 1
			}
			projectItems = append(projectItems, makeSetupItem("todo", fmt.Sprintf("%s: %d {{...}} placeholder(s) left", f, count), fmt.Sprintf("fill %s in with the person", f)))
		}
	}
	osCfg := loadYAMLMap(filepath.Join(root, "openspec", "config.yaml"))
	if strings.TrimSpace(asString(osCfg["context"])) == "" {
		projectItems = append(projectItems, makeSetupItem("todo", "openspec/config.yaml has no context", "add the constraints agents cannot infer from the code"))
	} else {
		projectItems = append(projectItems, makeSetupItem("ok", "openspec/config.yaml has a context", nil))
	}

	// 3. repositories section
	var repoItems []map[string]any
	var childPaths []string
	if repo, err := gitx.Open(root); err == nil {
		childPaths, _ = repo.ChildPaths()
	}
	if len(childPaths) == 0 {
		if entries, err := os.ReadDir(filepath.Join(root, "repos")); err == nil {
			for _, e := range entries {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
					childPaths = append(childPaths, "repos/"+e.Name())
				}
			}
		}
	}
	if len(childPaths) == 0 {
		repoItems = append(repoItems, makeSetupItem("todo", "no child repository under repos/", "foreman-kit setup add-repo <url> [--name NAME]"))
	} else {
		for _, relPath := range childPaths {
			name := filepath.Base(relPath)
			cfgFile := filepath.Join(root, filepath.FromSlash(relPath), "repo.config.yaml")
			if !fileExists(cfgFile) {
				repoItems = append(repoItems, makeSetupItem("todo", fmt.Sprintf("%s: no repo.config.yaml", relPath), fmt.Sprintf("foreman-kit setup repo %s", name)))
				continue
			}
			b, _ := os.ReadFile(cfgFile)
			var held map[string]any
			if yaml.Unmarshal(b, &held) == nil {
				if older := config.RepoOfAnOlderKit(held); older != "" {
					repoItems = append(repoItems, makeSetupItem("todo", fmt.Sprintf("%s: %s %s", relPath, config.RepoConfigFile, older), "foreman-kit update"))
					continue
				}
			}
			left := doubleBraceRe.FindAllString(string(b), -1)
			if len(left) > 0 {
				repoItems = append(repoItems, makeSetupItem("todo", fmt.Sprintf("%s: placeholders left in repo.config.yaml (%s)", relPath, strings.Join(left, ", ")), fmt.Sprintf("edit %s/repo.config.yaml", relPath)))
			} else {
				repoItems = append(repoItems, makeSetupItem("ok", fmt.Sprintf("%s: configured", relPath), nil))
			}
		}
	}

	// 4. settings section
	var settingsItems []map[string]any
	promptErrs, _ := CheckPrompts(root)
	if len(promptErrs) > 0 {
		settingsItems = append(settingsItems, makeSetupItem("todo", fmt.Sprintf("settings: %d error(s)", len(promptErrs)), "foreman-kit config check"))
	} else {
		settingsItems = append(settingsItems, makeSetupItem("ok", "settings valid", nil))
	}

	// 5. commit section
	var commitItems []map[string]any
	if !exists(filepath.Join(root, ".git")) {
		commitItems = append(commitItems, makeSetupItem("todo", "not a git repository", "git init"))
	} else {
		if _, err := gitx.RunGit(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
			commitItems = append(commitItems, makeSetupItem("todo", "nothing committed yet", `git add -A && git commit -m "chore: set up the OpenSpec kit"`))
		} else {
			dirty, _ := gitx.RunGit(ctx, root, "status", "--porcelain", "--ignore-submodules=dirty")
			if strings.TrimSpace(dirty) != "" {
				linesCount := len(strings.Split(strings.TrimRight(dirty, "\n"), "\n"))
				commitItems = append(commitItems, makeSetupItem("todo", fmt.Sprintf("%d uncommitted path(s) in the umbrella; a cycle pauses on any outside its write scope", linesCount), `git add -A && git commit -m "chore: set up the OpenSpec kit"`))
			} else {
				commitItems = append(commitItems, makeSetupItem("ok", fmt.Sprintf("committed on %s", baseBranch), nil))
			}
		}
	}

	// 6. run section
	var runItems []map[string]any
	var cycleExecs []store.Execution
	if st != nil {
		cycleExecs, _ = st.ListExecutions(ctx, "cycle", "", 5)
	}
	if len(cycleExecs) > 0 {
		runItems = append(runItems, makeSetupItem("ok", "a cycle has run", nil))
	} else {
		runItems = append(runItems, makeSetupItem("todo", "run the preflight", "foreman-kit preflight   # must end with preflight: OK"))
		runItems = append(runItems, makeSetupItem("todo", "no cycle has run: the daemon runs it", "foreman-kit -d -w   # it builds, merges and runs the cycle; foreman-kit daemon status says whether it runs"))
	}

	sections := map[string]any{
		"kit":          kitItems,
		"project":      projectItems,
		"repositories": repoItems,
		"settings":     settingsItems,
		"commit":       commitItems,
		"run":          runItems,
	}
	schedule := map[string]any{
		"daemon":        fmt.Sprintf("cd %s && foreman-kit -d -w", root),
		"cycle_prompt":  ".agents/skills/foreman-kit-cycle/references/routine-prompt.md",
		"visual_prompt": ".agents/skills/foreman-kit-visual-qa/references/routine-prompt.md",
	}

	return map[string]any{
		"root":        root,
		"base_branch": baseBranch,
		"sections":    sections,
		"schedule":    schedule,
	}, nil
}

// FormatSetupStatusText formats the SetupStatus report into human-readable CLI output.
func FormatSetupStatusText(report map[string]any) string {
	root := asString(report["root"])
	baseBranch := asString(report["base_branch"])
	lines := []string{fmt.Sprintf("== Setup of %s (base branch %s)", filepath.Base(root), baseBranch)}

	sections, _ := report["sections"].(map[string]any)
	sectionOrder := []string{"kit", "project", "repositories", "settings", "commit", "run"}
	var firstTodo map[string]any

	for _, sec := range sectionOrder {
		items, ok := sections[sec].([]map[string]any)
		if !ok {
			continue
		}
		lines = append(lines, sec+":")
		for _, it := range items {
			st := asString(it["state"])
			what := asString(it["what"])
			pad := 5 - len(st)
			if pad < 1 {
				pad = 1
			}
			lines = append(lines, fmt.Sprintf("  [%s]%s%s", st, strings.Repeat(" ", pad), what))
			if fix := asString(it["fix"]); fix != "" && st != "ok" {
				lines = append(lines, "          "+fix)
			}
			if firstTodo == nil && st == "todo" {
				firstTodo = it
			}
		}
	}

	if firstTodo != nil {
		lines = append(lines, fmt.Sprintf("next: %s -> %s", asString(firstTodo["what"]), asString(firstTodo["fix"])))
	} else {
		lines = append(lines, "next: nothing left; watch it with foreman-kit progress")
	}
	if sched, ok := report["schedule"].(map[string]any); ok {
		lines = append(lines, fmt.Sprintf("the daemon of this application: %s", asString(sched["daemon"])))
	}
	return strings.Join(lines, "\n")
}

// RunSetupCLI implements `foreman-kit setup status | add-repo <url> | repo <name>`.
func RunSetupCLI(ctx context.Context, root string, st *store.Store, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}

	jsonOut := false
	force := false
	var nameFlag, branchFlag string
	var positional []string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--json":
			jsonOut = true
		case args[i] == "--force":
			force = true
		case args[i] == "--name" && i+1 < len(args):
			nameFlag = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--name="):
			nameFlag = strings.TrimPrefix(args[i], "--name=")
		case args[i] == "--branch" && i+1 < len(args):
			branchFlag = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--branch="):
			branchFlag = strings.TrimPrefix(args[i], "--branch=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) == 0 {
		fmt.Fprintln(stderr, "Usage: foreman-kit setup status [--json] | add-repo <url> [--name NAME] [--branch BRANCH] | repo <name> [--force]")
		return 2
	}

	switch positional[0] {
	case "status":
		rep, err := SetupStatus(ctx, root, st)
		if err != nil {
			fmt.Fprintf(stderr, "app_setup: %v\n", err)
			return 1
		}
		if jsonOut {
			b, _ := json.MarshalIndent(rep, "", "  ")
			fmt.Fprintln(stdout, string(b))
		} else {
			fmt.Fprintln(stdout, FormatSetupStatusText(rep))
		}
		return 0
	case "add-repo":
		if len(positional) != 2 {
			fmt.Fprintln(stderr, "Usage: foreman-kit setup add-repo <url> [--name NAME] [--branch BRANCH]")
			return 2
		}
		if err := SetupAddRepo(ctx, root, positional[1], nameFlag, branchFlag, stdout); err != nil {
			fmt.Fprintf(stderr, "app_setup: %v\n", err)
			return 1
		}
		return 0
	case "repo":
		if len(positional) != 2 {
			fmt.Fprintln(stderr, "Usage: foreman-kit setup repo <name> [--force]")
			return 2
		}
		if err := SetupRepo(root, positional[1], force, stdout); err != nil {
			fmt.Fprintf(stderr, "app_setup: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintln(stderr, "Usage: foreman-kit setup status [--json] | add-repo <url> [--name NAME] [--branch BRANCH] | repo <name> [--force]")
		return 2
	}
}

// RunKitCLI implements `foreman-kit init | upgrade | verify | self-upgrade`.
func RunKitCLI(ctx context.Context, defaultTarget string, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: foreman-kit init|upgrade|verify|self-upgrade [<app-dir>] [options]")
		return 2
	}

	opts := KitOptions{
		Roles: make(map[string]string),
	}
	var sourceFlag string
	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--force":
			opts.Force = true
		case args[i] == "--agents" && i+1 < len(args):
			opts.Agents = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--agents="):
			opts.Agents = strings.TrimPrefix(args[i], "--agents=")
		case args[i] == "--plan" && i+1 < len(args):
			opts.Roles["plan"] = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--plan="):
			opts.Roles["plan"] = strings.TrimPrefix(args[i], "--plan=")
		case args[i] == "--build" && i+1 < len(args):
			opts.Roles["build"] = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--build="):
			opts.Roles["build"] = strings.TrimPrefix(args[i], "--build=")
		case args[i] == "--cycle" && i+1 < len(args):
			opts.Roles["cycle"] = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--cycle="):
			opts.Roles["cycle"] = strings.TrimPrefix(args[i], "--cycle=")
		case args[i] == "--selection" && i+1 < len(args):
			opts.Selection = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--selection="):
			opts.Selection = strings.TrimPrefix(args[i], "--selection=")
		case args[i] == "--order" || strings.HasPrefix(args[i], "--order="):
			fmt.Fprintf(stderr, "kit: --order is now --selection (%s)\n", strings.Join(config.SELECTIONS, ", "))
			return 2
		case args[i] == "--version" && i+1 < len(args):
			opts.Version = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--version="):
			opts.Version = strings.TrimPrefix(args[i], "--version=")
		case args[i] == "--commit":
			opts.Commit = true
		case args[i] == "--source" && i+1 < len(args):
			sourceFlag = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--source="):
			sourceFlag = strings.TrimPrefix(args[i], "--source=")
		default:
			positional = append(positional, args[i])
		}
	}

	cmd := positional[0]
	target := defaultTarget
	if len(positional) > 1 {
		target = positional[1]
	}
	if target == "" {
		target = "."
	}
	absTarget, err := filepath.Abs(target)
	if err == nil {
		target = absTarget
	}

	switch cmd {
	case "init":
		if err := Init(ctx, target, opts, stdout); err != nil {
			fmt.Fprintf(stderr, "kit: %v\n", err)
			return 1
		}
		return 0
	case "upgrade":
		if err := Upgrade(ctx, target, opts, stdout); err != nil {
			fmt.Fprintf(stderr, "kit: %v\n", err)
			return 1
		}
		return 0
	case "verify":
		ok, err := Verify(target, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "kit: %v\n", err)
			return 1
		}
		if !ok {
			return 1
		}
		return 0
	case "self-upgrade":
		if sourceFlag == "" && len(positional) > 1 && dirExists(target) && fileExists(filepath.Join(target, "cmd", "foreman-kit", "main.go")) {
			sourceFlag = target
		}
		if err := SelfUpgrade(ctx, SelfUpgradeOptions{SourceDir: sourceFlag}, stdout); err != nil {
			fmt.Fprintf(stderr, "kit: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintln(stderr, "Usage: foreman-kit init|upgrade|verify|self-upgrade [<app-dir>] [options]")
		return 2
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// stripGoComments blanks the // and /* */ comments of Go source (keeping every newline, so
// line structure survives) and copies interpreted strings, raw strings and runes verbatim,
// so that a "//" inside a literal is not taken for a comment.
func stripGoComments(code string) string {
	var b strings.Builder
	b.Grow(len(code))
	n := len(code)
	for i := 0; i < n; {
		c := code[i]
		switch {
		case c == '/' && i+1 < n && code[i+1] == '/':
			for i < n && code[i] != '\n' {
				b.WriteByte(' ')
				i++
			}
		case c == '/' && i+1 < n && code[i+1] == '*':
			b.WriteString("  ")
			i += 2
			for i < n && !(code[i] == '*' && i+1 < n && code[i+1] == '/') {
				if code[i] == '\n' {
					b.WriteByte('\n')
				} else {
					b.WriteByte(' ')
				}
				i++
			}
			if i < n {
				b.WriteString("  ")
				i += 2
			}
		case c == '"' || c == '\'':
			b.WriteByte(c)
			i++
			for i < n && code[i] != c && code[i] != '\n' {
				if code[i] == '\\' && i+1 < n {
					b.WriteByte(code[i])
					i++
				}
				b.WriteByte(code[i])
				i++
			}
			if i < n && code[i] == c {
				b.WriteByte(c)
				i++
			}
		case c == '`':
			j := strings.IndexByte(code[i+1:], '`')
			if j < 0 {
				b.WriteString(code[i:])
				i = n
			} else {
				b.WriteString(code[i : i+j+2])
				i += j + 2
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

var (
	// A flag named port: flag.Int, flag.IntVar, cobra/pflag Int, IntP, IntVar, IntVarP, or
	// their String forms with a quoted numeric default. Groups: method, default, usage.
	goPortFlagRe = regexp.MustCompile(`\b(Int|IntVar|IntP|IntVarP|String|StringVar|StringP|StringVarP)\(\s*(?:&[\w.]+\s*,\s*)?"port"\s*,\s*(?:"[^"\n]*"\s*,\s*)??(\d{1,5}|[A-Za-z_]\w*|"\d{1,5}")\s*,\s*"((?:[^"\\\n]|\\.)*)"`)
	// The number a usage text documents as the default: "TCP port (default 4567)".
	goUsageDefaultRe = regexp.MustCompile(`default\s+(\d{2,5})\b`)
	// A const declaration, single or grouped.
	goConstBlockRe  = regexp.MustCompile(`(?s)\bconst\s*\((.*?)\)`)
	goConstSingleRe = regexp.MustCompile(`\bconst\s+([A-Za-z_]\w*)\s*(?:int\s*)?=\s*(\d{1,5})\b`)
)

// goSourceFiles lists the non-test .go files under dir in lexical walk order, with the same
// skips as readSources.
func goSourceFiles(dir string) []string {
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", "dist", "build", ".next", "coverage":
				if p != dir {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".go") && !strings.HasSuffix(d.Name(), "_test.go") {
			if info, err := d.Info(); err == nil && info.Size() < 200000 {
				files = append(files, p)
			}
		}
		return nil
	})
	return files
}

// goConstValue resolves name to the integer of a const declared in the non-test .go files of
// dir (that directory only, the flag's package), or "".
func goConstValue(dir, name string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	inBlock := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `\s*(?:int\s*)?=\s*(\d{1,5})\b`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		code := stripGoComments(string(b))
		for _, m := range goConstSingleRe.FindAllStringSubmatch(code, -1) {
			if m[1] == name {
				return m[2]
			}
		}
		for _, blk := range goConstBlockRe.FindAllStringSubmatch(code, -1) {
			if m := inBlock.FindStringSubmatch(blk[1]); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// goFlagPort returns the port of the first flag named port under dir whose default resolves
// to a port: a literal, a same-package const, or for a zero default the "default N" of its
// usage text. It returns "" when no flag yields one.
func goFlagPort(dir string) string {
	for _, p := range goSourceFiles(dir) {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, m := range goPortFlagRe.FindAllStringSubmatch(stripGoComments(string(b)), -1) {
			method, def, usage := m[1], m[2], m[3]
			isString := strings.HasPrefix(method, "String")
			switch {
			case strings.HasPrefix(def, `"`):
				if !isString {
					continue
				}
				def = strings.Trim(def, `"`)
			case def[0] >= '0' && def[0] <= '9':
				if isString {
					continue
				}
			default:
				if isString {
					continue
				}
				def = goConstValue(filepath.Dir(p), def)
			}
			if def == "" {
				continue
			}
			if strings.Trim(def, "0") == "" {
				if u := goUsageDefaultRe.FindStringSubmatch(usage); u != nil {
					return u[1]
				}
				continue
			}
			return def
		}
	}
	return ""
}

// goHealthRouteRes match, for each of healthPaths, a route registration whose quoted pattern
// is that path, optionally after an HTTP method ("GET /healthz").
var goHealthRouteRes = func() []*regexp.Regexp {
	res := make([]*regexp.Regexp, len(healthPaths))
	for i, hp := range healthPaths {
		res[i] = regexp.MustCompile(`\b(?:HandleFunc|Handle|Get|GET|Any|Head|HEAD)\(\s*["` + "`" + `](?:[A-Z]+\s+)?` + regexp.QuoteMeta(hp) + `["` + "`" + `]`)
	}
	return res
}()

// detectGoHealth returns the first of healthPaths registered as a route in comment-free Go
// code, or "".
func detectGoHealth(code string) string {
	for i, re := range goHealthRouteRes {
		if re.MatchString(code) {
			return healthPaths[i]
		}
	}
	return ""
}
