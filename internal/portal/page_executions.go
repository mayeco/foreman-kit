package portal

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// Executions: everything that ran, whatever started it. A prompt of a worker (PLAN, BUILD, a
// repair), the judge of a cycle, an agent a person ran from the portal and an operation of the kit
// are rows of one table, so they have one list and one page.

// ---- what an execution is called ------------------------------------------------------------------

// executionName says what an execution is, in the words of the operator.
func executionName(ex store.Execution) string {
	switch ex.Kind {
	case "worker":
		switch phase := ex.Phase; {
		case phase == "plan":
			return "PLAN"
		case phase == "apply":
			return "BUILD"
		case strings.HasPrefix(phase, "plan-repair-"):
			return "PLAN repair " + strings.TrimPrefix(phase, "plan-repair-")
		case strings.HasPrefix(phase, "repair-"):
			return "BUILD repair " + strings.TrimPrefix(phase, "repair-")
		case phase != "":
			return phase
		}
		return "Worker prompt"
	case "cycle":
		return "Cycle"
	case "portal_run":
		switch ex.ParentRelation {
		case "continue":
			return "Follow-up of an agent run"
		case "rerun":
			return "Agent run, again"
		}
		return "Agent run"
	case "portal_job":
		switch kind := ex.JobKind; {
		case kind == "dispatch":
			return "Dispatcher"
		case kind == "merge":
			return "Merger"
		case kind == "merge-and-sync":
			// No route starts it any more: the name is for the executions on record.
			return "Merger, then sync"
		case kind == "gates":
			return "Gates on the base"
		case strings.HasPrefix(kind, "gates-"):
			return "Gates in the worktree"
		}
		return "Kit operation"
	}
	return wordingOf(vocExecutionKind, ex.Kind).Text
}

// executionAbout says what an execution worked on: its change, its cycle, or the beginning of the
// prompt a person wrote.
func executionAbout(ex store.Execution) string {
	switch ex.Kind {
	case "cycle":
		return ex.CycleStamp
	case "portal_run":
		if line := firstLine(ex.Summary); line != "" {
			return line
		}
		return firstLine(ex.PromptText)
	}
	return ex.ChangeName
}

// ---- the list --------------------------------------------------------------------------------------

// executionRow is an execution in the list.
type executionRow struct {
	store.ExecutionDetail
	// Name says what ran; About, what it ran on.
	Name  string `json:"name"`
	About string `json:"about,omitempty"`
	// Idle is the agent of the execution while it shows no activity.
	Idle *store.IdleAgent `json:"idle,omitempty"`
}

// EnginesTried lists the engines that worked on the execution, in the order they were tried. Before
// the first try it is the engines the execution was asked to run on.
func (row executionRow) EnginesTried() []string {
	var out []string
	for _, a := range row.Attempts {
		if len(out) == 0 || out[len(out)-1] != a.Engine {
			out = append(out, a.Engine)
		}
	}
	if len(out) == 0 && row.Agent != "" {
		out = append(out, row.Agent)
	}
	return out
}

// executionsPage is the list of what ran: /executions and its mirror, /api/executions.
type executionsPage struct {
	Items []executionRow `json:"items"`
	// Next is the cursor of the page that follows, "" when this one is the last.
	Next string `json:"next,omitempty"`

	// What the page was asked for and what it offers to ask for instead. The API returns the list.
	Filter   store.ExecutionFilter `json:"-"`
	Kinds    []option              `json:"-"`
	Statuses []option              `json:"-"`
	Engines  []option              `json:"-"`
	// Older is the address of the page that follows; Newest, of the first page when this is not it.
	Older  string `json:"-"`
	Newest string `json:"-"`
	// All is the address of the list with no filter by change or cycle.
	All     string `json:"-"`
	running int
}

func (p *executionsPage) head(v *pageView) {
	v.Title, v.Section = "Executions", "/executions"
	v.Crumbs = []crumb{{Label: "Executions"}}
	if p.running > 0 {
		v.Live = 5
	}
}

// option is one entry of a filter: a value, its label, how many items it has and its address.
type option struct {
	Value  string
	Label  string
	Count  int
	Href   string
	Active bool
}

// hrefWith returns the address of r with the given query parameters replaced (pairs of key, value).
// An empty value takes the parameter out.
func hrefWith(r *http.Request, kv ...string) string {
	q := r.URL.Query()
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			q.Del(kv[i])
		} else {
			q.Set(kv[i], kv[i+1])
		}
	}
	if enc := q.Encode(); enc != "" {
		return r.URL.Path + "?" + enc
	}
	return r.URL.Path
}

// limitOf reads the size of a page a request asks for; 0 is the size the store gives by default.
func limitOf(q url.Values) (int, error) {
	raw := strings.TrimSpace(q.Get("limit"))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, errInvalid("limit is a number from 1 to %d", store.MaxPageSize)
	}
	return n, nil
}

// executionFilterOf reads the filter of the list from the query of a request.
func executionFilterOf(q url.Values) (store.ExecutionFilter, error) {
	get := func(key string) string { return strings.TrimSpace(q.Get(key)) }
	f := store.ExecutionFilter{
		Kind: get("kind"), Status: get("status"), Engine: get("engine"),
		Change: get("change"), Cycle: get("cycle"), Before: get("before"),
	}
	if _, ok := vocabularies[vocExecutionKind][f.Kind]; f.Kind != "" && !ok {
		return f, errInvalid("%q is not a kind of execution", f.Kind)
	}
	if _, ok := vocabularies[vocExecutionStatus][f.Status]; f.Status != "" && !ok {
		return f, errInvalid("%q is not a status of an execution", f.Status)
	}
	var err error
	f.Limit, err = limitOf(q)
	return f, err
}

// idleByExecution returns the agents that show no activity now, by the execution each runs in.
func idleByExecution(ctx context.Context, db *store.View) (map[string]*store.IdleAgent, error) {
	agents, err := db.ListIdleAgents(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*store.IdleAgent, len(agents))
	for i := range agents {
		out[agents[i].ExecID] = &agents[i]
	}
	return out, nil
}

// The filters of the list, in the order the page offers them.
var (
	executionKindOrder   = []string{"worker", "cycle", "portal_run", "portal_job"}
	executionStatusOrder = []string{"running", "succeeded", "failed", "stopped", "timed_out"}
	executionKindLabels  = map[string]string{"worker": "Workers", "cycle": "Cycles", "portal_run": "Agent runs", "portal_job": "Kit operations"}
)

func (s *Server) readExecutions(ctx context.Context, db *store.View, r *http.Request) (*executionsPage, error) {
	f, err := executionFilterOf(r.URL.Query())
	if err != nil {
		return nil, err
	}
	list, err := db.ListExecutionsPage(ctx, f)
	if err != nil {
		return nil, err
	}
	idle, err := idleByExecution(ctx, db)
	if err != nil {
		return nil, err
	}
	engines, err := db.ListEngineNames(ctx)
	if err != nil {
		return nil, err
	}

	p := &executionsPage{Next: list.Next, Filter: f, Items: make([]executionRow, 0, len(list.Items))}
	for _, d := range list.Items {
		row := executionRow{ExecutionDetail: d, Name: executionName(d.Execution), About: executionAbout(d.Execution)}
		if d.Status == "running" && !d.OwnerGone {
			row.Idle = idle[d.ExecID]
			p.running++
		}
		p.Items = append(p.Items, row)
	}

	// A filter that changes starts from the first page: the cursor is of the list as it was filtered.
	p.Kinds = []option{{Label: "All", Href: hrefWith(r, "kind", "", "before", ""), Active: f.Kind == ""}}
	for _, kind := range executionKindOrder {
		p.Kinds = append(p.Kinds, option{Value: kind, Label: executionKindLabels[kind], Href: hrefWith(r, "kind", kind, "before", ""), Active: f.Kind == kind})
	}
	for _, status := range executionStatusOrder {
		p.Statuses = append(p.Statuses, option{Value: status, Label: wordingOf(vocExecutionStatus, status).Text, Active: f.Status == status})
	}
	for _, engine := range engines {
		p.Engines = append(p.Engines, option{Value: engine, Label: engine, Active: f.Engine == engine})
	}
	if list.Next != "" {
		p.Older = hrefWith(r, "before", list.Next)
	}
	if f.Before != "" {
		p.Newest = hrefWith(r, "before", "")
	}
	if f.Change != "" || f.Cycle != "" {
		p.All = hrefWith(r, "change", "", "cycle", "", "before", "")
	}
	return p, nil
}

// ---- starting one ----------------------------------------------------------------------------------

// newExecutionPage is the form that runs an agent on a prompt: /executions/new.
type newExecutionPage struct {
	Engines []EngineChoice
	// Default is the engine the form starts on.
	Default string
}

func (p *newExecutionPage) head(v *pageView) {
	v.Title, v.Section = "New agent run", "/executions"
	v.Crumbs = []crumb{{Label: "Executions", Href: "/executions"}, {Label: "New agent run"}}
}

func (s *Server) readNewExecution(context.Context, *store.View, *http.Request) (*newExecutionPage, error) {
	return &newExecutionPage{Engines: s.exec.Engines(), Default: s.exec.DefaultEngine()}, nil
}

// ---- one execution ---------------------------------------------------------------------------------

// executionPage is one execution: /executions/{id} and its mirror, /api/executions/{id}.
type executionPage struct {
	store.ExecutionRecord
	// Name says what ran; About, what it ran on.
	Name  string `json:"name"`
	About string `json:"about,omitempty"`
	// Outcome says in a sentence what the execution is doing, or how it ended and what follows.
	Outcome wording `json:"outcome"`
	// Idle is the agent of the execution while it shows no activity.
	Idle *store.IdleAgent `json:"idle,omitempty"`
	// Events are the events of the execution, the latest first.
	Events []store.EventRecord `json:"events"`
	// What a person can do with the execution from the portal.
	CanStop     bool `json:"can_stop"`
	CanContinue bool `json:"can_continue"`
	CanRerun    bool `json:"can_rerun"`

	// The page alone shows these. The API gives the output at /api/executions/{id}/log; the engines
	// are the ones the form that continues a run offers.
	Log     store.LogTail  `json:"-"`
	Engines []EngineChoice `json:"-"`
	// WholeLog is the address of the page with the whole output, "" when the page already shows it.
	WholeLog string `json:"-"`
}

// Live reports whether the execution is going on: recorded as running, by a process that is alive.
func (p *executionPage) Live() bool { return p.Status == "running" && !p.OwnerGone }

// Title is the heading of the page.
func (p *executionPage) Title() string {
	if p.About != "" && p.Kind != "portal_run" {
		return p.Name + " · " + p.About
	}
	return p.Name
}

// EnginesTried lists the engines that worked on the execution, in the order they were tried.
func (p *executionPage) EnginesTried() []string {
	return executionRow{ExecutionDetail: p.ExecutionDetail}.EnginesTried()
}

func (p *executionPage) head(v *pageView) {
	v.Title, v.Section = p.Title(), "/executions"
	v.Crumbs = []crumb{{Label: "Executions", Href: "/executions"}, {Label: p.Title()}}
	if p.Live() {
		v.Live = 5
	}
}

// executionLogTail is how much of the end of an output the page of an execution shows at first.
const executionLogTail = 256 << 10

func (s *Server) readExecution(ctx context.Context, db *store.View, r *http.Request) (*executionPage, error) {
	id := r.PathValue("id")
	rec, err := db.Execution(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errNotFound("there is no execution %q", id)
	}
	p := &executionPage{
		ExecutionRecord: *rec,
		Name:            executionName(rec.Execution),
		About:           executionAbout(rec.Execution),
		Events:          []store.EventRecord{},
	}
	if p.Live() {
		idle, err := idleByExecution(ctx, db)
		if err != nil {
			return nil, err
		}
		p.Idle = idle[id]
	}
	p.Outcome = executionOutcome(p)

	events, err := db.EventsOfExecution(ctx, id)
	if err != nil {
		return nil, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		p.Events = append(p.Events, events[i])
	}

	portal := rec.Kind == "portal_run" || rec.Kind == "portal_job"
	p.CanStop = portal && rec.Status == "running"
	p.CanContinue = rec.Kind == "portal_run"
	p.CanRerun = rec.Kind == "portal_run" && rec.Status != "running"
	if p.CanContinue {
		p.Engines = s.exec.Engines()
	}

	// The script that keeps the page up to date asks for it again and again. The output is not read
	// for it: the page follows it through its stream.
	if !liveRefresh(r) {
		size := executionLogTail
		if r.URL.Query().Get("output") == "all" {
			size = 0
		}
		if p.Log, err = db.LogTail(ctx, id, size); err != nil {
			return nil, err
		}
		if p.Log.Truncated {
			p.WholeLog = hrefWith(r, "output", "all")
		}
	}
	return p, nil
}

// executionOutcome says in a sentence what an execution is doing or how it ended, and what follows.
func executionOutcome(p *executionPage) wording {
	subject := "The agent"
	work := "its prompt"
	switch p.Kind {
	case "worker":
		work = "its " + p.Name + " prompt"
	case "cycle":
		subject, work = "The judge of the cycle", "its prompt"
	case "portal_run":
		work = "the prompt"
	case "portal_job":
		subject, work = p.Name, ""
	}
	finished := func(verb string) string {
		if work == "" {
			return subject + " " + verb
		}
		return subject + " " + verb + " " + work
	}
	last := lastAttempt(p.Attempts)

	switch p.Status {
	case "running":
		switch {
		case p.OwnerGone:
			text := fmt.Sprintf("Recorded as running, but the process that ran it (pid %d) is gone: it ended without recording how.", p.PID)
			if p.Kind == "worker" {
				text += " The next dispatcher closes it and runs the phase again."
			} else if p.Kind != "cycle" {
				text += " Stop it to close its record."
			}
			return wording{Text: text, Tone: "warning"}
		case p.Idle != nil:
			return wording{Tone: "warning", Text: fmt.Sprintf(
				"Running, but %s has shown no activity for %s: %s. It may be reading or waiting for its model; if it stays like this, the idle watchdog stops it at %s (%s).",
				p.Idle.Engine, durationText(seconds(p.Idle.IdleSeconds)), idleCriteria, durationText(seconds(p.Idle.LimitSeconds)), settings.IdleTimeout.Path())}
		case work == "":
			return wording{Text: subject + " is running now.", Tone: "info"}
		}
		return wording{Text: "Running now: " + strings.ToLower(subject[:1]) + subject[1:] + " is working on " + work + ".", Tone: "info"}

	case "succeeded":
		return wording{Text: finished("finished") + ".", Tone: "success"}

	case "stopped":
		text := fmt.Sprintf("Stopped before it finished (exit %d).", p.ExitCode)
		switch p.ExitCode {
		case 143:
			text = "Stopped before it finished (SIGTERM, exit 143)."
		case 137:
			text = "Killed before it finished (SIGKILL, exit 137): it did not stop in time when asked, or the system ran out of memory."
		case 130:
			text = "Interrupted before it finished (SIGINT, exit 130)."
		}
		switch p.Kind {
		case "worker":
			text += " That happens when the daemon restarts, or when the kit is paused or the worker is stopped by hand. The dispatcher runs this phase again on a later pass."
		case "portal_run", "portal_job":
			text += " A person stopped it from the portal, or the portal shut down while it ran."
		}
		return wording{Text: text, Tone: "warning"}

	case "timed_out":
		return wording{Text: "Stopped by the idle watchdog: the agent showed no CPU, output or file activity for too long.", Tone: "error"}

	case "failed":
		if last != nil && (last.Outcome == "idle_stalled" || last.IdleSeconds > 0) {
			return wording{Tone: "error", Text: fmt.Sprintf(
				"Stopped by the idle watchdog (exit %d) after %s without activity: %s. The agent that stalled is not tried again in the same call; the next agent of the role, or a later dispatch, continues from what it left.",
				p.ExitCode, durationText(seconds(last.IdleSeconds)), idleCriteria)}
		}
		if last != nil && work != "" {
			if w := wordingOf(vocAttemptOutcome, last.Outcome); w.Means != "" && last.Outcome != "cancelled" {
				return wording{Tone: "error", Text: fmt.Sprintf("%s could not finish %s on %s: %s (exit %d). %s", subject, work, last.Engine, w.Text, p.ExitCode, w.Means)}
			}
		}
		if work == "" {
			return wording{Text: fmt.Sprintf("%s failed (exit %d): see its output below.", subject, p.ExitCode), Tone: "error"}
		}
		return wording{Text: fmt.Sprintf("%s exited with an error (exit %d): see its output below.", subject, p.ExitCode), Tone: "error"}
	}
	return wording{Text: "Recorded as " + wordingOf(vocExecutionStatus, p.Status).Text + "."}
}

// idleCriteria is what the idle watchdog looks at, as the pages say it.
const idleCriteria = "no file changed, nothing printed and no command of its own used CPU"

func lastAttempt(attempts []store.AttemptDetail) *store.AttemptDetail {
	if len(attempts) == 0 {
		return nil
	}
	return &attempts[len(attempts)-1]
}
