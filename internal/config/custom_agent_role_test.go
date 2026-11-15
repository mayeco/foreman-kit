package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// writeApplicationWithCustomAgent writes the files of an application whose agents: lists only claude
// and whose commands: also give myagent, an agent the kit does not ship.
func writeApplicationWithCustomAgent(t *testing.T) string {
	t.Helper()
	t.Setenv(settings.RunOverridesEnv, "")
	root := t.TempDir()
	settingstest.WriteFiles(t, root, nil)
	p := filepath.Join(root, ".foreman-kit", "agents.yaml")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := yaml.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	commands, _ := data["commands"].(map[string]any)
	if commands == nil {
		commands = map[string]any{}
	}
	commands["myagent"] = map[string]any{"run": "myagent --prompt {{prompt_file}} {{args}}", "ping": "myagent --version"}
	data["commands"] = commands
	text, err := yaml.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, text, 0o644); err != nil {
		t.Fatal(err)
	}
	// The file is written again as update writes it: the commands the kit ships, and those of myagent.
	claude := []string{"claude"}
	if err := Write(root, claude, map[string][]string{"plan": claude, "build": claude, "cycle": claude}, ""); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestSetAcceptsARoleNamingACustomAgentWithCommands: an agent the kit does not ship and agents.yaml
// gives commands is named in a role by `config set` without being among the agents: only a shipped
// agent needs its skills installed. One with no commands is refused, and so is a shipped agent the
// application does not list.
func TestSetAcceptsARoleNamingACustomAgentWithCommands(t *testing.T) {
	root := writeApplicationWithCustomAgent(t)
	key := string(settings.RoleAgents("build"))
	var out bytes.Buffer
	if err := Set(root, key, "myagent,claude", false, &out); err != nil {
		t.Fatalf("config set %s myagent,claude: %v", key, err)
	}
	cfg := Load(root)
	if got := strings.Join(cfg.Roles["build"], ","); got != "myagent,claude" {
		t.Errorf("the build role lists %q", got)
	}
	if got := strings.Join(cfg.Agents, ","); got != "claude" {
		t.Errorf("agents: lists %q, want claude alone", got)
	}
	if rep := Check(root)[".foreman-kit/agents.yaml"]; len(rep.Errors) != 0 {
		t.Errorf("config check after the set: %v", rep.Errors)
	}
	if err := Set(root, key, "[myagent]", true, &out); err != nil {
		t.Errorf("a custom agent written as the file holds it: %v", err)
	}

	if err := Set(root, key, "ghost", true, &out); err == nil || !strings.Contains(err.Error(), "must list agents among") {
		t.Errorf("an agent with no commands: %v", err)
	}
	if err := Set(root, key, "myagent,codex", true, &out); err == nil || !strings.Contains(err.Error(), "codex not among the agents of .foreman-kit/agents.yaml (claude)") {
		t.Errorf("a shipped agent outside agents:: %v", err)
	}
}

// TestCheckRunOverridesAcceptsARoleNamingACustomAgentWithCommands: --set takes the same value `config
// set` takes, so the run does not stop before it starts.
func TestCheckRunOverridesAcceptsARoleNamingACustomAgentWithCommands(t *testing.T) {
	root := writeApplicationWithCustomAgent(t)
	build := settings.RoleAgents("build")
	settingstest.Give(t, build, "myagent")
	if err := CheckRunOverrides(root); err != nil {
		t.Errorf("--set %s=myagent: %v", build, err)
	}
	settingstest.Give(t, build, "myagent,codex")
	if err := CheckRunOverrides(root); err == nil || !strings.Contains(err.Error(), "codex not among the agents") {
		t.Errorf("--set with a shipped agent outside agents:: %v", err)
	}
	settingstest.Give(t, build, "ghost")
	if err := CheckRunOverrides(root); err == nil || !strings.Contains(err.Error(), "must list agents among") {
		t.Errorf("--set with an agent with no commands: %v", err)
	}
}

func writeLock(t *testing.T, root string, agents ...string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"version": "1.0.0", "agents": agents, "files": map[string]any{}})
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "kit.lock"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestKitDriftDoesNotAskForTheSkillsOfACustomAgent: kit.lock records where skills are installed, and
// the kit has none to install for an agent it does not ship.
func TestKitDriftDoesNotAskForTheSkillsOfACustomAgent(t *testing.T) {
	root := writeApplicationWithCustomAgent(t)
	claude := []string{"claude"}
	if err := Write(root, []string{"claude", "myagent"}, map[string][]string{"plan": claude, "build": claude, "cycle": claude}, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(Load(root).Agents, ","); got != "claude,myagent" {
		t.Fatalf("agents: lists %s", got)
	}
	writeLock(t, root, "claude")
	if drift := CheckKitDrift(root); drift != "" {
		t.Errorf("a custom agent in agents: is drift: %s", drift)
	}
	if rep := Check(root)[".foreman-kit/agents.yaml"]; len(rep.Errors) != 0 {
		t.Errorf("config check: %v", rep.Errors)
	}
	writeLock(t, root, "claude", "myagent")
	if drift := CheckKitDrift(root); drift != "" {
		t.Errorf("a lock that holds the custom agent is drift: %s", drift)
	}
	writeLock(t, root, "codex")
	if drift := CheckKitDrift(root); !strings.Contains(drift, "skills installed for codex") {
		t.Errorf("another shipped agent in the lock is not drift: %q", drift)
	}
}
