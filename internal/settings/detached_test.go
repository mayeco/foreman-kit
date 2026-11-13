package settings

import (
	"strings"
	"testing"
)

func agentsData(buildAgents []any, hermes map[string]any) map[string]any {
	return map[string]any{
		"roles": map[string]any{
			"plan":  map[string]any{"agents": []any{"claude"}},
			"build": map[string]any{"agents": buildAgents},
			"cycle": map[string]any{"agents": "claude"},
		},
		"commands": map[string]any{
			"claude": map[string]any{"run": "claude -p {{prompt}} {{args}}"},
			"hermes": hermes,
		},
	}
}

// A role that names a detached agent without a stop command is a problem of the agents file, which a
// person mends by choosing another agent (Choice); with a stop command, or named by no role, it is not.
func TestADetachedAgentWithoutStopHoldsNoRole(t *testing.T) {
	file := AgentCommands.FilePath()
	detached := map[string]any{"run": "hermes -z {{prompt}} {{args}}", "detached": true}

	got := detachedRoleProblems(file, agentsData([]any{"hermes", "claude"}, detached))
	if len(got) != 1 || got[0].Key != RoleAgents("build") || !got[0].Choice || got[0].File != file {
		t.Fatalf("problems = %+v, want one Choice problem of roles.build.agents", got)
	}
	for _, want := range []string{"roles.build.agents", "hermes is detached", "commands.hermes.stop", "Name another agent in the role"} {
		if !strings.Contains(got[0].Text, want) {
			t.Errorf("the problem does not say %q: %s", want, got[0].Text)
		}
	}

	// Written as a comma-separated line, the role names it all the same.
	if got := detachedRoleProblems(file, agentsData([]any{"claude, hermes"}, detached)); len(got) != 1 {
		t.Fatalf("a role written as a line was not checked: %+v", got)
	}

	withStop := map[string]any{"run": "hermes -z {{prompt}} {{args}}", "detached": true, "stop": "hermes stop {{pid}}"}
	if got := detachedRoleProblems(file, agentsData([]any{"hermes"}, withStop)); len(got) != 0 {
		t.Fatalf("a detached agent with a stop command was refused a role: %+v", got)
	}
	if got := detachedRoleProblems(file, agentsData([]any{"claude"}, detached)); len(got) != 0 {
		t.Fatalf("a detached agent named by no role is a problem: %+v", got)
	}
	notDetached := map[string]any{"run": "hermes -z {{prompt}} {{args}}"}
	if got := detachedRoleProblems(file, agentsData([]any{"hermes"}, notDetached)); len(got) != 0 {
		t.Fatalf("an agent that is not detached is a problem: %+v", got)
	}
}
