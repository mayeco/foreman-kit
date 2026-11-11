package config

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// PromptsDir is the relative directory containing the application's prompts.
const PromptsDir = ".foreman-kit/prompts"

// RulesTemplate is the relative path to the kit's OpenSpec authoring rules template.
const RulesTemplate = ".foreman-kit/templates/authoring_rules.md"

// EnginesDownExitCode (75) indicates no agent engine could work; not counted as an attempt in PromptReport.
const EnginesDownExitCode = 75

var (
	promptCommentRe     = regexp.MustCompile(`(?s)<!--.*?-->\n?`)
	promptPlaceholderRe = regexp.MustCompile(`\{\{(\w+)\}\}`)
	sinceMinutesRe      = regexp.MustCompile(`^(\d+)m$`)
	sinceHoursRe        = regexp.MustCompile(`^(\d+)h$`)
	sinceDaysRe         = regexp.MustCompile(`^(\d+)d$`)
)

// PLACEHOLDERS maps each prompt role to its known placeholders and their descriptions.
var PLACEHOLDERS = map[string]map[string]string{
	"plan": {
		"change":          "the change's name",
		"goal":            "the backlog item's description",
		"memory":          "the notes of one repository or capability each (.agent-memory/modules/), those of the capabilities the change modifies first (empty while there are none)",
		"evaluator_hint":  "how to ask the evaluator a yes/no question (empty without one)",
		"authoring_rules": "the kit's rules for OpenSpec artifacts (.foreman-kit/templates/authoring_rules.md)",
		"feedback":        "on a repair round, exactly what failed verification; on the first call after an automatic retry, also what quarantined the change (empty on the first attempt)",
	},
	"build": {
		"change":          "the change's name",
		"branch":          "the feature branch every child repository is on",
		"memory":          "the notes of one repository or capability each (.agent-memory/modules/), those of the capabilities the change modifies first (empty while there are none)",
		"evaluator_hint":  "how to ask the evaluator a yes/no question (empty without one)",
		"authoring_rules": "the kit's rules for OpenSpec artifacts (.foreman-kit/templates/authoring_rules.md)",
		"feedback":        "on a repair round, exactly what failed verification; on the first call after an automatic retry, also what quarantined the change (empty on the first attempt)",
		"rebuild":         "when the change is rebuilt after a rebase conflict, where the old code is (empty otherwise)",
	},
	"cycle": {},
}

// PlaceholdersOrder preserves the canonical order of known placeholders per prompt role.
var PlaceholdersOrder = map[string][]string{
	"plan":  {"change", "goal", "memory", "evaluator_hint", "authoring_rules", "feedback"},
	"build": {"change", "branch", "memory", "evaluator_hint", "authoring_rules", "feedback", "rebuild"},
	"cycle": {},
}

// REQUIRED maps each prompt role to the placeholders that must be present.
var REQUIRED = map[string][]string{
	"plan":  {"change", "feedback"},
	"build": {"change", "branch", "feedback", "rebuild"},
	"cycle": {},
}

// PromptProblems holds validation errors and warnings for .foreman-kit/prompts/.
type PromptProblems struct {
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// PromptPath returns the path to .foreman-kit/prompts/<name>.md in root.
func PromptPath(root, name string) string {
	if root == "" {
		root = AppRoot("")
	}
	return filepath.Join(root, filepath.FromSlash(PromptsDir), name+".md")
}

// Render reads .foreman-kit/prompts/<name>.md, removes HTML comments, and substitutes {{placeholder}} variables.
// Unknown placeholders are left untouched as written.
func Render(root, name string, vars map[string]string) (string, error) {
	file := PromptPath(root, name)
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%s/%s.md is missing: restore it from the kit's release (a seed), then commit it", PromptsDir, name)
	}
	noComments := promptCommentRe.ReplaceAllString(string(raw), "")
	rendered := promptPlaceholderRe.ReplaceAllStringFunc(noComments, func(match string) string {
		m := promptPlaceholderRe.FindStringSubmatch(match)
		if len(m) == 2 && vars != nil {
			if val, ok := vars[m[1]]; ok {
				return val
			}
		}
		return match
	})
	return rendered, nil
}

// Placeholders returns the unique placeholder names used in .foreman-kit/prompts/<name>.md (outside HTML comments).
func Placeholders(root, name string) ([]string, error) {
	file := PromptPath(root, name)
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	noComments := promptCommentRe.ReplaceAllString(string(raw), "")
	matches := promptPlaceholderRe.FindAllStringSubmatch(noComments, -1)
	seen := make(map[string]bool)
	var out []string
	for _, m := range matches {
		if len(m) == 2 && !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	if out == nil {
		return []string{}, nil
	}
	return out, nil
}

// Version returns the 12-char SHA-256 hex version of .foreman-kit/prompts/<name>.md (without HTML comments,
// plus .foreman-kit/templates/authoring_rules.md when the prompt uses {{authoring_rules}}).
func Version(root, name string) (string, error) {
	file := PromptPath(root, name)
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	noComments := promptCommentRe.ReplaceAllString(string(raw), "")
	parts := []string{noComments}
	used, _ := Placeholders(root, name)
	for _, p := range used {
		if p == "authoring_rules" {
			if rulesBytes, err := ReadKitFileBytes(root, RulesTemplate); err == nil {
				parts = append(parts, string(rulesBytes))
			}
			break
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:12], nil
}

// Problems checks all prompt files in .foreman-kit/prompts/ for missing files, unknown placeholders, and missing required placeholders.
func Problems(root string) PromptProblems {
	report := PromptProblems{
		Errors:   []string{},
		Warnings: []string{},
	}
	for _, name := range RolesOrder {
		known := PLACEHOLDERS[name]
		rel := fmt.Sprintf("%s/%s.md", PromptsDir, name)
		file := PromptPath(root, name)
		if !fileExists(file) {
			report.Errors = append(report.Errors, fmt.Sprintf("%s is missing: restore it from the kit's release (a seed)", rel))
			continue
		}
		used, err := Placeholders(root, name)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		usedSet := make(map[string]bool, len(used))
		for _, p := range used {
			usedSet[p] = true
			if _, ok := known[p]; !ok {
				suffix := " (it takes none)"
				if len(known) > 0 {
					suffix = fmt.Sprintf(" (known: %s)", strings.Join(PlaceholdersOrder[name], ", "))
				}
				report.Errors = append(report.Errors, fmt.Sprintf("%s: unknown placeholder {{%s}}%s", rel, p, suffix))
			}
		}
		for _, req := range REQUIRED[name] {
			if !usedSet[req] {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: {{%s}} is missing: %s", rel, req, known[req]))
			}
		}
	}
	return report
}

// RunPromptsCLI implements `foreman-kit prompts check | render <plan|build|cycle> [k=v ...] | version <plan|build|cycle>`.
func RunPromptsCLI(root string, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if root == "" {
		root = AppRoot("")
	}
	usage := "Usage: ruby bin/lib/prompts.rb check | render <plan|build|cycle> [name=value ...] | version <plan|build|cycle>"
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	switch args[0] {
	case "check":
		if len(args) != 1 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		rep := Problems(root)
		for _, e := range rep.Errors {
			fmt.Fprintf(stdout, "ERROR %s\n", e)
		}
		if len(rep.Errors) == 0 {
			fmt.Fprintln(stdout, "prompts: OK")
			return 0
		}
		fmt.Fprintf(stdout, "prompts: FAIL (%d)\n", len(rep.Errors))
		return 1
	case "version":
		if len(args) != 2 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		name := args[1]
		if !fileExists(PromptPath(root, name)) {
			fmt.Fprintf(stderr, "prompts: %s/%s.md is missing\n", PromptsDir, name)
			return 1
		}
		ver, err := Version(root, name)
		if err != nil {
			fmt.Fprintf(stderr, "prompts: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, ver)
		return 0
	case "render":
		if len(args) < 2 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		name := args[1]
		vars := make(map[string]string)
		for _, pair := range args[2:] {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 {
				vars[parts[0]] = parts[1]
			} else {
				vars[parts[0]] = ""
			}
		}
		out, err := Render(root, name, vars)
		if err != nil {
			fmt.Fprintf(stderr, "prompts: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, out)
		return 0
	default:
		fmt.Fprintln(stderr, usage)
		return 2
	}
}

// ==============================================================================
// PromptReport (port of bin/prompt_report.rb)
// ==============================================================================

// PromptReportData represents the report produced by bin/prompt_report.rb.
type PromptReportData struct {
	Since string             `json:"since"`
	By    string             `json:"by"`
	Roles []PromptRoleReport `json:"roles"`
}

// PromptRoleReport holds version metrics for a single role ("plan" or "build").
type PromptRoleReport struct {
	Role     string                `json:"role"`
	Current  *string               `json:"current"`
	Versions []PromptVersionReport `json:"versions"`
}

// PromptVersionReport holds metrics for a single prompt or memory version.
type PromptVersionReport struct {
	Version               string                  `json:"version"`
	FirstUsed             string                  `json:"first_used"`
	LastUsed              string                  `json:"last_used"`
	Changes               int                     `json:"changes"`
	Calls                 int                     `json:"calls"`
	Repairs               int                     `json:"repairs"`
	RepairsPerChange      float64                 `json:"repairs_per_change"`
	Finished              int                     `json:"finished"`
	FinishedWithoutRepair int                     `json:"finished_without_repair"`
	Outcomes              map[string]int          `json:"outcomes"`
	Engines               map[string]int          `json:"engines"`
	ChangeList            []PromptChangeReportRow `json:"change_list"`
	Current               bool                    `json:"current"`
}

// PromptChangeReportRow summarizes one change's repairs and outcome under a version.
type PromptChangeReportRow struct {
	Change  string `json:"change"`
	Repairs int    `json:"repairs"`
	Outcome string `json:"outcome"`
}

var promptReportKeys = map[string]string{
	"prompt": "prompt_version",
	"memory": "memory_version",
}

var promptReportRoles = []string{"plan", "build"}

var unfinishedOutcomes = map[string]bool{
	"in flight":            true,
	"pending or abandoned": true,
}

func roleOfPhase(phase string) string {
	if strings.HasPrefix(phase, "plan") {
		return "plan"
	}
	return "build"
}

func isRepairPhase(phase string) bool {
	return strings.Contains(phase, "repair")
}

// ParseSince parses a duration shorthand ("90m", "24h", "7d", "30d") or an ISO-8601/RFC3339 timestamp.
func ParseSince(text string) (time.Time, error) {
	text = strings.TrimSpace(text)
	now := time.Now().UTC()
	if m := sinceMinutesRe.FindStringSubmatch(text); len(m) == 2 {
		n, _ := strconv.Atoi(m[1])
		return now.Add(-time.Duration(n) * time.Minute), nil
	}
	if m := sinceHoursRe.FindStringSubmatch(text); len(m) == 2 {
		n, _ := strconv.Atoi(m[1])
		return now.Add(-time.Duration(n) * time.Hour), nil
	}
	if m := sinceDaysRe.FindStringSubmatch(text); len(m) == 2 {
		n, _ := strconv.Atoi(m[1])
		return now.Add(-time.Duration(n) * 24 * time.Hour), nil
	}
	for _, layout := range []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("--since takes 90m, 24h, 7d or a time, not %q", text)
}

// DefaultOutcomeResolver builds a change outcome lookup function backed by gitx, store, and .foreman-kit/agent_proposals.json.
func DefaultOutcomeResolver(ctx context.Context, root string, st *store.Store) func(string) string {
	integrated := make(map[string]bool)
	if repo, err := gitx.Open(root); err == nil {
		baseRef := "origin/" + settings.String(root, settings.BaseBranch)
		if _, err := repo.RevParse(baseRef); err != nil {
			baseRef = "HEAD"
		}
		if list, err := repo.CompletedSince("", baseRef); err == nil {
			for _, it := range list {
				integrated[it.ChangeName] = true
			}
		}
	}
	// Also check openspec/changes/archive/YYYY-MM-DD-<name>
	archDir := filepath.Join(root, "openspec", "changes", "archive")
	if entries, err := os.ReadDir(archDir); err == nil {
		re := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-(.+)$`)
		for _, e := range entries {
			if e.IsDir() {
				if m := re.FindStringSubmatch(e.Name()); len(m) == 2 {
					integrated[m[1]] = true
				}
			}
		}
	}

	blocked := make(map[string]string)
	ready := make(map[string]bool)
	inFlight := make(map[string]bool)

	_ = withStore(root, st, func(s *store.Store) error {
		if qList, err := s.ListQuarantine(ctx, "blocked"); err == nil {
			for _, q := range qList {
				blocked[q.ChangeName] = q.Reason
			}
		}
		if rList, err := s.ListReadyQueue(ctx); err == nil {
			for _, r := range rList {
				ready[r.ChangeName] = true
			}
		}
		if locks, err := s.ListLocks(ctx); err == nil {
			host, _ := os.Hostname()
			nowUnix := time.Now().UTC().Unix()
			for _, l := range locks {
				chg, ok := store.WorkerLeaseChange(l.LockKey)
				if !ok {
					continue
				}
				if !store.IsLockStale(l, host, nowUnix) {
					inFlight[chg] = true
				}
			}
		}
		return nil
	})

	proposals := make(map[string]string)
	if raw, err := os.ReadFile(filepath.Join(root, ".foreman-kit", "agent_proposals.json")); err == nil {
		var list []map[string]any
		if json.Unmarshal(raw, &list) == nil {
			for _, p := range list {
				if chg, ok := p["change_name"].(string); ok {
					if stStr, ok := p["status"].(string); ok {
						proposals[chg] = stStr
					}
				}
			}
		}
	}

	return func(change string) string {
		if integrated[change] {
			return "integrated"
		}
		if reason, ok := blocked[change]; ok {
			return fmt.Sprintf("blocked (%s)", reason)
		}
		if ready[change] {
			return "ready to merge"
		}
		if inFlight[change] {
			return "in flight"
		}
		if propStatus, ok := proposals[change]; ok {
			return fmt.Sprintf("proposal %s", propStatus)
		}
		return "pending or abandoned"
	}
}

// PromptReport generates the prompt/memory version performance report from agent.exit events in internal/store.
func PromptReport(ctx context.Context, root string, st *store.Store, since time.Time, by string, outcomeOf func(string) string) (*PromptReportData, error) {
	if root == "" {
		root = AppRoot("")
	}
	key, ok := promptReportKeys[by]
	if !ok {
		return nil, fmt.Errorf("--by takes prompt or memory, not %q", by)
	}
	if outcomeOf == nil {
		outcomeOf = DefaultOutcomeResolver(ctx, root, st)
	}

	var events []store.EventRecord
	_ = withStore(root, st, func(s *store.Store) error {
		var err error
		events, err = s.QueryEvents(ctx, store.EventFilter{
			Since: since,
			Event: "agent.exit",
		})
		return err
	})

	// Fallback to log/events.jsonl if SQLite store had no events and the file exists
	if len(events) == 0 {
		events = readLegacyEventsJSONL(root, since)
	}

	var calls []store.EventRecord
	for _, e := range events {
		if e.Event != "agent.exit" {
			continue
		}
		ver, _ := e.Payload[key].(string)
		if ver == "" {
			continue
		}
		code, _ := toInt(e.Payload["code"])
		if code == EnginesDownExitCode {
			continue
		}
		calls = append(calls, e)
	}

	currentMap := make(map[string]*string, 2)
	if by == "memory" {
		fp := memory.Fingerprint(root)
		for _, r := range promptReportRoles {
			v := fp
			currentMap[r] = &v
		}
	} else {
		for _, r := range promptReportRoles {
			if fileExists(PromptPath(root, r)) {
				if v, err := Version(root, r); err == nil {
					verCopy := v
					currentMap[r] = &verCopy
				}
			}
		}
	}

	out := &PromptReportData{
		Since: since.UTC().Format(time.RFC3339),
		By:    by,
		Roles: make([]PromptRoleReport, 0, len(promptReportRoles)),
	}

	for _, role := range promptReportRoles {
		byChange := make(map[string][]store.EventRecord)
		var changeOrder []string
		for _, e := range calls {
			phase := e.Phase
			if phase == "" {
				phase, _ = e.Payload["phase"].(string)
			}
			if roleOfPhase(phase) != role {
				continue
			}
			chg := e.Change
			if chg == "" {
				chg, _ = e.Payload["change"].(string)
			}
			if _, seen := byChange[chg]; !seen {
				changeOrder = append(changeOrder, chg)
			}
			byChange[chg] = append(byChange[chg], e)
		}

		byVersion := make(map[string][]string)
		var versionOrder []string
		for _, chg := range changeOrder {
			list := byChange[chg]
			ver, _ := list[0].Payload[key].(string)
			if _, seen := byVersion[ver]; !seen {
				versionOrder = append(versionOrder, ver)
			}
			byVersion[ver] = append(byVersion[ver], chg)
		}

		versions := make([]PromptVersionReport, 0, len(versionOrder))
		for _, ver := range versionOrder {
			chgNames := byVersion[ver]
			var verCalls []store.EventRecord
			repairsByChange := make(map[string]int, len(chgNames))
			outcomesByChange := make(map[string]string, len(chgNames))
			var finishedChanges []string

			for _, chg := range chgNames {
				cList := byChange[chg]
				verCalls = append(verCalls, cList...)
				repCount := 0
				for _, ev := range cList {
					ph := ev.Phase
					if ph == "" {
						ph, _ = ev.Payload["phase"].(string)
					}
					if isRepairPhase(ph) {
						repCount++
					}
				}
				repairsByChange[chg] = repCount
				o := outcomeOf(chg)
				outcomesByChange[chg] = o
				if !unfinishedOutcomes[o] {
					finishedChanges = append(finishedChanges, chg)
				}
			}

			var times []string
			enginesTally := make(map[string]int)
			for _, ev := range verCalls {
				ts := ev.TS
				if ts == "" {
					ts, _ = ev.Payload["ts"].(string)
				}
				times = append(times, ts)
				eng := ev.Engine
				if eng == "" {
					eng, _ = ev.Payload["engine"].(string)
				}
				if eng != "" {
					enginesTally[eng]++
				}
			}
			sort.Strings(times)
			firstUsed, lastUsed := "", ""
			if len(times) > 0 {
				firstUsed = times[0]
				lastUsed = times[len(times)-1]
			}

			repairsSum := 0
			for _, r := range repairsByChange {
				repairsSum += r
			}
			finishedNoRepair := 0
			for _, chg := range finishedChanges {
				if repairsByChange[chg] == 0 {
					finishedNoRepair++
				}
			}
			outcomesTally := make(map[string]int)
			changeRows := make([]PromptChangeReportRow, 0, len(chgNames))
			for _, chg := range chgNames {
				outcomesTally[outcomesByChange[chg]]++
				changeRows = append(changeRows, PromptChangeReportRow{
					Change:  chg,
					Repairs: repairsByChange[chg],
					Outcome: outcomesByChange[chg],
				})
			}

			isCurrent := currentMap[role] != nil && *currentMap[role] == ver
			versions = append(versions, PromptVersionReport{
				Version:               ver,
				FirstUsed:             firstUsed,
				LastUsed:              lastUsed,
				Changes:               len(chgNames),
				Calls:                 len(verCalls),
				Repairs:               repairsSum,
				RepairsPerChange:      math.Round((float64(repairsSum)/float64(len(chgNames)))*100) / 100,
				Finished:              len(finishedChanges),
				FinishedWithoutRepair: finishedNoRepair,
				Outcomes:              outcomesTally,
				Engines:               enginesTally,
				ChangeList:            changeRows,
				Current:               isCurrent,
			})
		}

		sort.Slice(versions, func(i, j int) bool {
			return versions[i].FirstUsed < versions[j].FirstUsed
		})

		out.Roles = append(out.Roles, PromptRoleReport{
			Role:     role,
			Current:  currentMap[role],
			Versions: versions,
		})
	}

	return out, nil
}

// FormatPromptReportText formats PromptReportData into the human-readable text format of bin/prompt_report.rb.
func FormatPromptReportText(r *PromptReportData) string {
	what := "prompt (.foreman-kit/prompts/<role>.md and the authoring rules)"
	if r.By == "memory" {
		what = "memory (" + memory.Dir + "/)"
	}
	lines := []string{
		fmt.Sprintf("== Versions of the %s since %s; each change counts under the version of its first call of the role", what, r.Since),
	}
	for _, role := range r.Roles {
		countDesc := fmt.Sprintf("%d version(s)", len(role.Versions))
		if len(role.Versions) == 0 {
			countDesc = "no calls with a recorded version"
		}
		curStr := "unknown"
		if role.Current != nil && *role.Current != "" {
			curStr = *role.Current
		}
		lines = append(lines, fmt.Sprintf("%s: %s, current %s", role.Role, countDesc, curStr))
		for _, v := range role.Versions {
			t1 := formatShortTime(v.FirstUsed)
			t2 := formatShortTime(v.LastUsed)
			span := t1
			if t2 != t1 {
				span = t1 + " .. " + t2
			}
			curMark := ""
			if v.Current {
				curMark = " (current)"
			}
			lines = append(lines, fmt.Sprintf("  %s%s  %s: %d change(s), %d call(s), %d repair round(s) (%v per change), %d of %d finished without a repair",
				v.Version, curMark, span, v.Changes, v.Calls, v.Repairs, v.RepairsPerChange, v.FinishedWithoutRepair, v.Finished))
			lines = append(lines, fmt.Sprintf("    outcomes: %s; agents: %s", formatTallyMap(v.Outcomes), formatTallyMap(v.Engines)))
		}
	}
	lines = append(lines, "A version with a few changes says little. A new prompt version is an edit of .foreman-kit/prompts/<role>.md or a kit upgrade that changed the authoring rules; the change's repairs.md says what each repair round fixed.")
	return strings.Join(lines, "\n")
}

// RunPromptReportCLI implements `foreman-kit prompt-report [--since 30d|24h|<time>] [--by prompt|memory] [--json]`.
func RunPromptReportCLI(ctx context.Context, root string, st *store.Store, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if root == "" {
		root = AppRoot("")
	}
	sinceStr := "30d"
	by := "prompt"
	asJSON := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--since":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "Usage: ruby bin/prompt_report.rb [--since 30d|24h|<time>] [--by prompt|memory] [--json]")
				return 2
			}
			i++
			sinceStr = args[i]
		case "--by":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "Usage: ruby bin/prompt_report.rb [--since 30d|24h|<time>] [--by prompt|memory] [--json]")
				return 2
			}
			i++
			by = args[i]
		case "--json":
			asJSON = true
		default:
			fmt.Fprintln(stderr, "Usage: ruby bin/prompt_report.rb [--since 30d|24h|<time>] [--by prompt|memory] [--json]")
			return 2
		}
	}

	since, err := ParseSince(sinceStr)
	if err != nil {
		fmt.Fprintf(stderr, "prompt_report: %v\n", err)
		return 2
	}
	rep, err := PromptReport(ctx, root, st, since, by, nil)
	if err != nil {
		fmt.Fprintf(stderr, "prompt_report: %v\n", err)
		return 2
	}
	if asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(stdout, string(b))
	} else {
		fmt.Fprintln(stdout, FormatPromptReportText(rep))
	}
	return 0
}

func formatShortTime(ts string) string {
	if len(ts) >= 16 {
		ts = ts[:16]
	}
	return strings.ReplaceAll(ts, "T", " ")
}

func formatTallyMap(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func readLegacyEventsJSONL(root string, since time.Time) []store.EventRecord {
	p := filepath.Join(root, "log", "events.jsonl")
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []store.EventRecord
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var m map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			continue
		}
		tsStr, _ := m["ts"].(string)
		tsTime, err := time.Parse(time.RFC3339, tsStr)
		if err != nil || tsTime.Before(since) {
			continue
		}
		ev, _ := m["event"].(string)
		chg, _ := m["change"].(string)
		eng, _ := m["engine"].(string)
		ph, _ := m["phase"].(string)
		out = append(out, store.EventRecord{
			TS:      tsStr,
			TSUnix:  tsTime.Unix(),
			Event:   ev,
			Change:  chg,
			Engine:  eng,
			Phase:   ph,
			Payload: m,
		})
	}
	return out
}
