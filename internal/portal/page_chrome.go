package portal

import (
	"context"
	"log"

	"github.com/mayeco/foreman-kit/internal/store"
)

// What every page has around its content: the menu, whether the kit is paused, and how many things
// wait for a person. It is read from the snapshot the page is read from, and it writes nothing.

// waiting is what waits for a person, read from the rows that are open and from the questions agents
// left in the worktrees.
type waiting struct {
	store.Inbox
	// Questions holds the question an agent left in the worktree of each change, by its name. It is
	// the one thing here that comes from files: the orchestrator does not record it as a block.
	Questions map[string]string `json:"questions"`
}

// unblockedQuestions returns the changes an agent asked a question about and that are not blocked:
// the question of a blocked change is shown with its block.
func (w waiting) unblockedQuestions() []string {
	blocked := map[string]bool{}
	for _, b := range w.Blocked {
		blocked[b.ChangeName] = true
	}
	var names []string
	for name := range w.Questions {
		if !blocked[name] {
			names = append(names, name)
		}
	}
	return names
}

// Count returns how many things wait for a person: the rows of Needs you.
func (w waiting) Count() int {
	return w.Inbox.Count() + len(w.unblockedQuestions())
}

// Urgent reports whether something that waits stops work: anything but a proposal nobody decided.
func (w waiting) Urgent() bool {
	return w.Count() > len(w.Proposals)
}

// readWaiting reads what waits for a person.
func (s *Server) readWaiting(ctx context.Context, db *store.View) (waiting, error) {
	in, err := db.ListInbox(ctx)
	return waiting{Inbox: in, Questions: s.changes.Questions()}, err
}

// chrome fills what every page shows around its content. A page that cannot read it is still
// drawn, without the count of what waits: the failure goes to the log.
func (s *Server) chrome(ctx context.Context, db *store.View, v *pageView) {
	v.App = s.app
	w, err := s.readWaiting(ctx, db)
	if err != nil {
		log.Printf("portal: reading what waits for a person: %v", err)
		// A part of the count would read as the whole of it.
		w = waiting{}
	}
	if w.Pause != nil {
		v.Paused, v.PauseReason = true, w.Pause.Reason
	}
	v.Nav = navOf(w)
	v.chromed = true
}

// systemPages are the pages of System. The menu has one entry for all of them, and each of them
// lists the others in a strip of tabs.
var systemPages = []navItem{
	{Href: "/system/health", Label: "Health", Icon: "heart-pulse"},
	{Href: "/system/settings", Label: "Settings", Icon: "sliders-horizontal"},
	{Href: "/system/setup", Label: "Setup", Icon: "wrench"},
	{Href: "/system/processes", Label: "Processes", Icon: "cpu"},
	{Href: "/system/prompts", Label: "Prompt templates", Icon: "file-text"},
	{Href: "/system/appearance", Label: "Appearance", Icon: "palette"},
}

// navOf returns the menu. Needs you is the one entry with a number: how many things wait for a
// person, in the color of an error when one of them stops work.
func navOf(w waiting) []navGroup {
	needs := navItem{Href: "/inbox", Label: "Needs you", Icon: "hand", Count: w.Count(), Tone: "warning"}
	if w.Urgent() {
		needs.Tone = "error"
	}
	return []navGroup{
		{Items: []navItem{
			{Href: "/", Label: "Dashboard", Icon: "activity"},
			needs,
		}},
		{Title: "Work", Items: []navItem{
			{Href: "/board", Label: "Board", Icon: "square-kanban"},
			{Href: "/backlog", Label: "Backlog", Icon: "list-ordered"},
			{Href: "/changes", Label: "Changes", Icon: "git-branch"},
			{Href: "/merge", Label: "Merge queue", Icon: "git-merge"},
			{Href: "/executions", Label: "Executions", Icon: "terminal"},
		}},
		{Title: "Observe", Items: []navItem{
			{Href: "/cycles", Label: "Cycles", Icon: "orbit"},
			{Href: "/events", Label: "Events", Icon: "radio-tower"},
			{Href: "/quality", Label: "Assessments", Icon: "clipboard-check"},
			{Href: "/visual", Label: "Visual QA", Icon: "scan-eye"},
		}},
		{Title: "Agents", Items: []navItem{
			{Href: "/agents", Label: "Agents", Icon: "bot"},
		}},
		{Title: "System", Items: []navItem{
			{Href: "/system", Label: "System", Icon: "sliders-horizontal"},
		}},
	}
}
