package evaluator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// Policy represents .foreman-kit/evaluator.yaml.
type Policy struct {
	// Enabled is false when every gate is off, whatever its mode.
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Model   string `yaml:"model" json:"model"`
	// BaseURL is the endpoint of the evaluator ("" for the provider's own).
	BaseURL string `yaml:"base_url" json:"base_url"`
	Timeout string `yaml:"timeout" json:"timeout"`
	// Retries is how many times a question is asked again.
	Retries int `yaml:"retries" json:"retries"`
	// WhenUnavailable is what a gate does when the evaluator does not answer: pass, block or stop.
	WhenUnavailable string                `yaml:"when_unavailable" json:"when_unavailable"`
	State           PolicyState           `yaml:"state" json:"state"`
	AbstainBand     []float64             `yaml:"abstain_band" json:"abstain_band"`
	Canary          *GatePolicy           `yaml:"canary,omitempty" json:"canary,omitempty"`
	Gates           map[string]GatePolicy `yaml:"gates" json:"gates"`
}

// PolicyState says what of a change is sent to the evaluator.
type PolicyState struct {
	// MaxChars is what every string sent to it is clipped to.
	MaxChars int `yaml:"max_chars" json:"max_chars"`
	// IncludeDiffs is false when the change_risk gate gets file lists and diffstats, never code.
	IncludeDiffs *bool `yaml:"include_diffs" json:"include_diffs"`
}

// BlocksWhenUnavailable reports whether an enforce gate blocks its change when the evaluator does
// not answer.
func (p *Policy) BlocksWhenUnavailable() bool {
	return p != nil && (p.WhenUnavailable == settings.UnavailableBlock || p.WhenUnavailable == settings.UnavailableStop)
}

// StopsWhenUnavailable reports whether the preflight fails when the evaluator does not answer.
func (p *Policy) StopsWhenUnavailable() bool {
	return p != nil && p.WhenUnavailable == settings.UnavailableStop
}

// GatePolicy represents a gate entry in .foreman-kit/evaluator.yaml, including both
// evaluation questions/criteria and decision thresholds.
type GatePolicy struct {
	Mode         string                     `yaml:"mode" json:"mode"`
	File         string                     `yaml:"file,omitempty" json:"file,omitempty"`
	State        any                        `yaml:"state,omitempty" json:"state,omitempty"`
	Questions    map[string]RawQuestionSpec `yaml:"questions,omitempty" json:"questions,omitempty"`
	Assert       []string                   `yaml:"assert,omitempty" json:"assert,omitempty"`
	RerunWhen    *RoutingRule               `yaml:"rerun_when,omitempty" json:"rerun_when,omitempty"`
	EscalateWhen *RoutingRule               `yaml:"escalate_when,omitempty" json:"escalate_when,omitempty"`
	BlockWhen    []RoutingRule              `yaml:"block_when,omitempty" json:"block_when,omitempty"`
	// AllowWhen overrides BlockWhen: when any of its rules matches, the gate does not block.
	AllowWhen []RoutingRule `yaml:"allow_when,omitempty" json:"allow_when,omitempty"`
	// Actions route a state to an action (state -> question -> action): the first rule that matches
	// names the action the orchestrator takes in enforce mode. Without a match, the gate's fallback
	// (the behavior from before the gate existed) applies; GateActions lists each gate's actions.
	Actions             []RoutingRule `yaml:"actions,omitempty" json:"actions,omitempty"`
	MinActionableImpact *float64      `yaml:"min_actionable_impact,omitempty" json:"min_actionable_impact,omitempty"`
	RejectChoices       []string      `yaml:"reject_choices,omitempty" json:"reject_choices,omitempty"`
}

// RoutingRule represents a rule in rerun_when, escalate_when, or block_when.
type RoutingRule struct {
	Question      string   `yaml:"question" json:"question"`
	Choice        string   `yaml:"choice,omitempty" json:"choice,omitempty"`
	MinConfidence *float64 `yaml:"min_confidence,omitempty" json:"min_confidence,omitempty"`
	Min           *float64 `yaml:"min,omitempty" json:"min,omitempty"`
	Max           *float64 `yaml:"max,omitempty" json:"max,omitempty"`
	// Action is the action an `actions:` rule selects.
	Action string `yaml:"action,omitempty" json:"action,omitempty"`
}

// GateActions lists, for each routing gate, the actions its `actions:` rules may name. The first one
// is the gate's fallback: the orchestrator's behavior without the evaluator, used when it is off, unavailable,
// advisory, or no rule matches.
var GateActions = map[string][]string{
	"failure_triage":          {"repair", "hold_environment"},
	"system_error_triage":     {"pause", "block_change", "retry_later"},
	"policy_violation_triage": {"repair", "revert_and_continue", "quarantine"},
	"agent_outcome":           {"defer", "needs_human", "pause"},
	"semantic_merge_conflict": {"integrate", "send_back", "needs_human"},
	"push_rejection":          {"retry", "needs_human"},
}

// FallbackAction is gate's action when the evaluator does not decide.
func FallbackAction(gate string) string {
	if a := GateActions[gate]; len(a) > 0 {
		return a[0]
	}
	return ""
}

// ActionFor returns the action of the first `actions:` rule of g that v matches, and that rule.
func ActionFor(v Verdict, g GatePolicy) (string, *RoutingRule) {
	for i := range g.Actions {
		if g.Actions[i].Action != "" && MatchesRule(v, &g.Actions[i]) {
			return g.Actions[i].Action, &g.Actions[i]
		}
	}
	return "", nil
}

// ConditionResult represents the evaluation of one assert condition.
type ConditionResult struct {
	Condition string  `json:"condition"`
	Question  string  `json:"question"`
	Operator  string  `json:"operator"`
	Threshold float64 `json:"threshold"`
	Actual    float64 `json:"actual"`
	Passed    bool    `json:"passed"`
}

// EvalResult represents the JSON wire structure compatible with jev eval output and evaluation_cache.
type EvalResult struct {
	Model   string                    `json:"model"`
	Answers map[string]map[string]any `json:"answers"`
	Gate    *GateResult               `json:"gate,omitempty"`
	CostUSD float64                   `json:"cost_usd,omitempty"`
}

// GateResult holds evaluated assert conditions.
type GateResult struct {
	Passed     bool              `json:"passed"`
	Conditions []ConditionResult `json:"conditions"`
}

// Verdict represents the outcome of asking the evaluator a gate.
type Verdict struct {
	Gate    string                    `json:"gate"`
	Mode    string                    `json:"mode"`
	Status  string                    `json:"status"` // "passed", "failed", "abstained", "skipped", "unavailable"
	Answers map[string]map[string]any `json:"answers,omitempty"`
	Failed  []ConditionResult         `json:"failed,omitempty"`
	Model   string                    `json:"model,omitempty"`
	Cached  bool                      `json:"cached,omitempty"`
	// EvaluationID is the row of evaluations that records this verdict (0 when the judge has no store).
	EvaluationID int64 `json:"evaluation_id,omitempty"`
}

func (v Verdict) Enforce() bool { return v.Mode == "enforce" }

func (v Verdict) Blocking() bool {
	return v.Enforce() && (v.Status == "failed" || v.Status == "abstained")
}

// PolicyFile is where an application keeps the evaluator's settings, gates and questions.
const PolicyFile = ".foreman-kit/evaluator.yaml"

// LegacyPolicyFile is the name PolicyFile had while the evaluator was named after its provider.
// `foreman-kit update` renames it.
const LegacyPolicyFile = ".foreman-kit/jev_policy.yaml"

// loadEmbeddedSeedPolicy parses the embedded seed of PolicyFile.
func loadEmbeddedSeedPolicy() (*Policy, error) {
	raw, err := assets.ReadSeed(PolicyFile)
	if err != nil {
		return nil, err
	}
	var pol Policy
	if err := yaml.Unmarshal(raw, &pol); err != nil {
		return nil, err
	}
	return &pol, nil
}

// LoadPolicy loads .foreman-kit/evaluator.yaml from root (falling back to embedded seed),
// and fills any omitted gate questions/canary from the embedded seed policy.
func LoadPolicy(root string) (*Policy, error) {
	var raw []byte
	var err error
	if root != "" {
		// A worktree without the file reads the one of the main checkout, like every other
		// setting: a change cut before the file was renamed still has it under its old name. So
		// does one cut before `foreman-kit update` rewrote the files (settings.FilesRoot).
		dirs := []string{settings.FilesRoot(root)}
		if main := store.MainRoot(root); main != dirs[0] {
			dirs = append(dirs, main)
		}
		found := false
		for _, dir := range dirs {
			if p := filepath.Join(dir, filepath.FromSlash(PolicyFile)); fileExists(p) {
				raw, err = os.ReadFile(p)
				found = true
				break
			}
		}
		if !found {
			for _, dir := range dirs {
				if fileExists(filepath.Join(dir, filepath.FromSlash(LegacyPolicyFile))) {
					// An application installed before the file was renamed still keeps its gates
					// under the old name. The seed would evaluate with other gates than the ones
					// it set, so nothing is evaluated until the file is renamed.
					return nil, fmt.Errorf("%s has not been renamed to %s: run foreman-kit update", LegacyPolicyFile, PolicyFile)
				}
			}
		}
	}
	if len(raw) == 0 && err == nil {
		raw, err = assets.ReadSeed(PolicyFile)
	}
	if err != nil {
		return nil, err
	}

	var pol Policy
	if err := yaml.Unmarshal(raw, &pol); err != nil {
		return nil, err
	}
	// The settings of the file take their names and what they accept from the catalog, read from
	// the same bytes, with what `--set` gave this run in their place. What a run is given after
	// the policy was loaded is applied when the value is used (EffectiveModel, EffectiveBaseURL,
	// Disabled).
	var data map[string]any
	_ = yaml.Unmarshal(raw, &data)
	r := (&settings.Reader{Root: root}).Use(settings.EvaluatorFile, data)
	pol.Enabled = r.Bool(settings.EvaluatorEnabled)
	pol.Model = r.String(settings.EvaluatorModel)
	pol.BaseURL = r.String(settings.EvaluatorBaseURL)
	pol.Timeout = settings.FormatDuration(r.Duration(settings.EvaluatorTimeout))
	pol.Retries = r.Int(settings.EvaluatorRetries)
	pol.State.MaxChars = r.Int(settings.EvaluatorStateMaxChars)
	includeDiffs := r.Bool(settings.EvaluatorIncludeDiffs)
	pol.State.IncludeDiffs = &includeDiffs
	if len(pol.AbstainBand) != 2 {
		band, _ := settings.Of(settings.EvaluatorAbstainBand).Default.([]float64)
		pol.AbstainBand = append([]float64(nil), band...)
	}
	pol.WhenUnavailable = r.String(settings.EvaluatorWhenUnavailable)
	if pol.Gates == nil {
		pol.Gates = make(map[string]GatePolicy)
	}
	// The mode a run was given for a gate (--set evaluator.gates.plan_review.mode=off).
	for name, gate := range pol.Gates {
		if _, known := settings.Lookup(settings.GateMode(name)); !known {
			continue
		}
		if raw, given := settings.GivenForThisRun(settings.GateMode(name)); given {
			gate.Mode = fmt.Sprint(raw)
			pol.Gates[name] = gate
		}
	}

	if seedPol, err := loadEmbeddedSeedPolicy(); err == nil && seedPol != nil {
		if pol.Canary == nil && seedPol.Canary != nil {
			c := *seedPol.Canary
			pol.Canary = &c
		}
		for gateName, g := range pol.Gates {
			if len(g.Questions) == 0 {
				if sg, ok := seedPol.Gates[gateName]; ok && len(sg.Questions) > 0 {
					g.Questions = sg.Questions
					pol.Gates[gateName] = g
				}
			}
		}
	}
	return &pol, nil
}

// EffectiveModel returns the model that answers the gates: the one `--set` gave this run, else the
// one the policy names.
func (p *Policy) EffectiveModel() string {
	model := settings.Of(settings.EvaluatorModel)
	if raw, given := settings.GivenForThisRun(model.Key); given {
		if name, ok := raw.(string); ok && strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name)
		}
	}
	if p != nil && p.Model != "" {
		return p.Model
	}
	fallback, _ := model.Default.(string)
	return fallback
}

// TimeoutDuration returns the limit of one attempt.
func (p *Policy) TimeoutDuration() time.Duration {
	if p != nil {
		if d, ok := settings.ParseDuration(p.Timeout); ok {
			return d
		}
	}
	fallback, _ := settings.Of(settings.EvaluatorTimeout).Default.(string)
	d, _ := settings.ParseDuration(fallback)
	return d
}

// EffectiveBaseURL returns the endpoint the evaluator is called at: the one `--set` gave this run,
// else the one the policy names ("" for the provider's own).
func (p *Policy) EffectiveBaseURL() string {
	if raw, given := settings.GivenForThisRun(settings.EvaluatorBaseURL); given {
		if url, ok := raw.(string); ok {
			return strings.TrimSpace(url)
		}
	}
	if p != nil {
		return p.BaseURL
	}
	return ""
}

// Disabled reports whether every gate is off: the policy says so (enabled: false), or the run was
// given it (--set evaluator.enabled=false).
func (p *Policy) Disabled() bool {
	if p == nil || !p.Enabled {
		return true
	}
	return settings.EvaluatorOffForThisRun()
}

func (p *Policy) GateMode(gate string) string {
	if p.Disabled() {
		return settings.GateOff
	}
	g, ok := p.Gates[gate]
	if !ok || g.Mode == "" {
		return settings.GateOff
	}
	// Handle YAML 1.1 legacy boolean false if someone wrote unquoted false
	if g.Mode == "false" {
		return settings.GateOff
	}
	return g.Mode
}

// ActiveGates returns, sorted, the gates whose effective mode is advisory or enforce.
func (p *Policy) ActiveGates() []string {
	if p == nil {
		return nil
	}
	var active []string
	for name := range p.Gates {
		switch p.GateMode(name) {
		case "advisory", "enforce":
			active = append(active, name)
		}
	}
	sort.Strings(active)
	return active
}

// RequireAPIKeyForActiveGates fails when a gate is active (advisory or enforce)
// and no evaluator API key is exported in the environment (TYPESAFE_API_KEY).
// A credentials file does not satisfy it. To run without a
// key every gate in .foreman-kit/evaluator.yaml must be set to mode: off.
func (p *Policy) RequireAPIKeyForActiveGates() error {
	active := p.ActiveGates()
	if len(active) == 0 || APIKeyFromEnv() != "" {
		return nil
	}
	return fmt.Errorf("gates of .foreman-kit/evaluator.yaml are active without TYPESAFE_API_KEY (%s): export it or set every gate to mode: off",
		strings.Join(active, ", "))
}

// ParseCondition parses "<question_id> (>=|<=|==|>|<) <threshold>".
func ParseCondition(cond string) (question, op string, threshold float64, err error) {
	parts := strings.Fields(strings.TrimSpace(cond))
	if len(parts) != 3 {
		return "", "", 0, fmt.Errorf("invalid assert condition %q (expected '<question> <op> <threshold>')", cond)
	}
	question = parts[0]
	op = parts[1]
	switch op {
	case ">=", "<=", "==", ">", "<":
	default:
		return "", "", 0, fmt.Errorf("unsupported operator %q in %q", op, cond)
	}
	threshold, err = strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid threshold %q in %q: %w", parts[2], cond, err)
	}
	return question, op, threshold, nil
}

// ValidateConditionsAgainstTemplate ensures every assert references a numeric (noul or score) question in tf.
func ValidateConditionsAgainstTemplate(tf *TemplateFile, asserts []string) error {
	for _, cond := range asserts {
		qID, _, _, err := ParseCondition(cond)
		if err != nil {
			return err
		}
		q, ok := tf.RawQuestions[qID]
		if !ok {
			return fmt.Errorf("assert %q references unknown question %q", cond, qID)
		}
		if q.Type != "noul" && q.Type != "score" {
			return fmt.Errorf("assert %q references non-numeric question %q (type %s)", cond, qID, q.Type)
		}
	}
	return nil
}

// EvaluateConditions evaluates assert conditions against an answers map.
func EvaluateConditions(answers map[string]map[string]any, asserts []string) (*GateResult, error) {
	if len(asserts) == 0 {
		return &GateResult{Passed: true}, nil
	}
	res := &GateResult{
		Passed:     true,
		Conditions: make([]ConditionResult, 0, len(asserts)),
	}
	for _, cond := range asserts {
		qID, op, threshold, err := ParseCondition(cond)
		if err != nil {
			return nil, err
		}
		ans, ok := answers[qID]
		if !ok {
			return nil, fmt.Errorf("missing answer for question %q", qID)
		}
		var actual float64
		if v, ok := toFloat(ans["noul"]); ok {
			actual = v
		} else if v, ok := toFloat(ans["score"]); ok {
			actual = v
		} else {
			return nil, fmt.Errorf("question %q has no numeric noul/score value", qID)
		}

		passed := false
		switch op {
		case ">=":
			passed = actual >= threshold
		case "<=":
			passed = actual <= threshold
		case "==":
			passed = actual == threshold
		case ">":
			passed = actual > threshold
		case "<":
			passed = actual < threshold
		}
		if !passed {
			res.Passed = false
		}
		res.Conditions = append(res.Conditions, ConditionResult{
			Condition: cond,
			Question:  qID,
			Operator:  op,
			Threshold: threshold,
			Actual:    actual,
			Passed:    passed,
		})
	}
	return res, nil
}

// BlockingRules returns the block_when rules of g that v matches, unless an allow_when rule matches.
func BlockingRules(v Verdict, g GatePolicy) []RoutingRule {
	for i := range g.AllowWhen {
		if MatchesRule(v, &g.AllowWhen[i]) {
			return nil
		}
	}
	var hit []RoutingRule
	for i := range g.BlockWhen {
		if MatchesRule(v, &g.BlockWhen[i]) {
			hit = append(hit, g.BlockWhen[i])
		}
	}
	return hit
}

// ShouldBlock reports whether v matches a block_when rule of g that no allow_when rule overrides.
func ShouldBlock(v Verdict, g GatePolicy) bool { return len(BlockingRules(v, g)) > 0 }

// RuleLabel renders a routing rule for reports, e.g. "importance=critical" or "risk>=0.8".
func RuleLabel(r RoutingRule) string {
	switch {
	case r.Choice != "":
		return r.Question + "=" + r.Choice
	case r.Min != nil:
		return fmt.Sprintf("%s>=%v", r.Question, *r.Min)
	case r.Max != nil:
		return fmt.Sprintf("%s<=%v", r.Question, *r.Max)
	}
	return r.Question
}

// MatchesRule reports whether a Verdict's answers match a RoutingRule.
func MatchesRule(v Verdict, rule *RoutingRule) bool {
	if rule == nil || rule.Question == "" {
		return false
	}
	ans, ok := v.Answers[rule.Question]
	if !ok {
		return false
	}
	if rule.Choice != "" {
		ch, _ := ans["choice"].(string)
		conf, _ := toFloat(ans["confidence"])
		minConf := 0.0
		if rule.MinConfidence != nil {
			minConf = *rule.MinConfidence
		}
		return ch == rule.Choice && conf >= minConf
	}
	if rule.Min != nil {
		val, ok := toFloat(ans["noul"])
		return ok && val >= *rule.Min
	}
	if rule.Max != nil {
		val, ok := toFloat(ans["noul"])
		return ok && val <= *rule.Max
	}
	return false
}

// SummarizeAnswers produces the compact map logged in evaluator.answer events.
func SummarizeAnswers(answers map[string]map[string]any) map[string]any {
	out := make(map[string]any, len(answers))
	for k, a := range answers {
		switch a["type"] {
		case "noul":
			out[k] = a["noul"]
		case "choice":
			out[k] = fmt.Sprintf("%v@%v", a["choice"], a["confidence"])
		case "score":
			out[k] = a["score"]
		default:
			out[k] = a
		}
	}
	return out
}

// Feedback formats repair feedback for an agent when a gate rejects an artifact.
func Feedback(root string, pol *Policy, v Verdict) string {
	if pol == nil || len(pol.Gates) == 0 {
		return fmt.Sprintf("The evaluator's %s gate rejected this.", v.Gate)
	}
	if _, ok := pol.Gates[v.Gate]; !ok {
		return fmt.Sprintf("The evaluator's %s gate rejected this.", v.Gate)
	}
	tf, _, err := LoadGateTemplate(root, v.Gate, pol)
	var lines []string
	for _, c := range v.Failed {
		var inst, yesCrit string
		if err == nil && tf != nil {
			if q, ok := tf.RawQuestions[c.Question]; ok {
				inst = q.Instructions
				yesCrit = q.ChoiceCriteria()["true"]
			}
		}
		lines = append(lines, fmt.Sprintf("- %s failed (measured %v). Question: %s A yes means: %s",
			c.Condition, c.Actual, inst, yesCrit))
	}
	return fmt.Sprintf("The evaluator's %s gate (a calibrated yes/no judge, model %s) rejected this:\n%s",
		v.Gate, v.Model, strings.Join(lines, "\n"))
}

// DefaultMaxStateTotalBytes caps the total JSON payload size sent to the evaluator (~15,000 tokens) so
// multi-capability changes never trigger exit 2 (rule: size-question) in production.
const DefaultMaxStateTotalBytes = 60000

// ClipState recursively clips strings exceeding maxChars and enforces the global JSON byte budget.
func ClipState(v any, maxChars int) any {
	if maxChars <= 0 {
		maxChars, _ = settings.Of(settings.EvaluatorStateMaxChars).Default.(int)
	}
	maxTotalBytes := DefaultMaxStateTotalBytes
	if maxChars*3 < maxTotalBytes {
		maxTotalBytes = maxChars * 3
	}
	return ClipStateWithBudget(v, maxChars, maxTotalBytes)
}

// ClipStateWithBudget clips individual strings to maxChars and iteratively shrinks maxChars
// if the serialized JSON exceeds maxTotalBytes.
func ClipStateWithBudget(v any, maxChars int, maxTotalBytes int) any {
	if maxChars <= 0 {
		maxChars, _ = settings.Of(settings.EvaluatorStateMaxChars).Default.(int)
	}
	if maxTotalBytes <= 0 {
		maxTotalBytes = DefaultMaxStateTotalBytes
	}
	curMax := maxChars
	for attempt := 0; attempt < 6; attempt++ {
		clipped := clipStringsRecursive(v, curMax)
		b, err := json.Marshal(clipped)
		if err != nil || len(b) <= maxTotalBytes || curMax <= 256 {
			return clipped
		}
		ratio := float64(maxTotalBytes) / float64(len(b)) * 0.80
		nextMax := int(float64(curMax) * ratio)
		if nextMax >= curMax {
			nextMax = curMax / 2
		}
		if nextMax < 256 {
			nextMax = 256
		}
		curMax = nextMax
	}
	return clipStringsRecursive(v, curMax)
}

func clipStringsRecursive(v any, maxChars int) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			out[k] = clipStringsRecursive(item, maxChars)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = clipStringsRecursive(item, maxChars)
		}
		return out
	case string:
		if len(val) > maxChars {
			return fmt.Sprintf("%s\n[... clipped %d characters]", val[:maxChars], len(val)-maxChars)
		}
		return val
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return v
		}
		var generic any
		if err := json.Unmarshal(b, &generic); err != nil {
			return v
		}
		switch generic.(type) {
		case map[string]any, []any, string:
			return clipStringsRecursive(generic, maxChars)
		default:
			return v
		}
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
