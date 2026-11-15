package assets

import (
	"strings"
	"testing"
)

// gatesGuidance lists, per shipped file, what it must tell the agents about running the gates: the
// gates without the regression sweep, in the foreground, and never a command left in the background.
var gatesGuidance = []struct {
	file   string
	tokens []string
}{
	{"seeds/.foreman-kit/prompts/build.md", []string{
		"`foreman-kit gates --no-regression`", "in the foreground",
		"Never run the regression sweep yourself", "the dispatcher runs the gates and the regression after you\n  finish",
		"Never send a command to the background and never wait for one", "stopped as idle", "under 9 minutes",
	}},
	{"seeds/.foreman-kit/prompts/plan.md", []string{
		"`foreman-kit gates --no-regression` in the foreground",
		"Never write a task that\nasks for the regression sweep, the whole suite, enforced coverage or a background run",
	}},
	{"seeds/.foreman-kit/prompts/cycle.md", []string{
		"Run every command in the foreground and keep it under 9 minutes", "never leave one in the background and wait for it", "stopped as idle",
	}},
	{"templates/authoring_rules.md", []string{
		"`foreman-kit gates --no-regression` in the foreground",
		"Never write a task that asks for the regression sweep, the whole suite, enforced coverage or a background run",
		"Never send a command to the background or wait for one",
	}},
	{"skills/foreman-kit-qa/SKILL.md", []string{
		"`foreman-kit gates --repo <repo> --no-regression`", "it cannot be combined with `--regression`",
	}},
}

// TestShippedAgentGuidanceRunsTheGatesWithoutTheRegression: the seed prompts, the authoring rules and
// the QA skill tell agents to run `foreman-kit gates --no-regression` in the foreground and never to
// wait on a background command; the build prompt no longer says only `foreman-kit gates`.
func TestShippedAgentGuidanceRunsTheGatesWithoutTheRegression(t *testing.T) {
	for _, g := range gatesGuidance {
		raw, err := FS.ReadFile(g.file)
		if err != nil {
			t.Fatalf("read %s: %v", g.file, err)
		}
		text := string(raw)
		for _, tok := range g.tokens {
			if !strings.Contains(text, tok) {
				t.Errorf("%s does not say %q", g.file, tok)
			}
		}
	}
}

// TestSeedPromptsKeepTheirPlaceholders: the guidance added no placeholder and dropped none.
func TestSeedPromptsKeepTheirPlaceholders(t *testing.T) {
	want := map[string][]string{
		"build": {"{{change}}", "{{branch}}", "{{memory}}", "{{evaluator_hint}}", "{{authoring_rules}}", "{{feedback}}", "{{rebuild}}"},
		"plan":  {"{{change}}", "{{goal}}", "{{memory}}", "{{evaluator_hint}}", "{{authoring_rules}}", "{{feedback}}"},
		"cycle": {},
	}
	for name, placeholders := range want {
		raw, err := FS.ReadFile("seeds/.foreman-kit/prompts/" + name + ".md")
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		if end := strings.Index(body, "-->"); end >= 0 {
			body = body[end+3:]
		}
		for _, p := range placeholders {
			if !strings.Contains(body, p) {
				t.Errorf("%s.md lost %s", name, p)
			}
		}
		if got := strings.Count(body, "{{"); got != countAll(body, placeholders) {
			t.Errorf("%s.md holds %d placeholders, want only %v", name, got, placeholders)
		}
	}
}

func countAll(body string, placeholders []string) int {
	n := 0
	for _, p := range placeholders {
		n += strings.Count(body, p)
	}
	return n
}
