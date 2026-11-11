package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/store"
)

// Routing gates (state -> question -> action). Each one asks the evaluator about a state the orchestrator used to
// handle with one fixed reaction (pause the workspace, wait for a human, retry blindly) and takes the
// action the gate's `actions:` rules select. Without a decision (gate off or advisory, the evaluator unavailable,
// no rule matched) the fallback is that fixed reaction, so a gate can only improve on it.
const (
	SystemErrorGate     = "system_error_triage"
	PolicyViolationGate = "policy_violation_triage"
	AgentOutcomeGate    = "agent_outcome"
	SemanticMergeGate   = "semantic_merge_conflict"
	PushRejectionGate   = "push_rejection"
	FailureTriageGate   = "failure_triage"
)

// SemanticConflictMarker opens the rebuild feedback the merger sends for a semantic merge conflict.
const SemanticConflictMarker = "## Concurrent change contradicted"

// judgedSendBack reports whether rebuild details come from an evaluator gate rather than a rebase conflict: the
// rebase is then clean and the BUILD agent must be given the feedback explicitly.
func judgedSendBack(details string) bool {
	return strings.HasPrefix(details, WorkLossMarker) || strings.HasPrefix(details, SemanticConflictMarker)
}

// routeGate asks gate about state and returns the action to take with the verdict. The action is the
// gate's fallback unless the gate is enforced and one of its `actions:` rules matches.
func routeGate(ctx context.Context, judge *evaluator.Judge, st *store.Store, gate, change string, state any) (string, *evaluator.Verdict) {
	fallback := evaluator.FallbackAction(gate)
	if judge == nil {
		return fallback, nil
	}
	v, err := judge.Ask(ctx, gate, state, change)
	if err != nil || v == nil || len(v.Answers) == 0 {
		return fallback, v
	}
	suggested, rule := evaluator.ActionFor(*v, judge.Policy().Gates[gate])
	action := fallback
	if suggested != "" && v.Enforce() {
		action = suggested
	}
	if st != nil {
		payload := map[string]any{
			"gate":    gate,
			"action":  action,
			"mode":    v.Mode,
			"answers": evaluator.SummarizeAnswers(v.Answers),
		}
		if suggested != "" {
			payload["suggested"] = suggested
		}
		if rule != nil {
			payload["rule"] = evaluator.RuleLabel(*rule)
		}
		emitRoute(ctx, st, change, v, payload)
	}
	return action, v
}

// emitRoute emits evaluator.route for the action taken on a verdict and records that action on the
// verdict's evaluation, so the question, its answer and what the orchestrator did with it are one row.
func emitRoute(ctx context.Context, st *store.Store, change string, v *evaluator.Verdict, payload map[string]any) {
	if st == nil {
		return
	}
	_ = st.EmitEvent(ctx, store.EventInput{Event: "evaluator.route", Change: change, Payload: payload})
	if v != nil && v.EvaluationID != 0 {
		if action, _ := payload["action"].(string); action != "" {
			_ = st.SetEvaluationRoute(ctx, v.EvaluationID, action, payload)
		}
	}
}

// agentErrorLine is the agent's own last output line (not agent_exec's), which usually names the error.
func agentErrorLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" && !strings.HasPrefix(l, "agent_exec:") {
			return l
		}
	}
	return "every engine failed without changing anything"
}

func answerSummary(v *evaluator.Verdict) string {
	if v == nil || len(v.Answers) == 0 {
		return ""
	}
	keys := make([]string, 0, len(v.Answers))
	for k := range v.Answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	sum := evaluator.SummarizeAnswers(v.Answers)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, sum[k]))
	}
	return "evaluator " + v.Gate + ": " + strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------------------------------
// system_error_triage
// ---------------------------------------------------------------------------------------------------

// recentSystemErrors lists the other changes blocked by an orchestrator or transient error in the last
// hour, as "change: first line of the error".
func recentSystemErrors(ctx context.Context, st *store.Store, except string, now time.Time) []string {
	qs, err := st.ListQuarantine(ctx, "blocked")
	if err != nil {
		return nil
	}
	var out []string
	for _, q := range qs {
		if q.ChangeName == except || (q.Reason != "orchestrator_error" && q.Reason != "transient_error") {
			continue
		}
		if t, err := time.Parse(time.RFC3339, q.BlockedAt); err == nil && now.Sub(t) > time.Hour {
			continue
		}
		out = append(out, q.ChangeName+": "+strings.TrimSpace(strings.SplitN(q.Details, "\n", 2)[0]))
	}
	return out
}

// triageSystemError decides what an orchestrator_error of change at stage does: pause the workspace
// (the fallback), block only the change, or retry it later.
func triageSystemError(ctx context.Context, judge *evaluator.Judge, st *store.Store, change, stage, details string, now time.Time) (string, *evaluator.Verdict) {
	paused := false
	if ws, err := st.GetWorkspaceState(ctx); err == nil {
		paused = ws.Paused
	}
	state := map[string]any{
		"change":                change,
		"stage":                 stage,
		"error":                 tailLines(details, 60),
		"other_changes_failing": recentSystemErrors(ctx, st, change, now),
		"workspace_paused":      paused,
	}
	return routeGate(ctx, judge, st, SystemErrorGate, change, state)
}

// ---------------------------------------------------------------------------------------------------
// policy_violation_triage
// ---------------------------------------------------------------------------------------------------

// protectedRootPrefixes are umbrella paths a BUILD agent leaving changes in always goes back to the agent
// (they control the pipeline, the specs, or the memory): the evaluator never judges them.
var protectedRootPrefixes = []string{
	".github/", ".gitlab-ci.yml", ".gitmodules", ".agents/", ".foreman-kit/kit.lock",
	".foreman-kit/", "openspec/specs/", ".agent-memory/", "memory/", "MEMORY.md", "AGENTS.md", "CLAUDE.md", "README.md", "ROADMAP.md",
}

func isProtectedRootPath(p string) bool {
	for _, pre := range protectedRootPrefixes {
		if p == strings.TrimSuffix(pre, "/") || strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// outsidePathState describes each path for the evaluator: whether it is new, changed or deleted against fork, and a
// short preview of its content (or of the change to it).
func outsidePathState(ctx context.Context, wt, fork string, paths []string) []map[string]any {
	var out []map[string]any
	for i, p := range paths {
		if i >= 20 {
			break
		}
		status := "added"
		if _, err := gitx.RunGit(ctx, wt, "cat-file", "-e", fork+":"+p); err == nil {
			status = "modified"
			if _, err := os.Lstat(filepath.Join(wt, filepath.FromSlash(p))); err != nil {
				status = "deleted"
			}
		}
		preview := ""
		switch status {
		case "modified":
			// --literal-pathspecs: p is a file name, not a pattern ("pages/[id].go" also matches pages/i.go).
			d, _ := gitx.RunGit(ctx, wt, "--literal-pathspecs", "diff", "--no-color", fork, "--", p)
			preview = d
		case "added":
			if b, err := os.ReadFile(filepath.Join(wt, filepath.FromSlash(p))); err == nil {
				if strings.ContainsRune(string(b[:min(len(b), 512)]), 0) {
					preview = "(binary)"
				} else {
					preview = string(b)
				}
			}
		}
		if len(preview) > 1500 {
			preview = cutText(preview, 1500) + "\n... (truncated)"
		}
		out = append(out, map[string]any{"path": p, "status": status, "preview": preview})
	}
	return out
}

// revertOutsidePaths puts every path back as it is at fork (deleting what fork does not have) and commits
// the result when the agent had committed any of them.
func revertOutsidePaths(ctx context.Context, wt, fork string, paths []string) error {
	for _, p := range paths {
		if _, err := gitx.RunGit(ctx, wt, "cat-file", "-e", fork+":"+p); err == nil {
			if _, err := gitx.RunGit(ctx, wt, "checkout", fork, "--", p); err != nil {
				return err
			}
			continue
		}
		_, _ = gitx.RunGit(ctx, wt, "rm", "-r", "-q", "--cached", "--ignore-unmatch", "--", p)
		if err := os.RemoveAll(filepath.Join(wt, filepath.FromSlash(p))); err != nil {
			return err
		}
	}
	if _, err := gitx.RunGit(ctx, wt, "diff", "--cached", "--quiet"); err == nil {
		return nil
	}
	_, err := gitx.RunGit(ctx, wt, "commit", "--quiet", "-m", "chore(foreman-kit): revert files left outside the allowed paths")
	return err
}

// ---------------------------------------------------------------------------------------------------
// semantic_merge_conflict
// ---------------------------------------------------------------------------------------------------

// ConcurrentCommit is a commit that landed on a child's base after the change started and touches a file
// the change also changes.
type ConcurrentCommit struct {
	Repo    string `json:"repo"`
	Commit  string `json:"commit"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Message string `json:"message"`
	Diff    string `json:"diff"`
}

// ConcurrentCommitsOnFiles lists the commits of onto committed at or after since that touch the files the
// change (onto..rebased) changes, and the change's diff of those files.
func ConcurrentCommitsOnFiles(ctx context.Context, childDir, repo, onto, rebased string, since time.Time) ([]ConcurrentCommit, string, error) {
	if onto == "" || rebased == "" || onto == rebased || since.IsZero() {
		return nil, "", nil
	}
	names, err := gitx.RunGit(ctx, childDir, "diff", "--name-only", "-z", "--no-renames", onto, rebased)
	if err != nil {
		return nil, "", err
	}
	var files []string
	for _, f := range strings.Split(names, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return nil, "", nil
	}
	// --literal-pathspecs: these are file names, not patterns. Read as a pathspec, "pages/[id].go" also
	// matches pages/i.go, whose commits would count as concurrent work on the change's files, and a name
	// that starts with ":" is taken for pathspec magic and fails the command.
	args := append([]string{"--literal-pathspecs", "log", "-n", "50", "--format=%H%x1f%an%x1f%ct", onto, "--"}, files...)
	logOut, err := gitx.RunGit(ctx, childDir, args...)
	if err != nil {
		return nil, "", err
	}
	var out []ConcurrentCommit
	for _, line := range strings.Split(strings.TrimSpace(logOut), "\n") {
		f := strings.Split(line, "\x1f")
		if len(f) != 3 {
			continue
		}
		ct, _ := strconv.ParseInt(f[2], 10, 64)
		if ct < since.Unix() {
			continue
		}
		if len(out) >= workLossMaxCommits {
			break
		}
		msg, _ := gitx.RunGit(ctx, childDir, "log", "-1", "--format=%B", f[0])
		dargs := append([]string{"--literal-pathspecs", "show", "--no-color", "--format=", f[0], "--"}, files...)
		diff, _ := gitx.RunGit(ctx, childDir, dargs...)
		// cutText: never inside a multi-byte character. These texts are stored as the details of a
		// send-back and handed to the BUILD agent as an argument, which must be valid UTF-8.
		if len(diff) > 3000 {
			diff = cutText(diff, 3000) + "\n... (diff truncated)"
		}
		msg = cutText(strings.TrimSpace(msg), workLossMaxMessageBytes)
		out = append(out, ConcurrentCommit{Repo: repo, Commit: f[0], Author: f[1], Date: time.Unix(ct, 0).UTC().Format(time.RFC3339), Message: msg, Diff: diff})
	}
	if len(out) == 0 {
		return nil, "", nil
	}
	cargs := append([]string{"--literal-pathspecs", "diff", "--no-color", "--unified=2", onto, rebased, "--"}, files...)
	changeDiff, _ := gitx.RunGit(ctx, childDir, cargs...)
	if len(changeDiff) > workLossMaxDiffPerRepo {
		changeDiff = cutText(changeDiff, workLossMaxDiffPerRepo) + "\n... (diff truncated)"
	}
	return out, changeDiff, nil
}

func formatConcurrentCommits(cs []ConcurrentCommit, summary string) string {
	var b strings.Builder
	if summary != "" {
		b.WriteString(summary + ".\n")
	}
	for _, c := range cs {
		subject := strings.SplitN(c.Message, "\n", 2)[0]
		fmt.Fprintf(&b, "\n%s %s by %s (%s): %q\n", c.Repo, shortSHA(c.Commit), c.Author, c.Date, subject)
	}
	return strings.TrimSpace(b.String())
}

// checkSemanticMergeConflict asks semantic_merge_conflict about concurrent commits on the files the change
// changes. In enforce a contradiction sends the change back while rebuilds remain, then quarantines it as
// semantic_conflict; duplicated work waits for a human; a human approval lets it integrate.
func (m *Merger) checkSemanticMergeConflict(ctx context.Context, manifest store.ReadyManifest, wt string, childOnto, childRebased map[string]string) error {
	change := manifest.ChangeName
	since := m.changeStartedAt(ctx, change)
	if since.IsZero() || len(childRebased) == 0 {
		return nil
	}
	repos := make([]string, 0, len(childRebased))
	for cp := range childRebased {
		repos = append(repos, cp)
	}
	sort.Strings(repos)
	var concurrent []ConcurrentCommit
	changeDiff := map[string]string{}
	for _, cp := range repos {
		cs, d, err := ConcurrentCommitsOnFiles(ctx, filepath.Join(wt, filepath.FromSlash(cp)), cp, childOnto[cp], childRebased[cp], since)
		if err != nil || len(cs) == 0 {
			continue
		}
		concurrent = append(concurrent, cs...)
		changeDiff[cp] = d
	}
	if len(concurrent) == 0 {
		return nil
	}
	state := map[string]any{
		"change":      change,
		"proposal":    readFileOrEmpty(filepath.Join(wt, "openspec", "changes", change, "proposal.md")),
		"change_diff": changeDiff,
		"concurrent":  concurrent,
	}
	action, v := routeGate(ctx, m.judge, m.store, SemanticMergeGate, change, state)
	if action == "integrate" {
		return nil
	}
	if q, _ := m.store.GetQuarantine(ctx, change); q != nil && q.ApprovedRisk {
		return nil
	}
	report := formatConcurrentCommits(concurrent, answerSummary(v))
	if action == "send_back" && m.rebuildAllowed(ctx, change) {
		return &sendBackError{reason: SemanticMergeGate, msg: SemanticConflictMarker + "\n\n" +
			"Commits that teammates landed on the base after this change started change the same files, and " +
			"your change contradicts them. Keep what they establish (read their messages) and redo your change " +
			"on top of it; if your change must override them, say so explicitly in proposal.md.\n\n" + report}
	}
	return NewBlockedError("semantic_conflict", report+"\n\nApprove to integrate it anyway: foreman-kit approve "+change)
}
