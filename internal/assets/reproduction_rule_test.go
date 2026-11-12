package assets

import (
	"fmt"
	"strings"
	"testing"
)

type assetRuleRequirement struct {
	file    string
	section string
	tokens  []string
}

var reproductionRuleRequirements = []assetRuleRequirement{
	{
		file:    "skills/foreman-kit-qa/SKILL.md",
		section: "Rules:disposable-instance",
		tokens: []string{
			"disposable instance",
			"FOREMANKIT_STATE_DIR",
			"FOREMANKIT_DB_PATH",
			"foreman-kit portal --port",
			"foreman-kit sim",
			"never against the application's own store, portal or remotes",
			"Read-only probes",
			"real instance",
		},
	},
	{
		file:    "skills/foreman-kit-qa/SKILL.md",
		section: "Rules:reproduce-twice",
		tokens: []string{
			"Reproduce a defect twice",
			"disposable-instance rule",
		},
	},
	{
		file:    "skills/foreman-kit-qa/SKILL.md",
		section: "Verify:reproduce-when",
		tokens: []string{
			"reproduce the WHEN",
			"disposable-instance rule",
		},
	},
	{
		file:    "seeds/.foreman-kit/prompts/cycle.md",
		section: "Scope",
		tokens: []string{
			"disposable instance",
			"FOREMANKIT_STATE_DIR",
			"FOREMANKIT_DB_PATH",
			"foreman-kit portal --port",
			"foreman-kit sim",
		},
	},
	{
		file:    "skills/foreman-kit-cycle/references/report.template.md",
		section: "Scratch instances",
		tokens: []string{
			"## Scratch instances",
			"directory",
			"state directory",
			"database",
			"port",
			`"none"`,
		},
	},
}

func extractRuleSection(file, section, text string) (string, error) {
	switch section {
	case "Rules:disposable-instance":
		rules, ok := markdownSection(text, "## Rules")
		if !ok {
			return "", fmt.Errorf("%s: missing ## Rules heading", file)
		}
		for _, line := range strings.Split(rules, "\n") {
			if strings.Contains(line, "Disposable instance") {
				return line, nil
			}
		}
		return "", fmt.Errorf("%s: missing disposable-instance rule line under ## Rules", file)
	case "Rules:reproduce-twice":
		rules, ok := markdownSection(text, "## Rules")
		if !ok {
			return "", fmt.Errorf("%s: missing ## Rules heading", file)
		}
		for _, line := range strings.Split(rules, "\n") {
			if strings.Contains(line, "Reproduce a defect twice") {
				return line, nil
			}
		}
		return "", fmt.Errorf("%s: missing 'Reproduce a defect twice' rule under ## Rules", file)
	case "Verify:reproduce-when":
		verify, ok := markdownSection(text, "## Verify integrated changes")
		if !ok {
			return "", fmt.Errorf("%s: missing ## Verify integrated changes heading", file)
		}
		for _, line := range strings.Split(verify, "\n") {
			if strings.Contains(line, "reproduce the WHEN") {
				return line, nil
			}
		}
		return "", fmt.Errorf("%s: missing 'reproduce the WHEN' step under ## Verify integrated changes", file)
	case "Scope":
		idx := strings.Index(text, "Scope, checked after you exit:")
		if idx < 0 {
			return "", fmt.Errorf("%s: missing Scope block", file)
		}
		return text[idx:], nil
	case "Scratch instances":
		scratchIdx := strings.Index(text, "## Scratch instances")
		if scratchIdx < 0 {
			return "", fmt.Errorf("%s: missing ## Scratch instances heading", file)
		}
		cmdIdx := strings.Index(text, "## Commands executed")
		if cmdIdx < 0 || scratchIdx > cmdIdx {
			return "", fmt.Errorf("%s: ## Scratch instances must appear before ## Commands executed", file)
		}
		return text[scratchIdx:cmdIdx], nil
	default:
		return "", fmt.Errorf("unknown section %q", section)
	}
}

func markdownSection(text, heading string) (string, bool) {
	idx := strings.Index(text, heading+"\n")
	if idx < 0 {
		return "", false
	}
	rest := text[idx+len(heading)+1:]
	if next := strings.Index(rest, "\n## "); next >= 0 {
		return rest[:next], true
	}
	return rest, true
}

func checkReproductionRule(files map[string]string) error {
	for _, req := range reproductionRuleRequirements {
		text, ok := files[req.file]
		if !ok {
			return fmt.Errorf("missing file %s", req.file)
		}
		block, err := extractRuleSection(req.file, req.section, text)
		if err != nil {
			return err
		}
		for _, tok := range req.tokens {
			if !strings.Contains(block, tok) {
				return fmt.Errorf("%s (%s): missing %q", req.file, req.section, tok)
			}
		}
	}
	return nil
}

func TestCycleScratchReproductionRuleInShippedAssets(t *testing.T) {
	files := make(map[string]string)
	for _, req := range reproductionRuleRequirements {
		if _, seen := files[req.file]; seen {
			continue
		}
		raw, err := FS.ReadFile(req.file)
		if err != nil {
			t.Fatalf("read %s: %v", req.file, err)
		}
		files[req.file] = string(raw)
	}

	if err := checkReproductionRule(files); err != nil {
		t.Fatal(err)
	}

	// Verify the check fails when any required token is removed.
	for _, req := range reproductionRuleRequirements {
		for _, tok := range req.tokens {
			mutated := make(map[string]string, len(files))
			for k, v := range files {
				mutated[k] = v
			}
			mutated[req.file] = strings.ReplaceAll(files[req.file], tok, "")
			if err := checkReproductionRule(mutated); err == nil {
				t.Errorf("%s (%s): removing %q did not fail checkReproductionRule", req.file, req.section, tok)
			}
		}
	}
}
