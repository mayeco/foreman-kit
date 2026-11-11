package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"
	"github.com/zendev-sh/goai/provider/anthropic"
	"github.com/zendev-sh/goai/provider/compat"
	"github.com/zendev-sh/goai/provider/google"
	"github.com/zendev-sh/goai/provider/ollama"
	"github.com/zendev-sh/goai/provider/openai"
	"github.com/zendev-sh/goai/provider/openrouter"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// Config holds declarative configuration for structured LLM calls (from .foreman-kit/pipeline.yaml -> llm or environment).
type Config struct {
	Enabled        bool   `json:"enabled"         yaml:"enabled"`
	Provider       string `json:"provider"        yaml:"provider"`
	Model          string `json:"model"           yaml:"model"`
	BaseURL        string `json:"base_url"        yaml:"base_url"`
	APIKey         string `json:"-"               yaml:"-"`
	TimeoutSeconds int    `json:"timeout_seconds" yaml:"timeout_seconds"`
	MaxRetries     int    `json:"max_retries"     yaml:"max_retries"`
}

// Client wraps github.com/zendev-sh/goai (GenerateObject[T] + SchemaFrom[T]) with
// SQLite caching (llm_cache), validator/v10 self-correction retries, and telemetry events.
type Client struct {
	cfg   Config
	store *store.Store
}

// LoadConfig returns the `diagnostics:` section of <root>/.foreman-kit/pipeline.yaml as the settings
// catalog reads it: each key as --set gave it to this run, else as the file holds it. Without a
// root they are the values the kit ships, with what --set gave the run over them.
func LoadConfig(root string) Config {
	r := settings.At(root)
	if root == "" {
		r.Use(settings.DiagnosticsEnabled.File(), nil)
		r.Stderr = io.Discard
	}
	cfg := Config{
		Enabled:        r.Bool(settings.DiagnosticsEnabled),
		Provider:       r.String(settings.DiagnosticsProvider),
		Model:          r.String(settings.DiagnosticsModel),
		BaseURL:        r.String(settings.DiagnosticsBaseURL),
		TimeoutSeconds: r.Seconds(settings.DiagnosticsTimeout),
		MaxRetries:     r.Int(settings.DiagnosticsRetries),
	}
	cfg.APIKey = resolveAPIKey(cfg.Provider)
	if cfg.Provider == "ollama" && cfg.APIKey == "" {
		cfg.APIKey = "ollama"
	}
	return cfg
}

// apiKeyEnvFor lists the environment variables that may hold provider's API key, most specific first.
// A key is never taken from another vendor's variable: that sends the secret to the wrong API (and every
// call fails). An OpenAI-compatible "compat" endpoint needs FOREMANKIT_DIAGNOSTICS_API_KEY, so an OpenAI key is
// never sent to a third-party base URL.
func apiKeyEnvFor(provider string) []string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic":
		return []string{"FOREMANKIT_DIAGNOSTICS_API_KEY", "ANTHROPIC_API_KEY"}
	case "google", "gemini":
		return []string{"FOREMANKIT_DIAGNOSTICS_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"}
	case "openrouter":
		return []string{"FOREMANKIT_DIAGNOSTICS_API_KEY", "OPENROUTER_API_KEY"}
	case "ollama", "compat":
		return []string{"FOREMANKIT_DIAGNOSTICS_API_KEY"}
	default:
		return []string{"FOREMANKIT_DIAGNOSTICS_API_KEY", "OPENAI_API_KEY"}
	}
}

// resolveAPIKey returns the first non-empty API key variable for provider.
func resolveAPIKey(provider string) string {
	for _, env := range apiKeyEnvFor(provider) {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	return ""
}

// NewClient constructs a structured LLM client with defaults applied.
func NewClient(cfg Config, st *store.Store) *Client {
	if cfg.Provider == "" {
		cfg.Provider, _ = settings.Of(settings.DiagnosticsProvider).Default.(string)
	}
	if cfg.Model == "" {
		cfg.Model, _ = settings.Of(settings.DiagnosticsModel).Default.(string)
	}
	if cfg.TimeoutSeconds <= 0 {
		shipped, _ := settings.Of(settings.DiagnosticsTimeout).Default.(string)
		limit, _ := settings.ParseDuration(shipped)
		cfg.TimeoutSeconds = int(limit / time.Second)
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries, _ = settings.Of(settings.DiagnosticsRetries).Default.(int)
	}
	if cfg.APIKey == "" {
		cfg.APIKey = resolveAPIKey(cfg.Provider)
	}
	if cfg.Provider == "ollama" && cfg.APIKey == "" {
		cfg.APIKey = "ollama"
	}
	return &Client{cfg: cfg, store: st}
}

// NewClientForRoot creates a Client configured from <root>/.foreman-kit/pipeline.yaml, with its key from
// the environment.
func NewClientForRoot(root string, st *store.Store) *Client {
	return NewClient(LoadConfig(root), st)
}

// Available reports whether the structured LLM client is enabled and has an API key configured.
func (c *Client) Available() bool {
	return c != nil && c.cfg.Enabled && c.cfg.APIKey != ""
}

// languageModel builds the appropriate zendev-sh/goai provider.LanguageModel for c.cfg.Provider.
func (c *Client) languageModel() provider.LanguageModel {
	switch strings.ToLower(strings.TrimSpace(c.cfg.Provider)) {
	case "anthropic":
		opts := []anthropic.Option{anthropic.WithAPIKey(c.cfg.APIKey)}
		if c.cfg.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(c.cfg.BaseURL))
		}
		return anthropic.Chat(c.cfg.Model, opts...)
	case "google", "gemini":
		opts := []google.Option{google.WithAPIKey(c.cfg.APIKey)}
		if c.cfg.BaseURL != "" {
			opts = append(opts, google.WithBaseURL(c.cfg.BaseURL))
		}
		return google.Chat(c.cfg.Model, opts...)
	case "openrouter":
		opts := []openrouter.Option{openrouter.WithAPIKey(c.cfg.APIKey)}
		if c.cfg.BaseURL != "" {
			opts = append(opts, openrouter.WithBaseURL(c.cfg.BaseURL))
		}
		return openrouter.Chat(c.cfg.Model, opts...)
	case "ollama":
		var opts []ollama.Option
		if c.cfg.BaseURL != "" {
			opts = append(opts, ollama.WithBaseURL(c.cfg.BaseURL))
		}
		return ollama.Chat(c.cfg.Model, opts...)
	case "compat":
		opts := []compat.Option{compat.WithAPIKey(c.cfg.APIKey)}
		if c.cfg.BaseURL != "" {
			opts = append(opts, compat.WithBaseURL(c.cfg.BaseURL))
		}
		return compat.Chat(c.cfg.Model, opts...)
	default:
		opts := []openai.Option{openai.WithAPIKey(c.cfg.APIKey)}
		if c.cfg.BaseURL != "" {
			opts = append(opts, openai.WithBaseURL(c.cfg.BaseURL))
		}
		return openai.Chat(c.cfg.Model, opts...)
	}
}

// ExtractStructured executes a typed and validated extraction with zendev-sh/goai GenerateObject[T],
// backed by SQLite llm_cache and go-playground/validator/v10 self-correction retries.
func ExtractStructured[T any](
	ctx context.Context,
	c *Client,
	taskKind string,
	changeName string,
	systemPrompt string,
	userContent string,
	out *T,
) error {
	if !c.Available() {
		return fmt.Errorf("llm: structured client is not enabled or has no API key")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	keyRaw, _ := json.Marshal([]string{taskKind, c.cfg.Provider, c.cfg.Model, systemPrompt, userContent})
	sum := sha256.Sum256(keyRaw)
	cacheKey := hex.EncodeToString(sum[:])[:24]

	if c.store != nil {
		if cachedJSON, ok, err := c.store.GetLLMCache(ctx, cacheKey); err == nil && ok {
			if err := json.Unmarshal([]byte(cachedJSON), out); err == nil {
				_ = c.store.EmitEvent(ctx, store.EventInput{
					Event:  "llm.extract",
					Change: changeName,
					Payload: map[string]any{
						"task_kind": taskKind,
						"provider":  c.cfg.Provider,
						"model":     c.cfg.Model,
						"cached":    true,
					},
				})
				return nil
			}
		}
	}

	model := c.languageModel()
	val := validator.New()
	msgs := []provider.Message{
		goai.UserMessage(userContent),
	}

	var totalIn, totalOut int
	var lastErr error
	maxAttempts := c.cfg.MaxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		res, err := goai.GenerateObject[T](
			ctx,
			model,
			goai.WithSystem(systemPrompt),
			goai.WithMessages(msgs...),
			goai.WithTemperature(0),
			goai.WithTimeout(time.Duration(c.cfg.TimeoutSeconds)*time.Second),
			goai.WithMaxRetries(1),
		)
		if err != nil {
			lastErr = err
			break
		}
		totalIn += res.Usage.InputTokens
		totalOut += res.Usage.OutputTokens

		if vErr := val.Struct(res.Object); vErr != nil {
			lastErr = fmt.Errorf("validation failed: %w", vErr)
			if attempt < maxAttempts {
				prevJSON, _ := json.Marshal(res.Object)
				msgs = append(msgs,
					goai.AssistantMessage(string(prevJSON)),
					goai.UserMessage(fmt.Sprintf("Validation failed: %s. Fix the invalid fields and return a valid JSON object.", vErr.Error())),
				)
				continue
			}
			return lastErr
		}

		*out = res.Object
		if c.store != nil {
			if b, err := json.Marshal(out); err == nil {
				_ = c.store.PutLLMCache(ctx, cacheKey, taskKind, c.cfg.Model, string(b), totalIn, totalOut)
			}
			_ = c.store.EmitEvent(ctx, store.EventInput{
				Event:  "llm.extract",
				Change: changeName,
				Payload: map[string]any{
					"task_kind":     taskKind,
					"provider":      c.cfg.Provider,
					"model":         c.cfg.Model,
					"attempts":      attempt,
					"cached":        false,
					"input_tokens":  totalIn,
					"output_tokens": totalOut,
				},
			})
		}
		return nil
	}
	return lastErr
}

// =============================================================================
// High-level Domain Methods for the 8 Use Cases
// =============================================================================

// 1. DiagnoseGateFailure extracts a surgical GateFailureDiagnosis from failing gate output and delta specs.
func (c *Client) DiagnoseGateFailure(ctx context.Context, change, gateOutput string, deltaSpecs map[string]string) (*GateFailureDiagnosis, error) {
	payload, _ := json.Marshal(map[string]any{
		"change":      change,
		"gate_output": gateOutput,
		"delta_specs": deltaSpecs,
	})
	var out GateFailureDiagnosis
	err := ExtractStructured(
		ctx, c, "gate_diagnosis", change,
		"You are a deterministic build-failure diagnostician. Extract the exact failing gate, category, implicated files, violated scenario, root cause, and repair checklist.",
		string(payload),
		&out,
	)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// 2. ExplainRejection translates a numeric evaluator gate failure into specific deficient items.
func (c *Client) ExplainRejection(ctx context.Context, change, gate string, failedConditions any, artifacts map[string]any) (*RejectionBreakdown, error) {
	payload, _ := json.Marshal(map[string]any{
		"change":            change,
		"gate":              gate,
		"failed_conditions": failedConditions,
		"artifacts":         artifacts,
	})
	var out RejectionBreakdown
	err := ExtractStructured(
		ctx, c, "rejection_breakdown", change,
		"You are an OpenSpec specification auditor. Identify the exact scenario, requirement, or task sections that caused the calibrated judge condition to fail.",
		string(payload),
		&out,
	)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// 3. AuditPlanSemantics performs pre-BUILD semantic validation of OpenSpec artifacts.
func (c *Client) AuditPlanSemantics(ctx context.Context, change, proposal, design, tasks string, deltaSpecs map[string]string, protectedPaths []string) (*PlanSemanticValidation, error) {
	payload, _ := json.Marshal(map[string]any{
		"change":          change,
		"proposal":        proposal,
		"design":          design,
		"tasks":           tasks,
		"delta_specs":     deltaSpecs,
		"protected_paths": protectedPaths,
	})
	var out PlanSemanticValidation
	err := ExtractStructured(
		ctx, c, "plan_audit", change,
		"Audit the OpenSpec change plan before BUILD. Flag vague scenarios, uncovered scenarios missing tasks in tasks.md, or any attempt to modify protected repository paths.",
		string(payload),
		&out,
	)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// 4. NormalizeProposals performs semantic deduplication and dependency/lock inference across proposals.
func (c *Client) NormalizeProposals(ctx context.Context, proposals any, activeBacklog any, canonicalCapabilities []string) (*ProposalNormalizationBatch, error) {
	payload, _ := json.Marshal(map[string]any{
		"proposals":              proposals,
		"active_backlog":         activeBacklog,
		"canonical_capabilities": canonicalCapabilities,
	})
	var out ProposalNormalizationBatch
	err := ExtractStructured(
		ctx, c, "proposal_dedup", "",
		"Analyze candidate proposals against active backlog items and canonical capabilities. Detect semantic duplicates and infer depends_on and locks.",
		string(payload),
		&out,
	)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// 5. SummarizeRebaseConflict extracts a RebaseConflictBrief when a child repo rebase hits content conflicts.
func (c *Client) SummarizeRebaseConflict(ctx context.Context, change, childRepo, conflictOutput string) (*RebaseConflictBrief, error) {
	payload, _ := json.Marshal(map[string]any{
		"change":          change,
		"child_repo":      childRepo,
		"conflict_output": conflictOutput,
	})
	var out RebaseConflictBrief
	err := ExtractStructured(
		ctx, c, "rebase_brief", change,
		"Analyze the git rebase conflict output and summarize upstream intent, worker intent, and a step-by-step reconciliation guide for the coding agent.",
		string(payload),
		&out,
	)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// 6. CurateMemory classifies handoff.md lessons into the files of the memory (.agent-memory/).
func (c *Client) CurateMemory(ctx context.Context, change, handoffText string, existingMemory map[string]string) (*CuratedMemoryPlan, error) {
	payload, _ := json.Marshal(map[string]any{
		"change":          change,
		"handoff":         handoffText,
		"existing_memory": existingMemory,
	})
	var out CuratedMemoryPlan
	err := ExtractStructured(
		ctx, c, "memory_curate", change,
		"Select durable, evidence-backed lessons from handoff.md and route each to a file of .agent-memory/: pitfalls.md, conventions.md, decisions.md, modules/repo-<repo>.md, or modules/spec-<cap>.md.",
		string(payload),
		&out,
	)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// 7. InferRepoSetup infers non-interactive gate and dev commands for repo.config.yaml from repository files.
func (c *Client) InferRepoSetup(ctx context.Context, repoName string, repoFiles map[string]string) (*InferredRepoSetup, error) {
	payload, _ := json.Marshal(map[string]any{
		"repo_name":  repoName,
		"repo_files": repoFiles,
	})
	var out InferredRepoSetup
	err := ExtractStructured(
		ctx, c, "setup_infer", repoName,
		"Infer exact non-interactive setup, lint, typecheck, test, build, regression, and dev commands for repo.config.yaml.",
		string(payload),
		&out,
	)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// QuarantineTriageEnvelope represents the discriminated union envelope for Use Case 8 using goai.GenerateObject.
type QuarantineTriageEnvelope struct {
	ActionType        string   `json:"action_type"                  jsonschema:"enum=approve_risk,enum=answer_clarification,enum=fix_environment,enum=unblock_and_retry" validate:"required,oneof=approve_risk answer_clarification fix_environment unblock_and_retry"`
	RiskSummary       string   `json:"risk_summary,omitempty"       jsonschema:"description=For approve_risk: why the evaluator flagged this change as high risk"`
	SafetyCheck       string   `json:"safety_check,omitempty"       jsonschema:"description=For approve_risk: what the human should verify in the diff"`
	QuestionFromAgent string   `json:"question_from_agent,omitempty" jsonschema:"description=For answer_clarification: core question from .needs-human"`
	SuggestedOptions  []string `json:"suggested_options,omitempty"  jsonschema:"description=For answer_clarification: 2-3 concrete product options"`
	Service           string   `json:"service,omitempty"            jsonschema:"description=For fix_environment: missing service or toolchain"`
	Command           string   `json:"command,omitempty"            jsonschema:"description=For fix_environment: exact terminal command to run"`
	Reason            string   `json:"reason,omitempty"             jsonschema:"description=For unblock_and_retry: why this failure was transient"`
}

// 8. TriageQuarantineUnion selects and validates one of the 4 operator action recommendation structs for a blocked change.
func (c *Client) TriageQuarantineUnion(ctx context.Context, change, reason, stage, details, needsHuman string) (any, error) {
	payload, _ := json.Marshal(map[string]any{
		"change":      change,
		"reason":      reason,
		"stage":       stage,
		"details":     details,
		"needs_human": needsHuman,
	})
	var env QuarantineTriageEnvelope
	if err := ExtractStructured(
		ctx, c, "quarantine_triage", change,
		"Select the single most appropriate operator action recommendation (approve_risk, answer_clarification, fix_environment, or unblock_and_retry) for this quarantined change.",
		string(payload),
		&env,
	); err != nil {
		return nil, err
	}
	val := validator.New()
	switch env.ActionType {
	case "approve_risk":
		r := ApproveRiskRecommendation{ActionType: env.ActionType, RiskSummary: env.RiskSummary, SafetyCheck: env.SafetyCheck}
		return r, val.Struct(r)
	case "answer_clarification":
		r := AnswerClarificationRecommendation{ActionType: env.ActionType, QuestionFromAgent: env.QuestionFromAgent, SuggestedOptions: env.SuggestedOptions}
		return r, val.Struct(r)
	case "fix_environment":
		r := FixEnvironmentRecommendation{ActionType: env.ActionType, Service: env.Service, Command: env.Command}
		return r, val.Struct(r)
	case "unblock_and_retry":
		r := UnblockAndRetryRecommendation{ActionType: env.ActionType, Reason: env.Reason}
		return r, val.Struct(r)
	default:
		return nil, fmt.Errorf("unknown action_type %q", env.ActionType)
	}
}
