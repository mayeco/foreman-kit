package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/proc"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// RETRYABLE_REASONS lists quarantine reasons that can heal on their own and are eligible
// for automatic retries according to blocked_changes.retries and blocked_changes.retry_after.
var RETRYABLE_REASONS = []string{
	"gate_failure",
	"incomplete_tasks",
	"spec_invalid",
	"plan_review",
	"scenario_coverage",
	"repair_stalled",
	"rebase_conflict",
	"rebase_failed",
	"push_failed",
	"manifest_stale",
	// Routing gates of the evaluator: a transient system error, an environment that fails the gates or the agents.
	"transient_error",
	"environment_hold",
	"environment_unavailable",
	// A call of a detached agent the kit stopped still works in the worktree; it ends on its own.
	"worktree_busy",
}

// SYSTEM_REASONS lists system/framework failure reasons that pause the pipeline on first occurrence.
var SYSTEM_REASONS = []string{
	"judge_unavailable",
	"engine_unavailable",
	"orchestrator_error",
	// agent_outcome: the machine is broken (disk full, read-only, permissions); retried once unpaused.
	"environment_unavailable",
}

// Slot and exit constants matching the Ruby reference implementation.
const (
	CycleSlot  = 98
	MergerSlot = 99
	// BaseCheckSlot is the slot of the merger's checks of a base alone (merge.outside_commits:
	// verify): never the merger's own, whose test database and ports the change's gates just used (a
	// migration applied there would make the base look broken).
	BaseCheckSlot       = 96
	EnginesDownExitCode = 75
	// WorkdirBusyExitCode is the exit of an agent call that did not start because processes of a
	// stopped detached call still work in its worktree.
	WorkdirBusyExitCode = proc.ExitWorkdirBusy
)

// Merger state machine states stored in merge_attempts.state.
const (
	MergerStateIdle             = "IDLE"
	MergerStateClaimed          = "CLAIMED"
	MergerStatePolicyCheck      = "POLICY_CHECK"
	MergerStateRebasingChildren = "REBASING_CHILDREN"
	MergerStateCheckingWorkLoss = "CHECKING_WORK_LOSS"
	MergerStateAssembling       = "ASSEMBLING"
	MergerStateAuditingTasks    = "AUDITING_TASKS"
	MergerStateVerifyingBase    = "VERIFYING_BASE"
	MergerStateRunningGates     = "RUNNING_GATES"
	MergerStateArchiving        = "ARCHIVING"
	MergerStateCommitting       = "COMMITTING"
	MergerStatePushingChildren  = "PUSHING_CHILDREN"
	MergerStatePushingRoot      = "PUSHING_ROOT"
	MergerStateCompleted        = "COMPLETED"
)

var (
	ChangeNameRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	uncheckedTaskRe   = regexp.MustCompile(`(?m)^\s*(?:[-*+]|\d{1,9}[.)])\s+\[ \]`)
	checkedTaskRe     = regexp.MustCompile(`(?m)^\s*(?:[-*+]|\d{1,9}[.)])\s+\[[xX]\]`)
	htmlCommentRe     = regexp.MustCompile(`(?s)<!--.*?-->\n?`)
	placeholderRe     = regexp.MustCompile(`\{\{(\w+)\}\}`)
	secretRedactRe    = regexp.MustCompile(`(?i)((?:password|passwd|secret|token|api[_-]?key|access[_-]?key)\w*["']?\s*[:=]\s*["']?)[^\s"',]+`)
	urlCredentialsRe  = regexp.MustCompile(`(://[^/\s:@]+:)[^@\s/]+@`)
	heavyRegressionMu sync.Mutex
)

// ExtractOpenSpecInvalidItems parses JSON output from `openspec validate --json` and returns a compact
// JSON representation containing ONLY items with "valid": false.
func ExtractOpenSpecInvalidItems(raw string) string {
	var data map[string]any
	if parseOpenSpecJSON(raw, &data) != nil || data == nil {
		return tailLines(raw, 80)
	}
	items, ok := data["items"].([]any)
	if !ok || len(items) == 0 {
		return tailLines(raw, 80)
	}
	var invalid []any
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if valid, isBool := m["valid"].(bool); isBool && !valid {
				invalid = append(invalid, m)
			}
		}
	}
	if len(invalid) == 0 {
		return tailLines(raw, 80)
	}
	compact := map[string]any{
		"invalid_items": invalid,
		"summary":       data["summary"],
	}
	if b, err := json.MarshalIndent(compact, "", "  "); err == nil {
		return string(b)
	}
	return tailLines(raw, 80)
}

// ExtractGateFailureExcerpt extracts the actionable failure section from a single gate's output
// so it is never buried under thousands of progress dots or later passing gate outputs.
func ExtractGateFailureExcerpt(label, rawOut string, exitCode int) string {
	var prefix string
	if exitCode == 124 {
		prefix = fmt.Sprintf("GATE TIMEOUT: %s exceeded wall-clock limit (exit 124).\n", label)
	}
	if strings.HasPrefix(label, "openspec:") {
		return prefix + ExtractOpenSpecInvalidItems(rawOut)
	}
	// RSpec failure block extraction: from "Failures:" to "Failed examples:" (inclusive)
	if idx := strings.Index(rawOut, "\nFailures:\n"); idx >= 0 {
		sub := rawOut[idx+1:]
		if endIdx := strings.Index(sub, "\nFailed examples:\n"); endIdx >= 0 {
			rest := sub[endIdx+1:]
			// At most 25 lines of the list, which has one line per failure: whole, it pushed the
			// Failures block out of the last lines of the gate output, the ones the BUILD agent gets.
			lines := strings.SplitN(rest, "\n", 26)
			if len(lines) > 25 {
				lines = lines[:25]
			}
			return prefix + sub[:endIdx+1] + strings.Join(lines, "\n")
		}
		return prefix + tailLines(sub, 80)
	}
	// Go test failure block extraction: lines starting with "--- FAIL:" and surrounding context
	if strings.Contains(rawOut, "--- FAIL:") {
		var failLines []string
		lines := strings.Split(rawOut, "\n")
		inFail := false
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "--- FAIL:") {
				inFail = true
			}
			if inFail {
				failLines = append(failLines, line)
				if len(failLines) >= 60 {
					break
				}
			}
		}
		if len(failLines) > 0 {
			return prefix + strings.Join(failLines, "\n")
		}
	}
	return prefix + tailLines(rawOut, 60)
}

// PriorityRanks maps priority names to numeric sort order (0 = highest).
var PriorityRanks = map[string]int{
	"critical": 0,
	"high":     1,
	"normal":   2,
	"low":      3,
}

// PriorityNames maps numeric rank back to canonical priority label.
var PriorityNames = map[int]string{
	0: "critical",
	1: "high",
	2: "normal",
	3: "low",
}

// IsRetryableReason reports whether reason is in RETRYABLE_REASONS.
func IsRetryableReason(reason string) bool {
	for _, r := range RETRYABLE_REASONS {
		if r == reason {
			return true
		}
	}
	return false
}

// IsSystemReason reports whether reason is in SYSTEM_REASONS.
func IsSystemReason(reason string) bool {
	for _, r := range SYSTEM_REASONS {
		if r == reason {
			return true
		}
	}
	return false
}

// ProtectedChildPaths returns the paths of a child repository a change may not touch:
// repository.protected_paths of .foreman-kit/pipeline.yaml, plus the repository's env_script.
func ProtectedChildPaths(envScript string) []string {
	base := settings.Strings("", settings.ProtectedPaths)
	out := make([]string, 0, len(base)+1)
	seen := make(map[string]bool, len(base)+1)
	for _, p := range base {
		p = strings.TrimSpace(filepath.ToSlash(p))
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	envScript = strings.TrimSpace(filepath.ToSlash(envScript))
	if envScript != "" && !seen[envScript] {
		out = append(out, envScript)
	}
	return out
}

// ProtectedChildPathsFromBlob reads repo.config.yaml from childRepo at forkRev and returns
// all protected paths for that child repository.
func ProtectedChildPathsFromBlob(childRepo *gitx.Repo, forkRev string) []string {
	var envScript string
	if childRepo != nil && forkRev != "" {
		if raw, err := childRepo.ReadBlob(forkRev, "repo.config.yaml"); err == nil && len(raw) > 0 {
			var cfg map[string]any
			if yaml.Unmarshal(raw, &cfg) == nil {
				if es, ok := cfg["env_script"].(string); ok {
					envScript = strings.TrimSpace(es)
				}
			}
		}
	}
	return ProtectedChildPaths(envScript)
}

// IsProtectedChildPath reports whether relPath matches any of the protected child paths
// (either exact match or inside a protected directory such as .github/...).
func IsProtectedChildPath(relPath string, protected []string) bool {
	relPath = strings.TrimPrefix(filepath.ToSlash(relPath), "/")
	for _, prot := range protected {
		prot = strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(prot), "/"), "/")
		if prot == "" {
			continue
		}
		if relPath == prot || strings.HasPrefix(relPath, prot+"/") {
			return true
		}
	}
	return false
}

// BacklogItem represents an entry in .foreman-kit/agent_backlog.json or .foreman-kit/agent_history.json.
const maxBacklogTextBytes = 8 * 1024

func sanitizeBacklogText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= maxBacklogTextBytes {
		return s
	}
	const suffix = "... (truncated)"
	if maxBacklogTextBytes <= len(suffix) {
		return cutText(s, maxBacklogTextBytes)
	}
	return cutText(s, maxBacklogTextBytes-len(suffix)) + suffix
}

type BacklogItem struct {
	ChangeName   string         `json:"change_name"`
	Description  string         `json:"description,omitempty"`
	Why          string         `json:"why,omitempty"`
	Priority     string         `json:"priority,omitempty"`
	ImpactScore  float64        `json:"impact_score,omitempty"`
	DependsOn    []string       `json:"depends_on,omitempty"`
	Locks        []string       `json:"locks,omitempty"`
	QueuedAt     string         `json:"queued_at,omitempty"`
	IntegratedAt string         `json:"integrated_at,omitempty"`
	Extra        map[string]any `json:"-"`
}

// stringListField reads a backlog list field written as a JSON list or as one (comma-separated) string.
func stringListField(v any) []string {
	var parts []string
	switch t := v.(type) {
	case string:
		parts = strings.Split(t, ",")
	case []any:
		for _, it := range t {
			if it != nil {
				parts = append(parts, fmt.Sprint(it))
			}
		}
	case []string:
		parts = t
	}
	var out []string
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// BacklogItemFromMap converts a generic JSON object map into a BacklogItem while preserving Extra fields.
func BacklogItemFromMap(m map[string]any) BacklogItem {
	var item BacklogItem
	if m == nil {
		return item
	}
	b, _ := json.Marshal(m)
	_ = json.Unmarshal(b, &item)
	item.Description = sanitizeBacklogText(item.Description)
	item.Why = sanitizeBacklogText(item.Why)
	// json.Unmarshal drops a list field written as a string ("depends_on": "add-api") or with
	// non-string items, which silently bypassed dependency and lock scheduling: read both leniently.
	item.DependsOn = stringListField(m["depends_on"])
	item.Locks = stringListField(m["locks"])
	if v, ok := m["impact_score"]; ok {
		switch n := v.(type) {
		case float64:
			item.ImpactScore = n
		case int:
			item.ImpactScore = float64(n)
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
				item.ImpactScore = f
			}
		}
	}
	extra := make(map[string]any)
	for k, v := range m {
		switch k {
		case "change_name", "description", "why", "priority", "impact_score", "depends_on", "locks", "queued_at", "integrated_at":
		default:
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		item.Extra = extra
	}
	return item
}

// ToMap converts BacklogItem to a map[string]any suitable for JSON serialization.
func (b BacklogItem) ToMap() map[string]any {
	out := make(map[string]any)
	for k, v := range b.Extra {
		out[k] = v
	}
	out["change_name"] = b.ChangeName
	if b.Description != "" {
		out["description"] = b.Description
	}
	if b.Why != "" {
		out["why"] = b.Why
	}
	if b.Priority != "" {
		out["priority"] = b.Priority
	}
	if b.ImpactScore != 0 {
		out["impact_score"] = b.ImpactScore
	}
	if len(b.DependsOn) > 0 {
		out["depends_on"] = b.DependsOn
	}
	if len(b.Locks) > 0 {
		out["locks"] = b.Locks
	}
	if b.QueuedAt != "" {
		out["queued_at"] = b.QueuedAt
	}
	if b.IntegratedAt != "" {
		out["integrated_at"] = b.IntegratedAt
	}
	return out
}

// QuarantineInfo holds enriched quarantine and retry state for a change.
type QuarantineInfo struct {
	ChangeName      string `json:"change_name"`
	Status          string `json:"status"`
	Reason          string `json:"reason,omitempty"`
	Details         string `json:"details,omitempty"`
	Stage           string `json:"stage,omitempty"`
	Worktree        string `json:"worktree,omitempty"`
	Branch          string `json:"branch,omitempty"`
	Host            string `json:"host,omitempty"`
	Retries         int    `json:"retries"`
	RetryLimit      int    `json:"retry_limit,omitempty"`
	NextRetryAt     string `json:"next_retry_at,omitempty"`
	RetryNote       string `json:"retry_note,omitempty"`
	Rebuilds        int    `json:"rebuilds"`
	ConflictDetails string `json:"conflict_details,omitempty"`
	ApprovedRisk    bool   `json:"approved_risk"`
	BlockedAt       string `json:"blocked_at,omitempty"`
	UpdatedAt       string `json:"updated_at,omitempty"`
}

// BuildQuarantineInfo constructs QuarantineInfo with computed retry schedule/notes from a store.QuarantineRecord.
func BuildQuarantineInfo(rec store.QuarantineRecord, retryLimit int, retryAfter time.Duration, now time.Time) QuarantineInfo {
	info := QuarantineInfo{
		ChangeName:      rec.ChangeName,
		Status:          rec.Status,
		Reason:          rec.Reason,
		Details:         rec.Details,
		Stage:           rec.Stage,
		Worktree:        rec.Worktree,
		Branch:          rec.Branch,
		Host:            rec.Host,
		Retries:         rec.Retries,
		RetryLimit:      retryLimit,
		Rebuilds:        rec.Rebuilds,
		ConflictDetails: rec.ConflictDetails,
		ApprovedRisk:    rec.ApprovedRisk,
		BlockedAt:       rec.BlockedAt,
		UpdatedAt:       rec.UpdatedAt,
	}
	if retryLimit <= 0 || rec.Status != "blocked" {
		return info
	}
	if !IsRetryableReason(rec.Reason) {
		info.RetryNote = "never retried: needs a person"
		return info
	}
	if rec.Retries >= retryLimit {
		info.RetryNote = fmt.Sprintf("no retries left: %d of %d used", rec.Retries, retryLimit)
		return info
	}
	blockedAt, err := time.Parse(time.RFC3339, rec.BlockedAt)
	if err != nil {
		if t, err2 := time.Parse(time.RFC3339, rec.UpdatedAt); err2 == nil {
			blockedAt = t
		} else {
			blockedAt = time.Unix(0, 0).UTC()
		}
	}
	dueAt := blockedAt.Add(retryAfter)
	info.NextRetryAt = dueAt.UTC().Format(time.RFC3339)
	whenDue := "at the next dispatch"
	if dueAt.After(now) {
		whenDue = "at " + info.NextRetryAt
	}
	info.RetryNote = fmt.Sprintf("retry %d of %d %s", rec.Retries+1, retryLimit, whenDue)
	return info
}

// RunningDispatchInfo describes an active dispatcher process holding the dispatcher lock.
type RunningDispatchInfo struct {
	PID       int               `json:"pid"`
	StartedAt string            `json:"started_at,omitempty"`
	Workers   int               `json:"workers"`
	Results   map[string]string `json:"results"`
}

// DryRunItem describes the scheduling status of a single backlog entry in DryRunReport.
type DryRunItem struct {
	Change      string   `json:"change"`
	Priority    string   `json:"priority"`
	CountsAs    string   `json:"counts_as"`
	WaitedHours *float64 `json:"waited_hours"`
	State       string   `json:"state"`
	Why         string   `json:"why"`
	Next        bool     `json:"next"`
}

// DryRunReport holds the complete output of Dispatcher.DryRun(ctx).
type DryRunReport struct {
	Paused      bool                 `json:"paused"`
	PauseReason string               `json:"pause_reason,omitempty"`
	Dispatch    *RunningDispatchInfo `json:"dispatch"`
	FreeWorkers int                  `json:"free_workers"`
	Items       []DryRunItem         `json:"items"`
	// MainBehind is how many commits the main checkout, which the backlog is read from, is behind
	// Upstream (a dispatch fast-forwards it first; a dry run only reads it).
	MainBehind int    `json:"main_behind,omitempty"`
	Upstream   string `json:"upstream,omitempty"`
	// BacklogError is why .foreman-kit/agent_backlog.json cannot be parsed ("" when it can).
	BacklogError string `json:"backlog_error,omitempty"`
}

// FormatJSON renders the DryRunReport as pretty-printed JSON matching `openspec_dispatcher.rb --dry-run --json`.
func (r *DryRunReport) FormatJSON() (string, error) {
	payload := map[string]any{
		"paused":       r.Paused,
		"dispatch":     r.Dispatch,
		"free_workers": r.FreeWorkers,
		"items":        r.Items,
	}
	if r.BacklogError != "" {
		payload["backlog_error"] = r.BacklogError
	}
	if r.MainBehind > 0 {
		payload["main_behind"] = r.MainBehind
		payload["upstream"] = r.Upstream
	}
	if r.Items == nil {
		payload["items"] = []DryRunItem{}
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// FormatText renders the DryRunReport as human-readable text matching `openspec_dispatcher.rb --dry-run`.
func (r *DryRunReport) FormatText(maxWorkers int, buildingCount int) string {
	if maxWorkers <= 0 {
		maxWorkers = 2
	}
	var head string
	if r.Dispatch != nil {
		if buildingCount == 0 {
			for _, it := range r.Items {
				if it.State == "building" {
					buildingCount++
				}
			}
		}
		head = fmt.Sprintf("dispatch: running (pid %d, %d workers, %d building, %d free)",
			r.Dispatch.PID, r.Dispatch.Workers, buildingCount, r.FreeWorkers)
	} else {
		head = fmt.Sprintf("dispatch: not running; the next cycle (or foreman-kit dispatch) starts one with %d workers", maxWorkers)
	}
	lines := []string{head}
	if r.MainBehind > 0 {
		lines = append(lines, fmt.Sprintf("main checkout: %d commit(s) behind %s, so this backlog may be stale (a dispatch fast-forwards it first; or foreman-kit sync)", r.MainBehind, r.Upstream))
	}
	if r.Paused {
		reason := strings.TrimSpace(strings.SplitN(r.PauseReason, "\n", 2)[0])
		if reason != "" {
			lines = append(lines, fmt.Sprintf("PAUSED (%s): nothing new starts", reason))
		} else {
			lines = append(lines, "PAUSED: nothing new starts")
		}
	}
	if r.BacklogError != "" {
		lines = append(lines, "BACKLOG UNREADABLE: .foreman-kit/agent_backlog.json: "+r.BacklogError+"; nothing new starts until it is valid JSON")
	}
	if len(r.Items) == 0 {
		lines = append(lines, "backlog: nothing pending")
		return strings.Join(lines, "\n")
	}
	width := 0
	for _, it := range r.Items {
		if len(it.Change) > width {
			width = len(it.Change)
		}
	}
	for n, it := range r.Items {
		prio := it.Priority
		if it.CountsAs != "" && it.CountsAs != it.Priority {
			prio = it.Priority + ">" + it.CountsAs
		}
		waited := "-"
		if it.WaitedHours != nil {
			waited = fmt.Sprintf("%.1fh", *it.WaitedHours)
		}
		lines = append(lines, fmt.Sprintf("%2d. %-*s  %-15s %6s  %s", n+1, width, it.Change, prio, waited, it.Why))
	}
	return strings.Join(lines, "\n")
}

// BlockedError represents a deterministic or policy blocker that quarantines a change.
type BlockedError struct {
	Reason  string
	Details string
}

func NewBlockedError(reason, details string) *BlockedError {
	if len(details) > 4000 {
		details = details[len(details)-4000:]
	}
	return &BlockedError{Reason: reason, Details: details}
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("%s: %s", e.Reason, tailLines(e.Details, 3))
}

// EnginesDownError indicates every engine of an agent call failed without modifying the worktree (exit 75).
type EnginesDownError struct {
	Output string
	// Role is the agent role whose every engine was out ("plan" or "build").
	Role string
}

func (e *EnginesDownError) Error() string {
	return "engines_down: " + tailLines(e.Output, 5)
}

// AgentRunRequest encapsulates an agent invocation for PLAN or BUILD.
type AgentRunRequest struct {
	Change     string
	Phase      string
	Role       string
	Engine     string
	Worktree   string
	Prompt     string
	PromptFile string
	Slot       int
	ScratchDir string
	Timeout    time.Duration
	Env        map[string]string
	// ExecID is the execution the caller already recorded for this call: the runner writes the
	// agent's output and attempts under it and leaves recording how it ended to the caller. Empty,
	// the default runner records an execution of its own.
	ExecID string
}

// AgentRunResult holds the result of an AgentRunnerFunc call.
type AgentRunResult struct {
	Output     string
	ExitCode   int
	UsedEngine string
	// ExecID is the execution the agent's output was recorded under ("" when none was).
	ExecID string
}

// AgentRunnerFunc executes an AI agent in a worker worktree.
type AgentRunnerFunc func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error)

// GateRunRequest encapsulates a quality gate execution in a worker or integration worktree.
type GateRunRequest struct {
	Change   string
	Worktree string
	BaseRef  string
	Slot     int
	Timeout  time.Duration
	Env      map[string]string
	// ChildOnto maps a child repository path to the commit the merger rebased the change's
	// commits onto: the repository's current base branch. Empty outside the merger.
	ChildOnto map[string]string
	// OutsideCommits is merge.outside_commits. Where ChildOnto holds commits the gitlink BaseRef
	// records does not (pushed outside the pipeline), "ignore" and "verify" measure the child's
	// FOREMANKIT_CHANGED_FILES and regression.when from ChildOnto, so those commits never count as the
	// change's ("verify" tests them on the base alone, in the merger); "include" (or empty) measures
	// from the gitlink, so they do.
	OutsideCommits string
}

// GateRunResult holds the result of a GateRunnerFunc call.
type GateRunResult struct {
	Output   string
	ExitCode int
	OK       bool
	// Failures lists the gates that failed, so the merger can tell which repository each belongs to
	// without parsing Output.
	Failures []GateFailure
	// RoundID is the gate round the gates were recorded under (0 when they were not recorded).
	RoundID int64
}

// GateFailure is one failing gate of a GateRunnerFunc call.
type GateFailure struct {
	// Child is the child repository path (repos/core); empty for a gate of the OpenSpec layer.
	Child string
	// Gate is the gate key: "test", "regression:full", "config" (an invalid repo.config.yaml),
	// "env_script" (a missing env_script), or "openspec:specs" / "openspec:<change>" for the OpenSpec layer.
	Gate     string
	ExitCode int
}

// GateRunnerFunc executes quality gates in a worktree.
type GateRunnerFunc func(ctx context.Context, req GateRunRequest) (GateRunResult, error)

// OpenSpecResult holds the output and exit status of an openspec command.
type OpenSpecResult struct {
	Output   string
	ExitCode int
	OK       bool
}

// OpenSpecRunnerFunc executes an openspec CLI subcommand (or built-in equivalent) in dir.
type OpenSpecRunnerFunc func(ctx context.Context, dir string, args ...string) (OpenSpecResult, error)

// PipelineSettings is what the dispatcher and the merger read from .foreman-kit/pipeline.yaml, and the
// limits of an agent call they plan with, from .foreman-kit/agents.yaml.
type PipelineSettings struct {
	BaseBranch string
	Remote     string
	Workers    int
	// PriorityBumpEvery is how long a backlog item waits before it is ordered one priority higher
	// (0: never).
	PriorityBumpEvery time.Duration
	// RepairRounds is how many times a phase is sent back to its agent after it fails.
	RepairRounds int
	GateTimeout  time.Duration
	// BlockedRetries is how many times a blocked change that can heal on its own runs again, and
	// BlockedRetryAfter how long it waits before each of those runs.
	BlockedRetries    int
	BlockedRetryAfter time.Duration
	// OutsideCommits is what the merger does with commits pushed to a child repository outside the
	// pipeline: config.OutsideCommitsVerify, OutsideCommitsIgnore or OutsideCommitsInclude.
	OutsideCommits string
	// BaseRecheckAfter is how long after a base failed on its own it is checked again (0: never).
	BaseRecheckAfter time.Duration
	ConflictRebuilds int
	LocksStaleAfter  time.Duration

	// PlanTimeout and BuildTimeout limit one agent call of each role.
	PlanTimeout     time.Duration
	BuildTimeout    time.Duration
	AgentRetries    int
	AgentRetryAfter time.Duration
	// OutageMaxWait is the longest no worker starts after every agent of a role went out, and
	// OutageGiveUpAfter how many outages a change waits through before it is blocked (0: never).
	OutageMaxWait     time.Duration
	OutageGiveUpAfter int

	// dispatcherWaits and mergerWaits say what keeps a file each of them reads from being used
	// ("" when nothing does): see settings.Reader.Unusable.
	dispatcherWaits string
	mergerWaits     string
}

// LoadPipelineSettings returns the settings the dispatcher and the merger run with, each one as the
// settings catalog reads it: the value --set gave this run, else its file in root (or in the main
// checkout). Without a root they are the values the kit ships, with what --set gave the run over
// them.
func LoadPipelineSettings(root string) PipelineSettings {
	r := settings.At(root)
	var dispatcherWaits, mergerWaits string
	if root == "" {
		r.Use(settings.BaseBranch.File(), nil).Use(settings.Agents.File(), nil)
		r.Stderr = io.Discard
	} else {
		// The files are judged as they are written, whatever --set gave this run: each of the
		// two reads its own, and neither waits for a file it does not read. The same Reader gives
		// the values below, so each file is read once.
		dispatcherWaits = r.Unusable(settings.DispatcherFiles...)
		mergerWaits = r.Unusable(settings.MergerFiles...)
		// The gates of a repository run as its repo.config.yaml says: one that still holds the keys
		// an older foreman-kit read keeps both from starting anything, like a file of their own.
		if older := config.RepoConfigsUnusable(root); older != "" {
			if dispatcherWaits == "" {
				dispatcherWaits = older
			}
			if mergerWaits == "" {
				mergerWaits = older
			}
		}
		if dispatcherWaits != "" || mergerWaits != "" {
			// What cannot be read is said by whoever waits for it, once, not key by key.
			r.Stderr = io.Discard
		}
	}
	return PipelineSettings{
		dispatcherWaits:   dispatcherWaits,
		mergerWaits:       mergerWaits,
		BaseBranch:        r.String(settings.BaseBranch),
		Remote:            r.String(settings.Remote),
		Workers:           r.Int(settings.Workers),
		PriorityBumpEvery: r.Duration(settings.PriorityBumpEvery),
		RepairRounds:      r.Int(settings.RepairRounds),
		GateTimeout:       r.Duration(settings.GateTimeout),
		BlockedRetries:    r.Int(settings.BlockedRetries),
		BlockedRetryAfter: r.Duration(settings.BlockedRetryAfter),
		OutsideCommits:    r.String(settings.OutsideCommits),
		BaseRecheckAfter:  r.Duration(settings.BaseRecheckAfter),
		ConflictRebuilds:  r.Int(settings.ConflictRebuilds),
		LocksStaleAfter:   r.Duration(settings.LocksStaleAfter),
		PlanTimeout:       r.Duration(settings.RoleTimeout("plan")),
		BuildTimeout:      r.Duration(settings.RoleTimeout("build")),
		AgentRetries:      r.Int(settings.AgentRetries),
		AgentRetryAfter:   r.Duration(settings.AgentRetryAfter),
		OutageMaxWait:     r.Duration(settings.OutageMaxWait),
		OutageGiveUpAfter: r.Int(settings.OutageGiveUpAfter),
	}
}

// noBaseBranch says why nothing runs against these settings when the application names no base branch
// ("" when it names one), the one key that has no value of the kit's own. Starting a change from, or
// publishing to, a branch nobody named is never an answer to a file that lacks it.
func (s PipelineSettings) noBaseBranch() string {
	if s.BaseBranch == "" {
		return fmt.Sprintf("%s is missing or not a branch name: nothing starts and nothing is integrated until it names the branch every change starts from (foreman-kit config set %s <branch>)",
			settings.BaseBranch.Where(), settings.BaseBranch)
	}
	return ""
}

// DispatcherWaits says why a dispatcher starts no change with these settings ("" when it can): the
// application names no base branch, or a file the dispatcher reads (settings.DispatcherFiles) lacks
// a key, holds a value the key does not accept, or writes a key nothing reads. The dispatcher does
// not work with a value nobody wrote: it says which key keeps it and goes on when the file is
// mended. The changes already being built are not stopped.
func (s PipelineSettings) DispatcherWaits() string {
	if why := s.noBaseBranch(); why != "" {
		return why
	}
	if s.dispatcherWaits != "" {
		return s.dispatcherWaits + ": no change starts until it is mended"
	}
	return ""
}

// MergerWaits says why a merger integrates nothing with these settings ("" when it can), like
// DispatcherWaits for the files the merger reads (settings.MergerFiles).
func (s PipelineSettings) MergerWaits() string {
	if why := s.noBaseBranch(); why != "" {
		return why
	}
	if s.mergerWaits != "" {
		return s.mergerWaits + ": nothing is integrated until it is mended"
	}
	return ""
}

// LoadAgentRoleEngine returns the agents of role ("plan", "build" or "cycle"), in order and joined by
// commas: its environment override, else .foreman-kit/agents.yaml. A role without agents has none: nothing
// runs in its place.
func LoadAgentRoleEngine(root, role string) string {
	list, err := config.Engines(root, role)
	if err != nil {
		return ""
	}
	return strings.Join(list, ",")
}

// RenderPrompt renders .foreman-kit/prompts/<name>.md (falling back to embedded seed) with vars,
// returning both the rendered text and the 12-char prompt version hash.
func RenderPrompt(root, name string, vars map[string]string) (string, string) {
	var raw []byte
	if root != "" {
		p := filepath.Join(root, ".foreman-kit", "prompts", name+".md")
		if b, err := os.ReadFile(p); err == nil {
			raw = b
		} else if mainRoot := store.MainRoot(root); mainRoot != root {
			if b, err := os.ReadFile(filepath.Join(mainRoot, ".foreman-kit", "prompts", name+".md")); err == nil {
				raw = b
			}
		}
	}
	if len(raw) == 0 {
		raw, _ = assets.ReadSeed(".foreman-kit/prompts/" + name + ".md")
	}
	stripped := htmlCommentRe.ReplaceAllString(string(raw), "")
	hashInput := stripped
	if strings.Contains(stripped, "{{authoring_rules}}") {
		if rules, err := assets.ReadTemplate(root, "authoring_rules.md"); err == nil {
			hashInput += "\x00" + string(rules)
		}
	}
	sum := sha256.Sum256([]byte(hashInput))
	version := hex.EncodeToString(sum[:])[:12]

	rendered := placeholderRe.ReplaceAllStringFunc(stripped, func(m string) string {
		sub := placeholderRe.FindStringSubmatch(m)
		if len(sub) == 2 {
			if v, ok := vars[sub[1]]; ok {
				return v
			}
		}
		return m
	})
	return rendered, version
}

// MemoryFingerprintAndPromptBlock returns the version of the memory in the worktree wt (the twelve
// characters of memory.Fingerprint) and the {{memory}} block of the PLAN and BUILD prompts
// (memory.PromptBlock).
func MemoryFingerprintAndPromptBlock(wt string, capabilities []string) (string, string) {
	return memory.Fingerprint(wt), memory.PromptBlock(wt, capabilities)
}

// DefaultAgentRunner returns the default AgentRunnerFunc that invokes bin/agent_exec.sh (if present)
// or runs the configured engine command directly, stripping push credentials from the worker environment
// and recording execution logs in store.
func DefaultAgentRunner(st *store.Store) AgentRunnerFunc {
	return func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		timeout := req.Timeout
		if timeout <= 0 {
			timeout = 2 * time.Hour
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		// A role without agents runs none: the call below then says which key lists none.
		engine := strings.TrimSpace(req.Engine)
		firstEngine := strings.TrimSpace(strings.Split(engine, ",")[0])

		scratchDir := req.ScratchDir
		if scratchDir == "" && req.Worktree != "" && req.Change != "" {
			scratchDir = store.RuntimePath(store.MainRoot(req.Worktree), "tmp", req.Change)
		}
		if scratchDir != "" {
			_ = os.MkdirAll(scratchDir, 0o755)
		}

		// If AGENT_STUB is set and engine is stub, run stub directly
		if firstEngine == "stub" && os.Getenv("AGENT_STUB") != "" {
			stubPath := os.Getenv("AGENT_STUB")
			cmd := exec.CommandContext(runCtx, "bash", stubPath, req.PromptFile)
			cmd.Dir = req.Worktree
			cmd.Env = buildWorkerEnv(req.Slot, scratchDir, req.Env)
			outBytes, err := cmd.CombinedOutput()
			exitCode := 0
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				} else if runCtx.Err() == context.DeadlineExceeded {
					exitCode = 124
				} else {
					exitCode = 1
				}
			}
			return AgentRunResult{
				Output:     string(outBytes),
				ExitCode:   exitCode,
				UsedEngine: "stub",
			}, nil
		}

		// Delegate to proc.AgentExec so the commands of every agent, the selection among them, retries,
		// quota/auth detection, continuation notes, idle watchdog, and SQLite log_chunks work natively.
		var outBuf bytes.Buffer
		execID := req.ExecID
		ownExec := execID == "" && st != nil
		if ownExec {
			execID = NewWorkerExecID(req.Change, req.Phase)
			_ = st.CreateExecution(ctx, store.Execution{
				ExecID:           execID,
				Kind:             "worker",
				ChangeName:       req.Change,
				EnginesRequested: engine,
				Phase:            req.Phase,
				Role:             req.Role,
				Status:           "running",
				PID:              os.Getpid(),
				PromptText:       req.Prompt,
			})
		}

		res, _ := proc.AgentExec(runCtx, proc.AgentExecOptions{
			Workdir:    req.Worktree,
			Prompt:     req.Prompt,
			PromptFile: req.PromptFile,
			Engines:    engine,
			Role:       req.Role,
			Change:     req.Change,
			Phase:      req.Phase,
			Env:        buildWorkerEnv(req.Slot, scratchDir, req.Env),
			Store:      st,
			ExecID:     execID,
			SkipEvent:  true, // Dispatcher emits enriched agent.exit event with prompt/memory version
			Stdout:     &outBuf,
			Stderr:     &outBuf,
		})

		used := res.UsedEngine
		if used == "" {
			used = firstEngine
		}
		if ownExec {
			// Recorded whatever became of ctx. When the worker is stopped (a daemon stop, a lost lease)
			// ctx has ended, a write under it fails without running, and the execution stayed "running"
			// under this live PID until the next dispatcher reaped it.
			_, _ = st.EndExecution(context.WithoutCancel(ctx), execID, store.ExecutionEnd{
				Status:     AgentExecutionStatus(ctx, res.ExitCode),
				ExitCode:   res.ExitCode,
				EngineUsed: used,
			})
		}
		return AgentRunResult{
			Output:     res.Output,
			ExitCode:   res.ExitCode,
			UsedEngine: used,
			ExecID:     execID,
		}, nil
	}
}

// DefaultGateRunner returns the default GateRunnerFunc that executes OpenSpec validation and
// each touched child repository's configured gates from repo.config.yaml (with env_script and FOREMANKIT_SLOT).
func DefaultGateRunner(st *store.Store) GateRunnerFunc {
	run := defaultGateRunner(st)
	if st == nil {
		return run
	}
	// The gates of one call are one round: every gate it runs is recorded under it, and it closes
	// with the result of the call. The round belongs to the worker run or the merge attempt in the
	// scope of ctx.
	return func(ctx context.Context, req GateRunRequest) (GateRunResult, error) {
		gateContext := "worker_verify"
		if req.Slot == MergerSlot {
			gateContext = "merger"
		}
		roundID, _ := st.BeginGateRound(context.WithoutCancel(ctx), store.GateRound{
			Context:    gateContext,
			ChangeName: req.Change,
			Slot:       req.Slot,
			BaseRef:    req.BaseRef,
		})
		res, err := run(store.WithGateRound(ctx, roundID), req)
		result := "passed"
		switch {
		case ctx.Err() != nil:
			result = "stopped"
		case err != nil || !res.OK:
			result = "failed"
		}
		_ = st.FinishGateRound(context.WithoutCancel(ctx), roundID, result)
		res.RoundID = roundID
		return res, err
	}
}

func defaultGateRunner(st *store.Store) GateRunnerFunc {
	osRunner := DefaultOpenSpecRunner()
	return func(ctx context.Context, req GateRunRequest) (GateRunResult, error) {
		timeout := req.Timeout
		if timeout <= 0 {
			timeout = 45 * time.Minute
		}
		// The deadline covers the time the gates run, never the time a regression waits for
		// heavyRegressionMu (another regression suite of the process, such as a worker's BUILD
		// verification while the daemon merges): that wait pushes it back (see the gate loop).
		deadline := time.Now().Add(timeout)
		runCtx, cancel := context.WithDeadline(ctx, deadline)
		defer func() { cancel() }()

		var buf bytes.Buffer
		var failed []string
		var failures []GateFailure
		var failureExcerpts []string
		passed := 0
		gateContext := "worker_verify"
		if req.Slot == MergerSlot {
			gateContext = "merger"
		}

		// repoPaths maps a repository's short name, which labels its gates, to its path.
		repoPaths := map[string]string{"openspec": store.UmbrellaRepoPath}
		recordGate := func(repoName, gateName, cmdStr, rawOut string, rc int, dur time.Duration) {
			var excerpt string
			label := repoName + ":" + gateName
			if rc != 0 {
				excerpt = ExtractGateFailureExcerpt(label, rawOut, rc)
				failureExcerpts = append(failureExcerpts, fmt.Sprintf("[%s (exit %d)]\n%s", label, rc, excerpt))
			}
			if st != nil {
				// Not under runCtx: it carries the deadline of the gates, so the gate that deadline
				// stopped, and every gate reported after it, was never recorded.
				_ = st.RecordGateRun(context.WithoutCancel(runCtx), store.GateRunRecord{
					ChangeName:     req.Change,
					Context:        gateContext,
					RepoName:       repoName,
					RepoPath:       repoPaths[repoName],
					GateName:       gateName,
					Command:        cmdStr,
					Slot:           req.Slot,
					ExitCode:       rc,
					DurationMS:     dur.Milliseconds(),
					FailureExcerpt: excerpt,
				})
			}
		}

		baseRef := req.BaseRef
		if baseRef == "" {
			baseRef = "HEAD"
		}

		// Determine changed files in umbrella worktree
		changedPaths, _ := gitx.ChangedPaths(runCtx, req.Worktree, baseRef)
		changedMap := make(map[string]bool, len(changedPaths))
		for _, p := range changedPaths {
			changedMap[p] = true
		}

		openspecTouched := false
		activeChanges := make(map[string]bool)
		for p := range changedMap {
			if strings.HasPrefix(p, "openspec/") {
				openspecTouched = true
				parts := strings.Split(p, "/")
				if len(parts) >= 3 && parts[1] == "changes" && parts[2] != "archive" {
					activeChanges[parts[2]] = true
				}
			}
		}

		if openspecTouched {
			t0 := time.Now()
			fmt.Fprintln(&buf, "---- [gate] openspec:specs: openspec validate --specs --strict --json")
			res, _ := osRunner(runCtx, req.Worktree, "validate", "--specs", "--strict", "--json")
			if res.OK {
				fmt.Fprintln(&buf, "---- [pass] openspec:specs")
				passed++
				recordGate("openspec", "specs", "openspec validate --specs --strict --json", res.Output, 0, time.Since(t0))
			} else {
				fmt.Fprintln(&buf, res.Output)
				fmt.Fprintln(&buf, "---- [FAIL] openspec:specs")
				failed = append(failed, "openspec:specs")
				failures = append(failures, GateFailure{Gate: "openspec:specs", ExitCode: 1})
				recordGate("openspec", "specs", "openspec validate --specs --strict --json", res.Output, 1, time.Since(t0))
			}

			var chgList []string
			for c := range activeChanges {
				if stInfo, err := os.Stat(filepath.Join(req.Worktree, "openspec", "changes", c)); err == nil && stInfo.IsDir() {
					chgList = append(chgList, c)
				}
			}
			sort.Strings(chgList)
			for _, c := range chgList {
				t1 := time.Now()
				label := "openspec:" + c
				cmdStr := fmt.Sprintf("openspec validate %s --strict --json", c)
				fmt.Fprintf(&buf, "---- [gate] %s: %s\n", label, cmdStr)
				res, _ := osRunner(runCtx, req.Worktree, "validate", c, "--strict", "--json")
				if res.OK {
					fmt.Fprintf(&buf, "---- [pass] %s\n", label)
					passed++
					recordGate("openspec", c, cmdStr, res.Output, 0, time.Since(t1))
				} else {
					fmt.Fprintln(&buf, res.Output)
					fmt.Fprintf(&buf, "---- [FAIL] %s\n", label)
					failed = append(failed, label)
					failures = append(failures, GateFailure{Gate: label, ExitCode: 1})
					recordGate("openspec", c, cmdStr, res.Output, 1, time.Since(t1))
				}
			}
		}

		wtRepo, err := gitx.Open(req.Worktree)
		var childPaths []string
		if err == nil {
			childPaths, _ = wtRepo.ChildPaths()
		}

		var touchedRepos []string
		for _, cp := range childPaths {
			touched := false
			for p := range changedMap {
				if p == cp || strings.HasPrefix(p, cp+"/") {
					touched = true
					break
				}
			}
			if !touched && wtRepo != nil {
				h1, err1 := wtRepo.TreeEntryHash(baseRef, cp)
				h2, err2 := wtRepo.TreeEntryHash("HEAD", cp)
				if err1 == nil && err2 == nil && h1 != h2 {
					touched = true
				}
			}
			if touched {
				touchedRepos = append(touchedRepos, cp)
			}
		}

		for _, cp := range touchedRepos {
			name := filepath.Base(cp)
			repoPaths[name] = cp
			childDir := filepath.Join(req.Worktree, filepath.FromSlash(cp))
			// A worktree cut before the keys of the file were renamed reads the file of the main
			// checkout, where foreman-kit update rewrote it.
			rawCfg, cfgPath, err := config.ReadRepoConfig("", childDir)
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(&buf, "---- [warn] %s: no repo.config.yaml, no gates defined\n", cp)
				continue
			}
			if err != nil {
				fmt.Fprintf(&buf, "---- [FAIL] %s: %v\n", cp, err)
				failed = append(failed, name+":config")
				failures = append(failures, GateFailure{Child: cp, Gate: "config", ExitCode: 1})
				continue
			}

			parsedCfg, err := parseChildGateConfig(rawCfg)
			if err != nil {
				fmt.Fprintf(&buf, "---- [FAIL] %s: invalid repo.config.yaml: %v\n", cp, err)
				failed = append(failed, name+":config")
				failures = append(failures, GateFailure{Child: cp, Gate: "config", ExitCode: 1})
				continue
			}

			var childChangedFiles []string
			if wtRepo != nil {
				childBaseSHA, _ := wtRepo.TreeEntryHash(baseRef, cp)
				childBaseSHA = childDiffBase(runCtx, &buf, childDir, cp, childBaseSHA, req.ChildOnto[cp], req.OutsideCommits)
				if childBaseSHA != "" {
					// -z: paths unquoted (plain output C-quotes non-ASCII names, which then match no
					// regression.when glob); --no-renames: both sides of a rename, as in diffNames.
					if out, err := gitx.RunGit(runCtx, childDir, "diff", "--name-only", "-z", "--no-renames", childBaseSHA, "--"); err == nil {
						for _, f := range strings.Split(out, "\x00") {
							if f != "" {
								childChangedFiles = append(childChangedFiles, f)
							}
						}
					}
				}
			}
			if out, err := gitx.RunGit(runCtx, childDir, "ls-files", "-z", "--others", "--exclude-standard"); err == nil {
				for _, f := range strings.Split(out, "\x00") {
					if f != "" {
						childChangedFiles = append(childChangedFiles, f)
					}
				}
			}
			sort.Strings(childChangedFiles)

			type gateEntry struct {
				key string
				cmd string
			}
			var gatesToRun []gateEntry
			for _, gateKey := range parsedCfg.orderedGateKeys() {
				gatesToRun = append(gatesToRun, gateEntry{key: gateKey, cmd: strings.TrimSpace(parsedCfg.Gates[gateKey])})
			}

			// Evaluate regression.when (exclusions applied) against childChangedFiles
			if len(parsedCfg.Regression) > 0 && len(parsedCfg.RegressionWhen) > 0 {
				match := proc.MatchRegressionWhen(parsedCfg.RegressionWhen, childChangedFiles)
				if note := match.ExcludedNote(cp); note != "" {
					fmt.Fprintln(&buf, note)
				}
				if match.Triggered() {
					fmt.Fprintf(&buf, "---- [info] %s: %s matches regression.when: the regression runs after the gates\n", cp, match.Hit)
					var regKeys []string
					for k := range parsedCfg.Regression {
						regKeys = append(regKeys, k)
					}
					sort.Strings(regKeys)
					for _, rk := range regKeys {
						gatesToRun = append(gatesToRun, gateEntry{key: "regression:" + rk, cmd: strings.TrimSpace(parsedCfg.Regression[rk])})
					}
				}
			}

			if es := parsedCfg.envScriptRel(); es != "" {
				envScriptPath := filepath.Join(childDir, filepath.FromSlash(es))
				if !fileExists(envScriptPath) {
					msg := fmt.Sprintf("repo_exec: env_script %s of %s does not exist\n", es, cfgPath)
					fmt.Fprint(&buf, msg)
					recordGate(name, "env_script", es, msg, 2, 0)
					failures = append(failures, GateFailure{Child: cp, Gate: "env_script", ExitCode: 2})
					return GateRunResult{Output: buf.String(), ExitCode: 2, OK: false, Failures: failures}, nil
				}
			}

			runner := childGateRun{
				Worktree:  req.Worktree,
				ChildDir:  childDir,
				EnvScript: parsedCfg.envScriptRel(),
				Slot:      req.Slot,
				Changed:   childChangedFiles,
				Env:       req.Env,
				Timeout:   timeout,
				Store:     st,
			}
			func() {
				for _, g := range gatesToRun {
					gateKey := g.key
					cmdStr := g.cmd
					if cmdStr == "" {
						continue
					}
					if strings.Contains(cmdStr, "{{") {
						fmt.Fprintf(&buf, "---- [warn] %s:%s not instantiated (%s); skipped\n", cp, gateKey, cmdStr)
						continue
					}
					if strings.HasPrefix(gateKey, "regression:") && !runner.RegressionLockHeld {
						// The regression commands come last: hold heavyRegressionMu for all of them, and
						// give back to the deadline the time spent waiting for it, which says nothing
						// about the change.
						waitStart := time.Now()
						heavyRegressionMu.Lock()
						runner.RegressionLockHeld = true
						defer heavyRegressionMu.Unlock()
						if waited := time.Since(waitStart); waited > 0 && ctx.Err() == nil {
							cancel()
							deadline = deadline.Add(waited)
							runCtx, cancel = context.WithDeadline(ctx, deadline)
						}
					}
					label := name + ":" + gateKey
					fmt.Fprintf(&buf, "---- [gate] %s: %s\n", label, cmdStr)
					rawOut, rc, ok, dur := runner.run(runCtx, gateKey, cmdStr)
					buf.WriteString(rawOut)
					if ok {
						fmt.Fprintf(&buf, "---- [pass] %s\n", label)
						passed++
						recordGate(name, gateKey, cmdStr, rawOut, 0, dur)
					} else {
						fmt.Fprintf(&buf, "---- [FAIL] %s (exit %d)\n", label, rc)
						failed = append(failed, label)
						failures = append(failures, GateFailure{Child: cp, Gate: gateKey, ExitCode: rc})
						recordGate(name, gateKey, cmdStr, rawOut, rc, dur)
					}
				}
			}()
		}

		fmt.Fprintln(&buf, "========================================================================")
		if len(failed) == 0 {
			fmt.Fprintf(&buf, "gates: PASS (%d executed)\n", passed)
			return GateRunResult{Output: buf.String(), ExitCode: 0, OK: true}, nil
		}
		if len(failureExcerpts) > 0 {
			fmt.Fprintln(&buf, "=== FAILED GATES EXCERPT ===")
			fmt.Fprintln(&buf, strings.Join(failureExcerpts, "\n\n"))
		}
		fmt.Fprintf(&buf, "gates: FAIL (%s)\n", strings.Join(failed, " "))
		return GateRunResult{Output: buf.String(), ExitCode: 1, OK: false, Failures: failures}, nil
	}
}

// childDiffBase returns the commit a child repository's changed files (FOREMANKIT_CHANGED_FILES, and the
// files regression.when matches) are measured from: pointer, the gitlink the base ref records. At merge,
// onto is the commit the merger rebased the change onto; when it holds commits pointer does not, they
// were pushed to the repository outside the pipeline (a hotfix), and merge.outside_commits decides:
// "ignore" and "verify" measure from onto, so they never count as the change's; anything else keeps
// pointer.
// The gate output says which, so an operator sees why a hotfix did or did not widen the tests.
func childDiffBase(ctx context.Context, out *bytes.Buffer, childDir, cp, pointer, onto, mode string) string {
	if pointer == "" || onto == "" || onto == pointer {
		return pointer
	}
	countOut, err := gitx.RunGit(ctx, childDir, "rev-list", "--count", pointer+".."+onto)
	if err != nil {
		return pointer
	}
	n, err := strconv.Atoi(strings.TrimSpace(countOut))
	if err != nil || n <= 0 {
		return pointer
	}
	if mode == config.OutsideCommitsIgnore {
		fmt.Fprintf(out, "---- [info] %s: %d commit(s) pushed to the repository outside the pipeline enter the umbrella with this change; %s is %s, so only the change's own commits count as changed (FOREMANKIT_CHANGED_FILES, regression.when)\n", cp, n, settings.OutsideCommits.Path(), mode)
		return onto
	}
	if mode == config.OutsideCommitsVerify {
		fmt.Fprintf(out, "---- [info] %s: %d commit(s) pushed to the repository outside the pipeline enter the umbrella with this change; %s is %s, so only the change's own commits count as changed (FOREMANKIT_CHANGED_FILES, regression.when) and the merger tests the outside commits on the base alone\n", cp, n, settings.OutsideCommits.Path(), mode)
		return onto
	}
	fmt.Fprintf(out, "---- [info] %s: %d commit(s) pushed to the repository outside the pipeline enter the umbrella with this change; %s is %s, so their files count as changed too (FOREMANKIT_CHANGED_FILES, regression.when)\n", cp, n, settings.OutsideCommits.Path(), config.OutsideCommitsInclude)
	return pointer
}

func matchRepoGateGlob(pattern, target string) bool {
	return proc.MatchRegressionGlob(pattern, target)
}

// openSpecCLIWaitDelay is how long DefaultOpenSpecRunner waits for the output of an `openspec` CLI that
// has exited, or that was told to stop, before it stops reading.
var openSpecCLIWaitDelay = 10 * time.Second

// DefaultOpenSpecRunner executes `openspec` CLI if installed and configured in dir,
// or falls back to the built-in Go OpenSpec implementation so tests and standalone environments work reliably.
func DefaultOpenSpecRunner() OpenSpecRunnerFunc {
	return func(ctx context.Context, dir string, args ...string) (OpenSpecResult, error) {
		if os.Getenv("FOREMANKIT_FORCE_BUILTIN") != "1" {
			if bin, err := exec.LookPath("openspec"); err == nil && fileExists(filepath.Join(dir, "openspec", "config.yaml")) {
				cmd := exec.CommandContext(ctx, bin, args...)
				cmd.Dir = dir
				// The CLI is a Node program: a helper it leaves running with its output inherited keeps
				// the pipe open after the CLI has exited, and CombinedOutput then waits for that helper
				// for as long as it lives, also once ctx has ended. WaitDelay bounds that wait; a CLI
				// that itself exited 0 has succeeded.
				cmd.WaitDelay = openSpecCLIWaitDelay
				out, err := cmd.CombinedOutput()
				if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
					err = nil
				}
				if err == nil {
					return OpenSpecResult{Output: string(out), ExitCode: 0, OK: true}, nil
				}
				// If openspec CLI ran and returned structured output, return it unless builtin is needed
				if len(out) > 0 {
					code := 1
					if exitErr, ok := err.(*exec.ExitError); ok {
						code = exitErr.ExitCode()
					}
					return OpenSpecResult{Output: string(out), ExitCode: code, OK: false}, nil
				}
			}
		}
		return BuiltinOpenSpec(ctx, dir, args...)
	}
}

// BuiltinOpenSpec implements the deterministic OpenSpec operations (`new change`, `status`, `validate`, `list`, `archive`)
// in pure Go so that the orchestrator works even when the external `openspec` Node CLI is not installed.
func BuiltinOpenSpec(_ context.Context, dir string, args ...string) (OpenSpecResult, error) {
	if len(args) == 0 {
		return OpenSpecResult{Output: "missing subcommand", ExitCode: 1, OK: false}, nil
	}
	switch args[0] {
	case "new":
		// openspec new change <name> [--json] [--description ...]
		if len(args) < 3 || args[1] != "change" {
			return OpenSpecResult{Output: "usage: openspec new change <name>", ExitCode: 1, OK: false}, nil
		}
		change := args[2]
		chgDir := filepath.Join(dir, "openspec", "changes", change)
		if err := os.MkdirAll(filepath.Join(chgDir, "specs"), 0o755); err != nil {
			return OpenSpecResult{Output: err.Error(), ExitCode: 1, OK: false}, err
		}
		_ = os.WriteFile(filepath.Join(chgDir, ".openspec.yaml"), []byte("schema: spec-driven\n"), 0o644)
		b, _ := json.Marshal(map[string]any{
			"change": map[string]any{"id": change, "path": chgDir},
		})
		return OpenSpecResult{Output: string(b), ExitCode: 0, OK: true}, nil

	case "status":
		// openspec status --change <name> --json
		var change string
		for i := 1; i < len(args); i++ {
			if args[i] == "--change" && i+1 < len(args) {
				change = args[i+1]
				i++
			}
		}
		chgDir := filepath.Join(dir, "openspec", "changes", change)
		statusOf := func(ok bool) string {
			if ok {
				return "done"
			}
			return "ready"
		}
		hasProposal := nonEmptyFile(filepath.Join(chgDir, "proposal.md"))
		hasDesign := nonEmptyFile(filepath.Join(chgDir, "design.md"))
		hasTasks := nonEmptyFile(filepath.Join(chgDir, "tasks.md"))
		hasSpecs := len(listSpecFiles(filepath.Join(chgDir, "specs"))) > 0
		payload := map[string]any{
			"changeName": change,
			"artifacts": []map[string]any{
				{"id": "proposal", "status": statusOf(hasProposal)},
				{"id": "specs", "status": statusOf(hasSpecs)},
				{"id": "design", "status": statusOf(hasDesign)},
				{"id": "tasks", "status": statusOf(hasTasks)},
			},
		}
		b, _ := json.Marshal(payload)
		return OpenSpecResult{Output: string(b), ExitCode: 0, OK: true}, nil

	case "validate":
		// either: openspec validate --specs --strict --json
		// or:     openspec validate <change> --strict --json
		isSpecs := false
		var targetChange string
		for _, a := range args[1:] {
			if a == "--specs" {
				isSpecs = true
			} else if !strings.HasPrefix(a, "-") && targetChange == "" {
				targetChange = a
			}
		}
		if isSpecs {
			specsDir := filepath.Join(dir, "openspec", "specs")
			specFiles := listSpecFiles(specsDir)
			var items []map[string]any
			allValid := true
			for _, sf := range specFiles {
				rel, _ := filepath.Rel(specsDir, filepath.Dir(sf))
				capID := filepath.ToSlash(rel)
				valid, msg := validateSpecMarkdown(sf, true)
				if !valid {
					allValid = false
				}
				items = append(items, map[string]any{"id": capID, "valid": valid, "message": msg})
			}
			b, _ := json.Marshal(map[string]any{"valid": allValid, "items": items})
			code := 0
			if !allValid {
				code = 1
			}
			return OpenSpecResult{Output: string(b), ExitCode: code, OK: allValid}, nil
		}

		chgDir := filepath.Join(dir, "openspec", "changes", targetChange)
		if !nonEmptyFile(filepath.Join(chgDir, "proposal.md")) {
			return OpenSpecResult{Output: `{"valid":false,"error":"missing proposal.md"}`, ExitCode: 1, OK: false}, nil
		}
		if !nonEmptyFile(filepath.Join(chgDir, "design.md")) {
			return OpenSpecResult{Output: `{"valid":false,"error":"missing design.md"}`, ExitCode: 1, OK: false}, nil
		}
		if !nonEmptyFile(filepath.Join(chgDir, "tasks.md")) {
			return OpenSpecResult{Output: `{"valid":false,"error":"missing tasks.md"}`, ExitCode: 1, OK: false}, nil
		}
		specFiles := listSpecFiles(filepath.Join(chgDir, "specs"))
		if len(specFiles) == 0 {
			return OpenSpecResult{Output: `{"valid":false,"error":"missing delta specs under specs/"}`, ExitCode: 1, OK: false}, nil
		}
		for _, sf := range specFiles {
			rel, _ := filepath.Rel(filepath.Join(chgDir, "specs"), filepath.Dir(sf))
			capID := filepath.ToSlash(rel)
			if ok, msg := validateSpecMarkdown(sf, false); !ok {
				b, _ := json.Marshal(map[string]any{"valid": false, "capability": capID, "error": msg})
				return OpenSpecResult{Output: string(b), ExitCode: 1, OK: false}, nil
			}
			canonicalPath := filepath.Join(dir, "openspec", "specs", filepath.FromSlash(capID), "spec.md")
			if ok, msg := validateDeltaSpecAgainstCanonical(sf, canonicalPath); !ok {
				b, _ := json.Marshal(map[string]any{"valid": false, "capability": capID, "error": msg})
				return OpenSpecResult{Output: string(b), ExitCode: 1, OK: false}, nil
			}
		}
		return OpenSpecResult{Output: `{"valid":true}`, ExitCode: 0, OK: true}, nil

	case "list":
		// openspec list --json
		changesRoot := filepath.Join(dir, "openspec", "changes")
		entries, _ := os.ReadDir(changesRoot)
		var changes []map[string]any
		for _, e := range entries {
			if !e.IsDir() || e.Name() == "archive" {
				continue
			}
			completed, total := CountTasksInFile(filepath.Join(changesRoot, e.Name(), "tasks.md"))
			changes = append(changes, map[string]any{
				"name":           e.Name(),
				"completedTasks": completed,
				"totalTasks":     total,
			})
		}
		b, _ := json.Marshal(map[string]any{"changes": changes})
		return OpenSpecResult{Output: string(b), ExitCode: 0, OK: true}, nil

	case "archive":
		// openspec archive <change> --yes --json
		if len(args) < 2 {
			return OpenSpecResult{Output: "missing change name", ExitCode: 1, OK: false}, nil
		}
		change := args[1]
		chgDir := filepath.Join(dir, "openspec", "changes", change)
		if st, err := os.Stat(chgDir); err != nil || !st.IsDir() {
			return OpenSpecResult{Output: `{"error":"change directory not found"}`, ExitCode: 1, OK: false}, nil
		}
		deltaRoot := filepath.Join(chgDir, "specs")
		specFiles := listSpecFiles(deltaRoot)
		added, modified, removed, renamed := 0, 0, 0, 0
		for _, sf := range specFiles {
			rel, _ := filepath.Rel(deltaRoot, filepath.Dir(sf))
			dstFile := filepath.Join(dir, "openspec", "specs", rel, "spec.md")
			deltaBytes, err := os.ReadFile(sf)
			if err != nil {
				return OpenSpecResult{Output: err.Error(), ExitCode: 1, OK: false}, nil
			}
			if err := os.MkdirAll(filepath.Dir(dstFile), 0o755); err != nil {
				return OpenSpecResult{Output: err.Error(), ExitCode: 1, OK: false}, nil
			}
			if existing, err := os.ReadFile(dstFile); err == nil && len(existing) > 0 {
				merged, addCnt, modCnt, remCnt, renCnt, err := applySpecDelta(string(existing), string(deltaBytes))
				if err != nil {
					b, _ := json.Marshal(map[string]any{
						"error":  err.Error(),
						"status": []map[string]any{{"code": "archive_spec_update_failed", "message": err.Error()}},
					})
					return OpenSpecResult{Output: string(b), ExitCode: 1, OK: false}, nil
				}
				added += addCnt
				modified += modCnt
				removed += remCnt
				renamed += renCnt
				// A canonical spec that cannot be written fails the archive: the change must not be
				// moved to the archive and integrated without its requirements in openspec/specs/.
				if err := os.WriteFile(dstFile, []byte(merged), 0o644); err != nil {
					return OpenSpecResult{Output: err.Error(), ExitCode: 1, OK: false}, nil
				}
			} else {
				deltaStr := string(deltaBytes)
				deltaBySec, _ := parseSpecRequirementsBySection(deltaStr)
				addReqs := deltaBySec["ADDED Requirements"]
				modReqs := deltaBySec["MODIFIED Requirements"]
				remReqs := deltaBySec["REMOVED Requirements"]
				renReqs := parseRenamedRequirements(deltaStr)
				if len(addReqs) == 0 && (len(modReqs) > 0 || len(remReqs) > 0 || len(renReqs) > 0) {
					msg := "cannot apply non-ADDED delta requirements to non-existent canonical spec"
					b, _ := json.Marshal(map[string]any{
						"error":  msg,
						"status": []map[string]any{{"code": "archive_spec_update_failed", "message": msg}},
					})
					return OpenSpecResult{Output: string(b), ExitCode: 1, OK: false}, nil
				}
				if len(addReqs) > 0 {
					added += len(addReqs)
				} else {
					added++
				}
				normalized := strings.Replace(deltaStr, "\n## ADDED Requirements\n", "\n## Requirements\n", 1)
				if strings.HasPrefix(normalized, "## ADDED Requirements\n") {
					normalized = "## Requirements\n" + strings.TrimPrefix(normalized, "## ADDED Requirements\n")
				}
				if err := os.WriteFile(dstFile, []byte(normalized), 0o644); err != nil {
					return OpenSpecResult{Output: err.Error(), ExitCode: 1, OK: false}, nil
				}
			}
		}
		archiveName := time.Now().UTC().Format("2006-01-02") + "-" + change
		archiveDir := filepath.Join(dir, "openspec", "changes", "archive", archiveName)
		if err := os.MkdirAll(filepath.Dir(archiveDir), 0o755); err != nil {
			return OpenSpecResult{Output: err.Error(), ExitCode: 1, OK: false}, nil
		}
		_ = os.RemoveAll(archiveDir)
		if err := os.Rename(chgDir, archiveDir); err != nil {
			return OpenSpecResult{Output: err.Error(), ExitCode: 1, OK: false}, nil
		}
		payload := map[string]any{
			"archive": map[string]any{
				"archivedAs": "openspec/changes/archive/" + archiveName,
				"totals": map[string]any{
					"added":    added,
					"modified": modified,
					"removed":  removed,
					"renamed":  renamed,
				},
			},
		}
		b, _ := json.Marshal(payload)
		return OpenSpecResult{Output: string(b), ExitCode: 0, OK: true}, nil
	}
	return OpenSpecResult{Output: "unsupported openspec command", ExitCode: 1, OK: false}, nil
}

// CountTasksInFile counts completed (`[x]`/`[X]`) and total task checkboxes in tasks.md.
func CountTasksInFile(tasksPath string) (completed int, total int) {
	completed, total, _ = UncheckedTasksDetails(tasksPath, "")
	return completed, total
}

// UncheckedTasksDetails parses tasks.md and returns completed count, total count, and a detailed
// multi-line report listing exact 1-based line numbers and text of every unchecked task.
func UncheckedTasksDetails(tasksPath, change string) (completed int, total int, details string) {
	raw, err := os.ReadFile(tasksPath)
	if err != nil {
		return 0, 0, "Tasks complete: 0/0 (tasks.md missing or unreadable)"
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var uncheckedLines []string
	for idx, line := range lines {
		if checkedTaskRe.MatchString(line) {
			completed++
		} else if uncheckedTaskRe.MatchString(line) {
			if len(uncheckedLines) < 25 {
				uncheckedLines = append(uncheckedLines, fmt.Sprintf("  - Line %d: %s", idx+1, strings.TrimSpace(line)))
			}
			total++
		}
	}
	total += completed
	if len(uncheckedLines) == 0 {
		return completed, total, fmt.Sprintf("Tasks complete: %d/%d", completed, total)
	}
	relPath := "tasks.md"
	if change != "" {
		relPath = "openspec/changes/" + change + "/tasks.md"
	}
	return completed, total, fmt.Sprintf(
		"Tasks complete: %d/%d. The following task items in %s are still unchecked ([ ]):\n%s\nVerify each unchecked task, implement anything missing, and mark `- [x]` in %s.",
		completed, total, relPath, strings.Join(uncheckedLines, "\n"), relPath,
	)
}

// ValidateOpenSpecNative runs fast (<2ms) native Go checks on a change's delta specs:
// 1. Every `### Requirement:` paragraph must be <= 500 runes (which `openspec validate --specs` enforces only after archive).
// 2. Every new capability (not yet in openspec/specs/<cap>/spec.md) must have a `## Purpose` >= 50 chars without TBD.
// 3. Scenarios must use 4 hashes (`#### Scenario:`), never 3 (`### Scenario:`).
func ValidateOpenSpecNative(wt, change string) (bool, string) {
	deltaRoot := filepath.Join(wt, "openspec", "changes", change, "specs")
	specFiles := listSpecFiles(deltaRoot)
	for _, sf := range specFiles {
		rel, _ := filepath.Rel(deltaRoot, filepath.Dir(sf))
		capID := filepath.ToSlash(rel)
		canonPath := filepath.Join(wt, "openspec", "specs", filepath.FromSlash(capID), "spec.md")
		isNewCap := !nonEmptyFile(canonPath)
		if ok, msg := validateSpecMarkdown(sf, isNewCap); !ok {
			return false, fmt.Sprintf("capability %s: %s", capID, msg)
		}
	}
	return true, ""
}

func validateSpecMarkdown(path string, requirePurpose bool) (bool, string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err.Error()
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	bySec, _ := parseSpecRequirementsBySection(text)
	renReqs := parseRenamedRequirements(text)
	addReqs := bySec["ADDED Requirements"]
	modReqs := bySec["MODIFIED Requirements"]
	remReqs := bySec["REMOVED Requirements"]
	canonReqs := bySec["Requirements"]
	unsecReqs := bySec[""]

	// Only require ## Purpose for canonical specs or delta specs introducing ADDED requirements on a new capability
	isDeltaWithoutAdd := len(addReqs) == 0 && len(canonReqs) == 0 && len(unsecReqs) == 0 && (len(modReqs)+len(remReqs)+len(renReqs) > 0)
	if requirePurpose && !isDeltaWithoutAdd {
		idx := strings.Index(text, "## Purpose")
		if idx < 0 {
			return false, "missing '## Purpose' section (at least 50 characters required)"
		}
		// The purpose ends at the next section. Look for it before trimming: with nothing under the
		// heading, the next section started the trimmed text, was not seen as a heading, and its
		// own text was measured as the purpose.
		rest := text[idx+len("## Purpose"):]
		if nextHdr := strings.Index(rest, "\n## "); nextHdr >= 0 {
			rest = rest[:nextHdr]
		}
		rest = strings.TrimSpace(rest)
		if strings.HasPrefix(strings.ToUpper(rest), "TBD") {
			return false, "section '## Purpose' cannot be a TBD placeholder"
		}
		if utf8.RuneCountInString(rest) < 50 {
			return false, "section '## Purpose' must be at least 50 characters"
		}
	}
	if strings.Contains(text, "\n### Scenario:") {
		return false, "scenarios must use 4 hashes ('#### Scenario:'), found '### Scenario:'"
	}

	totalOps := len(addReqs) + len(modReqs) + len(remReqs) + len(renReqs) + len(canonReqs) + len(unsecReqs)
	if totalOps == 0 {
		return false, "missing '### Requirement:' block"
	}

	// Require #### Scenario: only for canonical, ADDED, or MODIFIED requirements (never REMOVED or RENAMED)
	for _, group := range [][]parsedRequirementBlock{addReqs, modReqs, canonReqs, unsecReqs} {
		for _, r := range group {
			if len(r.Scenarios) == 0 {
				return false, "missing '#### Scenario:' block"
			}
		}
	}

	// Verify each requirement's descriptive paragraph is <= 500 characters
	lines := strings.Split(text, "\n")
	curReq := ""
	inReqDesc := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "### Requirement:") {
			curReq = strings.TrimSpace(strings.TrimPrefix(trimmed, "### Requirement:"))
			inReqDesc = true
			continue
		}
		if strings.HasPrefix(trimmed, "#### Scenario:") || strings.HasPrefix(trimmed, "## ") {
			inReqDesc = false
			continue
		}
		if inReqDesc && trimmed != "" {
			if n := utf8.RuneCountInString(trimmed); n > 500 {
				return false, fmt.Sprintf("requirement %q description exceeds 500 characters (%d chars)", curReq, n)
			}
		}
	}
	return true, ""
}

type parsedRequirementBlock struct {
	Name      string
	RawBlock  string
	Scenarios []string
}

type renamedRequirement struct {
	From string
	To   string
}

func cleanRequirementHeaderName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`")
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "### Requirement:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "### Requirement:"))
	}
	return strings.Trim(s, "` ")
}

func parseRenamedRequirements(text string) []renamedRequirement {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	curSection := ""
	pendingFrom := ""
	var out []renamedRequirement

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			curSection = strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			pendingFrom = ""
			continue
		}
		if curSection != "RENAMED Requirements" || trimmed == "" {
			continue
		}
		item := strings.TrimSpace(strings.TrimLeft(trimmed, "-*+"))
		upper := strings.ToUpper(item)
		if strings.HasPrefix(upper, "FROM:") {
			pendingFrom = cleanRequirementHeaderName(item[len("FROM:"):])
			continue
		}
		if strings.HasPrefix(upper, "TO:") {
			toName := cleanRequirementHeaderName(item[len("TO:"):])
			if pendingFrom != "" && toName != "" {
				out = append(out, renamedRequirement{From: pendingFrom, To: toName})
				pendingFrom = ""
			}
			continue
		}
		if strings.Contains(item, "->") {
			parts := strings.SplitN(item, "->", 2)
			fromName := cleanRequirementHeaderName(parts[0])
			toName := cleanRequirementHeaderName(parts[1])
			if fromName != "" && toName != "" {
				out = append(out, renamedRequirement{From: fromName, To: toName})
			}
		}
	}
	return out
}

func parseSpecRequirementsBySection(text string) (bySection map[string][]parsedRequirementBlock, allReqs map[string]parsedRequirementBlock) {
	bySection = make(map[string][]parsedRequirementBlock)
	allReqs = make(map[string]parsedRequirementBlock)

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	curSection := ""
	curReqName := ""
	var curReqLines []string
	var curScenarios []string

	flushReq := func() {
		if curReqName == "" {
			return
		}
		blk := parsedRequirementBlock{
			Name:      curReqName,
			RawBlock:  strings.TrimSpace(strings.Join(curReqLines, "\n")),
			Scenarios: curScenarios,
		}
		bySection[curSection] = append(bySection[curSection], blk)
		allReqs[curReqName] = blk
		curReqName = ""
		curReqLines = nil
		curScenarios = nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			flushReq()
			curSection = strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			continue
		}
		if strings.HasPrefix(trimmed, "### Requirement:") {
			flushReq()
			curReqName = strings.TrimSpace(strings.TrimPrefix(trimmed, "### Requirement:"))
			curReqLines = []string{line}
			continue
		}
		if curReqName != "" {
			curReqLines = append(curReqLines, line)
			if strings.HasPrefix(trimmed, "#### Scenario:") {
				scen := strings.TrimSpace(strings.TrimPrefix(trimmed, "#### Scenario:"))
				if scen != "" {
					curScenarios = append(curScenarios, scen)
				}
			}
		}
	}
	flushReq()
	return bySection, allReqs
}

func validateDeltaSpecAgainstCanonical(deltaPath, canonicalPath string) (bool, string) {
	deltaRaw, err := os.ReadFile(deltaPath)
	if err != nil {
		return false, err.Error()
	}
	deltaText := string(deltaRaw)
	deltaBySec, _ := parseSpecRequirementsBySection(deltaText)
	modReqs := deltaBySec["MODIFIED Requirements"]
	remReqs := deltaBySec["REMOVED Requirements"]
	renReqs := parseRenamedRequirements(deltaText)
	if len(modReqs) == 0 && len(remReqs) == 0 && len(renReqs) == 0 {
		return true, ""
	}
	canonRaw, err := os.ReadFile(canonicalPath)
	if err != nil || len(canonRaw) == 0 {
		return false, "cannot apply MODIFIED/REMOVED/RENAMED Requirements to non-existent canonical spec"
	}
	_, canonReqs := parseSpecRequirementsBySection(string(canonRaw))
	for _, mr := range modReqs {
		cr, ok := canonReqs[mr.Name]
		if !ok {
			return false, fmt.Sprintf("MODIFIED requirement %q does not exist in canonical spec", mr.Name)
		}
		modScenSet := make(map[string]bool, len(mr.Scenarios))
		for _, s := range mr.Scenarios {
			modScenSet[s] = true
		}
		for _, cs := range cr.Scenarios {
			if !modScenSet[cs] {
				return false, fmt.Sprintf("MODIFIED requirement %q is stale: missing canonical scenario %q", mr.Name, cs)
			}
		}
	}
	for _, rr := range remReqs {
		if _, ok := canonReqs[rr.Name]; !ok {
			return false, fmt.Sprintf("REMOVED requirement %q does not exist in canonical spec", rr.Name)
		}
	}
	for _, ren := range renReqs {
		if _, ok := canonReqs[ren.From]; !ok {
			return false, fmt.Sprintf("RENAMED requirement %q does not exist in canonical spec", ren.From)
		}
	}
	return true, ""
}

func applyDeltaSpecToCanonical(canonicalText, deltaText string) (string, error) {
	merged, _, _, _, _, err := applySpecDelta(canonicalText, deltaText)
	return merged, err
}

func applySpecDelta(canonicalText, deltaText string) (string, int, int, int, int, error) {
	deltaBySec, _ := parseSpecRequirementsBySection(deltaText)
	modReqs := deltaBySec["MODIFIED Requirements"]
	addReqs := deltaBySec["ADDED Requirements"]
	remReqs := deltaBySec["REMOVED Requirements"]
	renReqs := parseRenamedRequirements(deltaText)

	_, canonReqs := parseSpecRequirementsBySection(canonicalText)
	for _, mr := range modReqs {
		cr, ok := canonReqs[mr.Name]
		if !ok {
			return "", 0, 0, 0, 0, fmt.Errorf("MODIFIED requirement %q does not exist in canonical spec", mr.Name)
		}
		modScenSet := make(map[string]bool, len(mr.Scenarios))
		for _, s := range mr.Scenarios {
			modScenSet[s] = true
		}
		for _, cs := range cr.Scenarios {
			if !modScenSet[cs] {
				return "", 0, 0, 0, 0, fmt.Errorf("MODIFIED requirement %q is stale: missing canonical scenario %q", mr.Name, cs)
			}
		}
	}
	for _, rr := range remReqs {
		if _, ok := canonReqs[rr.Name]; !ok {
			return "", 0, 0, 0, 0, fmt.Errorf("REMOVED requirement %q does not exist in canonical spec", rr.Name)
		}
	}
	for _, ren := range renReqs {
		if _, ok := canonReqs[ren.From]; !ok {
			return "", 0, 0, 0, 0, fmt.Errorf("RENAMED requirement %q does not exist in canonical spec", ren.From)
		}
	}

	result := strings.ReplaceAll(canonicalText, "\r\n", "\n")
	added, modified, removed, renamed := 0, 0, 0, 0

	for _, mr := range modReqs {
		cr := canonReqs[mr.Name]
		if cr.RawBlock == "" || !strings.Contains(result, cr.RawBlock) {
			return "", 0, 0, 0, 0, fmt.Errorf("MODIFIED requirement %q block not found in canonical spec", mr.Name)
		}
		result = strings.Replace(result, cr.RawBlock, mr.RawBlock, 1)
		modified++
	}

	for _, rr := range remReqs {
		cr := canonReqs[rr.Name]
		if cr.RawBlock != "" && strings.Contains(result, cr.RawBlock) {
			if strings.Contains(result, "\n\n"+cr.RawBlock) {
				result = strings.Replace(result, "\n\n"+cr.RawBlock, "", 1)
			} else if strings.Contains(result, cr.RawBlock+"\n\n") {
				result = strings.Replace(result, cr.RawBlock+"\n\n", "", 1)
			} else {
				result = strings.Replace(result, cr.RawBlock, "", 1)
			}
			removed++
		}
	}

	for _, ren := range renReqs {
		oldHdr := "### Requirement: " + ren.From
		newHdr := "### Requirement: " + ren.To
		if strings.Contains(result, oldHdr+"\n") {
			result = strings.Replace(result, oldHdr+"\n", newHdr+"\n", 1)
			renamed++
		} else if strings.HasSuffix(result, oldHdr) {
			result = strings.TrimSuffix(result, oldHdr) + newHdr
			renamed++
		}
	}

	for _, ar := range addReqs {
		if existing, ok := canonReqs[ar.Name]; ok && existing.RawBlock != "" && strings.Contains(result, existing.RawBlock) {
			result = strings.Replace(result, existing.RawBlock, ar.RawBlock, 1)
		} else {
			result = strings.TrimRight(result, "\n") + "\n\n" + ar.RawBlock + "\n"
		}
		added++
	}

	result = strings.TrimRight(result, "\n") + "\n"
	return result, added, modified, removed, renamed, nil
}

func listSpecFiles(specsDir string) []string {
	var out []string
	_ = filepath.WalkDir(specsDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "spec.md" {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func buildWorkerEnv(slot int, scratchDir string, extra map[string]string) []string {
	stripped := map[string]bool{
		"SSH_AUTH_SOCK": true,
		"GH_TOKEN":      true,
		"GITHUB_TOKEN":  true,
		"GITLAB_TOKEN":  true,
		"GIT_ASKPASS":   true,
	}
	if scratchDir != "" {
		stripped["TMPDIR"] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		k := strings.SplitN(kv, "=", 2)[0]
		if stripped[k] {
			continue
		}
		env = append(env, kv)
	}
	if slot > 0 {
		// OPENSPEC_SLOT: the name a script written before the kit was called Foreman reads.
		env = append(env, fmt.Sprintf("FOREMANKIT_SLOT=%d", slot), fmt.Sprintf("OPENSPEC_SLOT=%d", slot))
	}
	if scratchDir != "" {
		env = append(env, "TMPDIR="+scratchDir)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func findScript(worktree, rel string) string {
	mainRoot := store.MainRoot(worktree)
	for _, base := range []string{worktree, mainRoot} {
		if base == "" {
			continue
		}
		p := filepath.Join(base, filepath.FromSlash(rel))
		if fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func nonEmptyFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}

func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\r\n")
	if s == "" || n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// RedactExcerpt returns the last maxLines/maxChars of text with secrets and URL credentials redacted.
func RedactExcerpt(text string, maxLines, maxChars int) string {
	if maxLines <= 0 {
		maxLines = 40
	}
	if maxChars <= 0 {
		maxChars = 4000
	}
	tail := tailLines(text, maxLines)
	// Redact before truncating: a cut inside "API_TOKEN=" or "://user:" would leave the secret unmatched.
	tail = secretRedactRe.ReplaceAllString(tail, "${1}[redacted]")
	tail = urlCredentialsRe.ReplaceAllString(tail, "${1}[redacted]@")
	if len(tail) > maxChars {
		cut := len(tail) - maxChars
		for cut < len(tail) && !utf8.RuneStart(tail[cut]) {
			cut++ // never start inside a multi-byte character
		}
		tail = "..." + tail[cut:]
	}
	return tail
}

// FencedCodeBlock wraps text in a Markdown code block whose backtick fence is longer than any backtick run inside text.
func FencedCodeBlock(text string) string {
	maxTicks := 2
	cur := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '`' {
			cur++
			if cur > maxTicks {
				maxTicks = cur
			}
		} else {
			cur = 0
		}
	}
	fence := strings.Repeat("`", maxTicks+1)
	return fmt.Sprintf("%stext\n%s\n%s\n", fence, text, fence)
}

// NewWorkerExecID mints the id of a worker's agent execution. The id is opaque: nothing reads the
// change or the phase back out of it, they are columns of the execution.
func NewWorkerExecID(change, phase string) string {
	return fmt.Sprintf("worker-%s-%s-%d", change, phase, time.Now().UTC().UnixNano())
}

// AgentExecutionStatus is the status an agent execution ended with: stopped when the context it ran
// under ended (a daemon stop, a lost lease), which says nothing about the agent.
func AgentExecutionStatus(ctx context.Context, exitCode int) string {
	switch {
	case ctx.Err() != nil:
		return "stopped"
	case exitCode != 0:
		return "failed"
	}
	return "succeeded"
}
