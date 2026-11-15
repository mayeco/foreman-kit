package cycle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/config"
)

// installedWithCustomAgent installs an application for claude, then gives .foreman-kit/agents.yaml
// commands for myagent, an agent the kit does not ship, and lists it as agents and roles say.
func installedWithCustomAgent(t *testing.T, agents []string, roles map[string][]string) string {
	t.Helper()
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "1")
	ctx := context.Background()
	app := filepath.Join(t.TempDir(), "app")
	if err := Init(ctx, app, KitOptions{Agents: "claude"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(app, ".foreman-kit", "agents.yaml")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := yaml.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	commands, _ := data["commands"].(map[string]any)
	commands["myagent"] = map[string]any{"run": "myagent --prompt {{prompt_file}} {{args}}", "ping": "myagent --version"}
	text, err := yaml.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, text, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.Write(app, agents, roles, ""); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, app)
	commitAll(t, app, "an application that lists a custom agent")
	return app
}

// TestUpdateKeepsACustomAgentThatHasCommands: an application whose agents.yaml lists an agent the kit
// does not ship, with commands, is updated instead of refused with `unknown agent`. The agent, its
// commands and the role that names it are kept, and the drift check does not ask for its skills.
func TestUpdateKeepsACustomAgentThatHasCommands(t *testing.T) {
	ctx := context.Background()
	app := installedWithCustomAgent(t, []string{"claude", "myagent"}, map[string][]string{
		"plan": {"claude"}, "build": {"myagent", "claude"}, "cycle": {"claude"},
	})

	var out bytes.Buffer
	if ok, err := Verify(app, &out); err != nil || !ok {
		t.Fatalf("verify before the update: %v %v\n%s", ok, err, out.String())
	}
	out.Reset()
	if err := Upgrade(ctx, app, KitOptions{Version: "9.0.0-test"}, &out); err != nil {
		t.Fatalf("Upgrade: %v\n%s", err, out.String())
	}
	cfg := LoadAgentsConfig(app)
	if got := strings.Join(cfg.Agents, ","); got != "claude,myagent" {
		t.Errorf("agents: lists %q after the update", got)
	}
	if got := strings.Join(cfg.Roles["build"], ","); got != "myagent,claude" {
		t.Errorf("the build role lists %q after the update", got)
	}
	spec, written, _ := config.CommandsFor(app, "myagent")
	if !written || spec.Ping != "myagent --version" || !strings.Contains(spec.Run.(string), "myagent --prompt") {
		t.Errorf("the commands of myagent after the update: %+v (written %v)", spec, written)
	}
	if rep := config.Check(app)[".foreman-kit/agents.yaml"]; len(rep.Errors) != 0 {
		t.Errorf("config check after the update: %v", rep.Errors)
	}
	out.Reset()
	if ok, err := Verify(app, &out); err != nil || !ok {
		t.Errorf("verify after the update: %v %v\n%s", ok, err, out.String())
	}
	if drift := config.CheckKitDrift(app); drift != "" {
		t.Errorf("drift after the update: %s", drift)
	}
}

// TestUpdateKeepsARoleThatNamesACustomAgentOutsideAgents: `config set` lets a role name a custom
// agent without listing it in agents:, and an update afterwards keeps that role as it is.
func TestUpdateKeepsARoleThatNamesACustomAgentOutsideAgents(t *testing.T) {
	ctx := context.Background()
	app := installedWithCustomAgent(t, []string{"claude"}, map[string][]string{
		"plan": {"claude"}, "build": {"myagent", "claude"}, "cycle": {"claude"},
	})
	var out bytes.Buffer
	if err := Upgrade(ctx, app, KitOptions{Version: "9.0.0-test"}, &out); err != nil {
		t.Fatalf("Upgrade: %v\n%s", err, out.String())
	}
	cfg := LoadAgentsConfig(app)
	if got := strings.Join(cfg.Roles["build"], ","); got != "myagent,claude" {
		t.Errorf("the build role lists %q after the update", got)
	}
	if got := strings.Join(cfg.Agents, ","); got != "claude" {
		t.Errorf("agents: lists %q after the update", got)
	}
	if _, written, _ := config.CommandsFor(app, "myagent"); !written {
		t.Error("the commands of myagent were lost")
	}
}

// TestUpdateAgentsNamesACustomAgentOnlyWithCommands: --agents takes a custom agent the file gives
// commands, and still refuses a name it knows nothing of.
func TestUpdateAgentsNamesACustomAgentOnlyWithCommands(t *testing.T) {
	ctx := context.Background()
	app := installedWithCustomAgent(t, []string{"claude"}, map[string][]string{
		"plan": {"claude"}, "build": {"claude"}, "cycle": {"claude"},
	})
	var out bytes.Buffer
	if err := Upgrade(ctx, app, KitOptions{Version: "9.0.0-test", Agents: "claude,ghost"}, &out); err == nil || !strings.Contains(err.Error(), "unknown agent ghost") {
		t.Fatalf("an agent with no commands: %v", err)
	}
	if err := Upgrade(ctx, app, KitOptions{Version: "9.0.0-test", Agents: "claude,myagent", Roles: map[string]string{"build": "myagent"}}, &out); err != nil {
		t.Fatalf("--agents claude,myagent: %v\n%s", err, out.String())
	}
	cfg := LoadAgentsConfig(app)
	if got := strings.Join(cfg.Agents, ","); got != "claude,myagent" {
		t.Errorf("agents: lists %q", got)
	}
	if got := strings.Join(cfg.Roles["build"], ","); got != "myagent" {
		t.Errorf("the build role lists %q", got)
	}
}

// TestResolveSelectionKeepsCustomAgents: a custom agent is a valid name among the agents and in a
// role, and a role that names one keeps its list even when one agent is listed.
func TestResolveSelectionKeepsCustomAgents(t *testing.T) {
	cur := AgentsFileConfig{
		Agents: []string{"claude"},
		Roles:  map[string][]string{"build": {"myagent", "claude"}, "plan": {"claude"}},
		Custom: map[string]bool{"myagent": true},
	}
	agents, roles, err := resolveSelection(cur, KitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(agents, ",") != "claude" || strings.Join(roles["build"], ",") != "myagent,claude" || strings.Join(roles["plan"], ",") != "claude" {
		t.Errorf("agents %v, roles %v", agents, roles)
	}
	if _, _, err := resolveSelection(AgentsFileConfig{}, KitOptions{Agents: "claude,myagent"}); err == nil || !strings.Contains(err.Error(), "unknown agent myagent") {
		t.Errorf("a custom agent the file does not give commands: %v", err)
	}
}
