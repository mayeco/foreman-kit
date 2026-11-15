package config

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

var allowedToolsRe = regexp.MustCompile(`--allowedTools "([^"]*)"`)

// restrictedAllowedTools returns the entries of the --allowedTools of a restricted claude command.
func restrictedAllowedTools(t *testing.T, command string) []string {
	t.Helper()
	found := allowedToolsRe.FindStringSubmatch(command)
	if found == nil {
		t.Fatalf("the restricted claude command has no --allowedTools: %s", command)
	}
	var entries []string
	rest := found[1]
	for rest != "" {
		// An entry is a bare tool name or Tool(pattern), the pattern holding spaces.
		end := strings.IndexAny(rest, " (")
		if end < 0 {
			entries, rest = append(entries, rest), ""
			continue
		}
		if rest[end] == ' ' {
			entries, rest = append(entries, rest[:end]), rest[end+1:]
			continue
		}
		close := strings.Index(rest, ")")
		if close < 0 {
			t.Fatalf("an unbalanced tool entry in --allowedTools: %q", rest)
		}
		entries, rest = append(entries, rest[:close+1]), strings.TrimPrefix(rest[close+1:], " ")
	}
	return entries
}

// subcommandsNotForTheJudgment are the words after `foreman-kit` in the shipped prompt and skills
// that name no command the agent runs: what the script or the daemon runs for it (cycle, -d,
// proposals review), a person's commands (approve, base accept, --set) and the database path the text describes.
// config is checked on its own: only `config check` is allowed.
var subcommandsNotForTheJudgment = map[string]bool{
	"cycle": true, "-d": true, "--set": true, "approve": true, "base": true, "proposals": true, "db": true, "config": true,
}

var foremanKitWordRe = regexp.MustCompile("foreman-kit (-{0,2}[a-z][a-z-]*)")

func embeddedText(t *testing.T, name string) string {
	t.Helper()
	data, err := assets.FS.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// section returns the text of text from the heading start up to the heading end.
func section(t *testing.T, text, start, end string) string {
	t.Helper()
	from := strings.Index(text, start)
	to := strings.Index(text, end)
	if from < 0 || to < from {
		t.Fatalf("the skill has no section from %q to %q", start, end)
	}
	return text[from:to]
}

// Every foreman-kit subcommand the shipped cycle prompt, the judgment procedure of the cycle skill
// and the QA skill tell the agent to run is allowed by the restricted claude command: without it a
// restricted judgment cannot evaluate a defect, nor reproduce in a scratch instance.
func TestRestrictedClaudeAllowsEverySubcommandThePromptAndSkillsRun(t *testing.T) {
	cycleSkill := embeddedText(t, "skills/foreman-kit-cycle/SKILL.md")
	texts := map[string]string{
		"the cycle prompt seed":      embeddedText(t, "seeds/.foreman-kit/prompts/cycle.md"),
		"the cycle skill's judgment": section(t, cycleSkill, "## Hard rules for the judgment agent", "## Operating the cycle"),
		"the QA skill":               embeddedText(t, "skills/foreman-kit-qa/SKILL.md"),
	}
	entries := map[string]bool{}
	for _, entry := range restrictedAllowedTools(t, SUPPORTED["claude"].Restricted) {
		entries[entry] = true
	}
	checked := map[string]bool{}
	for where, text := range texts {
		for _, found := range foremanKitWordRe.FindAllStringSubmatch(text, -1) {
			word := found[1]
			if subcommandsNotForTheJudgment[word] {
				continue
			}
			checked[word] = true
			if want := "Bash(foreman-kit " + word + " *)"; !entries[want] {
				t.Errorf("%s tells the agent to run `foreman-kit %s`, but the restricted claude command lacks %s", where, word, want)
			}
		}
	}
	// The ones the issue names must have been found: the test reads what it should.
	for _, word := range []string{"evaluate", "sim", "portal", "status", "progress", "exec", "gates"} {
		if !checked[word] {
			t.Errorf("the prompt and skills no longer name `foreman-kit %s`: did the test stop reading them?", word)
		}
	}
}

// config check is allowed (the cycle ends by validating with it); config set, which changes the
// settings, is not.
func TestRestrictedClaudeAllowsConfigCheckAndNotConfigSet(t *testing.T) {
	entries := map[string]bool{}
	for _, entry := range restrictedAllowedTools(t, SUPPORTED["claude"].Restricted) {
		entries[entry] = true
	}
	if !entries["Bash(foreman-kit config check *)"] {
		t.Error("the restricted claude command does not allow foreman-kit config check")
	}
	for _, entry := range []string{"Bash(foreman-kit config *)", "Bash(foreman-kit config set *)", "Bash(foreman-kit *)"} {
		if entries[entry] {
			t.Errorf("the restricted claude command allows %s", entry)
		}
	}
}

// The seed of .foreman-kit/agents.yaml holds the commands the binary ships for claude.
func TestAgentsSeedClaudeCommandsEqualTheBinarys(t *testing.T) {
	seed, err := assets.ReadSeed(AgentsFile)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := yaml.Unmarshal(seed, &data); err != nil {
		t.Fatal(err)
	}
	spec := agentsConfigOf(data).Commands["claude"]
	shipped := SUPPORTED["claude"]
	if spec.Run != shipped.Run || spec.Restricted != shipped.Restricted || spec.Ping != shipped.Ping {
		t.Fatalf("the seed's claude commands differ from the binary's:\nrun: %v\nrestricted: %v\nping: %v", spec.Run, spec.Restricted, spec.Ping)
	}
	for _, want := range []string{"Bash(foreman-kit evaluate *)", "Bash(foreman-kit sim *)", "Bash(foreman-kit portal *)", "Bash(foreman-kit config check *)"} {
		if !strings.Contains(spec.Restricted.(string), want) {
			t.Errorf("the seed's restricted claude command lacks %s", want)
		}
	}
}

// An application whose agents.yaml still holds the claude commands the previous kit shipped gets the
// extended restricted command from update; the run and ping commands stay as they were.
func TestUpdateExtendsTheRestrictedClaudeCommandOfAnEarlierKit(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, nil)
	shipped := SUPPORTED["claude"]
	earlier := strings.Replace(shipped.Restricted, claudeAllowedTools, formerClaudeAllowedTools, 1)
	if earlier == shipped.Restricted || strings.Contains(earlier, "foreman-kit evaluate") {
		t.Fatal("the test's earlier command does not differ from the shipped one")
	}
	editAgentsCommands(t, root, nil, map[string]map[string]any{
		"claude": {"run": shipped.Run, "restricted": earlier, "ping": shipped.Ping},
	})
	if !ConfigNeedsRewrite(root) {
		t.Fatal("update does not rewrite a file that holds the previous claude commands")
	}
	m := applyMigration(t, root)
	if notes := strings.Join(m.Notes, "\n"); !strings.Contains(notes, "commands.claude: the commands an earlier foreman-kit shipped") {
		t.Errorf("the notes do not say the claude commands were replaced:\n%s", notes)
	}
	got := Load(root).Commands["claude"]
	if got.Restricted != shipped.Restricted || got.Run != shipped.Run || got.Ping != shipped.Ping {
		t.Errorf("claude = %+v, want the commands shipped now", got)
	}
	if ConfigNeedsRewrite(root) {
		t.Error("a second update still has something to rewrite")
	}
}

// A restricted claude command a person edited is kept by update, even though the shipped one changed.
func TestUpdateKeepsAnEditedRestrictedClaudeCommand(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, nil)
	shipped := SUPPORTED["claude"]
	edited := strings.Replace(strings.Replace(shipped.Restricted, claudeAllowedTools, formerClaudeAllowedTools, 1), "Bash(jev validate *)", "Bash(jev validate *) Bash(make *)", 1)
	editAgentsCommands(t, root, nil, map[string]map[string]any{
		"claude": {"run": shipped.Run, "restricted": edited, "ping": shipped.Ping},
	})
	if ConfigNeedsRewrite(root) {
		applyMigration(t, root)
	}
	if got := Load(root).Commands["claude"].Restricted; got != edited {
		t.Fatalf("claude's restricted command = %v, want the one the person wrote", got)
	}
}
