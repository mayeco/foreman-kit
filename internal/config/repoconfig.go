package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/store"
)

// RepoConfigFile is the file a child repository describes itself in, at its root: its base branch,
// how it runs, what its gates need and the commands that check a change to it.
const RepoConfigFile = "repo.config.yaml"

// RepoFormer is a key of repo.config.yaml an older foreman-kit read, with the key that holds its
// value now. None is read under its former name: `foreman-kit update` rewrites the file, and
// `foreman-kit config check` names each one until it has.
type RepoFormer struct {
	Was, Is string
	// Why says what the new name tells that the former one hid.
	Why string
}

var repoFormers = []RepoFormer{
	{"gates.setup", "setup", "it is not a gate: it prepares the environment before the gates and before the regression"},
	{"service.prepare", "visual_qa.prepare", "only the visual session runs it"},
	{"requires", "required_services", "they are services that must answer before anything starts"},
	{"regression", "regression.commands", "an empty regression runs nothing, instead of the gates"},
	{"regression_when", "regression.when", "it sits beside the commands it triggers"},
}

// RepoFormers returns the keys of repo.config.yaml an older foreman-kit read.
func RepoFormers() []RepoFormer {
	return append([]RepoFormer(nil), repoFormers...)
}

// RepoFormerKeys returns the keys data, a repo.config.yaml as read, writes the way an older
// foreman-kit read them. A file that writes none is one of today.
func RepoFormerKeys(data map[string]any) []RepoFormer {
	var out []RepoFormer
	for _, f := range repoFormers {
		if repoWritesFormer(data, f.Was) {
			out = append(out, f)
		}
	}
	return out
}

func repoWritesFormer(data map[string]any, was string) bool {
	switch was {
	case "gates.setup":
		gates, _ := data["gates"].(map[string]any)
		_, ok := gates["setup"]
		return ok
	case "service.prepare":
		service, _ := data["service"].(map[string]any)
		_, ok := service["prepare"]
		return ok
	case "regression":
		// The commands sat directly under the key, each a name and its command. Today the key holds
		// two sections: commands, a mapping, and when, a list.
		regression, _ := data["regression"].(map[string]any)
		for name, value := range regression {
			switch name {
			case "commands":
				if _, isMapping := value.(map[string]any); isMapping || value == nil {
					continue
				}
			case "when":
				if _, isList := value.([]any); isList || value == nil {
					continue
				}
			}
			return true
		}
		return false
	}
	_, ok := data[was]
	return ok
}

// RepoOfAnOlderKit says that data, a repo.config.yaml as read, holds keys an older foreman-kit
// read, naming them, or "" when it holds none.
func RepoOfAnOlderKit(data map[string]any) string {
	former := RepoFormerKeys(data)
	if len(former) == 0 {
		return ""
	}
	names := make([]string, len(former))
	for i, f := range former {
		names[i] = f.Was
	}
	return fmt.Sprintf("holds keys an older foreman-kit read (%s): foreman-kit update rewrites it", strings.Join(names, ", "))
}

// ReadRepoConfig returns the repo.config.yaml of the child repository checked out at childDir, and
// the path of the file it was read from.
//
// A checkout cut before `foreman-kit update` renamed the keys of the file still holds the ones an
// older foreman-kit read: the worktree of a change that was being built, or any checkout of the
// repository until the rewritten file is pushed to its base branch. None of those keys is read. The
// file of the same repository in the main checkout is read in its place, where update rewrote it;
// when that one is of an older kit too, the error says so and nothing runs as the file says.
//
// mainRoot is the main checkout of the umbrella ("" to find it from childDir).
func ReadRepoConfig(mainRoot, childDir string) (raw []byte, path string, err error) {
	path = filepath.Join(childDir, RepoConfigFile)
	raw, err = os.ReadFile(path)
	if err != nil {
		return nil, path, err
	}
	return RepoConfigOfToday(mainRoot, childDir, raw, path)
}

// RepoConfigOfToday returns raw, the repo.config.yaml of the child repository checked out at
// childDir as from holds it (a file, or a commit of the repository), when it is written with the
// keys of today. When it holds keys an older foreman-kit read, the file of the same repository in
// the main checkout is returned in its place, as ReadRepoConfig does.
func RepoConfigOfToday(mainRoot, childDir string, raw []byte, from string) ([]byte, string, error) {
	var data map[string]any
	if yaml.Unmarshal(raw, &data) != nil {
		// What is not valid YAML is for the caller to say, in its own words.
		return raw, from, nil
	}
	older := RepoOfAnOlderKit(data)
	if older == "" {
		return raw, from, nil
	}
	if inMain := repoConfigInMain(mainRoot, childDir); inMain != "" && inMain != from {
		if mainRaw, readErr := os.ReadFile(inMain); readErr == nil {
			var mainData map[string]any
			if yaml.Unmarshal(mainRaw, &mainData) == nil && RepoOfAnOlderKit(mainData) == "" {
				return mainRaw, inMain, nil
			}
		}
	}
	return nil, from, fmt.Errorf("%s %s", RepoConfigFile, older)
}

// repoConfigInMain returns the path of the repo.config.yaml the main checkout holds for the child
// repository checked out at childDir, repos/<name> of a checkout or a worktree of the umbrella, or
// "" when childDir is not one.
func repoConfigInMain(mainRoot, childDir string) string {
	abs, err := filepath.Abs(childDir)
	if err != nil || filepath.Base(filepath.Dir(abs)) != "repos" {
		return ""
	}
	if mainRoot == "" {
		mainRoot = store.MainRoot(filepath.Dir(filepath.Dir(abs)))
	}
	return filepath.Join(mainRoot, "repos", filepath.Base(abs), RepoConfigFile)
}

// repoConfigPaths returns the repo.config.yaml of every child repository checked out in the main
// checkout at root, sorted.
func repoConfigPaths(root string) []string {
	matches, _ := filepath.Glob(filepath.Join(root, "repos", "*", RepoConfigFile))
	sort.Strings(matches)
	return matches
}

// RepoConfigsUnusable says why no new work starts in the application at root because of the
// repo.config.yaml of one of its repositories ("" when nothing does): the file still holds keys an
// older foreman-kit read, so its gates would not run as it says.
func RepoConfigsUnusable(root string) string {
	for _, path := range repoConfigPaths(root) {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var data map[string]any
		if yaml.Unmarshal(raw, &data) != nil {
			continue
		}
		if older := RepoOfAnOlderKit(data); older != "" {
			rel, _ := filepath.Rel(root, path)
			return filepath.ToSlash(rel) + " " + older
		}
	}
	return ""
}

// RepoMigration is what `foreman-kit update` writes in the repositories of an application whose
// repo.config.yaml files hold keys an older foreman-kit read.
type RepoMigration struct {
	// Files maps the path of each file, relative to the application root, to its new contents.
	Files map[string][]byte
	// Repos lists, sorted, the repositories whose file is rewritten, relative to the root.
	Repos []string
	// Notes says, file by file, where each value went and what a person has to review.
	Notes []string
}

// Pending reports whether there is a file to write or something to tell.
func (m *RepoMigration) Pending() bool {
	return m != nil && (len(m.Files) > 0 || len(m.Notes) > 0)
}

// RepoConfigsNeedRewrite reports whether a repository of the application at root has a
// repo.config.yaml with keys an older foreman-kit read.
func RepoConfigsNeedRewrite(root string) bool {
	return RepoConfigsUnusable(root) != ""
}

// MigrateRepoConfigs returns the repo.config.yaml of each repository checked out in the application
// at root rewritten with the keys of today. It writes nothing: the caller does. A file it cannot
// rewrite is left as it is, with a note that says which keys to rename by hand.
func MigrateRepoConfigs(root string) *RepoMigration {
	m := &RepoMigration{Files: map[string][]byte{}}
	for _, path := range repoConfigPaths(root) {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		out, notes, err := MigrateRepoConfig(raw)
		switch {
		case err != nil:
			var renames []string
			var data map[string]any
			_ = yaml.Unmarshal(raw, &data)
			for _, f := range RepoFormerKeys(data) {
				renames = append(renames, f.Was+" is "+f.Is)
			}
			m.Notes = append(m.Notes, fmt.Sprintf("  REVIEW: %s could not be rewritten (%v); rename its keys by hand: %s", rel, err, strings.Join(renames, ", ")))
		case string(out) != string(raw):
			m.Files[rel] = out
			m.Repos = append(m.Repos, filepath.ToSlash(filepath.Dir(rel)))
			m.Notes = append(m.Notes, fmt.Sprintf("  rewrote %s with the keys of this foreman-kit", rel))
			for _, note := range notes {
				m.Notes = append(m.Notes, "    "+note)
			}
		}
	}
	return m
}

// MigrateRepoConfig returns raw, a repo.config.yaml, with each key an older foreman-kit read moved
// to the key that holds it now, and what a person should know about the move. The lines that do
// not move stay as they were, comments included. A file of today is returned as it is.
//
// The regression of a repository that had no command of its own ran its gates. Nothing stands in
// for a missing value any more, so the gates that ran are written as its regression commands: the
// repository goes on checking what it checked.
func MigrateRepoConfig(raw []byte) ([]byte, []string, error) {
	var old map[string]any
	if err := yaml.Unmarshal(raw, &old); err != nil {
		return nil, nil, fmt.Errorf("it is not valid YAML: %s", firstLineOf(err.Error()))
	}
	former := map[string]bool{}
	for _, f := range RepoFormerKeys(old) {
		former[f.Was] = true
	}
	if len(former) == 0 {
		return raw, nil, nil
	}
	// A value written under both names is for a person to choose between.
	for _, both := range []struct {
		was     string
		written bool
		is      string
	}{
		{"requires", old["required_services"] != nil, "required_services"},
		{"gates.setup", old["setup"] != nil, "setup"},
		{"service.prepare", old["visual_qa"] != nil, "visual_qa"},
	} {
		if former[both.was] && both.written {
			return nil, nil, fmt.Errorf("%s and %s are both written", both.was, both.is)
		}
	}

	lines := splitKeepEOL(string(raw))
	if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
		lines[n-1] += "\n"
	}
	var notes []string
	var err error

	if former["requires"] {
		at, found := repoKeyLine(lines, 0, len(lines), 0, "requires")
		if !found {
			return nil, nil, fmt.Errorf("requires is not written on a line of its own")
		}
		lines[at] = "required_services:" + strings.TrimPrefix(lines[at], "requires:")
		notes = append(notes, "required_services: was requires")
	}
	if former["gates.setup"] {
		if lines, err = repoMoveSetup(lines); err != nil {
			return nil, nil, err
		}
		notes = append(notes, "setup: was gates.setup")
	}
	if former["service.prepare"] {
		if lines, err = repoMovePrepare(lines); err != nil {
			return nil, nil, err
		}
		notes = append(notes, "visual_qa.prepare: was service.prepare")
	}
	lines, regressionNotes, err := repoMoveRegression(lines, raw, old)
	if err != nil {
		return nil, nil, err
	}
	notes = append(notes, regressionNotes...)

	out := []byte(strings.Join(repoCommentsOfToday(lines), ""))
	if err := repoMigrationKeptEveryValue(old, out); err != nil {
		return nil, nil, err
	}
	return out, notes, nil
}

// repoFormerComments are the sentences the kit wrote in the comments of a repo.config.yaml that the
// move makes false, with what each says now. A comment a person wrote is theirs and stays.
var repoFormerComments = []struct{ was, is string }{
	{"after gates.setup:", "after setup:"},
	{"Empty: the gates.", "Empty: the sweep runs nothing in this repository."},
	{"repo has changes: setup, lint,", "repo has changes, after setup: lint,"},
}

func repoCommentsOfToday(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		if repoComment(line) {
			for _, c := range repoFormerComments {
				line = strings.ReplaceAll(line, c.was, c.is)
			}
		}
		out[i] = line
	}
	return out
}

// ---- the lines of a YAML file -------------------------------------------------------------------

func repoIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

func repoBlank(line string) bool { return strings.TrimSpace(line) == "" }

func repoComment(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "#") }

// repoListItem reports whether line writes an item of a list.
func repoListItem(line string) bool {
	item := strings.TrimSpace(line)
	return item == "-" || strings.HasPrefix(item, "- ")
}

// repoKeyLine returns the line of lines[from:to] that writes key at indent.
func repoKeyLine(lines []string, from, to, indent int, key string) (int, bool) {
	prefix := strings.Repeat(" ", indent) + key + ":"
	for i := from; i < to; i++ {
		if strings.HasPrefix(lines[i], prefix) {
			return i, true
		}
	}
	return 0, false
}

// repoEntry is an entry of a mapping: its comment lines just above it, the line of its key and the
// lines of its value, lines[head:end], with the key on lines[key].
type repoEntry struct {
	head, key, end int
	indent         int
}

// repoEntryAt returns the entry of lines[from:to] whose key is written at indent. The blank lines
// and the comments that follow its value belong to the entry after it.
func repoEntryAt(lines []string, from, to, indent int, key string) (repoEntry, bool) {
	at, found := repoKeyLine(lines, from, to, indent, key)
	if !found {
		return repoEntry{}, false
	}
	end := to
	for i := at + 1; i < to; i++ {
		if repoBlank(lines[i]) || repoComment(lines[i]) {
			continue
		}
		// A list may be written at the indent of its key: its items are still the value of the key.
		if in := repoIndent(lines[i]); in < indent || (in == indent && !repoListItem(lines[i])) {
			end = i
			break
		}
	}
	for end > at+1 && (repoBlank(lines[end-1]) || (repoComment(lines[end-1]) && repoIndent(lines[end-1]) <= indent)) {
		end--
	}
	head := at
	for head > from && repoComment(lines[head-1]) && repoIndent(lines[head-1]) == indent {
		head--
	}
	return repoEntry{head: head, key: at, end: end, indent: indent}, true
}

// repoInline returns what the line of a key holds after the key: its value when it is written on
// that line, with its comment, or only white space and a comment when the value is on the lines
// below. block says which.
func repoInline(line, key string) (rest string, block bool) {
	rest = strings.TrimPrefix(strings.TrimLeft(line, " "), key+":")
	trimmed := strings.TrimSpace(rest)
	return rest, trimmed == "" || strings.HasPrefix(trimmed, "#")
}

// repoChildIndent returns the indent of the entries of the mapping whose key is on lines[e.key].
func repoChildIndent(lines []string, e repoEntry) (int, bool) {
	for i := e.key + 1; i < e.end; i++ {
		if repoBlank(lines[i]) || repoComment(lines[i]) {
			continue
		}
		if in := repoIndent(lines[i]); in > e.indent {
			return in, true
		}
		break
	}
	return 0, false
}

// repoShift returns lines with delta spaces added to (or, when negative, taken from) the start of
// each one that is not blank.
func repoShift(lines []string, delta int) ([]string, error) {
	out := make([]string, len(lines))
	for i, line := range lines {
		switch {
		case repoBlank(line):
			out[i] = line
		case delta >= 0:
			out[i] = strings.Repeat(" ", delta) + line
		default:
			if repoIndent(line) < -delta {
				return nil, fmt.Errorf("a line is indented less than the key it belongs to: %q", strings.TrimRight(line, "\n"))
			}
			out[i] = line[-delta:]
		}
	}
	return out, nil
}

func repoSplice(lines []string, from, to int, with []string) []string {
	out := make([]string, 0, len(lines)-(to-from)+len(with))
	out = append(out, lines[:from]...)
	out = append(out, with...)
	return append(out, lines[to:]...)
}

// repoTakeChild takes the entry called key out of the mapping parent holds, and returns the lines
// without it and the lines of the entry. A mapping left with no entry is written as empty.
func repoTakeChild(lines []string, parent, key string) (rest []string, taken []string, childIndent int, err error) {
	p, found := repoEntryAt(lines, 0, len(lines), 0, parent)
	if !found {
		return nil, nil, 0, fmt.Errorf("%s is not written on a line of its own", parent)
	}
	if _, block := repoInline(lines[p.key], parent); !block {
		return nil, nil, 0, fmt.Errorf("%s is written on one line", parent)
	}
	childIndent, ok := repoChildIndent(lines, p)
	if !ok {
		return nil, nil, 0, fmt.Errorf("%s has no entries", parent)
	}
	c, found := repoEntryAt(lines, p.key+1, p.end, childIndent, key)
	if !found {
		return nil, nil, 0, fmt.Errorf("%s.%s is not written on a line of its own", parent, key)
	}
	taken = append([]string(nil), lines[c.head:c.end]...)
	rest = repoSplice(lines, c.head, c.end, nil)
	// The mapping without that entry: when none is left, it is written as empty.
	p, _ = repoEntryAt(rest, 0, len(rest), 0, parent)
	if _, has := repoChildIndent(rest, p); !has {
		rest[p.key] = parent + ": {}\n"
	}
	return rest, taken, childIndent, nil
}

// repoMoveSetup moves gates.setup to the top-level key setup, written just above the gates and
// their comment.
func repoMoveSetup(lines []string) ([]string, error) {
	rest, taken, childIndent, err := repoTakeChild(lines, "gates", "setup")
	if err != nil {
		return nil, err
	}
	moved, err := repoShift(taken, -childIndent)
	if err != nil {
		return nil, err
	}
	gates, _ := repoEntryAt(rest, 0, len(rest), 0, "gates")
	return repoSplice(rest, gates.head, gates.head, append(moved, "\n")), nil
}

// repoMovePrepare moves service.prepare to visual_qa.prepare, a section written just below service.
func repoMovePrepare(lines []string) ([]string, error) {
	rest, taken, childIndent, err := repoTakeChild(lines, "service", "prepare")
	if err != nil {
		return nil, err
	}
	// The comment of the entry goes above the new section, and the entry inside it.
	comments := 0
	for comments < len(taken) && repoComment(taken[comments]) {
		comments++
	}
	head, err := repoShift(taken[:comments], -childIndent)
	if err != nil {
		return nil, err
	}
	body, err := repoShift(taken[comments:], 2-childIndent)
	if err != nil {
		return nil, err
	}
	section := append([]string{"\n"}, head...)
	section = append(section, "visual_qa:\n")
	section = append(section, body...)
	service, _ := repoEntryAt(rest, 0, len(rest), 0, "service")
	return repoSplice(rest, service.end, service.end, section), nil
}

// repoUsableCommand reports whether a command of a repository runs: it is written and it is not a
// {{...}} placeholder nobody filled in.
func repoUsableCommand(v any) bool {
	text, ok := v.(string)
	return ok && strings.TrimSpace(text) != "" && !strings.Contains(text, "{{")
}

// repoGatesThatRun returns the gates of data, a repo.config.yaml as read, whose command runs.
func repoGatesThatRun(data map[string]any) map[string]any {
	gates, _ := data["gates"].(map[string]any)
	out := map[string]any{}
	for name, command := range gates {
		if name != "setup" && repoUsableCommand(command) {
			out[name] = command
		}
	}
	return out
}

// repoRegression says what the regression of a repo.config.yaml an older foreman-kit read holds
// once the file is rewritten.
type repoRegression int

const (
	// regressionOfToday: the section is already written the way it is read, and its lines stay.
	regressionOfToday repoRegression = iota
	// regressionOwn: it had commands of its own, which go under regression.commands.
	regressionOwn
	// regressionGates: it had no command that runs, so the sweep ran its gates: the ones that run
	// are written as its commands.
	regressionGates
	// regressionNone: it had no command and no gate that runs: it has no regression, as before.
	regressionNone
)

func repoRegressionOf(old map[string]any) repoRegression {
	held, _ := old["regression"].(map[string]any)
	if repoWritesFormer(old, "regression") {
		for _, command := range held {
			if repoUsableCommand(command) {
				return regressionOwn
			}
		}
	} else if len(held) > 0 {
		return regressionOfToday
	}
	if len(repoGatesThatRun(old)) > 0 {
		return regressionGates
	}
	return regressionNone
}

// repoKeysInOrder returns the names of the entries of the top-level mapping key of raw, in the
// order the file writes them.
func repoKeysInOrder(raw []byte, key string) []string {
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	top := doc.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value != key || top.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		var names []string
		for j := 0; j+1 < len(top.Content[i+1].Content); j += 2 {
			names = append(names, top.Content[i+1].Content[j].Value)
		}
		return names
	}
	return nil
}

// repoGatesAsCommands returns the lines that write, as regression.commands, the gates of raw whose
// command runs, in the order the file writes them.
func repoGatesAsCommands(raw []byte, old map[string]any) ([]string, error) {
	run := repoGatesThatRun(old)
	commands := &yaml.Node{Kind: yaml.MappingNode}
	for _, name := range repoKeysInOrder(raw, "gates") {
		command, runs := run[name].(string)
		if !runs {
			continue
		}
		commands.Content = append(commands.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: command})
	}
	if len(commands.Content) != 2*len(run) {
		return nil, fmt.Errorf("gates is not written as one mapping")
	}
	var written bytes.Buffer
	enc := yaml.NewEncoder(&written)
	enc.SetIndent(2)
	if err := enc.Encode(commands); err != nil {
		return nil, err
	}
	_ = enc.Close()
	body, _ := repoShift(splitKeepEOL(written.String()), 4)
	return append([]string{"  commands:\n"}, body...), nil
}

// repoCutRange widens lines[from:to], an entry that is taken out, by the blank line that taking it
// out would leave doubled, or first or last in the file.
func repoCutRange(lines []string, from, to int) (int, int) {
	blankBefore := from > 0 && repoBlank(lines[from-1])
	blankAfter := to < len(lines) && repoBlank(lines[to])
	switch {
	case from == 0 && blankAfter:
		return from, to + 1
	case blankBefore && (blankAfter || to == len(lines)):
		return from - 1, to
	}
	return from, to
}

// repoMoveRegression writes the regression section of today: the commands that sat directly under
// regression go under regression.commands, and regression_when becomes regression.when. The
// section is written where regression was; in a file that had none, where regression_when was; in
// a file that had neither, below the gates.
func repoMoveRegression(lines []string, raw []byte, old map[string]any) ([]string, []string, error) {
	held, _ := old["regression"].(map[string]any)
	if repoWritesFormer(old, "regression") {
		for name, value := range held {
			_, isMapping := value.(map[string]any)
			_, isList := value.([]any)
			if (name == "commands" && (isMapping || value == nil)) || (name == "when" && (isList || value == nil)) {
				return nil, nil, fmt.Errorf("regression holds commands beside regression.%s", name)
			}
		}
	}
	regression, hasRegression := repoEntryAt(lines, 0, len(lines), 0, "regression")
	whenEntry, hasWhen := repoEntryAt(lines, 0, len(lines), 0, "regression_when")
	if _, wrote := old["regression"]; wrote && !hasRegression {
		return nil, nil, fmt.Errorf("regression is not written on a line of its own")
	}
	if _, wrote := old["regression_when"]; wrote && !hasWhen {
		return nil, nil, fmt.Errorf("regression_when is not written on a line of its own")
	}

	// The line of the key, and whether its value is written on the lines below it.
	keyLine, block := "regression:\n", true
	if hasRegression {
		if _, block = repoInline(lines[regression.key], "regression"); block {
			keyLine = lines[regression.key]
		}
	}

	var notes []string
	var commands []string
	whenIndent := 2
	switch repoRegressionOf(old) {
	case regressionOfToday:
		if !hasWhen {
			return lines, nil, nil
		}
		if _, both := held["when"]; both {
			return nil, nil, fmt.Errorf("regression_when and regression.when are both written")
		}
		if !block {
			return nil, nil, fmt.Errorf("regression is written on one line")
		}
		whenIndent, _ = repoChildIndent(lines, regression)
		commands = append(commands, lines[regression.key+1:regression.end]...)
	case regressionOwn:
		notes = append(notes, "regression.commands: was regression")
		if !block {
			rest, _ := repoInline(lines[regression.key], "regression")
			commands = []string{"  commands:" + strings.TrimRight(rest, "\n") + "\n"}
			break
		}
		childIndent, _ := repoChildIndent(lines, regression)
		body, err := repoShift(lines[regression.key+1:regression.end], 4-childIndent)
		if err != nil {
			return nil, nil, err
		}
		commands = append([]string{"  commands:\n"}, body...)
	case regressionGates:
		var err error
		if commands, err = repoGatesAsCommands(raw, old); err != nil {
			return nil, nil, err
		}
		notes = append(notes, "regression.commands: REVIEW: it had no command of its own, so its regression ran the gates; the ones that run are written there, to go on checking the same. Empty it for a repository that needs no regression")
	case regressionNone:
		if !hasRegression && !hasWhen {
			return lines, nil, nil
		}
		commands = []string{"  commands: {}\n"}
		notes = append(notes, "regression.commands: empty, as it was: this repository has no regression")
	}

	var when []string
	if hasWhen {
		head, _ := repoShift(lines[whenEntry.head:whenEntry.key], whenIndent)
		rest, _ := repoInline(lines[whenEntry.key], "regression_when")
		when = append(head, strings.Repeat(" ", whenIndent)+"when:"+strings.TrimRight(rest, "\n")+"\n")
		items := lines[whenEntry.key+1 : whenEntry.end]
		delta := whenIndent
		for _, item := range items {
			if !repoBlank(item) && !repoComment(item) {
				if repoIndent(item) == 0 {
					// A list written at the margin goes below its key.
					delta = whenIndent + 2
				}
				break
			}
		}
		body, err := repoShift(items, delta)
		if err != nil {
			return nil, nil, err
		}
		when = append(when, body...)
		notes = append(notes, "regression.when: was regression_when")
	}

	section := append([]string{keyLine}, commands...)
	section = append(section, when...)
	cut := func(from, to int) {
		from, to = repoCutRange(lines, from, to)
		lines = repoSplice(lines, from, to, nil)
	}
	switch {
	case hasRegression && hasWhen && whenEntry.head > regression.key:
		cut(whenEntry.head, whenEntry.end)
		lines = repoSplice(lines, regression.key, regression.end, section)
	case hasRegression && hasWhen:
		lines = repoSplice(lines, regression.key, regression.end, section)
		cut(whenEntry.head, whenEntry.end)
	case hasRegression:
		lines = repoSplice(lines, regression.key, regression.end, section)
	case hasWhen:
		lines = repoSplice(lines, whenEntry.head, whenEntry.end, section)
	default:
		gates, found := repoEntryAt(lines, 0, len(lines), 0, "gates")
		if !found {
			return nil, nil, fmt.Errorf("gates is not written on a line of its own")
		}
		section = append([]string{"\n"}, section...)
		if gates.end < len(lines) && !repoBlank(lines[gates.end]) {
			section = append(section, "\n")
		}
		lines = repoSplice(lines, gates.end, gates.end, section)
	}
	return lines, notes, nil
}

// repoOfToday returns what old, a repo.config.yaml as an older foreman-kit read it, says under the
// keys of today.
func repoOfToday(old map[string]any) map[string]any {
	now := map[string]any{}
	for name, value := range old {
		now[name] = value
	}
	section := func(name string) map[string]any {
		held, _ := old[name].(map[string]any)
		out := map[string]any{}
		for k, v := range held {
			out[k] = v
		}
		return out
	}
	if repoWritesFormer(old, "gates.setup") {
		gates := section("gates")
		now["setup"] = gates["setup"]
		delete(gates, "setup")
		now["gates"] = gates
	}
	if repoWritesFormer(old, "service.prepare") {
		service, visual := section("service"), section("visual_qa")
		visual["prepare"] = service["prepare"]
		delete(service, "prepare")
		now["service"], now["visual_qa"] = service, visual
	}
	if services, wrote := old["requires"]; wrote {
		now["required_services"] = services
		delete(now, "requires")
	}
	regression := map[string]any{}
	switch repoRegressionOf(old) {
	case regressionOfToday:
		regression = section("regression")
	case regressionOwn:
		regression["commands"] = old["regression"]
	case regressionGates:
		regression["commands"] = repoGatesThatRun(old)
	}
	if paths, wrote := old["regression_when"]; wrote {
		regression["when"] = paths
		delete(now, "regression_when")
	}
	now["regression"] = regression
	return now
}

// repoMigrationKeptEveryValue checks the file written against the one it was written from: each
// value sits under the key that holds it now, nothing else changed and no former key is left.
func repoMigrationKeptEveryValue(old map[string]any, written []byte) error {
	var now map[string]any
	if err := yaml.Unmarshal(written, &now); err != nil {
		return fmt.Errorf("the file written is not valid YAML: %s", firstLineOf(err.Error()))
	}
	if left := RepoFormerKeys(now); len(left) > 0 {
		return fmt.Errorf("the file written still holds %s", left[0].Was)
	}
	want := repoOfToday(old)
	names := map[string]bool{}
	for name := range want {
		names[name] = true
	}
	for name := range now {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		if !repoSame(want[name], now[name]) {
			return fmt.Errorf("%s would change", name)
		}
	}
	return nil
}

// repoSame reports whether two values of a repo.config.yaml say the same. A key that holds nothing
// says what a key that is not written says.
func repoSame(a, b any) bool {
	if repoHoldsNothing(a) && repoHoldsNothing(b) {
		return true
	}
	ma, aIsMapping := a.(map[string]any)
	mb, bIsMapping := b.(map[string]any)
	if !aIsMapping || !bIsMapping {
		return valuesEqual(a, b)
	}
	for name, value := range ma {
		if !repoSame(value, mb[name]) {
			return false
		}
	}
	for name, value := range mb {
		if _, compared := ma[name]; !compared && !repoHoldsNothing(value) {
			return false
		}
	}
	return true
}

// repoHoldsNothing reports whether v holds nothing: it is missing, or an empty list or text, or a
// mapping whose entries hold nothing.
func repoHoldsNothing(v any) bool {
	switch val := v.(type) {
	case nil:
		return true
	case string:
		return val == ""
	case map[string]any:
		for _, value := range val {
			if !repoHoldsNothing(value) {
				return false
			}
		}
		return true
	case []any:
		return len(val) == 0
	}
	return false
}
