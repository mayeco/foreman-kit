package portal

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/mayeco/foreman-kit/internal/store"
)

// The list of changes: /changes draws it and /api/changes returns it. The state of a change is the
// one v_change_state gives it; the filter, the search and the total are resolved in SQL over every
// change.

// changesPerPage is how many changes a page of the list holds.
const changesPerPage = 25

// stateTab is one state the list can be narrowed to, with how many changes are in it.
type stateTab struct {
	State  string
	Label  string
	Count  int
	Href   string
	Active bool
}

// changesPage is one page of the list of changes.
type changesPage struct {
	Items []store.ChangeOverview `json:"items"`
	Page  int                    `json:"page"`
	Per   int                    `json:"per"`
	Total int                    `json:"total"`
	Pages int                    `json:"pages"`
	// Counts is how many changes are in each state, among those the search matches.
	Counts map[string]int `json:"counts"`
	// State and Query are the filter the page was asked for.
	State string `json:"state,omitempty"`
	Query string `json:"q,omitempty"`

	// What the page draws around the list.
	Tabs  []stateTab `json:"-"`
	From  int        `json:"-"`
	To    int        `json:"-"`
	Prev  string     `json:"-"`
	Next  string     `json:"-"`
	Clear string     `json:"-"`
}

func (p *changesPage) head(v *pageView) {
	v.Title, v.Section = "Changes", "/changes"
	v.Crumbs = []crumb{{Label: "Changes"}}
}

func (s *Server) readChanges(ctx context.Context, db *store.View, r *http.Request) (*changesPage, error) {
	q := r.URL.Query()
	f := store.ChangeFilter{State: strings.TrimSpace(q.Get("state")), Query: strings.TrimSpace(q.Get("q")), Page: 1, Per: changesPerPage}
	if _, ok := vocabularies[vocChangeState][f.State]; f.State != "" && !ok {
		return nil, errInvalid("%q is not a state of a change: %s", f.State, strings.Join(store.ChangeStates, ", "))
	}
	if raw := strings.TrimSpace(q.Get("page")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, errInvalid("page is a number from 1")
		}
		f.Page = n
	}
	list, err := db.ListChangeOverviews(ctx, f)
	if err != nil {
		return nil, err
	}
	counts, err := db.CountChangesByState(ctx, f.Query)
	if err != nil {
		return nil, err
	}
	p := &changesPage{
		Items: list.Items, Page: list.Page, Per: list.Per, Total: list.Total, Pages: list.Pages,
		Counts: counts, State: f.State, Query: f.Query,
	}
	if p.Items == nil {
		p.Items = []store.ChangeOverview{}
	}

	// The tabs: every change, then each state some change is in, and the one asked for.
	all := 0
	for _, n := range counts {
		all += n
	}
	p.Tabs = append(p.Tabs, stateTab{Label: "All", Count: all, Href: hrefWith(r, "state", "", "page", ""), Active: f.State == ""})
	for _, st := range store.ChangeStates {
		if counts[st] == 0 && st != f.State {
			continue
		}
		p.Tabs = append(p.Tabs, stateTab{State: st, Label: wordingOf(vocChangeState, st).Text, Count: counts[st],
			Href: hrefWith(r, "state", st, "page", ""), Active: st == f.State})
	}
	if len(p.Items) > 0 {
		p.From = (p.Page-1)*p.Per + 1
		p.To = p.From + len(p.Items) - 1
	}
	if p.Page > 1 {
		p.Prev = hrefWith(r, "page", pageParam(p.Page-1))
	}
	if p.Page < p.Pages {
		p.Next = hrefWith(r, "page", pageParam(p.Page+1))
	}
	if f.Query != "" {
		p.Clear = hrefWith(r, "q", "", "page", "")
	}
	return p, nil
}

// pageParam is the page parameter of an address: the first page has none.
func pageParam(n int) string {
	if n <= 1 {
		return ""
	}
	return strconv.Itoa(n)
}
