package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/settings"
)

// This file writes configuration files: the seed of a file with the values of one application in
// it. `foreman-kit init` writes .foreman-kit/agents.yaml that way, `foreman-kit config set` changes one
// value of a file, and `foreman-kit update` rewrites the files of an application installed before
// the keys of today (migrate.go).

// RenderSeed returns the seed of the configuration file called name with each key of values set to
// its value: the comments and the order of the seed, the values of the application. A key values
// does not name keeps the value the seed ships. The commands of the agents (agents.commands) are
// written as a block, one agent after the other; every other value is written on the line of its key.
func RenderSeed(name string, values map[settings.Key]any) ([]byte, error) {
	f, ok := settings.FileNamed(name)
	if !ok {
		return nil, fmt.Errorf("%s is not a configuration file", name)
	}
	text, err := assets.ReadSeed(f.Path)
	if err != nil {
		return nil, fmt.Errorf("%s has no seed: %w", f.Path, err)
	}
	for _, s := range settings.In(name) {
		value, given := values[s.Key]
		if !given {
			continue
		}
		if s.Key == settings.AgentCommands {
			commands, _ := value.(map[string]any)
			text, err = ReplaceTopLevelBlock(text, s.Key.Path(), renderCommands(commands, agentOrder(values, commands)))
		} else {
			text, err = setInText(text, s.Key.Parts(), value)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", f.Path, s.Key.Path(), err)
		}
	}

	// What was written is what was asked for: the file read back holds each value.
	var written map[string]any
	if err := yaml.Unmarshal(text, &written); err != nil {
		return nil, fmt.Errorf("%s: the file written is not valid YAML: %w", f.Path, err)
	}
	for key, want := range values {
		if key.File() != name {
			continue
		}
		if got := digMap(written, key.Parts()...); !sameValue(got, want) {
			return nil, fmt.Errorf("%s: %s was written as %s, not %s", f.Path, key.Path(), formatInspect(got), formatInspect(plainValue(want)))
		}
	}
	return text, nil
}

// agentOrder returns the order the commands of the agents are written in: the agents of the
// application as it lists them, then any other agent the file defines, by name.
func agentOrder(values map[settings.Key]any, commands map[string]any) []string {
	var order []string
	seen := map[string]bool{}
	for _, name := range Names(values[settings.Agents]) {
		if _, has := commands[name]; has && !seen[name] {
			seen[name] = true
			order = append(order, name)
		}
	}
	var rest []string
	for name := range commands {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(order, rest...)
}

// plainValue gives a value the shape YAML reads it back in: the commands of an agent as a mapping.
func plainValue(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}

// sameValue reports whether a value read from a file is the one that was to be written.
func sameValue(got, want any) bool {
	a, errA := json.Marshal(plainValue(got))
	b, errB := json.Marshal(plainValue(want))
	if errA != nil || errB != nil {
		return reflect.DeepEqual(got, want)
	}
	return string(a) == string(b)
}

// setInText returns the YAML text with the value of the key at path keys replaced by value, on the
// line of the key: the comments, the blank lines and every other line stay as they are. A key the
// text does not write yet is added after the last line of its section, with the sections it lacks.
// value is a scalar, a list of scalars or a mapping of them, all written on one line.
func setInText(text []byte, keys []string, value any) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(text, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		// A file with nothing but comments: there is no mapping to add the key to yet.
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map", Line: len(splitKeepEOL(string(text)))}}}
	}
	astLoc, err := navigateAndMutateAST(&doc, keys, value)
	if err != nil {
		return nil, err
	}

	lines := splitKeepEOL(string(text))
	var afterText string
	edited := false
	if astLoc.keyNode != nil && astLoc.keyNode.Line > 0 && astLoc.keyNode.Line <= len(lines) {
		lineIdx := astLoc.keyNode.Line - 1
		// A value that spans several lines (a block, a folded text) cannot be replaced on its line.
		if maxNodeLine(astLoc.valNode) <= astLoc.keyNode.Line && !isBlockScalar(astLoc.valNode) {
			if m := lineKeyValueRe.FindStringSubmatch(lines[lineIdx]); m != nil {
				head := m[1]
				if !strings.HasSuffix(head, " ") && !strings.HasSuffix(head, "\t") {
					head += " "
				}
				eol := m[4]
				if eol == "" {
					eol = "\n"
				}
				lines[lineIdx] = head + inlineYAML(value, astLoc.valNode) + m[3] + eol
				afterText = strings.Join(lines, "")
				edited = true
			}
		}
	} else if astLoc.keyNode != nil && astLoc.keyNode.Line == 0 && astLoc.insertLine >= 0 && astLoc.insertLine < len(lines) {
		path := astLoc.newPath
		if len(path) == 0 {
			path = keys[len(keys)-1:]
		}
		block := make([]string, 0, len(path))
		for i, name := range path {
			line := strings.Repeat(" ", astLoc.childIndent+2*i) + name + ":"
			if i == len(path)-1 {
				line += " " + inlineYAML(value, nil)
			}
			block = append(block, line+"\n")
		}
		idx := astLoc.insertLine + 1
		if !strings.HasSuffix(lines[idx-1], "\n") {
			lines[idx-1] += "\n"
		}
		lines = append(lines[:idx], append(block, lines[idx:]...)...)
		afterText = strings.Join(lines, "")
		edited = true
	}
	if !edited {
		return nil, fmt.Errorf("its value does not sit on one line")
	}

	var after, expected map[string]any
	if err := yaml.Unmarshal([]byte(afterText), &after); err != nil {
		return nil, fmt.Errorf("the edit would break the file")
	}
	if err := doc.Decode(&expected); err != nil {
		return nil, fmt.Errorf("the edit would break the file")
	}
	if !mapsEquivalent(after, expected) {
		return nil, fmt.Errorf("the edit would change more than that key")
	}
	return []byte(afterText), nil
}

// isBlockScalar reports whether n is a text written over several lines (| or >).
func isBlockScalar(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && (n.Style == yaml.LiteralStyle || n.Style == yaml.FoldedStyle)
}

// inlineYAML writes value on one line, the way a configuration file holds it: a word as it is when
// YAML reads it back as the same text, quoted otherwise; a list as [a, b]; a mapping as {k: v}.
// old is the node the value replaces: a text that was quoted stays quoted.
func inlineYAML(value any, old *yaml.Node) string {
	switch v := value.(type) {
	case nil:
		return `""`
	case string:
		wasQuoted := old != nil && (old.Style == yaml.DoubleQuotedStyle || old.Style == yaml.SingleQuotedStyle)
		if !wasQuoted && v != "" && plainScalarRe.MatchString(v) {
			var parsed any
			if yaml.Unmarshal([]byte(v), &parsed) == nil && parsed == v {
				return v
			}
		}
		b, _ := json.Marshal(v)
		return string(b)
	case []string:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = inlineYAML(item, nil)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = inlineYAML(item, nil)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []float64:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = fmt.Sprint(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		names := make([]string, 0, len(v))
		for name := range v {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, len(names))
		for i, name := range names {
			parts[i] = inlineYAML(name, nil) + ": " + inlineYAML(v[name], nil)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(value)
}

// topLevelBlock returns the lines of the top-level key called key in lines: from the line of the
// key to the last line of its value, [start, end). The blank lines and the comments that follow the
// value belong to the key after it. It reports false when the text does not write the key.
func topLevelBlock(lines []string, key string) (start, end int, found bool) {
	start = -1
	for i, line := range lines {
		if strings.HasPrefix(line, key+":") {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, 0, false
	}
	end = len(lines)
	for i := start + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		end = i
		break
	}
	for end > start+1 {
		prev := lines[end-1]
		if strings.TrimSpace(prev) == "" || prev[0] == '#' {
			end--
			continue
		}
		break
	}
	return start, end, true
}

// TopLevelBlock returns the text of the top-level key called key in a YAML text, from the line of
// the key to the last line of its value, and whether the text writes the key.
func TopLevelBlock(text []byte, key string) (string, bool) {
	lines := splitKeepEOL(string(text))
	start, end, found := topLevelBlock(lines, key)
	if !found {
		return "", false
	}
	block := strings.Join(lines[start:end], "")
	if !strings.HasSuffix(block, "\n") {
		block += "\n"
	}
	return block, true
}

// ReplaceTopLevelBlock returns the YAML text with the block of the top-level key called key replaced
// by block, which starts with the line of the key. A text that does not write the key gets the
// block at its end.
func ReplaceTopLevelBlock(text []byte, key, block string) ([]byte, error) {
	if !strings.HasPrefix(block, key+":") {
		return nil, fmt.Errorf("the block does not start with %s:", key)
	}
	if !strings.HasSuffix(block, "\n") {
		block += "\n"
	}
	lines := splitKeepEOL(string(text))
	start, end, found := topLevelBlock(lines, key)
	if !found {
		joined := strings.Join(lines, "")
		if joined != "" && !strings.HasSuffix(joined, "\n") {
			joined += "\n"
		}
		return []byte(joined + block), nil
	}
	if end > 0 && !strings.HasSuffix(lines[end-1], "\n") {
		lines[end-1] += "\n"
	}
	out := strings.Join(lines[:start], "") + block + strings.Join(lines[end:], "")
	return []byte(out), nil
}

// renderCommands writes the commands block of .foreman-kit/agents.yaml: each agent with its run,
// restricted and ping commands, exactly as they will run.
func renderCommands(commands map[string]any, order []string) string {
	if len(order) == 0 {
		return "commands: {}\n"
	}
	var b strings.Builder
	b.WriteString("commands:\n")
	for _, name := range order {
		spec, ok := ParseCommandSpec(commands[name])
		if !ok {
			continue
		}
		b.WriteString("  " + inlineYAML(name, nil) + ":\n")
		for _, entry := range []struct {
			key   string
			value any
		}{{"run", spec.Run}, {"restricted", spec.Restricted}, {"ping", spec.Ping}} {
			if entry.value != nil {
				b.WriteString(renderCommand("    ", entry.key, entry.value))
			}
		}
		if spec.Skills != "" {
			b.WriteString("    skills: " + inlineYAML(spec.Skills, nil) + "\n")
		}
		if spec.Tool != "" {
			b.WriteString("    tool: " + inlineYAML(spec.Tool, nil) + "\n")
		}
		if spec.StdinNull != nil {
			b.WriteString(fmt.Sprintf("    stdin_null: %t\n", *spec.StdinNull))
		}
		if spec.Detached {
			b.WriteString("    detached: true\n")
		}
		if spec.Stop != nil {
			b.WriteString(renderCommand("    ", "stop", spec.Stop))
		}
		if len(spec.Activity) > 0 {
			b.WriteString("    activity: " + inlineYAML(spec.Activity, nil) + "\n")
		}
	}
	return b.String()
}

// commandWidth is the column the lines of a long command are folded at.
const commandWidth = 96

// renderCommand writes one command of an agent under key: on the line of the key when it fits,
// else folded over several lines (YAML joins them back with one space each). A command either way
// would not read back as written is quoted on one line instead.
func renderCommand(indent, key string, command any) string {
	text, isText := command.(string)
	if !isText {
		// A list of arguments is taken as it is.
		words, _ := CommandWords(command)
		return indent + key + ": " + inlineYAML(words, nil) + "\n"
	}
	readsBack := func(candidate string) bool {
		var parsed map[string]any
		return yaml.Unmarshal([]byte(candidate), &parsed) == nil && parsed[key] == text
	}
	if len(indent)+len(key)+2+len(text) <= commandWidth {
		if plain := key + ": " + text + "\n"; readsBack(plain) {
			return indent + plain
		}
	}
	if !strings.Contains(text, "  ") && !strings.ContainsAny(text, "\n\t") && text == strings.TrimSpace(text) && text != "" {
		var folded strings.Builder
		folded.WriteString(key + ": >-\n")
		line := ""
		for _, word := range strings.Split(text, " ") {
			if line != "" && len(indent)+2+len(line)+1+len(word) > commandWidth {
				folded.WriteString("  " + line + "\n")
				line = word
				continue
			}
			if line != "" {
				line += " "
			}
			line += word
		}
		folded.WriteString("  " + line + "\n")
		if readsBack(folded.String()) {
			return indent + strings.ReplaceAll(strings.TrimSuffix(folded.String(), "\n"), "\n", "\n"+indent) + "\n"
		}
	}
	quoted, _ := json.Marshal(text)
	return indent + key + ": " + string(quoted) + "\n"
}
