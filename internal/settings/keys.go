package settings

// The keys of .foreman-kit/pipeline.yaml: how a change travels from the backlog to the base branch.
const (
	// BaseBranch is the branch every change starts from and the merger publishes to.
	BaseBranch Key = "pipeline.repository.base_branch"
	// Remote is the git remote the kit fetches from and pushes to.
	Remote Key = "pipeline.repository.remote"
	// WorktreesDir is where the worktree of each change lives, relative to the application root.
	WorktreesDir Key = "pipeline.repository.worktrees_dir"
	// ProtectedPaths lists the paths of a child repository a change may not touch.
	ProtectedPaths Key = "pipeline.repository.protected_paths"
	// Workers is how many changes are built at the same time.
	Workers Key = "pipeline.dispatch.workers"
	// PriorityBumpEvery is how long a backlog item waits before it is ordered one priority higher.
	PriorityBumpEvery Key = "pipeline.dispatch.priority_bump_every"
	// RepairRounds is how many times a phase is sent back to its agent after it fails.
	RepairRounds Key = "pipeline.build.repair_rounds"
	// GateTimeout is the limit of one repository's gates.
	GateTimeout Key = "pipeline.gates.timeout"
	// BlockedRetries is how many times a blocked change that can heal on its own runs again.
	BlockedRetries Key = "pipeline.blocked_changes.retries"
	// BlockedRetryAfter is how long a blocked change waits before each of those runs.
	BlockedRetryAfter Key = "pipeline.blocked_changes.retry_after"
	// OutsideCommits is what the merger does with commits pushed to a child repository outside the
	// pipeline.
	OutsideCommits Key = "pipeline.merge.outside_commits"
	// BaseRecheckAfter is how long after a base failed on its own it is checked again, once.
	BaseRecheckAfter Key = "pipeline.merge.base_recheck_after"
	// ConflictRebuilds is how many times a change that no longer rebases is rebuilt.
	ConflictRebuilds Key = "pipeline.merge.conflict_rebuilds"
	// LocksStaleAfter is how long a lock lives without being renewed.
	LocksStaleAfter Key = "pipeline.locks.stale_after"
	// DiagnosticsEnabled turns on the model that explains failures in words.
	DiagnosticsEnabled Key = "pipeline.diagnostics.enabled"
	// DiagnosticsProvider is who serves that model.
	DiagnosticsProvider Key = "pipeline.diagnostics.provider"
	// DiagnosticsModel is that model.
	DiagnosticsModel Key = "pipeline.diagnostics.model"
	// DiagnosticsBaseURL is the endpoint of that model, for the providers that need one.
	DiagnosticsBaseURL Key = "pipeline.diagnostics.base_url"
	// DiagnosticsTimeout is the limit of one request to that model.
	DiagnosticsTimeout Key = "pipeline.diagnostics.timeout"
	// DiagnosticsRetries is how many times an answer that does not fit is asked again.
	DiagnosticsRetries Key = "pipeline.diagnostics.retries"
)

// The keys of .foreman-kit/agents.yaml: which agent CLIs the application uses, who does what and the
// limits of one call.
const (
	// Agents lists the agent CLIs the application uses.
	Agents Key = "agents.agents"
	// AgentSelection is how each call picks among the agents of its role.
	AgentSelection Key = "agents.selection"
	// AgentPermissions says which command of each agent runs: its run or its restricted one.
	AgentPermissions Key = "agents.permissions"
	// IdleTimeout is how long an agent may show no work before it is stopped.
	IdleTimeout Key = "agents.limits.idle_timeout"
	// IdleCPUPercent is the share of one core from which an agent's CPU counts as work.
	IdleCPUPercent Key = "agents.limits.idle_cpu_percent"
	// AgentRetries is how many more times a failed call is tried with the same agent.
	AgentRetries Key = "agents.limits.retries"
	// AgentRetryAfter is the wait before each of those attempts.
	AgentRetryAfter Key = "agents.limits.retry_after"
	// PingTimeout is the limit of the preflight's ping of one agent.
	PingTimeout Key = "agents.limits.ping_timeout"
	// OutageMaxWait is the longest no worker starts after every agent of a role went out.
	OutageMaxWait Key = "agents.outage.max_wait"
	// OutageGiveUpAfter is how many outages a change waits through before it is blocked.
	OutageGiveUpAfter Key = "agents.outage.give_up_after"
	// AgentCommands maps each agent to the commands that run it.
	AgentCommands Key = "agents.commands"
)

// RoleAgents returns the key that lists the agents of role ("plan", "build" or "cycle").
func RoleAgents(role string) Key {
	return Key("agents.roles." + role + ".agents")
}

// RoleTimeout returns the key that limits one call of role.
func RoleTimeout(role string) Key {
	return Key("agents.roles." + role + ".timeout")
}

// RoleArgs returns the key that maps each agent to the arguments its command gets in role.
func RoleArgs(role string) Key {
	return Key("agents.roles." + role + ".args")
}

// The keys of .foreman-kit/cycle.yaml: what one cycle does.
const (
	// CycleBudget is what the cycle's judgment is told it may spend.
	CycleBudget Key = "cycle.budget.total"
	// CycleExploring is how much of that budget goes to the strategy of the cycle.
	CycleExploring Key = "cycle.budget.exploring"
	// QueueMaxFindings is how many findings one cycle queues or proposes.
	QueueMaxFindings Key = "cycle.queue.max_findings"
	// QueueMaxOpen is how many changes may be open before the cycle queues nothing new.
	QueueMaxOpen Key = "cycle.queue.max_open"
	// RoadmapFile is the file with the features to build.
	RoadmapFile Key = "cycle.roadmap.file"
	// RoadmapMaxProposals is how many roadmap features one cycle proposes.
	RoadmapMaxProposals Key = "cycle.roadmap.max_proposals"
	// RoadmapMaxOpen is how many roadmap proposals may be open before the cycle proposes no more.
	RoadmapMaxOpen Key = "cycle.roadmap.max_open"
	// ProposalsApprovedBy is who approves a proposal before it is queued.
	ProposalsApprovedBy Key = "cycle.proposals.approved_by"
	// PromptEditsApprovedBy is who approves an edit of .foreman-kit/prompts/ the cycle proposes.
	PromptEditsApprovedBy Key = "cycle.proposals.prompt_edits_approved_by"
	// MemoryMaxChars is how many characters each file of the memory read by every change may hold.
	MemoryMaxChars Key = "cycle.memory.max_chars"
	// MemoryMaxCharsPerScope is how many characters the notes of one repository or capability may hold.
	MemoryMaxCharsPerScope Key = "cycle.memory.max_chars_per_scope"
	// CycleSkills lists the application's own QA skills the cycle uses.
	CycleSkills Key = "cycle.skills"
)

// The keys of .foreman-kit/daemon.yaml: what the resident process does besides building and merging.
const (
	// CycleEvery is the longest the daemon waits between the start of two cycles.
	CycleEvery Key = "daemon.cycle.every"
	// CycleEveryWhenOutOfWork is how long after a cycle began the daemon starts the next one once
	// the pipeline has run out of work.
	CycleEveryWhenOutOfWork Key = "daemon.cycle.every_when_out_of_work"
	// DaemonPortal says the daemon serves the Web Portal.
	DaemonPortal Key = "daemon.portal.enabled"
	// DaemonBind is the address the portal listens on.
	DaemonBind Key = "daemon.portal.bind"
	// DaemonPort is the port the portal listens on.
	DaemonPort Key = "daemon.portal.port"
)

// The keys of .foreman-kit/visual_qa.yaml: the browser session.
const (
	// VisualQAEnabled says a browser session checks integrated changes.
	VisualQAEnabled Key = "visual_qa.enabled"
	// VisualQATimeBudget is what one visual session is told it may spend.
	VisualQATimeBudget Key = "visual_qa.time_budget"
	// VisualQAExpiresAfter is how long a session that never ended keeps its lock.
	VisualQAExpiresAfter Key = "visual_qa.expires_after"
	// VisualQAMaxChecks is how many queued checks one visual session takes.
	VisualQAMaxChecks Key = "visual_qa.max_checks"
	// VisualQAMaxFindings is how many findings one visual session queues or proposes.
	VisualQAMaxFindings Key = "visual_qa.max_findings"
	// VisualQASlot is the slot of the visual session's commands.
	VisualQASlot Key = "visual_qa.slot"
	// VisualQASkills lists the application's own QA skills the visual session uses.
	VisualQASkills Key = "visual_qa.skills"
)

// The keys of .foreman-kit/exploration.yaml: what the cycle and the visual session look for.
const (
	// ExplorationSelection is how each run picks its strategy.
	ExplorationSelection Key = "exploration.selection"
	// ExplorationStrategies lists the strategies.
	ExplorationStrategies Key = "exploration.strategies"
)

// The keys of .foreman-kit/evaluator.yaml: how the evaluator is called and what is done with its answers.
const (
	// EvaluatorEnabled turns every gate of the evaluator on or off at once.
	EvaluatorEnabled Key = "evaluator.enabled"
	// EvaluatorModel is the model that answers the gates.
	EvaluatorModel Key = "evaluator.model"
	// EvaluatorBaseURL is the endpoint of the evaluator, when it is not the provider's own.
	EvaluatorBaseURL Key = "evaluator.base_url"
	// EvaluatorTimeout is the limit of one attempt.
	EvaluatorTimeout Key = "evaluator.timeout"
	// EvaluatorRetries is how many times a question is asked again.
	EvaluatorRetries Key = "evaluator.retries"
	// EvaluatorWhenUnavailable is what a gate does when the model does not answer.
	EvaluatorWhenUnavailable Key = "evaluator.when_unavailable"
	// EvaluatorStateMaxChars is what every string sent to the model is clipped to.
	EvaluatorStateMaxChars Key = "evaluator.state.max_chars"
	// EvaluatorIncludeDiffs says the change_risk gate is sent code.
	EvaluatorIncludeDiffs Key = "evaluator.state.include_diffs"
	// EvaluatorAbstainBand is the range of scores read as "cannot tell".
	EvaluatorAbstainBand Key = "evaluator.abstain_band"
)

// GateMode returns the key that holds the mode of a gate of the evaluator ("plan_review").
func GateMode(gate string) Key {
	return Key(EvaluatorFile + ".gates." + gate + ".mode")
}
