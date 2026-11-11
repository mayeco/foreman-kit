package portal

import "strings"

// The words of the portal for the values the database records. Every page says a state through
// wordingOf, so a state reads the same wherever it shows and its color means the same thing. A value
// the schema allows and this file does not name fails a test: a new state gets its words here before
// it reaches a page.

// wording is how a recorded value reads on a page.
type wording struct {
	// Text is the value in the words of the operator.
	Text string `json:"text"`
	// Tone is the color of its badge: "success", "warning", "error", "info", or "" for neutral.
	Tone string `json:"tone,omitempty"`
	// Means says in a sentence what the value means and what follows it, "" when the word is enough.
	Means string `json:"means,omitempty"`
}

// The vocabularies, named after the column whose values they translate. changes.state is the state
// of a change as v_change_state computes it.
const (
	vocChangeState      = "changes.state"
	vocBacklogStatus    = "changes.backlog_status"
	vocExecutionKind    = "executions.kind"
	vocExecutionStatus  = "executions.status"
	vocAttemptOutcome   = "execution_attempts.outcome"
	vocIdleEndedBy      = "attempt_idle_periods.ended_by"
	vocRunResult        = "change_runs.result"
	vocRunOrigin        = "change_runs.origin"
	vocGateContext      = "gate_rounds.context"
	vocGateResult       = "gate_rounds.result"
	vocEvaluationStatus = "evaluations.status"
	vocEvaluationMode   = "evaluations.mode"
	vocBlockResolution  = "change_blocks.resolution"
	vocManifestOutcome  = "ready_manifests.outcome"
	vocMergeState       = "merge_attempts.state"
	vocMergeResult      = "merge_attempts.result"
	vocBaseOrigin       = "base_verifications.origin"
	vocBaseStatus       = "base_verifications.status"
	vocVisualStatus     = "visual_qa_items.status"
	vocEngineHealth     = "engines.health_status"
	vocPausedBy         = "workspace_pauses.paused_by"
	vocDispatchMode     = "dispatcher_runs.mode"
	vocCycleTrigger     = "cycles.trigger"
	vocCycleWaits       = "cycle_schedule.waits"
	vocProposalStatus   = "proposals.status"
)

var vocabularies = map[string]map[string]wording{
	vocChangeState: {
		"queued":            {"queued", "", "In the backlog with nothing open: the dispatcher picks it up when its turn comes."},
		"building":          {"building", "info", "A worker is planning or building it now."},
		"ready":             {"ready to merge", "success", "Built, and every gate passed: it waits for the merger."},
		"merging":           {"merging", "info", "The merger is integrating it now."},
		"blocked":           {"blocked", "error", "Stopped until a person decides: nothing runs for it."},
		"rebuild_requested": {"rebuild requested", "warning", "The merger sent it back: the dispatcher builds it again on its next pass."},
		"integrated":        {"integrated", "success", "Merged: its commits are on the base branches."},
		"discarded":         {"discarded", "", "Taken out of the backlog: nothing runs for it."},
		"unknown":           {"not in the backlog", "warning", "The kit has records of it, and the backlog of the main checkout does not list it: it was taken out, or a cycle queued it and has not brought it there yet."},
	},
	vocBacklogStatus: {
		"unknown":    {"not in the backlog", "warning", ""},
		"queued":     {"queued", "", ""},
		"integrated": {"integrated", "success", ""},
		"discarded":  {"discarded", "", ""},
	},
	vocExecutionKind: {
		"worker":     {"worker", "", "A prompt of the worker of a change: PLAN, BUILD or a repair."},
		"cycle":      {"cycle", "", "The judge of a cycle."},
		"portal_run": {"agent run", "", "A prompt a person sent to an agent from the portal."},
		"portal_job": {"kit operation", "", "The dispatcher, the merger or the gates, started from the portal."},
	},
	vocExecutionStatus: {
		"running":   {"running", "info", ""},
		"succeeded": {"succeeded", "success", ""},
		"failed":    {"failed", "error", ""},
		"stopped":   {"stopped", "warning", ""},
		"timed_out": {"timed out", "error", ""},
	},
	vocAttemptOutcome: {
		"ok":               {"finished", "success", ""},
		"error":            {"exited with an error", "error", ""},
		"timeout":          {"ran out of time", "error", ""},
		"idle_stalled":     {"stopped by the idle watchdog", "error", "It showed no activity for the whole idle limit."},
		"quota_exceeded":   {"out of quota", "error", "The engine refused the work: its quota is used up."},
		"auth_failed":      {"not signed in", "error", "The engine refused the work: it is not authenticated."},
		"not_installed":    {"not installed", "error", "The command of the engine is not on PATH."},
		"context_overflow": {"prompt too long for its context", "error", ""},
		"too_large":        {"prompt too large", "error", ""},
		"cancelled":        {"cancelled", "warning", "It was stopped from outside before it finished."},
	},
	vocIdleEndedBy: {
		"activity": {"then it worked again", "", ""},
		"stalled":  {"stopped by the idle watchdog", "error", ""},
		"exit":     {"until it ended", "", ""},
	},
	vocRunResult: {
		"ready":    {"ready to merge", "success", "Built, and every gate passed."},
		"blocked":  {"blocked", "error", "It stopped for a person to decide."},
		"held":     {"held", "", "Another change holds a lock it needs: it runs again by itself."},
		"deferred": {"deferred", "", "Every engine of a role was unavailable: the dispatcher tries again later."},
		"paused":   {"paused the kit", "warning", "It hit a problem of the whole workspace, and the kit was paused."},
		"stopped":  {"stopped", "warning", "It was stopped before it finished: the dispatcher runs it again."},
		"lost":     {"lost its lease", "warning", "Another dispatcher took the change over: this run stopped and recorded nothing more."},
	},
	vocRunOrigin: {
		"backlog": {"from the backlog", "", ""},
		"retry":   {"retry after a block", "", ""},
		"rebuild": {"rebuild asked by the merger", "", ""},
	},
	vocGateContext: {
		"worker_verify": {"verification by the worker", "", ""},
		"merger":        {"gates of the merger", "", ""},
		"base_verify":   {"check of the base branch", "", ""},
		"cli":           {"run by hand", "", ""},
	},
	vocGateResult: {
		"passed":  {"passed", "success", ""},
		"failed":  {"failed", "error", ""},
		"stopped": {"stopped", "warning", ""},
	},
	vocEvaluationStatus: {
		"passed":      {"approved", "success", ""},
		"failed":      {"rejected", "error", ""},
		"abstained":   {"could not tell", "warning", ""},
		"skipped":     {"not asked", "", "The gate of the evaluator is off, or the evaluator is not configured."},
		"unavailable": {"unavailable", "warning", "The evaluator could not be reached."},
	},
	// The mode a gate of the evaluator was in when it was asked (settings.GateModes).
	vocEvaluationMode: {
		"enforce":  {"enforced", "", "The orchestrator acts on the answer."},
		"advisory": {"advisory", "", "The answer is recorded, and nothing acts on it."},
		"off":      {"off", "", "The gate is off: the evaluator is not asked."},
	},
	vocBlockResolution: {
		"auto_retry": {"retried by the dispatcher", "", ""},
		"cleared":    {"cleared", "", "The block was lifted and the counters of the change kept."},
		"unblocked":  {"unblocked by a person", "success", "The block was lifted and the counters of the change started over."},
		"approved":   {"risk approved by a person", "success", ""},
		"superseded": {"replaced by a later block", "", ""},
		"integrated": {"closed when the change merged", "success", ""},
		"abandoned":  {"abandoned", "", ""},
	},
	vocManifestOutcome: {
		"integrated":         {"integrated", "success", ""},
		"sent_back":          {"sent back to be rebuilt", "warning", ""},
		"blocked":            {"blocked", "error", ""},
		"removed":            {"removed", "", ""},
		"invalid":            {"invalid", "error", ""},
		"already_integrated": {"already integrated", "success", ""},
	},
	vocMergeState: {
		"CLAIMED":            {"claimed", "info", ""},
		"POLICY_CHECK":       {"checking the merge policy", "info", ""},
		"REBASING_CHILDREN":  {"rebasing the repositories", "info", ""},
		"CHECKING_WORK_LOSS": {"checking that no work is lost", "info", ""},
		"ASSEMBLING":         {"assembling the umbrella", "info", ""},
		"AUDITING_TASKS":     {"auditing the tasks", "info", ""},
		"VERIFYING_BASE":     {"verifying the base branch", "info", ""},
		"RUNNING_GATES":      {"running the gates", "info", ""},
		"ARCHIVING":          {"archiving the specs", "info", ""},
		"COMMITTING":         {"committing", "info", ""},
		"PUSHING_CHILDREN":   {"pushing the repositories", "info", ""},
		"PUSHING_ROOT":       {"pushing the umbrella", "info", ""},
		"COMPLETED":          {"completed", "success", ""},
	},
	vocMergeResult: {
		"integrated":         {"integrated", "success", ""},
		"sent_back":          {"sent back to be rebuilt", "warning", ""},
		"blocked":            {"blocked", "error", ""},
		"push_rejected":      {"push rejected", "error", "The merger tries again on its next pass."},
		"already_integrated": {"already integrated", "success", ""},
		"held":               {"held by a broken base", "warning", "The base branch fails on its own: the change waits for it."},
		"stopped":            {"stopped", "warning", "The merger was stopped before it finished: it tries again."},
	},
	vocBaseOrigin: {
		"merge":   {"checked while merging", "", ""},
		"recheck": {"checked again on its own", "", ""},
	},
	vocBaseStatus: {
		"passed":   {"passes", "success", ""},
		"failed":   {"fails on its own", "error", ""},
		"accepted": {"accepted with its failures", "warning", ""},
	},
	vocVisualStatus: {
		"pending":       {"waiting to be looked at", "warning", ""},
		"in_progress":   {"being looked at", "info", ""},
		"verified":      {"verified", "success", ""},
		"regression":    {"regression", "error", ""},
		"not-visual":    {"nothing to look at", "", ""},
		"cannot-verify": {"could not be verified", "warning", ""},
		"dropped":       {"dropped", "", ""},
	},
	vocEngineHealth: {
		"unknown":        {"not checked yet", "", ""},
		"ok":             {"healthy", "success", ""},
		"auth_failed":    {"not signed in", "error", ""},
		"quota_exceeded": {"out of quota", "error", ""},
		"config_error":   {"misconfigured", "error", ""},
		"not_installed":  {"not installed", "error", ""},
		"timeout":        {"did not answer in time", "warning", ""},
	},
	vocPausedBy: {
		"dispatcher": {"the dispatcher", "", ""},
		"merger":     {"the merger", "", ""},
		"portal":     {"a person, from the portal", "", ""},
		"cli":        {"a person, from the command line", "", ""},
	},
	vocDispatchMode: {
		"once":       {"one pass", "", ""},
		"continuous": {"continuous", "", ""},
	},
	vocCycleTrigger: {
		"startup":     {"when the daemon started", "", ""},
		"interval":    {"on its interval", "", ""},
		"out_of_work": {"ahead of its interval, because the pipeline ran out of work", "", ""},
		"manual":      {"started by a person", "", ""},
	},
	// The status of a proposal, as the proposals file gives it.
	vocProposalStatus: {
		"proposed":   {"waits for a decision", "warning", "A cycle proposed it, and nobody decided yet."},
		"approved":   {"approved", "info", "The next cycle queues it while the queue has room."},
		"queued":     {"queued", "", "It is in the backlog as a change."},
		"integrated": {"integrated", "success", "Its change was merged."},
		"rejected":   {"rejected", "", "It is not proposed again."},
		"unknown":    {"not in the main checkout", "", "A cycle wrote it in its own copy: a person decides it once the main checkout lists it."},
	},
	// What keeps the next cycle from starting before its interval, in the daemon's words
	// (store.CycleSchedule.Waits). Each one ends the sentence "it waits because ...".
	vocCycleWaits: {
		"delay":       {"the delay since the last cycle began has not passed", "", ""},
		"no_report":   {"the dispatcher has not looked at the backlog since the last cycle", "", ""},
		"busy":        {"the workers have work", "", ""},
		"queue_full":  {"as many changes are open as the queue takes", "warning", "The cycle queues nothing new until some of them are integrated: it runs on its interval."},
		"no_movement": {"nothing was built, blocked or integrated in the last two cycles", "", "It runs on its interval."},
		"no_cycle":    {"no cycle has run yet", "", ""},
	},
}

// wordingOf returns how value reads in a vocabulary. A value the vocabulary does not have reads as it
// is recorded, with underscores as spaces and no tone: it is shown, never hidden.
func wordingOf(vocabulary, value string) wording {
	if w, ok := vocabularies[vocabulary][value]; ok {
		return w
	}
	return wording{Text: strings.ReplaceAll(value, "_", " ")}
}
