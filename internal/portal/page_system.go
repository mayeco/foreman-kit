package portal

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// System: how the kit is installed and what runs for it as a whole. Each of its pages is one tab of
// /system, and /api/system/{tab} returns the read of that tab.

// preflightKinds are the two operations that run the preflight of the kit: all of it, or only the
// services its repositories need.
var preflightKinds = []string{"preflight", "preflight-services"}

// healthOutputBytes is how much of the output of the last preflight the page of health shows.
const healthOutputBytes = 64 << 10

// healthPage is the preflight of the kit as a person last ran it from the portal. The preflight is
// what a cycle runs first: the toolchain, the files of the kit, the services the repositories need,
// a ping of the agents and the canary of the evaluator. It is an operation of the kit, run as one:
// what is here is the execution that ran it.
type healthPage struct {
	// Last is the latest preflight a person started from the portal, nil when none was started.
	Last *store.ExecutionDetail `json:"last"`
	// Output is what it printed: all of it when it is short, its end otherwise.
	Output *store.LogTail `json:"output,omitempty"`
}

// Running says the last preflight is still going.
func (p *healthPage) Running() bool {
	return p.Last != nil && p.Last.Status == "running" && !p.Last.OwnerGone
}

// ServicesOnly says the last preflight checked only the services.
func (p *healthPage) ServicesOnly() bool {
	return p.Last != nil && p.Last.JobKind == "preflight-services"
}

func (p *healthPage) head(v *pageView) {
	v.Title, v.Section = "Health", "/system/health"
	v.Crumbs = []crumb{{Label: "System", Href: "/system/health"}, {Label: "Health"}}
	if p.Running() {
		v.Live = 5
	}
}

func (s *Server) readHealth(ctx context.Context, db *store.View, _ *http.Request) (*healthPage, error) {
	last, err := db.LatestJob(ctx, preflightKinds...)
	if err != nil {
		return nil, err
	}
	p := &healthPage{Last: last}
	if last != nil {
		tail, err := db.LogTail(ctx, last.ExecID, healthOutputBytes)
		if err != nil {
			return nil, err
		}
		p.Output = &tail
	}
	return p, nil
}

// setupPage is the checklist that takes this application from installed to ready to schedule, as
// the kit works it out from its files and from what it has on record.
type setupPage struct {
	SetupReport
}

func (p *setupPage) head(v *pageView) {
	v.Title, v.Section = "Setup", "/system/setup"
	v.Crumbs = []crumb{{Label: "System", Href: "/system/health"}, {Label: "Setup"}}
}

func (s *Server) askSetup(ctx context.Context, _ *http.Request) (*setupPage, error) {
	rep, err := s.work.Setup(ctx)
	if err != nil {
		return nil, err
	}
	return &setupPage{SetupReport: rep}, nil
}

// settingsPage is the configuration of the application: its files with what the check of the kit
// says of each, the settings of one of them as the catalog of the kit describes them, and the one a
// person asked to change.
type settingsPage struct {
	SettingsReport
	// Recommends says the kit has enough on record to say which settings it would change: the tab
	// of the recommendations exists.
	Recommends bool `json:"recommends"`

	// Tabs are the files, each with what the check found in it and the one shown marked.
	Tabs []settingsTab `json:"-"`
	// Found lists where the check found something, each with the way to it.
	Found []foundIn `json:"-"`
}

// settingsTab is one configuration file the page of settings can show, with how many errors and
// warnings the check found in it.
type settingsTab struct {
	Label, Href      string
	Errors, Warnings int
	Active           bool
}

// foundIn is something the check found errors or warnings in, with where the page says them.
type foundIn struct {
	Name, Href string
}

// settingsOthers is where the page of settings says what the check found outside the files of the
// catalog.
const settingsOthers = "settings-others"

func (p *settingsPage) head(v *pageView) {
	v.Title, v.Section = "Settings", "/system/settings"
	v.Crumbs = []crumb{{Label: "System", Href: "/system/health"}, {Label: "Settings"}}
}

// settingHref is the address where a person changes the setting called key: the page of settings
// with the editor of that setting open.
func settingHref(key any) string {
	return "/system/settings?edit=" + url.QueryEscape(fmt.Sprint(key)) + "#edit"
}

func (s *Server) askSettings(ctx context.Context, r *http.Request) (*settingsPage, error) {
	q := r.URL.Query()
	rep, err := s.work.Settings(q.Get("file"), q.Get("edit"))
	if err != nil {
		return nil, err
	}
	rec, err := s.work.Recommendations(ctx, "")
	if err != nil {
		return nil, err
	}
	p := &settingsPage{SettingsReport: rep, Recommends: rec.Ready}
	for _, f := range rep.Files {
		tab := settingsTab{Label: f.Name, Href: "/system/settings?file=" + url.QueryEscape(f.Name),
			Errors: len(f.Errors), Warnings: len(f.Warnings), Active: f.Name == rep.File.Name}
		p.Tabs = append(p.Tabs, tab)
		if tab.Errors+tab.Warnings > 0 {
			p.Found = append(p.Found, foundIn{Name: f.Path, Href: tab.Href})
		}
	}
	for _, other := range rep.Others {
		if len(other.Errors)+len(other.Warnings) > 0 {
			p.Found = append(p.Found, foundIn{Name: other.Name, Href: "#" + settingsOthers})
		}
	}
	return p, nil
}

// recommendationsPage is what the kit would change of its settings from what it has on record over
// a stretch: the evaluation `foreman-kit config recommend` prints. The portal shows it and decides
// nothing: a person applies what they agree with, one setting at a time.
type recommendationsPage struct {
	cycle.Recommendations

	// Stretches are the stretches the page offers, the one it shows marked: the ones the kit has
	// records from before the start of.
	Stretches []stateTab `json:"-"`
	// Short says the records do not go back as far as the stretch shown.
	Short bool `json:"-"`
}

func (p *recommendationsPage) head(v *pageView) {
	v.Title, v.Section = "Recommendations", "/system/settings"
	v.Crumbs = []crumb{{Label: "System", Href: "/system/health"}, {Label: "Settings", Href: "/system/settings"}, {Label: "Recommendations"}}
}

// stretchWords writes the name the kit gives a stretch ("24h", "7d") in words; a name of another
// shape stays as it is.
func stretchWords(name string) string {
	if len(name) < 2 {
		return name
	}
	n, err := strconv.Atoi(name[:len(name)-1])
	if err != nil || n < 1 {
		return name
	}
	switch name[len(name)-1] {
	case 'h':
		return countText(n, "hour", "hours")
	case 'd':
		return countText(n, "day", "days")
	}
	return name
}

func (s *Server) askRecommendations(ctx context.Context, r *http.Request) (*recommendationsPage, error) {
	rec, err := s.work.Recommendations(ctx, r.URL.Query().Get("window"))
	if err != nil {
		return nil, err
	}
	p := &recommendationsPage{Recommendations: rec}
	for _, w := range rec.Windows {
		shown := w.Name == rec.Window
		if shown && !w.Available {
			p.Short = true
		}
		if !w.Available && !shown {
			continue
		}
		p.Stretches = append(p.Stretches, stateTab{State: w.Name, Label: stretchWords(w.Name), Href: hrefWith(r, "window", w.Name), Active: shown})
	}
	return p, nil
}

// promptsPage is the prompts of the application: one for each role, with its version, the
// placeholders it writes and what the check of the kit says of them all.
type promptsPage struct {
	PromptsReport
}

func (p *promptsPage) head(v *pageView) {
	v.Title, v.Section = "Prompt templates", "/system/prompts"
	v.Crumbs = []crumb{{Label: "System", Href: "/system/health"}, {Label: "Prompt templates"}}
}

func (s *Server) askPrompts(_ context.Context, _ *http.Request) (*promptsPage, error) {
	return &promptsPage{PromptsReport: s.work.Prompts()}, nil
}

// promptViews are the two ways the page of a prompt shows it: the file as it is, and what an agent
// would be given.
const (
	promptAsWritten = "template"
	promptAsSample  = "sample"
)

// promptPage is the prompt of one role: its file, and a sample of what an agent is given.
type promptPage struct {
	PromptTemplate

	// View is the way the page shows it, and Tabs lead to the two of them.
	View string    `json:"-"`
	Tabs []tabItem `json:"-"`
}

func (p *promptPage) head(v *pageView) {
	v.Title, v.Section = strings.ToUpper(p.Role)+" prompt", "/system/prompts"
	v.Crumbs = []crumb{{Label: "System", Href: "/system/health"}, {Label: "Prompt templates", Href: "/system/prompts"}, {Label: strings.ToUpper(p.Role)}}
}

func (s *Server) askPrompt(_ context.Context, r *http.Request) (*promptPage, error) {
	view := r.URL.Query().Get("view")
	switch view {
	case "":
		view = promptAsWritten
	case promptAsWritten, promptAsSample:
	default:
		return nil, errInvalid("view is %q, the file as it is, or %q, what an agent would be given: %q is neither", promptAsWritten, promptAsSample, view)
	}
	t, err := s.work.Prompt(r.PathValue("role"))
	if err != nil {
		return nil, err
	}
	return &promptPage{PromptTemplate: t, View: view, Tabs: []tabItem{
		{Key: promptAsWritten, Label: "Template", Icon: "file-text", Href: hrefWith(r, "view", "")},
		{Key: promptAsSample, Label: "Sample render", Icon: "sparkles", Href: hrefWith(r, "view", promptAsSample)},
	}}, nil
}

// processesPerList is how many dispatches and how many pauses the page of processes shows at once.
const processesPerList = 10

// processesPage is what runs for the kit as a whole and what ran: the locks its processes hold, the
// passes of its dispatcher with what their workers did, and the pauses of the workspace.
type processesPage struct {
	// Locks are the locks and the leases on record, the one taken first first. A stale one is still
	// listed: nothing holds it, and the next process that wants it takes it.
	Locks []store.HeldLock `json:"locks"`
	// DaemonGiven lists what the daemon that runs now was started with `foreman-kit --set`: see
	// dashboardPage.DaemonGiven. The page says it next to the lock of that daemon.
	DaemonGiven []string `json:"daemon_given"`
	// Dispatches and Pauses are one page of each, the latest first.
	Dispatches store.DispatchPage `json:"dispatches"`
	Pauses     store.PausePage    `json:"pauses"`

	// Held counts the locks something still holds: the page keeps itself up to date while there are.
	Held             int    `json:"-"`
	OlderDispatches  string `json:"-"`
	LatestDispatches string `json:"-"`
	OlderPauses      string `json:"-"`
	LatestPauses     string `json:"-"`
}

func (p *processesPage) head(v *pageView) {
	v.Title, v.Section = "Processes", "/system/processes"
	v.Crumbs = []crumb{{Label: "System", Href: "/system/health"}, {Label: "Processes"}}
	if p.Held > 0 {
		v.Live = 15
	}
}

func (s *Server) readProcesses(ctx context.Context, db *store.View, r *http.Request) (*processesPage, error) {
	q := r.URL.Query()
	limit, err := limitOf(q)
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = processesPerList
	}
	dispatchesBefore, pausesBefore := strings.TrimSpace(q.Get("dispatches_before")), strings.TrimSpace(q.Get("pauses_before"))

	p := &processesPage{}
	if p.Locks, err = db.ListLocks(ctx); err != nil {
		return nil, err
	}
	for _, l := range p.Locks {
		if !l.Stale {
			p.Held++
		}
	}
	if p.DaemonGiven, err = s.daemonGiven(ctx); err != nil {
		return nil, err
	}
	if p.Dispatches, err = db.ListDispatcherRuns(ctx, dispatchesBefore, limit); err != nil {
		return nil, err
	}
	if p.Pauses, err = db.ListPauses(ctx, pausesBefore, limit); err != nil {
		return nil, err
	}
	if p.Dispatches.Items == nil {
		p.Dispatches.Items = []store.DispatchRecord{}
	}
	if p.Pauses.Items == nil {
		p.Pauses.Items = []store.Pause{}
	}
	if p.Dispatches.Next != "" {
		p.OlderDispatches = hrefWith(r, "dispatches_before", p.Dispatches.Next)
	}
	if dispatchesBefore != "" {
		p.LatestDispatches = hrefWith(r, "dispatches_before", "")
	}
	if p.Pauses.Next != "" {
		p.OlderPauses = hrefWith(r, "pauses_before", p.Pauses.Next)
	}
	if pausesBefore != "" {
		p.LatestPauses = hrefWith(r, "pauses_before", "")
	}
	return p, nil
}
