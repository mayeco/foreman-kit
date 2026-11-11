package portal

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mayeco/foreman-kit/internal/store"
)

// The pages of one change. /changes/{name} tells its story from the rows of what happened to it;
// its files, its events and what a person can do in its worktree are pages next to it. A round of
// gates has a page of its own, /gates/{id}, that the story and the page of an execution link to.

// ---- the head every page of a change has -----------------------------------------------------------

// changeHead is what every page of a change shows above its tabs: what is on record about the
// change now.
type changeHead struct {
	store.ChangeRecord
	// NeedsHuman is the question an agent left for a person in the worktree of the change. It is the
	// one fact about a change that comes from a file: the orchestrator does not record it as a block.
	NeedsHuman string `json:"needs_human,omitempty"`
	// HasWorktree says the change has a worktree now, where an agent or the gates can be run.
	HasWorktree bool `json:"has_worktree"`

	Tab  string    `json:"-"`
	Tabs []tabItem `json:"-"`
}

// Stopped reports whether the change waits for a person: it is blocked, or an agent asked a question.
func (h *changeHead) Stopped() bool { return h.Block != nil || h.NeedsHuman != "" }

// Stage is how far the change got in the pipeline: 1 queued, 2 building, 3 ready, 4 merging,
// 5 integrated, and 0 when it is in none of them.
func (h *changeHead) Stage() int {
	switch h.State {
	case store.ChangeQueued:
		return 1
	case store.ChangeBuilding, store.ChangeRebuildRequested:
		return 2
	case store.ChangeReady:
		return 3
	case store.ChangeMerging:
		return 4
	case store.ChangeIntegrated:
		return 5
	case store.ChangeBlocked:
		// A blocked change stays where it was stopped: with the merger when it had been handed over.
		if h.Manifest != nil {
			return 3
		}
		return 2
	}
	return 0
}

// The tabs of a change, in order.
const (
	tabStory     = "story"
	tabFiles     = "files"
	tabEvents    = "events"
	tabIntervene = "intervene"
)

var changeTabLabels = map[string]string{tabStory: "Story", tabFiles: "Files", tabEvents: "Events", tabIntervene: "Intervene"}

func (h *changeHead) head(v *pageView) {
	v.Section = "/changes"
	v.Title = h.ChangeName
	v.Crumbs = []crumb{{Label: "Changes", Href: "/changes"}, {Label: h.ChangeName}}
	if h.Tab != tabStory {
		v.Title = h.ChangeName + " · " + changeTabLabels[h.Tab]
		v.Crumbs = []crumb{{Label: "Changes", Href: "/changes"}, {Label: h.ChangeName, Href: changeHref(h.ChangeName)}, {Label: changeTabLabels[h.Tab]}}
	}
	// What is in progress moves: the page keeps itself up to date while the change is with a worker
	// or with the merger.
	if h.Run != nil || h.Merge != nil {
		v.Live = 10
	}
}

// changeHref is the address of the page of a change.
func changeHref(name string) string { return "/changes/" + url.PathEscape(name) }

// readChangeHead reads what every page of a change shows, for the tab the page is.
func (s *Server) readChangeHead(ctx context.Context, db *store.View, name, tab string) (*changeHead, error) {
	rec, err := db.Change(ctx, name)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errNotFound("the kit has no record of a change named %s", name)
	}
	h := &changeHead{ChangeRecord: *rec, NeedsHuman: s.changes.NeedsHuman(name), HasWorktree: s.changes.HasWorktree(name), Tab: tab}
	base := changeHref(name)
	h.Tabs = []tabItem{
		{Key: tabStory, Label: changeTabLabels[tabStory], Icon: "history", Href: base},
		{Key: tabFiles, Label: changeTabLabels[tabFiles], Icon: "file-code", Href: base + "/files"},
		{Key: tabEvents, Label: changeTabLabels[tabEvents], Icon: "radio-tower", Href: base + "/events"},
	}
	// A person works in the worktree of a change: with none, there is nowhere to intervene.
	if h.HasWorktree || tab == tabIntervene {
		h.Tabs = append(h.Tabs, tabItem{Key: tabIntervene, Label: changeTabLabels[tabIntervene], Icon: "wrench", Href: base + "/intervene"})
	}
	return h, nil
}

// ---- the story ---------------------------------------------------------------------------------------

// changeStoryPage is a change and a page of its story: /changes/{name} and /api/changes/{name}.
type changeStoryPage struct {
	Change *changeHead `json:"change"`
	// Items are the episodes of the story, the latest first; Next, the cursor of the ones before.
	Items []store.Episode `json:"items"`
	Next  string          `json:"next,omitempty"`

	// Episodes are the same episodes as the page draws them.
	Episodes []episodeView `json:"-"`
	Older    string        `json:"-"`
	Newest   string        `json:"-"`
}

// episodeView is an episode as the story draws it. A worker run is drawn as its executions in the
// order they ran, each with what followed from it; the lists of the run say the same without order.
type episodeView struct {
	store.Episode
	// Steps are the executions of a worker run.
	Steps []runStep
	// Rounds and Evaluations are the rounds of gates and the questions to the evaluator of a worker
	// run that follow from none of its executions.
	Rounds      []store.GateRoundDetail
	Evaluations []store.Evaluation
}

// runStep is one execution of a worker run with what followed from it: the rounds of gates that
// verified its work, the repair it carried out, and the questions put to the evaluator meanwhile.
type runStep struct {
	store.ExecutionDetail
	Name        string
	Rounds      []store.GateRoundDetail
	Repair      *store.RepairRoundRecord
	Evaluations []store.Evaluation
}

// EnginesTried lists the engines that worked on the execution, in the order they were tried.
func (st runStep) EnginesTried() []string {
	return executionRow{ExecutionDetail: st.ExecutionDetail}.EnginesTried()
}

// episodeViewOf arranges an episode for the page.
func episodeViewOf(ep store.Episode) episodeView {
	view := episodeView{Episode: ep}
	run := ep.Run
	if run == nil {
		return view
	}
	at := map[string]int{}
	for i, ex := range run.Executions {
		at[ex.ExecID] = i
		view.Steps = append(view.Steps, runStep{ExecutionDetail: ex, Name: executionName(ex.Execution)})
	}
	for _, round := range run.GateRounds {
		if i, ok := at[round.AfterExecID]; ok {
			view.Steps[i].Rounds = append(view.Steps[i].Rounds, round)
		} else {
			view.Rounds = append(view.Rounds, round)
		}
	}
	for i := range run.Repairs {
		if at, ok := at[run.Repairs[i].ExecID]; ok {
			view.Steps[at].Repair = &run.Repairs[i]
		}
	}
	for _, ev := range run.Evaluations {
		if i, ok := at[ev.ExecID]; ok && ev.ExecID != "" {
			view.Steps[i].Evaluations = append(view.Steps[i].Evaluations, ev)
		} else {
			view.Evaluations = append(view.Evaluations, ev)
		}
	}
	return view
}

func (p *changeStoryPage) head(v *pageView) { p.Change.head(v) }

func (s *Server) readChangeStory(ctx context.Context, db *store.View, r *http.Request) (*changeStoryPage, error) {
	name := r.PathValue("name")
	head, err := s.readChangeHead(ctx, db, name, tabStory)
	if err != nil {
		return nil, err
	}
	limit, err := limitOf(r.URL.Query())
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = 20
	}
	before := strings.TrimSpace(r.URL.Query().Get("before"))
	story, err := db.ChangeStory(ctx, name, before, limit)
	if err != nil {
		return nil, err
	}
	p := &changeStoryPage{Change: head, Items: story.Episodes, Next: story.Next}
	if p.Items == nil {
		p.Items = []store.Episode{}
	}
	for _, ep := range p.Items {
		p.Episodes = append(p.Episodes, episodeViewOf(ep))
	}
	if story.Next != "" {
		p.Older = hrefWith(r, "before", story.Next)
	}
	if before != "" {
		p.Newest = hrefWith(r, "before", "")
	}
	return p, nil
}

// ---- the files ---------------------------------------------------------------------------------------

// changeFilesPage is the OpenSpec files of a change, with one of them open.
type changeFilesPage struct {
	Change *changeHead `json:"change"`
	Artifacts
	// File is the file the page shows, nil when the change has none.
	File *ArtifactContent `json:"file,omitempty"`
	// Whole is the address of the page with the whole file, "" when the page already shows it.
	Whole string `json:"-"`
}

func (p *changeFilesPage) head(v *pageView) {
	p.Change.head(v)
	if p.File != nil {
		v.Title = p.Change.ChangeName + " · " + p.File.Name
		v.Crumbs[len(v.Crumbs)-1].Href = changeHref(p.Change.ChangeName) + "/files"
		v.Crumbs = append(v.Crumbs, crumb{Label: p.File.Name})
	}
	v.Live = 0
}

// artifactShown is how much of the end of a file the page of a change shows at first.
const artifactShown = 300000

// readChangeFile returns one OpenSpec file of a change: /api/changes/{name}/files/{file...}.
func (s *Server) readChangeFile(ctx context.Context, db *store.View, r *http.Request) (*ArtifactContent, error) {
	name := r.PathValue("name")
	if known, err := db.Change(ctx, name); err != nil {
		return nil, err
	} else if known == nil {
		return nil, errNotFound("the kit has no record of a change named %s", name)
	}
	size := artifactShown
	if r.URL.Query().Get("whole") != "" {
		size = 0
	}
	content, err := s.changes.ReadArtifact(name, r.PathValue("file"), size)
	if err != nil {
		return nil, err
	}
	return &content, nil
}

func (s *Server) readChangeFiles(ctx context.Context, db *store.View, r *http.Request) (*changeFilesPage, error) {
	name := r.PathValue("name")
	head, err := s.readChangeHead(ctx, db, name, tabFiles)
	if err != nil {
		return nil, err
	}
	p := &changeFilesPage{Change: head, Artifacts: s.changes.Artifacts(name)}
	file := r.PathValue("file")
	if file == "" && len(p.Files) > 0 {
		file = p.Files[0].Name
	}
	if file == "" {
		return p, nil
	}
	size := artifactShown
	if r.URL.Query().Get("whole") != "" {
		size = 0
	}
	content, err := s.changes.ReadArtifact(name, file, size)
	if err != nil {
		return nil, err
	}
	p.File = &content
	if content.Truncated {
		p.Whole = hrefWith(r, "whole", "1")
	}
	return p, nil
}

// ---- the events --------------------------------------------------------------------------------------

// changeEventsPage is a page of the events of a change: /changes/{name}/events and its mirror.
type changeEventsPage struct {
	Items []store.EventRecord `json:"items"`
	Next  string              `json:"next,omitempty"`

	Change *changeHead `json:"-"`
	// Event is the type the page was asked for; Types, the types the change has events of.
	Event  string                 `json:"-"`
	Types  []store.EventTypeCount `json:"-"`
	Total  int                    `json:"-"`
	Older  string                 `json:"-"`
	Newest string                 `json:"-"`
}

func (p *changeEventsPage) head(v *pageView) { p.Change.head(v) }

func (s *Server) readChangeEvents(ctx context.Context, db *store.View, r *http.Request) (*changeEventsPage, error) {
	name := r.PathValue("name")
	head, err := s.readChangeHead(ctx, db, name, tabEvents)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	limit, err := limitOf(q)
	if err != nil {
		return nil, err
	}
	p := &changeEventsPage{Change: head, Event: strings.TrimSpace(q.Get("event"))}
	before := strings.TrimSpace(q.Get("before"))
	page, err := db.ListEventsPage(ctx, store.EventPageFilter{Change: name, Event: p.Event, Before: before, Limit: limit})
	if err != nil {
		return nil, err
	}
	p.Items, p.Next = page.Items, page.Next
	if p.Items == nil {
		p.Items = []store.EventRecord{}
	}
	if p.Types, err = db.CountEventsByType(ctx, store.EventPageFilter{Change: name}); err != nil {
		return nil, err
	}
	for _, t := range p.Types {
		p.Total += t.Count
	}
	if page.Next != "" {
		p.Older = hrefWith(r, "before", page.Next)
	}
	if before != "" {
		p.Newest = hrefWith(r, "before", "")
	}
	return p, nil
}

// ---- intervening -------------------------------------------------------------------------------------

// changeIntervenePage is what a person can do in the worktree of a change: /changes/{name}/intervene.
type changeIntervenePage struct {
	Change  *changeHead
	Engines []EngineChoice
	Default string
	// Prompt is what the form starts with, and From says where it was taken from: "plan" or "build"
	// for the prompt a worker would be given now, or the execution whose prompt it is.
	Prompt string
	From   string
	// Sent are the prompts a person sent to an agent in this worktree, the latest first.
	Sent []store.ExecutionDetail
}

func (p *changeIntervenePage) head(v *pageView) {
	p.Change.head(v)
	v.Live = 0
}

func (s *Server) readChangeIntervene(ctx context.Context, db *store.View, r *http.Request) (*changeIntervenePage, error) {
	name := r.PathValue("name")
	head, err := s.readChangeHead(ctx, db, name, tabIntervene)
	if err != nil {
		return nil, err
	}
	p := &changeIntervenePage{Change: head, Engines: s.exec.Engines(), Default: s.exec.DefaultEngine()}
	switch from := strings.TrimSpace(r.URL.Query().Get("from")); from {
	case "":
	case "plan", "build":
		if p.Prompt, err = s.changes.RenderedPrompt(name, from); err != nil {
			return nil, err
		}
		p.From = from
	default:
		ex, err := db.Execution(ctx, from)
		if err != nil {
			return nil, err
		}
		if ex == nil || ex.ChangeName != name {
			return nil, errNotFound("the change %s has no execution %q to take a prompt from", name, from)
		}
		p.Prompt, p.From = ex.PromptText, from
	}
	sent, err := db.ListExecutionsPage(ctx, store.ExecutionFilter{Kind: "portal_run", Change: name, Limit: 10})
	if err != nil {
		return nil, err
	}
	p.Sent = sent.Items
	return p, nil
}

// ---- a round of gates --------------------------------------------------------------------------------

// gateRoundPage is one round of gates with the result of each gate: /gates/{id} and its mirror.
type gateRoundPage struct {
	store.GateRoundDetail
	// Failed counts the gates of the round that did not pass.
	Failed int `json:"failed"`
}

func (p *gateRoundPage) head(v *pageView) {
	title := "Gates #" + strconv.FormatInt(p.ID, 10)
	v.Title, v.Section = title, "/changes"
	v.Crumbs = []crumb{{Label: "Changes", Href: "/changes"}}
	if p.ChangeName != "" {
		v.Crumbs = append(v.Crumbs, crumb{Label: p.ChangeName, Href: changeHref(p.ChangeName)})
	}
	v.Crumbs = append(v.Crumbs, crumb{Label: title})
	if p.Result == "" {
		v.Live = 5
	}
}

func (s *Server) readGateRound(ctx context.Context, db *store.View, r *http.Request) (*gateRoundPage, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return nil, errNotFound("there is no round of gates %q", r.PathValue("id"))
	}
	round, err := db.GateRound(ctx, id)
	if err != nil {
		return nil, err
	}
	if round == nil {
		return nil, errNotFound("there is no round of gates %d", id)
	}
	p := &gateRoundPage{GateRoundDetail: *round}
	for _, g := range round.Runs {
		if g.ExitCode != 0 {
			p.Failed++
		}
	}
	return p, nil
}
