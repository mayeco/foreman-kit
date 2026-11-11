package portal

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// Visual QA as a person works it from the portal: ask for a check in a browser, open the session
// that takes the checks that wait, say how each one ended, and close the session. The kit does each
// of these (internal/cycle); what is here are the refusals a person reads, and the files a session
// leaves behind.

// VisualSetup says whether this application runs visual QA, and where a person turns it on or off.
type VisualSetup struct {
	Enabled bool `json:"enabled"`
	// Key names the setting that turns it on, and Where says it with its file.
	Key   string `json:"key"`
	Where string `json:"where"`
}

// VisualSetup reads whether this application runs visual QA.
func (w *workspace) VisualSetup() VisualSetup {
	return VisualSetup{Enabled: cycle.GetVisualQASettings(w.root).Enabled, Key: string(settings.VisualQAEnabled), Where: settings.VisualQAEnabled.Where()}
}

// visualVerdicts are the ways a check in a browser ends.
var visualVerdicts = []string{"verified", "regression", "not-visual", "cannot-verify"}

// visualCheck reads one check as it is on record.
func (w *workspace) visualCheck(ctx context.Context, id string) (*store.VisualQACheck, error) {
	var check *store.VisualQACheck
	err := w.store.View(ctx, func(db *store.View) (err error) {
		check, err = db.VisualQACheck(ctx, id)
		return err
	})
	return check, err
}

// RequestVisualCheck asks for a check in a browser: what to look at and, when it is about one, the
// change. It is 409 while this application does not run visual QA, and 404 for a change the kit
// has no record of: the registry gets no change from a name typed here.
func (w *workspace) RequestVisualCheck(ctx context.Context, what, change string) (*store.VisualQACheck, error) {
	what, change = strings.TrimSpace(what), strings.TrimSpace(change)
	if what == "" {
		return nil, errInvalid("what says what to look at in the browser, and it is empty")
	}
	if change != "" {
		if !cycle.ChangeNameRe.MatchString(change) {
			return nil, errInvalid("%q is not the name of a change", change)
		}
		state, err := w.store.ChangeState(ctx, change)
		if err != nil {
			return nil, err
		}
		if state == "" {
			return nil, errNotFound("the kit knows no change named %s", change)
		}
	}
	if setup := w.VisualSetup(); !setup.Enabled {
		return nil, errConflict("this application does not run visual QA: %s turns it on", setup.Where)
	}
	item, err := cycle.VisualQARequest(ctx, w.root, w.store, what, change, nil)
	if err != nil {
		return nil, err
	}
	return w.visualCheck(ctx, item.ItemID)
}

// ResolveVisualCheck records how a check in a browser ended. It is 404 for a check that is not on
// record and 409 for one that already ended.
func (w *workspace) ResolveVisualCheck(ctx context.Context, id, verdict, note string) (*store.VisualQACheck, error) {
	verdict = strings.TrimSpace(verdict)
	known := false
	for _, v := range visualVerdicts {
		known = known || v == verdict
	}
	if !known {
		return nil, errInvalid("verdict is one of %s, and %q is none of them", strings.Join(visualVerdicts, ", "), verdict)
	}
	check, err := w.visualCheck(ctx, id)
	if err != nil {
		return nil, err
	}
	if check == nil {
		return nil, errNotFound("no check in a browser is on record with the number %s", id)
	}
	if check.Status != "pending" && check.Status != "in_progress" {
		return nil, errConflict("the check %s already ended as %s", id, check.Status)
	}
	if err := cycle.VisualQADone(ctx, w.root, w.store, check.ItemID, verdict, note, nil); err != nil {
		return nil, err
	}
	return w.visualCheck(ctx, check.ItemID)
}

// visualSession reads the session in progress.
func (w *workspace) visualSession(ctx context.Context) (*store.VisualSession, error) {
	var session *store.VisualSession
	err := w.store.View(ctx, func(db *store.View) (err error) {
		session, err = db.VisualSession(ctx)
		return err
	})
	return session, err
}

// kitSaid returns the last thing the kit printed, without the name of the command in front of it.
func kitSaid(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimPrefix(strings.TrimSpace(lines[len(lines)-1]), "visual-qa: ")
}

// BeginVisualSession opens a visual QA session: it takes the checks that wait and writes their
// plan. It answers with the session and with what the kit printed. It is 409 when no session
// opened: one is open already, the kit is paused, or this application does not run visual QA.
func (w *workspace) BeginVisualSession(ctx context.Context) (*store.VisualSession, string, error) {
	open, err := w.visualSession(ctx)
	if err != nil {
		return nil, "", err
	}
	if open != nil {
		return nil, "", errConflict("the session %s is open: end it before another begins", open.Stamp)
	}
	var out strings.Builder
	// The session of the portal is worked by a person in their own browser: it prepares no worktree.
	if err := cycle.VisualQABegin(ctx, w.root, w.store, cycle.VisualQAOptions{SkipWorktreePrep: true}, &out); err != nil {
		return nil, out.String(), errConflict("no session opened: %v", err)
	}
	session, err := w.visualSession(ctx)
	if err != nil {
		return nil, out.String(), err
	}
	if session == nil {
		return nil, out.String(), errConflict("no session opened: %s", kitSaid(out.String()))
	}
	return session, strings.TrimSpace(out.String()), nil
}

// EndVisualSession closes the session in progress: the checks it did not get to go back to wait, and
// what it found is recorded. It answers with what the kit printed, and is 409 when no session is
// open.
func (w *workspace) EndVisualSession(ctx context.Context) (string, error) {
	// A session whose time ran out still holds its lock until one ends it or another begins.
	lock, err := w.store.GetLock(ctx, "visual_qa")
	if err != nil {
		return "", err
	}
	if lock == nil {
		return "", errConflict("no visual QA session is open")
	}
	var out strings.Builder
	if err := cycle.VisualQAEnd(ctx, w.root, w.store, cycle.VisualQAOptions{SkipWorktreePrep: true}, &out); err != nil {
		return out.String(), err
	}
	return strings.TrimSpace(out.String()), nil
}

// VisualSessionRecord is a visual QA session that left its plan among the files of the kit.
type VisualSessionRecord struct {
	Stamp string `json:"stamp"`
	// StartedAt is the instant its stamp names; HasReport says it wrote its report.
	StartedAt string `json:"started_at"`
	HasReport bool   `json:"has_report"`
}

// visualSessionsPerPage is how many sessions a page of their list holds.
const visualSessionsPerPage = 50

// VisualSessions lists the sessions that left a plan, the latest first: the ones before the stamp
// `before`, or the latest ones when it is empty. next is the stamp to ask the page that follows
// with, "" when this one is the last. A session is a file the kit wrote, not a row: the list is
// read from the folder of the cycle.
func (w *workspace) VisualSessions(before string) (sessions []VisualSessionRecord, total int, next string) {
	sessions = []VisualSessionRecord{}
	entries, err := os.ReadDir(store.RuntimePath(w.root, "cycle"))
	if err != nil {
		return sessions, 0, ""
	}
	var stamps []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "visual-") || !strings.HasSuffix(name, ".md") {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, "visual-"), ".md")
		if _, err := cycle.ParseStampTime(stamp); err == nil {
			stamps = append(stamps, stamp)
		}
	}
	// A stamp is written so that its order as text is its order in time.
	sort.Sort(sort.Reverse(sort.StringSlice(stamps)))
	total = len(stamps)
	for _, stamp := range stamps {
		if before != "" && stamp >= before {
			continue
		}
		if len(sessions) == visualSessionsPerPage {
			next = sessions[len(sessions)-1].Stamp
			break
		}
		at, _ := cycle.ParseStampTime(stamp)
		sessions = append(sessions, VisualSessionRecord{Stamp: stamp, StartedAt: at.UTC().Format(time.RFC3339),
			HasReport: isFile(store.RuntimePath(w.root, "cycle", "reports", "visual-"+stamp+".md"))})
	}
	return sessions, total, next
}

// VisualSessionFiles returns the plan and the report of a session, each nil when the session did
// not write it. A stamp that is not one names no file.
func (w *workspace) VisualSessionFiles(stamp string) (plan, report *CycleReport) {
	if _, err := cycle.ParseStampTime(stamp); err != nil {
		return nil, nil
	}
	name := "visual-" + stamp + ".md"
	plan = readCycleReport(filepath.Join(store.RuntimePath(w.root, "cycle"), name))
	report = readCycleReport(store.RuntimePath(w.root, "cycle", "reports", name))
	return plan, report
}
