// Package settings is the catalog of an application's configuration: the one definition of every key
// of its .foreman-kit/ files (the file it lives in, what it accepts, the value the kit ships, what it
// decides and who reads it) and the one loader of their values. The seeds, `foreman-kit config
// show | check | set`, `foreman-kit update` and every process that reads a setting take them from
// here, so a key is named, valued and validated in one place.
//
// Every key is written in its file. A key that is missing, or holds something it does not accept, is
// a problem the Reader reports (Reader.Problems) and `foreman-kit config check` fails on; nothing
// stands in for it silently.
//
// A key is added in one entry of the table in this file, plus its line in the seed of its file: a
// test compares every seed with the catalog, key by key.
package settings

import (
	"fmt"
	"strings"
	"time"
)

// File is one configuration file of an application.
type File struct {
	// Name is the first word of every key of the file: "pipeline" in pipeline.gates.timeout.
	Name string
	// Path is where the file lives, relative to the application root.
	Path string
	// About says what the file decides.
	About string
	// ReadBy says which process reads it.
	ReadBy string
}

// files lists the configuration files, in the order `foreman-kit config show` prints them. Each one
// is read by one process; exploration.yaml alone is content two of them share.
var files = []File{
	{Name: "pipeline", Path: ".foreman-kit/pipeline.yaml", About: "how a change travels from the backlog to the base branch", ReadBy: "the dispatcher and the merger"},
	{Name: "agents", Path: ".foreman-kit/agents.yaml", About: "which agent CLIs this application uses, who does what and the limits of one call", ReadBy: "every agent call and the preflight"},
	{Name: "cycle", Path: ".foreman-kit/cycle.yaml", About: "what one cycle does: how long it may take and what it may queue", ReadBy: "the cycle"},
	{Name: "daemon", Path: ".foreman-kit/daemon.yaml", About: "when the cycle runs and whether the Web Portal is served", ReadBy: "the daemon, when it starts"},
	{Name: "visual_qa", Path: ".foreman-kit/visual_qa.yaml", About: "the browser session an agent app runs as its own scheduled task", ReadBy: "the visual session"},
	{Name: "exploration", Path: ".foreman-kit/exploration.yaml", About: "what the cycle and the visual session look for", ReadBy: "the cycle and the visual session"},
	{Name: EvaluatorFile, Path: ".foreman-kit/evaluator.yaml", About: "the evaluator and its gates: how it is called, what each gate asks and what the kit does with the answers", ReadBy: "every gate of the evaluator"},
}

// EvaluatorFile is the name of the file that holds the evaluator and its gates.
const EvaluatorFile = "evaluator"

// Files returns the configuration files, in display order.
func Files() []File {
	return append([]File(nil), files...)
}

// FileNamed returns the configuration file called name ("pipeline").
func FileNamed(name string) (File, bool) {
	for _, f := range files {
		if f.Name == name {
			return f, true
		}
	}
	return File{}, false
}

// Key names one setting as `foreman-kit config set` takes it: the name of its file, a dot, and its
// path inside the file ("pipeline.gates.timeout"). Code refers to a setting by its constant, never
// by its text: a key that is renamed or moved to another file changes in this package alone.
type Key string

// File returns the name of the file the key lives in ("pipeline").
func (k Key) File() string {
	name, _, _ := strings.Cut(string(k), ".")
	return name
}

// Path returns the key as it is written inside its file ("gates.timeout").
func (k Key) Path() string {
	_, path, _ := strings.Cut(string(k), ".")
	return path
}

// Parts returns the path of the key split at its dots.
func (k Key) Parts() []string {
	return strings.Split(k.Path(), ".")
}

// FilePath returns the relative path of the file the key lives in (".foreman-kit/pipeline.yaml").
func (k Key) FilePath() string {
	f, _ := FileNamed(k.File())
	return f.Path
}

// Where returns the key with its file, as a message names it: "gates.timeout in
// .foreman-kit/pipeline.yaml".
func (k Key) Where() string {
	return k.Path() + " in " + k.FilePath()
}

// Kind is what a setting accepts.
type Kind string

const (
	// KindInt is a whole number, Setting.Min or more (or "never" where Setting.Never allows it).
	KindInt Kind = "int"
	// KindBool is true or false.
	KindBool Kind = "bool"
	// KindText is any text.
	KindText Kind = "string"
	// KindPath is a file or a directory of the application, relative to its root.
	KindPath Kind = "path"
	// KindEnum is one word of Setting.Values.
	KindEnum Kind = "enum"
	// KindBranch is a git branch name.
	KindBranch Kind = "branch"
	// KindDuration is a time written with its unit (30s, 15m, 2h, 1h30m), or "never" where
	// Setting.Never allows it.
	KindDuration Kind = "duration"
	// KindSlot is "none" or the number of a slot.
	KindSlot Kind = "slot"
	// KindList is a list.
	KindList Kind = "list"
	// KindAgents is the agents of a role, in order of preference.
	KindAgents Kind = "agents"
	// KindCommands maps each agent to the commands that run it.
	KindCommands Kind = "commands"
	// KindArgs maps each agent to the arguments added to its command.
	KindArgs Kind = "args"
)

// Setting is the definition of one configuration key.
type Setting struct {
	Key  Key
	Kind Kind
	// Default is the value the kit ships for the key: what the seed of its file writes and what
	// `foreman-kit update` writes when an application's file lacks the key. Nothing reads it in
	// place of a key that is missing.
	Default any
	// Seed is the value the seed writes for a key that has no value of the kit's own, because it
	// names something of the application: its base branch, its agents.
	Seed any
	// Values lists what a KindEnum accepts.
	Values []string
	// Min is the least a KindInt accepts.
	Min int
	// Never says a KindDuration or a KindInt also accepts "never".
	Never bool
	// Zero says a KindDuration also accepts 0s: no wait at all.
	Zero bool
	// MinDuration is the least a KindDuration accepts (a second when zero).
	MinDuration time.Duration
	// Empty says a KindText or a KindPath also accepts "": none.
	Empty bool
	// Means says what the key decides.
	Means string
	// ReadBy says which commands read it.
	ReadBy string
	// Change says how to change it when `foreman-kit config set` cannot.
	Change string
}

// Shipped returns the value the seed of the file writes for the key: Default, or Seed for a key
// that names something of the application.
func (s Setting) Shipped() any {
	if s.Default != nil {
		return s.Default
	}
	return s.Seed
}

// Scalar reports whether the key holds one value (a number, a word, a switch) rather than a list or a
// mapping.
func (s Setting) Scalar() bool {
	switch s.Kind {
	case KindList, KindAgents, KindCommands, KindArgs:
		return false
	}
	return true
}

// Values of pipeline.merge.outside_commits: what the merger does with commits pushed to a child
// repository outside the pipeline (a hotfix), which it integrates with the next change that touches
// the repository either way.
const (
	// OutsideCommitsVerify tests them once on their own: when their files trigger the repository's
	// regression, the merger runs it on the base alone, and a gate that fails at merge is rerun
	// there to tell whether the base or the change broke it. A base that fails holds that
	// repository's changes, without blocking them, until it moves or a person accepts it
	// (foreman-kit base accept).
	OutsideCommitsVerify = "verify"
	// OutsideCommitsIgnore never tests them as part of a change: only the commits of the change
	// being merged count for its diff-aware tests and its regression.
	OutsideCommitsIgnore = "ignore"
	// OutsideCommitsInclude counts them as the change's own, and tests them with it.
	OutsideCommitsInclude = "include"
)

// Settable reports whether the key takes a value written on a command line: `foreman-kit config
// set` changes it in its file, and `foreman-kit --set` gives it to one run. It does when the key
// holds one value, or the agents of a role. A list, the arguments of a role and the commands of
// the agents are edited in their file (Change says how).
func (s Setting) Settable() bool {
	switch s.Kind {
	case KindList, KindArgs, KindCommands:
		return false
	}
	return true
}

// Roles lists the roles an agent runs in, in display order.
var Roles = []string{"plan", "build", "cycle"}

// RoleDoes says what the agent of each role does.
var RoleDoes = map[string]string{
	"plan":  "writes the OpenSpec change of each backlog item",
	"build": "implements the tasks in the child repositories",
	"cycle": "runs the cycle's judgment",
}

// roleReaders says which command runs the agent of each role.
var roleReaders = map[string]string{
	"plan":  "foreman-kit dispatch (PLAN)",
	"build": "foreman-kit dispatch (BUILD)",
	"cycle": "foreman-kit cycle (the cycle's judgment)",
}

// roleTimeouts is the limit the kit ships for one call of each role.
var roleTimeouts = map[string]string{"plan": "30m", "build": "2h", "cycle": "1h"}

// Values of agents.selection: how each call picks among the agents of its role.
const (
	// SelectionInOrder starts with the first agent, then the next when one fails.
	SelectionInOrder = "in_order"
	// SelectionRandom starts with any of them, then the others in the order listed.
	SelectionRandom = "random"
	// SelectionLeastBusy starts with the one the fewest running calls use.
	SelectionLeastBusy = "least_busy"
)

// Values of agents.permissions: which command of each agent runs.
const (
	PermissionsFull       = "full"
	PermissionsRestricted = "restricted"
)

// Values of cycle.proposals.approved_by and cycle.proposals.prompt_edits_approved_by.
const (
	ApprovalEvaluator = "evaluator"
	ApprovalHuman     = "human"
	ApprovalNobody    = "nobody"
)

// Values of exploration.selection.
const (
	ExplorationInOrder = "in_order"
	ExplorationRandom  = "random"
)

// The sessions a strategy of .foreman-kit/exploration.yaml runs in (strategies[].session).
const (
	SessionCycle    = "cycle"
	SessionVisualQA = "visual_qa"
)

// Sessions lists the sessions a strategy may name.
var Sessions = []string{SessionCycle, SessionVisualQA}

// Values of evaluator.when_unavailable: what a gate does when the evaluator does not answer.
const (
	// UnavailablePass lets the gate pass.
	UnavailablePass = "pass"
	// UnavailableBlock makes an enforce gate block its change.
	UnavailableBlock = "block"
	// UnavailableStop blocks like UnavailableBlock, and fails the preflight: no cycle judges and no
	// worker starts while the evaluator does not answer.
	UnavailableStop = "stop"
)

// Modes of a gate of the evaluator.
const (
	GateOff      = "off"
	GateAdvisory = "advisory"
	GateEnforce  = "enforce"
)

// GateModes lists the modes of a gate of the evaluator.
var GateModes = []string{GateOff, GateAdvisory, GateEnforce}

// gate is one gate of the evaluator: who asks it, and the mode the seed ships it in.
type gate struct {
	name   string
	seed   string
	reader string
}

// gates lists the gates of the evaluator, in display order.
var gates = []gate{
	{"plan_review", GateAdvisory, "foreman-kit dispatch after openspec validate --strict: enforce sends failures back to the plan agent"},
	{"failure_triage", GateEnforce, "foreman-kit dispatch when gates fail in BUILD: enforce re-runs a flaky gate once, holds a change the environment keeps failing (environment_hold), escalates ambiguity"},
	{"repair_progress", GateAdvisory, "foreman-kit dispatch when gates fail again after a repair: enforce blocks a change that is not converging (repair_stalled)"},
	{"scenario_coverage", GateAdvisory, "foreman-kit dispatch after BUILD's gates pass: enforce sends scenarios without a real test back to the BUILD agent"},
	{"change_risk", GateAdvisory, "foreman-kit dispatch after BUILD: enforce blocks a risky change as risk_review"},
	{"concurrent_work_loss", GateEnforce, "foreman-kit merge when a change removes a teammate's concurrent work: enforce sends it back to restore it, then blocks it as concurrent_work_loss"},
	{"system_error_triage", GateEnforce, "foreman-kit dispatch and merge before an orchestrator_error pauses the workspace: enforce blocks only the change, or retries a transient error later"},
	{"policy_violation_triage", GateEnforce, "foreman-kit dispatch when a BUILD leaves files outside its paths: enforce reverts stray artifacts and continues, blocks tampering"},
	{"agent_outcome", GateEnforce, "foreman-kit dispatch when every agent failed without changes: enforce sends an agent that gave up to a person, pauses on an environment failure"},
	{"semantic_merge_conflict", GateAdvisory, "foreman-kit merge when a change and concurrent commits touch the same files: enforce sends a contradiction back, then blocks it as semantic_conflict"},
	{"push_rejection", GateEnforce, "foreman-kit merge when a push is rejected: enforce sends a rejection by rule (protected branch, hook) to a person instead of retrying"},
	{"task_priority", GateAdvisory, "the cycle's judgment before it queues a backlog item"},
	{"proposal_review", GateEnforce, fmt.Sprintf("foreman-kit proposals after each judgment when %s is %s: enforce queues what passes", ProposalsApprovedBy.Path(), ApprovalEvaluator)},
}

// Gates returns the names of the gates of the evaluator, in display order.
func Gates() []string {
	out := make([]string, len(gates))
	for i, g := range gates {
		out[i] = g.name
	}
	return out
}

// IsGate reports whether name is a gate the kit asks the evaluator.
func IsGate(name string) bool {
	for _, g := range gates {
		if g.name == name {
			return true
		}
	}
	return false
}

// IsContent reports whether key names something a file holds that is not a setting: the canary of the
// evaluator and what each of its gates asks (questions, thresholds, routing rules). Only the mode of
// a gate the kit asks is a setting; a gate of the application's own that nothing asks is its content.
func IsContent(key Key) bool {
	if key.File() != EvaluatorFile {
		return false
	}
	parts := key.Parts()
	switch parts[0] {
	case "canary":
		return true
	case "gates":
		return len(parts) >= 3 && (parts[2] != "mode" || !IsGate(parts[1]))
	}
	return false
}

// all is the catalog: every setting, in the order `foreman-kit config show` prints them.
var all = build()

// index finds a setting by its key.
var index = func() map[Key]int {
	m := make(map[Key]int, len(all))
	for i, s := range all {
		if _, dup := m[s.Key]; dup {
			panic("settings: " + string(s.Key) + " is defined twice")
		}
		m[s.Key] = i
	}
	return m
}()

// All returns every setting, in display order.
func All() []Setting {
	return append([]Setting(nil), all...)
}

// In returns the settings of the file called name, in display order.
func In(file string) []Setting {
	var out []Setting
	for _, s := range all {
		if s.Key.File() == file {
			out = append(out, s)
		}
	}
	return out
}

// Lookup returns the definition of key.
func Lookup(key Key) (Setting, bool) {
	i, ok := index[key]
	if !ok {
		return Setting{}, false
	}
	return all[i], true
}

// Of returns the definition of a key of the catalog. It panics on a key that is not one: code names
// its settings by their constants, so that is a programming error.
func Of(key Key) Setting {
	s, ok := Lookup(key)
	if !ok {
		panic("settings: " + string(key) + " is not in the catalog")
	}
	return s
}

// SlotFree is the slot the visual session may take: workers take 1, 2, ..., the merger's checks of a
// base alone 96, the cycle's judgment 98, the merger 99.
const SlotFree = 97

func build() []Setting {
	var out []Setting

	// ---- .foreman-kit/pipeline.yaml
	out = append(out,
		Setting{
			Key: BaseBranch, Kind: KindBranch, Seed: "main",
			Means:  "the branch every change starts from and the merger publishes to. It has no value of the kit's own: without it the dispatcher and the merger do not start",
			ReadBy: "foreman-kit dispatch, foreman-kit merge, foreman-kit cycle, foreman-kit visual-qa, foreman-kit progress",
		},
		Setting{
			Key: Remote, Kind: KindText, Default: "origin",
			Means:  "the git remote the kit fetches from and pushes to",
			ReadBy: "foreman-kit dispatch, foreman-kit merge, foreman-kit cycle, foreman-kit visual-qa",
		},
		Setting{
			Key: WorktreesDir, Kind: KindPath, Default: ".worktrees",
			Means:  "where the worktree of each change lives, relative to the application root",
			ReadBy: "foreman-kit dispatch, foreman-kit merge, foreman-kit cycle, foreman-kit visual-qa",
		},
		Setting{
			Key: ProtectedPaths, Kind: KindList, Default: []string{"repo.config.yaml", ".github", ".gitlab-ci.yml"},
			Means:  "paths of a child repository a change may not touch, besides the repository's env_script",
			ReadBy: "foreman-kit dispatch, after each BUILD",
			Change: "edit " + ProtectedPaths.FilePath() + " by hand, then run check",
		},
		Setting{
			Key: Workers, Kind: KindInt, Min: 1, Default: 2,
			Means:  "changes built at the same time",
			ReadBy: "foreman-kit dispatch",
		},
		Setting{
			Key: PriorityBumpEvery, Kind: KindDuration, Never: true, Default: "6h",
			Means:  "a backlog item is ordered one priority higher each time it has waited this long, so a steady flow of higher ones does not starve it; never: off",
			ReadBy: "foreman-kit dispatch",
		},
		Setting{
			Key: RepairRounds, Kind: KindInt, Default: 2,
			Means:  "times a phase is sent back to its agent after it fails (PLAN: validation, BUILD: gates) before the change is blocked; 0: never sent back",
			ReadBy: "foreman-kit dispatch",
		},
		Setting{
			Key: GateTimeout, Kind: KindDuration, Default: "45m",
			Means:  "limit of one repository's gates: in a worker, at merge and on a base alone",
			ReadBy: "foreman-kit dispatch, foreman-kit merge",
		},
		Setting{
			Key: BlockedRetries, Kind: KindInt, Default: 1,
			Means:  "times a change blocked for a reason that can heal on its own (a failing gate, a rejected push, a stale manifest) runs again, never one a person must decide; 0: it waits for a person",
			ReadBy: "foreman-kit dispatch, foreman-kit status, foreman-kit progress",
		},
		Setting{
			Key: BlockedRetryAfter, Kind: KindDuration, Zero: true, Default: "1h",
			Means:  "how long a blocked change waits before each of those runs; 0s: it runs at the next dispatch",
			ReadBy: "foreman-kit dispatch, foreman-kit status, foreman-kit progress",
		},
		Setting{
			Key: OutsideCommits, Kind: KindEnum, Default: OutsideCommitsVerify,
			Values: []string{OutsideCommitsVerify, OutsideCommitsIgnore, OutsideCommitsInclude},
			Means:  fmt.Sprintf("commits pushed to a child repository outside the pipeline (a hotfix), which the merger integrates with the next change of that repository either way. verify: tested once on their own (the regression on the base alone when their files trigger it, and a gate that fails at merge rerun there); a base that fails holds that repository's changes, without blocking them, until it moves, its one recheck (%s later) passes, or foreman-kit base accept. ignore: never tested as part of a change: only the change's own commits count for its diff-aware tests and its regression. include: counted as that change's own, and tested with it", BaseRecheckAfter.Path()),
			ReadBy: "foreman-kit merge, foreman-kit base",
		},
		Setting{
			Key: BaseRecheckAfter, Kind: KindDuration, Never: true, Default: "1h",
			Means:  "a base that fails on its own is checked again once, this long after; never: only when it moves or a person accepts it",
			ReadBy: "foreman-kit merge, foreman-kit base",
		},
		Setting{
			Key: ConflictRebuilds, Kind: KindInt, Default: 3,
			Means:  "times a change that no longer rebases onto the base branch is rebuilt on it before it waits for a person as rebase_conflict; 0: never rebuilt",
			ReadBy: "foreman-kit dispatch, foreman-kit merge",
		},
		Setting{
			Key: LocksStaleAfter, Kind: KindDuration, Default: "2h",
			Means:  "a lock whose owner has not renewed it for this long may be taken by another process: the dispatcher's, the merger's and each change's",
			ReadBy: "foreman-kit dispatch, foreman-kit merge",
		},
		Setting{
			Key: DiagnosticsEnabled, Kind: KindBool, Default: false,
			Means:  "a model explains a failure in words (a failing gate, a refusal by the evaluator, a rebase conflict) and audits a plan; it never writes code. Its key comes from the environment",
			ReadBy: "foreman-kit dispatch, foreman-kit merge",
		},
		Setting{
			Key: DiagnosticsProvider, Kind: KindEnum, Default: "openai",
			Values: []string{"openai", "anthropic", "google", "openrouter", "ollama", "compat"},
			Means:  "who serves that model; compat is any OpenAI-compatible endpoint",
			ReadBy: "foreman-kit dispatch, foreman-kit merge",
		},
		Setting{
			Key: DiagnosticsModel, Kind: KindText, Default: "gpt-4o-mini",
			Means:  "the model",
			ReadBy: "foreman-kit dispatch, foreman-kit merge",
		},
		Setting{
			Key: DiagnosticsBaseURL, Kind: KindText, Empty: true, Default: "",
			Means:  "the endpoint, for compat and ollama; \"\": the provider's own",
			ReadBy: "foreman-kit dispatch, foreman-kit merge",
		},
		Setting{
			Key: DiagnosticsTimeout, Kind: KindDuration, Default: "20s",
			Means:  "limit of one request to that model",
			ReadBy: "foreman-kit dispatch, foreman-kit merge",
		},
		Setting{
			Key: DiagnosticsRetries, Kind: KindInt, Min: 1, Default: 2,
			Means:  "times an answer that does not fit what was asked is asked again",
			ReadBy: "foreman-kit dispatch, foreman-kit merge",
		},
	)

	// ---- .foreman-kit/agents.yaml
	out = append(out, Setting{
		Key:    Agents,
		Kind:   KindList,
		Seed:   []string{"claude"},
		Means:  "the agent CLIs installed in this application; each one has the kit's skills",
		ReadBy: "foreman-kit init; foreman-kit check-engines pings each one",
		Change: "foreman-kit update --agents LIST: it installs their skills and writes their commands",
	})
	for _, role := range Roles {
		out = append(out,
			Setting{
				Key:    RoleAgents(role),
				Kind:   KindAgents,
				Seed:   []string{"claude"},
				Means:  fmt.Sprintf("%s; agents in order of preference: one that fails is tried again %s times, then the next continues its work. A role without agents does not run", RoleDoes[role], AgentRetries.Path()),
				ReadBy: roleReaders[role],
			},
			roleTimeout(role),
			Setting{
				Key:     RoleArgs(role),
				Kind:    KindArgs,
				Default: map[string]any{},
				Means:   "arguments each agent's command gets in this role (a model, a provider, a profile): agent => a string split as a shell would split it, or a list. They replace {{args}} in the command, or follow it when it has no {{args}}",
				ReadBy:  roleReaders[role],
				Change:  "edit " + RoleArgs(role).FilePath() + " by hand, one entry per agent under " + RoleArgs(role).Path() + ", then run check",
			},
		)
	}
	out = append(out,
		Setting{
			Key:     AgentSelection,
			Kind:    KindEnum,
			Default: SelectionInOrder,
			Values:  []string{SelectionInOrder, SelectionRandom, SelectionLeastBusy},
			Means:   "how each call picks among the agents of its role. in_order: the first one, then the next when it fails; random: any of them first, then the others in the order listed, spreading the calls over the agents' quotas; least_busy: the one with the fewest calls running, so changes built at once spread over them",
			ReadBy:  "foreman-kit agent-exec, on every agent call",
		},
		Setting{
			Key:     AgentPermissions,
			Kind:    KindEnum,
			Default: PermissionsFull,
			Values:  []string{PermissionsFull, PermissionsRestricted},
			Means:   "full: each agent runs its run command; restricted: each runs its restricted command, and an agent without one cannot be used",
			ReadBy:  "foreman-kit agent-exec, on every agent call",
		},
		Setting{
			Key: IdleTimeout, Kind: KindDuration, Never: true, Default: "20m",
			Means:  "an agent that shows no work for this long is stopped and the attempt counts as failed: no CPU at the rate of work across its processes, no change in its worktree, no output. From a quarter of it on, foreman-kit status and the portal show the agent as idle; never: it is not watched",
			ReadBy: "foreman-kit agent-exec, foreman-kit status",
		},
		Setting{
			Key: IdleCPUPercent, Kind: KindInt, Default: 5,
			Means:  "CPU counts as work from this share of one core, in percent, between two looks of the watchdog: a frozen CLI still wakes up for its timers (about 1), a test run or a build uses far more; 0: any CPU use counts",
			ReadBy: "foreman-kit agent-exec",
		},
		Setting{
			Key: AgentRetries, Kind: KindInt, Default: 1,
			Means:  "extra attempts with the same agent after a failure, before the next agent of the role (none after a usage limit or a stall); 0: none",
			ReadBy: "foreman-kit agent-exec, foreman-kit dispatch",
		},
		Setting{
			Key: AgentRetryAfter, Kind: KindDuration, Zero: true, Default: "2m",
			Means:  "wait before each of those attempts; 0s: none",
			ReadBy: "foreman-kit agent-exec",
		},
		Setting{
			Key: PingTimeout, Kind: KindDuration, Default: "3m",
			Means:  "limit of the preflight's ping of each agent",
			ReadBy: "foreman-kit check-engines, foreman-kit preflight",
		},
		Setting{
			Key: OutageMaxWait, Kind: KindDuration, MinDuration: time.Minute, Default: "15m",
			Means:  "when every agent of a role is out (a usage limit, a lost login) no worker starts for one minute; the wait doubles on each further outage, up to this",
			ReadBy: "foreman-kit dispatch",
		},
		Setting{
			Key: OutageGiveUpAfter, Kind: KindInt, Min: 1, Never: true, Default: 3,
			Means:  "outages a change waits through before it is blocked; never: it waits as long as it takes",
			ReadBy: "foreman-kit dispatch",
		},
		Setting{
			Key:     AgentCommands,
			Kind:    KindCommands,
			Default: map[string]any{},
			Means:   "the commands that run each agent, exactly as written: the kit adds nothing to them. agent => run (" + AgentPermissions.Path() + ": full), restricted (" + AgentPermissions.Path() + ": restricted) and ping (the preflight), each a string split as a shell would split it, or a list. Placeholders: {{prompt}}, {{prompt_file}}, {{workdir}}, {{timeout}} (the role's timeout, in seconds) and {{args}} (the role's args). An agent whose CLI hands each call to a service outside the process the kit starts is detached: true, holds a role only with a stop command ({{workdir}}, {{pid}}), and lists in activity the files it writes as it works",
			ReadBy:  "foreman-kit agent-exec and foreman-kit check-engines, on every agent call and preflight ping",
			Change:  "edit " + AgentCommands.FilePath() + " by hand under commands, then run check; foreman-kit update --agents LIST writes the commands of an agent it adds",
		},
	)

	// ---- .foreman-kit/cycle.yaml
	out = append(out,
		Setting{
			Key: CycleBudget, Kind: KindDuration, MinDuration: time.Minute, Default: "45m",
			Means:  "what the judgment is told it may spend verifying and exploring. Its hard limit is " + RoleTimeout("cycle").Where(),
			ReadBy: "foreman-kit cycle, into the judgment's prompt",
		},
		Setting{
			Key: CycleExploring, Kind: KindDuration, MinDuration: time.Minute, Default: "15m",
			Means:  "at least this much of the budget goes to the strategy of the cycle",
			ReadBy: "foreman-kit cycle, into the judgment's prompt",
		},
		Setting{
			Key: QueueMaxFindings, Kind: KindInt, Min: 1, Default: 3,
			Means:  "findings one cycle queues or proposes; the rest stay in its report",
			ReadBy: "foreman-kit cycle",
		},
		Setting{
			Key: QueueMaxOpen, Kind: KindInt, Min: 1, Default: 8,
			Means:  "changes that may be open at once (queued, building, ready for the merger, merging, blocked or waiting to be rebuilt). The cycle queues what still fits, at most " + QueueMaxFindings.Path() + "; with none left it verifies and reports, an approved proposal waits for room, and the daemon starts no cycle before " + CycleEvery.Where(),
			ReadBy: "foreman-kit cycle, foreman-kit proposals review, foreman-kit visual-qa end, foreman-kit -d",
		},
		Setting{
			Key: RoadmapFile, Kind: KindPath, Empty: true, Seed: "ROADMAP.md",
			Means:  "the features to build, in priority order; \"\": no roadmap",
			ReadBy: "foreman-kit cycle, foreman-kit proposals, foreman-kit progress",
		},
		Setting{
			Key: RoadmapMaxProposals, Kind: KindInt, Min: 1, Default: 1,
			Means:  "roadmap features one cycle proposes",
			ReadBy: "foreman-kit cycle",
		},
		Setting{
			Key: RoadmapMaxOpen, Kind: KindInt, Min: 1, Default: 3,
			Means:  "no roadmap proposal while this many are proposed, approved or queued",
			ReadBy: "foreman-kit cycle",
		},
		Setting{
			Key: ProposalsApprovedBy, Kind: KindEnum, Default: ApprovalEvaluator,
			Values: []string{ApprovalEvaluator, ApprovalHuman, ApprovalNobody},
			Means:  "who approves a proposal before it is queued: the evaluator (gate proposal_review; what it does not approve waits for a person), a human, or nobody",
			ReadBy: "foreman-kit proposals after each judgment and visual session; foreman-kit cycle",
		},
		Setting{
			Key: PromptEditsApprovedBy, Kind: KindEnum, Default: ApprovalHuman,
			Values: []string{ApprovalHuman, ApprovalNobody},
			Means:  "who approves an edit of .foreman-kit/prompts/ the cycle proposes from evidence: a human (foreman-kit proposals show, then approve), or nobody (foreman-kit proposals applies it after the judgment)",
			ReadBy: "foreman-kit proposals after each judgment and visual session; foreman-kit cycle",
		},
		Setting{
			Key: MemoryMaxChars, Kind: KindInt, Min: 1, Default: 12000,
			Means:  "characters each file of the memory that applies to every change may hold (conventions.md, pitfalls.md and decisions.md of .agent-memory/). Over it, the next cycle and visual session are told to trim that file first",
			ReadBy: "foreman-kit cycle, foreman-kit visual-qa, foreman-kit config check",
		},
		Setting{
			Key: MemoryMaxCharsPerScope, Kind: KindInt, Min: 1, Default: 6000,
			Means:  "characters the notes of one repository or capability may hold (each file of .agent-memory/modules/, read only by the changes that touch it)",
			ReadBy: "foreman-kit cycle, foreman-kit visual-qa, foreman-kit config check",
		},
		Setting{
			Key: CycleSkills, Kind: KindList, Default: []string{},
			Means:  "the application's own QA skills the cycle uses",
			ReadBy: "the cycle's judgment",
			Change: "edit " + CycleSkills.FilePath() + ", for example skills: [live-qa-sweep]",
		},
	)

	// ---- .foreman-kit/daemon.yaml
	out = append(out,
		Setting{
			Key: CycleEvery, Kind: KindDuration, MinDuration: time.Minute, Default: "60m",
			Means:  "the longest the daemon waits between the start of two cycles; it builds and merges continuously, not on this interval",
			ReadBy: "foreman-kit -d, foreman-kit daemon",
		},
		Setting{
			Key: CycleEveryWhenOutOfWork, Kind: KindDuration, Never: true, MinDuration: time.Minute, Default: "15m",
			Means:  "with a worker free, nothing it can start, room under " + QueueMaxOpen.Where() + " and a change built, blocked or integrated in either of the last two cycles, the next cycle starts this long after the last one began; never: only " + CycleEvery.Path(),
			ReadBy: "foreman-kit -d, foreman-kit daemon",
		},
		Setting{
			Key: DaemonPortal, Kind: KindBool, Default: false,
			Means:  "the daemon serves the Web Portal; -w / --web on the command line turns it on for one run",
			ReadBy: "foreman-kit -d, foreman-kit daemon",
		},
		Setting{
			Key: DaemonBind, Kind: KindText, Default: "127.0.0.1",
			Means:  "the address the portal listens on",
			ReadBy: "foreman-kit -d -w, foreman-kit daemon",
		},
		Setting{
			Key: DaemonPort, Kind: KindInt, Min: 1, Default: 4567,
			Means:  "the port the portal listens on; the next free one when it is taken, unless --port names one",
			ReadBy: "foreman-kit -d -w, foreman-kit daemon",
		},
	)

	// ---- .foreman-kit/visual_qa.yaml
	out = append(out,
		Setting{
			Key: VisualQAEnabled, Kind: KindBool, Default: true,
			Means:  "a browser session in an agent app checks integrated changes and runs the strategies of its session; false: integrated changes are not queued for it",
			ReadBy: "foreman-kit cycle, foreman-kit visual-qa",
		},
		Setting{
			Key: VisualQATimeBudget, Kind: KindDuration, MinDuration: time.Minute, Default: "40m",
			Means:  "what one session is told it may spend",
			ReadBy: "foreman-kit visual-qa",
		},
		Setting{
			Key: VisualQAExpiresAfter, Kind: KindDuration, MinDuration: time.Minute, Default: "70m",
			Means:  "a session that never ended frees its lock after this",
			ReadBy: "foreman-kit visual-qa",
		},
		Setting{
			Key: VisualQAMaxChecks, Kind: KindInt, Min: 1, Default: 5,
			Means:  "queued checks one session takes; the rest wait",
			ReadBy: "foreman-kit visual-qa",
		},
		Setting{
			Key: VisualQAMaxFindings, Kind: KindInt, Min: 1, Default: 3,
			Means:  "findings one session queues or proposes; the rest stay in its report",
			ReadBy: "foreman-kit visual-qa",
		},
		Setting{
			Key: VisualQASlot, Kind: KindSlot, Default: SlotNone,
			Means:  fmt.Sprintf("the FOREMANKIT_SLOT of the session's commands, which run in the _visual_qa worktree. none: the ports and databases of the main checkout; a number gives it its own, as each repository's env_script derives them (%d is free: workers take 1, 2, ..., the merger's checks of a base alone 96, the cycle's judgment 98, the merger 99)", SlotFree),
			ReadBy: "foreman-kit visual-qa, foreman-kit exec",
		},
		Setting{
			Key: VisualQASkills, Kind: KindList, Default: []string{},
			Means:  "the application's own QA skills the session uses",
			ReadBy: "foreman-kit visual-qa",
			Change: "edit " + VisualQASkills.FilePath() + ", for example skills: [browser-qa]",
		},
	)

	// ---- .foreman-kit/exploration.yaml
	out = append(out,
		Setting{
			Key: ExplorationSelection, Kind: KindEnum, Default: ExplorationInOrder,
			Values: []string{ExplorationInOrder, ExplorationRandom},
			Means:  "how each run picks a strategy of its own session. in_order: in file order; random: never one of the most recent half again",
			ReadBy: "foreman-kit cycle, foreman-kit visual-qa",
		},
		Setting{
			Key: ExplorationStrategies, Kind: KindList,
			Means:  "slug, name, kind, session, enabled and directive of each strategy",
			ReadBy: "foreman-kit cycle, foreman-kit visual-qa",
			Change: "edit " + ExplorationStrategies.FilePath() + " by hand, then run check",
		},
	)

	// ---- .foreman-kit/evaluator.yaml
	out = append(out,
		Setting{
			Key: EvaluatorEnabled, Kind: KindBool, Default: true,
			Means:  "false: every gate is off",
			ReadBy: "foreman-kit dispatch, foreman-kit merge, foreman-kit proposals, foreman-kit check-engines",
		},
		Setting{
			Key: EvaluatorModel, Kind: KindText, Default: "jev-1.14.0",
			Means:  "the pinned, versioned model that answers the gates; the provider's name appears only here",
			ReadBy: "foreman-kit dispatch, foreman-kit check-engines",
		},
		Setting{
			Key: EvaluatorBaseURL, Kind: KindText, Empty: true, Default: "",
			Means:  "the endpoint of the evaluator; \"\": the provider's own",
			ReadBy: "foreman-kit dispatch, foreman-kit check-engines",
		},
		Setting{
			Key: EvaluatorTimeout, Kind: KindDuration, Default: "30s",
			Means:  "limit of one attempt",
			ReadBy: "foreman-kit dispatch, foreman-kit proposals",
		},
		Setting{
			Key: EvaluatorRetries, Kind: KindInt, Default: 2,
			Means:  "times a question is asked again when the evaluator does not answer",
			ReadBy: "foreman-kit dispatch, foreman-kit proposals",
		},
		Setting{
			Key: EvaluatorWhenUnavailable, Kind: KindEnum, Default: UnavailablePass,
			Values: []string{UnavailablePass, UnavailableBlock, UnavailableStop},
			Means:  "when the evaluator does not answer. pass: the gate passes; block: an enforce gate blocks its change; stop: like block, and the preflight fails, so no cycle judges and no worker starts",
			ReadBy: "foreman-kit dispatch, foreman-kit proposals, foreman-kit check-engines",
		},
		Setting{
			Key: EvaluatorStateMaxChars, Kind: KindInt, Min: 1, Default: 24000,
			Means:  "every string sent to the evaluator is clipped to this",
			ReadBy: "foreman-kit dispatch, foreman-kit proposals",
		},
		Setting{
			Key: EvaluatorIncludeDiffs, Kind: KindBool, Default: true,
			Means:  "false: change_risk gets file lists and diffstats, never code",
			ReadBy: "foreman-kit dispatch",
		},
		Setting{
			Key: EvaluatorAbstainBand, Kind: KindList, Default: []float64{0.4, 0.6},
			Means:  "a score inside it is \"cannot tell\"; an enforce gate still blocks on it",
			ReadBy: "foreman-kit dispatch, foreman-kit proposals",
			Change: "edit " + EvaluatorAbstainBand.FilePath() + " by hand, then run check",
		},
	)
	for _, g := range gates {
		out = append(out, Setting{
			Key:     GateMode(g.name),
			Kind:    KindEnum,
			Default: g.seed,
			Values:  GateModes,
			Means:   "off | advisory (answers recorded) | enforce (answers decide)",
			ReadBy:  g.reader,
		})
	}
	return out
}

// roleTimeout returns the setting that limits one call of role.
func roleTimeout(role string) Setting {
	s := Setting{
		Key:     RoleTimeout(role),
		Kind:    KindDuration,
		Default: roleTimeouts[role],
		Means:   "limit of one call of this role; {{timeout}} in the agent's command, in seconds",
		ReadBy:  roleReaders[role],
	}
	if role == "cycle" {
		s.Means = "limit of the cycle's judgment; foreman-kit config check warns when " + CycleBudget.Where() + " does not fit in it"
	}
	return s
}
