package evaluator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	gojev "github.com/shanehull/go-jev"

	"github.com/mayeco/foreman-kit/internal/store"
)

// Judge wraps the native github.com/shanehull/go-jev client, policy, and SQLite cache.
type Judge struct {
	root    string
	policy  *Policy
	store   *store.Store
	baseURL string
}

// NewJudge creates a Judge for root backed by st.
func NewJudge(root string, st *store.Store) (*Judge, error) {
	pol, err := LoadPolicy(root)
	if err != nil {
		return nil, err
	}
	return &Judge{
		root:    root,
		policy:  pol,
		store:   st,
		baseURL: pol.EffectiveBaseURL(),
	}, nil
}

func (j *Judge) Policy() *Policy { return j.policy }

// APIKeyFromEnv returns the evaluator's API key exported in the environment: TYPESAFE_API_KEY, the
// provider's own variable.
func APIKeyFromEnv() string {
	for _, envName := range []string{"TYPESAFE_API_KEY"} {
		if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
			return v
		}
	}
	return ""
}

// ResolveAPIKey looks up the Jev API key from environment or local credentials file,
// without requiring the external `jev` CLI.
func ResolveAPIKey() string {
	if v := APIKeyFromEnv(); v != "" {
		return v
	}
	var credPaths []string
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		credPaths = append(credPaths, filepath.Join(xdg, "jev", "credentials.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		credPaths = append(credPaths,
			filepath.Join(home, ".config", "jev", "credentials.json"),
			filepath.Join(home, ".jev", "credentials.json"),
		)
	}
	for _, p := range credPaths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			for _, k := range []string{"api_key", "apiKey", "token", "key"} {
				if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}

// Available reports whether the evaluator is enabled and has an API key configured.
func (j *Judge) Available() bool {
	if j == nil || j.policy.Disabled() {
		return false
	}
	return ResolveAPIKey() != ""
}

// EvaluateTemplate evaluates a gate or template file against state and optional asserts using github.com/shanehull/go-jev.
func (j *Judge) EvaluateTemplate(ctx context.Context, file string, state any, asserts []string, dryRun bool) (*EvalResult, int, error) {
	// The questions come from the Judge's own policy, like its mode, asserts and routing rules. Read
	// again from disk here, a question renamed in .foreman-kit/evaluator.yaml while a daemon ran no longer
	// matched the asserts in memory, and every enforce gate failed as a configuration error.
	tf, _, err := loadTemplateWithPolicy(j.root, file, j.policy)
	if err != nil {
		return nil, 2, err
	}
	if len(asserts) == 0 && j.policy != nil {
		norm := NormalizeGateName(file)
		if norm == "canary" && j.policy.Canary != nil {
			asserts = j.policy.Canary.Assert
		} else if g, ok := j.policy.Gates[norm]; ok {
			asserts = g.Assert
		} else {
			for _, g := range j.policy.Gates {
				if g.File == file {
					asserts = g.Assert
					break
				}
			}
		}
	}
	if err := ValidateConditionsAgainstTemplate(tf, asserts); err != nil {
		return nil, 2, err
	}
	if state == nil {
		state = tf.State
	}
	if state == nil {
		return nil, 2, fmt.Errorf("no state provided for %s", file)
	}

	model := j.policy.EffectiveModel()
	if dryRun {
		return &EvalResult{
			Model:   model,
			Answers: map[string]map[string]any{},
			Gate:    &GateResult{Passed: true},
		}, 0, nil
	}

	apiKey := ResolveAPIKey()
	if apiKey == "" {
		return nil, 3, fmt.Errorf("no TYPESAFE_API_KEY configured")
	}

	hc := &http.Client{Timeout: j.policy.TimeoutDuration()}
	opts := []gojev.ClientOption{
		gojev.WithAPIKey(apiKey),
		gojev.WithModel(model),
		gojev.WithHTTPClient(hc),
		gojev.WithMaxAttempts(j.policy.Retries + 1),
	}
	if j.baseURL != "" {
		opts = append(opts, gojev.WithBaseURL(j.baseURL))
	}
	client, err := gojev.New(opts...)
	if err != nil {
		return nil, 3, err
	}

	var input gojev.Input
	if s, ok := state.(string); ok {
		input = gojev.State(s)
	} else {
		input = gojev.Value(state)
	}

	resp, err := client.SystemOne(ctx, input, tf.Questions, gojev.WithRequestModel(model))
	if err != nil {
		var apiErr *gojev.APIError
		if errors.As(err, &apiErr) && apiErr.Message == "" && strings.TrimSpace(apiErr.Body) != "" {
			return nil, 6, fmt.Errorf("%w: %s", err, strings.TrimSpace(apiErr.Body))
		}
		return nil, 6, err
	}

	answers := convertGoJevAnswers(resp)
	gateRes, err := EvaluateConditions(answers, asserts)
	if err != nil {
		// The asserts were checked against the questions before the request: an answer that is
		// missing or not numeric here is the response's fault, an API failure like any other (6),
		// not a configuration error (2) that an enforce gate reports instead of following
		// when_unavailable.
		return nil, 6, fmt.Errorf("evaluator: unexpected response: %w", err)
	}

	evalRes := &EvalResult{
		Model:   resp.Model,
		Answers: answers,
		Gate:    gateRes,
	}
	if evalRes.Model == "" {
		evalRes.Model = model
	}
	exitCode := 0
	if !gateRes.Passed {
		exitCode = 10
	}
	return evalRes, exitCode, nil
}

// Ask evaluates a named gate from .foreman-kit/evaluator.yaml against state, using SQLite evaluation_cache and emitting events.
func (j *Judge) Ask(ctx context.Context, gateName string, state any, change string) (*Verdict, error) {
	mode := j.policy.GateMode(gateName)
	if mode == "off" {
		return &Verdict{Gate: gateName, Mode: mode, Status: "skipped"}, nil
	}
	if !j.Available() {
		return j.handleUnavailable(ctx, gateName, mode, change, "the evaluator has no API key configured")
	}

	cfg, ok := j.policy.Gates[gateName]
	if !ok {
		return &Verdict{Gate: gateName, Mode: "off", Status: "skipped"}, nil
	}
	_, tplRaw, err := LoadGateTemplate(j.root, gateName, j.policy)
	if err != nil {
		return nil, err
	}
	target := gateName
	if cfg.File != "" {
		target = cfg.File
	}

	clipped := ClipState(state, j.policy.State.MaxChars)
	stateBytes, err := json.Marshal(clipped)
	if err != nil {
		return nil, err
	}
	model := j.policy.EffectiveModel()

	keyMaterial, _ := json.Marshal([]any{gateName, string(tplRaw), string(stateBytes), model, cfg.Assert})
	sum := sha256.Sum256(keyMaterial)
	cacheKey := hex.EncodeToString(sum[:])[:16]

	var evalRes EvalResult
	cached := false
	if j.store != nil {
		if cachedJSON, ok, err := j.store.GetEvaluationCache(ctx, cacheKey); err == nil && ok {
			if json.Unmarshal([]byte(cachedJSON), &evalRes) == nil {
				cached = true
			}
		}
	}

	if !cached {
		res, code, err := j.EvaluateTemplate(ctx, target, clipped, cfg.Assert, false)
		if isSizeOrPayloadError(err) {
			// Automatically halve total byte budget and retry once on size-question / 413 / 422 rejection
			smaller := ClipStateWithBudget(state, j.policy.State.MaxChars/4, DefaultMaxStateTotalBytes/2)
			res, code, err = j.EvaluateTemplate(ctx, target, smaller, cfg.Assert, false)
			if err == nil {
				clipped = smaller
				stateBytes, _ = json.Marshal(clipped)
			}
		}
		if err != nil {
			// An advisory gate must NEVER return an error that pauses the pipeline
			if mode == "advisory" || isSizeOrPayloadError(err) {
				return j.handleUnavailable(ctx, gateName, mode, change, err.Error())
			}
			if code == 2 {
				return nil, err
			}
			return j.handleUnavailable(ctx, gateName, mode, change, err.Error())
		}
		evalRes = *res
		if j.store != nil {
			if b, err := json.Marshal(evalRes); err == nil {
				_ = j.store.PutEvaluationCache(ctx, cacheKey, gateName, model, string(stateBytes), string(b))
			}
		}
	}

	return j.buildVerdict(ctx, gateName, mode, evalRes, change, cached, cacheKey), nil
}

func isSizeOrPayloadError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	var apiErr *gojev.APIError
	if errors.As(err, &apiErr) && apiErr.Body != "" {
		s += " " + strings.ToLower(apiErr.Body)
	}
	return strings.Contains(s, "size-question") ||
		strings.Contains(s, "too large") ||
		strings.Contains(s, "entity too large") ||
		strings.Contains(s, "413")
}

func (j *Judge) handleUnavailable(ctx context.Context, gateName, mode, change, detail string) (*Verdict, error) {
	var evaluationID int64
	if j.store != nil {
		// The question that got no answer is on record too, with the event that announces it.
		evaluationID, _ = j.store.RecordEvaluation(ctx, store.Evaluation{
			Gate:       gateName,
			Mode:       mode,
			Status:     "unavailable",
			ChangeName: change,
		}, &store.EventInput{
			Event:  "evaluator.unavailable",
			Change: change,
			Payload: map[string]any{
				"gate":   gateName,
				"detail": detail,
			},
		})
	}
	if mode == "enforce" && j.policy.BlocksWhenUnavailable() {
		return &Verdict{Gate: gateName, Mode: mode, Status: "unavailable", EvaluationID: evaluationID}, fmt.Errorf("judge_unavailable: %s: %s", gateName, detail)
	}
	return &Verdict{Gate: gateName, Mode: mode, Status: "unavailable", EvaluationID: evaluationID}, nil
}

func (j *Judge) buildVerdict(ctx context.Context, gateName, mode string, data EvalResult, change string, cached bool, cacheKey string) *Verdict {
	low, high := 0.4, 0.6
	if len(j.policy.AbstainBand) == 2 {
		low, high = j.policy.AbstainBand[0], j.policy.AbstainBand[1]
	}

	var missed []ConditionResult
	var undecided, hardFailed []ConditionResult
	if data.Gate != nil {
		for _, c := range data.Gate.Conditions {
			if !c.Passed {
				missed = append(missed, c)
				if c.Actual >= low && c.Actual <= high {
					undecided = append(undecided, c)
				} else {
					hardFailed = append(hardFailed, c)
				}
			}
		}
	}

	status := "passed"
	if len(hardFailed) > 0 {
		status = "failed"
	} else if len(undecided) > 0 {
		status = "abstained"
	}

	var evaluationID int64
	if j.store != nil {
		summaryBytes, _ := json.Marshal(SummarizeAnswers(data.Answers))
		cost := data.CostUSD
		if cached {
			cost = 0
		}
		// The evaluation and the event that announces it are written together: who asked (the change,
		// and the worker run or merge attempt in the scope of ctx) is on the evaluation.
		evaluationID, _ = j.store.RecordEvaluation(ctx, store.Evaluation{
			Gate:        gateName,
			Mode:        mode,
			Status:      status,
			Model:       data.Model,
			Cached:      cached,
			CacheKey:    cacheKey,
			CostUSD:     cost,
			AnswersJSON: string(summaryBytes),
			ChangeName:  change,
		}, &store.EventInput{
			Event:  "evaluator.answer",
			Change: change,
			Payload: map[string]any{
				"gate":     gateName,
				"mode":     mode,
				"status":   status,
				"model":    data.Model,
				"answers":  string(summaryBytes),
				"cost_usd": cost,
				"cached":   cached,
			},
		})
	}

	return &Verdict{
		EvaluationID: evaluationID,
		Gate:         gateName,
		Mode:         mode,
		Status:       status,
		Answers:      data.Answers,
		Failed:       missed,
		Model:        data.Model,
		Cached:       cached,
	}
}

func convertGoJevAnswers(resp *gojev.Response) map[string]map[string]any {
	out := make(map[string]map[string]any, len(resp.Answers))
	for k, ans := range resp.Answers {
		switch a := ans.(type) {
		case gojev.NoulAnswer:
			out[k] = map[string]any{
				"type": "noul",
				"noul": a.Noul,
			}
		case gojev.ChoiceAnswer:
			out[k] = map[string]any{
				"type":          "choice",
				"choice":        a.Choice,
				"confidence":    a.Confidence,
				"probabilities": a.Probabilities,
			}
		case gojev.ScoreAnswer:
			out[k] = map[string]any{
				"type":          "score",
				"score":         a.Score,
				"confidence":    a.Confidence,
				"legend":        a.Legend,
				"probabilities": a.Probabilities,
			}
		}
	}
	return out
}
