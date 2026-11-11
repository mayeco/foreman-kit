package evaluator

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

var gateBlockActions = map[string]string{
	"plan_review":       "repair the plan",
	"scenario_coverage": "repair the tests",
	"repair_progress":   "quarantine repair_stalled",
	"proposal_review":   "wait for a human",
}

// Report represents the output of foreman-kit evaluation-report.
type Report struct {
	Since string           `json:"since"`
	Model string           `json:"model"`
	Gates []GateReportItem `json:"gates"`
}

// GateReportItem summarizes the evaluator's answers and what happened to the changes for one gate.
type GateReportItem struct {
	Gate                   string                `json:"gate"`
	Mode                   string                `json:"mode"`
	Answers                int                   `json:"answers"`
	Cached                 int                   `json:"cached"`
	Unavailable            int                   `json:"unavailable"`
	Statuses               map[string]int        `json:"statuses"`
	CostUSD                float64               `json:"cost_usd"`
	Questions              map[string]any        `json:"questions"`
	EnforceWouldStop       map[string]int        `json:"enforce_would_stop"`
	EnforceWouldLetThrough map[string]int        `json:"enforce_would_let_through"`
	Changes                []ChangeGateReportRow `json:"changes"`
}

// ChangeGateReportRow summarizes the evaluations of a single change on a gate.
type ChangeGateReportRow struct {
	Change       string         `json:"change"`
	Answers      int            `json:"answers"`
	Statuses     map[string]int `json:"statuses"`
	EnforceWould []string       `json:"enforce_would"`
	Outcome      string         `json:"outcome"`
}

// BuildReport generates the calibration report of the evaluator from SQLite events and workspace state.
func BuildReport(ctx context.Context, root string, st *store.Store, since time.Time, onlyGate string, outcomeOf func(string) string) (*Report, error) {
	pol, err := LoadPolicy(root)
	if err != nil {
		return nil, err
	}
	if outcomeOf == nil {
		outcomeOf = func(string) string { return "pending or abandoned" }
	}

	var evts []store.EventRecord
	if st != nil {
		evts, err = st.QueryEvents(ctx, store.EventFilter{Since: since})
		if err != nil {
			return nil, err
		}
	}

	var answerEvts, unavailEvts []store.EventRecord
	for _, e := range evts {
		switch e.Event {
		case "evaluator.answer":
			answerEvts = append(answerEvts, e)
		case "evaluator.unavailable":
			unavailEvts = append(unavailEvts, e)
		}
	}

	gateOrder := []string{"plan_review", "failure_triage", "repair_progress", "scenario_coverage", "change_risk", "concurrent_work_loss", "system_error_triage", "policy_violation_triage", "agent_outcome", "semantic_merge_conflict", "push_rejection", "task_priority", "proposal_review"}
	seen := make(map[string]bool)
	var names []string
	for _, g := range gateOrder {
		if _, ok := pol.Gates[g]; ok {
			seen[g] = true
			names = append(names, g)
		}
	}
	for g := range pol.Gates {
		if !seen[g] {
			seen[g] = true
			names = append(names, g)
		}
	}
	for _, e := range answerEvts {
		if g, _ := e.Payload["gate"].(string); g != "" && !seen[g] {
			seen[g] = true
			names = append(names, g)
		}
	}

	if onlyGate != "" {
		var filtered []string
		for _, n := range names {
			if n == onlyGate {
				filtered = append(filtered, n)
			}
		}
		names = filtered
	}

	out := &Report{
		Since: since.UTC().Format(time.RFC3339),
		Model: pol.Model,
		Gates: make([]GateReportItem, 0, len(names)),
	}

	for _, gate := range names {
		cfg := pol.Gates[gate]
		var gAnswers []store.EventRecord
		for _, e := range answerEvts {
			if e.Payload["gate"] == gate {
				gAnswers = append(gAnswers, e)
			}
		}
		unavailCount := 0
		for _, e := range unavailEvts {
			if e.Payload["gate"] == gate {
				unavailCount++
			}
		}
		out.Gates = append(out.Gates, buildGateReport(gate, cfg, gAnswers, unavailCount, outcomeOf))
	}
	return out, nil
}

func buildGateReport(gate string, cfg GatePolicy, events []store.EventRecord, unavailable int, outcomeOf func(string) string) GateReportItem {
	byChange := make(map[string][]store.EventRecord)
	var changeOrder []string
	for _, e := range events {
		chg := e.Change
		if chg == "" {
			chg, _ = e.Payload["change"].(string)
		}
		if _, ok := byChange[chg]; !ok {
			changeOrder = append(changeOrder, chg)
		}
		byChange[chg] = append(byChange[chg], e)
	}

	changes := make([]ChangeGateReportRow, 0, len(changeOrder))
	stopTally := make(map[string]int)
	throughTally := make(map[string]int)

	for _, chg := range changeOrder {
		list := byChange[chg]
		stTally := make(map[string]int)
		var actions []string
		seenAct := make(map[string]bool)
		for _, e := range list {
			st, _ := e.Payload["status"].(string)
			stTally[st]++
			if act := enforceActionForEvent(gate, cfg, e); act != "" && !seenAct[act] {
				seenAct[act] = true
				actions = append(actions, act)
			}
		}
		if actions == nil {
			actions = []string{}
		}
		outcome := outcomeOf(chg)
		row := ChangeGateReportRow{
			Change:       chg,
			Answers:      len(list),
			Statuses:     stTally,
			EnforceWould: actions,
			Outcome:      outcome,
		}
		changes = append(changes, row)
		if len(actions) > 0 {
			stopTally[outcome]++
		} else {
			throughTally[outcome]++
		}
	}

	statuses := make(map[string]int)
	cachedCount := 0
	totalCost := 0.0
	for _, e := range events {
		st, _ := e.Payload["status"].(string)
		statuses[st]++
		if c, ok := e.Payload["cached"].(bool); ok && c {
			cachedCount++
		}
		if cost, ok := toFloat(e.Payload["cost_usd"]); ok {
			totalCost += cost
		}
	}
	mode := cfg.Mode
	if mode == "" {
		mode = "off"
	}

	return GateReportItem{
		Gate:                   gate,
		Mode:                   mode,
		Answers:                len(events),
		Cached:                 cachedCount,
		Unavailable:            unavailable,
		Statuses:               statuses,
		CostUSD:                math.Round(totalCost*1e6) / 1e6,
		Questions:              computeQuestionStats(events, cfg),
		EnforceWouldStop:       stopTally,
		EnforceWouldLetThrough: throughTally,
		Changes:                changes,
	}
}

func enforceActionForEvent(gate string, cfg GatePolicy, e store.EventRecord) string {
	ans := parseEventAnswers(e)
	switch gate {
	case "failure_triage":
		if matchesSummaryRule(ans, cfg.RerunWhen) {
			return "re-run the gates"
		}
		if matchesSummaryRule(ans, cfg.EscalateWhen) {
			return "escalate spec_ambiguity"
		}
		return actionForSummary(ans, cfg)
	case "system_error_triage", "policy_violation_triage", "agent_outcome", "semantic_merge_conflict", "push_rejection":
		return actionForSummary(ans, cfg)
	case "change_risk":
		for i := range cfg.BlockWhen {
			if matchesSummaryRule(ans, &cfg.BlockWhen[i]) {
				return "quarantine risk_review"
			}
		}
	case "concurrent_work_loss":
		for i := range cfg.AllowWhen {
			if matchesSummaryRule(ans, &cfg.AllowWhen[i]) {
				return ""
			}
		}
		for i := range cfg.BlockWhen {
			if matchesSummaryRule(ans, &cfg.BlockWhen[i]) {
				return "send back to restore the work, then quarantine concurrent_work_loss"
			}
		}
	default:
		st, _ := e.Payload["status"].(string)
		if st == "failed" || st == "abstained" {
			if act, ok := gateBlockActions[gate]; ok {
				return act
			}
			return "block"
		}
	}
	return ""
}

// actionForSummary is the `actions:` rule a recorded answer set selects, if any.
func actionForSummary(ans map[string]any, cfg GatePolicy) string {
	for i := range cfg.Actions {
		if matchesSummaryRule(ans, &cfg.Actions[i]) {
			return cfg.Actions[i].Action
		}
	}
	return ""
}

func parseEventAnswers(e store.EventRecord) map[string]any {
	raw := e.Payload["answers"]
	switch v := raw.(type) {
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(v), &m) == nil {
			return m
		}
	case map[string]any:
		return v
	}
	return map[string]any{}
}

func matchesSummaryRule(answers map[string]any, rule *RoutingRule) bool {
	if rule == nil || rule.Question == "" {
		return false
	}
	val, ok := answers[rule.Question]
	if !ok {
		return false
	}
	if rule.Choice != "" {
		s, ok := val.(string)
		if !ok || !strings.Contains(s, "@") {
			return false
		}
		parts := strings.SplitN(s, "@", 2)
		conf, _ := strconv.ParseFloat(parts[1], 64)
		minConf := 0.0
		if rule.MinConfidence != nil {
			minConf = *rule.MinConfidence
		}
		return parts[0] == rule.Choice && conf >= minConf
	}
	num, ok := toFloat(val)
	if !ok {
		return false
	}
	if rule.Min != nil {
		return num >= *rule.Min
	}
	if rule.Max != nil {
		return num <= *rule.Max
	}
	return false
}

func computeQuestionStats(events []store.EventRecord, cfg GatePolicy) map[string]any {
	limits := make(map[string]struct {
		op  string
		val float64
	})
	for _, cond := range cfg.Assert {
		if q, op, val, err := ParseCondition(cond); err == nil {
			limits[q] = struct {
				op  string
				val float64
			}{op, val}
		}
	}

	numVals := make(map[string][]float64)
	choiceVals := make(map[string][]string)
	var qOrder []string
	seen := make(map[string]bool)

	for _, e := range events {
		ans := parseEventAnswers(e)
		for q, v := range ans {
			if !seen[q] {
				seen[q] = true
				qOrder = append(qOrder, q)
			}
			if s, ok := v.(string); ok && strings.Contains(s, "@") {
				choiceVals[q] = append(choiceVals[q], strings.SplitN(s, "@", 2)[0])
			} else if f, ok := toFloat(v); ok {
				numVals[q] = append(numVals[q], f)
			} else {
				choiceVals[q] = append(choiceVals[q], fmt.Sprint(v))
			}
		}
	}

	out := make(map[string]any, len(qOrder))
	for _, q := range qOrder {
		if choices, ok := choiceVals[q]; ok && len(choices) > 0 {
			tally := make(map[string]int)
			for _, c := range choices {
				tally[c]++
			}
			out[q] = map[string]any{"choices": tally}
			continue
		}
		nums := numVals[q]
		if len(nums) == 0 {
			continue
		}
		sorted := append([]float64(nil), nums...)
		sort.Float64s(sorted)
		stat := map[string]any{
			"min":       math.Round(sorted[0]*1000) / 1000,
			"median":    math.Round(sorted[len(sorted)/2]*1000) / 1000,
			"max":       math.Round(sorted[len(sorted)-1]*1000) / 1000,
			"threshold": nil,
			"missed":    nil,
		}
		if lim, ok := limits[q]; ok {
			stat["threshold"] = fmt.Sprintf("%s %v", lim.op, lim.val)
			missed := 0
			for _, v := range nums {
				if lim.op == "<=" && v > lim.val {
					missed++
				} else if lim.op != "<=" && v < lim.val {
					missed++
				}
			}
			stat["missed"] = missed
		}
		out[q] = stat
	}
	return out
}

// FormatReportText formats Report into human-readable CLI output.
func FormatReportText(r *Report) string {
	model := r.Model
	if model == "" {
		model = "not pinned"
	}
	lines := []string{
		fmt.Sprintf("== Evaluator answers since %s (model %s; what enforce would do follows today's .foreman-kit/evaluator.yaml)", r.Since, model),
	}
	for _, g := range r.Gates {
		if g.Gate == "task_priority" && g.Answers == 0 {
			lines = append(lines, fmt.Sprintf("%s (%s): asked by agents through the jev CLI, so it leaves no events", g.Gate, g.Mode))
			continue
		}
		if g.Answers == 0 {
			extra := ""
			if g.Unavailable > 0 {
				extra = fmt.Sprintf(", %d unavailable", g.Unavailable)
			}
			lines = append(lines, fmt.Sprintf("%s (%s): no answers%s", g.Gate, g.Mode, extra))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s (%s): %d answers, %d cached, %d unavailable, $%v", g.Gate, g.Mode, g.Answers, g.Cached, g.Unavailable, g.CostUSD))
		lines = append(lines, fmt.Sprintf("  status: %s", formatTally(g.Statuses)))
		var qKeys []string
		for k := range g.Questions {
			qKeys = append(qKeys, k)
		}
		sort.Strings(qKeys)
		for _, q := range qKeys {
			s, _ := g.Questions[q].(map[string]any)
			if choices, ok := s["choices"].(map[string]int); ok {
				lines = append(lines, fmt.Sprintf("  %s: %s", q, formatTally(choices)))
			} else {
				limit := ""
				if th, ok := s["threshold"].(string); ok && th != "" {
					limit = fmt.Sprintf(", threshold %s, missed %v", th, s["missed"])
				}
				lines = append(lines, fmt.Sprintf("  %s: %v / %v / %v (min / median / max)%s", q, s["min"], s["median"], s["max"], limit))
			}
		}
		var stopped []ChangeGateReportRow
		for _, c := range g.Changes {
			if len(c.EnforceWould) > 0 {
				stopped = append(stopped, c)
			}
		}
		lines = append(lines, fmt.Sprintf("  enforce would stop %d change(s): %s", len(stopped), formatTally(g.EnforceWouldStop)))
		for _, c := range stopped {
			lines = append(lines, fmt.Sprintf("    - %s: %s; now %s", c.Change, strings.Join(c.EnforceWould, ", "), c.Outcome))
		}
		lines = append(lines, fmt.Sprintf("  enforce would let through %d change(s): %s", len(g.Changes)-len(stopped), formatTally(g.EnforceWouldLetThrough)))
	}
	lines = append(lines, "J0 (the canary) and J5 (asked by agents) leave no events; answers older than the event store are gone.")
	return strings.Join(lines, "\n")
}

func formatTally(m map[string]int) string {
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
