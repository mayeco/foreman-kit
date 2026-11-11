package portal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/orchestrator"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// changes is what the portal does about one change besides reading its rows. It shows the files of
// the change, which live in a worktree or in the workspace, and it carries out what a person decides
// about it: unblock it, accept its risk, give the lease of its lost worker back, validate its specs,
// reset its worker, accept the base that holds it.
type changes struct {
	root  string
	store *store.Store
	hub   *orchestrator.Hub // nil outside the daemon: its notifications do nothing
}

// record returns what is on record about a change now.
func (c *changes) record(ctx context.Context, name string) (*store.ChangeRecord, error) {
	var rec *store.ChangeRecord
	err := c.store.View(ctx, func(v *store.View) (err error) {
		rec, err = v.Change(ctx, name)
		return err
	})
	return rec, err
}

// known returns the record of the change name, or the problem of a name that is not one of a change
// or of a change the kit has no record of.
func (c *changes) known(ctx context.Context, name string) (*store.ChangeRecord, error) {
	if !cycle.ChangeNameRe.MatchString(name) {
		return nil, errInvalid("%q is not the name of a change", name)
	}
	rec, err := c.record(ctx, name)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errNotFound("the kit has no record of a change named %s", name)
	}
	return rec, nil
}

// ---- the files of a change --------------------------------------------------------------------------

// The OpenSpec artifacts of a change the portal shows, besides its specs.
var artifactNames = []string{"proposal.md", "design.md", "tasks.md", "repairs.md", "handoff.md", needsHumanFile}

// needsHumanFile is where an agent leaves a question only a person can answer. It is the one fact
// about a change the portal reads from a file: the orchestrator does not record it as a block.
const needsHumanFile = ".needs-human"

// ArtifactFile is one file of a change: its path under the directory of the change, with slashes.
type ArtifactFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Artifacts are the OpenSpec files of a change and where they are now.
type Artifacts struct {
	// Source is "worktree" while the change is being worked on, "active" when it is in
	// openspec/changes of the workspace, "archived" once it is integrated, "" when it has none.
	Source string         `json:"source"`
	Files  []ArtifactFile `json:"files"`
	dir    string
}

// artifactDir returns the directory that holds the artifacts of a change now, and what it is.
func (c *changes) artifactDir(change string) (dir, source string) {
	if wt := filepath.Join(worktreeOf(c.root, change), "openspec", "changes", change); isDir(wt) {
		return wt, "worktree"
	}
	if active := filepath.Join(c.root, "openspec", "changes", change); isDir(active) {
		return active, "active"
	}
	// An archived change is in archive/<date>-<name>: the latest one of that name.
	archive := filepath.Join(c.root, "openspec", "changes", "archive")
	entries, err := os.ReadDir(archive)
	if err != nil {
		return "", ""
	}
	var matches []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), "-"+change) && len(e.Name()) == len(change)+len("2006-01-02-") {
			matches = append(matches, e.Name())
		}
	}
	if len(matches) == 0 {
		return "", ""
	}
	sort.Strings(matches)
	return filepath.Join(archive, matches[len(matches)-1]), "archived"
}

// Artifacts lists the OpenSpec files of a change: its documents, then its specs.
func (c *changes) Artifacts(change string) Artifacts {
	out := Artifacts{Files: []ArtifactFile{}}
	if !cycle.ChangeNameRe.MatchString(change) {
		return out
	}
	out.dir, out.Source = c.artifactDir(change)
	if out.dir == "" {
		return out
	}
	for _, name := range artifactNames {
		if info, err := os.Stat(filepath.Join(out.dir, name)); err == nil && !info.IsDir() {
			out.Files = append(out.Files, ArtifactFile{Name: name, Size: info.Size()})
		}
	}
	// A directory that cannot be walked has no spec to list: the documents above are still shown.
	_ = filepath.WalkDir(filepath.Join(out.dir, "specs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "spec.md" {
			return nil
		}
		rel, err := filepath.Rel(out.dir, path)
		if err != nil {
			return nil
		}
		file := ArtifactFile{Name: filepath.ToSlash(rel)}
		if info, err := d.Info(); err == nil {
			file.Size = info.Size()
		}
		out.Files = append(out.Files, file)
		return nil
	})
	return out
}

// ArtifactContent is one file of a change, as text.
type ArtifactContent struct {
	Name string `json:"name"`
	// Source is where the file was read: "worktree", "active" or "archived".
	Source  string `json:"source"`
	Content string `json:"content"`
	// Size is the size of the whole file; Truncated says Content is only its end.
	Size       int64  `json:"size"`
	Truncated  bool   `json:"truncated"`
	ModifiedAt string `json:"modified_at"`
}

// ReadArtifact returns one of the files Artifacts lists for the change: all of it when it fits in
// maxBytes or maxBytes is not positive, its end otherwise. A name that is not on that list is not
// found, whatever it points at.
func (c *changes) ReadArtifact(change, file string, maxBytes int) (ArtifactContent, error) {
	arts := c.Artifacts(change)
	listed := false
	for _, f := range arts.Files {
		listed = listed || f.Name == file
	}
	if !listed {
		return ArtifactContent{}, errNotFound("the change %s has no file named %s", change, file)
	}
	path := filepath.Join(arts.dir, filepath.FromSlash(file))
	raw, err := os.ReadFile(path)
	if err != nil {
		return ArtifactContent{}, fmt.Errorf("reading %s of %s: %w", file, change, err)
	}
	out := ArtifactContent{Name: file, Source: arts.Source, Content: string(raw), Size: int64(len(raw))}
	if info, err := os.Stat(path); err == nil {
		out.ModifiedAt = info.ModTime().UTC().Format(time.RFC3339)
	}
	if maxBytes > 0 && len(raw) > maxBytes {
		end := string(raw[len(raw)-maxBytes:])
		// The end starts at a line, not in the middle of one.
		if nl := strings.IndexByte(end, '\n'); nl >= 0 {
			end = end[nl+1:]
		}
		out.Content, out.Truncated = end, true
	}
	return out, nil
}

// Task is one task of a change: a line of its tasks.md with a box, checked or not.
type Task struct {
	// Section is the heading the task is under, "" for a task under none.
	Section string `json:"section,omitempty"`
	Text    string `json:"text"`
	Done    bool   `json:"done"`
}

// Tasks are the tasks of a change as its tasks.md lists them, where that file is now.
type Tasks struct {
	// Source is where the file was read: "worktree", "active" or "archived".
	Source string `json:"source"`
	Done   int    `json:"done"`
	Total  int    `json:"total"`
	Items  []Task `json:"items"`
}

var (
	// A task is an item of a list with a box: what the orchestrator counts when it audits the tasks
	// of a change before its merge (uncheckedTaskRe and checkedTaskRe in internal/orchestrator).
	taskLineRe    = regexp.MustCompile(`^\s*(?:[-*+]|\d{1,9}[.)])\s+\[([ xX])\]\s*(.*?)\s*$`)
	taskHeadingRe = regexp.MustCompile(`^#{1,6}\s+(.*?)[\s#]*$`)
)

// tasksFileLimit is how much of a tasks.md is read: far more than the plan of a change holds.
const tasksFileLimit = 256 << 10

// Tasks returns the tasks of a change, each with whether its box is checked, in the order of the
// file. It is nil for a change with no tasks.md, and for a tasks.md that lists no task: a change
// that was not planned yet has neither.
func (c *changes) Tasks(change string) *Tasks {
	if !cycle.ChangeNameRe.MatchString(change) {
		return nil
	}
	dir, source := c.artifactDir(change)
	if dir == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(dir, "tasks.md"))
	if err != nil {
		return nil
	}
	// The file was only read: a close that fails loses nothing.
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, tasksFileLimit))
	if err != nil {
		return nil
	}
	out := &Tasks{Source: source, Items: []Task{}}
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if m := taskHeadingRe.FindStringSubmatch(line); m != nil {
			section = m[1]
			continue
		}
		m := taskLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		task := Task{Section: section, Text: m[2], Done: m[1] != " "}
		out.Items = append(out.Items, task)
		out.Total++
		if task.Done {
			out.Done++
		}
	}
	if out.Total == 0 {
		return nil
	}
	return out
}

// NeedsHuman returns the question an agent left for a person in the worktree of a change, "" when
// there is none.
func (c *changes) NeedsHuman(change string) string {
	if !cycle.ChangeNameRe.MatchString(change) {
		return ""
	}
	raw, err := os.ReadFile(c.needsHumanPath(change))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// Questions returns the question an agent left for a person in each worktree that has one, by the
// name of its change.
func (c *changes) Questions() map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(settings.WorktreesBase(c.root))
	if err != nil {
		// No worktree at all: nobody asked anything.
		return out
	}
	for _, e := range entries {
		name, ok := strings.CutPrefix(e.Name(), "worker-")
		if !ok || !e.IsDir() {
			continue
		}
		if question := c.NeedsHuman(name); question != "" {
			out[name] = question
		}
	}
	return out
}

func (c *changes) needsHumanPath(change string) string {
	return filepath.Join(worktreeOf(c.root, change), "openspec", "changes", change, needsHumanFile)
}

// HasWorktree reports whether a change has a worktree now.
func (c *changes) HasWorktree(change string) bool {
	return cycle.ChangeNameRe.MatchString(change) && isDir(worktreeOf(c.root, change))
}

// RenderedPrompt returns the PLAN or the BUILD prompt of a change as the kit would write it now,
// from the templates and the memory of this application: what a worker starting now would be given,
// before what only its run knows (the feedback of a failed gate, the reason of a rebuild).
func (c *changes) RenderedPrompt(change, role string) (string, error) {
	if !cycle.ChangeNameRe.MatchString(change) {
		return "", errInvalid("%q is not the name of a change", change)
	}
	if role != "plan" && role != "build" {
		return "", errInvalid("%q is not a prompt of a worker: plan or build", role)
	}
	// A template that cannot be read leaves its rules out of the prompt; the prompt is still rendered.
	rules, _ := assets.ReadTemplate(c.root, "authoring_rules.md")
	return cycle.RenderPrompt(c.root, role, map[string]string{
		"change":          change,
		"goal":            "",
		"branch":          "feat/spec-" + change,
		"memory":          cycle.MemoryPromptBlock(c.root, nil),
		"evaluator_hint":  "",
		"authoring_rules": string(rules),
		"feedback":        "",
		"rebuild":         "",
	})
}

// ---- what a person decides about a change -----------------------------------------------------------

// Unblock lifts what stops a change until a person decides: its block in force, and the question an
// agent left for a person in its worktree. resetRetries starts the counters of the change over (its
// retries and rebuilds no longer count), which also withdraws a risk that was accepted for it.
func (c *changes) Unblock(ctx context.Context, name string, resetRetries bool) (*store.ChangeRecord, error) {
	rec, err := c.known(ctx, name)
	if err != nil {
		return nil, err
	}
	question := c.needsHumanPath(name)
	asked := isFile(question)
	if rec.Block == nil && !asked {
		return nil, errConflict("the change %s is not blocked", name)
	}
	if asked {
		if err := os.Remove(question); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("removing the question of %s: %w", name, err)
		}
	}
	if err := c.store.ResolveQuarantine(ctx, name, store.QuarantineResolution{By: "portal", Forget: resetRetries}); err != nil {
		return nil, err
	}
	c.hub.BacklogChanged()
	return c.record(ctx, name)
}

// ApproveRisk records that a person accepts the risk of a change. A block it has for a reason a
// person can accept is lifted by that; any other block stays, and the approval stands for when it
// is lifted.
func (c *changes) ApproveRisk(ctx context.Context, name string) (*store.ChangeRecord, error) {
	rec, err := c.known(ctx, name)
	if err != nil {
		return nil, err
	}
	if rec.State == store.ChangeIntegrated || rec.State == store.ChangeDiscarded {
		return nil, errConflict("the change %s is %s: it has no risk left to accept", name, wordingOf(vocChangeState, rec.State).Text)
	}
	if err := c.store.ApproveChangeRiskBy(ctx, name, "portal"); err != nil {
		return nil, err
	}
	c.hub.BacklogChanged()
	return c.record(ctx, name)
}

// Reclaim gives back the lease of a change whose worker is gone, so that the dispatcher takes the
// change again. The lease of a worker that is alive is never given back.
func (c *changes) Reclaim(ctx context.Context, name string) (*store.ChangeRecord, error) {
	if _, err := c.known(ctx, name); err != nil {
		return nil, err
	}
	key := store.WorkerLeaseKey(name)
	lease, err := c.store.GetLock(ctx, key)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, errConflict("the change %s has no lease to give back: nothing holds it, and the dispatcher takes it on its next pass", name)
	}
	if store.IsPIDAlive(lease.PID) {
		return nil, errConflict("the worker of %s (pid %d) is alive: its lease is not given back while it works", name, lease.PID)
	}
	// Only the row of the dead owner: a worker that took the stale lease after the check above
	// keeps the lease it has just taken.
	if err := c.store.ReleaseLock(ctx, key, lease.PID); err != nil {
		return nil, err
	}
	c.hub.BacklogChanged()
	return c.record(ctx, name)
}

// Validation is what validating the specs of a change found.
type Validation struct {
	OK       bool     `json:"ok"`
	Problems []string `json:"problems"`
	// Output says it in text, for a person.
	Output string `json:"output"`
}

// Validate checks the OpenSpec artifacts of a change in its worktree, with the checks the dispatcher
// runs before it builds: the proposal and the task list are there, and the delta specs are written
// the way the archive will accept them.
func (c *changes) Validate(ctx context.Context, name string) (Validation, error) {
	if _, err := c.known(ctx, name); err != nil {
		return Validation{}, err
	}
	wt := worktreeOf(c.root, name)
	if !isDir(wt) {
		return Validation{}, errConflict("the change %s has no worktree: its specs are validated only while it is being worked on", name)
	}
	v := Validation{Problems: []string{}}
	dir := filepath.Join(wt, "openspec", "changes", name)
	for _, required := range []string{"proposal.md", "tasks.md"} {
		if !isFile(filepath.Join(dir, required)) {
			v.Problems = append(v.Problems, "openspec/changes/"+name+"/"+required+" is missing")
		}
	}
	if ok, problem := orchestrator.ValidateOpenSpecNative(wt, name); !ok {
		v.Problems = append(v.Problems, problem)
	}
	v.OK = len(v.Problems) == 0
	v.Output = "The proposal, the task list and the delta specs of " + name + " are valid."
	if !v.OK {
		v.Output = strings.Join(v.Problems, "\n")
	}
	return v, nil
}

// resetLeaseTTL is how long a reset keeps the lease of the change it resets.
const resetLeaseTTL = 10 * time.Minute

// Reset removes what a worker built for a change, so that the change starts again from the backlog:
// its worktree, its branch, its place in the merge queue, its block and its counters. It refuses
// while a worker of the change is alive.
func (c *changes) Reset(ctx context.Context, name string) (*store.ChangeRecord, error) {
	if !cycle.ChangeNameRe.MatchString(name) {
		return nil, errInvalid("%q is not the name of a change", name)
	}
	// A worktree is reset whether or not the kit has a record of its change: one left behind under
	// that name is removed the same way. The answer is then no record at all.
	rec, err := c.record(ctx, name)
	if err != nil {
		return nil, err
	}
	if rec == nil && !isDir(worktreeOf(c.root, name)) {
		return nil, errNotFound("the kit has no record of a change named %s, and no worktree of it", name)
	}
	// The lease of the worker is held for the whole reset, not checked once: taking it fails while a
	// live worker holds it, takes over the lease of a dead one, and makes a dispatcher that picks the
	// change up meanwhile find it busy instead of building in a worktree this reset is deleting.
	key := store.WorkerLeaseKey(name)
	if _, err := c.store.AcquireLock(ctx, key, store.LockOptions{OwnerType: "portal_reset", TTL: resetLeaseTTL}); err != nil {
		if !errors.Is(err, store.ErrLockHeld) {
			// Never delete a worktree whose lease could not be taken.
			return nil, fmt.Errorf("taking the lease of %s to reset it: %w", name, err)
		}
		if lease, _ := c.store.GetLock(ctx, key); lease != nil {
			return nil, errConflict("the change %s is not reset while its worker (pid %d) is alive", name, lease.PID)
		}
		return nil, errConflict("the change %s is not reset while its lease is held", name)
	}
	// From here on the reset runs to its end whatever becomes of the request: stopped half way, it
	// would leave the worktree deleted and the branch, the manifest and the block behind.
	work := context.WithoutCancel(ctx)
	var failed []string
	note := func(what string, err error) {
		if err != nil {
			failed = append(failed, what+": "+err.Error())
		}
	}
	wt := worktreeOf(c.root, name)
	if repo, err := gitx.Open(c.root); err == nil {
		if isDir(wt) {
			note("removing the worktree", repo.RemoveWorktree(work, wt))
		}
		// The branch may not exist: a change that never got to build has none.
		_, _ = gitx.RunGit(work, c.root, "branch", "-D", "feat/spec-"+name)
	} else if isDir(wt) {
		note("removing the worktree", os.RemoveAll(wt))
	}
	if isDir(wt) {
		failed = append(failed, "the worktree is still there")
	}
	// The change can be dispatched again only once its worktree and its branch are gone.
	note("taking it out of the merge queue", c.store.CloseReady(work, name, store.ManifestRemoved))
	note("lifting its block", c.store.ResolveQuarantine(work, name, store.QuarantineResolution{By: "portal", Forget: true}))
	note("giving its lease back", c.store.ReleaseLock(work, key, os.Getpid()))
	// After the lease is given back: a dispatcher woken before would find it held and skip the change.
	c.hub.BacklogChanged()
	if len(failed) > 0 {
		return nil, fmt.Errorf("the reset of %s was left unfinished: %s", name, strings.Join(failed, "; "))
	}
	return c.record(work, name)
}

// AcceptedBase is a broken base a person accepted as it is.
type AcceptedBase struct {
	store.BrokenBase
	// Verdicts is how many failed verdicts of that base were marked as accepted.
	Verdicts int `json:"verdicts"`
}

// AcceptBase accepts the base of a child repository that fails on its own, at sha (its current
// broken base when sha is empty): the merger then integrates the changes it held there, with their
// own tests.
func (c *changes) AcceptBase(ctx context.Context, repo, sha string) (AcceptedBase, error) {
	repo = cycle.NormalizeRepoArg(repo)
	if repo == "" {
		return AcceptedBase{}, errInvalid("the repository whose base is accepted is missing")
	}
	broken, err := c.store.ListBrokenBases(ctx)
	if err != nil {
		return AcceptedBase{}, err
	}
	found := false
	for _, b := range broken {
		found = found || (b.Repo == repo && (sha == "" || strings.HasPrefix(b.SHA, strings.TrimSpace(sha))))
	}
	if !found {
		if strings.TrimSpace(sha) == "" {
			return AcceptedBase{}, errConflict("the base of %s is not broken: there is nothing to accept", repo)
		}
		return AcceptedBase{}, errConflict("the base of %s is not broken at %s: there is nothing to accept", repo, strings.TrimSpace(sha))
	}
	base, verdicts, err := cycle.AcceptBrokenBase(ctx, c.store, repo, sha, cycle.AcceptingUser())
	if err != nil {
		return AcceptedBase{}, err
	}
	c.hub.ReadyEnqueued()
	return AcceptedBase{BrokenBase: base, Verdicts: verdicts}, nil
}
