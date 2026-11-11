package portal

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/orchestrator"
	"github.com/mayeco/foreman-kit/internal/store"
)

// workspace is what a person does about the kit as a whole and about its queue: pause it and resume
// it, add a change to the backlog or take one out, decide on a proposal, start a cycle, ask the
// dispatcher what it would start, bring the repositories up to date.
type workspace struct {
	root  string
	store *store.Store
	hub   *orchestrator.Hub
	kit   Kit
	judge *evaluator.Judge
	// files keeps the edits this process makes of the backlog and of the proposals one at a time.
	files sync.Mutex
	// checks shares one run of the check of the configuration among the pages that ask at once,
	// and setups one run of the checklist of the setup.
	checks fresh[checkedConfig]
	setups fresh[setupRead]
}

// mirror brings the mirror of the backlog and of the proposals in the database up to date with
// their files, after an edit of this service: the pages read the mirror. The edit is done by then,
// so a failure to mirror it goes to the log and the watch of the files tries again.
func (w *workspace) mirror(ctx context.Context) {
	if err := w.store.SyncJSONToSQLite(ctx, w.root); err != nil {
		log.Printf("portal: bringing the mirror of the backlog up to date: %v", err)
	}
}

// ---- the pause ---------------------------------------------------------------------------------------

// Pause opens a pause of the kit, for reason: nothing new starts until it is resumed.
func (w *workspace) Pause(ctx context.Context, reason string) (*store.Pause, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "paused from the portal"
	}
	opened, err := w.store.Pause(ctx, reason, "portal", "")
	if err != nil {
		return nil, err
	}
	if !opened {
		return nil, errConflict("the kit is already paused")
	}
	return w.openPause(ctx)
}

// Resume closes the pause in force: the cycle, the dispatcher and the merger start again on their
// next pass.
func (w *workspace) Resume(ctx context.Context) error {
	resumed, err := w.store.Resume(ctx, "portal")
	if err != nil {
		return err
	}
	// The file an earlier kit paused with: it has no say any more, and is not left behind.
	if err := os.Remove(store.RuntimePath(w.root, "PAUSE")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("portal: removing the pause file of an earlier kit: %v", err)
	}
	if !resumed {
		return errConflict("the kit is not paused")
	}
	// The loops of the daemon sleep while paused: integration and dispatch go on now.
	w.hub.ReadyEnqueued()
	w.hub.BacklogChanged()
	return nil
}

func (w *workspace) openPause(ctx context.Context) (*store.Pause, error) {
	var pause *store.Pause
	err := w.store.View(ctx, func(db *store.View) (err error) {
		pause, err = db.OpenPause(ctx)
		return err
	})
	return pause, err
}

// ---- the backlog -------------------------------------------------------------------------------------

// BacklogItem is a change a person adds to the backlog.
type BacklogItem struct {
	ChangeName  string   `json:"change_name"`
	Priority    string   `json:"priority"`
	Description string   `json:"description"`
	DependsOn   []string `json:"depends_on,omitempty"`
	QueuedAt    string   `json:"queued_at"`
}

// backlogPriorities are the priorities a change of the backlog can have, the most urgent first.
var backlogPriorities = []string{"critical", "high", "normal", "low"}

// readBacklog reads the backlog for an edit that writes the list back. A file that cannot be read
// (caught half-written, in the middle of a merge, not valid JSON) is a failure, never an empty
// backlog: the edited "empty" list would replace it, and every change in it would be lost. A file
// that is missing is refused for the same reason: an application always has one, so one that is
// gone was deleted, and the edit would write a backlog with nothing of what it held.
func (w *workspace) readBacklog() ([]map[string]any, error) {
	path := filepath.Join(w.root, filepath.FromSlash(cycle.BacklogFile))
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, errConflict("%s is missing from the main checkout, so the backlog was left as it is: bring the file back first (git restore %s)", cycle.BacklogFile, cycle.BacklogFile)
	}
	items, err := cycle.ReadJSONList(path)
	if err != nil {
		return nil, fmt.Errorf("%s cannot be read, so it was left as it is: %w", cycle.BacklogFile, err)
	}
	return items, nil
}

// BacklogProblem says what keeps the backlog of the main checkout from being read as a list, in
// words that follow "the backlog is": "missing", or why it cannot be read. It is "" when nothing
// does. A backlog that is missing reads as an empty queue to everything that reads it, with no
// error, and every change on record then shows as not in the backlog: the pages that show the queue
// say why. It reads that one file, so a page asks on every refresh.
func (w *workspace) BacklogProblem() string {
	path := filepath.Join(w.root, filepath.FromSlash(cycle.BacklogFile))
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return "missing"
	}
	if _, err := cycle.ReadJSONList(path); err != nil {
		return "not a list the kit can read (" + err.Error() + ")"
	}
	return ""
}

// AddToBacklog queues a change: it adds it to the backlog file, which is where the dispatcher reads
// what to build. dependsOn names the changes it waits for, each a name or several separated by commas.
func (w *workspace) AddToBacklog(ctx context.Context, name, priority, description string, dependsOn []string) (BacklogItem, error) {
	item := BacklogItem{ChangeName: strings.TrimSpace(name), Priority: strings.TrimSpace(priority), Description: strings.TrimSpace(description)}
	if !cycle.ChangeNameRe.MatchString(item.ChangeName) {
		return item, errInvalid("%q is not a name for a change: lowercase letters, digits and hyphens", item.ChangeName)
	}
	if item.Priority == "" {
		item.Priority = "normal"
	}
	known := false
	for _, p := range backlogPriorities {
		known = known || p == item.Priority
	}
	if !known {
		return item, errInvalid("%q is not a priority: %s", item.Priority, strings.Join(backlogPriorities, ", "))
	}
	if item.Description == "" {
		return item, errInvalid("the change needs a description: what to build, and how to accept it")
	}
	seen := map[string]bool{}
	for _, raw := range dependsOn {
		for _, dep := range strings.Split(raw, ",") {
			dep = strings.TrimSpace(dep)
			switch {
			case dep == "" || seen[dep]:
				continue
			case !cycle.ChangeNameRe.MatchString(dep):
				return item, errInvalid("%q is not the name of a change it could depend on", dep)
			case dep == item.ChangeName:
				return item, errInvalid("a change cannot depend on itself")
			}
			seen[dep] = true
			item.DependsOn = append(item.DependsOn, dep)
		}
	}

	w.files.Lock()
	defer w.files.Unlock()
	items, err := w.readBacklog()
	if err != nil {
		return item, err
	}
	for _, it := range items {
		if asStr(it["change_name"]) == item.ChangeName {
			return item, errConflict("%s is already in the backlog", item.ChangeName)
		}
	}
	// The kit knows a change by its name for good: one it built, discarded or is working on is not
	// queued a second time under the same name.
	state, err := w.store.ChangeState(ctx, item.ChangeName)
	if err != nil {
		return item, err
	}
	if state != "" && state != store.ChangeUnknown && state != store.ChangeQueued {
		return item, errConflict("the kit already has a change named %s, and it is %s: queue this one under another name",
			item.ChangeName, wordingOf(vocChangeState, state).Text)
	}

	item.QueuedAt = time.Now().UTC().Format(time.RFC3339)
	entry := map[string]any{"change_name": item.ChangeName, "priority": item.Priority, "description": item.Description, "queued_at": item.QueuedAt}
	if len(item.DependsOn) > 0 {
		entry["depends_on"] = item.DependsOn
	}
	if err := cycle.WriteJSONList(filepath.Join(w.root, cycle.BacklogFile), append(items, entry)); err != nil {
		return item, err
	}
	w.mirror(ctx)
	w.hub.BacklogChanged()
	return item, nil
}

// RemoveFromBacklog takes a change out of the backlog file. Only a change that waits with nothing
// open is taken out here: one a worker builds, or that is blocked or in the merge queue, has rows
// that would be left without their change.
func (w *workspace) RemoveFromBacklog(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	w.files.Lock()
	defer w.files.Unlock()
	items, err := w.readBacklog()
	if err != nil {
		return err
	}
	kept := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if asStr(it["change_name"]) != name {
			kept = append(kept, it)
		}
	}
	if len(kept) == len(items) {
		return errNotFound("the backlog has no change named %s", name)
	}
	state, err := w.store.ChangeState(ctx, name)
	if err != nil {
		return err
	}
	if state != "" && state != store.ChangeUnknown && state != store.ChangeQueued {
		return errConflict("%s is %s: only a change that waits with nothing open is taken out of the backlog here",
			name, wordingOf(vocChangeState, state).Text)
	}
	if err := cycle.WriteJSONList(filepath.Join(w.root, cycle.BacklogFile), kept); err != nil {
		return err
	}
	w.mirror(ctx)
	w.hub.BacklogChanged()
	return nil
}

// Abandon takes a change out of the work of the kit for good: out of the backlog file and of the
// merge queue, with its block lifted, a rebuild the merger asked for spent, and the change
// discarded in the registry. It is what `foreman-kit abandon` does. A change a worker is building
// or the merger is integrating is not abandoned here: the portal does not stop the process that
// has it, and that process would go on with a change the registry calls discarded.
func (w *workspace) Abandon(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if !cycle.ChangeNameRe.MatchString(name) {
		return errInvalid("%q is not the name of a change", name)
	}
	w.files.Lock()
	defer w.files.Unlock()
	state, err := w.store.ChangeState(ctx, name)
	if err != nil {
		return err
	}
	switch state {
	case "":
		return errNotFound("the kit knows no change named %s", name)
	case store.ChangeBuilding:
		return errConflict("a worker is building %s: it can be abandoned once its run ends", name)
	case store.ChangeMerging:
		return errConflict("the merger is integrating %s: it can be abandoned once that attempt ends", name)
	case store.ChangeIntegrated:
		return errConflict("%s is integrated: its commits are on the base branches", name)
	case store.ChangeDiscarded:
		return errConflict("%s is already discarded", name)
	}

	// The backlog first: the dispatcher does not take a change the file no longer lists.
	items, err := w.readBacklog()
	if err != nil {
		return err
	}
	kept := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if asStr(it["change_name"]) != name {
			kept = append(kept, it)
		}
	}
	if len(kept) != len(items) {
		if err := cycle.WriteJSONList(filepath.Join(w.root, cycle.BacklogFile), kept); err != nil {
			return err
		}
	}
	if err := w.store.CloseReady(ctx, name, store.ManifestRemoved); err != nil {
		return err
	}
	if err := w.store.ResolveQuarantine(ctx, name, store.QuarantineResolution{Resolution: store.BlockAbandoned, By: "portal", Forget: true}); err != nil {
		return err
	}
	if err := w.store.MarkChangeDiscarded(ctx, name); err != nil {
		return err
	}
	// The question an agent left for a person in the worktree has nobody left to answer it for.
	question := filepath.Join(worktreeOf(w.root, name), "openspec", "changes", name, needsHumanFile)
	if err := os.Remove(question); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("portal: removing the question of %s, which was abandoned: %v", name, err)
	}
	w.mirror(ctx)
	w.hub.BacklogChanged()
	return nil
}

// ---- the proposals -----------------------------------------------------------------------------------

// DecideProposal records the decision of a person about a proposal: approved, it is queued by the
// next cycle while the queue has room; rejected, it is not proposed again.
func (w *workspace) DecideProposal(ctx context.Context, id string, approve bool) error {
	id = strings.TrimSpace(id)
	w.files.Lock()
	defer w.files.Unlock()
	items, err := cycle.ReadJSONList(filepath.Join(w.root, filepath.FromSlash(cycle.ProposalsFile)))
	if err != nil {
		return fmt.Errorf("%s cannot be read, so it was left as it is: %w", cycle.ProposalsFile, err)
	}
	status, found := "", false
	for _, it := range items {
		if asStr(it["change_name"]) == id {
			status, found = asStr(it["status"]), true
			break
		}
	}
	switch {
	case !found:
		return errNotFound("there is no proposal named %s", id)
	case status != "proposed" && status != "approved":
		return errConflict("the proposal %s is %s: it is not waiting for a decision", id, status)
	}
	decision := "rejected"
	if approve {
		decision = "approved"
	}
	if err := cycle.DecideProposal(w.root, id, decision, nil); err != nil {
		return err
	}
	w.mirror(ctx)
	w.hub.BacklogChanged()
	return nil
}

// ReviewProposals asks the judge about the proposals nobody decided, applies what it approves, and
// returns what the review said.
func (w *workspace) ReviewProposals(ctx context.Context) (string, error) {
	w.files.Lock()
	var out strings.Builder
	err := cycle.ReviewProposals(ctx, w.root, w.store, w.judge, &out)
	w.files.Unlock()
	// The review may have decided proposals and queued the approved ones, even when it ends in an error.
	w.mirror(ctx)
	w.hub.BacklogChanged()
	if err != nil {
		return "", errConflict("the proposals could not be reviewed: %v", err)
	}
	return strings.TrimSpace(out.String()), nil
}

// ---- the cycle, the dispatcher and the repositories -------------------------------------------------

// StartCycle starts a cycle that goes on after the request, and returns what starting it said.
func (w *workspace) StartCycle(ctx context.Context) (string, error) {
	out, err := w.kit.StartCycle(ctx, w.root)
	out = strings.TrimSpace(out)
	if err != nil {
		return "", errConflict("the cycle did not start: %s", firstNonEmptyLine(out, err.Error()))
	}
	return out, nil
}

// Simulation is what the dispatcher would do on its next pass, as it says it, at one instant. It is
// worked out from what the dispatcher has in memory and in the worktrees, and nothing of it is kept.
type Simulation struct {
	ComputedAt string `json:"computed_at"`
	// Output is the report as the dispatcher prints it.
	Output string                     `json:"output"`
	Report *orchestrator.DryRunReport `json:"report"`
}

// SimulateDispatch asks the dispatcher what it would start now and why each change of the backlog
// waits.
func (w *workspace) SimulateDispatch(ctx context.Context) (Simulation, error) {
	rep, text, err := w.kit.SimulateDispatch(ctx, w.root)
	if err != nil {
		return Simulation{}, errConflict("the dispatcher could not say what it would start: %v", err)
	}
	at := time.Now().UTC()
	return Simulation{ComputedAt: at.Format(time.RFC3339), Report: rep,
		Output: "computed at " + at.Format("15:04:05") + " UTC\n\n" + strings.TrimSpace(text)}, nil
}

// SyncRepos brings the main checkout and the child repositories up to date with their remotes, and
// returns what the sync said. publish also pushes what the kit committed.
//
// It moves nothing while a visual QA session is in progress. The session works in the main checkout,
// and neither a cycle nor the dispatcher moves that checkout until it ends; the sync itself does not
// look, so the action does.
func (w *workspace) SyncRepos(ctx context.Context, publish bool) (string, error) {
	if stamp := cycle.ActiveVisualSession(ctx, w.store); stamp != "" {
		return "", errConflict("the visual QA session %s is in progress, and the main checkout is left as it is until it ends: end the session, then sync", stamp)
	}
	var out strings.Builder
	if err := cycle.SyncRepos(ctx, w.root, w.store, publish, &out); err != nil {
		return "", errConflict("the repositories could not be brought up to date: %s", firstNonEmptyLine(lastLines(out.String(), 1), err.Error()))
	}
	return strings.TrimSpace(out.String()), nil
}

// ---- the report of a cycle ---------------------------------------------------------------------------

// maxCycleReportBytes is how much of the report of a cycle a page shows; cycleReportWindow, how far
// back from the end of a cycle its report is looked for, minute by minute.
const (
	maxCycleReportBytes = 200 << 10
	cycleReportWindow   = 4 * time.Hour
)

// cycleReportName is how the judge of a cycle names its report: the minute it wrote it, in UTC.
const cycleReportName = "20060102-1504"

// CycleReport is the report of a cycle, as text.
type CycleReport struct {
	// Name is the name of the file. Recorded says the cycle recorded it as its report; when it did
	// not, the file is the one of the kit's reports that was written while the cycle ran.
	Name     string `json:"name"`
	Recorded bool   `json:"recorded"`
	Text     string `json:"text"`
	// Size is the size of the whole file; Truncated says Text is only its end.
	Size      int64 `json:"size"`
	Truncated bool  `json:"truncated,omitempty"`
}

// CycleReport returns the report of a cycle, nil when none is found. It is the file the cycle
// recorded as its report, when it recorded one. The judge names its report after the minute it
// writes it, which the cycle does not record: failing a recorded one, the report is the file of the
// kit's reports named after a minute in which the cycle ran and written during it, the last of them.
func (w *workspace) CycleReport(c store.Cycle) *CycleReport {
	dir := store.RuntimePath(w.root, "cycle", "reports")
	if c.ReportPath != "" {
		// Only a file of the kit's reports is read, whatever the row says.
		if path := filepath.Clean(c.ReportPath); filepath.Dir(path) == dir {
			if rep := readCycleReport(path); rep != nil {
				rep.Recorded = true
				return rep
			}
		}
	}
	started, err := time.Parse(time.RFC3339, c.StartedAt)
	if err != nil {
		return nil
	}
	ended := time.Now()
	if c.FinishedAt != "" {
		if ended, err = time.Parse(time.RFC3339, c.FinishedAt); err != nil {
			return nil
		}
	}
	first := started.UTC().Truncate(time.Minute)
	if limit := ended.Add(-cycleReportWindow); first.Before(limit) {
		first = limit.UTC().Truncate(time.Minute)
	}
	// The clock of the judge and the instant the cycle was closed may be a minute apart. A file named
	// after a minute two cycles share belongs to the one it was written during.
	latest := ended.Add(time.Minute)
	for at := latest.UTC().Truncate(time.Minute); !at.Before(first); at = at.Add(-time.Minute) {
		path := filepath.Join(dir, at.Format(cycleReportName)+".md")
		info, err := os.Stat(path)
		if err != nil || info.ModTime().Before(started) || info.ModTime().After(latest) {
			continue
		}
		if rep := readCycleReport(path); rep != nil {
			return rep
		}
	}
	return nil
}

// readCycleReport reads a report: all of it when it fits in maxCycleReportBytes, its end otherwise.
func readCycleReport(path string) *CycleReport {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	rep := &CycleReport{Name: filepath.Base(path), Text: string(raw), Size: int64(len(raw))}
	if len(raw) > maxCycleReportBytes {
		end := strings.ToValidUTF8(string(raw[len(raw)-maxCycleReportBytes:]), "")
		// The end starts at a line, not in the middle of one.
		if nl := strings.IndexByte(end, '\n'); nl >= 0 {
			end = end[nl+1:]
		}
		rep.Text, rep.Truncated = end, true
	}
	return rep
}
