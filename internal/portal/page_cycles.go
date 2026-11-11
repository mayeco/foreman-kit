package portal

import (
	"context"
	"net/http"
	"strings"

	"github.com/mayeco/foreman-kit/internal/store"
)

// Cycles: what each cycle judged. /cycles lists the table of cycles and /cycles/{stamp} shows one,
// with what was recorded under it (its judgment, the changes it queued, the proposals it made, the
// checks in a browser it asked for); /api/cycles and /api/cycles/{stamp} return the same reads.

// cyclesPage is one page of the cycles, the latest first.
type cyclesPage struct {
	Items []store.Cycle `json:"items"`
	Next  string        `json:"next,omitempty"`

	Older  string `json:"-"`
	Newest string `json:"-"`
	// Running says the latest cycle is still going: the page keeps itself up to date.
	Running bool `json:"-"`
}

func (p *cyclesPage) head(v *pageView) {
	v.Title, v.Section = "Cycles", "/cycles"
	v.Crumbs = []crumb{{Label: "Cycles"}}
	if p.Running {
		v.Live = 15
	}
}

func (s *Server) readCycles(ctx context.Context, db *store.View, r *http.Request) (*cyclesPage, error) {
	q := r.URL.Query()
	limit, err := limitOf(q)
	if err != nil {
		return nil, err
	}
	before := strings.TrimSpace(q.Get("before"))
	page, err := db.ListCyclesPage(ctx, before, limit)
	if err != nil {
		return nil, err
	}
	p := &cyclesPage{Items: page.Items, Next: page.Next}
	if page.Next != "" {
		p.Older = hrefWith(r, "before", page.Next)
	}
	if before != "" {
		p.Newest = hrefWith(r, "before", "")
	}
	p.Running = before == "" && len(p.Items) > 0 && p.Items[0].FinishedAt == ""
	return p, nil
}

// cyclePage is one cycle.
type cyclePage struct {
	*store.CycleRecord
	// Report is the report of the cycle, nil when none is found.
	Report *CycleReport `json:"report,omitempty"`

	// Rows holds its executions as the page draws them.
	Rows []executionRow `json:"-"`
}

func (p *cyclePage) head(v *pageView) {
	v.Title, v.Section = "Cycle "+p.Stamp, "/cycles"
	v.Crumbs = []crumb{{Label: "Cycles", Href: "/cycles"}, {Label: p.Stamp}}
	if p.FinishedAt == "" {
		v.Live = 10
	}
}

func (s *Server) readCycle(ctx context.Context, db *store.View, r *http.Request) (*cyclePage, error) {
	stamp := r.PathValue("stamp")
	rec, err := db.Cycle(ctx, stamp)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errNotFound("no cycle is on record with the stamp %s", stamp)
	}
	p := &cyclePage{CycleRecord: rec, Report: s.work.CycleReport(rec.Cycle)}
	idle, err := db.ListIdleAgents(ctx)
	if err != nil {
		return nil, err
	}
	for _, ex := range rec.Executions {
		row := executionRow{ExecutionDetail: ex, Name: executionName(ex.Execution), About: executionAbout(ex.Execution)}
		for i := range idle {
			if idle[i].ExecID == ex.ExecID {
				row.Idle = &idle[i]
			}
		}
		p.Rows = append(p.Rows, row)
	}
	return p, nil
}
