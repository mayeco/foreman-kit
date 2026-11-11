package settings

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/store"
)

// Reader reads the settings of one application. It reads each file once, the first time one of its
// keys is asked for, so the values it returns belong to one moment: a process that wants to follow
// an edit of the files asks for a new Reader.
//
// A value comes from the file of its key in the application root, or from `foreman-kit --set` when
// the run was given one for the key (RunOverridesEnv). Nothing else changes what a key holds: no
// environment variable overrides one. When the root is a worktree, the same file of the main
// checkout is read in its place in two cases: the worktree does not have the file, or it was cut
// before `foreman-kit update` rewrote the files and still holds the ones an older foreman-kit
// read (FilesRoot). Every key is written in its file: one that is missing, or holds something
// the setting does not accept, is a problem (Problems lists them, and `foreman-kit config check`
// fails on them). A caller that reads such a key anyway gets the value the kit ships for it, with
// one line on Stderr that names the key.
type Reader struct {
	// Root is the application root ("" for the one the process runs in).
	Root string
	// FilesAlone makes the Reader return what the files hold and nothing a run was given with
	// `--set`: what `foreman-kit config check` judges, and what `foreman-kit update` rewrites.
	FilesAlone bool
	// Stderr receives the notes about keys that could not be used (os.Stderr when nil).
	Stderr io.Writer

	files map[string]*fileData
	said  map[string]bool
	// from is the checkout the files are read from (FilesRoot), found once.
	from string
	// overrides are the values `--set` gave this run, read once.
	overrides     map[Key]string
	overridesRead bool
}

// fileData is one configuration file as read.
type fileData struct {
	data    map[string]any
	present bool
	err     error
}

// At returns a Reader of the application at root.
func At(root string) *Reader {
	return &Reader{Root: root}
}

// WrittenAt returns a Reader of the application at root that reads what its files hold, and nothing
// a run was given with `--set`.
func WrittenAt(root string) *Reader {
	return &Reader{Root: root, FilesAlone: true}
}

// Use makes the Reader take the contents of the file called name from data instead of reading it
// (a file the caller already holds, or one that only exists in a test). It returns the Reader.
func (r *Reader) Use(name string, data map[string]any) *Reader {
	if r.files == nil {
		r.files = make(map[string]*fileData)
	}
	if data == nil {
		data = map[string]any{}
	}
	r.files[name] = &fileData{data: data, present: true}
	return r
}

func (r *Reader) root() string {
	if r.Root == "" {
		return store.AppRoot("")
	}
	return r.Root
}

// saidToProcess remembers what was already written to the process's own stderr, so a key that is
// read on every pass of a long-running process is reported once.
var saidToProcess sync.Map

// say writes one note about a key or a file that cannot be used, once.
func (r *Reader) say(note string) {
	if r.said == nil {
		r.said = make(map[string]bool)
	}
	if r.said[note] {
		return
	}
	r.said[note] = true
	if r.Stderr == nil {
		if _, again := saidToProcess.LoadOrStore(r.root()+"\x00"+note, true); again {
			return
		}
		fmt.Fprintln(os.Stderr, "settings: "+note)
		return
	}
	fmt.Fprintln(r.Stderr, "settings: "+note)
}

// load reads the file called name, once.
func (r *Reader) load(name string) *fileData {
	if fd, ok := r.files[name]; ok {
		return fd
	}
	if r.files == nil {
		r.files = make(map[string]*fileData)
	}
	fd := &fileData{data: map[string]any{}}
	r.files[name] = fd

	f, ok := FileNamed(name)
	if !ok {
		return fd
	}
	if r.from == "" {
		r.from = FilesRoot(r.root())
	}
	root := r.from
	p := filepath.Join(root, filepath.FromSlash(f.Path))
	if !isFile(p) {
		p = filepath.Join(store.MainRoot(root), filepath.FromSlash(f.Path))
	}
	if !isFile(p) {
		return fd
	}
	fd.present = true
	raw, err := os.ReadFile(p)
	if err != nil {
		fd.err = err
		return fd
	}
	var decoded any
	if err := yaml.Unmarshal(raw, &decoded); err != nil {
		fd.err = fmt.Errorf("not valid YAML: %s", firstLine(err.Error()))
		return fd
	}
	switch m := decoded.(type) {
	case nil:
	case map[string]any:
		fd.data = m
	default:
		fd.err = fmt.Errorf("must be a mapping")
	}
	return fd
}

// Data returns the contents of the file called name as a mapping (empty when the file is missing or
// cannot be read), whether the file exists, and why it could not be read.
func (r *Reader) Data(name string) (data map[string]any, present bool, err error) {
	fd := r.load(name)
	return fd.data, fd.present, fd.err
}

// Raw returns the value of key as its file holds it, and whether the file writes it. What `--set`
// gave the run is not consulted.
func (r *Reader) Raw(key Key) (any, bool) {
	fd := r.load(key.File())
	raw := dig(fd.data, key.Parts()...)
	return raw, raw != nil
}

// Lookup returns the value in effect for key before it is validated: the one `--set` gave this
// run, else what its file holds. source names where it came from, for a message ("--set
// pipeline.dispatch.workers", or "dispatch.workers in .foreman-kit/pipeline.yaml"), and set says whether
// anything sets the key at all.
func (r *Reader) Lookup(key Key) (raw any, source string, set bool) {
	if raw, given := r.override(key); given {
		return raw, "--set " + string(key), true
	}
	raw, set = r.Raw(key)
	return raw, key.Where(), set
}

// Value returns the value in effect for key, with the type Setting.Parse gives it. A key that is
// missing, or set to something the setting does not accept, is reported once on Stderr and read as
// the value the kit ships for it (nil when it ships none).
func (r *Reader) Value(key Key) any {
	s := Of(key)
	raw, source, set := r.Lookup(key)
	if set {
		if v, ok := s.Parse(raw); ok {
			return v
		}
		r.say(fmt.Sprintf("%s must be %s, not %q%s", source, s.Expected(), fmt.Sprint(raw), standIn(s)))
		return fallback(s)
	}
	fd := r.load(key.File())
	switch {
	case fd.err != nil:
		r.say(fmt.Sprintf("%s is %s: none of its keys can be read", key.FilePath(), fd.err))
	case !fd.present:
		r.say(fmt.Sprintf("%s is missing: none of its keys can be read (foreman-kit update writes it)", key.FilePath()))
	default:
		r.say(fmt.Sprintf("%s is missing%s", key.Where(), standIn(s)))
	}
	return fallback(s)
}

// standIn says what a caller reads in place of a key that cannot be used.
func standIn(s Setting) string {
	if s.Default == nil {
		return "; it has no value"
	}
	return fmt.Sprintf("; reading it as %v", s.Default)
}

// fallback returns the value the kit ships for s, with the type Parse gives a value of its kind.
func fallback(s Setting) any {
	if s.Default == nil {
		return nil
	}
	if v, ok := s.Parse(s.Default); ok {
		return v
	}
	return s.Default
}

// Int returns the value of a setting that holds a whole number (0 when it is "never").
func (r *Reader) Int(key Key) int {
	n, _ := wholeNumber(r.Value(key))
	return n
}

// String returns the value of a setting that holds one word or a text ("" when it has none).
func (r *Reader) String(key Key) string {
	v := r.Value(key)
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// Bool returns the value of a setting that is a switch.
func (r *Reader) Bool(key Key) bool {
	b, _ := r.Value(key).(bool)
	return b
}

// Duration returns the value of a setting that holds a time (0 when it is "never", or 0s).
func (r *Reader) Duration(key Key) time.Duration {
	d, _ := r.Value(key).(time.Duration)
	return d
}

// Text returns the value in effect for key written the way its file holds it: a time with its unit
// (or never), a slot as its number (or none), anything else as it reads.
func (r *Reader) Text(key Key) string {
	return Of(key).written(r.Value(key))
}

// written writes v, a value Parse returned for the setting, the way its file holds it.
func (s Setting) written(v any) string {
	switch s.Kind {
	case KindDuration:
		d, _ := v.(time.Duration)
		if d == 0 && s.Never {
			return Never
		}
		return FormatDuration(d)
	case KindSlot:
		if n, _ := v.(int); n > 0 {
			return fmt.Sprint(n)
		}
		return SlotNone
	case KindInt:
		if n, _ := v.(int); n == 0 && s.Never {
			return Never
		}
	}
	return Inline(v)
}

// Seconds returns the value of a setting that holds a time, in whole seconds.
func (r *Reader) Seconds(key Key) int {
	return int(r.Duration(key) / time.Second)
}

// Slot returns the value of a setting that holds a slot number (0 when it is "none").
func (r *Reader) Slot(key Key) int {
	n, _ := r.Value(key).(int)
	return n
}

// Strings returns the value of a setting that holds a list of words.
func (r *Reader) Strings(key Key) []string {
	var out []string
	switch list := r.Value(key).(type) {
	case []string:
		out = append(out, list...)
	case []any:
		for _, item := range list {
			if s := strings.TrimSpace(fmt.Sprint(item)); item != nil && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// Described is one setting as it stands in an application: where it lives, what it decides and the
// value its file holds. It is what `foreman-kit config show` prints and what a page or a message
// uses to name a setting without writing its key.
type Described struct {
	Key  Key    `json:"key"`
	File string `json:"file"`
	// Value is what the file holds (nil when the key is missing).
	Value any `json:"value"`
	// Default is the value the kit ships for the key (nil when it ships none).
	Default any `json:"default"`
	// Set says the file writes the key.
	Set bool `json:"set"`
	// Problem says why the key cannot be used as it stands ("" when it can).
	Problem string `json:"problem,omitempty"`
	// Values lists what an enum accepts.
	Values []string `json:"values,omitempty"`
	Means  string   `json:"means"`
	ReadBy string   `json:"read_by"`
	// Change says how to change it when `foreman-kit config set` cannot.
	Change string `json:"change,omitempty"`
	// OverriddenBy names what replaces the value of the file for this run, with its value: the
	// `--set` the run was started with ("--set pipeline.dispatch.workers=4").
	OverriddenBy string `json:"overridden_by,omitempty"`
	// Kind is what the setting accepts, and Accepts the same in words ("a time with its unit (30s,
	// 15m, 2h), or never").
	Kind    Kind   `json:"kind"`
	Accepts string `json:"accepts"`
	// InEffect is the value this run reads, written the way the file holds it (20m, never, [a, b]):
	// the one `--set` gave it, else what the file holds.
	// It is "" when nothing usable sets the key (Problem says why), and for what a file does not
	// write on one line (the commands, the strategies): Value holds it.
	InEffect string `json:"in_effect"`
}

// Describe returns key as it stands in the application of the Reader.
func (r *Reader) Describe(key Key) Described {
	s := Of(key)
	raw, set := r.Raw(key)
	d := Described{
		Key:     key,
		File:    key.FilePath(),
		Value:   raw,
		Default: s.Shipped(),
		Set:     set,
		Values:  s.Values,
		Means:   s.Explained(),
		ReadBy:  s.ReadBy,
		Change:  s.Change,
		Kind:    s.Kind,
		Accepts: s.Expected(),
	}
	if !set {
		d.Problem = "missing"
	} else {
		d.Problem = s.Problem(raw)
	}
	if text, given := r.overrideText(key); given {
		d.OverriddenBy = "--set " + string(key) + "=" + text
	}
	if inEffect, _, isSet := r.Lookup(key); isSet {
		if v, ok := s.Parse(inEffect); ok && onOneLine(v) {
			d.InEffect = s.written(v)
		}
	}
	return d
}

// onOneLine reports whether v is written on one line of a file: one value, or a list of them.
func onOneLine(v any) bool {
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

// Explained returns what the setting decides.
func (s Setting) Explained() string {
	return s.Means
}

// Problem is one thing a configuration file holds, or lacks, that keeps it from being used.
type Problem struct {
	// File is the relative path of the file (".foreman-kit/pipeline.yaml").
	File string
	// Key is the key the problem is about ("" when it is about the whole file).
	Key Key
	// Text says what is wrong, starting with the path of the key inside its file.
	Text string
	// Former says the key is one an older foreman-kit read: the file was written for it, and
	// `foreman-kit update` rewrites it.
	Former bool
}

func (p Problem) String() string {
	return p.File + ": " + p.Text
}

// Problems lists what keeps the file called name from being used: the file is missing or cannot be
// read, a key of the catalog is missing or holds something it does not accept, or the file writes a
// key the catalog does not know (one an older foreman-kit read, or a typo). What `--set` gave the
// run is not consulted: the problems are those of the file.
func (r *Reader) Problems(name string) []Problem {
	f, ok := FileNamed(name)
	if !ok {
		return nil
	}
	fd := r.load(name)
	if fd.err != nil {
		return []Problem{{File: f.Path, Text: fd.err.Error()}}
	}
	if !fd.present {
		return []Problem{{File: f.Path, Text: "missing (foreman-kit update writes it)"}}
	}

	var out []Problem
	known := map[string]bool{}
	for _, s := range In(name) {
		known[string(s.Key)] = true
		raw := dig(fd.data, s.Key.Parts()...)
		if raw == nil {
			text := s.Key.Path() + ": missing"
			if shipped := s.Shipped(); shipped != nil {
				text += fmt.Sprintf(" (the kit ships %s)", Inline(shipped))
			}
			out = append(out, Problem{File: f.Path, Key: s.Key, Text: text})
			continue
		}
		if problem := s.Problem(raw); problem != "" {
			out = append(out, Problem{File: f.Path, Key: s.Key, Text: fmt.Sprintf("%s: %s (is %s)", s.Key.Path(), problem, Inline(raw))})
		}
	}

	reported := map[string]bool{}
	for _, leaf := range leaves(fd.data, name) {
		key := Key(leaf)
		if known[leaf] || IsContent(key) || insideKnown(leaf, known) {
			continue
		}
		// A section written with nothing in it: the keys it lacks are reported one by one.
		_, isSection := dig(fd.data, key.Parts()...).(map[string]any)
		if isSection && holdsKnown(leaf, known) {
			continue
		}
		// A former key that held a mapping is reported once, not once per entry.
		if former, at := formerAt(key); at != "" {
			if !reported[at] {
				reported[at] = true
				out = append(out, Problem{File: f.Path, Key: Key(at), Text: Key(at).Path() + ": " + former.NoLongerRead(), Former: true})
			}
			continue
		}
		if holdsKnown(leaf, known) {
			out = append(out, Problem{File: f.Path, Key: key, Text: fmt.Sprintf("%s: must be a section with its own keys (is %s)", key.Path(), Inline(dig(fd.data, key.Parts()...)))})
			continue
		}
		out = append(out, Problem{File: f.Path, Key: key, Text: key.Path() + ": not a setting of this file (a typo?)"})
	}
	return out
}

// insideKnown reports whether leaf is something a key of the catalog holds: an entry of a list or
// of a mapping (the command of an agent), which is its key's.
func insideKnown(leaf string, known map[string]bool) bool {
	for k := range known {
		if strings.HasPrefix(leaf, k+".") {
			return true
		}
	}
	return false
}

// holdsKnown reports whether leaf is a section keys of the catalog live in.
func holdsKnown(leaf string, known map[string]bool) bool {
	for k := range known {
		if strings.HasPrefix(k, leaf+".") {
			return true
		}
	}
	return false
}

// leaves returns every path of data that ends in a value: a scalar, a list, or an empty mapping.
func leaves(data any, prefix string) []string {
	m, ok := data.(map[string]any)
	if !ok || len(m) == 0 {
		return []string{prefix}
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []string
	for _, k := range names {
		out = append(out, leaves(m[k], prefix+"."+k)...)
	}
	return out
}

// Inline writes a value on one line, the way a file would hold it.
func Inline(v any) string {
	switch val := v.(type) {
	case nil:
		return "nothing"
	case string:
		if val == "" {
			return `""`
		}
		return val
	case []string:
		return "[" + strings.Join(val, ", ") + "]"
	case []float64:
		parts := make([]string, len(val))
		for i, f := range val {
			parts[i] = fmt.Sprint(f)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []any:
		parts := make([]string, len(val))
		for i, item := range val {
			parts[i] = Inline(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		if len(val) == 0 {
			return "{}"
		}
		names := make([]string, 0, len(val))
		for k := range val {
			names = append(names, k)
		}
		sort.Strings(names)
		parts := make([]string, len(names))
		for i, k := range names {
			parts[i] = k + ": " + Inline(val[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// Describe returns key as it stands in the application at root.
func Describe(root string, key Key) Described { return At(root).Describe(key) }

// Value returns the value in effect for key in the application at root.
func Value(root string, key Key) any { return At(root).Value(key) }

// Int returns the value of a setting of the application at root that holds a whole number.
func Int(root string, key Key) int { return At(root).Int(key) }

// String returns the value of a setting of the application at root that holds one word or a text.
func String(root string, key Key) string { return At(root).String(key) }

// Bool returns the value of a setting of the application at root that is a switch.
func Bool(root string, key Key) bool { return At(root).Bool(key) }

// Duration returns the value of a setting of the application at root that holds a time.
func Duration(root string, key Key) time.Duration { return At(root).Duration(key) }

// Strings returns the value of a setting of the application at root that holds a list of words.
func Strings(root string, key Key) []string { return At(root).Strings(key) }

func dig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mp, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mp[k]
	}
	return cur
}

func firstLine(text string) string {
	return strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// WorktreesBase returns the directory the worktrees of the application at root live in
// (repository.worktrees_dir): as it is written when absolute, else under root.
func WorktreesBase(root string) string {
	dir := At(root).String(WorktreesDir)
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(root, filepath.FromSlash(dir))
}

// RemoteName returns the git remote the application at root fetches from and pushes to
// (repository.remote).
func RemoteName(root string) string {
	return At(root).String(Remote)
}
