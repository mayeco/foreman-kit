package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/settings"
)

// This file rewrites the configuration files of an application written for an older foreman-kit:
// `foreman-kit update` calls it. Each file comes out as the seed of today with the values of the
// application in it, moved from the keys that held them before (settings.Formers). Nothing reads a
// former key afterwards: one left in a file is an error of `foreman-kit config check`.

// Migration is what rewriting the configuration files of an application comes to.
type Migration struct {
	// Files maps each file to rewrite, relative to the application root, to its new contents.
	Files map[string][]byte
	// Changed lists those files, in the order `foreman-kit config show` prints them.
	Changed []string
	// Notes says, a line each, what moved where and what a person has to look at.
	Notes []string
}

// Pending reports whether the migration rewrites anything.
func (m *Migration) Pending() bool { return m != nil && len(m.Files) > 0 }

// Agents returns .foreman-kit/agents.yaml as the application will hold it: the rewritten file when the
// migration rewrites it, else the one on disk (nil when there is none).
func (m *Migration) Agents(root string) *AgentsConfig {
	if m != nil {
		if text, ok := m.Files[AgentsFile]; ok {
			var data map[string]any
			if yaml.Unmarshal(text, &data) == nil && data != nil {
				return agentsConfigOf(data)
			}
		}
	}
	return Load(root)
}

// formerCommandTemplates are the command lines .foreman-kit/agents.yaml held for each agent before its
// commands ran exactly as written. A line left as it was never ran: the binary built the command
// itself, the one the kit now ships in full.
var formerCommandTemplates = map[string]string{
	"claude":       "claude -p {{prompt}} --output-format json --permission-mode bypassPermissions --no-session-persistence",
	"codex":        "codex exec --dangerously-bypass-approvals-and-sandbox --ephemeral --json -C {{workdir}} {{prompt}}",
	"copilot":      "copilot -p {{prompt}} --allow-all --no-ask-user --autopilot --output-format json --no-auto-update",
	"hermes":       "hermes -z {{prompt}} --yolo --accept-hooks",
	"opencode":     "opencode run --format json {{prompt}}",
	"agy":          "agy chat --mode agent -r --yolo {{prompt}}",
	"kilo":         "kilo run --auto --format json {{prompt}}",
	"cline":        "cline -y --json --timeout {{timeout}} -c {{workdir}} {{prompt}}",
	"pi":           "pi -p --no-session --mode json {{prompt}}",
	"omp":          "omp -p --no-session --mode json {{prompt}}",
	"crush":        "crush run -q -y -c {{workdir}} {{prompt}}",
	"cursor-agent": "cursor-agent -p --force --trust --output-format json --workspace {{workdir}} {{prompt}}",
	"zcode":        "zcode -p {{prompt}} --output-format json",
}

// formerPlaceholderRe matches a placeholder a command could hold before and cannot now: what it
// stood for is written in the command itself.
var formerPlaceholderRe = regexp.MustCompile(`"?\{\{(budget_usd|claude_allowed_tools|claude_denied_tools|copilot_denied_tools)\}\}"?`)

// formerBudgetKey held Claude Code's spending cap per call.
const formerBudgetKey = settings.Key("pipeline.limits.agent_budget_usd")

// configData is the configuration files of an application as they stand on disk.
type configData struct {
	root string
	// data holds the parsed contents of each file, by the name of the file; text its bytes.
	data map[string]map[string]any
	text map[string][]byte
	// broken says why a file that exists cannot be read.
	broken map[string]error
}

func readConfigData(root string) *configData {
	c := &configData{root: root, data: map[string]map[string]any{}, text: map[string][]byte{}, broken: map[string]error{}}
	for _, f := range settings.Files() {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			continue
		}
		c.text[f.Name] = raw
		var data map[string]any
		if err := yaml.Unmarshal(raw, &data); err != nil {
			c.broken[f.Name] = err
			continue
		}
		if data == nil {
			data = map[string]any{}
		}
		c.data[f.Name] = data
	}
	return c
}

// old reads one key, of today or former, as its file holds it.
func (c *configData) old(key settings.Key) any {
	return digMap(c.data[key.File()], key.Parts()...)
}

// needsRewrite reports whether the file called name is missing, writes a former key or value, or
// lacks or misuses a key of today.
func (c *configData) needsRewrite(name string) bool {
	data, present := c.data[name]
	if !present {
		return c.broken[name] == nil
	}
	if settings.HasFormer(name, data) {
		return true
	}
	r := (&settings.Reader{Root: c.root, FilesAlone: true}).Use(name, data)
	return len(r.Problems(name)) > 0 || (name == settings.Agents.File() && formerCommands(data))
}

// formerCommands reports whether the commands of .foreman-kit/agents.yaml are still written the way they
// were before they ran as written: an agent's command as a bare line, or with a placeholder that is
// gone.
func formerCommands(data map[string]any) bool {
	commands, _ := data["commands"].(map[string]any)
	for name, raw := range commands {
		if text, isText := raw.(string); isText && strings.TrimSpace(text) == formerCommandTemplates[name] {
			return true
		}
		spec, ok := ParseCommandSpec(raw)
		if !ok {
			continue
		}
		for _, command := range []any{spec.Run, spec.Restricted, spec.Ping} {
			if text, isText := command.(string); isText && formerPlaceholderRe.MatchString(text) {
				return true
			}
		}
	}
	return false
}

// ConfigNeedsRewrite reports whether `foreman-kit update` has configuration files of root to
// rewrite: one was written for an older foreman-kit, is missing, or lacks a key.
func ConfigNeedsRewrite(root string) bool {
	c := readConfigData(root)
	for _, f := range settings.Files() {
		if c.needsRewrite(f.Name) {
			return true
		}
	}
	return false
}

// MigrateConfig returns the configuration files of root rewritten for this foreman-kit: each file
// that needs it as the seed of today with the values of the application in it. It writes nothing.
// A file that is not valid YAML is left alone, with a note: a person has to mend it first.
func MigrateConfig(root string) (*Migration, error) {
	c := readConfigData(root)
	m := &Migration{Files: map[string][]byte{}}
	for _, f := range settings.Files() {
		if err := c.broken[f.Name]; err != nil {
			m.Notes = append(m.Notes, fmt.Sprintf("%s is not valid YAML (%s): mend it, then run foreman-kit update again", f.Path, firstLineOf(err.Error())))
			continue
		}
		if !c.needsRewrite(f.Name) {
			continue
		}
		text, notes, err := c.rewrite(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		m.Files[f.Path] = text
		m.Changed = append(m.Changed, f.Path)
		verb := "rewrote"
		if _, present := c.data[f.Name]; !present {
			verb = "created"
		}
		m.Notes = append(m.Notes, fmt.Sprintf("%s %s with the keys of this foreman-kit", verb, f.Path))
		m.Notes = append(m.Notes, notes...)
	}
	if gone := c.goneNotes(); len(gone) > 0 && len(m.Changed) > 0 {
		m.Notes = append(m.Notes, "keys nothing reads any more, with what does their job now:")
		m.Notes = append(m.Notes, gone...)
	}
	return m, nil
}

// rewrite returns the file f as the seed of today with the values of the application.
func (c *configData) rewrite(f settings.File) ([]byte, []string, error) {
	var notes []string
	values := map[settings.Key]any{}
	current := c.data[f.Name]
	for _, s := range settings.In(f.Name) {
		// The commands of the agents and the strategies are written as blocks, below.
		if s.Key == settings.AgentCommands || s.Key == settings.ExplorationStrategies {
			continue
		}
		raw := digMap(current, s.Key.Parts()...)
		if raw != nil && s.Problem(raw) == "" && checkSettingTypeProblemQuiet(s, raw) {
			values[s.Key] = raw
			continue
		}
		move, moved := settings.Moved(s.Key, c.old)
		switch {
		case moved:
			// A value that is the kit's own stays as the seed writes it, and moves without a word;
			// one the application chose is written and named.
			if sameSetting(s, move.Value, s.Shipped()) {
				continue
			}
			values[s.Key] = move.Value
			notes = append(notes, fmt.Sprintf("  %s: %s (was %s: %s in %s)", s.Key.Path(), settings.Inline(move.Value), move.From.Path(), settings.Inline(move.Was), formerFilePath(move.From)))
		case move.Lost != "":
			notes = append(notes, fmt.Sprintf("  %s: REVIEW: %s: %s in %s does not fit it (it must be %s); the kit's value %s was written", s.Key.Path(), move.Lost.Path(), settings.Inline(move.Was), formerFilePath(move.Lost), s.Expected(), settings.Inline(s.Shipped())))
		case raw != nil:
			notes = append(notes, fmt.Sprintf("  %s: REVIEW: %s does not fit it (it must be %s); the kit's value %s was written", s.Key.Path(), settings.Inline(raw), s.Expected(), settings.Inline(s.Shipped())))
		}
	}
	if f.Name == settings.Agents.File() {
		commands, commandNotes := c.migratedCommands(values)
		values[settings.AgentCommands] = commands
		notes = append(notes, commandNotes...)
	}

	text, err := RenderSeed(f.Name, values)
	if err != nil {
		return nil, nil, err
	}
	// What a file holds that is not a setting stays the application's own: its strategies, and the
	// canary and the gates of the evaluator.
	switch f.Name {
	case settings.ExplorationStrategies.File():
		if block, ok := TopLevelBlock(c.text[f.Name], "strategies"); ok {
			converted, changed := sessionsOfStrategies(block)
			if text, err = ReplaceTopLevelBlock(text, "strategies", converted); err != nil {
				return nil, nil, err
			}
			if changed > 0 {
				notes = append(notes, fmt.Sprintf("  strategies: wrote session on each one (%d had needs: browser and run in the visual session; the others run in the cycle)", changed))
			}
		}
	case settings.EvaluatorFile:
		for _, key := range []string{"canary", "gates"} {
			block, ok := TopLevelBlock(c.text[f.Name], key)
			if !ok {
				continue
			}
			if text, err = ReplaceTopLevelBlock(text, key, commentsOfToday(block)); err != nil {
				return nil, nil, err
			}
		}
		added, err := addMissingGates(&text)
		if err != nil {
			return nil, nil, err
		}
		for _, gate := range added {
			shipped, _ := settings.Of(settings.GateMode(gate)).Default.(string)
			notes = append(notes, fmt.Sprintf("  gates.%s: REVIEW: added with mode %s, as it was while the file did not have this gate; the kit ships it as %s", gate, settings.GateOff, shipped))
		}
	}

	// The file read back has every key of today, and nothing else.
	var written map[string]any
	if err := yaml.Unmarshal(text, &written); err != nil {
		return nil, nil, fmt.Errorf("the file written is not valid YAML: %w", err)
	}
	r := (&settings.Reader{Root: c.root, FilesAlone: true}).Use(f.Name, written)
	for _, p := range r.Problems(f.Name) {
		return nil, nil, fmt.Errorf("the file written still has a problem: %s", p.Text)
	}
	return text, notes, nil
}

// sameSetting reports whether a and b are the same value of s, however each is written: 60m and 1h
// are the same time.
func sameSetting(s settings.Setting, a, b any) bool {
	pa, okA := s.Parse(a)
	pb, okB := s.Parse(b)
	if okA && okB && s.Scalar() {
		return pa == pb
	}
	return sameValue(a, b)
}

// checkSettingTypeProblemQuiet reports whether a value that fits the kind of s also holds what the
// kind expects inside: the agents of a role as a list, the arguments of an agent as text or a list.
func checkSettingTypeProblemQuiet(s settings.Setting, raw any) bool {
	if s.Kind != settings.KindArgs {
		return true
	}
	byAgent, _ := raw.(map[string]any)
	for agent, args := range byAgent {
		if checkArgsValue(agent, args) != "" {
			return false
		}
	}
	return true
}

// formerFilePath returns the relative path of the file a former key lived in.
func formerFilePath(key settings.Key) string {
	if f, ok := settings.FileNamed(key.File()); ok {
		return f.Path
	}
	return "config/" + key.File() + ".yaml"
}

// goneNotes says, for each former key the application set and nothing took over, what does its job
// now: its value is not lost without a word.
func (c *configData) goneNotes() []string {
	var notes []string
	for _, former := range settings.Formers() {
		if len(former.To) > 0 {
			continue
		}
		raw := c.old(former.Key)
		if raw == nil {
			continue
		}
		notes = append(notes, fmt.Sprintf("  %s: %s in %s is %s", former.Key.Path(), settings.Inline(raw), formerFilePath(former.Key), former.NoLongerRead()))
	}
	return notes
}

// needsLineRe matches the needs line of a strategy, and slugLineRe the line that begins one.
var (
	needsLineRe   = regexp.MustCompile(`^(\s+)needs:\s*["']?browser["']?\s*(#.*)?$`)
	slugLineRe    = regexp.MustCompile(`^(\s*)-\s+slug:`)
	sessionLineRe = regexp.MustCompile(`^\s+(-\s+)?session:`)
)

// sessionsOfStrategies returns the strategies block of .foreman-kit/exploration.yaml with the session of
// each strategy written: visual_qa where it said needs: browser, cycle where it said nothing. It
// also returns how many said needs: browser.
func sessionsOfStrategies(block string) (string, int) {
	lines := strings.SplitAfter(block, "\n")
	type strategy struct{ start, end int }
	var items []strategy
	for i, line := range lines {
		if slugLineRe.MatchString(line) {
			if n := len(items); n > 0 {
				items[n-1].end = i
			}
			items = append(items, strategy{start: i, end: len(lines)})
		}
	}
	if len(items) == 0 {
		return block, 0
	}
	browser := 0
	out := append([]string(nil), lines[:items[0].start]...)
	for _, item := range items {
		body := lines[item.start:item.end]
		hasSession, needs := false, -1
		for i, line := range body {
			if sessionLineRe.MatchString(line) {
				hasSession = true
			}
			if needsLineRe.MatchString(line) {
				needs = i
			}
		}
		indent := slugLineRe.FindStringSubmatch(body[0])[1] + "  "
		switch {
		case needs >= 0:
			browser++
			if hasSession {
				body = append(append([]string(nil), body[:needs]...), body[needs+1:]...)
			} else {
				body = append([]string(nil), body...)
				body[needs] = indent + "session: " + settings.SessionVisualQA + "\n"
			}
		case !hasSession:
			withSession := append([]string(nil), body[:1]...)
			withSession = append(withSession, indent+"session: "+settings.SessionCycle+"\n")
			body = append(withSession, body[1:]...)
		}
		out = append(out, body...)
	}
	return strings.Join(out, ""), browser
}

// gateModeLineRe matches the mode line of a gate as the seed writes it.
var gateModeLineRe = regexp.MustCompile(`(?m)^(\s+mode:[ \t]*)[A-Za-z"']+`)

// formerNamesInComments are the settings the comments of a block the application keeps (the gates of
// the evaluator) name by the word an older kit read, with what a comment of today calls each.
var formerNamesInComments = []struct{ was, is string }{
	{"blocked_retry_hours", settings.BlockedRetryAfter.Path()},
}

// commentsOfToday returns block with the settings its comment lines name written the way today's
// files write them. Only lines that are a comment and nothing else change: what the block decides
// stays as the application wrote it.
func commentsOfToday(block string) string {
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, name := range formerNamesInComments {
			line = strings.ReplaceAll(line, name.was, name.is)
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

// addMissingGates appends to the gates of .foreman-kit/evaluator.yaml, in text, each gate of the catalog
// the file does not have: its questions and thresholds as the seed writes them, and mode off, which
// is what a gate the file did not list was. It returns the gates it added.
func addMissingGates(text *[]byte) ([]string, error) {
	var written map[string]any
	if err := yaml.Unmarshal(*text, &written); err != nil {
		return nil, err
	}
	have, _ := written["gates"].(map[string]any)
	seed, err := assets.ReadSeed(settings.GateMode("plan_review").FilePath())
	if err != nil {
		return nil, err
	}
	seedGates, ok := TopLevelBlock(seed, "gates")
	if !ok {
		return nil, fmt.Errorf("the seed has no gates")
	}
	var added []string
	block, _ := TopLevelBlock(*text, "gates")
	for _, gate := range settings.Gates() {
		if _, has := have[gate]; has {
			continue
		}
		piece, ok := nestedBlock(seedGates, gate)
		if !ok {
			return nil, fmt.Errorf("the seed has no gate %s", gate)
		}
		if block == "" || strings.TrimSpace(block) == "gates: {}" || strings.TrimSpace(block) == "gates:" {
			block = "gates:\n"
		}
		if !strings.HasSuffix(block, "\n") {
			block += "\n"
		}
		if loc := gateModeLineRe.FindStringSubmatchIndex(piece); loc != nil {
			piece = piece[:loc[3]] + settings.GateOff + piece[loc[1]:]
		}
		block += "\n" + piece
		added = append(added, gate)
	}
	if len(added) == 0 {
		return nil, nil
	}
	out, err := ReplaceTopLevelBlock(*text, "gates", block)
	if err != nil {
		return nil, err
	}
	*text = out
	return added, nil
}

// nestedBlock returns, from the block of a top-level key, the lines of the entry called name one
// level in: its comment lines just above it, its own line and everything under it.
func nestedBlock(block, name string) (string, bool) {
	lines := strings.SplitAfter(block, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "  "+name+":") {
			start = i
			break
		}
	}
	if start < 0 {
		return "", false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent <= 2 && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			end = i
			break
		}
	}
	// The comments of the entry after it, and the blank lines before them, are not this entry's.
	for end > start+1 {
		prev := strings.TrimSpace(lines[end-1])
		prevIndent := len(lines[end-1]) - len(strings.TrimLeft(lines[end-1], " "))
		if prev == "" || (strings.HasPrefix(prev, "#") && prevIndent <= 2) {
			end--
			continue
		}
		break
	}
	// The comment lines right above the entry are its own.
	first := start
	for first > 0 {
		prev := lines[first-1]
		if strings.HasPrefix(strings.TrimSpace(prev), "#") && strings.HasPrefix(prev, "  ") {
			first--
			continue
		}
		break
	}
	return strings.Join(lines[first:end], ""), true
}

// migratedCommands returns the commands .foreman-kit/agents.yaml writes for an application whose file
// held them the former way, with what a person has to know about them.
//
// An agent whose command was the line the kit wrote (or that had none) never ran that line: the
// binary built its command, the one the kit now ships in full, and that is what is written. A
// command a person had edited is kept, with what the binary added to it written out: the
// placeholders that are gone, the role's arguments where they went, the prompt at its end.
func (c *configData) migratedCommands(values map[settings.Key]any) (map[string]any, []string) {
	var notes []string
	agentsData := c.data[settings.Agents.File()]
	written, _ := agentsData["commands"].(map[string]any)

	var names []string
	seen := map[string]bool{}
	add := func(list []string) {
		for _, name := range list {
			if name != "" && name != "stub" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	add(Names(values[settings.Agents]))
	for _, role := range RolesOrder {
		add(Names(values[settings.RoleAgents(role)]))
	}
	var others []string
	for name := range written {
		others = append(others, name)
	}
	sort.Strings(others)
	add(others)

	budget := ShippedClaudeBudget
	if raw := c.old(formerBudgetKey); raw != nil {
		if text := strings.TrimSpace(fmt.Sprint(raw)); text != "" {
			budget = text
		}
	}
	restricted := fmt.Sprint(values[settings.AgentPermissions]) == settings.PermissionsRestricted
	if values[settings.AgentPermissions] == nil {
		restricted = false
	}

	out := map[string]any{}
	for _, name := range names {
		raw, has := written[name]
		spec, parsed := ParseCommandSpec(raw)
		shipped, known := ShippedCommands(name)
		bare, isBare := raw.(string)
		untouched := !has || !parsed || (isBare && strings.TrimSpace(bare) == formerCommandTemplates[name])
		if untouched {
			if !known {
				continue
			}
			if name == "claude" && budget != ShippedClaudeBudget {
				from, to := "--max-budget-usd "+ShippedClaudeBudget+" ", "--max-budget-usd "+budget+" "
				shipped.Run = strings.Replace(fmt.Sprint(shipped.Run), from, to, 1)
				shipped.Restricted = strings.Replace(fmt.Sprint(shipped.Restricted), from, to, 1)
				notes = append(notes, fmt.Sprintf("  commands.claude: --max-budget-usd %s is written in its run and restricted commands (was %s in %s)", budget, formerBudgetKey.Path(), formerFilePath(formerBudgetKey)))
			}
			if has {
				notes = append(notes, fmt.Sprintf("  commands.%s: the commands the binary built for it are written in full, to run exactly as written", name))
			}
			out[name] = shipped
			continue
		}

		// A command a person wrote: kept, with what used to be added to it written out.
		if formerShape(raw) {
			spec.Run = asWritten(spec.Run, budget)
			spec.Restricted = asWritten(spec.Restricted, budget)
			spec.Ping = asWritten(spec.Ping, budget)
			notes = append(notes, fmt.Sprintf("  commands.%s: kept as you wrote it; {{args}} and {{prompt}} are written where the binary used to put them", name))
		}
		if spec.Restricted == nil && restricted {
			notes = append(notes, fmt.Sprintf("  commands.%s: REVIEW: it has no restricted command, and %s is %s: it ran its run command; now it cannot be used until you write one", name, settings.AgentPermissions.Path(), settings.PermissionsRestricted))
		}
		out[name] = spec
	}
	return out, notes
}

// formerShape reports whether the commands of an agent are written the way they were before they
// ran as written: a bare line or list, or a mapping without {{args}} in its run command.
func formerShape(raw any) bool {
	entry, isMap := raw.(map[string]any)
	if !isMap {
		return true
	}
	run := entry["run"]
	if run == nil {
		run = entry["command"]
	}
	text, isText := run.(string)
	return !isText || !strings.Contains(text, "{{args}}") || formerPlaceholderRe.MatchString(text)
}

// asWritten returns a command a person wrote the former way as it has to be written to run the same
// today: the lists and the cap its placeholders stood for, {{args}} where the role's arguments went
// (before a lone --, else at the end) and {{prompt}} at the end of a command that named no prompt.
func asWritten(command any, budget string) any {
	text, isText := command.(string)
	if !isText {
		return command
	}
	text = formerPlaceholderRe.ReplaceAllStringFunc(strings.TrimSpace(text), func(found string) string {
		name := formerPlaceholderRe.FindStringSubmatch(found)[1]
		switch name {
		case "budget_usd":
			return budget
		case "claude_allowed_tools":
			return `"` + claudeAllowedTools + `"`
		case "claude_denied_tools":
			return `"` + claudeDeniedTools + `"`
		}
		return `"shell(git commit),shell(git push),shell(git rebase),shell(git reset),shell(git checkout),shell(git switch)"`
	})
	if !strings.Contains(text, "{{args}}") {
		if i := strings.Index(text+" ", " -- "); i >= 0 {
			text = text[:i] + " {{args}}" + text[i:]
		} else {
			text += " {{args}}"
		}
	}
	if !strings.Contains(text, "{{prompt}}") && !strings.Contains(text, "{{prompt_file}}") {
		text += " {{prompt}}"
	}
	return text
}

func firstLineOf(text string) string {
	return strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
}
