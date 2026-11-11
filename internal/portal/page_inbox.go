package portal

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/mayeco/foreman-kit/internal/store"
)

// Needs you: everything that waits for a person, in one list. /inbox draws it and /api/inbox returns
// it. Each row comes from a row that is open in the database, except the question an agent left in
// a worktree, which is a file.

// The kinds of thing that wait for a person, as the kind parameter of /api/inbox names them.
const (
	inboxPause       = "pause"
	inboxBlocked     = "blocked"
	inboxQuestions   = "questions"
	inboxBrokenBases = "broken_bases"
	inboxLostRuns    = "lost_runs"
	inboxProposals   = "proposals"
)

var inboxKinds = []string{inboxPause, inboxBlocked, inboxQuestions, inboxBrokenBases, inboxLostRuns, inboxProposals}

// blockedChange is a change that is blocked, with what a person needs to decide about it.
type blockedChange struct {
	store.OpenBlock
	// Question is what its agent asked a person, when it left a question in the worktree.
	Question string `json:"question,omitempty"`
	// HasWorktree says the change has a worktree to intervene in.
	HasWorktree bool `json:"has_worktree"`
}

// agentQuestion is a question an agent left for a person in the worktree of a change that is not
// blocked.
type agentQuestion struct {
	ChangeName string `json:"change_name"`
	Question   string `json:"question"`
}

// inboxPage is what waits for a person.
type inboxPage struct {
	// Count is how many things wait, of every kind, whatever kind the read was asked for. Urgent says
	// one of them stops work: anything but a proposal.
	Count  int  `json:"count"`
	Urgent bool `json:"urgent"`
	// Kind is the kind the read was asked for, "" for all of them: the lists of the others are empty.
	Kind string `json:"kind,omitempty"`

	Pause       *store.Pause       `json:"pause,omitempty"`
	Blocked     []blockedChange    `json:"blocked"`
	Questions   []agentQuestion    `json:"questions"`
	BrokenBases []store.BrokenBase `json:"broken_bases"`
	LostRuns    []store.LostRun    `json:"lost_runs"`
	Proposals   []store.Proposal   `json:"proposals"`
}

func (p *inboxPage) head(v *pageView) {
	v.Title, v.Section = "Needs you", "/inbox"
	v.Crumbs = []crumb{{Label: "Needs you"}}
	// What waits changes when the kit works, not only when a person decides.
	v.Live = 15
}

func (s *Server) readInbox(ctx context.Context, db *store.View, r *http.Request) (*inboxPage, error) {
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind != "" {
		known := false
		for _, k := range inboxKinds {
			known = known || k == kind
		}
		if !known {
			return nil, errInvalid("%q is not a kind of thing that waits: %s", kind, strings.Join(inboxKinds, ", "))
		}
	}
	w, err := s.readWaiting(ctx, db)
	if err != nil {
		return nil, err
	}
	p := &inboxPage{
		Count: w.Count(), Urgent: w.Urgent(), Kind: kind,
		Blocked: []blockedChange{}, Questions: []agentQuestion{}, BrokenBases: []store.BrokenBase{},
		LostRuns: []store.LostRun{}, Proposals: []store.Proposal{},
	}
	want := func(k string) bool { return kind == "" || kind == k }
	if want(inboxPause) {
		p.Pause = w.Pause
	}
	if want(inboxBlocked) {
		for _, b := range w.Blocked {
			p.Blocked = append(p.Blocked, blockedChange{OpenBlock: b, Question: w.Questions[b.ChangeName], HasWorktree: s.changes.HasWorktree(b.ChangeName)})
		}
	}
	if want(inboxQuestions) {
		names := w.unblockedQuestions()
		sort.Strings(names)
		for _, name := range names {
			p.Questions = append(p.Questions, agentQuestion{ChangeName: name, Question: w.Questions[name]})
		}
	}
	if want(inboxBrokenBases) {
		p.BrokenBases = w.BrokenBases
	}
	if want(inboxLostRuns) {
		p.LostRuns = w.LostRuns
	}
	if want(inboxProposals) {
		p.Proposals = w.Proposals
	}
	return p, nil
}
