package sim

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// checkPortalReads reads what a scenario left through every read the portal has, inside one View,
// and checks what must hold on any database: every read answers, a list paged in small pages is the
// same list read in one, no row is on two pages, and the counters of a change agree with its story.
// It is how each read is verified against the rows a real pipeline writes, and not only against a
// seeded test database.
func checkPortalReads(ctx context.Context, st *store.Store) error {
	return st.View(ctx, func(v *store.View) error {
		for _, check := range []func(context.Context, *store.View) error{
			checkChangeReads, checkExecutionReads, checkEventReads, checkQueueReads, checkCycleReads, checkVisualReads, checkAggregateReads,
		} {
			if err := check(ctx, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// checkChangeReads walks the list of changes and the story of each change.
func checkChangeReads(ctx context.Context, v *store.View) error {
	counts, err := v.CountChangesByState(ctx, "")
	if err != nil {
		return fmt.Errorf("CountChangesByState: %w", err)
	}
	total := 0
	for _, n := range counts {
		total += n
	}

	seen := map[string]store.ChangeOverview{}
	byState := map[string]int{}
	for page := 1; ; page++ {
		got, err := v.ListChangeOverviews(ctx, store.ChangeFilter{Page: page, Per: 3})
		if err != nil {
			return fmt.Errorf("ListChangeOverviews: %w", err)
		}
		if got.Total != total {
			return fmt.Errorf("the list of changes counts %d and the states add up to %d", got.Total, total)
		}
		for _, c := range got.Items {
			if _, dup := seen[c.ChangeName]; dup {
				return fmt.Errorf("change %s is on two pages of the list", c.ChangeName)
			}
			seen[c.ChangeName] = c
			byState[c.State]++
		}
		if page >= got.Pages {
			break
		}
	}
	if len(seen) != total {
		return fmt.Errorf("the pages of the list hold %d changes of %d", len(seen), total)
	}
	for state, n := range counts {
		if byState[state] != n {
			return fmt.Errorf("%d changes are listed as %s and the count says %d", byState[state], state, n)
		}
		if n == 0 {
			continue
		}
		filtered, err := v.ListChangeOverviews(ctx, store.ChangeFilter{State: state, Per: store.MaxPageSize})
		if err != nil {
			return fmt.Errorf("ListChangeOverviews(%s): %w", state, err)
		}
		if filtered.Total != n {
			return fmt.Errorf("the filter %s matches %d changes and the count says %d", state, filtered.Total, n)
		}
	}

	for name, overview := range seen {
		whole, err := v.ChangeStory(ctx, name, "", store.MaxPageSize)
		if err != nil {
			return fmt.Errorf("ChangeStory(%s): %w", name, err)
		}
		var paged []store.Episode
		for cursor, pages := "", 0; ; pages++ {
			part, err := v.ChangeStory(ctx, name, cursor, 2)
			if err != nil {
				return fmt.Errorf("ChangeStory(%s, %q): %w", name, cursor, err)
			}
			paged = append(paged, part.Episodes...)
			if cursor = part.Next; cursor == "" {
				break
			}
			if pages > 10000 {
				return fmt.Errorf("the story of %s never ends", name)
			}
		}
		if whole.Next == "" && episodeKeys(paged) != episodeKeys(whole.Episodes) {
			return fmt.Errorf("the story of %s read two episodes a page differs from the story read whole:\n%s\n%s",
				name, episodeKeys(paged), episodeKeys(whole.Episodes))
		}

		runs, executions, repairs := 0, 0, 0
		var last time.Time
		for _, ep := range paged {
			at, err := time.Parse(time.RFC3339, ep.At)
			if err != nil {
				return fmt.Errorf("an episode of %s has no time: %+v", name, ep)
			}
			if !last.IsZero() && at.After(last) {
				return fmt.Errorf("the story of %s is not the latest episode first: %s", name, episodeKeys(paged))
			}
			last = at
			switch ep.Kind {
			case store.EpisodeRun:
				if ep.Run == nil || ep.Run.ChangeName != name {
					return fmt.Errorf("a run episode of %s carries %+v", name, ep.Run)
				}
				runs++
				executions += len(ep.Run.Executions)
				repairs += len(ep.Run.Repairs)
				for _, e := range ep.Run.Executions {
					if e.ChangeRunID != ep.Run.ID {
						return fmt.Errorf("execution %s is listed under run %d of %s and belongs to run %d", e.ExecID, ep.Run.ID, name, e.ChangeRunID)
					}
					for i, a := range e.Attempts {
						if a.ExecID != e.ExecID || a.AttemptNo != i+1 {
							return fmt.Errorf("the attempts of %s are not its own, in order: %+v", e.ExecID, e.Attempts)
						}
					}
				}
				for _, g := range ep.Run.GateRounds {
					if g.ChangeRunID != ep.Run.ID {
						return fmt.Errorf("gate round %d is listed under run %d of %s", g.ID, ep.Run.ID, name)
					}
					for _, r := range g.Runs {
						if r.GateRoundID != g.ID {
							return fmt.Errorf("gate run %d is listed under round %d", r.ID, g.ID)
						}
					}
				}
			case store.EpisodeExecution:
				if ep.Execution == nil || ep.Execution.ChangeName != name || ep.Execution.ChangeRunID != 0 {
					return fmt.Errorf("an execution episode of %s carries %+v", name, ep.Execution)
				}
				executions++
			case store.EpisodeMerge:
				if ep.Merge == nil || ep.Merge.ChangeName != name {
					return fmt.Errorf("a merge episode of %s carries %+v", name, ep.Merge)
				}
			default:
				if ep.Manifest == nil && ep.Block == nil && ep.Rebuild == nil && ep.Approval == nil && ep.Deferral == nil &&
					ep.Gates == nil && ep.Evaluation == nil {
					return fmt.Errorf("an episode of %s of kind %s carries nothing", name, ep.Kind)
				}
			}
		}
		if runs != overview.Runs || executions != overview.Executions || repairs != overview.Repairs {
			return fmt.Errorf("the list says %s took %d runs, %d executions and %d repairs; its story has %d, %d and %d",
				name, overview.Runs, overview.Executions, overview.Repairs, runs, executions, repairs)
		}
	}
	return nil
}

func episodeKeys(episodes []store.Episode) string {
	keys := make([]string, 0, len(episodes))
	for _, ep := range episodes {
		id := int64(0)
		switch {
		case ep.Run != nil:
			id = ep.Run.ID
		case ep.Merge != nil:
			id = ep.Merge.ID
		case ep.Manifest != nil:
			id = ep.Manifest.ID
		case ep.Block != nil:
			id = ep.Block.ID
		case ep.Rebuild != nil:
			id = ep.Rebuild.ID
		case ep.Approval != nil:
			id = ep.Approval.ID
		case ep.Deferral != nil:
			id = ep.Deferral.ID
		case ep.Gates != nil:
			id = ep.Gates.ID
		case ep.Evaluation != nil:
			id = ep.Evaluation.ID
		}
		key := fmt.Sprintf("%s#%d@%s", ep.Kind, id, ep.At)
		if ep.Execution != nil {
			key = fmt.Sprintf("%s#%s@%s", ep.Kind, ep.Execution.ExecID, ep.At)
		}
		keys = append(keys, key)
	}
	return strings.Join(keys, " ")
}

// checkExecutionReads walks the list of executions in small pages and by each kind.
func checkExecutionReads(ctx context.Context, v *store.View) error {
	var all []string
	seen := map[string]bool{}
	byKind := map[string]int{}
	for cursor, pages := "", 0; ; pages++ {
		page, err := v.ListExecutionsPage(ctx, store.ExecutionFilter{Before: cursor, Limit: 3})
		if err != nil {
			return fmt.Errorf("ListExecutionsPage(%q): %w", cursor, err)
		}
		for _, e := range page.Items {
			if seen[e.ExecID] {
				return fmt.Errorf("execution %s is on two pages of the list", e.ExecID)
			}
			seen[e.ExecID] = true
			all = append(all, e.ExecID)
			byKind[e.Kind]++
			for i, a := range e.Attempts {
				if a.ExecID != e.ExecID || a.AttemptNo != i+1 {
					return fmt.Errorf("the attempts of %s are not its own, in order: %+v", e.ExecID, e.Attempts)
				}
			}
		}
		if cursor = page.Next; cursor == "" {
			break
		}
		if pages > 100000 {
			return fmt.Errorf("the list of executions never ends")
		}
	}
	if len(all) <= store.MaxPageSize {
		whole, err := v.ListExecutionsPage(ctx, store.ExecutionFilter{Limit: store.MaxPageSize})
		if err != nil {
			return fmt.Errorf("ListExecutionsPage: %w", err)
		}
		var ids []string
		for _, e := range whole.Items {
			ids = append(ids, e.ExecID)
		}
		if strings.Join(ids, " ") != strings.Join(all, " ") {
			return fmt.Errorf("the executions read three a page differ from the list read whole:\n%v\n%v", all, ids)
		}
	}
	for kind, n := range byKind {
		got := 0
		for cursor := ""; ; {
			page, err := v.ListExecutionsPage(ctx, store.ExecutionFilter{Kind: kind, Before: cursor, Limit: store.MaxPageSize})
			if err != nil {
				return fmt.Errorf("ListExecutionsPage(kind %s): %w", kind, err)
			}
			got += len(page.Items)
			if cursor = page.Next; cursor == "" {
				break
			}
		}
		if got != n {
			return fmt.Errorf("the filter by kind %s lists %d executions and the whole list has %d of them", kind, got, n)
		}
	}

	// The record of each execution: its own run, gates and repair, and its output, which reads the
	// same whole and chunk by chunk.
	for _, id := range all {
		rec, err := v.Execution(ctx, id)
		if err != nil {
			return fmt.Errorf("Execution(%s): %w", id, err)
		}
		if rec == nil {
			return fmt.Errorf("execution %s is in the list and has no record of its own", id)
		}
		if (rec.Run != nil) != (rec.ChangeRunID != 0) || (rec.Run != nil && rec.Run.ID != rec.ChangeRunID) {
			return fmt.Errorf("execution %s is of the run %d and its record shows the run %+v", id, rec.ChangeRunID, rec.Run)
		}
		for _, round := range rec.GateRounds {
			if round.AfterExecID != id || round.ChangeRunID != rec.ChangeRunID {
				return fmt.Errorf("the gates of %s include a round that verified something else: %+v", id, round.GateRound)
			}
		}
		if rec.Repair != nil && rec.Repair.ExecID != id {
			return fmt.Errorf("the repair of %s is the one of %s", id, rec.Repair.ExecID)
		}
		if rec.Parent != nil && rec.Parent.ExecID != rec.ParentExecID {
			return fmt.Errorf("execution %s continues %s and its record shows %s", id, rec.ParentExecID, rec.Parent.ExecID)
		}
		if rec.Next != nil && (rec.Next.ChangeName != rec.ChangeName || rec.Next.ExecID == id) {
			return fmt.Errorf("what ran after %s for %s is %s of %s", id, rec.ChangeName, rec.Next.ExecID, rec.Next.ChangeName)
		}
		tail, err := v.LogTail(ctx, id, 0)
		if err != nil {
			return fmt.Errorf("LogTail(%s): %w", id, err)
		}
		var chunks strings.Builder
		for after := ""; ; {
			page, err := v.ReadLogChunks(ctx, id, after, 50)
			if err != nil {
				return fmt.Errorf("ReadLogChunks(%s, %q): %w", id, after, err)
			}
			for _, c := range page.Items {
				chunks.WriteString(c.Content)
			}
			if after = page.Next; after == "" {
				break
			}
		}
		if tail.Truncated || chunks.String() != tail.Content || int64(len(tail.Content)) != tail.Bytes {
			return fmt.Errorf("the output of %s is %d bytes read whole, %d counted and %d read by chunks", id, len(tail.Content), tail.Bytes, chunks.Len())
		}
	}
	return nil
}

// checkEventReads walks the events in small pages and compares them with the counts by type.
func checkEventReads(ctx context.Context, v *store.View) error {
	counts, err := v.CountEventsByType(ctx, store.EventPageFilter{})
	if err != nil {
		return fmt.Errorf("CountEventsByType: %w", err)
	}
	total := 0
	for _, c := range counts {
		total += c.Count
	}
	walked := map[string]int{}
	n, last := 0, int64(0)
	for cursor := ""; ; {
		page, err := v.ListEventsPage(ctx, store.EventPageFilter{Before: cursor, Limit: 7})
		if err != nil {
			return fmt.Errorf("ListEventsPage(%q): %w", cursor, err)
		}
		for _, e := range page.Items {
			if last != 0 && e.ID >= last {
				return fmt.Errorf("event %d is listed after event %d", e.ID, last)
			}
			last = e.ID
			walked[e.Event]++
			n++
		}
		if cursor = page.Next; cursor == "" {
			break
		}
	}
	if n != total {
		return fmt.Errorf("the pages of events hold %d events and the counts by type add up to %d", n, total)
	}
	for _, c := range counts {
		if walked[c.Event] != c.Count {
			return fmt.Errorf("%d events of type %s are listed and the count says %d", walked[c.Event], c.Event, c.Count)
		}
		of, err := v.ListEventsPage(ctx, store.EventPageFilter{Event: c.Event, Limit: 1})
		if err != nil || len(of.Items) != 1 || of.Items[0].Event != c.Event {
			return fmt.Errorf("ListEventsPage(event %s) = %+v, %v", c.Event, of.Items, err)
		}
	}
	return nil
}

// checkQueueReads reads the inbox, the backlog and the merge queue.
func checkQueueReads(ctx context.Context, v *store.View) error {
	in, err := v.ListInbox(ctx)
	if err != nil {
		return fmt.Errorf("ListInbox: %w", err)
	}
	for _, b := range in.Blocked {
		if b.ResolvedAt != "" || b.Reason == "" || b.Quarantine.Status != "blocked" || b.Quarantine.ChangeName != b.ChangeName {
			return fmt.Errorf("the inbox lists a block that is not in force: %+v", b)
		}
	}
	for _, p := range in.Proposals {
		if p.Status != "proposed" {
			return fmt.Errorf("the inbox lists a proposal that was decided: %+v", p)
		}
	}
	lost, err := v.ListLostRuns(ctx)
	if err != nil {
		return fmt.Errorf("ListLostRuns: %w", err)
	}
	if len(lost) != len(in.LostRuns) {
		return fmt.Errorf("ListLostRuns finds %d runs and the inbox %d", len(lost), len(in.LostRuns))
	}
	for _, r := range lost {
		if r.FinishedAt != "" {
			return fmt.Errorf("a run that ended is listed as lost: %+v", r)
		}
	}
	open, err := v.OpenPause(ctx)
	if err != nil {
		return fmt.Errorf("OpenPause: %w", err)
	}
	if (open == nil) != (in.Pause == nil) {
		return fmt.Errorf("OpenPause = %+v and the inbox has %+v", open, in.Pause)
	}

	backlog, err := v.ListWaitFacts(ctx)
	if err != nil {
		return fmt.Errorf("ListWaitFacts: %w", err)
	}
	for _, c := range backlog {
		if c.BacklogStatus != "queued" {
			return fmt.Errorf("the backlog lists %s, whose backlog status is %s", c.ChangeName, c.BacklogStatus)
		}
		for _, f := range c.Facts {
			switch f.Kind {
			case store.WaitDependency:
				if f.OtherChange == "" || f.OtherState == store.ChangeIntegrated {
					return fmt.Errorf("%s waits for a dependency that does not hold it: %+v", c.ChangeName, f)
				}
			case store.WaitBlock:
				if c.State != store.ChangeBlocked || f.Reason == "" {
					return fmt.Errorf("%s has a block as a fact and its state is %s: %+v", c.ChangeName, c.State, f)
				}
			case store.WaitDeferral:
				if c.OpenDeferrals == 0 {
					return fmt.Errorf("%s has a deferral as a fact and no open deferral: %+v", c.ChangeName, f)
				}
			case store.WaitLock:
				if f.LockName == "" || f.OtherChange == "" || f.OtherChange == c.ChangeName {
					return fmt.Errorf("%s shares a lock with nobody: %+v", c.ChangeName, f)
				}
			default:
				return fmt.Errorf("%s has a fact of an unknown kind: %+v", c.ChangeName, f)
			}
		}
	}

	mq, err := v.MergeQueue(ctx, "", 2)
	if err != nil {
		return fmt.Errorf("MergeQueue: %w", err)
	}
	for _, m := range mq.Waiting {
		if m.ClosedAt != "" || m.HeadSHA == "" || (m.Attempts > 0) != (m.Last != nil) {
			return fmt.Errorf("the merge queue lists a manifest that does not wait: %+v", m)
		}
	}
	if mq.Current != nil && mq.Current.FinishedAt != "" {
		return fmt.Errorf("the merge attempt in progress has ended: %+v", mq.Current)
	}
	recent, last := len(mq.Recent), int64(0)
	for cursor := mq.Next; cursor != ""; {
		more, err := v.MergeQueue(ctx, cursor, 2)
		if err != nil {
			return fmt.Errorf("MergeQueue(%q): %w", cursor, err)
		}
		for _, a := range more.Recent {
			if a.FinishedAt == "" || (last != 0 && a.ID >= last) {
				return fmt.Errorf("the attempts that ended are not the latest first: %+v", a)
			}
			last = a.ID
		}
		recent += len(more.Recent)
		cursor = more.Next
	}
	whole, err := v.MergeQueue(ctx, "", store.MaxPageSize)
	if err != nil {
		return fmt.Errorf("MergeQueue: %w", err)
	}
	if whole.Next == "" && len(whole.Recent) != recent {
		return fmt.Errorf("the merge attempts read two a page are %d and read whole are %d", recent, len(whole.Recent))
	}
	if _, err := v.ListProposals(ctx, "proposed", "", 0); err != nil {
		return fmt.Errorf("ListProposals: %w", err)
	}
	if _, err := v.ListIdleAgents(ctx); err != nil {
		return fmt.Errorf("ListIdleAgents: %w", err)
	}
	return nil
}

// checkCycleReads walks the cycles in small pages and reads each one with what was recorded under
// it: the list and the cycles next to each one agree, and a change is queued by one cycle.
func checkCycleReads(ctx context.Context, v *store.View) error {
	seen := map[string]bool{}
	queuedBy := map[string]string{}
	// The list goes from the latest cycle to the first: newer is the cycle read before this one,
	// and older, the one that cycle named as the one before it.
	newer, older, first := "", "", true
	for before := ""; ; {
		page, err := v.ListCyclesPage(ctx, before, 2)
		if err != nil {
			return fmt.Errorf("ListCyclesPage: %w", err)
		}
		for _, c := range page.Items {
			if seen[c.Stamp] {
				return fmt.Errorf("cycle %s is on two pages of the list", c.Stamp)
			}
			seen[c.Stamp] = true
			rec, err := v.Cycle(ctx, c.Stamp)
			if err != nil {
				return fmt.Errorf("Cycle(%s): %w", c.Stamp, err)
			}
			if rec == nil {
				return fmt.Errorf("cycle %s is in the list and is not read by its stamp", c.Stamp)
			}
			if rec.Cycle != c {
				return fmt.Errorf("cycle %s read by its stamp is %+v, and the list gives %+v", c.Stamp, rec.Cycle, c)
			}
			if rec.Newer != newer || (!first && older != c.Stamp) {
				return fmt.Errorf("cycle %s comes after %q in the list and names %q as the one after it; the one before named %q as its older one", c.Stamp, newer, rec.Newer, older)
			}
			newer, older, first = c.Stamp, rec.Older, false
			for _, e := range rec.Executions {
				if e.CycleStamp != c.Stamp {
					return fmt.Errorf("execution %s is under cycle %s and belongs to %q", e.ExecID, c.Stamp, e.CycleStamp)
				}
			}
			for _, q := range rec.Queued {
				if other, taken := queuedBy[q.ChangeName]; taken {
					return fmt.Errorf("change %s is queued by the cycles %s and %s", q.ChangeName, other, c.Stamp)
				}
				queuedBy[q.ChangeName] = c.Stamp
				change, err := v.Change(ctx, q.ChangeName)
				if err != nil {
					return fmt.Errorf("Change(%s): %w", q.ChangeName, err)
				}
				if change == nil || change.ChangeOverview != q {
					return fmt.Errorf("cycle %s lists the change it queued as %+v, and the change read by its name is not that", c.Stamp, q)
				}
			}
		}
		if before = page.Next; before == "" {
			break
		}
	}
	if !first && older != "" {
		return fmt.Errorf("the first cycle on record, %s, names %s as an older one", newer, older)
	}
	return nil
}

// checkVisualReads walks the checks in a browser in small pages, whole and by status, and compares
// them with their count by status. It reads the session in progress too.
func checkVisualReads(ctx context.Context, v *store.View) error {
	counts, err := v.CountVisualQAByStatus(ctx)
	if err != nil {
		return fmt.Errorf("CountVisualQAByStatus: %w", err)
	}
	if len(counts) != len(store.VisualQAStatuses) {
		return fmt.Errorf("the checks in a browser are counted in %d statuses, and there are %d", len(counts), len(store.VisualQAStatuses))
	}
	walk := func(status string) (int, error) {
		seen := map[string]bool{}
		last := int64(math.MaxInt64)
		for cursor := ""; ; {
			page, err := v.ListVisualQAPage(ctx, status, cursor, 2)
			if err != nil {
				return 0, fmt.Errorf("ListVisualQAPage(%q, %q): %w", status, cursor, err)
			}
			for _, c := range page.Items {
				if seen[c.ItemID] {
					return 0, fmt.Errorf("the check %s is on two pages", c.ItemID)
				}
				seen[c.ItemID] = true
				if status != "" && c.Status != status {
					return 0, fmt.Errorf("the check %s is %s among the checks in the status %s", c.ItemID, c.Status, status)
				}
				if _, known := counts[c.Status]; !known {
					return 0, fmt.Errorf("the check %s has the status %q, which is not one of the statuses of a check", c.ItemID, c.Status)
				}
				id, err := strconv.ParseInt(c.ItemID, 10, 64)
				if err != nil || id >= last {
					return 0, fmt.Errorf("the checks do not come the latest first: %s after %d", c.ItemID, last)
				}
				last = id
			}
			if cursor = page.Next; cursor == "" {
				return len(seen), nil
			}
		}
	}
	total := 0
	for _, status := range store.VisualQAStatuses {
		n, err := walk(status)
		if err != nil {
			return err
		}
		if n != counts[status] {
			return fmt.Errorf("%d checks are listed as %s and their count says %d", n, status, counts[status])
		}
		total += n
	}
	if all, err := walk(""); err != nil {
		return err
	} else if all != total {
		return fmt.Errorf("%d checks are listed in all, and the statuses add up to %d", all, total)
	}
	if _, err := v.VisualSession(ctx); err != nil {
		return fmt.Errorf("VisualSession: %w", err)
	}
	// The locks on record: each one says who took it and when it stops counting.
	locks, err := v.ListLocks(ctx)
	if err != nil {
		return fmt.Errorf("ListLocks: %w", err)
	}
	for _, l := range locks {
		if l.Key == "" || l.OwnerType == "" || l.AcquiredAt == "" || l.ExpiresAt < l.HeartbeatAt {
			return fmt.Errorf("a lock on record is not whole: %+v", l)
		}
	}
	return nil
}

// checkAggregateReads reads the aggregates since the beginning and the history of the workspace.
func checkAggregateReads(ctx context.Context, v *store.View) error {
	gates, err := v.GateStats(ctx, time.Time{})
	if err != nil {
		return fmt.Errorf("GateStats: %w", err)
	}
	for _, g := range gates {
		// A gate has a name, or it is the one that validates the specs of each change and says of how many.
		named := (g.GateName != "") != (g.Changes > 0)
		if g.Runs <= 0 || g.Failures > g.Runs || g.MaxMS > g.TotalMS || g.RepoName == "" || !named ||
			(g.Failures > 0) != (g.LastFailedRoundID > 0) || (g.Failures > 0) != (g.LastFailedAt != "") {
			return fmt.Errorf("the numbers of a gate do not add up: %+v", g)
		}
	}
	evals, err := v.EvaluationStats(ctx, time.Time{})
	if err != nil {
		return fmt.Errorf("EvaluationStats: %w", err)
	}
	for _, e := range evals {
		sum := 0
		for _, n := range e.ByStatus {
			sum += n
		}
		modes, routed := 0, 0
		for _, n := range e.ByMode {
			modes += n
		}
		for _, n := range e.ByRoute {
			routed += n
		}
		if sum != e.Evaluations || e.Passed > e.Evaluations || e.Cached > e.Evaluations || modes > e.Evaluations || routed > e.Evaluations {
			return fmt.Errorf("the numbers of the evaluations of a gate do not add up: %+v", e)
		}
	}
	engines, err := v.EngineStats(ctx, time.Time{})
	if err != nil {
		return fmt.Errorf("EngineStats: %w", err)
	}
	for _, e := range engines {
		sum := e.Running
		for _, n := range e.ByOutcome {
			sum += n
		}
		if sum != e.Attempts || e.Stalls > e.IdlePeriods {
			return fmt.Errorf("the numbers of an engine do not add up: %+v", e)
		}
	}
	// The engines on record are the ones the names list, and every engine that made an attempt is
	// one of them, with a health the schema knows.
	records, err := v.ListEngines(ctx)
	if err != nil {
		return fmt.Errorf("ListEngines: %w", err)
	}
	names, err := v.ListEngineNames(ctx)
	if err != nil {
		return fmt.Errorf("ListEngineNames: %w", err)
	}
	onRecord := map[string]bool{}
	for i, r := range records {
		if i >= len(names) || names[i] != r.Engine {
			return fmt.Errorf("the engines on record are %v by name, and ListEngines gives %s in place %d", names, r.Engine, i)
		}
		onRecord[r.Engine] = true
		if r.Health == "" || r.InUse == nil {
			return fmt.Errorf("the engine %s has no health or no list of who runs it: %+v", r.Engine, r)
		}
		if r.LastCheckedAt == "" && r.Health != "unknown" {
			return fmt.Errorf("the engine %s is %s and nothing checked it", r.Engine, r.Health)
		}
	}
	if len(names) != len(records) {
		return fmt.Errorf("%d engines are on record by name and ListEngines gives %d", len(names), len(records))
	}
	for _, e := range engines {
		if !onRecord[e.Engine] {
			return fmt.Errorf("the engine %s made attempts and is not on record", e.Engine)
		}
	}
	if later, err := v.GateStats(ctx, v.At().Add(time.Hour)); err != nil || len(later) != 0 {
		return fmt.Errorf("gate runs since an hour from now = %+v, %v", later, err)
	}

	for cursor := ""; ; {
		page, err := v.ListDispatcherRuns(ctx, cursor, 2)
		if err != nil {
			return fmt.Errorf("ListDispatcherRuns(%q): %w", cursor, err)
		}
		for _, d := range page.Items {
			sum := 0
			for _, n := range d.Results {
				sum += n
			}
			if sum != d.ChangeRuns {
				return fmt.Errorf("the worker runs of a dispatch do not add up: %+v", d)
			}
		}
		if cursor = page.Next; cursor == "" {
			break
		}
	}
	for cursor := ""; ; {
		page, err := v.ListPauses(ctx, cursor, 2)
		if err != nil {
			return fmt.Errorf("ListPauses(%q): %w", cursor, err)
		}
		if cursor = page.Next; cursor == "" {
			break
		}
	}
	return nil
}
