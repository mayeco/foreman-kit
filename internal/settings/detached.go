package settings

import (
	"fmt"
	"strings"
)

// DetachedHoldsNoRole is the problem of a role of .foreman-kit/agents.yaml that names a detached
// agent with no stop command: its CLI hands each call to a service outside the process the kit
// starts, so killing that process does not stop the work, and nothing else could.
func DetachedHoldsNoRole(role, agent string) string {
	at := AgentCommands.Path() + "." + agent
	return fmt.Sprintf("%s: %s is detached (%s.detached: its CLI hands each call to a service outside the process the kit starts) and has no %s.stop: "+
		"the kit could not stop its calls, which would keep working in the worktree after it moves on. Name another agent in the role, or write the command that stops one call of %s in %s.stop",
		RoleAgents(role).Path(), agent, at, at, agent, at)
}

// detachedRoleProblems lists the roles of the agents file data that name a detached agent with no
// stop command (DetachedHoldsNoRole).
func detachedRoleProblems(file string, data map[string]any) []Problem {
	commands, _ := dig(data, AgentCommands.Parts()...).(map[string]any)
	if len(commands) == 0 {
		return nil
	}
	var out []Problem
	for _, role := range Roles {
		key := RoleAgents(role)
		for _, agent := range agentNamesOf(dig(data, key.Parts()...)) {
			entry, _ := commands[agent].(map[string]any)
			if detached, _ := entry["detached"].(bool); !detached || entry["stop"] != nil {
				continue
			}
			out = append(out, Problem{File: file, Key: key, Text: DetachedHoldsNoRole(role, agent), Choice: true})
		}
	}
	return out
}

// agentNamesOf reads a list of agents as a role writes it: a list, or names separated by commas.
func agentNamesOf(raw any) []string {
	var names []string
	add := func(text string) {
		for _, name := range strings.Split(text, ",") {
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, name)
			}
		}
	}
	switch v := raw.(type) {
	case string:
		add(v)
	case []string:
		for _, item := range v {
			add(item)
		}
	case []any:
		for _, item := range v {
			if text, ok := item.(string); ok {
				add(text)
			}
		}
	}
	return names
}
