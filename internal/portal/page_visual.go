package portal

import (
	"context"
	"net/http"
	"strings"

	"github.com/mayeco/foreman-kit/internal/store"
)

// Visual QA: what there is to look at in a browser. /visual lists the checks a cycle, a merge or a
// person asked for, whole or in one status, with the session in progress; /visual/sessions lists
// the sessions that left a plan, and /visual/sessions/{stamp} shows the plan and the report of one.
// /api/visual and the two addresses under it return the same reads. The checks and the session in
// progress are rows; the plan and the report of a session are files the kit wrote, opened to show
// what they say.

// visualPage is the checks in a browser.
type visualPage struct {
	Setup VisualSetup `json:"setup"`
	// Session is the session in progress, nil when none is open.
	Session *store.VisualSession `json:"session,omitempty"`
	// Counts says how many checks are in each status; Status is the one the page lists, "" for all.
	Counts map[string]int        `json:"counts"`
	Status string                `json:"status,omitempty"`
	Items  []store.VisualQACheck `json:"items"`
	Next   string                `json:"next,omitempty"`

	// Tabs are the statuses the page offers; Total, the checks in all; Sessions, how many sessions
	// left a plan.
	Tabs     []stateTab `json:"-"`
	Total    int        `json:"-"`
	Sessions int        `json:"-"`
	Older    string     `json:"-"`
	Newest   string     `json:"-"`
	// Verdicts are the ways a check ends, for the control that records one.
	Verdicts []string `json:"-"`
}

func (p *visualPage) head(v *pageView) {
	v.Title, v.Section = "Visual QA", "/visual"
	v.Crumbs = []crumb{{Label: "Visual QA"}}
	if p.Session != nil {
		v.Live = 15
	}
}

func (s *Server) readVisual(ctx context.Context, db *store.View, r *http.Request) (*visualPage, error) {
	q := r.URL.Query()
	status := strings.TrimSpace(q.Get("status"))
	known := status == ""
	for _, st := range store.VisualQAStatuses {
		known = known || st == status
	}
	if !known {
		return nil, errInvalid("status is one of %s, and %q is none of them", strings.Join(store.VisualQAStatuses, ", "), status)
	}
	limit, err := limitOf(q)
	if err != nil {
		return nil, err
	}
	before := strings.TrimSpace(q.Get("before"))
	page, err := db.ListVisualQAPage(ctx, status, before, limit)
	if err != nil {
		return nil, err
	}
	counts, err := db.CountVisualQAByStatus(ctx)
	if err != nil {
		return nil, err
	}
	session, err := db.VisualSession(ctx)
	if err != nil {
		return nil, err
	}
	p := &visualPage{Setup: s.work.VisualSetup(), Session: session, Counts: counts, Status: status, Items: page.Items, Next: page.Next, Verdicts: visualVerdicts}
	_, p.Sessions, _ = s.work.VisualSessions("")

	for _, n := range counts {
		p.Total += n
	}
	p.Tabs = append(p.Tabs, stateTab{Label: "All", Count: p.Total, Href: hrefWith(r, "status", "", "before", ""), Active: status == ""})
	for _, st := range store.VisualQAStatuses {
		// A status no check is in is offered only while the page lists it.
		if counts[st] == 0 && st != status {
			continue
		}
		p.Tabs = append(p.Tabs, stateTab{State: st, Label: wordingOf(vocVisualStatus, st).Text, Count: counts[st],
			Href: hrefWith(r, "status", st, "before", ""), Active: st == status})
	}
	if page.Next != "" {
		p.Older = hrefWith(r, "before", page.Next)
	}
	if before != "" {
		p.Newest = hrefWith(r, "before", "")
	}
	return p, nil
}

// visualSessionsPage is the sessions that left a plan, the latest first.
type visualSessionsPage struct {
	// Current is the session in progress, nil when none is open.
	Current  *store.VisualSession  `json:"current,omitempty"`
	Sessions []VisualSessionRecord `json:"sessions"`
	Total    int                   `json:"total"`
	Next     string                `json:"next,omitempty"`

	Setup  VisualSetup `json:"-"`
	Checks int         `json:"-"`
	Older  string      `json:"-"`
	Newest string      `json:"-"`
}

func (p *visualSessionsPage) head(v *pageView) {
	v.Title, v.Section = "Visual QA sessions", "/visual"
	v.Crumbs = []crumb{{Label: "Visual QA", Href: "/visual"}, {Label: "Sessions"}}
}

func (s *Server) readVisualSessions(ctx context.Context, db *store.View, r *http.Request) (*visualSessionsPage, error) {
	before := strings.TrimSpace(r.URL.Query().Get("before"))
	current, err := db.VisualSession(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := db.CountVisualQAByStatus(ctx)
	if err != nil {
		return nil, err
	}
	p := &visualSessionsPage{Current: current, Setup: s.work.VisualSetup()}
	p.Sessions, p.Total, p.Next = s.work.VisualSessions(before)
	for _, n := range counts {
		p.Checks += n
	}
	if p.Next != "" {
		p.Older = hrefWith(r, "before", p.Next)
	}
	if before != "" {
		p.Newest = hrefWith(r, "before", "")
	}
	return p, nil
}

// visualSessionPage is one session: its plan and its report.
type visualSessionPage struct {
	Stamp string `json:"stamp"`
	// Open says the session is the one in progress.
	Open bool `json:"open"`
	// Plan is what the session set out to check; Report, what it found. Each is nil when the session
	// did not write it.
	Plan   *CycleReport `json:"plan,omitempty"`
	Report *CycleReport `json:"report,omitempty"`

	// Showing is the file the page shows, "plan" or "report"; Tabs, the ones the session has.
	Showing string       `json:"-"`
	Tabs    []stateTab   `json:"-"`
	Current *CycleReport `json:"-"`
}

func (p *visualSessionPage) head(v *pageView) {
	v.Title, v.Section = "Visual QA session "+p.Stamp, "/visual"
	v.Crumbs = []crumb{{Label: "Visual QA", Href: "/visual"}, {Label: "Sessions", Href: "/visual/sessions"}, {Label: p.Stamp}}
}

func (s *Server) readVisualSession(ctx context.Context, db *store.View, r *http.Request) (*visualSessionPage, error) {
	stamp := r.PathValue("stamp")
	plan, report := s.work.VisualSessionFiles(stamp)
	if plan == nil && report == nil {
		return nil, errNotFound("no visual QA session left a plan or a report under the stamp %s", stamp)
	}
	current, err := db.VisualSession(ctx)
	if err != nil {
		return nil, err
	}
	p := &visualSessionPage{Stamp: stamp, Open: current != nil && current.Stamp == stamp, Plan: plan, Report: report}
	file := strings.TrimSpace(r.URL.Query().Get("file"))
	switch {
	case file != "" && file != "plan" && file != "report":
		return nil, errInvalid("file is %q or %q, and %q is neither", "plan", "report", file)
	case file == "":
		// The report is what a person comes for, once the session wrote it.
		file = "plan"
		if report != nil {
			file = "report"
		}
	}
	p.Showing = file
	for _, tab := range []struct {
		key, label string
		text       *CycleReport
	}{{"report", "Report", report}, {"plan", "Plan", plan}} {
		if tab.text == nil {
			continue
		}
		p.Tabs = append(p.Tabs, stateTab{State: tab.key, Label: tab.label, Href: hrefWith(r, "file", tab.key), Active: tab.key == file})
		if tab.key == file {
			p.Current = tab.text
		}
	}
	return p, nil
}
