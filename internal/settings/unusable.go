package settings

import "fmt"

// The files each process of the kit reads. A process starts no new work while one of its files
// cannot be used (Reader.Unusable): it says which key keeps it and waits for the file to be mended,
// instead of working with a value nobody wrote.
var (
	// DispatcherFiles are the files of the dispatcher: it plans and builds with the agents, and its
	// gates ask the evaluator.
	DispatcherFiles = []string{"pipeline", "agents", EvaluatorFile}
	// MergerFiles are the files of the merger: its gates ask the evaluator, and it calls no agent.
	MergerFiles = []string{"pipeline", EvaluatorFile}
	// CycleFiles are the files of the cycle: its own, the strategies, the agent that judges, the
	// evaluator that reviews what it proposes, whether the visual session takes what was integrated,
	// and the branch and the worktrees of the pipeline.
	CycleFiles = []string{"pipeline", "agents", "cycle", "visual_qa", "exploration", EvaluatorFile}
	// VisualSessionFiles are the files of the visual session: its own, the strategies, and the
	// branch and the worktrees of the pipeline.
	VisualSessionFiles = []string{"pipeline", "visual_qa", "exploration"}
	// DaemonFiles is what the daemon reads when it starts. What it runs (the dispatcher, the
	// merger, the cycle) reads its own files on every pass.
	DaemonFiles = []string{"daemon"}
	// AgentCallFiles is what one call to an agent reads.
	AgentCallFiles = []string{"agents"}
)

// Unusable says why a process that reads the files called names starts no new work, or "" when it
// can: a file is missing or is not valid YAML, a key of the catalog is missing or holds something
// it does not accept, or the file writes a key the catalog does not know. They are the errors of
// `foreman-kit config check` about those files; the answer names the first one and says how many
// more there are. A file an older foreman-kit wrote is named as that, with the command that
// rewrites it.
//
// So does a variable an older foreman-kit read to override one of those files, when it is set in
// the environment of the process: whoever exports AGENT_PERMISSIONS=restricted or JEV_DISABLED=1
// expects it to hold, and nothing reads it any more.
//
// A Reader reads each file once: a process that waits for a file to be mended asks a new Reader on
// every pass.
func (r *Reader) Unusable(names ...string) string {
	var problems []Problem
	for _, name := range names {
		problems = append(problems, r.Problems(name)...)
	}
	if !r.FilesAlone {
		problems = append(problems, formerVariablesOf(names...)...)
	}
	if len(problems) == 0 {
		return ""
	}
	for _, p := range problems {
		if p.Former {
			return fmt.Sprintf("%s holds keys an older foreman-kit read (%s): foreman-kit update rewrites it", p.File, p.Key.Path())
		}
	}
	why := problems[0].String()
	switch more := len(problems) - 1; {
	case more == 1:
		why += " (and 1 more: foreman-kit config check lists them)"
	case more > 1:
		why += fmt.Sprintf(" (and %d more: foreman-kit config check lists them)", more)
	default:
		why += " (foreman-kit config check)"
	}
	return why
}

// Unusable says why a process of the application at root that reads the files called names starts
// no new work, or "" when it can.
func Unusable(root string, names ...string) string {
	return At(root).Unusable(names...)
}
