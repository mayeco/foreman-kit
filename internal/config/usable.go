package config

import (
	"fmt"
	"path/filepath"

	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/settings"
)

// checkConfigFile returns what `foreman-kit config check` reports about the settings file called
// section of the application at root: the same errors and warnings whoever asks. evaluatorData is
// the content of the evaluator's file, which the check of the cycle's file consults.
func checkConfigFile(root, section string, evaluatorData map[string]any) FileProblems {
	rep := FileProblems{Errors: []string{}, Warnings: []string{}}
	data, present, err := loadConfigYAML(root, section)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return rep
	}
	if !present {
		missing := "missing (foreman-kit update writes it)"
		if section == settings.EvaluatorFile && fileExists(filepath.Join(root, filepath.FromSlash(evaluator.LegacyPolicyFile))) {
			// An application installed before the file was renamed: its gates are still there.
			missing = fmt.Sprintf("missing: %s has not been renamed to it (foreman-kit update)", evaluator.LegacyPolicyFile)
		}
		rep.Errors = append(rep.Errors, missing)
		return rep
	}
	checkSectionSettings(root, section, data, &rep)
	switch section {
	case "agents":
		checkAgentsSection(root, data, &rep)
	case "cycle":
		checkCycleSection(root, data, &rep, evaluatorData)
	case "daemon":
		checkDaemonSection(root, data, &rep)
	case "visual_qa":
		checkVisualQASection(root, data, &rep)
	case "exploration":
		checkExplorationSection(root, data, &rep)
	case settings.EvaluatorFile:
		checkEvaluatorSection(root, data, &rep)
	}
	return rep
}

// Unusable says why a process of the application at root that reads the files called names starts
// no new work, or "" when it can. It is settings.Unusable (a file missing or not valid YAML, a key
// the catalog lacks, does not accept or does not know, a variable an older foreman-kit read) and,
// when the catalog finds nothing, every error `foreman-kit config check` reports about those files
// and the catalog cannot see: a role that names an agent with no commands, a command with a
// placeholder that does not exist, a slot a worker takes, an abstain band out of order, a strategy
// that cannot run. The answer names the first and says how many more there are. Warnings never keep
// a process from working.
//
// The files are judged as written, whatever `--set` gave the run, like the catalog's problems.
func Unusable(root string, names ...string) string {
	if why := settings.Unusable(root, names...); why != "" {
		return why
	}
	var errs []string
	var evaluatorData map[string]any
	for _, name := range names {
		if _, ok := settings.FileNamed(name); !ok {
			continue
		}
		if evaluatorData == nil && name == "cycle" {
			evaluatorData, _, _ = loadConfigYAML(root, settings.EvaluatorFile)
		}
		for _, e := range checkConfigFile(root, name, evaluatorData).Errors {
			errs = append(errs, configFile(name)+": "+e)
		}
	}
	if len(errs) == 0 {
		return ""
	}
	why := errs[0]
	switch more := len(errs) - 1; {
	case more == 1:
		why += " (and 1 more: foreman-kit config check lists them)"
	case more > 1:
		why += fmt.Sprintf(" (and %d more: foreman-kit config check lists them)", more)
	default:
		why += " (foreman-kit config check)"
	}
	return why
}
