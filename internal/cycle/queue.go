package cycle

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// The brake of the cycle. The judgment adds to the queue whether or not the pipeline empties it:
// when changes pile up blocked, or held behind one another, every cycle still queues its findings,
// and a daemon that starts the cycle before its tick queues them several times an hour.
// settings.QueueMaxOpen caps how many changes may be open at once. A cycle counts them before its
// judgment, tells the judgment how many findings still fit, and takes out of the backlog what it
// queued over that number; an approved proposal is queued only while there is room; and the daemon
// starts no cycle before its tick while there is none. This file holds the count and what follows
// from it.

// queueMaxOpenKey names the setting that caps the queue, as the logs and the prompts say it.
var queueMaxOpenKey = settings.QueueMaxOpen.Path()

// queueState is how full the queue is: the changes the pipeline has not finished with, against the
// most the application lets be open at once (settings.QueueMaxOpen).
type queueState struct {
	Open    int
	MaxOpen int
}

// Room is how many more changes fit.
func (q queueState) Room() int {
	if q.Open >= q.MaxOpen {
		return 0
	}
	return q.MaxOpen - q.Open
}

// Full reports whether no more change fits.
func (q queueState) Full() bool { return q.Room() == 0 }

// Quota is how many findings a cycle may queue: maxFindings (settings.QueueMaxFindings), or the
// room the queue has left when that is less.
func (q queueState) Quota(maxFindings int) int {
	return minInt(maxFindings, q.Room())
}

// String says how full the queue is, naming the setting.
func (q queueState) String() string {
	return fmt.Sprintf("%s open (%s: %d)", countOf(q.Open, "change"), queueMaxOpenKey, q.MaxOpen)
}

// countOf writes n with its noun: "1 change", "3 changes".
func countOf(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// backlogNames returns the change names items lists, each once and in order. An item without a
// usable name is no change: the dispatcher skips it.
func backlogNames(items []map[string]any) []string {
	seen := make(map[string]bool, len(items))
	names := make([]string, 0, len(items))
	for _, it := range items {
		name := strings.TrimSpace(asString(it["change_name"]))
		if !ChangeNameRe.MatchString(name) || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// openChanges counts the changes the pipeline has not finished with, backlog being the backlog as
// its file lists it now (store.CountOpenChanges). Without a store only that file is known.
func openChanges(ctx context.Context, st *store.Store, backlog []map[string]any) (int, error) {
	names := backlogNames(backlog)
	if st == nil {
		return len(names), nil
	}
	return st.CountOpenChanges(ctx, names)
}

// readQueue counts the open changes of an application against its cap. root is the checkout whose
// backlog and settings count: the worktree a cycle judges in, the main checkout for the daemon.
func readQueue(ctx context.Context, r *settings.Reader, root string, st *store.Store) (queueState, error) {
	q := queueState{MaxOpen: r.Int(settings.QueueMaxOpen)}
	backlog, err := ReadJSONList(filepath.Join(root, filepath.FromSlash(BacklogFile)))
	if err != nil {
		return q, err
	}
	q.Open, err = openChanges(ctx, st, backlog)
	return q, err
}

// queueIsFull reports whether the application at root has as many changes open as it allows, for the
// daemon, which asks on every pass of its dispatcher: a value of the setting it cannot use is the
// cycle's to report, not its own. full is false when the changes cannot be counted.
func queueIsFull(ctx context.Context, root string, st *store.Store) (q queueState, full bool) {
	r := &settings.Reader{Root: root, Stderr: io.Discard}
	q, err := readQueue(ctx, r, root, st)
	return q, err == nil && q.Full()
}

// cycleQuota is what a cycle may queue, decided before its judgment.
type cycleQuota struct {
	queueState
	// MaxFindings is settings.QueueMaxFindings, and Findings what this cycle may queue: that
	// number, or the room the queue has left when it is less.
	MaxFindings int
	Findings    int
	// Counted is false when the open changes were not counted: the cycle is then held to
	// MaxFindings alone, by its prompt.
	Counted bool
	// Before and BeforeProposed hold the names the backlog and the proposals listed when the
	// judgment began: an item with another name is one the judgment added.
	Before         map[string]bool
	BeforeProposed map[string]bool
}

// quotaFor returns what a cycle may queue with open changes open, by the settings r reads. A
// negative open says they were not counted.
func quotaFor(r *settings.Reader, open int) cycleQuota {
	q := cycleQuota{MaxFindings: r.Int(settings.QueueMaxFindings)}
	q.MaxOpen = r.Int(settings.QueueMaxOpen)
	q.Findings = q.MaxFindings
	if open >= 0 {
		q.Open, q.Counted = open, true
		q.Findings = q.Quota(q.MaxFindings)
	}
	return q
}

// planQuota counts the open changes before a judgment that runs in wt, and says in the cycle's log
// what the cycle may queue.
func planQuota(ctx context.Context, wt string, st *store.Store, out io.Writer) cycleQuota {
	r := cycleReader(wt, nil)
	before, beforeProposed := map[string]bool{}, map[string]bool{}
	open := -1
	backlog, err := ReadJSONList(filepath.Join(wt, filepath.FromSlash(BacklogFile)))
	if err == nil {
		for _, name := range backlogNames(backlog) {
			before[name] = true
		}
		open, err = openChanges(ctx, st, backlog)
	}
	if err != nil {
		fmt.Fprintf(out, "queue: the open changes cannot be counted (%v): this cycle is not held to %s\n", err, queueMaxOpenKey)
		open = -1
	}
	if proposals, err := ReadProposals(wt); err == nil {
		for _, name := range backlogNames(proposals) {
			beforeProposed[name] = true
		}
	}
	quota := quotaFor(r, open)
	quota.Before, quota.BeforeProposed = before, beforeProposed
	if !quota.Counted {
		return quota
	}
	if quota.Findings == 0 {
		fmt.Fprintf(out, "queue: %s: this cycle queues nothing new; it verifies and reports\n", quota.queueState)
	} else {
		fmt.Fprintf(out, "queue: %s: this cycle may queue %s\n", quota.queueState, countOf(quota.Findings, "finding"))
	}
	if st != nil && quota.Findings < quota.MaxFindings {
		_ = st.EmitEvent(ctx, store.EventInput{
			Event: "cycle.queue_limited",
			Payload: map[string]any{
				"open":         quota.Open,
				"max_open":     quota.MaxOpen,
				"findings":     quota.Findings,
				"max_findings": quota.MaxFindings,
			},
		})
	}
	return quota
}

// promptLine is the "Queue" line of "This cycle": how full the queue is and what follows for the
// judgment. It is empty when the open changes were not counted.
func (q cycleQuota) promptLine() string {
	if !q.Counted {
		return ""
	}
	verb := "are"
	if q.Open == 1 {
		verb = "is"
	}
	state := fmt.Sprintf("%s %s open (queued, building, ready, merging, blocked or waiting to be rebuilt) and `%s` in `%s` is %d",
		countOf(q.Open, "change"), verb, queueMaxOpenKey, settings.QueueMaxOpen.FilePath(), q.MaxOpen)
	switch {
	case q.Findings == 0:
		return fmt.Sprintf("- **Queue**: full. %s: record no finding this cycle, neither a backlog item nor a proposal, the roadmap included. Verify delivered work, triage and explore as usual, and list what you find in the report as not queued because the queue is full. The script takes out of the backlog what this cycle adds to it, and an approved proposal waits for room.\n", state)
	case q.Findings < q.MaxFindings:
		return fmt.Sprintf("- **Queue**: %s: it takes %d more, so this cycle records at most %s instead of %d; list the others in the report as not queued. The script takes out of the backlog what this cycle adds over that, and an approved proposal waits for room.\n",
			state, q.Room(), countOf(q.Findings, "finding"), q.MaxFindings)
	default:
		return fmt.Sprintf("- **Queue**: %s: there is room for this cycle's findings. The script takes out of the backlog what this cycle adds over their number.\n", state)
	}
}

// priorityRank orders backlog items as the dispatcher does: critical first, and a priority it does
// not know as normal.
func priorityRank(item map[string]any) int {
	normal := 0
	for i, p := range config.BacklogPriorities {
		if p == "normal" {
			normal = i
		}
	}
	prio := strings.TrimSpace(asString(item["priority"]))
	for i, p := range config.BacklogPriorities {
		if p == prio {
			return i
		}
	}
	return normal
}

// limitQueued takes out of the backlog of root what a judgment queued over quota. before holds the
// names the backlog listed when the judgment began: an item with another name is one the judgment
// queued. Those that stay are the ones with the highest priority and, among equals, the first
// listed. It returns the items the judgment queued that stay and the ones taken out; the file is
// only rewritten when one is.
func limitQueued(root string, before map[string]bool, quota int) (kept, removed []map[string]any, err error) {
	path := filepath.Join(root, filepath.FromSlash(BacklogFile))
	items, err := ReadJSONList(path)
	if err != nil {
		return nil, nil, err
	}
	// The names the judgment queued, in order, each with its first item.
	type queued struct {
		name string
		item map[string]any
	}
	var added []queued
	seen := map[string]bool{}
	for _, it := range items {
		name := strings.TrimSpace(asString(it["change_name"]))
		if !ChangeNameRe.MatchString(name) || before[name] || seen[name] {
			continue
		}
		seen[name] = true
		added = append(added, queued{name, it})
	}
	if quota < 0 {
		quota = 0
	}
	if len(added) <= quota {
		for _, a := range added {
			kept = append(kept, a.item)
		}
		return kept, nil, nil
	}
	sort.SliceStable(added, func(i, j int) bool { return priorityRank(added[i].item) < priorityRank(added[j].item) })
	out := map[string]bool{}
	for i, a := range added {
		if i < quota {
			kept = append(kept, a.item)
		} else {
			out[a.name] = true
		}
	}
	var rest []map[string]any
	for _, it := range items {
		if out[strings.TrimSpace(asString(it["change_name"]))] {
			removed = append(removed, it)
			continue
		}
		rest = append(rest, it)
	}
	if err := WriteJSONList(path, rest); err != nil {
		return nil, nil, err
	}
	return kept, removed, nil
}

// enforce holds the judgment that ran in wt to what the cycle may queue: what it added to the backlog
// over that number is taken out and kept in savePath, a JSON list beside the cycle's log, and the
// cycle's row says how many findings it recorded. Proposals are left as the judgment wrote them: one
// enters the queue only when it is approved and there is room (ReviewProposals).
func (q cycleQuota) enforce(ctx context.Context, wt, stamp, savePath string, st *store.Store, out io.Writer) {
	if !q.Counted {
		return
	}
	kept, removed, err := limitQueued(wt, q.Before, q.Findings)
	if err != nil {
		fmt.Fprintf(out, "queue: %s cannot be read (%v): what the judgment queued is not checked\n", BacklogFile, err)
		return
	}
	proposed := 0
	if proposals, err := ReadProposals(wt); err == nil {
		for _, name := range backlogNames(proposals) {
			if !q.BeforeProposed[name] {
				proposed++
			}
		}
	}
	if st != nil {
		_ = st.UpdateCycle(ctx, store.Cycle{Stamp: stamp, FindingsCount: len(kept) + proposed})
	}
	if len(removed) == 0 {
		return
	}
	names := backlogNames(removed)
	where := ""
	if savePath != "" {
		if err := WriteJSONList(savePath, removed); err == nil {
			where = "; they are kept in " + savePath
		}
	}
	fmt.Fprintf(out, "queue: the judgment queued %s and this cycle may queue %d (%s): %s taken out of the backlog%s\n",
		countOf(len(kept)+len(names), "change"), q.Findings, q.queueState, strings.Join(names, ", "), where)
	if st != nil {
		payload := map[string]any{
			"changes":  names,
			"findings": q.Findings,
			"open":     q.Open,
			"max_open": q.MaxOpen,
		}
		if where != "" {
			payload["kept_in"] = savePath
		}
		_ = st.EmitEvent(ctx, store.EventInput{Event: "cycle.not_queued", Payload: payload})
	}
}
