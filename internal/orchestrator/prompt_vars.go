package orchestrator

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/settings"
)

// PlanGoal returns the {{goal}} of the PLAN prompt of a change: its description, else its why, else its
// name, each sanitized like any text of the backlog. The dispatcher and the portal's rendering of "the
// prompt a worker would get now" both ask here, so the two cannot drift.
func PlanGoal(change, description, why string) string {
	goal := sanitizeBacklogText(description)
	if goal == "" {
		goal = sanitizeBacklogText(why)
	}
	if goal == "" {
		goal = change
	}
	return goal
}

// ChangeCapabilities returns the capabilities a change specifies in the worktree wt: the directories
// of openspec/changes/<change>/specs that hold a spec.md, sorted.
func ChangeCapabilities(wt, change string) []string {
	specsDir := filepath.Join(wt, "openspec", "changes", change, "specs")
	var caps []string
	for _, f := range listSpecFiles(specsDir) {
		if rel, err := filepath.Rel(specsDir, filepath.Dir(f)); err == nil {
			caps = append(caps, filepath.ToSlash(rel))
		}
	}
	sort.Strings(caps)
	return caps
}

// EvaluatorHint returns the {{evaluator_hint}} block of the PLAN and BUILD prompts: the paragraph that
// tells an agent how to ask the evaluator, empty when this run turned the evaluator off or the judge is
// not available (a nil judge included).
func EvaluatorHint(judge *evaluator.Judge) string {
	if settings.EvaluatorOffForThisRun() || judge == nil || !judge.Available() {
		return ""
	}
	return fmt.Sprintf(
		"\nWhen a yes/no judgment cannot be settled by running code (for example: does this test assert the THEN "+
			"clause of that scenario?), ask the evaluator, a calibrated judge that answers with a probability instead of text:\n"+
			"`jev noul \"<question>\" --true \"<what yes means>\" --false \"<what no means>\" --state-file <file> --model %s --field noul`\n"+
			"0.7 or more means yes, 0.3 or less means no, anything in between means it cannot tell. Never pass "+
			"secrets, .env files or credentials as state: the state is sent to the TypeSafe API.\n",
		judge.Policy().EffectiveModel(),
	)
}

// WorkerPromptVars returns the variables of the PLAN (role "plan") or BUILD (role "build") prompt of the
// backlog item that do not depend on a run: the change, its goal (PLAN only), the memory block of the
// worktree wt with the capabilities the change specifies there, the evaluator hint of judge and the
// authoring rules. The caller adds what only its run knows (feedback, rebuild, branch). The dispatcher
// starting a worker and the portal showing the prompt a worker would get now build their variables here.
func WorkerPromptVars(root, wt, role string, item BacklogItem, judge *evaluator.Judge) map[string]string {
	change := item.ChangeName
	_, memBlock := MemoryFingerprintAndPromptBlock(wt, ChangeCapabilities(wt, change))
	rules, _ := assets.ReadTemplate(root, "authoring_rules.md")
	vars := map[string]string{
		"change":          change,
		"memory":          memBlock,
		"evaluator_hint":  EvaluatorHint(judge),
		"authoring_rules": string(rules),
	}
	if role == "plan" {
		vars["goal"] = PlanGoal(change, item.Description, item.Why)
	}
	return vars
}
