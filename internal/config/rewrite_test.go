package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/settings"
)

var claudeRoles = map[string][]string{"plan": {"claude"}, "build": {"claude"}, "cycle": {"claude"}}

// The seed of .foreman-kit/agents.yaml is what Write writes for an application that uses claude: its
// commands come from the one table that holds them, so the seed cannot drift from it.
func TestTheAgentsSeedIsWhatWriteWritesForClaude(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, []string{"claude"}, claudeRoles, ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, ".foreman-kit", "agents.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := assets.ReadSeed(AgentsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(seed) {
		t.Fatalf(".foreman-kit/agents.yaml as Write writes it for claude is not the seed:\n%s", got)
	}
	// Written again over itself, it does not change.
	if err := Write(root, []string{"claude"}, claudeRoles, ""); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(root, ".foreman-kit", "agents.yaml")); string(again) != string(seed) {
		t.Fatal("Write changed a file that already held what it writes")
	}

	var data map[string]any
	if err := yaml.Unmarshal(seed, &data); err != nil {
		t.Fatal(err)
	}
	spec := agentsConfigOf(data).Commands["claude"]
	shipped := SUPPORTED["claude"]
	if spec.Run != shipped.Run || spec.Restricted != shipped.Restricted || spec.Ping != shipped.Ping {
		t.Fatalf("the seed does not hold the commands the kit ships for claude:\nrun: %v\nrestricted: %v\nping: %v", spec.Run, spec.Restricted, spec.Ping)
	}
	// What the binary used to add on its own is written there: the cap, the tool lists.
	for _, want := range []string{"--max-budget-usd 5", `--disallowedTools "Bash(git commit *)`, "{{args}}"} {
		if !strings.Contains(shipped.Run, want) || !strings.Contains(shipped.Restricted, want) {
			t.Errorf("claude's commands do not write %s", want)
		}
	}
	if !strings.Contains(shipped.Restricted, `--allowedTools "Read Edit Write`) || strings.Contains(shipped.Restricted, "bin/run_gates.sh") {
		t.Errorf("claude's restricted command: %s", shipped.Restricted)
	}
}

// Write changes the agents and the roles, and keeps everything else the file holds: the limits, the
// arguments of each role and the commands a person wrote.
func TestWriteKeepsWhatTheFileHoldsAndShipsTheCommandsOfANewAgent(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, []string{"claude"}, claudeRoles, ""); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".foreman-kit", "agents.yaml")
	raw, _ := os.ReadFile(path)
	edited := strings.Replace(string(raw), "  retries: 1\n", "  retries: 3\n", 1)
	edited = strings.Replace(edited, "    timeout: 2h\n    args: {}\n", "    timeout: 90m\n    args: {claude: \"--model claude-sonnet-5-5\"}\n", 1)
	edited = strings.Replace(edited, "commands:\n", "commands:\n  mybot:\n    run: mybot --go {{prompt_file}}\n", 1)
	if edited == string(raw) || strings.Count(edited, "mybot") != 2 {
		t.Fatal("the file to edit does not read as expected")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	roles := map[string][]string{"plan": {"claude"}, "build": {"codex", "claude"}, "cycle": {"crush"}}
	if err := Write(root, []string{"claude", "codex", "crush"}, roles, settings.SelectionLeastBusy); err != nil {
		t.Fatal(err)
	}
	cfg := Load(root)
	if got := strings.Join(cfg.Agents, ","); got != "claude,codex,crush" {
		t.Errorf("agents = %s", got)
	}
	if got := strings.Join(cfg.Roles["build"], ","); got != "codex,claude" || cfg.Selection != settings.SelectionLeastBusy {
		t.Errorf("build = %s, selection = %s", got, cfg.Selection)
	}
	r := &settings.Reader{Root: root, FilesAlone: true}
	if r.Int(settings.AgentRetries) != 3 || r.Text(settings.RoleTimeout("build")) != "1h30m" {
		t.Errorf("the limits were not kept: retries %d, build timeout %s", r.Int(settings.AgentRetries), r.Text(settings.RoleTimeout("build")))
	}
	if got := ExtraArgsForRole(root, "claude", "build", nil); strings.Join(got, " ") != "--model claude-sonnet-5-5" {
		t.Errorf("the arguments of the role were not kept: %q", got)
	}
	for name, want := range map[string]string{"codex": SUPPORTED["codex"].Run, "crush": SUPPORTED["crush"].Run, "mybot": "mybot --go {{prompt_file}}"} {
		if got := cfg.Commands[name].Run; got != want {
			t.Errorf("commands.%s.run = %v, want %s", name, got, want)
		}
	}
	if cfg.Commands["codex"].Restricted != SUPPORTED["codex"].Restricted || cfg.Commands["crush"].Restricted != nil {
		t.Errorf("restricted: codex %v, crush %v", cfg.Commands["codex"].Restricted, cfg.Commands["crush"].Restricted)
	}
	if closes := cfg.Commands["crush"].StdinNull; closes == nil || !*closes {
		t.Error("crush needs its standard input closed, and the file does not say so")
	}
	written, _ := os.ReadFile(path)
	if !strings.Contains(string(written), "# Each command runs exactly as written") || strings.Index(string(written), "  claude:") > strings.Index(string(written), "  codex:") {
		t.Errorf("the comments or the order of the agents were lost:\n%s", written)
	}

	// Every agent the kit knows keeps its commands, used or not, so one added later runs as written;
	// a binary of its own keeps them too.
	if err := Write(root, []string{"codex"}, map[string][]string{"plan": {"codex"}, "build": {"codex"}, "cycle": {"codex"}}, ""); err != nil {
		t.Fatal(err)
	}
	cfg = Load(root)
	if _, kept := cfg.Commands["claude"]; !kept {
		t.Error("the commands of claude were not kept although the kit knows it")
	}
	if _, kept := cfg.Commands["mybot"]; !kept || cfg.Selection != settings.SelectionLeastBusy {
		t.Errorf("mybot kept: %v, selection %s", kept, cfg.Selection)
	}
	if err := Write(root, []string{"codex"}, nil, "sideways"); err == nil {
		t.Error("a selection the kit does not know was written")
	}
}

// Whatever a command holds, the file reads it back as it was written.
func TestACommandReadsBackAsWritten(t *testing.T) {
	commands := []string{
		"claude -p {{prompt}}",
		"a  b   c",
		"tool --flag: value # not a comment",
		"- starts with a dash",
		`quoted "inner words" and 'single ones'`,
		"tool {{args}} -- {{prompt}}",
		strings.Repeat("word ", 60) + "end",
		"tab\tinside",
		"line\nbreak",
	}
	for _, name := range SupportedOrder {
		commands = append(commands, SUPPORTED[name].Run, SUPPORTED[name].Ping)
		if SUPPORTED[name].Restricted != "" {
			commands = append(commands, SUPPORTED[name].Restricted)
		}
	}
	for _, command := range commands {
		text := "commands:\n  agent:\n" + renderCommand("    ", "run", command)
		var data map[string]any
		if err := yaml.Unmarshal([]byte(text), &data); err != nil {
			t.Errorf("%q is written as invalid YAML: %v\n%s", command, err, text)
			continue
		}
		if got := digMap(data, "commands", "agent", "run"); got != command {
			t.Errorf("%q reads back as %q:\n%s", command, got, text)
		}
		for _, line := range strings.Split(text, "\n") {
			if len(line) > commandWidth+2 && strings.Contains(command, " ") && !strings.Contains(line, `\`) && len(command) > commandWidth && !strings.Contains(command, "  ") && !strings.ContainsAny(command, "\t\n") {
				t.Errorf("%q is written on a line of %d columns", command, len(line))
			}
		}
	}
	// A list of arguments stays a list.
	text := "commands:\n  agent:\n" + renderCommand("    ", "run", []any{"tool", "--say", "two words"})
	var data map[string]any
	if err := yaml.Unmarshal([]byte(text), &data); err != nil {
		t.Fatal(err)
	}
	if words, _ := CommandWords(digMap(data, "commands", "agent", "run")); strings.Join(words, "|") != "tool|--say|two words" {
		t.Errorf("a list of arguments reads back as %q", words)
	}
}

func TestTopLevelBlocks(t *testing.T) {
	text := "# head\nfirst: 1\n\n# about second\nsecond:\n  a: >-\n    folded\n    text\n  # inside\n  b: 2\n\n# about third\nthird: [x]\n"
	block, ok := TopLevelBlock([]byte(text), "second")
	if !ok || block != "second:\n  a: >-\n    folded\n    text\n  # inside\n  b: 2\n" {
		t.Fatalf("the block of second: %q, %v", block, ok)
	}
	if _, ok := TopLevelBlock([]byte(text), "fourth"); ok {
		t.Error("a key the text does not write has a block")
	}
	got, err := ReplaceTopLevelBlock([]byte(text), "second", "second: {}\n")
	if err != nil || string(got) != "# head\nfirst: 1\n\n# about second\nsecond: {}\n\n# about third\nthird: [x]\n" {
		t.Fatalf("replaced: %q, %v", got, err)
	}
	got, err = ReplaceTopLevelBlock([]byte(text), "third", "third:\n  - y\n  - z")
	if err != nil || !strings.HasSuffix(string(got), "# about third\nthird:\n  - y\n  - z\n") {
		t.Fatalf("the last block replaced: %q, %v", got, err)
	}
	got, err = ReplaceTopLevelBlock([]byte("first: 1"), "second", "second: 2\n")
	if err != nil || string(got) != "first: 1\nsecond: 2\n" {
		t.Fatalf("a block the text lacks: %q, %v", got, err)
	}
	if _, err := ReplaceTopLevelBlock([]byte(text), "second", "other: 1\n"); err == nil {
		t.Error("a block of another key was written under second")
	}
}
