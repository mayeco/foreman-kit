package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// Board: the changes of the workspace as cards in seven lanes, from what was discarded to what was
// integrated. /board draws it and /api/board returns it.
//
// A lane is not something a card is put in: the card of a change is in the lane its state puts it
// in, the state the database derives from the rows that are open (store.ChangeStates), and a
// proposal nobody queued yet is in Proposals. A person moves a card with one of the actions the
// portal already has (approve a proposal, unblock a change, accept its risk, give back the lease of
// a worker that is gone, abandon it); the card then shows where its state says. Only two lanes take
// a card from a person: Backlog and Discarded. The kit alone puts cards in the others.

// The lanes of the board, in the order a change goes through them.
const (
	laneDiscarded  = "discarded"
	laneProposals  = "proposals"
	laneBacklog    = "backlog"
	laneBuilding   = "building"
	laneBlocked    = "blocked"
	laneMerge      = "merge"
	laneIntegrated = "integrated"
)

// boardLanes are the lanes as the board lays them out. The two at the ends are folded: each shows
// how many cards it has, and its cards once it is opened.
var boardLanes = []boardLane{
	{Key: laneDiscarded, Title: "Discarded", Icon: "trash-2", Accepts: true, Folded: true,
		Empty: "Nothing was discarded, and every change on record is in the backlog."},
	{Key: laneProposals, Title: "Proposals", Icon: "lightbulb",
		Empty: "No proposal waits to be decided or queued."},
	{Key: laneBacklog, Title: "Backlog", Icon: "list-ordered", Accepts: true,
		Empty: "No change waits to be built."},
	{Key: laneBuilding, Title: "Building", Icon: "hammer",
		Empty: "No worker is building."},
	{Key: laneBlocked, Title: "Blocked", Icon: "octagon-alert",
		Empty: "No change is blocked."},
	{Key: laneMerge, Title: "Merge queue", Icon: "git-merge",
		Empty: "No change waits for the merger."},
	{Key: laneIntegrated, Title: "Integrated", Icon: "archive", Folded: true,
		Empty: "No change was integrated in these days."},
}

// laneOfState is the lane of a change in each state. A change the merger asked to build again waits
// for the dispatcher like one that was never built, so it is in Backlog; a change with records that
// the backlog does not list is with the discarded ones, which nothing runs for either.
var laneOfState = map[string]string{
	store.ChangeQueued:           laneBacklog,
	store.ChangeRebuildRequested: laneBacklog,
	store.ChangeBuilding:         laneBuilding,
	store.ChangeBlocked:          laneBlocked,
	store.ChangeReady:            laneMerge,
	store.ChangeMerging:          laneMerge,
	store.ChangeIntegrated:       laneIntegrated,
	store.ChangeDiscarded:        laneDiscarded,
	store.ChangeUnknown:          laneDiscarded,
}

const (
	// boardStalledAfter is how long nothing has to happen to a card that waits (in Backlog or in
	// Blocked) for the board to mark it as stalled.
	boardStalledAfter = 24 * time.Hour
	// boardIntegratedDays is how many days back the lane of what was integrated goes.
	boardIntegratedDays = 7
	// boardFoldedCards is how many cards of each kind a folded lane holds once it is opened: the
	// rest are in the list of changes.
	boardFoldedCards = 30
)

// Kinds of card.
const (
	cardChange   = "change"
	cardProposal = "proposal"
)

// The actions that move a card.
const (
	moveApprove     = "approve"      // approve a proposal
	moveReject      = "reject"       // reject a proposal
	moveUnblock     = "unblock"      // lift the block of a change, its counters kept
	moveApproveRisk = "approve_risk" // accept the risk a change is blocked for
	moveReclaim     = "reclaim"      // give back the lease of a worker that is gone
	moveAbandon     = "abandon"      // take a change out of the work of the kit
)

// boardMove is a lane a person can move a card to, with the action of the portal that does it.
type boardMove struct {
	// To is the lane the card is in once the action is done. The kit decides when: an approved
	// proposal stays in Proposals until a cycle queues it.
	To string `json:"to"`
	// Action is one of the move* constants; Method, Href and Body are its request.
	Action string `json:"action"`
	Method string `json:"method"`
	Href   string `json:"href"`
	Body   string `json:"body,omitempty"`
}

// boardCard is a change or a proposal on the board.
type boardCard struct {
	// Kind is "change" or "proposal"; Name, the name of the change or the id of the proposal.
	Kind string `json:"kind"`
	Name string `json:"name"`
	// State is the state of the change (store.ChangeStates) or the status of the proposal.
	State       string `json:"state"`
	Priority    string `json:"priority,omitempty"`
	Description string `json:"description,omitempty"`
	// Urgent says the card waits for a person: its change is blocked, its worker is gone, or an
	// agent left a question for a person in its worktree.
	Urgent bool `json:"urgent,omitempty"`
	// StalledSeconds is for how long nothing has happened to a card that waits in Backlog or in
	// Blocked, once that is longer than the board calls stalled; 0 otherwise.
	StalledSeconds int `json:"stalled_seconds,omitempty"`
	// Question is what an agent asked a person in the worktree of the change.
	Question string `json:"question,omitempty"`

	// What is on record about the card. Change is set for a change, and the others by its lane:
	// Facts in Backlog (what holds it back), Run and Idle in Building, Block in Blocked, and Merge
	// (the attempt in progress) or Manifest (the one that waits) in Merge queue.
	Change   *store.ChangeOverview `json:"change,omitempty"`
	Proposal *store.Proposal       `json:"proposal,omitempty"`
	Facts    []store.WaitFact      `json:"facts,omitempty"`
	Run      *store.RunningRun     `json:"run,omitempty"`
	Idle     *store.IdleAgent      `json:"idle,omitempty"`
	Block    *store.OpenBlock      `json:"block,omitempty"`
	Merge    *store.MergeAttempt   `json:"merge,omitempty"`
	Manifest *store.QueuedManifest `json:"manifest,omitempty"`
	// Tasks are the tasks of the change, as its tasks.md lists them: nil before it is planned. They
	// are read for the cards the board shows, which leaves out the cards of a folded lane.
	Tasks *Tasks `json:"tasks,omitempty"`

	// Moves are the lanes a person can move the card to. A card with none is moved by the kit alone.
	Moves []boardMove `json:"moves"`
}

// Href is the page the card opens: the page of its change, or the list of the proposals.
func (c boardCard) Href() string {
	if c.Kind == cardProposal {
		return "/backlog/proposals?status=" + url.QueryEscape(c.State)
	}
	return "/changes/" + url.PathEscape(c.Name)
}

// RunName says what the run of the card is on now ("BUILD", "PLAN repair 1"), "" between two
// executions, while the worker runs the gates or waits for the evaluator.
func (c boardCard) RunName() string {
	if c.Run == nil || c.Run.Execution == nil {
		return ""
	}
	return executionName(c.Run.Execution.Execution)
}

// RunEngines lists the engines that worked on what the run of the card is on.
func (c boardCard) RunEngines() []string {
	if c.Run == nil || c.Run.Execution == nil {
		return nil
	}
	return executionRow{ExecutionDetail: *c.Run.Execution}.EnginesTried()
}

// proposalReview is what was recorded about the review of a proposal: who reviewed it ("evaluator",
// or the setting that lets proposals through without one), and what the evaluator answered.
type proposalReview struct {
	By     string `json:"by"`
	Status string `json:"status"`
}

// Review returns the review of the proposal of the card, nil when none is on record.
func (c boardCard) Review() *proposalReview {
	if c.Proposal == nil || c.Proposal.ReviewJSON == "" {
		return nil
	}
	var r proposalReview
	if err := json.Unmarshal([]byte(c.Proposal.ReviewJSON), &r); err != nil || r.By == "" {
		return nil
	}
	return &r
}

// boardLane is one lane of the board with its cards.
type boardLane struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	// Accepts says a person can move a card here.
	Accepts bool `json:"accepts"`
	// Folded says the lane shows how many cards it has, and its cards once it is opened.
	Folded bool `json:"folded"`
	// Count is how many cards the lane has in all; Cards holds the ones it shows, and More says
	// where the rest are listed: a list for each kind of card the lane leaves out.
	Count int         `json:"count"`
	Cards []boardCard `json:"cards"`
	More  []boardRest `json:"more,omitempty"`

	Icon  string `json:"-"`
	Empty string `json:"-"`
	// Open says a folded lane shows its cards: the address asks for it (open=<lane>). Toggle is
	// the address of the board with this lane opened, or folded again.
	Open   bool   `json:"-"`
	Toggle string `json:"-"`
}

// boardRest is a list of the portal that holds the cards a folded lane leaves out.
type boardRest struct {
	Label string `json:"label"`
	Href  string `json:"href"`
}

// Strip says the lane is drawn as a narrow strip, with its title and how many cards it has: a
// folded lane until it is opened, and any other lane while it has no card.
func (l boardLane) Strip() bool {
	if l.Folded {
		return !l.Open
	}
	return len(l.Cards) == 0
}

// Urgent counts the cards of the lane that wait for a person.
func (l boardLane) Urgent() int {
	n := 0
	for _, c := range l.Cards {
		if c.Urgent {
			n++
		}
	}
	return n
}

// boardPage is the board of changes.
type boardPage struct {
	Lanes []boardLane `json:"lanes"`
	// StalledAfterSeconds is how long nothing has to happen to a card that waits for it to be
	// marked as stalled; IntegratedDays, how many days back the lane of what was integrated goes.
	StalledAfterSeconds int `json:"stalled_after_seconds"`
	IntegratedDays      int `json:"integrated_days"`
}

// StalledAfterHours is StalledAfterSeconds in hours, the way the page says it.
func (p *boardPage) StalledAfterHours() int { return p.StalledAfterSeconds / 3600 }

func (p *boardPage) head(v *pageView) {
	v.Title, v.Section = "Board", "/board"
	v.Crumbs = []crumb{{Label: "Board"}}
	v.Live = 10
}

// changeMoves returns where a person can move the card of a change, by its state and by what is
// open for it. A change a worker builds or the merger integrates has no move: the portal does not
// stop the process that has it.
func changeMoves(c boardCard) []boardMove {
	name := url.PathEscape(c.Name)
	post := func(to, action, path, body string) boardMove {
		return boardMove{To: to, Action: action, Method: http.MethodPost, Href: "/api/changes/" + name + path, Body: body}
	}
	abandon := post(laneDiscarded, moveAbandon, "/abandon", "")
	switch c.State {
	case store.ChangeQueued, store.ChangeRebuildRequested, store.ChangeReady:
		return []boardMove{abandon}
	case store.ChangeBlocked:
		back := post(laneBacklog, moveUnblock, "/unblock", `{"reset_retries":false}`)
		if c.Block != nil && store.RiskBlock(c.Block.Reason) {
			// A block for the risk of the change is lifted by accepting that risk: unblocked without
			// it, the gate that flagged the change blocks it again.
			back = post(laneBacklog, moveApproveRisk, "/risk-approval", "")
		}
		return []boardMove{back, abandon}
	case store.ChangeBuilding:
		if c.Run != nil && c.Run.Lost {
			return []boardMove{post(laneBacklog, moveReclaim, "/reclaim", "")}
		}
	}
	return []boardMove{}
}

// proposalMoves returns where a person can move the card of a proposal: one that waits for a
// decision is approved or rejected, and an approved one can still be rejected.
func proposalMoves(p store.Proposal) []boardMove {
	id := url.PathEscape(p.ID)
	reject := boardMove{To: laneDiscarded, Action: moveReject, Method: http.MethodPost, Href: "/api/proposals/" + id + "/reject"}
	switch p.Status {
	case "proposed":
		return []boardMove{{To: laneBacklog, Action: moveApprove, Method: http.MethodPost, Href: "/api/proposals/" + id + "/approve"}, reject}
	case "approved":
		return []boardMove{reject}
	}
	return []boardMove{}
}

// proposalCard is the card of a proposal.
func proposalCard(p store.Proposal) boardCard {
	return boardCard{Kind: cardProposal, Name: p.ID, State: p.Status, Priority: p.Priority, Description: p.Description,
		Proposal: &p, Moves: proposalMoves(p)}
}

// isChangeProposal reports whether a proposal is for a change. A proposal to edit a prompt is not
// a change: it is decided in Needs you and never enters the backlog.
func isChangeProposal(p store.Proposal) bool { return p.Kind != "prompt" }

// eachProposal calls fn with every proposal in a status, the latest first. The store lists them a
// page at a time.
func eachProposal(ctx context.Context, db *store.View, status string, fn func(store.Proposal)) error {
	for before := ""; ; {
		page, err := db.ListProposals(ctx, status, before, store.MaxPageSize)
		if err != nil {
			return err
		}
		for _, p := range page.Items {
			fn(p)
		}
		if before = page.Next; before == "" {
			return nil
		}
	}
}

// openLanes reads the folded lanes the address asks to show open: open=discarded,integrated.
func openLanes(q url.Values) (map[string]bool, error) {
	open := map[string]bool{}
	for _, value := range q["open"] {
		for _, key := range strings.Split(value, ",") {
			if key = strings.TrimSpace(key); key == "" {
				continue
			}
			folds := false
			for _, lane := range boardLanes {
				folds = folds || (lane.Folded && lane.Key == key)
			}
			if !folds {
				return nil, errInvalid("open names the folded lanes to show (%s, %s), and %q is not one", laneDiscarded, laneIntegrated, key)
			}
			open[key] = true
		}
	}
	return open, nil
}

// Empty says the board has no card at all, in any lane.
func (p *boardPage) Empty() bool {
	for _, lane := range p.Lanes {
		if lane.Count > 0 {
			return false
		}
	}
	return true
}

func (s *Server) readBoard(ctx context.Context, db *store.View, r *http.Request) (*boardPage, error) {
	open, err := openLanes(r.URL.Query())
	if err != nil {
		return nil, err
	}
	at := db.At()
	now, err := db.Dashboard(ctx, 1)
	if err != nil {
		return nil, err
	}
	queue, err := db.ListWaitFacts(ctx)
	if err != nil {
		return nil, err
	}
	merge, err := db.MergeQueue(ctx, "", 1)
	if err != nil {
		return nil, err
	}
	inbox, err := db.ListInbox(ctx)
	if err != nil {
		return nil, err
	}
	questions := s.changes.Questions()

	// What is open for each change, by its name.
	runs := map[string]*store.RunningRun{}
	for i := range now.Runs {
		runs[now.Runs[i].ChangeName] = &now.Runs[i]
	}
	idle := map[string]*store.IdleAgent{}
	for i := range now.Idle {
		idle[now.Idle[i].ExecID] = &now.Idle[i]
	}
	blocks := map[string]*store.OpenBlock{}
	for i := range inbox.Blocked {
		blocks[inbox.Blocked[i].ChangeName] = &inbox.Blocked[i]
	}
	manifests := map[string]*store.QueuedManifest{}
	for i := range merge.Waiting {
		manifests[merge.Waiting[i].ChangeName] = &merge.Waiting[i]
	}

	cards := map[string][]boardCard{}
	inState := map[string]int{}
	seen := map[string]bool{}
	place := func(c store.ChangeOverview, facts []store.WaitFact) {
		lane := laneOfState[c.State]
		if lane == "" || seen[c.ChangeName] {
			return
		}
		seen[c.ChangeName] = true
		inState[c.State]++
		card := boardCard{Kind: cardChange, Name: c.ChangeName, State: c.State, Priority: c.Priority, Description: c.Description, Change: &c}
		if lane != laneDiscarded && lane != laneIntegrated {
			// A question left in the worktree of a change nothing runs for any more asks nobody.
			card.Question = questions[c.ChangeName]
		}
		switch lane {
		case laneBacklog:
			card.Facts = facts
		case laneBuilding:
			if card.Run = runs[c.ChangeName]; card.Run != nil && card.Run.Execution != nil {
				card.Idle = idle[card.Run.Execution.ExecID]
			}
		case laneBlocked:
			card.Block = blocks[c.ChangeName]
		case laneMerge:
			if merge.Current != nil && merge.Current.ChangeName == c.ChangeName {
				card.Merge = merge.Current
			}
			card.Manifest = manifests[c.ChangeName]
		}
		card.Urgent = c.State == store.ChangeBlocked || card.Question != "" || (card.Run != nil && card.Run.Lost)
		if lane == laneBacklog || lane == laneBlocked {
			if last, ok := parseWhen(c.LastActivityAt); ok && at.Sub(last) >= boardStalledAfter {
				card.StalledSeconds = int(at.Sub(last) / time.Second)
			}
		}
		card.Moves = changeMoves(card)
		cards[lane] = append(cards[lane], card)
	}

	// The changes of the backlog come in the order the dispatcher weighs them: by priority, and then
	// by how long each has been queued.
	for _, q := range queue {
		place(q.ChangeOverview, q.Facts)
	}
	// A change the backlog does not list can still be open: a worker builds it, it is blocked, it
	// waits for the merger. The count of each state says whether one is missing, and then it is read.
	for _, state := range []string{store.ChangeBuilding, store.ChangeBlocked, store.ChangeReady, store.ChangeMerging, store.ChangeRebuildRequested} {
		if now.Counts[state] <= inState[state] {
			continue
		}
		page, err := db.ListChangeOverviews(ctx, store.ChangeFilter{State: state, Per: store.MaxPageSize})
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			place(c, nil)
		}
	}

	// Inside a lane the kit works on, the card that has been there for longest comes first; in the
	// merge queue, the one being merged and then the order the merger takes them in.
	started := func(c boardCard) string {
		if c.Run != nil {
			return c.Run.StartedAt
		}
		return "~"
	}
	sort.SliceStable(cards[laneBuilding], func(i, j int) bool { return started(cards[laneBuilding][i]) < started(cards[laneBuilding][j]) })
	blocked := func(c boardCard) string {
		if c.Block != nil {
			return c.Block.BlockedAt
		}
		return "~"
	}
	sort.SliceStable(cards[laneBlocked], func(i, j int) bool { return blocked(cards[laneBlocked][i]) < blocked(cards[laneBlocked][j]) })
	queued := func(c boardCard) string {
		switch {
		case c.Merge != nil:
			return ""
		case c.Manifest != nil:
			return c.Manifest.EnqueuedAt
		}
		return "~"
	}
	sort.SliceStable(cards[laneMerge], func(i, j int) bool { return queued(cards[laneMerge][i]) < queued(cards[laneMerge][j]) })

	// Proposals: the ones that wait for a decision, the oldest first, and then the approved ones,
	// which wait for a cycle to queue them.
	for _, p := range inbox.Proposals {
		if isChangeProposal(p) {
			cards[laneProposals] = append(cards[laneProposals], proposalCard(p))
		}
	}
	err = eachProposal(ctx, db, "approved", func(p store.Proposal) {
		if isChangeProposal(p) {
			cards[laneProposals] = append(cards[laneProposals], proposalCard(p))
		}
	})
	if err != nil {
		return nil, err
	}

	// The two folded lanes hold the latest cards of each kind, say how many they have in all, and
	// where the ones they leave out are listed.
	counts := map[string]int{}
	more := map[string][]boardRest{}
	for _, kind := range []struct{ state, rest string }{
		{store.ChangeUnknown, "The rest of the changes not in the backlog"},
		{store.ChangeDiscarded, "The rest of the discarded changes"},
	} {
		page, err := db.ListChangeOverviews(ctx, store.ChangeFilter{State: kind.state, Per: boardFoldedCards})
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			place(c, nil)
		}
		counts[laneDiscarded] += page.Total
		if page.Total > len(page.Items) {
			more[laneDiscarded] = append(more[laneDiscarded], boardRest{Label: kind.rest, Href: "/changes?state=" + url.QueryEscape(kind.state)})
		}
	}
	// The rejected proposals are counted over their whole list: the proposals to edit a prompt are
	// in it too, and they are not cards.
	rejected := 0
	err = eachProposal(ctx, db, "rejected", func(p store.Proposal) {
		if !isChangeProposal(p) {
			return
		}
		if rejected++; rejected <= boardFoldedCards {
			cards[laneDiscarded] = append(cards[laneDiscarded], proposalCard(p))
		}
	})
	if err != nil {
		return nil, err
	}
	counts[laneDiscarded] += rejected
	if rejected > boardFoldedCards {
		more[laneDiscarded] = append(more[laneDiscarded], boardRest{Label: "The rest of the rejected proposals", Href: "/backlog/proposals?status=rejected"})
	}

	// Integrated: the changes of the last days, the latest first. The list of changes gives them by
	// their last activity, and a change was integrated no later than that: its pages are read until
	// one ends before those days.
	since := at.Add(-boardIntegratedDays * 24 * time.Hour)
	var recent []store.ChangeOverview
	for n := 1; ; n++ {
		page, err := db.ListChangeOverviews(ctx, store.ChangeFilter{State: store.ChangeIntegrated, Page: n, Per: store.MaxPageSize})
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			if when, ok := parseWhen(c.IntegratedAt); ok && !when.Before(since) {
				recent = append(recent, c)
			}
		}
		if n >= page.Pages {
			break
		}
		if last, ok := parseWhen(page.Items[len(page.Items)-1].LastActivityAt); ok && last.Before(since) {
			break
		}
	}
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].IntegratedAt > recent[j].IntegratedAt })
	counts[laneIntegrated] = len(recent)
	if len(recent) > boardFoldedCards {
		more[laneIntegrated] = []boardRest{{Label: "The rest of the integrated changes", Href: "/changes?state=" + url.QueryEscape(store.ChangeIntegrated)}}
		recent = recent[:boardFoldedCards]
	}
	for _, c := range recent {
		place(c, nil)
	}

	p := &boardPage{StalledAfterSeconds: int(boardStalledAfter / time.Second), IntegratedDays: boardIntegratedDays}
	for _, lane := range boardLanes {
		lane.Cards = cards[lane.Key]
		if lane.Cards == nil {
			lane.Cards = []boardCard{}
		}
		lane.Count = len(lane.Cards)
		if n, counted := counts[lane.Key]; counted {
			lane.Count = n
		}
		lane.More = more[lane.Key]
		if lane.Folded {
			lane.Open = open[lane.Key]
			// The address of the board with this lane the other way, and the others as they are.
			var keys []string
			for _, other := range boardLanes {
				if other.Folded && open[other.Key] != (other.Key == lane.Key) {
					keys = append(keys, other.Key)
				}
			}
			lane.Toggle = hrefWith(r, "open", strings.Join(keys, ","))
		}
		if !lane.Strip() {
			// The tasks of a change are in a file of its worktree, or of the workspace: they are read
			// for the cards the page draws.
			for i := range lane.Cards {
				if lane.Cards[i].Kind == cardChange {
					lane.Cards[i].Tasks = s.changes.Tasks(lane.Cards[i].Name)
				}
			}
		}
		p.Lanes = append(p.Lanes, lane)
	}
	return p, nil
}
