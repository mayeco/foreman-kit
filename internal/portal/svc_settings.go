package portal

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/orchestrator"
	"github.com/mayeco/foreman-kit/internal/settings"
)

// The settings of the application as the catalog of the kit describes them (internal/settings):
// which files there are, what each key decides and accepts, and what its file holds. No key and no
// file is named here: a key that is renamed, or a file that is added, shows without a change of the
// portal.

// Checked is one thing `foreman-kit config check` checks, with what it found.
type Checked struct {
	// Name is the path of a file, or "application" for what the check says of the application as a
	// whole.
	Name     string   `json:"name"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// SettingsFile is one configuration file of the application: what it decides, who reads it and
// what the check says of it. A process that reads a file with an error starts no new work until the
// file is mended.
type SettingsFile struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	About  string `json:"about"`
	ReadBy string `json:"read_by"`
	Checked
}

// SettingState is one setting as it stands in the application: what the catalog says of it, and
// what its file holds, written the way a person reads it.
type SettingState struct {
	settings.Described
	// Option is the key as its file writes it ("gates.timeout").
	Option string `json:"option"`
	// Written is what the file holds on one line, "" for a key that is missing. Lines is the same as
	// YAML, for what a file does not write on one line (the commands, the strategies).
	Written string `json:"written"`
	Lines   string `json:"lines,omitempty"`
	// Typed is the value of now as a person types it for POST /api/system/settings, which takes what
	// `foreman-kit config set` takes: what Written holds for one value, and the agents of a role as
	// names separated by commas, where their file writes a list.
	Typed string `json:"typed"`
	// Shipped and ShippedLines are the value the kit ships, likewise: "" when it ships none.
	Shipped      string `json:"shipped"`
	ShippedLines string `json:"shipped_lines,omitempty"`
	// AsShipped says the file holds the value the kit ships.
	AsShipped bool `json:"as_shipped"`
	// Settable says POST /api/system/settings changes it, as the catalog says `foreman-kit config set`
	// does: it is one value, or the agents of a role.
	// Change, of the catalog, says how a person changes one that is not.
	Settable bool `json:"settable"`
}

// ProcessWait is a process of the kit that starts no new work with the configuration as it stands,
// and why.
type ProcessWait struct {
	Process string `json:"process"`
	Why     string `json:"why"`
}

// SettingsReport is the configuration of the application as it stands.
type SettingsReport struct {
	// Files are the configuration files, in the order `foreman-kit config show` prints them, and
	// Others what else `foreman-kit config check` checks: the backlog, the proposals, the prompts,
	// the file of each repository and the application as a whole.
	Files  []SettingsFile `json:"files"`
	Others []Checked      `json:"others"`
	// Errors and Warnings count what the check found in all of them.
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	// Waits lists the processes that start no new work until a file is mended.
	Waits []ProcessWait `json:"waits"`

	// File is the file whose settings are listed: the one asked for, else the first one with an
	// error, else the first one.
	File     SettingsFile   `json:"file"`
	Settings []SettingState `json:"settings"`
	// Editing is the setting a person asked to change, nil when none was asked for.
	Editing *SettingState `json:"editing,omitempty"`
}

// fresh shares one run of a read that is costly to run many times at once (it starts processes)
// among the callers that ask for it at the same time. What a caller gets was read after it asked: a
// caller that arrives while a read runs waits for the next one, which every caller that arrived
// meanwhile shares. Nothing is kept for a caller that asks later, and what is shared is only read.
type fresh[T any] struct {
	mu sync.Mutex
	// started counts the reads that have begun, and lastRun is the one that returned last.
	started atomic.Uint64
	lastRun uint64
	last    T
}

func (f *fresh[T]) get(read func() T) T {
	asked := f.started.Load()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastRun > asked {
		return f.last
	}
	f.lastRun = f.started.Add(1)
	f.last = read()
	return f.last
}

// checkedConfig is what the kit says of the configuration as it stands: what `foreman-kit config
// check` found in each thing it checks, the order it prints them in, and the processes that start
// no new work because of it.
type checkedConfig struct {
	found map[string]config.FileProblems
	order []string
	waits []ProcessWait
}

// checked asks the kit about the configuration. The check reads every file of the configuration and
// asks git about the application, and each process reads its files again to say whether it waits:
// the pages that ask at the same time share one run of it all.
func (w *workspace) checked() checkedConfig {
	return w.checks.get(func() checkedConfig {
		found := config.Check(w.root)
		return checkedConfig{found: found, order: config.CheckOrder(w.root, found), waits: w.waits()}
	})
}

// fitsOnOneLine says a file writes v on one line: one value, or a list of them.
func fitsOnOneLine(v any) bool {
	switch val := v.(type) {
	case map[string]any:
		return false
	case []any:
		for _, item := range val {
			switch item.(type) {
			case map[string]any, []any:
				return false
			}
		}
	}
	return true
}

// writtenAs writes a value the way a page shows it: on one line when a file holds it on one, as
// YAML otherwise.
func writtenAs(v any) (line, lines string) {
	if v == nil {
		return "", ""
	}
	if fitsOnOneLine(v) {
		return settings.Inline(v), ""
	}
	raw, err := yaml.Marshal(v)
	if err != nil {
		return settings.Inline(v), ""
	}
	return "", strings.TrimRight(string(raw), "\n")
}

// stateOf describes one setting of the catalog as r reads it.
func stateOf(r *settings.Reader, s settings.Setting) SettingState {
	d := r.Describe(s.Key)
	st := SettingState{Described: d, Option: s.Key.Path(), Settable: s.Settable()}
	if d.Set {
		st.Written, st.Lines = writtenAs(d.Value)
		st.Typed = st.Written
		if s.Kind == settings.KindAgents {
			st.Typed = strings.Join(config.Names(d.Value), ", ")
		}
	}
	st.Shipped, st.ShippedLines = writtenAs(d.Default)
	st.AsShipped = d.Set && d.Default != nil && st.Written == st.Shipped && st.Lines == st.ShippedLines
	return st
}

// Settings reads the configuration of the application: its files with what the check says of each,
// the settings of one of them (the file called file; with "", the file of the setting to edit, else
// the first one with an error, else the first one), and the setting called edit when a person asked
// to change one. A file or a setting the catalog does not have is refused.
func (w *workspace) Settings(file, edit string) (SettingsReport, error) {
	file, edit = strings.TrimSpace(file), strings.TrimSpace(edit)
	// The values come from the files; what replaces one for this process only shows as what
	// overrides it here.
	r := &settings.Reader{Root: w.root, Stderr: io.Discard}
	rep := SettingsReport{Files: []SettingsFile{}, Others: []Checked{}, Waits: []ProcessWait{}, Settings: []SettingState{}}

	if edit != "" {
		s, ok := settings.Lookup(settings.Key(edit))
		if !ok {
			return rep, errInvalid("edit names the setting to change, and %q is not one: the page of each file lists its settings", edit)
		}
		if file != "" && file != s.Key.File() {
			return rep, errInvalid("%s is a setting of %s, not of the file %q", edit, s.Key.FilePath(), file)
		}
		file = s.Key.File()
		st := stateOf(r, s)
		rep.Editing = &st
	}

	checked := w.checked()
	rep.Waits = checked.waits
	inFiles := map[string]bool{}
	chosen := -1
	for _, f := range settings.Files() {
		found := checked.found[f.Path]
		sf := SettingsFile{Name: f.Name, Path: f.Path, About: f.About, ReadBy: f.ReadBy,
			Checked: Checked{Name: f.Path, Errors: orEmpty(found.Errors), Warnings: orEmpty(found.Warnings)}}
		inFiles[f.Path] = true
		rep.Errors += len(sf.Errors)
		rep.Warnings += len(sf.Warnings)
		switch {
		case file == f.Name:
			chosen = len(rep.Files)
		case file == "" && chosen < 0 && len(sf.Errors) > 0:
			chosen = len(rep.Files)
		}
		rep.Files = append(rep.Files, sf)
	}
	for _, name := range checked.order {
		if inFiles[name] {
			continue
		}
		found := checked.found[name]
		rep.Errors += len(found.Errors)
		rep.Warnings += len(found.Warnings)
		rep.Others = append(rep.Others, Checked{Name: name, Errors: orEmpty(found.Errors), Warnings: orEmpty(found.Warnings)})
	}
	if chosen < 0 {
		if file != "" {
			return rep, errInvalid("file names a configuration file, and %q is not one: the page lists them", file)
		}
		chosen = 0
	}
	if len(rep.Files) == 0 {
		return rep, nil
	}
	rep.File = rep.Files[chosen]
	for _, s := range settings.In(rep.File.Name) {
		rep.Settings = append(rep.Settings, stateOf(r, s))
	}
	return rep, nil
}

// orEmpty returns list, or an empty one in the place of none.
func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// waits lists the processes of the kit that start no new work with the configuration as it stands,
// each as the process itself says it: a file it reads lacks a key, holds a value the key does not
// accept or writes a key nothing reads, or the application names no base branch.
func (w *workspace) waits() []ProcessWait {
	out := []ProcessWait{}
	add := func(process, why string) {
		if why != "" {
			out = append(out, ProcessWait{Process: process, Why: why})
		}
	}
	whyOf := func(err error) string {
		if err != nil {
			return err.Error()
		}
		return ""
	}
	pipeline := orchestrator.LoadPipelineSettings(w.root)
	add("The dispatcher", pipeline.DispatcherWaits())
	add("The merger", pipeline.MergerWaits())
	add("The cycle", whyOf(cycle.RequireUsable(w.root, settings.CycleFiles...)))
	add("The visual session", whyOf(cycle.RequireUsable(w.root, settings.VisualSessionFiles...)))
	add("The daemon, when it starts", settings.Unusable(w.root, settings.DaemonFiles...))
	return out
}

// SetSetting changes one setting the way `foreman-kit config set` does: the value is checked
// against what the catalog says the key accepts, only the line of the key is rewritten, and a change
// that would leave the file with a problem it did not have is refused. With dryRun nothing is
// written. It answers with what the kit printed: what changed, or what would.
func (w *workspace) SetSetting(key, value string, dryRun bool) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errInvalid("key names the setting to change, as the page of settings lists it: it is empty")
	}
	w.files.Lock()
	defer w.files.Unlock()
	var out strings.Builder
	if err := config.Set(w.root, key, value, dryRun, &out); err != nil {
		// A change the kit does not make is what was asked for: the key is not a setting, or the
		// value is not one it accepts. Anything else is a failure to read or to write the file.
		var refusal *config.Refusal
		if errors.As(err, &refusal) {
			return "", errInvalid("%s", refusal.Reason)
		}
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// Recommendations asks the kit which settings its records argue for changing over the stretch called
// window ("" for the one it evaluates by default). A stretch the kit does not evaluate is refused.
func (w *workspace) Recommendations(ctx context.Context, window string) (cycle.Recommendations, error) {
	window = strings.TrimSpace(window)
	if window != "" {
		// The kit says which stretches it evaluates: one it does not list is what was asked for, and
		// whatever fails after that is a failure to read its records.
		var names []string
		known := false
		for _, stretch := range cycle.RecommendWindows() {
			names = append(names, stretch.Name)
			known = known || stretch.Name == window
		}
		if !known {
			return cycle.Recommendations{}, errInvalid("window is a stretch the kit evaluates (%s), and %q is not one", strings.Join(names, ", "), window)
		}
	}
	return w.kit.Recommend(ctx, w.root, window)
}
