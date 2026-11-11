package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/store"
)

// WorkLossGate is the evaluator gate that judges a change removing a teammate's concurrent work.
const WorkLossGate = "concurrent_work_loss"

// WorkLossMarker opens the rebuild feedback the merger sends when a change removes concurrent work; the
// dispatcher recognizes it to hand the feedback to the BUILD agent even when the rebuild has no conflict.
const WorkLossMarker = "## Concurrent work lost"

const (
	workLossMaxCommits      = 10
	workLossMaxLinesPerCmt  = 40
	workLossMaxDiffPerRepo  = 6000
	workLossMaxMessageBytes = 2000
)

// LostCommit is a commit that landed on a child's base after the change started and whose lines the
// change removes.
type LostCommit struct {
	Repo      string   `json:"repo"`
	Commit    string   `json:"commit"`
	Author    string   `json:"author"`
	Date      string   `json:"date"`
	Message   string   `json:"message"`
	LostLines []string `json:"lost_lines"` // "file:line: text" on the base
	TotalLost int      `json:"total_lost"`
}

var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,(\d+))? @@`)

type removedLine struct {
	line int
	text string
}

// removedLines parses `git diff --unified=0 --src-prefix=a/ onto rebased` into the lines each file loses,
// numbered on onto. Inside a hunk a line is read by its first character and counted against the hunk
// header, never as a header: a removed line "-- note" reads "--- note", like the one naming the old file.
func removedLines(diff string) map[string][]removedLine {
	out := map[string][]removedLine{}
	file := ""
	oldLine, oldLeft, newLeft := 0, 0, 0
	for _, l := range strings.Split(diff, "\n") {
		if oldLeft > 0 || newLeft > 0 {
			switch {
			case strings.HasPrefix(l, "-") && oldLeft > 0:
				if file != "" {
					out[file] = append(out[file], removedLine{line: oldLine, text: l[1:]})
				}
				oldLine++
				oldLeft--
				continue
			case strings.HasPrefix(l, "+") && newLeft > 0:
				newLeft--
				continue
			case strings.HasPrefix(l, " ") && oldLeft > 0 && newLeft > 0:
				// An unchanged line between two hunks git fused (diff.interHunkContext).
				oldLine++
				oldLeft--
				newLeft--
				continue
			case strings.HasPrefix(l, `\`):
				// "\ No newline at end of file" belongs to the line before it.
				continue
			}
			oldLeft, newLeft = 0, 0
		}
		switch {
		case strings.HasPrefix(l, "diff --git "):
			file = ""
		case strings.HasPrefix(l, "--- "):
			file = oldDiffPath(strings.TrimPrefix(l, "--- "))
		case strings.HasPrefix(l, "@@"):
			if m := hunkHeaderRe.FindStringSubmatch(l); m != nil {
				oldLine, _ = strconv.Atoi(m[1])
				oldLeft, newLeft = hunkLineCount(m[2]), hunkLineCount(m[3])
			}
		}
	}
	return out
}

// oldDiffPath returns the file a "--- a/<path>" header names, "" for /dev/null. git ends the header
// with a tab when the name holds a space, and C-quotes a name with special characters, prefix included.
func oldDiffPath(p string) string {
	p = strings.TrimSuffix(p, "\t")
	if p == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(unquoteGitPath(p), "a/")
}

// hunkLineCount is a line count of a hunk header ("@@ -3,2 +3 @@"): 1 when omitted.
func hunkLineCount(s string) int {
	if s == "" {
		return 1
	}
	n, _ := strconv.Atoi(s)
	return n
}

type blameInfo struct {
	author        string
	committerTime int64
}

// blameLines maps each requested line of file at rev to the commit that last changed it.
func blameLines(ctx context.Context, dir, rev, file string, lines []removedLine) (map[int]string, map[string]blameInfo, error) {
	// --no-ignore-revs-file: a blame.ignoreRevsFile of the user's configuration naming a file this
	// repository does not have (a common global setting) makes git blame fail.
	args := []string{"blame", "--porcelain", "--no-ignore-revs-file"}
	for _, l := range lines {
		args = append(args, "-L", fmt.Sprintf("%d,%d", l.line, l.line))
	}
	args = append(args, rev, "--", file)
	out, err := gitx.RunGit(ctx, dir, args...)
	if err != nil {
		return nil, nil, err
	}
	byLine := map[int]string{}
	infos := map[string]blameInfo{}
	cur := ""
	for _, l := range strings.Split(out, "\n") {
		if l == "" || strings.HasPrefix(l, "\t") {
			continue
		}
		fields := strings.Fields(l)
		if len(fields) >= 3 && len(fields[0]) == 40 && isHex(fields[0]) {
			cur = fields[0]
			if final, err := strconv.Atoi(fields[2]); err == nil {
				byLine[final] = cur
			}
			continue
		}
		info := infos[cur]
		switch {
		case strings.HasPrefix(l, "author "):
			info.author = strings.TrimPrefix(l, "author ")
		case strings.HasPrefix(l, "committer-time "):
			info.committerTime, _ = strconv.ParseInt(strings.TrimPrefix(l, "committer-time "), 10, 64)
		}
		infos[cur] = info
	}
	return byLine, infos, nil
}

// unquoteGitPath undoes git's C-style quoting of a path with special characters ("a/x\ty").
func unquoteGitPath(p string) string {
	if strings.HasPrefix(p, `"`) {
		if u, err := strconv.Unquote(p); err == nil {
			return u
		}
	}
	return p
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// DetectConcurrentWorkLoss lists the commits of childDir's base (onto) committed at or after since whose
// lines the change (onto..rebased) removes. It returns nil when nothing concurrent is lost.
func DetectConcurrentWorkLoss(ctx context.Context, childDir, repo, onto, rebased string, since time.Time) ([]LostCommit, error) {
	if onto == "" || rebased == "" || onto == rebased || since.IsZero() {
		return nil, nil
	}
	// The prefixes are stated: diff.noprefix, diff.mnemonicPrefix and diff.srcPrefix change them.
	diff, err := gitx.RunGit(ctx, childDir, "-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff", "--unified=0", "--no-renames",
		"--src-prefix=a/", "--dst-prefix=b/", onto, rebased)
	if err != nil {
		return nil, err
	}
	removed := removedLines(diff)
	if len(removed) == 0 {
		return nil, nil
	}
	files := make([]string, 0, len(removed))
	for f := range removed {
		files = append(files, f)
	}
	sort.Strings(files)

	byCommit := map[string]*LostCommit{}
	var order []string
	for _, f := range files {
		lines := removed[f]
		byLine, infos, err := blameLines(ctx, childDir, onto, f, lines)
		if err != nil {
			continue
		}
		for _, rl := range lines {
			sha := byLine[rl.line]
			info, ok := infos[sha]
			if sha == "" || !ok || info.committerTime < since.Unix() {
				continue
			}
			lc := byCommit[sha]
			if lc == nil {
				lc = &LostCommit{Repo: repo, Commit: sha, Author: info.author, Date: time.Unix(info.committerTime, 0).UTC().Format(time.RFC3339)}
				byCommit[sha] = lc
				order = append(order, sha)
			}
			lc.TotalLost++
			if len(lc.LostLines) < workLossMaxLinesPerCmt {
				lc.LostLines = append(lc.LostLines, fmt.Sprintf("%s:%d: %s", f, rl.line, rl.text))
			}
		}
	}
	var lost []LostCommit
	for _, sha := range order {
		if len(lost) >= workLossMaxCommits {
			break
		}
		lc := byCommit[sha]
		msg, _ := gitx.RunGit(ctx, childDir, "log", "-1", "--format=%B", sha)
		lc.Message = cutText(strings.TrimSpace(msg), workLossMaxMessageBytes)
		lost = append(lost, *lc)
	}
	return lost, nil
}

// changeStartedAt is when the change's first worker started (its earliest change.start event).
func (m *Merger) changeStartedAt(ctx context.Context, change string) time.Time {
	evts, err := m.store.QueryEvents(ctx, store.EventFilter{Event: "change.start", ChangeName: change, Limit: 1})
	if err != nil || len(evts) == 0 {
		return time.Time{}
	}
	if evts[0].TSUnix > 0 {
		return time.Unix(evts[0].TSUnix, 0)
	}
	t, _ := time.Parse(time.RFC3339, evts[0].TS)
	return t
}

// checkConcurrentWorkLoss asks the concurrent_work_loss gate of the evaluator about any concurrent work the rebased
// children remove. A blocking verdict sends the change back to restore it while rebuilds remain, then
// quarantines it as concurrent_work_loss; a human approval (foreman-kit approve) lets it integrate.
func (m *Merger) checkConcurrentWorkLoss(ctx context.Context, manifest store.ReadyManifest, wt string, childOnto, childRebased map[string]string) error {
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

	var lost []LostCommit
	changeDiff := map[string]string{}
	for _, cp := range repos {
		childDir := filepath.Join(wt, filepath.FromSlash(cp))
		l, err := DetectConcurrentWorkLoss(ctx, childDir, cp, childOnto[cp], childRebased[cp], since)
		if err != nil || len(l) == 0 {
			continue
		}
		lost = append(lost, l...)
		if d, err := gitx.RunGit(ctx, childDir, "diff", "--no-color", "--no-ext-diff", "--unified=1", childOnto[cp], childRebased[cp]); err == nil {
			if len(d) > workLossMaxDiffPerRepo {
				d = cutText(d, workLossMaxDiffPerRepo) + "\n... (diff truncated)"
			}
			changeDiff[cp] = d
		}
	}
	if len(lost) == 0 {
		return nil
	}

	totalLines := 0
	for _, lc := range lost {
		totalLines += lc.TotalLost
	}
	detected := map[string]any{"commits": len(lost), "lines": totalLines, "since": since.UTC().Format(time.RFC3339)}
	_ = m.store.EmitEvent(ctx, store.EventInput{Event: "merge.work_loss_detected", Change: change, Payload: detected})

	if m.judge == nil {
		return nil
	}
	state := map[string]any{
		"change":      change,
		"proposal":    readFileOrEmpty(filepath.Join(wt, "openspec", "changes", change, "proposal.md")),
		"lost_work":   lost,
		"change_diff": changeDiff,
	}
	verdict, err := m.judge.Ask(ctx, WorkLossGate, state, change)
	if err != nil {
		if strings.HasPrefix(err.Error(), "judge_unavailable:") {
			return NewBlockedError("judge_unavailable", strings.TrimPrefix(err.Error(), "judge_unavailable: "))
		}
		m.logf("merger: %s: %s gate could not run: %v", change, WorkLossGate, err)
		return nil
	}
	if verdict == nil || len(verdict.Answers) == 0 {
		return nil
	}
	summary := evaluator.SummarizeAnswers(verdict.Answers)
	rules := evaluator.BlockingRules(*verdict, m.judge.Policy().Gates[WorkLossGate])
	var labels []string
	for _, r := range rules {
		labels = append(labels, evaluator.RuleLabel(r))
	}

	route := func(action string) {
		emitRoute(ctx, m.store, change, verdict, map[string]any{
			"gate":    WorkLossGate,
			"action":  action,
			"rules":   strings.Join(labels, ","),
			"commits": len(lost),
			"lines":   totalLines,
		})
	}
	if len(rules) == 0 {
		route("accepted")
		m.logf("merger: %s: removes %d line(s) of %d concurrent commit(s); the evaluator judged the loss acceptable", change, totalLines, len(lost))
		return nil
	}
	q, _ := m.store.GetQuarantine(ctx, change)
	approved := (q != nil && q.ApprovedRisk) ||
		fileExists(filepath.Join(m.root, "log", "approved", change)) ||
		(m.mainRoot != "" && fileExists(filepath.Join(m.mainRoot, "log", "approved", change)))
	if approved {
		route("approved_by_operator")
		return nil
	}
	if !verdict.Enforce() {
		route("flagged_advisory")
		return nil
	}

	report := FormatWorkLossReport(lost, labels, summary)
	if m.rebuildAllowed(ctx, change) {
		route("sent_back")
		return &sendBackError{reason: WorkLossGate, msg: WorkLossMarker + "\n\n" +
			"Your change removes work that teammates landed on the base after this change started. Restore " +
			"every line listed below (keep your own changes too); if the change must replace that work, " +
			"say so explicitly in proposal.md.\n\n" + report}
	}
	route("quarantined")
	return NewBlockedError(WorkLossGate, report+"\n\nApprove to integrate it anyway: foreman-kit approve "+change)
}

// FormatWorkLossReport renders the lost commits, the matched rules and the evaluator's answers for a human or an agent.
func FormatWorkLossReport(lost []LostCommit, rules []string, summary map[string]any) string {
	var b strings.Builder
	if len(rules) > 0 {
		sum, _ := json.Marshal(summary)
		fmt.Fprintf(&b, "The evaluator flagged %s (%s).\n", strings.Join(rules, ", "), sum)
	}
	for _, lc := range lost {
		subject := strings.SplitN(lc.Message, "\n", 2)[0]
		fmt.Fprintf(&b, "\n%s %s by %s (%s): %q, %d line(s) removed:\n", lc.Repo, shortSHA(lc.Commit), lc.Author, lc.Date, subject, lc.TotalLost)
		for _, l := range lc.LostLines {
			fmt.Fprintf(&b, "  %s\n", l)
		}
		if lc.TotalLost > len(lc.LostLines) {
			fmt.Fprintf(&b, "  ... and %d more\n", lc.TotalLost-len(lc.LostLines))
		}
	}
	return strings.TrimSpace(b.String())
}
