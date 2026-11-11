package cycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

const (
	ProposalsFile   = ".foreman-kit/agent_proposals.json"
	BacklogFile     = ".foreman-kit/agent_backlog.json"
	HistoryFile     = ".foreman-kit/agent_history.json"
	HistoryMaxLines = 1000
	CycleFile       = ".foreman-kit/cycle.yaml"
	PromptsDir      = ".foreman-kit/prompts"
	RulesTemplate   = ".foreman-kit/templates/authoring_rules.md"
)

var (
	ChangeNameRe         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	ArchiveDirNameRe     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-(.+)$`)
	RotatedHistoryFileRe = regexp.MustCompile(`^\.foreman-kit/agent_history\.(\d+)\.json$`)
	HTMLCommentRe        = regexp.MustCompile(`(?s)<!--.*?-->\n?`)
	PlaceholderRe        = regexp.MustCompile(`\{\{(\w+)\}\}`)
	ProposalOwnFields    = map[string]bool{
		"status":        true,
		"source":        true,
		"strategy":      true,
		"proposed_at":   true,
		"review":        true,
		"queued_at":     true,
		"integrated_at": true,
	}
	ProposalStatusRank = map[string]int{
		"proposed":   0,
		"approved":   1,
		"queued":     2,
		"integrated": 3,
		"rejected":   3,
	}
	ScopePaths = []string{
		memory.Dir,
		".foreman-kit/agent_backlog.json",
		".foreman-kit/agent_history.json",
		".foreman-kit/agent_proposals.json",
	}
	RecordedPaths = []string{
		memory.Dir,
		".foreman-kit/agent_backlog.json",
		".foreman-kit/agent_history.json",
		".foreman-kit/agent_proposals.json",
		".foreman-kit/prompts",
	}
	JSONListPaths = map[string]bool{
		".foreman-kit/agent_backlog.json":   true,
		".foreman-kit/agent_history.json":   true,
		".foreman-kit/agent_proposals.json": true,
	}
)

// PromptPlaceholders defines known placeholders per prompt role.
var PromptPlaceholders = map[string]map[string]string{
	"plan": {
		"change":          "the change's name",
		"goal":            "the backlog item's description",
		"memory":          "the notes of one repository or capability each (.agent-memory/modules/)",
		"evaluator_hint":  "how to ask the evaluator a yes/no question (empty without one)",
		"authoring_rules": "the kit's rules for OpenSpec artifacts (.foreman-kit/templates/authoring_rules.md)",
		"feedback":        "on a repair round, what failed verification",
	},
	"build": {
		"change":          "the change's name",
		"branch":          "the feature branch every child repository is on",
		"memory":          "the notes of one repository or capability each (.agent-memory/modules/)",
		"evaluator_hint":  "how to ask the evaluator a yes/no question (empty without one)",
		"authoring_rules": "the kit's rules for OpenSpec artifacts (.foreman-kit/templates/authoring_rules.md)",
		"feedback":        "on a repair round, what failed verification",
		"rebuild":         "when the change is rebuilt after a rebase conflict, where the old code is",
	},
	"cycle": {},
}

// RequiredPlaceholders defines placeholders that must remain present in each prompt.
var RequiredPlaceholders = map[string][]string{
	"plan":  {"change", "feedback"},
	"build": {"change", "branch", "feedback", "rebuild"},
	"cycle": {},
}

// ApplyError indicates a prompt proposal could not be applied cleanly.
type ApplyError struct {
	Msg string
}

func (e *ApplyError) Error() string { return e.Msg }

// PromptPath returns the path to .foreman-kit/prompts/<name>.md under root.
func PromptPath(root, name string) string {
	return filepath.Join(root, filepath.FromSlash(PromptsDir), name+".md")
}

// ReadPromptRaw reads .foreman-kit/prompts/<name>.md from root, falling back to embedded seeds if missing.
func ReadPromptRaw(root, name string, fallbackSeed bool) (string, error) {
	p := PromptPath(root, name)
	if data, err := os.ReadFile(p); err == nil {
		return string(data), nil
	}
	if fallbackSeed {
		if data, err := assets.ReadSeed(PromptsDir + "/" + name + ".md"); err == nil {
			return string(data), nil
		}
	}
	return "", fmt.Errorf("%s/%s.md is missing: restore it from the kit's release (a seed), then commit it", PromptsDir, name)
}

// RenderPrompt strips HTML comments and replaces known {{placeholder}} tokens.
func RenderPrompt(root, name string, vars map[string]string) (string, error) {
	raw, err := ReadPromptRaw(root, name, false)
	if err != nil {
		return "", err
	}
	withoutComments := HTMLCommentRe.ReplaceAllString(raw, "")
	rendered := PlaceholderRe.ReplaceAllStringFunc(withoutComments, func(match string) string {
		sub := PlaceholderRe.FindStringSubmatch(match)
		if len(sub) == 2 {
			if val, ok := vars[sub[1]]; ok {
				return val
			}
		}
		return match
	})
	return rendered, nil
}

// ExtractPlaceholders returns unique placeholder names used in .foreman-kit/prompts/<name>.md.
func ExtractPlaceholders(root, name string) ([]string, error) {
	raw, err := ReadPromptRaw(root, name, false)
	if err != nil {
		return nil, err
	}
	withoutComments := HTMLCommentRe.ReplaceAllString(raw, "")
	matches := PlaceholderRe.FindAllStringSubmatch(withoutComments, -1)
	seen := make(map[string]bool)
	var out []string
	for _, m := range matches {
		if len(m) == 2 && !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out, nil
}

// PromptVersion calculates the 12-char SHA-256 version of a prompt (without comments, plus authoring_rules if referenced).
func PromptVersion(root, name string) (string, error) {
	raw, err := ReadPromptRaw(root, name, false)
	if err != nil {
		return "", err
	}
	withoutComments := HTMLCommentRe.ReplaceAllString(raw, "")
	parts := []string{withoutComments}
	phs, _ := ExtractPlaceholders(root, name)
	for _, p := range phs {
		if p == "authoring_rules" {
			if rulesBytes, err := assets.ReadTemplate(root, "authoring_rules.md"); err == nil {
				parts = append(parts, string(rulesBytes))
			}
			break
		}
	}
	joined := strings.Join(parts, "\x00")
	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:])[:12], nil
}

// CheckPrompts validates all prompt files under .foreman-kit/prompts/ for missing files, unknown placeholders, and missing required placeholders.
func CheckPrompts(root string) (errs []string, warns []string) {
	for _, name := range []string{"plan", "build", "cycle"} {
		known := PromptPlaceholders[name]
		rel := fmt.Sprintf("%s/%s.md", PromptsDir, name)
		p := PromptPath(root, name)
		if !fileExists(p) {
			errs = append(errs, fmt.Sprintf("%s is missing: restore it from the kit's release (a seed)", rel))
			continue
		}
		used, err := ExtractPlaceholders(root, name)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		usedSet := make(map[string]bool, len(used))
		for _, u := range used {
			usedSet[u] = true
			if _, ok := known[u]; !ok {
				if len(known) == 0 {
					errs = append(errs, fmt.Sprintf("%s: unknown placeholder {{%s}} (it takes none)", rel, u))
				} else {
					var keys []string
					for k := range known {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					errs = append(errs, fmt.Sprintf("%s: unknown placeholder {{%s}} (known: %s)", rel, u, strings.Join(keys, ", ")))
				}
			}
		}
		for _, req := range RequiredPlaceholders[name] {
			if !usedSet[req] {
				errs = append(errs, fmt.Sprintf("%s: {{%s}} is missing: %s", rel, req, known[req]))
			}
		}
	}
	return errs, warns
}

// MemoryPromptBlock formats the {{memory}} block for PLAN and BUILD prompts (memory.PromptBlock): it
// reads names of files, runs nothing and writes nothing.
func MemoryPromptBlock(root string, capabilities []string) string {
	return memory.PromptBlock(root, capabilities)
}

// MemoryPlanLine returns what the plan of a cycle or of a visual session says about the memory
// (memory.PlanLine).
func MemoryPlanLine(root string) string {
	return memory.PlanLine(root)
}

// ReadJSONList reads a JSON file that contains either a top-level array of objects or {"changes": [...]}.
func ReadJSONList(path string) ([]map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []map[string]any{}, nil
		}
		return nil, err
	}
	return parseJSONListBytes(b)
}

func parseJSONListBytes(b []byte) ([]map[string]any, error) {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return []map[string]any{}, nil
	}
	var raw any
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		// Support newline-delimited minified JSON objects (1 object per line)
		var out []map[string]any
		for _, line := range bytes.Split(trimmed, []byte("\n")) {
			line = bytes.TrimSpace(bytes.TrimSuffix(bytes.TrimSpace(line), []byte(",")))
			if len(line) == 0 || bytes.Equal(line, []byte("[")) || bytes.Equal(line, []byte("]")) {
				continue
			}
			var m map[string]any
			if uErr := json.Unmarshal(line, &m); uErr != nil {
				return nil, err
			}
			out = append(out, m)
		}
		return out, nil
	}
	var slice []any
	switch v := raw.(type) {
	case []any:
		slice = v
	case map[string]any:
		// Only the legacy {"changes": [...]} wrapper is a list. Any other object (an agent wrapping its
		// items in {"proposals": [...]}, or writing one item without the list around it) is not an empty
		// list: read as one, ReviewProposals rewrote the file as [] and the 3-way merge took every item of
		// the base as deleted.
		ch, ok := v["changes"].([]any)
		if !ok {
			return nil, fmt.Errorf("expected JSON list or object with changes")
		}
		slice = ch
	default:
		return nil, fmt.Errorf("expected JSON list or object with changes")
	}
	out := make([]map[string]any, 0, len(slice))
	for _, item := range slice {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// FormatMinifiedJSONListOnePerLine formats items as a valid JSON array where each entry
// is minified on its own single line.
func FormatMinifiedJSONListOnePerLine(items []map[string]any) ([]byte, error) {
	if len(items) == 0 {
		return []byte("[]\n"), nil
	}
	var buf bytes.Buffer
	buf.WriteString("[\n")
	for i, item := range items {
		var lineBuf bytes.Buffer
		enc := json.NewEncoder(&lineBuf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(item); err != nil {
			return nil, err
		}
		line := bytes.TrimSuffix(lineBuf.Bytes(), []byte("\n"))
		buf.Write(line)
		if i < len(items)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("]\n")
	return buf.Bytes(), nil
}

// RotatedHistoryRelPaths returns existing rotated history files (".foreman-kit/agent_history.1.json", ...)
// sorted by rotation index ascending (1, 2, 3, ...).
func RotatedHistoryRelPaths(root string) []string {
	cfgDir := filepath.Join(root, ".foreman-kit")
	entries, err := os.ReadDir(cfgDir)
	if err != nil {
		return nil
	}
	type indexedFile struct {
		idx int
		rel string
	}
	var found []indexedFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		rel := ".foreman-kit/" + e.Name()
		m := RotatedHistoryFileRe.FindStringSubmatch(rel)
		if len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil && n >= 1 {
				found = append(found, indexedFile{idx: n, rel: rel})
			}
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].idx < found[j].idx })
	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, f.rel)
	}
	return out
}

// ReadAllHistory reads all history entries across rotated files (oldest to newest) and .foreman-kit/agent_history.json.
func ReadAllHistory(root string) ([]map[string]any, error) {
	rotated := RotatedHistoryRelPaths(root)
	var all []map[string]any
	seen := make(map[string]bool)
	// Read rotated files from oldest (highest N) to newest (1), then active .foreman-kit/agent_history.json
	for i := len(rotated) - 1; i >= 0; i-- {
		items, err := ReadJSONList(filepath.Join(root, filepath.FromSlash(rotated[i])))
		if err != nil {
			continue
		}
		for _, it := range items {
			name := asString(it["change_name"])
			if name != "" && !seen[name] {
				seen[name] = true
				all = append(all, it)
			}
		}
	}
	active, err := ReadJSONList(filepath.Join(root, filepath.FromSlash(HistoryFile)))
	if err != nil {
		return all, err
	}
	for _, it := range active {
		name := asString(it["change_name"])
		if name != "" && !seen[name] {
			seen[name] = true
			all = append(all, it)
		}
	}
	return all, nil
}

// WriteHistoryWithRotation writes items to .foreman-kit/agent_history.json.
// Whenever the pretty-printed file exceeds HistoryMaxLines (1000 lines), it rotates older items
// logrotate-style (agent_history.1.json -> agent_history.2.json, etc.) with each rotated file
// formatted minified (one change per line).
func WriteHistoryWithRotation(root string, items []map[string]any) ([]map[string]any, error) {
	if items == nil {
		items = []map[string]any{}
	}
	historyPath := filepath.Join(root, filepath.FromSlash(HistoryFile))
	if err := os.MkdirAll(filepath.Dir(historyPath), 0o755); err != nil {
		return nil, err
	}

	for len(items) > 1 {
		b, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return nil, err
		}
		b = append(b, '\n')
		if bytes.Count(b, []byte("\n")) <= HistoryMaxLines {
			break
		}

		// Find largest prefix items[:split] (1 <= split < len(items)) that fits within HistoryMaxLines
		split := len(items) - 1
		for split > 1 {
			prefixBytes, err := json.MarshalIndent(items[:split], "", "  ")
			if err == nil && bytes.Count(prefixBytes, []byte("\n"))+1 <= HistoryMaxLines {
				break
			}
			split--
		}

		toRotate := items[:split]
		items = items[split:]

		// Shift existing agent_history.<N>.json -> agent_history.<N+1>.json in descending order
		existing := RotatedHistoryRelPaths(root)
		for i := len(existing) - 1; i >= 0; i-- {
			m := RotatedHistoryFileRe.FindStringSubmatch(existing[i])
			if len(m) != 2 {
				continue
			}
			n, _ := strconv.Atoi(m[1])
			src := filepath.Join(root, filepath.FromSlash(existing[i]))
			dst := filepath.Join(root, ".foreman-kit", fmt.Sprintf("agent_history.%d.json", n+1))
			_ = os.Rename(src, dst)
		}

		// Write toRotate minified (one change per line) to .foreman-kit/agent_history.1.json
		minBytes, err := FormatMinifiedJSONListOnePerLine(toRotate)
		if err != nil {
			return nil, err
		}
		rot1Path := filepath.Join(root, ".foreman-kit", "agent_history.1.json")
		if err := writeTextAtomic(rot1Path, string(minBytes)); err != nil {
			return nil, err
		}
	}

	if err := WriteJSONList(historyPath, items); err != nil {
		return nil, err
	}
	return items, nil
}

// WriteJSONList atomically writes items as pretty-printed JSON to path.
func WriteJSONList(path string, items []map[string]any) error {
	if items == nil {
		items = []map[string]any{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(items); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeTextAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadProposals reads .foreman-kit/agent_proposals.json from root.
func ReadProposals(root string) ([]map[string]any, error) {
	return ReadJSONList(filepath.Join(root, filepath.FromSlash(ProposalsFile)))
}

// WriteProposals writes .foreman-kit/agent_proposals.json in root.
func WriteProposals(root string, items []map[string]any) error {
	return WriteJSONList(filepath.Join(root, filepath.FromSlash(ProposalsFile)), items)
}

// ArchivedChanges returns change names present in openspec/changes/archive/YYYY-MM-DD-<name>.
func ArchivedChanges(root string) []string {
	archDir := filepath.Join(root, "openspec", "changes", "archive")
	entries, err := os.ReadDir(archDir)
	if err != nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if m := ArchiveDirNameRe.FindStringSubmatch(e.Name()); len(m) == 2 {
			name := m[1]
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// IsChangeArchived reports whether changeName is in openspec/changes/archive/YYYY-MM-DD-<changeName>.
func IsChangeArchived(root, changeName string) bool {
	for _, c := range ArchivedChanges(root) {
		if c == changeName {
			return true
		}
	}
	return false
}

// ArchiveBacklog moves archived changes from .foreman-kit/agent_backlog.json into .foreman-kit/agent_history.json.
func ArchiveBacklog(root string, extraItem map[string]any, at string) (kept, history []map[string]any, moved []string, err error) {
	if at == "" {
		at = time.Now().UTC().Format(time.RFC3339)
	}
	backlogPath := filepath.Join(root, filepath.FromSlash(BacklogFile))
	historyPath := filepath.Join(root, filepath.FromSlash(HistoryFile))
	backlog, err := ReadJSONList(backlogPath)
	if err != nil {
		return nil, nil, nil, err
	}
	history, err = ReadJSONList(historyPath)
	if err != nil {
		return nil, nil, nil, err
	}
	archived := make(map[string]bool)
	for _, c := range ArchivedChanges(root) {
		archived[c] = true
	}

	var done []map[string]any
	for _, item := range backlog {
		chg := asString(item["change_name"])
		if archived[chg] {
			done = append(done, item)
		} else {
			kept = append(kept, item)
		}
	}
	if extraItem != nil {
		chg := asString(extraItem["change_name"])
		if ChangeNameRe.MatchString(chg) && archived[chg] {
			done = append(done, extraItem)
		}
	}

	histSeen := make(map[string]bool)
	for _, h := range history {
		histSeen[asString(h["change_name"])] = true
	}
	if allHist, err := ReadAllHistory(root); err == nil {
		for _, h := range allHist {
			histSeen[asString(h["change_name"])] = true
		}
	}

	added := 0
	movedSeen := make(map[string]bool)
	for _, entry := range done {
		chg := asString(entry["change_name"])
		if chg != "" && !movedSeen[chg] {
			movedSeen[chg] = true
			moved = append(moved, chg)
		}
		if histSeen[chg] {
			continue
		}
		histSeen[chg] = true
		cp := cloneMap(entry)
		if asString(cp["integrated_at"]) == "" {
			cp["integrated_at"] = at
		}
		history = append(history, cp)
		added++
	}

	if len(kept) != len(backlog) {
		if err := WriteJSONList(backlogPath, kept); err != nil {
			return nil, nil, nil, err
		}
	}
	if added > 0 || len(history) > 1 {
		rotatedActive, err := WriteHistoryWithRotation(root, history)
		if err != nil {
			return nil, nil, nil, err
		}
		history = rotatedActive
	}
	return kept, history, moved, nil
}

// ApplyPromptProposal applies a prompt proposal's edits to .foreman-kit/prompts/<prompt>.md,
// verifies placeholder constraints, and returns the applied metadata or an *ApplyError.
func ApplyPromptProposal(root string, item map[string]any) (map[string]any, error) {
	name := asString(item["prompt"])
	if _, ok := PromptPlaceholders[name]; !ok {
		return nil, &ApplyError{Msg: "prompt must be one of build, cycle, plan"}
	}
	rel := fmt.Sprintf("%s/%s.md", PromptsDir, name)
	file := PromptPath(root, name)
	if !fileExists(file) {
		return nil, &ApplyError{Msg: fmt.Sprintf("%s is missing", rel)}
	}

	rawEdits, ok := item["edits"].([]any)
	if !ok || len(rawEdits) == 0 {
		return nil, &ApplyError{Msg: `edits must be a list of {"old": <text in the prompt>, "new": <its replacement>}`}
	}
	type editPair struct {
		old string
		new string
	}
	var edits []editPair
	for _, re := range rawEdits {
		em, ok := re.(map[string]any)
		if !ok {
			return nil, &ApplyError{Msg: `edits must be a list of {"old": <text in the prompt>, "new": <its replacement>}`}
		}
		oldVal, okOld := em["old"].(string)
		newVal, okNew := em["new"].(string)
		if !okOld || oldVal == "" || !okNew {
			return nil, &ApplyError{Msg: `edits must be a list of {"old": <text in the prompt>, "new": <its replacement>}`}
		}
		edits = append(edits, editPair{old: oldVal, new: newVal})
	}

	beforeBytes, err := os.ReadFile(file)
	if err != nil {
		return nil, &ApplyError{Msg: err.Error()}
	}
	before := string(beforeBytes)
	after := before
	for i, ed := range edits {
		count := strings.Count(after, ed.old)
		if count != 1 {
			return nil, &ApplyError{Msg: fmt.Sprintf("edit %d: its old text occurs %d times in %s, not once (did the prompt change since?)", i+1, count, rel)}
		}
		after = strings.Replace(after, ed.old, ed.new, 1)
	}
	if after == before {
		return nil, &ApplyError{Msg: fmt.Sprintf("the edits leave %s as it is", rel)}
	}

	fromVer, _ := PromptVersion(root, name)
	if err := writeTextAtomic(file, after); err != nil {
		return nil, err
	}
	errs, _ := CheckPrompts(root)
	var promptErrs []string
	for _, e := range errs {
		if strings.HasPrefix(e, rel+":") {
			promptErrs = append(promptErrs, e)
		}
	}
	if len(promptErrs) > 0 {
		_ = writeTextAtomic(file, before)
		return nil, &ApplyError{Msg: fmt.Sprintf("the edited prompt fails its check: %s", strings.Join(promptErrs, "; "))}
	}
	toVer, _ := PromptVersion(root, name)
	return map[string]any{
		"at":     time.Now().UTC().Format(time.RFC3339),
		"prompt": rel,
		"from":   fromVer,
		"to":     toVer,
	}, nil
}

func buildProposalReviewState(root string, item map[string]any, items, backlog []map[string]any) map[string]any {
	roadmapRel := settings.String(root, settings.RoadmapFile)
	roadmapText := ""
	if roadmapRel != "" {
		if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(roadmapRel))); err == nil {
			roadmapText = string(b)
		}
	}
	var caps []string
	specsDir := filepath.Join(root, "openspec", "specs")
	if entries, err := os.ReadDir(specsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				caps = append(caps, e.Name())
			}
		}
	}
	if caps == nil {
		caps = []string{}
	}

	propCopy := make(map[string]any)
	for k, v := range item {
		if ProposalOwnFields[k] && k != "source" {
			continue
		}
		propCopy[k] = v
	}

	queued := make([]string, 0, len(backlog))
	for _, b := range backlog {
		queued = append(queued, asString(b["change_name"]))
	}
	var others []string
	for _, p := range items {
		if asString(p["change_name"]) == asString(item["change_name"]) {
			continue
		}
		others = append(others, fmt.Sprintf("%s (%s)", asString(p["change_name"]), asString(p["status"])))
	}
	if others == nil {
		others = []string{}
	}

	return map[string]any{
		"proposal":              propCopy,
		"roadmap":               roadmapText,
		"existing_capabilities": caps,
		"queued_changes":        queued,
		"other_proposals":       others,
	}
}

// ReviewProposals evaluates proposed items in .foreman-kit/agent_proposals.json, applies approved prompt proposals,
// moves approved change proposals to .foreman-kit/agent_backlog.json, and archives integrated items. An
// approved proposal is queued only while the queue has room (settings.QueueMaxOpen): one that does
// not fit stays approved, and the next review queues it once a change has left the queue.
func ReviewProposals(ctx context.Context, root string, st *store.Store, judge *evaluator.Judge, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	propPath := filepath.Join(root, filepath.FromSlash(ProposalsFile))
	backlogPath := filepath.Join(root, filepath.FromSlash(BacklogFile))
	historyPath := filepath.Join(root, filepath.FromSlash(HistoryFile))

	items, err := ReadJSONList(propPath)
	if err != nil {
		return err
	}
	backlog, err := ReadJSONList(backlogPath)
	if err != nil {
		return err
	}
	history, err := ReadJSONList(historyPath)
	if err != nil {
		return err
	}

	cycleSettings := settings.At(root)
	approvalMode := proposalsApproval(cycleSettings)
	promptApproval := cycleSettings.String(settings.PromptEditsApprovedBy)

	if judge == nil && approvalMode == settings.ApprovalEvaluator {
		judge, _ = evaluator.NewJudge(root, st)
	}

	counts := make(map[string]int)
	var countOrder []string
	incCount := func(k string) {
		if _, exists := counts[k]; !exists {
			countOrder = append(countOrder, k)
		}
		counts[k]++
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// How full the queue is before this review queues anything. When the open changes cannot be
	// counted, nothing holds a proposal back: that is what a review did before the queue had a cap.
	queue := queueState{MaxOpen: cycleSettings.Int(settings.QueueMaxOpen)}
	limited := true
	if queue.Open, err = openChanges(ctx, st, backlog); err != nil {
		fmt.Fprintf(out, "queue: the open changes cannot be counted (%v): approved proposals are not held to %s\n", err, queueMaxOpenKey)
		limited = false
	}
	var waiting []string

	for _, item := range items {
		chg := asString(item["change_name"])
		if !ChangeNameRe.MatchString(chg) {
			continue
		}
		if asString(item["kind"]) == "prompt" {
			if asString(item["status"]) == "proposed" && item["review"] == nil && promptApproval == settings.ApprovalNobody {
				item["status"] = "approved"
				item["review"] = map[string]any{"by": settings.ApprovalNobody, "at": now}
			}
			switch asString(item["status"]) {
			case "proposed":
				incCount("prompt edit(s) waiting for a human")
			case "approved":
				applied, err := ApplyPromptProposal(root, item)
				if err != nil {
					item["status"] = "rejected"
					rev, _ := item["review"].(map[string]any)
					if rev == nil {
						rev = make(map[string]any)
					}
					rev["apply_error"] = err.Error()
					item["review"] = rev
					if st != nil {
						errMsg := err.Error()
						if len(errMsg) > 300 {
							errMsg = errMsg[:300]
						}
						_ = st.EmitEvent(ctx, store.EventInput{
							Event:  "proposal.apply_failed",
							Change: chg,
							Payload: map[string]any{
								"error": errMsg,
							},
						})
					}
					incCount("prompt edit(s) rejected")
				} else {
					item["applied"] = applied
					item["status"] = "integrated"
					item["integrated_at"] = now
					if st != nil {
						_ = st.EmitEvent(ctx, store.EventInput{
							Event:  "proposal.applied",
							Change: chg,
							Payload: map[string]any{
								"prompt": applied["prompt"],
								"from":   applied["from"],
								"to":     applied["to"],
							},
						})
					}
					incCount("prompt edit(s) applied")
				}
			}
			continue
		}

		if asString(item["status"]) == "proposed" && item["review"] == nil {
			switch approvalMode {
			case settings.ApprovalNobody:
				item["status"] = "approved"
				item["review"] = map[string]any{"by": settings.ApprovalNobody, "at": now}
			case settings.ApprovalEvaluator:
				approved, reviewObj := askEvaluatorForProposal(ctx, judge, root, item, items, backlog, now)
				revStatus := asString(reviewObj["status"])
				if st != nil {
					_ = st.EmitEvent(ctx, store.EventInput{
						Event:  "proposal.reviewed",
						Change: chg,
						Payload: map[string]any{
							"status":   revStatus,
							"approved": approved,
						},
					})
				}
				if revStatus == "passed" || revStatus == "failed" || revStatus == "abstained" {
					item["review"] = reviewObj
					if approved {
						item["status"] = "approved"
					}
				} else {
					incCount(fmt.Sprintf("not reviewed (evaluator %s)", revStatus))
				}
			}
		}

		if asString(item["status"]) == "approved" {
			inBacklog := false
			for _, b := range backlog {
				if asString(b["change_name"]) == chg {
					inBacklog = true
					break
				}
			}
			inHistory := false
			for _, h := range history {
				if asString(h["change_name"]) == chg {
					inHistory = true
					break
				}
			}
			enters := !inBacklog && !inHistory && !IsChangeArchived(root, chg)
			if enters && limited && queue.Full() {
				// It stays approved: the next review queues it when there is room.
				waiting = append(waiting, chg)
				incCount("waiting for room in the queue")
				continue
			}
			if enters {
				bItem := make(map[string]any)
				for k, v := range item {
					if !ProposalOwnFields[k] {
						bItem[k] = v
					}
				}
				bItem["queued_at"] = now
				backlog = append(backlog, bItem)
				queue.Open++
			}
			item["status"] = "queued"
			item["queued_at"] = now
			by := "human"
			if rev, ok := item["review"].(map[string]any); ok && asString(rev["by"]) != "" {
				by = asString(rev["by"])
			}
			if st != nil {
				_ = st.EmitEvent(ctx, store.EventInput{
					Event:  "proposal.queued",
					Change: chg,
					Payload: map[string]any{
						"by": by,
					},
				})
			}
			incCount("queued")
		}

		if asString(item["status"]) == "queued" && IsChangeArchived(root, chg) {
			item["status"] = "integrated"
			item["integrated_at"] = now
		}
		if asString(item["status"]) == "proposed" {
			incCount("waiting for a human")
		}
	}

	if fileExists(propPath) || len(items) > 0 {
		if err := WriteJSONList(propPath, items); err != nil {
			return err
		}
	}
	if counts["queued"] > 0 {
		if err := WriteJSONList(backlogPath, backlog); err != nil {
			return err
		}
	}
	_, _, moved, err := ArchiveBacklog(root, nil, now)
	if err != nil {
		return err
	}
	if len(moved) > 0 {
		if _, exists := counts["archived"]; !exists {
			countOrder = append(countOrder, "archived")
		}
		counts["archived"] += len(moved)
	}

	summary := "nothing to review"
	if len(countOrder) > 0 {
		var parts []string
		for _, k := range countOrder {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
		summary = strings.Join(parts, ", ")
	}
	fmt.Fprintf(out, "proposals: %s (approval: %s, prompt edits: %s)\n", summary, approvalMode, promptApproval)
	if len(waiting) > 0 {
		fmt.Fprintf(out, "queue: %s: approved and waiting for room: %s\n", queue, strings.Join(waiting, ", "))
	}
	return nil
}

func askEvaluatorForProposal(ctx context.Context, judge *evaluator.Judge, root string, item map[string]any, items, backlog []map[string]any, now string) (bool, map[string]any) {
	if judge == nil {
		return false, map[string]any{"by": "evaluator", "at": now, "status": "unavailable"}
	}
	state := buildProposalReviewState(root, item, items, backlog)
	verdict, err := judge.Ask(ctx, "proposal_review", state, asString(item["change_name"]))
	if err != nil && verdict == nil {
		msg := err.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return false, map[string]any{"by": "evaluator", "at": now, "status": "error", "detail": msg}
	}
	rev := map[string]any{
		"by":     "evaluator",
		"at":     now,
		"mode":   verdict.Mode,
		"status": verdict.Status,
	}
	if verdict.Model != "" {
		rev["model"] = verdict.Model
	}
	if len(verdict.Answers) > 0 {
		rev["answers"] = evaluator.SummarizeAnswers(verdict.Answers)
	}
	if len(verdict.Failed) > 0 {
		failedStrs := make([]string, 0, len(verdict.Failed))
		for _, c := range verdict.Failed {
			failedStrs = append(failedStrs, fmt.Sprintf("%s (measured %v)", c.Condition, c.Actual))
		}
		rev["failed"] = failedStrs
	}
	approved := verdict.Status == "passed" && verdict.Enforce()
	return approved, rev
}

// DecideProposal marks a proposal as approved or rejected by a human.
func DecideProposal(root, name, status string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	propPath := filepath.Join(root, filepath.FromSlash(ProposalsFile))
	items, err := ReadJSONList(propPath)
	if err != nil {
		return err
	}
	var target map[string]any
	for _, it := range items {
		if asString(it["change_name"]) == name {
			target = it
			break
		}
	}
	if target == nil {
		return fmt.Errorf("no proposal named %s", name)
	}
	curStatus := asString(target["status"])
	if curStatus != "proposed" && curStatus != "approved" {
		return fmt.Errorf("%s is %s, not proposed", name, curStatus)
	}
	target["status"] = status
	rev, _ := target["review"].(map[string]any)
	if rev == nil {
		rev = make(map[string]any)
	}
	rev["decided_by"] = "human"
	rev["decided_at"] = time.Now().UTC().Format(time.RFC3339)
	target["review"] = rev

	if err := WriteJSONList(propPath, items); err != nil {
		return err
	}
	nxt := fmt.Sprintf("the next cycle queues it while the queue has room (%s)", queueMaxOpenKey)
	if asString(target["kind"]) == "prompt" {
		nxt = fmt.Sprintf("the next cycle applies it to %s/%s.md", PromptsDir, asString(target["prompt"]))
	}
	suffix := ""
	if status == "approved" {
		suffix = "; " + nxt
	}
	fmt.Fprintf(out, "proposals: %s %s%s\n", name, status, suffix)
	return nil
}

// ApproveProposal approves a proposal by name.
func ApproveProposal(root, name string, out io.Writer) error {
	return DecideProposal(root, name, "approved", out)
}

// RejectProposal rejects a proposal by name.
func RejectProposal(root, name string, out io.Writer) error {
	return DecideProposal(root, name, "rejected", out)
}

// ListProposals writes a formatted table of proposals to out.
func ListProposals(root string, out io.Writer) error {
	items, err := ReadProposals(root)
	if err != nil {
		return err
	}
	for _, i := range items {
		reviewStr := ""
		if rev, ok := i["review"].(map[string]any); ok && asString(rev["status"]) != "" {
			reviewStr = " evaluator=" + asString(rev["status"])
		}
		kindStr := ""
		if asString(i["kind"]) == "prompt" {
			kindStr = " prompt=" + asString(i["prompt"])
		}
		fmt.Fprintf(out, "%-11s %-40s %s%s%s\n",
			asString(i["status"]),
			asString(i["change_name"]),
			asString(i["source"]),
			kindStr,
			reviewStr,
		)
	}
	return nil
}

// ShowProposal writes detailed information (and prompt edit diff if applicable) for one proposal.
func ShowProposal(root, name string, out io.Writer) error {
	items, err := ReadProposals(root)
	if err != nil {
		return err
	}
	var item map[string]any
	for _, it := range items {
		if asString(it["change_name"]) == name {
			item = it
			break
		}
	}
	if item == nil {
		return fmt.Errorf("no proposal named %s", name)
	}
	kindLabel := "change"
	if asString(item["kind"]) == "prompt" {
		kindLabel = "prompt " + asString(item["prompt"])
	}
	sourceLabel := asString(item["source"])
	if sourceLabel == "" {
		sourceLabel = "unknown"
	}
	fmt.Fprintf(out, "%s (%s, %s, source %s)\n", asString(item["change_name"]), asString(item["status"]), kindLabel, sourceLabel)
	for _, k := range []string{"description", "evidence"} {
		if v := asString(item[k]); v != "" {
			fmt.Fprintf(out, "%s: %s\n", k, v)
		}
	}
	if rev, ok := item["review"].(map[string]any); ok {
		if ae := asString(rev["apply_error"]); ae != "" {
			fmt.Fprintf(out, "apply_error: %s\n", ae)
		}
	}
	if item["applied"] != nil {
		b, _ := json.Marshal(item["applied"])
		fmt.Fprintf(out, "applied: %s\n", string(b))
	}
	if edits, ok := item["edits"].([]any); ok {
		for idx, rawEdit := range edits {
			em, _ := rawEdit.(map[string]any)
			fmt.Fprintf(out, "--- edit %d of %s/%s.md\n", idx+1, PromptsDir, asString(item["prompt"]))
			for _, line := range strings.Split(strings.TrimSuffix(asString(em["old"]), "\n"), "\n") {
				fmt.Fprintf(out, "-%s\n", strings.TrimSuffix(line, "\r"))
			}
			for _, line := range strings.Split(strings.TrimSuffix(asString(em["new"]), "\n"), "\n") {
				fmt.Fprintf(out, "+%s\n", strings.TrimSuffix(line, "\r"))
			}
		}
	}
	return nil
}

// MergeJSONList3Way performs an in-memory 3-way merge of JSON lists keyed by change_name
// (used for .foreman-kit/agent_backlog.json, .foreman-kit/agent_history.json, .foreman-kit/agent_proposals.json).
func MergeJSONList3Way(relPath string, baseText, localText, targetText *string, archivedChanges []string) ([]map[string]any, bool) {
	parseOpt := func(s *string) ([]map[string]any, bool) {
		if s == nil {
			return []map[string]any{}, true
		}
		list, err := parseJSONListBytes([]byte(*s))
		if err != nil {
			return nil, false
		}
		return list, true
	}
	base, ok1 := parseOpt(baseText)
	local, ok2 := parseOpt(localText)
	remote, ok3 := parseOpt(targetText)
	if !ok1 || !ok2 || !ok3 {
		return nil, false
	}

	keyOf := func(m map[string]any) string {
		return asString(m["change_name"])
	}

	baseBy := make(map[string]map[string]any, len(base))
	for _, it := range base {
		baseBy[keyOf(it)] = it
	}
	localKeys := make(map[string]bool, len(local))
	for _, it := range local {
		localKeys[keyOf(it)] = true
	}

	var merged []map[string]any
	for _, it := range remote {
		k := keyOf(it)
		if _, inBase := baseBy[k]; inBase && !localKeys[k] {
			continue
		}
		merged = append(merged, cloneMap(it))
	}

	for _, item := range local {
		k := keyOf(item)
		if baseItem, inBase := baseBy[k]; inBase && reflect.DeepEqual(baseItem, item) {
			continue
		}
		at := -1
		for idx, m := range merged {
			if keyOf(m) == k {
				at = idx
				break
			}
		}
		if at == -1 {
			merged = append(merged, cloneMap(item))
		} else if !reflect.DeepEqual(merged[at], baseBy[k]) && relPath == ProposalsFile {
			merged[at] = newerProposal(merged[at], item)
		} else {
			merged[at] = cloneMap(item)
		}
	}

	if relPath == ProposalsFile {
		archSet := make(map[string]bool, len(archivedChanges))
		for _, c := range archivedChanges {
			archSet[c] = true
		}
		now := time.Now().UTC().Format(time.RFC3339)
		for _, item := range merged {
			if asString(item["status"]) == "queued" && archSet[asString(item["change_name"])] {
				item["status"] = "integrated"
				if asString(item["integrated_at"]) == "" {
					item["integrated_at"] = now
				}
			}
		}
	}
	if merged == nil {
		merged = []map[string]any{}
	}
	return merged, true
}

func newerProposal(remote, local map[string]any) map[string]any {
	lRank := ProposalStatusRank[asString(local["status"])]
	rRank := ProposalStatusRank[asString(remote["status"])]
	_, lHasRev := local["review"]
	_, rHasRev := remote["review"]

	out := make(map[string]any)
	if lRank > rRank || (lRank == rRank && lHasRev && !rHasRev) {
		for k, v := range remote {
			out[k] = v
		}
		for k, v := range local {
			out[k] = v
		}
	} else {
		for k, v := range local {
			out[k] = v
		}
		for k, v := range remote {
			out[k] = v
		}
	}
	return out
}

// MergeText3Way performs an in-memory 3-way union merge of text files (keeping lines added by both sides).
func MergeText3Way(baseText, localText, targetText string) string {
	if localText == baseText {
		return targetText
	}
	if targetText == baseText || targetText == "" {
		return localText
	}
	if targetText == localText {
		return targetText
	}

	// Try git merge-file --union -p if available, or pure Go line-level union merge.
	tmpDir, err := os.MkdirTemp("", "openspec-merge-*")
	if err == nil {
		defer os.RemoveAll(tmpDir)
		remFile := filepath.Join(tmpDir, "remote")
		baseFile := filepath.Join(tmpDir, "base")
		locFile := filepath.Join(tmpDir, "local")
		_ = os.WriteFile(remFile, []byte(targetText), 0o644)
		_ = os.WriteFile(baseFile, []byte(baseText), 0o644)
		_ = os.WriteFile(locFile, []byte(localText), 0o644)
		cmd := exec.Command("git", "merge-file", "--union", "-p", remFile, baseFile, locFile)
		if out, err := cmd.Output(); err == nil {
			return string(out)
		}
	}

	// Pure Go fallback union line merge
	baseLines := splitLinesKeepNewline(baseText)
	targetLines := splitLinesKeepNewline(targetText)
	localLines := splitLinesKeepNewline(localText)

	baseSet := make(map[string]bool, len(baseLines))
	for _, l := range baseLines {
		baseSet[l] = true
	}
	targetSet := make(map[string]bool, len(targetLines))
	for _, l := range targetLines {
		targetSet[l] = true
	}

	var out []string
	out = append(out, targetLines...)
	for _, l := range localLines {
		if !baseSet[l] && !targetSet[l] {
			out = append(out, l)
		}
	}
	return strings.Join(out, "")
}

func splitLinesKeepNewline(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Reconcile3Way reconciles changes from baseBlobs -> localBlobs onto the files in root. A nil text
// is a file that is not there, so the caller hands nil only for a file it knows is absent
// (blobAt, fileOrAbsent). Every file it lands on is read before the first one is written: one that
// cannot be read stops it with the checkout as it was.
//
// A file of the memory both sides changed is merged by agent-memory, section by section; where it
// cannot (both sides added to one section, as two sessions that each record a lesson do) the lines of
// both are kept, as for any other text. index.md is not merged: agent-memory derives it from the
// other files, and writes it again once they are in place (memory.Normalize), which also leaves the
// memory in the form the commit that follows has to record. ctx is the one those calls of
// agent-memory run under: a caller that must end what it began passes the context that lets it.
func Reconcile3Way(ctx context.Context, root string, baseBlobs, localBlobs map[string]*string) error {
	targets := make(map[string]*string, len(localBlobs))
	for rel, localPtr := range localBlobs {
		if ptrEqual(localPtr, baseBlobs[rel]) {
			continue
		}
		text, err := fileOrAbsent(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		targets[rel] = text
	}
	archived := ArchivedChanges(root)
	touchedMemory := false
	for rel, localPtr := range localBlobs {
		basePtr := baseBlobs[rel]
		if ptrEqual(localPtr, basePtr) {
			continue
		}
		targetPath := filepath.Join(root, filepath.FromSlash(rel))
		targetPtr := targets[rel]

		if JSONListPaths[rel] {
			if merged, ok := MergeJSONList3Way(rel, basePtr, localPtr, targetPtr, archived); ok {
				if rel == HistoryFile {
					if _, err := WriteHistoryWithRotation(root, merged); err != nil {
						return err
					}
				} else if err := WriteJSONList(targetPath, merged); err != nil {
					return err
				}
				continue
			}
		}

		touchedMemory = touchedMemory || memory.Inside(rel)

		// Text merge
		if localPtr == nil {
			if ptrEqual(targetPtr, basePtr) {
				_ = os.Remove(targetPath)
			}
			continue
		}
		if targetPtr == nil || ptrEqual(targetPtr, basePtr) {
			if err := writeTextAtomic(targetPath, *localPtr); err != nil {
				return err
			}
			continue
		}
		if *targetPtr == *localPtr {
			continue
		}
		baseStr := ""
		if basePtr != nil {
			baseStr = *basePtr
		}
		if rel == memory.IndexFile {
			// Written again below from the files it describes.
			continue
		}
		mergedText := ""
		if memory.Inside(rel) && strings.HasSuffix(rel, ".md") {
			bySection, clean, err := memory.Merge3Way(ctx, rel, baseStr, *targetPtr, *localPtr)
			if err != nil {
				return fmt.Errorf("cannot merge %s: %w", rel, err)
			}
			if clean {
				mergedText = bySection
			}
		}
		if mergedText == "" {
			mergedText = MergeText3Way(baseStr, *localPtr, *targetPtr)
		}
		if err := writeTextAtomic(targetPath, mergedText); err != nil {
			return err
		}
	}
	if touchedMemory {
		if err := memory.Normalize(ctx, root); err != nil {
			return fmt.Errorf("cannot bring %s to the form it is recorded in: %w", memory.Dir, err)
		}
	}
	_, _, _, _ = ArchiveBacklog(root, nil, "")
	return nil
}

// blobAt returns the text of rel as the commit rev of the checkout at dir records it, and nil when
// that commit does not have the file. A read that fails is an error, never "it does not have it":
// taken for an absent file, a backlog git could not load made the reconcile drop every item of the
// queue, a memory file was deleted, and the cycle published both.
func blobAt(ctx context.Context, dir, rev, rel string) (*string, error) {
	content, found, err := gitx.BlobAt(ctx, dir, rev, rel)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s as %s records it: %w", rel, rev, err)
	}
	if !found {
		return nil, nil
	}
	text := string(content)
	return &text, nil
}

// RebaseProposals rebases the session's record commit onto origin/<baseBranch>, reconciling
// .agent-memory/, .foreman-kit/agent_backlog.json, .foreman-kit/agent_history.json, .foreman-kit/agent_proposals.json,
// and .foreman-kit/prompts/* from what the record commit and the commit before it hold. A file of either
// that cannot be read stops it before anything is touched: the record commit stays.
//
// It rewrites the checkout in several steps: the reset onto the remote, the reconciled files, the
// commit. Once it begins it ends, even when ctx ends meanwhile (gitx.ToCompletion): told to stop after
// the reset, the record commit would be left behind HEAD with nothing in its place, and the reset that
// puts it back would be refused like every other command. A caller already told to stop begins
// nothing.
func RebaseProposals(ctx context.Context, root, baseBranch string, out io.Writer) error {
	if baseBranch == "" {
		var err error
		if baseBranch, err = RequireBaseBranch(root); err != nil {
			return err
		}
	}
	remote := settings.RemoteName(root)
	remoteRef := remote + "/" + baseBranch

	repo, err := gitx.Open(root)
	if err != nil {
		return err
	}
	if err := repo.Fetch(ctx, remote, baseBranch); err != nil {
		return fmt.Errorf("git fetch %s failed: %w", baseBranch, err)
	}

	if isAncestor(ctx, root, "HEAD", remoteRef) {
		_, err := gitx.RunGitToCompletion(ctx, root, "merge", "--ff-only", "--quiet", remoteRef)
		return err
	}
	if !isAncestor(ctx, root, "HEAD~1", remoteRef) {
		return fmt.Errorf("HEAD is not one record commit on top of %s; left as it is", remoteRef)
	}

	headSHA, err := repo.RevParse("HEAD")
	if err != nil {
		return err
	}
	msgOut, err := gitx.RunGit(ctx, root, "log", "-1", "--format=%B", "HEAD")
	if err != nil {
		return err
	}
	message := strings.TrimSpace(msgOut)

	changed, err := repo.ChangedFiles("HEAD~1", "HEAD")
	if err != nil {
		return err
	}
	baseBlobs := make(map[string]*string)
	localBlobs := make(map[string]*string)
	for _, rel := range changed {
		if !isRecordedPath(rel) {
			continue
		}
		if baseBlobs[rel], err = blobAt(ctx, root, headSHA+"~1", rel); err != nil {
			return err
		}
		if localBlobs[rel], err = blobAt(ctx, root, headSHA, rel); err != nil {
			return err
		}
	}

	// From the reset to the commit the record is in no commit HEAD reaches: none of it is stopped
	// with the caller, and neither is the reset that puts the record commit back when a step fails.
	if err := ctx.Err(); err != nil {
		return err
	}
	apply, done := gitx.ToCompletion(ctx)
	defer done()
	rollback := func() {
		_, _ = gitx.RunGit(apply, root, "reset", "--hard", "--quiet", headSHA)
	}

	if _, err := gitx.RunGit(apply, root, "reset", "--hard", "--quiet", remoteRef); err != nil {
		rollback()
		return err
	}
	if err := Reconcile3Way(apply, root, baseBlobs, localBlobs); err != nil {
		rollback()
		return err
	}

	var stage []string
	for _, p := range append(append([]string{}, RecordedPaths...), RotatedHistoryRelPaths(root)...) {
		if exists(filepath.Join(root, filepath.FromSlash(p))) {
			stage = append(stage, p)
			continue
		}
		tracked, err := isTrackedInGit(apply, root, p)
		if err != nil {
			rollback()
			return err
		}
		if tracked {
			stage = append(stage, p)
		}
	}
	if len(stage) > 0 {
		args := append([]string{"add", "-A", "--"}, stage...)
		if _, err := gitx.RunGit(apply, root, args...); err != nil {
			rollback()
			return err
		}
	}
	if _, err := gitx.RunGit(apply, root, "diff", "--cached", "--quiet"); err != nil {
		if _, err := gitx.RunGit(apply, root, "commit", "--quiet", "-m", message); err != nil {
			rollback()
			return err
		}
	}
	return nil
}

// CarryProposals merges uncommitted scope edits from srcRoot into dstRoot using 3-way reconciliation.
// When the commit srcRoot stands on, or one of its edited files, cannot be read, nothing is carried.
func CarryProposals(ctx context.Context, dstRoot, srcRoot string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	absSrc, err := filepath.Abs(srcRoot)
	if err != nil {
		return err
	}

	names, err := uncommittedScopeFiles(ctx, absSrc)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}

	baseBlobs := make(map[string]*string, len(names))
	localBlobs := make(map[string]*string, len(names))
	for _, rel := range names {
		if baseBlobs[rel], err = blobAt(ctx, absSrc, "HEAD", rel); err != nil {
			return err
		}
		if localBlobs[rel], err = fileOrAbsent(filepath.Join(absSrc, filepath.FromSlash(rel))); err != nil {
			return err
		}
	}

	if err := Reconcile3Way(ctx, dstRoot, baseBlobs, localBlobs); err != nil {
		return err
	}
	fmt.Fprintf(out, "proposals: carried %s from %s\n", strings.Join(names, ", "), absSrc)
	return nil
}

// IsAllowedScopePath reports whether relPath is inside the allowed session write scope
// (.agent-memory/**, .foreman-kit/agent_backlog.json, .foreman-kit/agent_history.json, .foreman-kit/agent_proposals.json).
func IsAllowedScopePath(relPath string) bool {
	relPath = filepath.ToSlash(strings.TrimPrefix(relPath, "./"))
	switch relPath {
	case ".foreman-kit/agent_backlog.json",
		".foreman-kit/agent_history.json",
		".foreman-kit/agent_proposals.json":
		return true
	}
	if RotatedHistoryFileRe.MatchString(relPath) {
		return true
	}
	return memory.Inside(relPath)
}

func isRecordedPath(relPath string) bool {
	if IsAllowedScopePath(relPath) {
		return true
	}
	relPath = filepath.ToSlash(strings.TrimPrefix(relPath, "./"))
	return relPath == ".foreman-kit/prompts" || strings.HasPrefix(relPath, ".foreman-kit/prompts/")
}

// commitsOutsideScope lists the paths outside the recorded scope that the commits a push of HEAD would
// publish (merge-base(HEAD, remoteRef)..HEAD) change. ScopeGuard only inspects uncommitted changes.
func commitsOutsideScope(ctx context.Context, dir, remoteRef string) ([]string, error) {
	mbOut, err := gitx.RunGit(ctx, dir, "merge-base", "HEAD", remoteRef)
	if err != nil {
		return nil, fmt.Errorf("cannot find the merge-base of HEAD and %s: %w", remoteRef, err)
	}
	diffOut, err := gitx.RunGit(ctx, dir, "diff", "--name-only", "-z", "--no-renames", strings.TrimSpace(mbOut), "HEAD", "--")
	if err != nil {
		return nil, err
	}
	var outside []string
	for _, p := range strings.Split(diffOut, "\x00") {
		if p != "" && !isRecordedPath(p) {
			outside = append(outside, p)
		}
	}
	return outside, nil
}

// refuseUnscopedCommits pauses the workspace and returns an error when HEAD carries commits that change
// paths outside the recorded scope, which a push of HEAD would publish to the base branch (for example an
// agent's own commit, or the operator's unpushed work when recording in the main checkout). It is a no-op
// when remoteRef does not resolve.
func refuseUnscopedCommits(ctx context.Context, dir, remoteRef, who string, st *store.Store, out io.Writer) error {
	if _, err := gitx.RunGit(ctx, dir, "rev-parse", "--verify", "--quiet", remoteRef); err != nil {
		return nil
	}
	outside, err := commitsOutsideScope(ctx, dir, remoteRef)
	if err == nil && len(outside) == 0 {
		return nil
	}
	detail := "cannot verify the commits HEAD carries"
	if err == nil {
		detail = "HEAD carries commits outside the recorded scope: " + strings.Join(outside, ", ")
	}
	fmt.Fprintf(out, "%s: %s; nothing published, pausing for a human\n", who, detail)
	if st != nil {
		_, _ = st.Pause(ctx, "scope violation in "+who, "cli", "")
	}
	return fmt.Errorf("%s: %s", who, detail)
}

// ScopeGuard verifies that only allowed scope files were modified in dir.
// If any other file was modified or added, it pauses the workspace via st.SetPaused and returns an error.
func ScopeGuard(ctx context.Context, dir, who string, st *store.Store, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	raw, err := gitx.RunGit(ctx, dir, "status", "--porcelain", "-uall", "--ignore-submodules=all")
	if err != nil {
		return err
	}
	var outside []string
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		pathPart := strings.TrimSpace(line[3:])
		// Handle renames "R  old -> new"
		if idx := strings.Index(pathPart, " -> "); idx >= 0 {
			oldP := strings.Trim(pathPart[:idx], `"`)
			newP := strings.Trim(pathPart[idx+4:], `"`)
			if !IsAllowedScopePath(oldP) || !IsAllowedScopePath(newP) {
				outside = append(outside, line)
			}
			continue
		}
		pathPart = strings.Trim(pathPart, `"`)
		if pathPart == "log" || pathPart == "log/" || pathPart == "log/.keep" || strings.HasPrefix(pathPart, "log/") {
			continue
		}
		if !IsAllowedScopePath(pathPart) {
			outside = append(outside, line)
		}
	}
	if len(outside) == 0 {
		return nil
	}
	outsideText := strings.Join(outside, "\n")
	fmt.Fprintf(out, "%s: the agent wrote outside its scope; nothing recorded, pausing for a human:\n%s\n", who, outsideText)
	reason := "scope violation in " + who
	if st != nil {
		_, _ = st.Pause(ctx, reason, "cli", "")
	}
	// Without a store, fall back to the PAUSE file in the workspace runtime directory.
	if st == nil {
		_ = os.WriteFile(store.RuntimePath(dir, "PAUSE"), []byte(reason+"\n"), 0o644)
	}
	return fmt.Errorf("%s: wrote outside scope: %s", who, outsideText)
}

// recordLockWait is how long a command waits for the lock of the records before it gives up, when
// its caller names no other limit. Nobody has a reason to change it: it is not a setting.
const recordLockWait = 30 * time.Minute

// WithRecordLock serializes checkout sync and record commits using the SQLite "record" lock in st.
func WithRecordLock(ctx context.Context, st *store.Store, ttl time.Duration, fn func() error) error {
	if st == nil {
		return fn()
	}
	if ttl <= 0 {
		ttl = recordLockWait
	}
	pid := os.Getpid()
	deadline := time.Now().Add(ttl)
	for {
		_, err := st.AcquireLock(ctx, "record", store.LockOptions{
			OwnerType: "record",
			PID:       pid,
			TTL:       ttl,
		})
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrLockHeld) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("record lock: held for %v; not running", ttl)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer func() {
		_ = st.ReleaseLock(context.Background(), "record", pid)
	}()
	return fn()
}

// RecordSession commits changes in RecordedPaths and pushes them to origin/<baseBranch> (with 3-way rebase fallback).
// If committing or pushing fails, it saves the commit under refs/openspec/unpublished/<worktree>-<stamp> using gitx.SaveUnpublishedCommit.
func RecordSession(ctx context.Context, dir, baseBranch, message string, st *store.Store, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if baseBranch == "" {
		var err error
		if baseBranch, err = RequireBaseBranch(dir); err != nil {
			return err
		}
	}
	remote := settings.RemoteName(dir)
	// The push below publishes every commit HEAD carries, not only the record: refuse commits that
	// change anything outside the recorded scope.
	if err := refuseUnscopedCommits(ctx, dir, remote+"/"+baseBranch, "record", st, out); err != nil {
		return err
	}

	// The memory is committed in the form agent-memory writes it in (memory.Normalize): a checkout
	// that receives that commit is then left as it is by every search an agent runs there. A session
	// whose memory cannot be brought to that form still records what it wrote, and says so.
	if err := memory.Normalize(ctx, dir); err != nil {
		fmt.Fprintf(out, "record: %v; the memory is recorded as the session left it\n", err)
	}
	if staged := memory.Staged(dir); len(staged) > 0 {
		fmt.Fprintf(out, "record: %d change(s) to the memory were staged in %s and never applied, so they are not recorded: %s (`%s review` there lists them)\n",
			len(staged), dir, strings.Join(staged, ", "), memory.Binary)
	}

	statusPaths := append(append([]string{}, RecordedPaths...), RotatedHistoryRelPaths(dir)...)
	statusArgs := append([]string{"status", "--porcelain", "-uall", "--"}, statusPaths...)
	statusOut, err := gitx.RunGit(ctx, dir, statusArgs...)
	if err == nil && strings.TrimSpace(statusOut) == "" {
		fmt.Fprintln(out, "record: nothing to record")
		return nil
	}

	repo, err := gitx.Open(dir)
	if err != nil {
		return err
	}

	changedRelPaths := collectChangedRecordedFiles(ctx, dir)
	stamp := time.Now().UTC().Format("20060102T150405Z")
	wtBase := filepath.Base(dir)

	var stage []string
	for _, p := range statusPaths {
		if exists(filepath.Join(dir, filepath.FromSlash(p))) {
			stage = append(stage, p)
			continue
		}
		tracked, err := isTrackedInGit(ctx, dir, p)
		if err != nil {
			return err
		}
		if tracked {
			stage = append(stage, p)
		}
	}

	committed := false
	if len(stage) > 0 {
		addArgs := append([]string{"add", "-A", "--"}, stage...)
		if _, err := gitx.RunGit(ctx, dir, addArgs...); err == nil {
			if _, err := gitx.RunGit(ctx, dir, "commit", "--quiet", "-m", message); err == nil {
				committed = true
			}
		}
	}

	if !committed {
		refName := fmt.Sprintf("refs/openspec/unpublished/%s-%s", wtBase, stamp)
		sha, saveErr := repo.SaveUnpublishedCommit(refName, message, changedRelPaths)
		if saveErr != nil {
			return fmt.Errorf("record: commit failed and SaveUnpublishedCommit failed: %w", saveErr)
		}
		fmt.Fprintf(out, "record: git refused the commit; kept as %s (%s)\n", refName, sha[:minInt(8, len(sha))])
		return fmt.Errorf("record: git refused the commit; kept as %s", refName)
	}

	for attempt := 1; attempt <= 3; attempt++ {
		if err := repo.Push(ctx, remote, "HEAD:refs/heads/"+baseBranch); err == nil {
			_, _ = gitx.RunGit(ctx, dir, "submodule", "update", "--init", "--quiet")
			return nil
		}
		if rebErr := RebaseProposals(ctx, dir, baseBranch, out); rebErr == nil {
			if err := repo.Push(ctx, remote, "HEAD:refs/heads/"+baseBranch); err == nil {
				_, _ = gitx.RunGit(ctx, dir, "submodule", "update", "--init", "--quiet")
				return nil
			}
		}
	}

	headSHA, _ := repo.RevParse("HEAD")
	shortSHA := headSHA
	if len(shortSHA) > 8 {
		shortSHA = shortSHA[:8]
	}
	refName := fmt.Sprintf("refs/openspec/unpublished/%s-%s-%s", wtBase, stamp, shortSHA)
	if _, refErr := gitx.RunGit(ctx, dir, "update-ref", refName, "HEAD"); refErr == nil {
		fmt.Fprintf(out, "record: not published; kept in %s for the next session to publish (and as %s)\n", dir, refName)
	} else if _, saveErr := repo.SaveUnpublishedCommit(refName, message, changedRelPaths); saveErr == nil {
		fmt.Fprintf(out, "record: not published; kept as %s\n", refName)
	} else {
		fmt.Fprintf(out, "record: not published; kept in %s for the next session to publish\n", dir)
	}
	return fmt.Errorf("record: could not push to %s/%s", remote, baseBranch)
}

func collectChangedRecordedFiles(ctx context.Context, dir string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, p := range RecordedPaths {
		full := filepath.Join(dir, filepath.FromSlash(p))
		info, err := os.Stat(full)
		if err == nil {
			if info.IsDir() {
				_ = filepath.WalkDir(full, func(path string, d os.DirEntry, err error) error {
					if err == nil && !d.IsDir() {
						if rel, err := filepath.Rel(dir, path); err == nil {
							rel = filepath.ToSlash(rel)
							if !seen[rel] {
								seen[rel] = true
								out = append(out, rel)
							}
						}
					}
					return nil
				})
			} else {
				rel := filepath.ToSlash(p)
				if !seen[rel] {
					seen[rel] = true
					out = append(out, rel)
				}
			}
		} else if os.IsNotExist(err) {
			rel := filepath.ToSlash(p)
			if !seen[rel] {
				seen[rel] = true
				out = append(out, rel)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ScopeEdits returns a deterministic snapshot of uncommitted scope files and their blob hashes in dir.
func ScopeEdits(ctx context.Context, dir string) (string, error) {
	files, err := uncommittedScopeFiles(ctx, dir)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, f := range files {
		full := filepath.Join(dir, filepath.FromSlash(f))
		b, err := os.ReadFile(full)
		hashStr := "deleted"
		if err == nil {
			sum := sha256.Sum256(b)
			hashStr = hex.EncodeToString(sum[:])
		}
		lines = append(lines, fmt.Sprintf("%s %s", hashStr, f))
	}
	return strings.Join(lines, "\n"), nil
}

// ReleaseScopeEdits discards uncommitted scope edits in root if they match beforeEdits.
func ReleaseScopeEdits(ctx context.Context, root, beforeEdits string) error {
	if strings.TrimSpace(beforeEdits) == "" {
		return nil
	}
	cur, err := ScopeEdits(ctx, root)
	if err != nil || cur != beforeEdits {
		return nil
	}
	for _, p := range ScopePaths {
		_, _ = gitx.RunGit(ctx, root, "checkout", "-q", "HEAD", "--", p)
		_, _ = gitx.RunGit(ctx, root, "clean", "-fdq", "--", p)
	}
	return nil
}

// PublishLeftoverRecord publishes a leftover record commit in wt before resetting the worktree,
// or saves it under refs/openspec/unpublished/ if it cannot be published.
func PublishLeftoverRecord(ctx context.Context, wt, baseRef, baseBranch string, out io.Writer) bool {
	if out == nil {
		out = io.Discard
	}
	if _, err := gitx.RunGit(ctx, wt, "rev-parse", "-q", "--verify", "HEAD~1"); err != nil {
		return false
	}
	if isAncestor(ctx, wt, "HEAD", baseRef) {
		return false
	}
	subj, err := gitx.RunGit(ctx, wt, "log", "-1", "--format=%s")
	if err != nil || !strings.HasPrefix(strings.TrimSpace(subj), "chore(memory): ") {
		return false
	}
	repo, err := gitx.Open(wt)
	if err != nil {
		return false
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	headSHA, _ := repo.RevParse("HEAD")
	shortSHA := headSHA
	if len(shortSHA) > 8 {
		shortSHA = shortSHA[:8]
	}

	if !isAncestor(ctx, wt, "HEAD~1", baseRef) {
		refName := fmt.Sprintf("refs/openspec/unpublished/%s-%s-%s", filepath.Base(wt), stamp, shortSHA)
		_, _ = gitx.RunGit(ctx, wt, "update-ref", refName, "HEAD")
		fmt.Fprintf(out, "the record an earlier session left in %s sits on a base origin no longer has; kept as %s\n", wt, refName)
		return false
	}
	// A "chore(memory): " subject proves nothing about what the commit changes.
	if outside, err := commitsOutsideScope(ctx, wt, baseRef); err != nil || len(outside) > 0 {
		refName := fmt.Sprintf("refs/openspec/unpublished/%s-%s-%s", filepath.Base(wt), stamp, shortSHA)
		_, _ = gitx.RunGit(ctx, wt, "update-ref", refName, "HEAD")
		fmt.Fprintf(out, "the record an earlier session left in %s changes paths outside the recorded scope (%s); kept as %s, not published\n",
			wt, strings.Join(outside, ", "), refName)
		return false
	}

	remote := settings.RemoteName(wt)
	if repo.Push(ctx, remote, "HEAD:refs/heads/"+baseBranch) == nil {
		fmt.Fprintf(out, "published the record an earlier session left in %s\n", wt)
		return true
	}
	if RebaseProposals(ctx, wt, baseBranch, out) == nil && repo.Push(ctx, remote, "HEAD:refs/heads/"+baseBranch) == nil {
		fmt.Fprintf(out, "published the record an earlier session left in %s\n", wt)
		return true
	}

	refName := fmt.Sprintf("refs/openspec/unpublished/%s-%s-%s", filepath.Base(wt), stamp, shortSHA)
	_, _ = gitx.RunGit(ctx, wt, "update-ref", refName, "HEAD")
	fmt.Fprintf(out, "could not publish the record an earlier session left in %s; kept as %s\n", wt, refName)
	return false
}

// PrepareSessionWorktree prepares .worktrees/_cycle or .worktrees/_visual_qa at baseRef.
func PrepareSessionWorktree(ctx context.Context, root, wt, baseRef, baseBranch string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return err
	}
	repo, err := gitx.Open(root)
	if err != nil {
		return err
	}

	wtValid := false
	if info, err := os.Stat(wt); err == nil && info.IsDir() {
		if topOut, err := gitx.RunGit(ctx, wt, "rev-parse", "--show-toplevel"); err == nil {
			topClean, _ := filepath.EvalSymlinks(strings.TrimSpace(topOut))
			wtClean, _ := filepath.EvalSymlinks(wt)
			if topClean != "" && topClean == wtClean {
				wtValid = true
			}
		}
	}

	if wtValid {
		if absGitDir, err := gitx.RunGit(ctx, wt, "rev-parse", "--absolute-git-dir"); err == nil {
			gitDirPath := strings.TrimSpace(absGitDir)
			_ = filepath.WalkDir(gitDirPath, func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() && d.Name() == "index.lock" {
					if info, err := d.Info(); err == nil && time.Since(info.ModTime()) >= 10*time.Minute {
						if os.Remove(p) == nil {
							fmt.Fprintf(out, "removed %s, which no git had touched for 10 minutes\n", p)
						}
					}
				}
				return nil
			})
		}
		if PublishLeftoverRecord(ctx, wt, baseRef, baseBranch, out) {
			if newRef, err := gitx.RunGit(ctx, wt, "rev-parse", "HEAD"); err == nil {
				baseRef = strings.TrimSpace(newRef)
			}
		}
	} else {
		_ = os.RemoveAll(wt)
		if err := repo.AddWorktree(ctx, wt, "", baseRef, true); err != nil {
			return err
		}
	}

	excludePath := filepath.Join(root, ".git", "info", "exclude")
	_ = os.MkdirAll(filepath.Dir(excludePath), 0o755)
	if exBytes, err := os.ReadFile(excludePath); err != nil || !strings.Contains("\n"+string(exBytes)+"\n", "\n/log\n") {
		f, err := os.OpenFile(excludePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			_, _ = f.WriteString("\n/log\n")
			_ = f.Close()
		}
	}

	_, _ = gitx.RunGit(ctx, wt, "rebase", "--abort")
	removeLegacyLogLink(wt)
	if _, err := gitx.RunGit(ctx, wt, "checkout", "--quiet", "--force", "--detach", baseRef); err != nil {
		return err
	}
	_, _ = gitx.RunGit(ctx, wt, "clean", "-fdq")

	// Runtime files live in store.RuntimeDir, shared by every worktree of the workspace;
	// worktrees no longer get a log/ symlink. Drop one left behind by older versions.
	removeLegacyLogLink(wt)

	if fileExists(filepath.Join(wt, ".gitmodules")) {
		_, _ = gitx.RunGit(ctx, wt, "submodule", "foreach", "--recursive", "git rebase --abort >/dev/null 2>&1 || true; git checkout --force -q; git clean -fdq")
		if _, err := gitx.RunGit(ctx, wt, "submodule", "update", "--init", "--recursive", "--checkout", "--force"); err != nil {
			return err
		}
		_, _ = gitx.RunGit(ctx, wt, "submodule", "foreach", "--recursive", "git clean -fdq")
	}

	// The worktree is kept from one session to the next, and with it the search index agent-memory
	// built there: without this the session would search the memory as the one before it left it.
	if err := memory.Refresh(ctx, wt); err != nil {
		fmt.Fprintf(out, "could not refresh the search index of the memory in %s: %v\n", wt, err)
	}
	return nil
}

// RunProposalsCLI implements the CLI entry point for `foreman-kit proposals ...`.
func RunProposalsCLI(ctx context.Context, root string, st *store.Store, judge *evaluator.Judge, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: foreman-kit proposals review | rebase <base> | carry <checkout> | list | show <change> | approve <change> | reject <change>")
		return 2
	}

	var err error
	switch args[0] {
	case "review":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "Usage: foreman-kit proposals review")
			return 2
		}
		err = ReviewProposals(ctx, root, st, judge, stdout)
	case "rebase":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Usage: foreman-kit proposals rebase <base>")
			return 2
		}
		err = RebaseProposals(ctx, root, args[1], stdout)
	case "carry":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Usage: foreman-kit proposals carry <checkout>")
			return 2
		}
		err = CarryProposals(ctx, root, args[1], stdout)
	case "list":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "Usage: foreman-kit proposals list")
			return 2
		}
		err = ListProposals(root, stdout)
	case "show":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Usage: foreman-kit proposals show <change>")
			return 2
		}
		err = ShowProposal(root, args[1], stdout)
	case "approve":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Usage: foreman-kit proposals approve <change>")
			return 2
		}
		err = ApproveProposal(root, args[1], stdout)
	case "reject":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Usage: foreman-kit proposals reject <change>")
			return 2
		}
		err = RejectProposal(root, args[1], stdout)
	default:
		fmt.Fprintln(stderr, "Usage: foreman-kit proposals review | rebase <base> | carry <checkout> | list | show <change> | approve <change> | reject <change>")
		return 2
	}

	if err != nil {
		fmt.Fprintf(stderr, "proposals: %v\n", err)
		return 1
	}
	return 0
}

func uncommittedScopeFiles(ctx context.Context, dir string) ([]string, error) {
	diffArgs := append([]string{"diff", "--name-only", "-z", "HEAD", "--"}, ScopePaths...)
	diffOut, err := gitx.RunGit(ctx, dir, diffArgs...)
	if err != nil {
		return nil, err
	}
	lsArgs := append([]string{"ls-files", "-z", "-o", "--exclude-standard", "--"}, ScopePaths...)
	lsOut, err := gitx.RunGit(ctx, dir, lsArgs...)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var names []string
	for _, raw := range strings.Split(diffOut+"\x00"+lsOut, "\x00") {
		p := strings.TrimSpace(raw)
		if p != "" && !seen[p] {
			seen[p] = true
			names = append(names, p)
		}
	}
	sort.Strings(names)
	return names, nil
}

func isAncestor(ctx context.Context, dir, revA, revB string) bool {
	_, err := gitx.RunGit(ctx, dir, "merge-base", "--is-ancestor", revA, revB)
	return err == nil
}

// isTrackedInGit reports whether the index of the checkout at dir holds relPath. A git that could
// not answer is an error, not "it does not": a file the record deleted would be left out of its
// commit, and the deletion with it.
func isTrackedInGit(ctx context.Context, dir, relPath string) (bool, error) {
	listed, err := gitx.RunGit(ctx, dir, "--literal-pathspecs", "ls-files", "-z", "--", relPath)
	if err != nil {
		return false, err
	}
	return listed != "", nil
}

func loadYAMLMap(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		out := make(map[string]any, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// fileOrAbsent returns the text of the file at path, and nil when no file is there. A file that
// cannot be read is an error: the reconcile takes nil for a file somebody deleted.
func fileOrAbsent(path string) (*string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

func ptrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// removeLegacyLogLink removes a wt/log symlink created by older versions (never a real directory).
func removeLegacyLogLink(wt string) {
	p := filepath.Join(wt, "log")
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(p)
	}
}
