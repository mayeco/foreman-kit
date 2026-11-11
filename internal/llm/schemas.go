package llm

import (
	"fmt"
	"strings"
)

// =============================================================================
// Use Case 1: Surgical Gate Failure Diagnosis Before Repair
// =============================================================================

type GateFailureDiagnosis struct {
	FailedGate       string   `json:"failed_gate"       jsonschema:"description=Name of the gate that failed (e.g. backend:test, web:lint)" validate:"required"`
	Category         string   `json:"category"          jsonschema:"enum=compile_error,enum=test_assertion,enum=lint_violation,enum=type_error,enum=missing_dependency,enum=environment_flaky" validate:"required,oneof=compile_error test_assertion lint_violation type_error missing_dependency environment_flaky"`
	FailingFiles     []string `json:"failing_files"     jsonschema:"description=Repository-relative file paths implicated in the failure" validate:"required,min=1"`
	ViolatedScenario string   `json:"violated_scenario" jsonschema:"description=Exact #### Scenario name from delta_specs if a behavioral test failed, or empty"`
	RootCause        string   `json:"root_cause"        jsonschema:"description=Concise 1-2 sentence technical explanation of why the gate failed" validate:"required,min=10,max=500"`
	RepairChecklist  []string `json:"repair_checklist"  jsonschema:"description=Concrete verification steps for the coding agent to fix without touching protected files" validate:"required,min=1,max=5"`
}

func (d *GateFailureDiagnosis) FormatMarkdown() string {
	if d == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Structured Gate Diagnosis (%s — %s):\n", d.FailedGate, d.Category)
	fmt.Fprintf(&b, "- Implicated files: %s\n", strings.Join(d.FailingFiles, ", "))
	if d.ViolatedScenario != "" {
		fmt.Fprintf(&b, "- Violated scenario: %s\n", d.ViolatedScenario)
	}
	fmt.Fprintf(&b, "- Root cause: %s\n", d.RootCause)
	b.WriteString("- Repair checklist:\n")
	for _, step := range d.RepairChecklist {
		fmt.Fprintf(&b, "  * %s\n", step)
	}
	return b.String()
}

// =============================================================================
// Use Case 2: Translating numeric rejections of the evaluator into Actionable Feedback
// =============================================================================

type DeficientSpecItem struct {
	Condition   string `json:"condition"    jsonschema:"description=The failed condition of the gate (e.g. scenarios_are_testable >= 0.6)" validate:"required"`
	Artifact    string `json:"artifact"     jsonschema:"description=File path under openspec/changes/<change>/ or repos/" validate:"required"`
	SectionName string `json:"section_name" jsonschema:"description=Exact Requirement or #### Scenario header name" validate:"required"`
	Defect      string `json:"defect"       jsonschema:"description=Why this specific section failed the rubric (e.g. vague THEN clause, no test asserting status code)" validate:"required"`
}

type RejectionBreakdown struct {
	Gate              string              `json:"gate"               jsonschema:"description=The evaluator gate that failed (plan_review or scenario_coverage)" validate:"required"`
	DeficientItems    []DeficientSpecItem `json:"deficient_items"    jsonschema:"description=Specific scenarios, requirements, or tasks that failed the rubric of the gate" validate:"required,min=1"`
	ActionableSummary string              `json:"actionable_summary" jsonschema:"description=Direct instructions for the PLAN or BUILD agent to raise the score above threshold" validate:"required"`
}

func (j *RejectionBreakdown) FormatMarkdown() string {
	if j == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Specific items that lowered the %s score:\n", j.Gate)
	for _, it := range j.DeficientItems {
		fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", it.Condition, it.Artifact, it.SectionName, it.Defect)
	}
	fmt.Fprintf(&b, "Action required: %s", j.ActionableSummary)
	return b.String()
}

// =============================================================================
// Use Case 3: Pre-BUILD Semantic Audit of OpenSpec Artifacts
// =============================================================================

type PlanSemanticValidation struct {
	Valid                  bool     `json:"valid"                     jsonschema:"description=True only if all scenarios are testable, tasks map to scenarios, and no protected paths are targeted"`
	VagueScenarios         []string `json:"vague_scenarios"           jsonschema:"description=Scenario names whose WHEN/THEN clauses lack observable outputs"`
	UncoveredScenarios     []string `json:"uncovered_scenarios"       jsonschema:"description=Scenario names in specs/ that have no corresponding implementation/test task in tasks.md"`
	TargetedProtectedPaths []string `json:"targeted_protected_paths"  jsonschema:"description=Any mention in design.md or tasks.md of editing repo.config.yaml, .github, .gitlab-ci.yml, or env_script"`
	BlockingReason         string   `json:"blocking_reason,omitempty" jsonschema:"description=If valid is false, concise explanation to feed back into PLAN repair"`
}

func (p *PlanSemanticValidation) FormatMarkdown() string {
	if p == nil || p.Valid {
		return ""
	}
	var parts []string
	if p.BlockingReason != "" {
		parts = append(parts, p.BlockingReason)
	}
	if len(p.TargetedProtectedPaths) > 0 {
		parts = append(parts, "Targeted protected paths (forbidden): "+strings.Join(p.TargetedProtectedPaths, ", "))
	}
	if len(p.VagueScenarios) > 0 {
		parts = append(parts, "Vague scenarios needing concrete WHEN/THEN: "+strings.Join(p.VagueScenarios, ", "))
	}
	if len(p.UncoveredScenarios) > 0 {
		parts = append(parts, "Scenarios missing tasks in tasks.md: "+strings.Join(p.UncoveredScenarios, ", "))
	}
	return strings.Join(parts, "\n")
}

// =============================================================================
// Use Case 4: Semantic Deduplication and Dependency Inference in Proposals
// =============================================================================

type ProposalNormalization struct {
	ChangeName          string   `json:"change_name"           jsonschema:"description=Original change_name of the proposal" validate:"required"`
	IsSemanticDuplicate bool     `json:"is_semantic_duplicate" jsonschema:"description=True if an existing backlog item, proposal, or canonical spec already covers this exact outcome"`
	DuplicateOf         string   `json:"duplicate_of,omitempty" jsonschema:"description=Name of the existing change or capability it duplicates"`
	InferredDependsOn   []string `json:"inferred_depends_on"   jsonschema:"description=Existing queued change_names that must be integrated before this change starts"`
	InferredLocks       []string `json:"inferred_locks"        jsonschema:"description=Capability slugs under openspec/specs/ or shared resources this change will modify"`
	NormalizedPriority  string   `json:"normalized_priority"   jsonschema:"enum=critical,enum=high,enum=normal,enum=low" validate:"required,oneof=critical high normal low"`
}

type ProposalNormalizationBatch struct {
	Proposals []ProposalNormalization `json:"proposals" jsonschema:"description=Normalized evaluation for each candidate proposal" validate:"required,min=1"`
}

// =============================================================================
// Use Case 5: Rebase Conflict Synthesis for the {{rebuild}} Prompt
// =============================================================================

type RebaseConflictBrief struct {
	ChildRepo           string   `json:"child_repo"            jsonschema:"description=Child repository path (e.g. repos/backend)" validate:"required"`
	ConflictingFiles    []string `json:"conflicting_files"     jsonschema:"description=Files that had content conflicts during rebase" validate:"required,min=1"`
	UpstreamIntent      string   `json:"upstream_intent"       jsonschema:"description=What the newly integrated commits on origin/base changed in those files" validate:"required"`
	WorkerIntent        string   `json:"worker_intent"         jsonschema:"description=What the worker branch was trying to implement in those files" validate:"required"`
	ReconciliationGuide string   `json:"reconciliation_guide"  jsonschema:"description=Step-by-step instructions for the BUILD agent to re-apply its feature on top of the new base without losing upstream changes" validate:"required"`
}

func (r *RebaseConflictBrief) FormatMarkdown() string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf(
		"Rebase Conflict Brief (%s — %s):\n- Upstream changes on base: %s\n- Worker changes to preserve: %s\n- Reconciliation guide: %s",
		r.ChildRepo, strings.Join(r.ConflictingFiles, ", "), r.UpstreamIntent, r.WorkerIntent, r.ReconciliationGuide,
	)
}

// =============================================================================
// Use Case 6: Curating and Routing Lessons from handoff.md into the memory (.agent-memory/)
// =============================================================================

type ScopedMemoryEntry struct {
	TargetScope string `json:"target_scope" jsonschema:"description=Target file under .agent-memory/: pitfalls.md, conventions.md, decisions.md, modules/repo-<repo>.md, or modules/spec-<capability>.md" validate:"required"`
	Cue         string `json:"cue"          jsonschema:"description=When this lesson applies (situation or trigger)" validate:"required"`
	Lesson      string `json:"lesson"       jsonschema:"description=Imperative rule of what to do and why" validate:"required"`
	Evidence    string `json:"evidence"     jsonschema:"description=Change name and commit/gate evidence that proved it" validate:"required"`
}

type CuratedMemoryPlan struct {
	EntriesToAdd       []ScopedMemoryEntry `json:"entries_to_add"        jsonschema:"description=Durable, non-obvious lessons backed by concrete evidence"`
	DuplicateIdsToDrop []string            `json:"duplicate_ids_to_drop" jsonschema:"description=Existing bullet summaries in .agent-memory/ superseded or duplicated by the new entries"`
}

// =============================================================================
// Use Case 7: Assisted Toolchain and Gate Inference for foreman-kit setup repo
// =============================================================================

type InferredRepoSetup struct {
	Kind           string         `json:"kind"            jsonschema:"enum=web,enum=cli" validate:"required,oneof=web cli"`
	BaseBranch     string         `json:"base_branch"     jsonschema:"description=Primary branch (main or master)" validate:"required"`
	SetupCmd       string         `json:"setup_cmd"       jsonschema:"description=Idempotent test DB or dependency preparation command"`
	LintCmd        string         `json:"lint_cmd"        jsonschema:"description=Fast non-interactive lint command from CI or package scripts" validate:"required"`
	TypecheckCmd   string         `json:"typecheck_cmd"   jsonschema:"description=Fast typecheck command if applicable"`
	TestCmd        string         `json:"test_cmd"        jsonschema:"description=Fast test command (honoring FOREMANKIT_CHANGED_FILES if supported)" validate:"required"`
	BuildCmd       string         `json:"build_cmd"       jsonschema:"description=Build verification command"`
	RegressionCmd  string         `json:"regression_cmd"  jsonschema:"description=Full regression suite command"`
	RegressionWhen []string       `json:"regression_when" jsonschema:"description=Glob patterns that should trigger full regression"`
	DevCommand     string         `json:"dev_command"     jsonschema:"description=Command to start local dev server on $PORT (for web repos)"`
	RequiredTCP    map[string]int `json:"required_tcp"    jsonschema:"description=Map of required local services (postgres, redis, etc.) to default TCP port"`
}

// =============================================================================
// Use Case 8: Executive Triage with Discriminated Union for Quarantine and .needs-human
// =============================================================================

type ApproveRiskRecommendation struct {
	ActionType  string `json:"action_type"  jsonschema:"const=approve_risk" validate:"required,eq=approve_risk"`
	RiskSummary string `json:"risk_summary" jsonschema:"description=Why the evaluator flagged this change as high risk" validate:"required"`
	SafetyCheck string `json:"safety_check" jsonschema:"description=What the human should verify in the diff before clicking Approve Risk" validate:"required"`
}

type AnswerClarificationRecommendation struct {
	ActionType        string   `json:"action_type"        jsonschema:"const=answer_clarification" validate:"required,eq=answer_clarification"`
	QuestionFromAgent string   `json:"question_from_agent" jsonschema:"description=The core product/spec question from .needs-human" validate:"required"`
	SuggestedOptions  []string `json:"suggested_options"  jsonschema:"description=2-3 concrete product decisions the human can pick to unblock the spec" validate:"required,min=2,max=3"`
}

type FixEnvironmentRecommendation struct {
	ActionType string `json:"action_type" jsonschema:"const=fix_environment" validate:"required,eq=fix_environment"`
	Service    string `json:"service"     jsonschema:"description=Missing service, port, or toolchain causing the failure" validate:"required"`
	Command    string `json:"command"     jsonschema:"description=Exact terminal command the operator should run on the host" validate:"required"`
}

type UnblockAndRetryRecommendation struct {
	ActionType string `json:"action_type" jsonschema:"const=unblock_and_retry" validate:"required,eq=unblock_and_retry"`
	Reason     string `json:"reason"      jsonschema:"description=Why this failure was transient and is safe to unblock immediately" validate:"required"`
}
