package store

import (
	"context"
	"database/sql"
	"strings"
)

// States of a change, as v_change_state derives them from the rows that are open.
const (
	ChangeQueued           = "queued"
	ChangeBuilding         = "building"
	ChangeReady            = "ready"
	ChangeMerging          = "merging"
	ChangeBlocked          = "blocked"
	ChangeRebuildRequested = "rebuild_requested"
	ChangeIntegrated       = "integrated"
	ChangeDiscarded        = "discarded"
	ChangeUnknown          = "unknown"
)

// ChangeStates lists every state a change can be in, in the order a change goes through them.
var ChangeStates = []string{
	ChangeQueued, ChangeBuilding, ChangeReady, ChangeMerging, ChangeBlocked, ChangeRebuildRequested,
	ChangeIntegrated, ChangeDiscarded, ChangeUnknown,
}

// ChangeOverview is a change as a list shows it: its state, how much work it took and when
// something last happened to it.
type ChangeOverview struct {
	ChangeName       string `json:"change_name"`
	Description      string `json:"description,omitempty"`
	Priority         string `json:"priority"`
	BacklogStatus    string `json:"backlog_status"`
	State            string `json:"state"`
	FirstSeenAt      string `json:"first_seen_at"`
	QueuedAt         string `json:"queued_at,omitempty"`
	IntegratedAt     string `json:"integrated_at,omitempty"`
	IntegratedCommit string `json:"integrated_commit,omitempty"`
	DiscardedAt      string `json:"discarded_at,omitempty"`
	Runs             int    `json:"runs"`
	Executions       int    `json:"executions"`
	Repairs          int    `json:"repairs"`
	// OpenBlockReason is the reason of the block in force, "" when the change is not blocked.
	OpenBlockReason string `json:"open_block_reason,omitempty"`
	// OpenDeferrals counts the times no engine could work on the change since one last did.
	OpenDeferrals int `json:"open_deferrals,omitempty"`
	// LastRunResult is how the latest worker run ended, "" while it runs or when there is none.
	LastRunResult  string `json:"last_run_result,omitempty"`
	LastActivityAt string `json:"last_activity_at"`
}

const changeOverviewColumns = `o.change_name, o.description, o.priority, o.backlog_status, o.state,
	o.first_seen_at, o.queued_at, o.integrated_at, o.integrated_commit, o.discarded_at,
	o.runs, o.executions, o.repairs, o.open_block_reason, o.open_deferrals, o.last_run_result, o.last_activity_at`

func scanChangeOverview(scan func(dest ...any) error) (ChangeOverview, error) {
	var c ChangeOverview
	var queued, integrated, discarded sql.NullInt64
	var commit, block, lastRun sql.NullString
	var firstSeen, lastActivity int64
	err := scan(&c.ChangeName, &c.Description, &c.Priority, &c.BacklogStatus, &c.State,
		&firstSeen, &queued, &integrated, &commit, &discarded,
		&c.Runs, &c.Executions, &c.Repairs, &block, &c.OpenDeferrals, &lastRun, &lastActivity)
	c.FirstSeenAt = msToRFC3339(firstSeen)
	c.QueuedAt = nullMSToRFC3339(queued)
	c.IntegratedAt = nullMSToRFC3339(integrated)
	c.IntegratedCommit = commit.String
	c.DiscardedAt = nullMSToRFC3339(discarded)
	c.OpenBlockReason = block.String
	c.LastRunResult = lastRun.String
	c.LastActivityAt = msToRFC3339(lastActivity)
	return c, err
}

// ChangeFilter selects a page of the list of changes.
type ChangeFilter struct {
	// State keeps the changes in one state of ChangeStates; every state when it is empty.
	State string
	// Query keeps the changes whose name or description contains it.
	Query string
	// Page is the page to return, from 1; Per, how many changes a page holds.
	Page int
	Per  int
}

// ChangePage is one page of the list of changes, with how many changes the filter matches in all.
type ChangePage struct {
	Items []ChangeOverview `json:"items"`
	Page  int              `json:"page"`
	Per   int              `json:"per"`
	Total int              `json:"total"`
	Pages int              `json:"pages"`
}

// changeWhere returns the conditions of a filter over rows that have the state of a change as
// s.state and its name and description as columns of text.
func changeWhere(state, query, text string) (string, []any) {
	var conds []string
	var args []any
	if state != "" {
		conds = append(conds, `s.state = ?`)
		args = append(args, state)
	}
	if q := strings.TrimSpace(query); q != "" {
		conds = append(conds, `(`+text+`.change_name LIKE ? ESCAPE '\' OR `+text+`.description LIKE ? ESCAPE '\')`)
		args = append(args, likePattern(q), likePattern(q))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return ` WHERE ` + strings.Join(conds, ` AND `), args
}

// changeStatesWhere returns the FROM and WHERE clauses that select the states of the changes a
// filter matches. The catalog is joined only when the filter reads the description.
func changeStatesWhere(state, query string) (string, []any) {
	if strings.TrimSpace(query) == "" {
		where, args := changeWhere(state, "", "")
		return ` FROM v_change_state s` + where, args
	}
	where, args := changeWhere(state, query, "c")
	return ` FROM v_change_state s JOIN changes c USING (change_name)` + where, args
}

// ListChangeOverviews returns one page of the changes that match f, the one something happened to
// most recently first. The filter and the total are resolved over every change, in SQL: the list
// pages by number because its order is a derived value, and it grows with the backlog, not with use.
func (v *View) ListChangeOverviews(ctx context.Context, f ChangeFilter) (ChangePage, error) {
	page := ChangePage{Page: f.Page, Per: pageSize(f.Per)}
	if page.Page < 1 {
		page.Page = 1
	}
	from, args := changeStatesWhere(f.State, f.Query)
	if err := v.q.QueryRowContext(ctx, `SELECT COUNT(*)`+from, args...).Scan(&page.Total); err != nil {
		return page, err
	}
	page.Pages = (page.Total + page.Per - 1) / page.Per
	// A page past the end is the last page, and the only page of an empty list is the first.
	page.Page = max(1, min(page.Page, page.Pages))

	// The names of the page are chosen first, with only the state and the last activity of each change
	// worked out; the counters are then read for the changes of the page alone.
	where, args := changeWhere(f.State, f.Query, "s")
	query := `
		WITH page AS (
			SELECT s.change_name, s.last_activity_at
			FROM v_change_overview s` + where + `
			ORDER BY s.last_activity_at DESC, s.change_name
			LIMIT ? OFFSET ?
		)
		SELECT ` + changeOverviewColumns + `
		FROM page JOIN v_change_overview o USING (change_name)
		ORDER BY page.last_activity_at DESC, page.change_name`
	args = append(args, page.Per, (page.Page-1)*page.Per)
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		c, err := scanChangeOverview(scan)
		if err == nil {
			page.Items = append(page.Items, c)
		}
		return err
	}, query, args...)
	return page, err
}

// CountChangesByState returns how many changes are in each state, among those whose name or
// description contains query (among all of them when it is empty). Every state of ChangeStates has
// an entry.
func (v *View) CountChangesByState(ctx context.Context, query string) (map[string]int, error) {
	out := make(map[string]int, len(ChangeStates))
	for _, st := range ChangeStates {
		out[st] = 0
	}
	from, args := changeStatesWhere("", query)
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		var state string
		var n int
		if err := scan(&state, &n); err != nil {
			return err
		}
		out[state] = n
		return nil
	}, `SELECT s.state, COUNT(*)`+from+` GROUP BY s.state`, args...)
	return out, err
}
