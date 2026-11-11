package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
)

func writeAgentsYAML(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "agents.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestArgsOfARole: an agent gets the arguments its role gives it, and none in a role that names
// none for it. The preflight's ping gets those of the first role that lists the agent.
func TestArgsOfARole(t *testing.T) {
	root := writeAgentsYAML(t, `agents: [claude, codex, kilo]
roles:
  plan:
    agents: [claude, codex]
    args: {claude: "--model claude-opus-5-5", codex: "-m gpt-x"}
  build:
    agents: [claude, codex]
    args:
      claude: ["--model", "claude-sonnet-5-5"]
      codex: "-m gpt-x"
  cycle:
    agents: [kilo]
    args: {kilo: "-m cycle-model"}
`)
	cases := []struct {
		agent, role string
		want        []string
	}{
		{"claude", "plan", []string{"--model", "claude-opus-5-5"}},
		{"claude", "build", []string{"--model", "claude-sonnet-5-5"}},
		{"claude", "cycle", []string{}},
		{"claude", "", []string{}},
		{"codex", "plan", []string{"-m", "gpt-x"}},
		{"kilo", "cycle", []string{"-m", "cycle-model"}},
		{"kilo", "plan", []string{}},
		{"opencode", "plan", []string{}},
	}
	for _, c := range cases {
		var errBuf bytes.Buffer
		got := ExtraArgsForRole(root, c.agent, c.role, &errBuf)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ExtraArgsForRole(%s, %q) = %q, want %q", c.agent, c.role, got, c.want)
		}
		if errBuf.Len() > 0 {
			t.Errorf("unexpected stderr for %s/%s: %s", c.agent, c.role, errBuf.String())
		}
	}
	// The ping checks what a real call of the agent's first role would use.
	for agent, want := range map[string][]string{
		"claude":   {"--model", "claude-opus-5-5"},
		"kilo":     {"-m", "cycle-model"},
		"opencode": {},
	} {
		if got := ExtraArgsWithStderr(root, agent, nil); !reflect.DeepEqual(got, want) {
			t.Errorf("the ping of %s gets %q, want %q", agent, got, want)
		}
	}
}

func TestArgsOfARoleThatCannotBeSplit(t *testing.T) {
	root := writeAgentsYAML(t, `agents: [claude]
roles:
  plan:
    agents: [claude]
    args: {claude: "--model 'unterminated"}
`)
	var errBuf bytes.Buffer
	if got := ExtraArgsForRole(root, "claude", "plan", &errBuf); len(got) != 0 {
		t.Fatalf("expected no args, got %q", got)
	}
	if !strings.Contains(errBuf.String(), "roles.plan.args.claude") {
		t.Fatalf("expected the key in the error, got %q", errBuf.String())
	}
}

func TestCheckArgsOfARole(t *testing.T) {
	setting := settings.Of(settings.RoleArgs("plan"))
	ok := map[string]any{"claude": "--model a", "codex": []any{"--model", "b"}}
	if msg := checkSettingTypeProblem("", setting, ok); msg != "" {
		t.Fatalf("expected valid arguments, got %q", msg)
	}
	// What extra_args held per role is not a value of today: the role is the key's own.
	perRole := map[string]any{"claude": map[string]any{"plan": "--model a"}}
	if msg := checkSettingTypeProblem("", setting, perRole); !strings.Contains(msg, "claude: must be a string or a list of strings") {
		t.Fatalf("expected a mapping per role to be refused, got %q", msg)
	}
	unsplittable := map[string]any{"claude": "--model 'a"}
	if msg := checkSettingTypeProblem("", setting, unsplittable); !strings.Contains(msg, "claude: cannot be split") {
		t.Fatalf("expected arguments that cannot be split to be refused, got %q", msg)
	}
}

// TestACommandRunsOnlyWhatItWrites: the placeholders a command may hold are named, and anything else
// in double braces is refused: what it stood for is written in the command.
func TestACommandRunsOnlyWhatItWrites(t *testing.T) {
	setting := settings.Of(settings.AgentCommands)
	ok := map[string]any{"claude": map[string]any{"run": "claude -p {{prompt}} {{args}}", "restricted": []any{"claude", "-p", "{{prompt}}"}, "ping": "claude -p {{prompt}}"}}
	if msg := checkSettingTypeProblem("", setting, ok); msg != "" {
		t.Fatalf("expected valid commands, got %q", msg)
	}
	for bad, want := range map[string]string{
		"claude -p {{prompt}} --max-budget-usd {{budget_usd}}": "claude.run: {{budget_usd}} is not a placeholder",
		"claude -p 'unterminated":                              "claude.run: cannot be split",
	} {
		commands := map[string]any{"claude": map[string]any{"run": bad}}
		if msg := checkSettingTypeProblem("", setting, commands); !strings.Contains(msg, want) {
			t.Errorf("%q: got %q, want %q", bad, msg, want)
		}
	}
}
