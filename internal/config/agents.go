package config

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// AgentsFile is the relative path to .foreman-kit/agents.yaml.
var AgentsFile = settings.Agents.FilePath()

// DEFAULT_SKILLS is the shared skills directory every application gets.
const DEFAULT_SKILLS = ".agents/skills"

// AgentMeta describes an agent CLI the kit knows: where its skills go and the commands the kit
// ships for it. Those commands are what `foreman-kit init` and `foreman-kit update --agents` write
// under commands.<agent> of .foreman-kit/agents.yaml; what runs is what that file holds.
type AgentMeta struct {
	Label  string `json:"label" yaml:"label"`
	Tool   string `json:"tool" yaml:"tool"`
	Skills string `json:"skills" yaml:"skills"`
	// Run is the command of a call while permissions is full.
	Run string `json:"run" yaml:"run"`
	// Restricted is the command of a call while permissions is restricted ("" when the CLI has no
	// way to hold its agent back: it cannot be used in that mode).
	Restricted string `json:"restricted,omitempty" yaml:"restricted,omitempty"`
	// Ping is the command of the preflight's ping.
	Ping string `json:"ping,omitempty" yaml:"ping,omitempty"`
	// StdinNull says the CLI needs its standard input closed.
	StdinNull bool `json:"stdin_null,omitempty" yaml:"stdin_null,omitempty"`
}

// AgentCommandSpec is how an agent is executed: commands.<agent> of .foreman-kit/agents.yaml. In the file
// it is a mapping with run, restricted and ping; a bare string or list is its run command.
type AgentCommandSpec struct {
	Run        any    `json:"run,omitempty" yaml:"run,omitempty"`
	Restricted any    `json:"restricted,omitempty" yaml:"restricted,omitempty"`
	Ping       any    `json:"ping,omitempty" yaml:"ping,omitempty"`
	Skills     string `json:"skills,omitempty" yaml:"skills,omitempty"`
	Tool       string `json:"tool,omitempty" yaml:"tool,omitempty"`
	StdinNull  *bool  `json:"stdin_null,omitempty" yaml:"stdin_null,omitempty"`
}

// The tool lists the kit ships in the commands of Claude Code and GitHub Copilot.
const (
	claudeAllowedTools = "Read Edit Write Glob Grep Bash(openspec *) Bash(git status *) Bash(git diff *) Bash(git log *) Bash(git show *) Bash(git rev-parse *) Bash(foreman-kit gates *) Bash(foreman-kit exec *) Bash(foreman-kit status *) Bash(foreman-kit progress *) Bash(foreman-kit completed-since *) Bash(foreman-kit evaluation-report *) Bash(foreman-kit prompt-report *) Bash(foreman-kit memory *) Bash(agent-memory *) Bash(foreman-kit visual-qa *) Bash(jev noul *) Bash(jev choice *) Bash(jev score *) Bash(jev eval *) Bash(jev validate *)"
	claudeDeniedTools  = "Bash(git commit *) Bash(git push *) Bash(git rebase *) Bash(git reset *) Bash(git checkout *) Bash(git switch *) Bash(rm -rf *)"
	copilotDenied      = `--deny-tool "shell(git commit)" --deny-tool "shell(git push)" --deny-tool "shell(git rebase)" --deny-tool "shell(git reset)" --deny-tool "shell(git checkout)" --deny-tool "shell(git switch)"`
	// ShippedClaudeBudget is the spending cap, in dollars, the kit writes in Claude Code's commands.
	ShippedClaudeBudget = "5"
)

// SUPPORTED maps the 13 agent CLIs the kit knows to their metadata and the commands it ships for
// them. An agent gets its role's args where its command says {{args}}.
var SUPPORTED = map[string]AgentMeta{
	"claude": {
		Label:      "Claude Code",
		Tool:       "claude",
		Skills:     ".claude/skills",
		Run:        `claude -p {{prompt}} --output-format json --no-session-persistence --permission-mode bypassPermissions --max-budget-usd ` + ShippedClaudeBudget + ` --disallowedTools "` + claudeDeniedTools + `" {{args}}`,
		Restricted: `claude -p {{prompt}} --output-format json --no-session-persistence --permission-mode acceptEdits --max-budget-usd ` + ShippedClaudeBudget + ` --allowedTools "` + claudeAllowedTools + `" --disallowedTools "` + claudeDeniedTools + `" {{args}}`,
		Ping:       "claude -p {{prompt}} --no-session-persistence --max-budget-usd 0.50 {{args}}",
	},
	"codex": {
		Label:      "OpenAI Codex",
		Tool:       "codex",
		Skills:     ".agents/skills",
		Run:        "codex exec --dangerously-bypass-approvals-and-sandbox {{args}} {{prompt}}",
		Restricted: "codex exec --full-auto {{args}} {{prompt}}",
		Ping:       "codex exec {{args}} {{prompt}}",
	},
	"copilot": {
		Label:  "GitHub Copilot",
		Tool:   "github-copilot",
		Skills: ".github/skills",
		Run:    "copilot -p {{prompt}} --silent --allow-all-tools " + copilotDenied + " {{args}}",
		Ping:   "copilot -p {{prompt}} --silent {{args}}",
	},
	"hermes": {
		Label:  "Hermes",
		Tool:   "hermes",
		Skills: ".hermes/skills",
		Run:    "hermes -z {{prompt}} --yolo --accept-hooks {{args}}",
		Ping:   "hermes -z {{prompt}} {{args}}",
	},
	"opencode": {
		Label:  "OpenCode",
		Tool:   "opencode",
		Skills: ".opencode/skills",
		Run:    "opencode run --format json {{args}} {{prompt}}",
		Ping:   "opencode run {{args}} {{prompt}}",
	},
	"agy": {
		Label:      "Google Antigravity",
		Tool:       "antigravity",
		Skills:     ".agents/skills",
		Run:        "agy -p {{prompt}} --print-timeout {{timeout}}s --dangerously-skip-permissions {{args}}",
		Restricted: "agy -p {{prompt}} --print-timeout {{timeout}}s --dangerously-skip-permissions --sandbox {{args}}",
		Ping:       "agy -p {{prompt}} --print-timeout 170s {{args}}",
	},
	"kilo": {
		Label:      "Kilo Code",
		Tool:       "kilocode",
		Skills:     ".kilocode/skills",
		Run:        "kilo run --agent code --auto {{args}} {{prompt}}",
		Restricted: "kilo run --agent code {{args}} {{prompt}}",
		Ping:       "kilo run --agent code {{args}} {{prompt}}",
	},
	"cline": {
		Label:  "Cline",
		Tool:   "cline",
		Skills: ".cline/skills",
		Run:    "cline --yolo --cwd {{workdir}} {{args}} {{prompt}}",
		Ping:   "cline --yolo {{args}} {{prompt}}",
	},
	"pi": {
		Label:      "pi",
		Tool:       "pi",
		Skills:     ".pi/skills",
		Run:        "pi -p --no-session --approve {{args}} -- {{prompt}}",
		Restricted: "pi -p --no-session --no-approve {{args}} -- {{prompt}}",
		Ping:       "pi -p --no-session {{args}} -- {{prompt}}",
	},
	"omp": {
		Label:      "oh-my-pi, a fork of pi",
		Tool:       "oh-my-pi",
		Skills:     ".omp/skills",
		Run:        "omp -p --no-session --approval-mode=yolo {{args}} {{prompt}}",
		Restricted: "omp -p --no-session {{args}} {{prompt}}",
		Ping:       "omp -p --no-session {{args}} {{prompt}}",
	},
	"crush": {
		Label:     "Charm's Crush",
		Tool:      "crush",
		Skills:    ".crush/skills",
		Run:       "crush run --quiet {{args}} {{prompt}}",
		Ping:      "crush run --quiet {{args}} {{prompt}}",
		StdinNull: true,
	},
	"cursor-agent": {
		Label:      "Cursor Agent",
		Tool:       "cursor",
		Skills:     ".cursor/skills",
		Run:        "cursor-agent -p --force --trust {{args}} {{prompt}}",
		Restricted: "cursor-agent -p --trust --sandbox enabled {{args}} {{prompt}}",
		Ping:       "cursor-agent -p --trust {{args}} {{prompt}}",
	},
	"zcode": {
		Label:      "ZCode",
		Tool:       "zcode",
		Skills:     ".zcode/skills",
		Run:        "zcode -p {{prompt}} --mode yolo {{args}}",
		Restricted: "zcode -p {{prompt}} --mode guarded {{args}}",
		Ping:       "zcode -p {{prompt}} {{args}}",
	},
}

// ShippedCommands returns the commands the kit ships for agent name, and whether it knows the agent.
func ShippedCommands(name string) (AgentCommandSpec, bool) {
	m, ok := SUPPORTED[name]
	if !ok {
		return AgentCommandSpec{}, false
	}
	spec := AgentCommandSpec{Run: m.Run}
	if m.Restricted != "" {
		spec.Restricted = m.Restricted
	}
	if m.Ping != "" {
		spec.Ping = m.Ping
	}
	if m.StdinNull {
		yes := true
		spec.StdinNull = &yes
	}
	return spec, true
}

// SupportedOrder preserves the canonical order of SUPPORTED from bin/lib/agents.rb.
var SupportedOrder = []string{
	"claude",
	"codex",
	"copilot",
	"hermes",
	"opencode",
	"agy",
	"kilo",
	"cline",
	"pi",
	"omp",
	"crush",
	"cursor-agent",
	"zcode",
}

// ROLES maps each agent role to what its agent does.
var ROLES = settings.RoleDoes

// RolesOrder preserves the canonical order of ROLES.
var RolesOrder = settings.Roles

// PERMISSIONS lists the valid agent permission modes.
var PERMISSIONS = settings.Of(settings.AgentPermissions).Values

// SELECTIONS lists the ways a call picks among the agents of its role.
var SELECTIONS = settings.Of(settings.AgentSelection).Values

// AgentsConfig is .foreman-kit/agents.yaml as it is written: the agents, each role's agents and args, and
// the commands. The limits and the modes are read through the settings catalog.
type AgentsConfig struct {
	Agents []string `json:"agents" yaml:"agents"`
	// Roles maps each role to its agents, in order of preference.
	Roles map[string][]string `json:"roles" yaml:"roles"`
	// RoleArgs maps each role to the arguments of each agent in it (a string or a list).
	RoleArgs    map[string]map[string]any   `json:"role_args,omitempty" yaml:"-"`
	Permissions string                      `json:"permissions" yaml:"permissions"`
	Selection   string                      `json:"selection" yaml:"selection"`
	Commands    map[string]AgentCommandSpec `json:"commands,omitempty" yaml:"commands,omitempty"`
}

// ParseCommandSpec converts a raw YAML value (string, slice, or map) into an AgentCommandSpec.
func ParseCommandSpec(raw any) (AgentCommandSpec, bool) {
	if raw == nil {
		return AgentCommandSpec{}, false
	}
	switch v := raw.(type) {
	case AgentCommandSpec:
		return v, true
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return AgentCommandSpec{}, false
		}
		return AgentCommandSpec{Run: s}, true
	case []string:
		if len(v) == 0 {
			return AgentCommandSpec{}, false
		}
		return AgentCommandSpec{Run: append([]string(nil), v...)}, true
	case []any:
		if len(v) == 0 {
			return AgentCommandSpec{}, false
		}
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		return AgentCommandSpec{Run: out}, true
	case map[string]any:
		spec := AgentCommandSpec{
			Run:        v["run"],
			Restricted: v["restricted"],
			Ping:       v["ping"],
		}
		if spec.Run == nil && v["command"] != nil {
			spec.Run = v["command"]
		}
		if s, ok := v["skills"].(string); ok {
			spec.Skills = strings.TrimSpace(s)
		}
		if t, ok := v["tool"].(string); ok {
			spec.Tool = strings.TrimSpace(t)
		}
		if b, ok := v["stdin_null"].(bool); ok {
			val := b
			spec.StdinNull = &val
		}
		return spec, spec.Run != nil || spec.Restricted != nil || spec.Ping != nil || spec.Skills != ""
	default:
		return AgentCommandSpec{}, false
	}
}

// CommandWords splits a command template value (string or slice) into words.
func CommandWords(cmd any) ([]string, error) {
	switch v := cmd.(type) {
	case nil:
		return nil, nil
	case []string:
		return append([]string(nil), v...), nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		return out, nil
	case string:
		return SplitShellwords(strings.TrimSpace(v))
	default:
		return SplitShellwords(strings.TrimSpace(fmt.Sprint(v)))
	}
}

// CommandsFor returns the commands of agent name: what commands.<name> of root's .foreman-kit/agents.yaml
// holds, and whether it holds any. An agent the file does not write has the commands the kit ships
// for it, when it knows the agent: `foreman-kit update` writes them, and `foreman-kit config check`
// reports them as missing until it does.
func CommandsFor(root, name string) (spec AgentCommandSpec, written bool, known bool) {
	if root != "" {
		if cfg := Load(root); cfg != nil {
			if spec, ok := cfg.Commands[name]; ok {
				return spec, true, true
			}
		}
	}
	spec, known = ShippedCommands(name)
	return spec, false, known
}

// ExecutableBinary returns the executable of agent name: the first word of its run command, or name.
func ExecutableBinary(root, name string) string {
	spec, _, known := CommandsFor(root, name)
	if known && spec.Run != nil {
		if words, err := CommandWords(spec.Run); err == nil && len(words) > 0 && words[0] != "" {
			return words[0]
		}
	}
	return name
}

// IsKnownOrExecutableAgent reports whether name is one of the 13 built-in agents, "stub", or an executable binary on PATH.
func IsKnownOrExecutableAgent(name string) bool {
	return IsKnownOrExecutableAgentInRoot("", name)
}

// IsKnownOrExecutableAgentInRoot checks whether name is a built-in agent, "stub", or resolves to an executable binary via root's .foreman-kit/agents.yaml or PATH.
func IsKnownOrExecutableAgentInRoot(root, name string) bool {
	if _, ok := SUPPORTED[name]; ok || name == "stub" {
		return true
	}
	if name == "" {
		return false
	}
	bin := ExecutableBinary(root, name)
	_, err := exec.LookPath(bin)
	return err == nil
}

// ParseAgents parses a comma-separated agent list (e.g. "hermes, opencode") into unique supported agent names.
func ParseAgents(list string) ([]string, error) {
	names := Names(list)
	if len(names) == 0 {
		return nil, fmt.Errorf("no agent given")
	}
	var unknown []string
	for _, n := range names {
		if _, ok := SUPPORTED[n]; !ok {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown agent %s (supported: %s)", strings.Join(unknown, ", "), strings.Join(SupportedOrder, ", "))
	}
	return names, nil
}

// Names normalizes a string, []string, or []any of comma-separated agent names into a deduplicated slice.
func Names(value any) []string {
	if value == nil {
		return nil
	}
	var rawParts []string
	switch v := value.(type) {
	case string:
		rawParts = strings.Split(v, ",")
	case []string:
		for _, item := range v {
			rawParts = append(rawParts, strings.Split(item, ",")...)
		}
	case []any:
		for _, item := range v {
			rawParts = append(rawParts, strings.Split(fmt.Sprint(item), ",")...)
		}
	default:
		rawParts = strings.Split(fmt.Sprint(v), ",")
	}
	seen := make(map[string]bool)
	var out []string
	for _, p := range rawParts {
		s := strings.TrimSpace(p)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// SkillsDir returns the skills directory for a supported agent.
func SkillsDir(name string) string {
	return SUPPORTED[name].Skills
}

// SkillsDirs returns the deduplicated list of skills directories for agents, starting with DEFAULT_SKILLS (.agents/skills).
func SkillsDirs(agents []string) []string {
	seen := map[string]bool{DEFAULT_SKILLS: true}
	out := []string{DEFAULT_SKILLS}
	for _, a := range agents {
		dir := SkillsDir(a)
		if dir != "" && !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	return out
}

// Tool returns the `openspec init --tools` identifier for an agent.
func Tool(name string) string {
	return SUPPORTED[name].Tool
}

// Tools returns the deduplicated `openspec init --tools` identifiers for agents, starting with "agents".
func Tools(agents []string) []string {
	seen := map[string]bool{"agents": true}
	out := []string{"agents"}
	for _, a := range agents {
		t := Tool(a)
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// Load reads .foreman-kit/agents.yaml from root: its own, or the one of the main checkout when root is a
// worktree that lacks the file or was cut before `foreman-kit update` rewrote it
// (settings.FilesRoot). Returns nil if .foreman-kit/agents.yaml does not exist.
func Load(root string) *AgentsConfig {
	if root == "" {
		root = AppRoot("")
	}
	root = settings.FilesRoot(root)
	p := filepath.Join(root, filepath.FromSlash(AgentsFile))
	if !fileExists(p) {
		p = filepath.Join(MainRoot(root), filepath.FromSlash(AgentsFile))
	}
	if !fileExists(p) {
		return nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var data map[string]any
	if err := yaml.Unmarshal(raw, &data); err != nil || data == nil {
		data = map[string]any{}
	}
	return agentsConfigOf(data)
}

// agentsConfigOf reads .foreman-kit/agents.yaml out of its parsed contents.
func agentsConfigOf(data map[string]any) *AgentsConfig {
	cfg := &AgentsConfig{
		Agents:   Names(data["agents"]),
		Roles:    make(map[string][]string),
		RoleArgs: make(map[string]map[string]any),
		Commands: make(map[string]AgentCommandSpec),
	}
	if rolesMap, ok := data["roles"].(map[string]any); ok {
		for role, raw := range rolesMap {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			cfg.Roles[role] = Names(entry["agents"])
			if args, ok := entry["args"].(map[string]any); ok && len(args) > 0 {
				cfg.RoleArgs[role] = args
			}
		}
	}
	if perm, ok := data["permissions"]; ok && perm != nil {
		cfg.Permissions = strings.TrimSpace(fmt.Sprint(perm))
	}
	if sel, ok := data["selection"]; ok && sel != nil {
		cfg.Selection = strings.TrimSpace(fmt.Sprint(sel))
	}
	if cmdMap, ok := data["commands"].(map[string]any); ok {
		for k, v := range cmdMap {
			if spec, ok := ParseCommandSpec(v); ok {
				cfg.Commands[k] = spec
			}
		}
	}
	return cfg
}

// ExtraArgs returns the arguments the preflight's ping of agent gets (PingArgs).
func ExtraArgs(root, agent string) []string {
	return ExtraArgsWithStderr(root, agent, os.Stderr)
}

// ExtraArgsWithStderr returns the arguments agent gets outside any role: the preflight's ping. They
// are the args of the first role, in order, that lists the agent, so the ping checks the model or
// the provider a real call of that role would use.
func ExtraArgsWithStderr(root, agent string, stderr io.Writer) []string {
	for _, role := range RolesOrder {
		// The agents of the role in effect: the ones a run was given with --set are the ones a
		// real call of this run would use.
		listed, _ := Engines(root, role)
		for _, name := range listed {
			if name == agent {
				return ExtraArgsForRole(root, agent, role, stderr)
			}
		}
	}
	return []string{}
}

// ExtraArgsForRole returns the arguments agent gets when it runs as role ("plan", "build" or
// "cycle"): roles.<role>.args.<agent> of .foreman-kit/agents.yaml, a string split as a shell would split
// it or a list. A role without an entry for the agent gives none.
func ExtraArgsForRole(root, agent, role string, stderr io.Writer) []string {
	if stderr == nil {
		stderr = os.Stderr
	}
	cfg := Load(root)
	if cfg == nil || role == "" {
		return []string{}
	}
	val, ok := cfg.RoleArgs[role][agent]
	if !ok || val == nil {
		return []string{}
	}
	args, err := ExtraArgsValue(val)
	if err != nil {
		fmt.Fprintf(stderr, "agents: %s.%s cannot be split (%v); none used\n", settings.RoleArgs(role).Path(), agent, err)
		return []string{}
	}
	return args
}

// ExtraArgsValue converts the args of one agent (a string split as a shell would, or a list) into arguments.
func ExtraArgsValue(val any) ([]string, error) {
	switch v := val.(type) {
	case nil:
		return []string{}, nil
	case []string:
		return append([]string(nil), v...), nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		return out, nil
	default:
		return SplitShellwords(fmt.Sprint(v))
	}
}

// SplitShellwords splits a string like POSIX shell / Ruby Shellwords.split.
func SplitShellwords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	inSingle := false
	inDouble := false
	escaped := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			inWord = true
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			} else {
				cur.WriteByte(c)
			}
			continue
		}
		if inDouble {
			if c == '"' {
				inDouble = false
			} else if c == '\\' {
				if i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\' || s[i+1] == '$' || s[i+1] == '`') {
					i++
					cur.WriteByte(s[i])
				} else {
					cur.WriteByte(c)
				}
			} else {
				cur.WriteByte(c)
			}
			continue
		}
		switch c {
		case '\\':
			escaped = true
			inWord = true
		case '\'':
			inSingle = true
			inWord = true
		case '"':
			inDouble = true
			inWord = true
		case ' ', '\t', '\n', '\r':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if escaped || inSingle || inDouble {
		return nil, fmt.Errorf("unmatched quote or escape in %q", s)
	}
	if inWord {
		words = append(words, cur.String())
	}
	if words == nil {
		return []string{}, nil
	}
	return words, nil
}

// Selection returns how a call picks among the agents of its role (agents.selection).
func Selection(root string) string {
	return SelectionTo(root, os.Stderr)
}

// SelectionTo returns how a call picks among the agents of its role, with the notes about a value
// that cannot be used written to stderr.
func SelectionTo(root string, stderr io.Writer) string {
	return agentsReader(root, stderr).String(settings.AgentSelection)
}

// Permissions returns which command of each agent runs (agents.permissions).
func Permissions(root string) string {
	return PermissionsTo(root, os.Stderr)
}

// PermissionsTo returns the permission mode in effect, with the notes about a value that cannot be
// used written to stderr. A value the setting does not know is read as restricted: it never widens
// what the agents may run.
func PermissionsTo(root string, stderr io.Writer) string {
	if stderr == nil {
		stderr = os.Stderr
	}
	s := settings.Of(settings.AgentPermissions)
	r := agentsReader(root, stderr)
	raw, _, set := r.Lookup(s.Key)
	if !set {
		return r.String(s.Key)
	}
	if v, ok := s.Parse(raw); ok {
		return v.(string)
	}
	fmt.Fprintf(stderr, "agents: unknown permissions %q (%s); using %s\n", fmt.Sprint(raw), strings.Join(PERMISSIONS, " | "), settings.PermissionsRestricted)
	return settings.PermissionsRestricted
}

// agentsReader reads the settings of root, with its notes written to stderr.
func agentsReader(root string, stderr io.Writer) *settings.Reader {
	return &settings.Reader{Root: root, Stderr: stderr}
}

// Engines returns the ordered slice of engines for role (plan, build, cycle). A role without
// agents has none: nothing runs in its place.
func Engines(root, role string) ([]string, error) {
	if _, ok := ROLES[role]; !ok {
		return nil, fmt.Errorf("unknown role %s (roles: %s)", role, strings.Join(RolesOrder, ", "))
	}
	raw, _, set := agentsReader(root, io.Discard).Lookup(settings.RoleAgents(role))
	if !set {
		return []string{}, nil
	}
	list := Names(raw)
	if list == nil {
		return []string{}, nil
	}
	return list, nil
}

// Engine returns the comma-separated engine list for role, or "" if none is configured.
func Engine(root, role string) (string, error) {
	list, err := Engines(root, role)
	if err != nil {
		return "", err
	}
	return strings.Join(list, ","), nil
}

// RoleTimeout returns the limit of one call of role.
func RoleTimeout(root, role string) time.Duration {
	if _, ok := ROLES[role]; !ok {
		role = "build"
	}
	return settings.At(root).Duration(settings.RoleTimeout(role))
}

// Write writes .foreman-kit/agents.yaml of root for agents, with each role's agents and how a call picks
// among them ("" keeps what the file says). Everything else the file holds is kept: the limits, the
// args and the commands. An agent the file has no commands for gets the ones the kit ships.
func Write(root string, agents []string, roles map[string][]string, selection string) error {
	if root == "" {
		root = AppRoot("")
	}
	if selection != "" && !slices.Contains(SELECTIONS, selection) {
		return fmt.Errorf("unknown selection %s (%s)", selection, strings.Join(SELECTIONS, ", "))
	}
	values := map[settings.Key]any{}
	current := map[string]any{}
	outPath := filepath.Join(root, filepath.FromSlash(AgentsFile))
	if raw, err := os.ReadFile(outPath); err == nil {
		_ = yaml.Unmarshal(raw, &current)
	}
	// What the file already holds, in the shape of today, is kept.
	for _, s := range settings.In(settings.Agents.File()) {
		if raw := digMap(current, s.Key.Parts()...); raw != nil && s.Problem(raw) == "" {
			values[s.Key] = raw
		}
	}
	values[settings.Agents] = append([]string(nil), agents...)
	for _, role := range RolesOrder {
		values[settings.RoleAgents(role)] = Names(roles[role])
	}
	if selection != "" {
		values[settings.AgentSelection] = selection
	}
	var inRoles []string
	for _, role := range RolesOrder {
		inRoles = append(inRoles, Names(roles[role])...)
	}
	values[settings.AgentCommands] = commandsFor(agents, inRoles, agentsConfigOf(current).Commands)

	text, err := RenderSeed(settings.Agents.File(), values)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	return WriteFileAtomic(outPath, text, 0o644)
}

// commandsFor returns the commands .foreman-kit/agents.yaml writes: those of each agent of the
// application and of each agent a role names, as the file already holds them or as the kit ships
// them, then those of any other agent the file defines that the kit does not know (a custom binary).
// The commands of an agent the kit knows that the application no longer uses are not kept.
func commandsFor(agents, inRoles []string, written map[string]AgentCommandSpec) map[string]any {
	out := map[string]any{}
	for _, name := range append(append([]string(nil), agents...), inRoles...) {
		if _, done := out[name]; done {
			continue
		}
		if spec, ok := written[name]; ok {
			out[name] = spec
		} else if spec, ok := ShippedCommands(name); ok {
			out[name] = spec
		}
	}
	for name, spec := range written {
		if _, done := out[name]; done {
			continue
		}
		if _, shipped := SUPPORTED[name]; !shipped {
			out[name] = spec
		}
	}
	return out
}

func withStore(root string, st *store.Store, fn func(*store.Store) error) error {
	if st != nil {
		return fn(st)
	}
	if root == "" {
		root = AppRoot("")
	}
	s, err := store.Open(root)
	if err != nil {
		return err
	}
	defer s.Close()
	return fn(s)
}

// Arrange orders candidate engines as agents.selection says (in_order, random or least_busy) and
// atomically claims the first engine in SQLite engine_claims for pid (when pid > 0).
func Arrange(ctx context.Context, root string, st *store.Store, list any, pid int) ([]string, error) {
	candidates := Names(list)
	if len(candidates) == 0 {
		return []string{}, nil
	}
	how := Selection(root)
	var arranged []string
	err := withStore(root, st, func(s *store.Store) error {
		var err error
		arranged, err = s.ArrangeAgents(ctx, candidates, how, pid, rand.Intn)
		return err
	})
	return arranged, err
}

// Claim registers pid as running agent in SQLite engine_claims.
func Claim(ctx context.Context, root string, st *store.Store, agent string, pid int) error {
	return withStore(root, st, func(s *store.Store) error {
		return s.ClaimAgent(ctx, pid, agent, "", "")
	})
}

// Release removes pid from SQLite engine_claims.
func Release(ctx context.Context, root string, st *store.Store, pid int) error {
	return withStore(root, st, func(s *store.Store) error {
		return s.ReleaseAgent(ctx, pid)
	})
}

// InUse returns a map of engine -> active call count from SQLite engine_claims (pruning dead PIDs).
func InUse(ctx context.Context, root string, st *store.Store) (map[string]int, error) {
	var counts map[string]int
	err := withStore(root, st, func(s *store.Store) error {
		var err error
		counts, err = s.AgentsInUse(ctx)
		return err
	})
	return counts, err
}

// RunAgentsCLI implements `foreman-kit agents ...`.
func RunAgentsCLI(ctx context.Context, root string, st *store.Store, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if root == "" {
		root = AppRoot("")
	}
	usage := "Usage: foreman-kit agents engine <plan|build|cycle> | list | selection | permissions | arrange <list> <pid> | claim <agent> <pid> | release <pid> | in-use | args <agent> [plan|build|cycle]"
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	switch args[0] {
	case "engine":
		if len(args) != 2 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		eng, err := Engine(root, args[1])
		if err != nil {
			fmt.Fprintf(stderr, "agents: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, eng)
		return 0
	case "list":
		if len(args) != 1 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		cfg := Load(root)
		var list []string
		if cfg != nil {
			list = cfg.Agents
		}
		fmt.Fprintln(stdout, strings.Join(list, " "))
		return 0
	case "permissions":
		if len(args) != 1 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		fmt.Fprintln(stdout, PermissionsTo(root, stderr))
		return 0
	case "selection":
		if len(args) != 1 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		fmt.Fprintln(stdout, SelectionTo(root, stderr))
		return 0
	case "args":
		if len(args) != 2 && len(args) != 3 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		list := ExtraArgsWithStderr(root, args[1], stderr)
		if len(args) == 3 {
			list = ExtraArgsForRole(root, args[1], args[2], stderr)
		}
		for _, arg := range list {
			fmt.Fprintf(stdout, "%s\x00", arg)
		}
		return 0
	case "arrange":
		if len(args) != 3 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		pid, err := strconv.Atoi(args[2])
		if err != nil {
			fmt.Fprintf(stderr, "agents: invalid pid %q\n", args[2])
			return 1
		}
		arranged, err := Arrange(ctx, root, st, args[1], pid)
		if err != nil {
			fmt.Fprintf(stderr, "agents: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, strings.Join(arranged, ","))
		return 0
	case "claim":
		if len(args) != 3 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		pid, err := strconv.Atoi(args[2])
		if err != nil {
			fmt.Fprintf(stderr, "agents: invalid pid %q\n", args[2])
			return 1
		}
		if err := Claim(ctx, root, st, args[1], pid); err != nil {
			fmt.Fprintf(stderr, "agents: %v\n", err)
			return 1
		}
		return 0
	case "release":
		if len(args) != 2 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		pid, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Fprintf(stderr, "agents: invalid pid %q\n", args[1])
			return 1
		}
		if err := Release(ctx, root, st, pid); err != nil {
			fmt.Fprintf(stderr, "agents: %v\n", err)
			return 1
		}
		return 0
	case "in-use":
		if len(args) != 1 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		counts, err := InUse(ctx, root, st)
		if err != nil {
			fmt.Fprintf(stderr, "agents: %v\n", err)
			return 1
		}
		var keys []string
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, a := range keys {
			n := counts[a]
			if n > 1 {
				parts = append(parts, fmt.Sprintf("%s %d", a, n))
			} else if n == 1 {
				parts = append(parts, a)
			}
		}
		fmt.Fprintln(stdout, strings.Join(parts, ", "))
		return 0
	default:
		fmt.Fprintln(stderr, usage)
		return 2
	}
}
