package config

import (
	"sort"
)

// RepoCommandProblems says, in the words `foreman-kit config check` prints, where a command of a
// repo.config.yaml (data, the file as read) is not text: setup, a gate (gates.<name>) or a
// regression command (regression.commands.<name>) written as a list or a section, such as
// `test: [go, test, ./...]`. It is the one reading of that rule: the check, `foreman-kit gates` and
// the dispatcher's and the merger's gate runner all use it, so none of them takes as a command what
// another refuses. An empty value (null) is no command and no problem.
func RepoCommandProblems(data map[string]any) []string {
	var problems []string
	if setup := data["setup"]; setup != nil {
		if _, isStr := setup.(string); !isStr {
			problems = append(problems, "setup: must be a command (text)")
		}
	}
	if gates, present := data["gates"]; present && gates != nil {
		if gMap, ok := gates.(map[string]any); ok {
			problems = append(problems, nonTextEntries("gates", gMap)...)
		} else {
			problems = append(problems, `gates: must map a name to its command, e.g. test: "go test ./..."`)
		}
	}
	if regression, ok := data["regression"].(map[string]any); ok {
		if commands := regression["commands"]; commands != nil {
			if cMap, ok := commands.(map[string]any); ok {
				problems = append(problems, nonTextEntries("regression.commands", cMap)...)
			} else {
				problems = append(problems, `regression.commands: must map a name to its command, e.g. test: "bundle exec rspec"`)
			}
		}
	}
	return problems
}

// nonTextEntries names, sorted, each entry of section (whose key path is path) that holds a list or a
// section where a command belongs.
func nonTextEntries(path string, section map[string]any) []string {
	names := make([]string, 0, len(section))
	for name := range section {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems []string
	for _, name := range names {
		if kind := nonScalarKind(section[name]); kind != "" {
			problems = append(problems, path+"."+name+": must be a command (text), not a "+kind+`: write it as one string, e.g. "go test ./..."`)
		}
	}
	return problems
}

// nonScalarKind returns "list" or "section" when v is one, and "" when v is a scalar or empty.
func nonScalarKind(v any) string {
	switch v.(type) {
	case []any:
		return "list"
	case map[string]any, map[any]any:
		return "section"
	}
	return ""
}
