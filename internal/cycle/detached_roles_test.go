package cycle

import "testing"

// The roles init and update --agents choose by themselves skip an agent that cannot hold one (hermes:
// detached, with no stop command); one a person names stays theirs, and config check says why.
func TestDefaultRolesSkipAnAgentThatCannotHoldOne(t *testing.T) {
	_, roles, err := resolveSelection(AgentsFileConfig{}, KitOptions{Agents: "hermes,claude"})
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"plan", "build", "cycle"} {
		if got := roles[role]; len(got) != 1 || got[0] != "claude" {
			t.Errorf("%s = %v, want [claude]", role, got)
		}
	}
	_, roles, err = resolveSelection(AgentsFileConfig{}, KitOptions{Agents: "hermes,claude", Roles: map[string]string{"build": "hermes"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := roles["build"]; len(got) != 1 || got[0] != "hermes" {
		t.Errorf("build = %v, want the hermes the person named", got)
	}
	// Alone, hermes is all there is: it is named, and config check refuses it.
	_, roles, _ = resolveSelection(AgentsFileConfig{}, KitOptions{Agents: "hermes"})
	if got := roles["plan"]; len(got) != 1 || got[0] != "hermes" {
		t.Errorf("plan = %v, want [hermes]", got)
	}
}
