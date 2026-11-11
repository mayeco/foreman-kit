package store

import (
	"context"
	"database/sql"
	"strconv"
)

// CyclePage is one page of the cycles on record, the latest first.
type CyclePage struct {
	Items []Cycle `json:"items"`
	// Next is the cursor of the page that follows, "" when this one is the last.
	Next string `json:"next,omitempty"`
}

// ListCyclesPage returns one page of the cycles, the latest first, paged by cursor over
// (started_at, stamp).
func (v *View) ListCyclesPage(ctx context.Context, before string, limit int) (CyclePage, error) {
	page := CyclePage{Items: []Cycle{}}
	at, key, err := timeCursor(before)
	if err != nil {
		return page, err
	}
	limit = pageSize(limit)
	cycles, instants, err := listCyclesTimed(ctx, v.q, ` WHERE (started_at, stamp) < (?, ?)
	    ORDER BY started_at DESC, stamp DESC LIMIT ?`, at, key, limit+1)
	if err != nil {
		return page, err
	}
	if len(cycles) > limit {
		cycles = cycles[:limit]
		page.Next = formatTimeCursor(instants[limit-1].started, cycles[limit-1].Stamp)
	}
	if cycles != nil {
		page.Items = cycles
	}
	return page, nil
}

// CycleRecord is a cycle with what the kit recorded under it.
type CycleRecord struct {
	Cycle
	// Executions holds the executions of the cycle, the first one first: its judgment, with the
	// engines that were tried for it.
	Executions []ExecutionDetail `json:"executions"`
	// Queued holds the changes the cycle queued, the first one queued first, each as it is now: the
	// ones it added to the backlog (RecordCycleWork), in no queue until the main checkout lists
	// them. A cycle that ran before the kit recorded that has none.
	Queued []ChangeOverview `json:"queued"`
	// Proposals holds the proposals recorded as made by the cycle, the first one first; VisualQA, the
	// checks in a browser it asked for.
	Proposals []Proposal     `json:"proposals"`
	VisualQA  []VisualQAItem `json:"visual_qa"`
	// Newer and Older are the stamps of the cycles next to it, "" at either end.
	Newer string `json:"newer,omitempty"`
	Older string `json:"older,omitempty"`
}

// Cycle returns the cycle named by stamp with what was recorded under it, or nil when there is none.
func (v *View) Cycle(ctx context.Context, stamp string) (*CycleRecord, error) {
	cycles, instants, err := listCyclesTimed(ctx, v.q, ` WHERE stamp = ?`, stamp)
	if err != nil || len(cycles) == 0 {
		return nil, err
	}
	rec := &CycleRecord{Cycle: cycles[0], Executions: []ExecutionDetail{}, Queued: []ChangeOverview{}, Proposals: []Proposal{}, VisualQA: []VisualQAItem{}}

	byExec := map[string]*ExecutionDetail{}
	var execs []*ExecutionDetail
	var ids []any
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		e, err := scanExecution(scan)
		if err == nil {
			d := v.executionDetail(e)
			execs = append(execs, d)
			byExec[d.ExecID] = d
			ids = append(ids, d.ExecID)
		}
		return err
	}, `SELECT `+executionListColumns+` FROM executions e WHERE e.cycle_stamp = ? ORDER BY e.started_at, e.exec_id`, stamp)
	if err != nil {
		return nil, err
	}
	if err := v.loadAttempts(ctx, ids, byExec); err != nil {
		return nil, err
	}
	for _, d := range execs {
		rec.Executions = append(rec.Executions, *d)
	}

	// The changes of the cycle are chosen first, through the index of their cycle; the state and the
	// counters are then read for those alone.
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		c, err := scanChangeOverview(scan)
		if err == nil {
			rec.Queued = append(rec.Queued, c)
		}
		return err
	}, `WITH queued AS (
	        SELECT change_name, COALESCE(queued_at, first_seen_at) AS at FROM changes WHERE cycle_stamp = ?
	    )
	    SELECT `+changeOverviewColumns+`
	    FROM queued JOIN v_change_overview o USING (change_name)
	    ORDER BY queued.at, queued.change_name`, stamp)
	if err != nil {
		return nil, err
	}

	err = v.each(ctx, func(scan func(dest ...any) error) error {
		p, err := scanProposal(scan)
		if err == nil {
			rec.Proposals = append(rec.Proposals, p)
		}
		return err
	}, `SELECT `+proposalColumns+` FROM proposals WHERE cycle_stamp = ? ORDER BY proposed_at, proposal_id`, stamp)
	if err != nil {
		return nil, err
	}

	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var it VisualQAItem
		var id, created int64
		var change, note sql.NullString
		var resolved sql.NullInt64
		if err := scan(&id, &change, &it.What, &it.Status, &note, &created, &resolved); err != nil {
			return err
		}
		it.ItemID = strconv.FormatInt(id, 10)
		it.ChangeName, it.VerdictNote = change.String, note.String
		it.CreatedAt = msToRFC3339(created)
		it.ResolvedAt = nullMSToRFC3339(resolved)
		rec.VisualQA = append(rec.VisualQA, it)
		return nil
	}, `SELECT id, change_name, what, status, verdict_note, created_at, resolved_at FROM visual_qa_items
	    WHERE cycle_stamp = ? ORDER BY created_at, id`, stamp)
	if err != nil {
		return nil, err
	}

	// The cycles next to it, each a search from its own place in the order of the list.
	started := instants[0].started
	next := func(query string) (string, error) {
		var other string
		err := v.q.QueryRowContext(ctx, query, started, stamp).Scan(&other)
		if err == sql.ErrNoRows {
			return "", nil
		}
		return other, err
	}
	if rec.Newer, err = next(`SELECT stamp FROM cycles WHERE (started_at, stamp) > (?, ?) ORDER BY started_at, stamp LIMIT 1`); err != nil {
		return nil, err
	}
	if rec.Older, err = next(`SELECT stamp FROM cycles WHERE (started_at, stamp) < (?, ?) ORDER BY started_at DESC, stamp DESC LIMIT 1`); err != nil {
		return nil, err
	}
	return rec, nil
}

// Event returns the event with that id, or nil when there is none. The event names the rows it is
// about: its change, its execution, its worker run, its merge attempt, its round of gates, its cycle.
func (v *View) Event(ctx context.Context, id int64) (*EventRecord, error) {
	rec, err := scanEvent(v.q.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE id = ?`, id).Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// UnlistedQueued is a change a cycle queued that the backlog of the main checkout has never listed.
// A cycle queues in its own worktree and records what it queued; the main checkout lists the change
// when the cycle ends, which brings that checkout up to date. While a visual QA session is in
// progress nothing moves that checkout, and the change is listed once the session has ended: on the
// next pass of the dispatcher, at the next cycle, or by `foreman-kit sync`. Until then the change
// is in no state of the pipeline (ChangeUnknown), and the registry has no hour at which that
// backlog listed it (changes.listed_at).
//
// A change a person takes out of the backlog later is in no state of the pipeline either, and it is
// not one of these: the registry keeps the hour the backlog first listed it.
//
// Nor is a change whose cycle was followed by one that ended: a cycle that ends brings the main
// checkout up to date, so what an earlier cycle published is listed by then. One still unlisted
// was never published (its cycle was cut between recording what it queued and publishing it) and
// will never be listed: it waits for nothing.
type UnlistedQueued struct {
	ChangeName string `json:"change_name"`
	CycleStamp string `json:"cycle_stamp"`
}

// ListUnlistedQueued returns those changes, the ones of the latest cycle first. The changes a cycle
// queued are chosen first, through the index of their cycle; the state is then read for the ones
// the backlog never listed.
func (v *View) ListUnlistedQueued(ctx context.Context, limit int) ([]UnlistedQueued, error) {
	out := []UnlistedQueued{}
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		var u UnlistedQueued
		if err := scan(&u.ChangeName, &u.CycleStamp); err != nil {
			return err
		}
		out = append(out, u)
		return nil
	}, `WITH queued AS (
	        SELECT change_name, cycle_stamp FROM changes WHERE cycle_stamp IS NOT NULL AND listed_at IS NULL
	    )
	    SELECT queued.change_name, queued.cycle_stamp
	    FROM queued JOIN v_change_state s USING (change_name)
	    JOIN cycles c ON c.stamp = queued.cycle_stamp
	    WHERE s.state = ?
	      AND NOT EXISTS (
	        SELECT 1 FROM cycles later
	        WHERE later.started_at > c.started_at AND later.finished_at IS NOT NULL
	      )
	    ORDER BY queued.cycle_stamp DESC, queued.change_name
	    LIMIT ?`, ChangeUnknown, pageSize(limit))
	return out, err
}
