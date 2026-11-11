package store

import (
	"context"
	"database/sql"
	"os"
	"runtime"
	"sort"
	"strconv"
)

// lockGone reports whether a lock is no longer held, as far as a read can tell without starting a
// process: its time to live ran out, or its owner on this host is dead. It is IsLockStale without the
// ps that reads the owner's start time where there is no /proc, so a process ID given to another
// process since is told only on Linux.
func lockGone(rec LockRecord, currentHost string, nowUnix int64) bool {
	hb := rec.HeartbeatAt
	if hb == 0 {
		hb = rec.AcquiredAt
	}
	if rec.TTLSeconds > 0 && nowUnix-hb > int64(rec.TTLSeconds) {
		return true
	}
	if rec.PID <= 0 || (rec.Host != "" && rec.Host != currentHost) {
		return false
	}
	if rec.PID == os.Getpid() {
		// Our own PID is always alive: the row is ours only if it was written after we started.
		return rec.AcquiredAt > 0 && rec.AcquiredAt+1 < selfStartUnix
	}
	if !IsPIDAlive(rec.PID) {
		return true
	}
	if runtime.GOOS == "linux" && (rec.AcquiredAt > 0 || rec.HeartbeatAt > 0) {
		return lockOwnerReplaced(rec, linuxProcessStartUnix(rec.PID), nowUnix)
	}
	return false
}

// LostRun is a worker run still open while nothing holds the lease of its change: the worker that was
// building the change is gone, and the change waits for its lease to be reclaimed.
type LostRun struct {
	ChangeRun
	// Lease is the lease the change still has on record, nil when it has none.
	Lease *LockRecord `json:"lease,omitempty"`
	// LeaseExpiredAt is when that lease ran out, "" when it has not: its owner is dead before then.
	LeaseExpiredAt string `json:"lease_expired_at,omitempty"`
}

// ListLostRuns returns the worker runs that are open without a live lease on their change, the
// oldest first.
func (v *View) ListLostRuns(ctx context.Context) ([]LostRun, error) {
	return v.lostRuns(ctx, "")
}

// lostRuns returns the lost runs of one change, or of every change when change is empty.
func (v *View) lostRuns(ctx context.Context, change string) ([]LostRun, error) {
	var out []LostRun
	var started []int64
	nowUnix := v.now / 1000
	where, args := `r.finished_at IS NULL`, []any{}
	if change != "" {
		where, args = `r.change_name = ? AND r.finished_at IS NULL`, []any{change}
	}
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		var pid, slot, acquired, heartbeat, ttl sql.NullInt64
		var host, key, owner sql.NullString
		var startedMS int64
		r, err := scanChangeRun(func(dest ...any) error {
			return scan(append(dest, &key, &owner, &pid, &host, &slot, &acquired, &heartbeat, &ttl, &startedMS)...)
		})
		if err != nil {
			return err
		}
		lost := LostRun{ChangeRun: r}
		if key.Valid {
			rec := LockRecord{
				LockKey: key.String, OwnerType: owner.String, PID: int(pid.Int64), Host: host.String, Slot: int(slot.Int64),
				AcquiredAt: acquired.Int64 / 1000, HeartbeatAt: heartbeat.Int64 / 1000, TTLSeconds: int(ttl.Int64),
			}
			if !lockGone(rec, v.s.host, nowUnix) {
				return nil
			}
			lost.Lease = &rec
			if expires := heartbeat.Int64 + ttl.Int64*1000; expires < v.now {
				lost.LeaseExpiredAt = msToRFC3339(expires)
			}
		}
		out = append(out, lost)
		started = append(started, startedMS)
		return nil
	}, `
		SELECT `+changeRunColumns+`, l.lock_key, l.owner_type, l.pid, l.host, l.slot, l.acquired_at, l.heartbeat_at, l.ttl_seconds, r.started_at
		FROM change_runs r LEFT JOIN locks l ON l.lock_key = '`+WorkerLeasePrefix+`' || r.change_name
		WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	// The statement has no ORDER BY on purpose: with one, SQLite walks every run in that order
	// instead of reading the open ones from their index. The open runs are few; they are sorted here.
	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		i, j := order[a], order[b]
		if started[i] != started[j] {
			return started[i] < started[j]
		}
		return out[i].ID < out[j].ID
	})
	sorted := make([]LostRun, len(out))
	for at, i := range order {
		sorted[at] = out[i]
	}
	return sorted, nil
}

// Proposal is a proposal as the mirror of .foreman-kit/agent_proposals.json holds it.
type Proposal struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Status is "proposed", "approved", "queued", "integrated" or "rejected", as the file of the main
	// checkout says, and "unknown" for a proposal a cycle made that the main checkout does not list yet.
	Status       string  `json:"status"`
	Priority     string  `json:"priority"`
	ImpactScore  float64 `json:"impact_score,omitempty"`
	Source       string  `json:"source,omitempty"`
	Strategy     string  `json:"strategy,omitempty"`
	Description  string  `json:"description,omitempty"`
	Evidence     string  `json:"evidence,omitempty"`
	TargetPrompt string  `json:"target_prompt,omitempty"`
	CycleStamp   string  `json:"cycle_stamp,omitempty"`
	// ReviewJSON is what the judge answered about the proposal, as it was recorded.
	ReviewJSON   string `json:"review_json,omitempty"`
	ProposedAt   string `json:"proposed_at"`
	QueuedAt     string `json:"queued_at,omitempty"`
	IntegratedAt string `json:"integrated_at,omitempty"`
}

const proposalColumns = `proposal_id, kind, status, priority, impact_score, source, strategy, description, evidence,
	target_prompt, cycle_stamp, review_json, proposed_at, queued_at, integrated_at`

func scanProposal(scan func(dest ...any) error) (Proposal, error) {
	var p Proposal
	var impact sql.NullFloat64
	var source, strategy, evidence, prompt, cycle, review sql.NullString
	var queued, integrated sql.NullInt64
	var proposed int64
	err := scan(&p.ID, &p.Kind, &p.Status, &p.Priority, &impact, &source, &strategy, &p.Description, &evidence,
		&prompt, &cycle, &review, &proposed, &queued, &integrated)
	p.ImpactScore = impact.Float64
	p.Source, p.Strategy, p.Evidence, p.TargetPrompt = source.String, strategy.String, evidence.String, prompt.String
	p.CycleStamp, p.ReviewJSON = cycle.String, review.String
	p.ProposedAt = msToRFC3339(proposed)
	p.QueuedAt = nullMSToRFC3339(queued)
	p.IntegratedAt = nullMSToRFC3339(integrated)
	return p, err
}

// ProposalPage is one page of proposals, the latest first.
type ProposalPage struct {
	Items []Proposal `json:"items"`
	Next  string     `json:"next,omitempty"`
}

// ListProposals returns one page of the proposals in a status, the latest first, paged by cursor
// over (proposed_at, id).
func (v *View) ListProposals(ctx context.Context, status, before string, limit int) (ProposalPage, error) {
	var page ProposalPage
	at, key, err := timeCursor(before)
	if err != nil {
		return page, err
	}
	limit = pageSize(limit)
	var proposed []int64
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var ms int64
		p, err := scanProposal(func(dest ...any) error { return scan(append(dest, &ms)...) })
		if err == nil {
			page.Items = append(page.Items, p)
			proposed = append(proposed, ms)
		}
		return err
	}, `SELECT `+proposalColumns+`, proposed_at FROM proposals
	    WHERE status = ? AND (proposed_at, proposal_id) < (?, ?)
	    ORDER BY proposed_at DESC, proposal_id DESC LIMIT ?`, status, at, key, limit+1)
	if err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = formatTimeCursor(proposed[limit-1], page.Items[limit-1].ID)
	}
	return page, nil
}

// OpenBlock is a block in force, with the quarantine of its change: how many times the change was
// retried and rebuilt since its counters last started over, and whether its risk is accepted.
type OpenBlock struct {
	BlockRecord
	Quarantine QuarantineRecord `json:"quarantine"`
}

// Inbox is what waits for a person, read from the rows that are open: nothing in it is worked out
// from events or from files.
type Inbox struct {
	// Blocked holds the blocks in force, the oldest first.
	Blocked []OpenBlock `json:"blocked"`
	// BrokenBases holds the repositories whose base fails on its own and was not accepted.
	BrokenBases []BrokenBase `json:"broken_bases"`
	// Proposals holds the proposals nobody decided on, the oldest first.
	Proposals []Proposal `json:"proposals"`
	// LostRuns holds the worker runs open without a live lease.
	LostRuns []LostRun `json:"lost_runs"`
	// Pause is the pause in force, nil when the workspace is not paused.
	Pause *Pause `json:"pause,omitempty"`
}

// Count returns how many things wait for a person.
func (in Inbox) Count() int {
	n := len(in.Blocked) + len(in.BrokenBases) + len(in.Proposals) + len(in.LostRuns)
	if in.Pause != nil {
		n++
	}
	return n
}

// ListInbox returns what waits for a person.
func (v *View) ListInbox(ctx context.Context) (Inbox, error) {
	in := Inbox{Blocked: []OpenBlock{}, BrokenBases: []BrokenBase{}, Proposals: []Proposal{}, LostRuns: []LostRun{}}

	// "+blocked_at": ordering by the bare column makes SQLite walk every block in the order of the
	// index of blocked_at, when the blocks in force are read from the index of the open ones.
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		b, err := scanBlockRecord(scan)
		if err == nil {
			in.Blocked = append(in.Blocked, OpenBlock{BlockRecord: b})
		}
		return err
	}, `SELECT `+blockRecordColumns+` FROM change_blocks WHERE resolved_at IS NULL ORDER BY +blocked_at, id`)
	if err != nil {
		return in, err
	}
	for i := range in.Blocked {
		st, err := v.s.loadQuarantineTx(ctx, v.q, in.Blocked[i].ChangeName)
		if err != nil {
			return in, err
		}
		if rec := st.record(); rec != nil {
			in.Blocked[i].Quarantine = *rec
		}
	}

	bases, err := listBrokenBases(ctx, v.q)
	if err != nil {
		return in, err
	}
	if bases != nil {
		in.BrokenBases = bases
	}

	err = v.each(ctx, func(scan func(dest ...any) error) error {
		p, err := scanProposal(scan)
		if err == nil {
			in.Proposals = append(in.Proposals, p)
		}
		return err
	}, `SELECT `+proposalColumns+` FROM proposals WHERE status = 'proposed' ORDER BY proposed_at, proposal_id`)
	if err != nil {
		return in, err
	}

	in.LostRuns, err = v.ListLostRuns(ctx)
	if err != nil {
		return in, err
	}

	in.Pause, err = v.OpenPause(ctx)
	return in, err
}

// Kinds of fact that hold a queued change back (the kinds of v_wait_facts).
const (
	WaitDependency = "dependency" // a change it depends on is not integrated
	WaitBlock      = "block"      // it is blocked
	WaitDeferral   = "deferral"   // no engine of a role could work on it
	WaitLock       = "lock"       // it declares a lock that a change in progress declares too
)

// WaitFact is one recorded fact that holds a queued change back.
type WaitFact struct {
	Kind string `json:"kind"`
	// OtherChange and OtherState name the change it waits for: the dependency, or the change in
	// progress that declares the same lock.
	OtherChange string `json:"other_change,omitempty"`
	OtherState  string `json:"other_state,omitempty"`
	LockName    string `json:"lock_name,omitempty"`
	// Reason is the reason of the block, or the role no engine could take.
	Reason string `json:"reason,omitempty"`
	Since  string `json:"since,omitempty"`
	// Until is when a deferred change is tried again, when that is known.
	Until string `json:"until,omitempty"`
}

// scanWaitFact reads a row of v_wait_facts: the change it is about, into change, and the fact.
func scanWaitFact(scan func(dest ...any) error, change *string) (WaitFact, error) {
	var f WaitFact
	var other, otherState, lock, reason sql.NullString
	var since, until sql.NullInt64
	if err := scan(change, &f.Kind, &other, &otherState, &lock, &reason, &since, &until); err != nil {
		return f, err
	}
	f.OtherChange, f.OtherState, f.LockName, f.Reason = other.String, otherState.String, lock.String, reason.String
	f.Since = nullMSToRFC3339(since)
	f.Until = nullMSToRFC3339(until)
	return f, nil
}

// QueuedChange is a change of the backlog with the recorded facts that hold it back.
type QueuedChange struct {
	ChangeOverview
	Facts []WaitFact `json:"facts"`
}

// ListWaitFacts returns the changes of the backlog, by priority and then by how long they have been
// queued, each with the recorded facts that hold it back. It says why a change waits; it does not
// say which one starts next, which the dispatcher decides with what it has in memory and in the
// worktrees.
func (v *View) ListWaitFacts(ctx context.Context) ([]QueuedChange, error) {
	var out []QueuedChange
	at := map[string]int{}
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		c, err := scanChangeOverview(scan)
		if err == nil {
			at[c.ChangeName] = len(out)
			out = append(out, QueuedChange{ChangeOverview: c, Facts: []WaitFact{}})
		}
		return err
	}, `
		SELECT `+changeOverviewColumns+` FROM v_change_overview o
		WHERE o.backlog_status = 'queued'
		ORDER BY CASE o.priority WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 WHEN 'low' THEN 3 ELSE 4 END,
		         COALESCE(o.queued_at, o.first_seen_at), o.change_name
	`)
	if err != nil || len(out) == 0 {
		return out, err
	}
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var change string
		f, err := scanWaitFact(scan, &change)
		if err != nil {
			return err
		}
		if i, ok := at[change]; ok {
			out[i].Facts = append(out[i].Facts, f)
		}
		return nil
	}, `
		SELECT change_name, kind, other_change, other_state, lock_name, reason, since_at, until_at
		FROM v_wait_facts ORDER BY change_name, kind, other_change, lock_name, reason
	`)
	return out, err
}

// QueuedManifest is a manifest waiting for the merger, with what the merger did with it so far.
type QueuedManifest struct {
	ManifestRecord
	// Risk is the evaluation of the change's risk its worker asked for, nil when it asked for none.
	Risk *Evaluation `json:"risk,omitempty"`
	// Attempts counts the merge attempts on the manifest; Last is the latest of them.
	Attempts int           `json:"attempts"`
	Last     *MergeAttempt `json:"last,omitempty"`
	// Pushes holds the child repositories already pushed for it.
	Pushes []ManifestPush `json:"pushes"`
}

// MergeQueue is what waits for the merger, what the merger does now and what it did last.
type MergeQueue struct {
	// Current is the attempt in progress, nil while the merger is idle.
	Current *MergeAttempt `json:"current,omitempty"`
	// Waiting holds the manifests in the queue, the one waiting for longest first.
	Waiting []QueuedManifest `json:"waiting"`
	// Recent holds the attempts that ended, the latest first; Next is the cursor of the ones before.
	Recent []MergeAttempt `json:"recent"`
	Next   string         `json:"next,omitempty"`
	// BrokenBases holds the repositories whose base fails on its own: the merger holds the changes
	// that touch them.
	BrokenBases []BrokenBase `json:"broken_bases"`
}

// MergeQueue returns the merge queue. before is the cursor of the attempts that ended ("" for the
// latest); limit, how many of them the page holds.
func (v *View) MergeQueue(ctx context.Context, before string, limit int) (MergeQueue, error) {
	mq := MergeQueue{Waiting: []QueuedManifest{}, Recent: []MergeAttempt{}, BrokenBases: []BrokenBase{}}
	cursor, err := idCursor(before)
	if err != nil {
		return mq, err
	}
	limit = pageSize(limit)
	const attempts = ` FROM merge_attempts m JOIN ready_manifests r ON r.id = m.manifest_id`

	cur, err := scanMergeAttempt(v.q.QueryRowContext(ctx, `SELECT `+mergeAttemptColumns+attempts+` WHERE m.finished_at IS NULL`).Scan)
	switch {
	case err == sql.ErrNoRows:
	case err != nil:
		return mq, err
	default:
		mq.Current = &cur
	}

	// The manifests in the queue, in the order the merger takes them.
	var ids []any
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var id int64
		if err := scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
		return nil
	}, `SELECT id FROM ready_manifests WHERE closed_at IS NULL ORDER BY enqueued_at, change_name`)
	if err != nil {
		return mq, err
	}
	records, err := v.loadManifestRecords(ctx, ids)
	if err != nil {
		return mq, err
	}
	at := map[int64]int{}
	var riskIDs []any
	for _, id := range ids {
		rec := records[id.(int64)]
		if rec == nil {
			continue
		}
		at[rec.ID] = len(mq.Waiting)
		mq.Waiting = append(mq.Waiting, QueuedManifest{ManifestRecord: *rec, Pushes: []ManifestPush{}})
		if rec.RiskEvaluationID != 0 {
			riskIDs = append(riskIDs, rec.RiskEvaluationID)
		}
	}
	risks := map[int64]*Evaluation{}
	if err := chunked(riskIDs, func(part []any) error {
		return v.each(ctx, func(scan func(dest ...any) error) error {
			ev, err := scanEvaluation(scan)
			if err == nil {
				risks[ev.ID] = &ev
			}
			return err
		}, `SELECT `+evaluationColumns+` FROM evaluations v WHERE v.id IN (`+placeholders(len(part))+`)`, part...)
	}); err != nil {
		return mq, err
	}
	for i := range mq.Waiting {
		mq.Waiting[i].Risk = risks[mq.Waiting[i].RiskEvaluationID]
	}
	if err := chunked(ids, func(part []any) error {
		if err := v.each(ctx, func(scan func(dest ...any) error) error {
			a, err := scanMergeAttempt(scan)
			if err != nil {
				return err
			}
			if i, ok := at[a.ManifestID]; ok {
				mq.Waiting[i].Attempts++
				mq.Waiting[i].Last = &a
			}
			return nil
		}, `SELECT `+mergeAttemptColumns+attempts+` WHERE m.manifest_id IN (`+placeholders(len(part))+`) ORDER BY m.id`, part...); err != nil {
			return err
		}
		return v.each(ctx, func(scan func(dest ...any) error) error {
			var p ManifestPush
			var pushed int64
			if err := scan(&p.ManifestID, &p.RepoPath, &p.PushedSHA, &p.MergeAttemptID, &pushed); err != nil {
				return err
			}
			p.PushedAt = msToRFC3339(pushed)
			if i, ok := at[p.ManifestID]; ok {
				mq.Waiting[i].Pushes = append(mq.Waiting[i].Pushes, p)
			}
			return nil
		}, `SELECT manifest_id, repo_path, pushed_sha, merge_attempt_id, pushed_at FROM manifest_pushes
		    WHERE manifest_id IN (`+placeholders(len(part))+`) ORDER BY pushed_at, repo_path`, part...)
	}); err != nil {
		return mq, err
	}

	err = v.each(ctx, func(scan func(dest ...any) error) error {
		a, err := scanMergeAttempt(scan)
		if err == nil {
			mq.Recent = append(mq.Recent, a)
		}
		return err
	}, `SELECT `+mergeAttemptColumns+attempts+` WHERE m.finished_at IS NOT NULL AND m.id < ? ORDER BY m.id DESC LIMIT ?`, cursor, limit+1)
	if err != nil {
		return mq, err
	}
	if len(mq.Recent) > limit {
		mq.Recent = mq.Recent[:limit]
		mq.Next = strconv.FormatInt(mq.Recent[limit-1].ID, 10)
	}

	bases, err := listBrokenBases(ctx, v.q)
	if bases != nil {
		mq.BrokenBases = bases
	}
	return mq, err
}
