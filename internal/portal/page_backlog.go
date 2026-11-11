package portal

import (
	"context"
	"net/http"
	"strings"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// Backlog: what comes next, and why each change waits. /backlog lists the changes of the queue with
// the recorded facts that hold each one back, /backlog/proposals lists the proposals, and
// /api/backlog returns both. The page says why a change waits; it does not say which one starts
// next: that is the dispatcher's to say, and a person asks it with "Simulate dispatch".

// The statuses a proposal goes through, in the order a person follows them.
var proposalStatuses = []string{"proposed", "approved", "queued", "integrated", "rejected"}

// proposalsPerPage is how many proposals a page of the list holds.
const proposalsPerPage = 25

// backlogPage is the queue and the proposals.
type backlogPage struct {
	// Queue holds the changes of the backlog, by priority and then by how long they have waited,
	// each with the recorded facts that hold it back.
	Queue []store.QueuedChange `json:"queue"`
	// Proposals is one page of the proposals in Status, the latest first.
	Proposals store.ProposalPage `json:"proposals"`
	Status    string             `json:"status"`
	// BacklogProblem says what keeps the file of the backlog from being read, "" when nothing does:
	// a backlog that is missing reads as an empty queue, and the page says so instead.
	BacklogProblem string `json:"backlog_problem"`
	BacklogPath    string `json:"-"`

	// What the page draws around the lists.
	Tab      string     `json:"-"`
	Waiting  int        `json:"-"`
	Statuses []stateTab `json:"-"`
	Older    string     `json:"-"`
	Newest   string     `json:"-"`
	// Priorities are the priorities the form that adds a change offers.
	Priorities []string `json:"-"`
}

func (p *backlogPage) head(v *pageView) {
	v.Title, v.Section = "Backlog", "/backlog"
	v.Crumbs = []crumb{{Label: "Backlog"}}
	if p.Tab == "proposals" {
		v.Title = "Proposals"
		v.Crumbs = []crumb{{Label: "Backlog", Href: "/backlog"}, {Label: "Proposals"}}
		return
	}
	// The queue moves while the kit works.
	v.Live = 15
}

// readBacklog returns the read of one tab of the page: "queue" or "proposals". The API takes the
// same read, with both lists.
func (s *Server) readBacklog(tab string) read[*backlogPage] {
	return func(ctx context.Context, db *store.View, r *http.Request) (*backlogPage, error) {
		q := r.URL.Query()
		p := &backlogPage{Tab: tab, Status: strings.TrimSpace(q.Get("status")), Priorities: []string{"normal", "high", "critical", "low"},
			BacklogProblem: s.work.BacklogProblem(), BacklogPath: cycle.BacklogFile}
		if p.Status == "" {
			p.Status = "proposed"
		}
		known := false
		for _, st := range proposalStatuses {
			known = known || st == p.Status
		}
		if !known {
			return nil, errInvalid("%q is not a status of a proposal: %s", p.Status, strings.Join(proposalStatuses, ", "))
		}
		limit, err := limitOf(q)
		if err != nil {
			return nil, err
		}
		if limit == 0 {
			limit = proposalsPerPage
		}
		before := strings.TrimSpace(q.Get("before"))

		if p.Queue, err = db.ListWaitFacts(ctx); err != nil {
			return nil, err
		}
		if p.Queue == nil {
			p.Queue = []store.QueuedChange{}
		}
		for _, c := range p.Queue {
			if c.State == store.ChangeQueued {
				p.Waiting++
			}
		}
		if p.Proposals, err = db.ListProposals(ctx, p.Status, before, limit); err != nil {
			return nil, err
		}
		if p.Proposals.Items == nil {
			p.Proposals.Items = []store.Proposal{}
		}

		for _, st := range proposalStatuses {
			p.Statuses = append(p.Statuses, stateTab{State: st, Label: wordingOf(vocProposalStatus, st).Text,
				Href: hrefWith(r, "status", st, "before", ""), Active: st == p.Status})
		}
		if p.Proposals.Next != "" {
			p.Older = hrefWith(r, "before", p.Proposals.Next)
		}
		if before != "" {
			p.Newest = hrefWith(r, "before", "")
		}
		return p, nil
	}
}
