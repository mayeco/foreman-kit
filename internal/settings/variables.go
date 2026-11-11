package settings

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// FormerVariable is an environment variable an older foreman-kit read to change what it did. None
// is read any more: a value for one run is given with `foreman-kit --set`, and the value an
// application always wants is written in its file.
type FormerVariable struct {
	Name string
	// Keys are the settings the variable overrode (none for a variable that was not a setting).
	Keys []Key
	// Now says what does its job today, when it is not just the keys.
	Now string
	// Secret says the variable held a key: its value is never shown.
	Secret bool
}

// shown returns the variable as a message names it: with its value, or only that it is set when it
// holds a secret.
func (v FormerVariable) shown(value string) string {
	if v.Secret {
		return v.Name + " is set"
	}
	return v.Name + "=" + value + " is set"
}

// stillSet says the variable is set with value and changes nothing, what does its job now, and how
// it leaves the environment: a shell exports it, or the .env file of the directory foreman-kit runs
// in writes it, and unsetting it in the shell does nothing about the second.
func (v FormerVariable) stillSet(value string) string {
	return fmt.Sprintf("%s and no longer read: %s, and take it out of the environment (unset it, or delete its line from .env)", v.shown(value), v.Instead(value))
}

// Instead says what to do in place of exporting the variable with value.
func (v FormerVariable) Instead(value string) string {
	if v.Now != "" {
		return v.Now
	}
	if len(v.Keys) == 0 {
		return "nothing reads it"
	}
	var sets []string
	for _, key := range v.Keys {
		if Of(key).Settable() {
			sets = append(sets, fmt.Sprintf("--set %s=<value>", key))
		}
	}
	if len(sets) == 0 {
		return fmt.Sprintf("write %s", v.Keys[0].Where())
	}
	return fmt.Sprintf("write %s, or give it to one run with %s", v.Keys[0].Where(), strings.Join(sets, " "))
}

// Files returns the names of the files whose readers the variable changed.
func (v FormerVariable) Files() []string {
	seen := map[string]bool{}
	var out []string
	for _, key := range v.Keys {
		if !seen[key.File()] {
			seen[key.File()] = true
			out = append(out, key.File())
		}
	}
	return out
}

var formerVariables = func() []FormerVariable {
	// The command of each agent is written whole in its file: a list of tools holds when it is
	// written there, and a list that restricted an agent from the environment restricts nothing.
	tools := fmt.Sprintf("the tools an agent may use are written in its own command: edit %s", AgentCommands.Where())
	out := []FormerVariable{
		{Name: "OPENSPEC_BASE_BRANCH", Keys: []Key{BaseBranch}},
		{Name: "OPENSPEC_REMOTE", Keys: []Key{Remote}},
		{Name: "OPENSPEC_WORKTREE_BASE", Keys: []Key{WorktreesDir}},
		{Name: "OPENSPEC_PROTECTED_PATHS", Keys: []Key{ProtectedPaths}},
		{Name: "OPENSPEC_WORKERS", Keys: []Key{Workers}},
		{Name: "OPENSPEC_AGING_HOURS", Keys: []Key{PriorityBumpEvery}},
		{Name: "OPENSPEC_GATE_TIMEOUT", Keys: []Key{GateTimeout}},
		{Name: "OPENSPEC_BLOCKED_RETRIES", Keys: []Key{BlockedRetries}},
		{Name: "OPENSPEC_BLOCKED_RETRY_HOURS", Keys: []Key{BlockedRetryAfter}},
		{Name: "OPENSPEC_MERGE_CHANGED_FILES", Keys: []Key{OutsideCommits}},
		{Name: "OPENSPEC_CONFLICT_REBUILDS", Keys: []Key{ConflictRebuilds}},
		{Name: "OPENSPEC_LEASE_TTL", Keys: []Key{LocksStaleAfter}},
		{Name: "OPENSPEC_LLM_ENABLED", Keys: []Key{DiagnosticsEnabled}},
		{Name: "OPENSPEC_LLM_PROVIDER", Keys: []Key{DiagnosticsProvider}},
		{Name: "OPENSPEC_LLM_MODEL", Keys: []Key{DiagnosticsModel}},
		{Name: "OPENSPEC_LLM_BASE_URL", Keys: []Key{DiagnosticsBaseURL}},
		{Name: "AGENT_ENGINE", Keys: []Key{RoleAgents("plan"), RoleAgents("build"), RoleAgents("cycle")}},
		{Name: "AGENT_ENGINE_PLAN", Keys: []Key{RoleAgents("plan")}},
		{Name: "AGENT_ENGINE_BUILD", Keys: []Key{RoleAgents("build")}},
		{Name: "AGENT_ENGINE_CYCLE", Keys: []Key{RoleAgents("cycle")}},
		{Name: "AGENT_TIMEOUT_SECONDS", Keys: []Key{RoleTimeout("plan"), RoleTimeout("build")}},
		{Name: "AGENT_ORDER", Keys: []Key{AgentSelection}},
		{Name: "AGENT_PERMISSIONS", Keys: []Key{AgentPermissions}},
		{Name: "AGENT_IDLE_SECONDS", Keys: []Key{IdleTimeout}},
		{Name: "AGENT_IDLE_CPU_PERCENT", Keys: []Key{IdleCPUPercent}},
		{Name: "AGENT_RETRIES", Keys: []Key{AgentRetries}},
		{Name: "AGENT_RETRY_DELAY_SECONDS", Keys: []Key{AgentRetryAfter}},
		{Name: "OPENSPEC_PING_SECONDS", Keys: []Key{PingTimeout}},
		{Name: "JEV_MODEL", Keys: []Key{EvaluatorModel}},
		{Name: "JEV_BASE_URL", Keys: []Key{EvaluatorBaseURL}},
		{Name: "JEV_DISABLED", Keys: []Key{EvaluatorEnabled},
			Now: fmt.Sprintf("write %s: false in %s, or give it to one run with --set %s=false", EvaluatorEnabled.Path(), EvaluatorEnabled.FilePath(), EvaluatorEnabled)},
		{Name: "JEV_REQUIRED", Keys: []Key{EvaluatorWhenUnavailable},
			Now: fmt.Sprintf("write %s: %s in %s, or give it to one run with --set %s=%s", EvaluatorWhenUnavailable.Path(), UnavailableStop, EvaluatorWhenUnavailable.FilePath(), EvaluatorWhenUnavailable, UnavailableStop)},
		{Name: "OPENSPEC_LLM_API_KEY", Keys: []Key{DiagnosticsEnabled}, Secret: true,
			Now: "the key of the diagnostics assistant is read from FOREMANKIT_DIAGNOSTICS_API_KEY, or from the provider's own variable: export it under that name"},
		{Name: "CLAUDE_ALLOWED_TOOLS", Keys: []Key{AgentCommands}, Now: tools},
		{Name: "CLAUDE_DISALLOWED_TOOLS", Keys: []Key{AgentCommands}, Now: tools},
		{Name: "COPILOT_DENIED_TOOLS", Keys: []Key{AgentCommands}, Now: tools},
		{Name: "JEV_API_KEY", Keys: []Key{EvaluatorEnabled}, Secret: true,
			Now: "the key of the evaluator is read from TYPESAFE_API_KEY: export it under that name"},
		{Name: "OPENSPEC_RECORD_LOCK_SECONDS",
			Now: "how long a process waits for the lock of the checkout it records in is not a setting: nothing replaces it"},
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}()

// FormerVariables returns the environment variables an older foreman-kit read, sorted by name.
func FormerVariables() []FormerVariable {
	return append([]FormerVariable(nil), formerVariables...)
}

// FormerVariablesSet returns the variables an older foreman-kit read that are set in this
// environment, each with its value: they change nothing any more, and whoever exports them expects
// that they do.
func FormerVariablesSet() []Problem {
	var out []Problem
	for _, v := range formerVariables {
		value := strings.TrimSpace(os.Getenv(v.Name))
		if value == "" {
			continue
		}
		p := Problem{
			File: "the environment",
			Text: v.stillSet(value),
		}
		if len(v.Keys) > 0 {
			p.Key = v.Keys[0]
		}
		out = append(out, p)
	}
	return out
}

// formerVariablesOf returns the former variables set in this environment that changed what a
// reader of one of the files called names did.
func formerVariablesOf(names ...string) []Problem {
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	var out []Problem
	for _, v := range formerVariables {
		value := strings.TrimSpace(os.Getenv(v.Name))
		if value == "" {
			continue
		}
		for _, file := range v.Files() {
			if wanted[file] {
				out = append(out, Problem{
					File: "the environment",
					Key:  v.Keys[0],
					Text: v.stillSet(value),
				})
				break
			}
		}
	}
	return out
}
