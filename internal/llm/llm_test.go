package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

func writeGoAIJSONResponse(w http.ResponseWriter, obj any) {
	contentBytes, _ := json.Marshal(obj)
	resp := map[string]any{
		"id":      "chatcmpl-goai-test",
		"object":  "chat.completion",
		"created": 1700000000,
		"model":   "gpt-4o-mini",
		"status":  "completed",
		"output": []map[string]any{
			{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{
						"type": "output_text",
						"text": string(contentBytes),
					},
				},
			},
		},
		"choices": []map[string]any{
			{
				"index":         0,
				"finish_reason": "stop",
				"message": map[string]any{
					"role":    "assistant",
					"content": string(contentBytes),
				},
			},
		},
		"usage": map[string]any{
			"input_tokens":      85,
			"output_tokens":     42,
			"prompt_tokens":     85,
			"completion_tokens": 42,
			"total_tokens":      127,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func TestExtractStructuredWithGoAIValidationRetryAndSQLiteCache(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	st, err := store.OpenAt(filepath.Join(tmp, "llm-test.db"), "llm-test", tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Attempt 1 returns an INVALID struct (invalid Category enum & empty FailingFiles),
	// triggering go-playground/validator/v10 self-correction retry on top of zendev-sh/goai GenerateObject[T]!
	// Attempt 2 returns a VALID GateFailureDiagnosis struct.
	httpCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		if httpCalls == 1 {
			writeGoAIJSONResponse(w, map[string]any{
				"failed_gate":      "backend:test",
				"category":         "not_a_valid_enum",
				"failing_files":    []string{},
				"root_cause":       "short",
				"repair_checklist": []string{},
			})
			return
		}
		writeGoAIJSONResponse(w, map[string]any{
			"failed_gate":       "backend:test",
			"category":          "test_assertion",
			"failing_files":     []string{"repos/backend/auth_test.go", "repos/backend/auth.go"},
			"violated_scenario": "#### Scenario: Reject expired JWT token",
			"root_cause":        "ValidateToken returns nil instead of ErrTokenExpired when exp claim is in the past.",
			"repair_checklist": []string{
				"Check token.ExpiresAt against time.Now().UTC() in repos/backend/auth.go",
				"Re-run go test ./... in repos/backend",
			},
		})
	}))
	defer srv.Close()

	client := NewClient(Config{
		Enabled:        true,
		Provider:       "openai",
		Model:          "gpt-4o-mini",
		BaseURL:        srv.URL,
		APIKey:         "test-key",
		TimeoutSeconds: 10,
		MaxRetries:     2,
	}, st)

	diag, err := client.DiagnoseGateFailure(ctx, "fix-jwt-expiry", "--- FAIL: TestExpiredJWT", map[string]string{"auth": "#### Scenario: Reject expired JWT token"})
	if err != nil {
		t.Fatalf("DiagnoseGateFailure failed: %v", err)
	}
	if httpCalls != 2 {
		t.Fatalf("expected GenerateObject[T] + validator/v10 to retry once on validation error (2 calls total), got %d", httpCalls)
	}
	if diag.FailedGate != "backend:test" || diag.Category != "test_assertion" || len(diag.FailingFiles) != 2 {
		t.Fatalf("unexpected diagnosis: %+v", diag)
	}
	md := diag.FormatMarkdown()
	if !strings.Contains(md, "Structured Gate Diagnosis (backend:test — test_assertion)") || !strings.Contains(md, "repos/backend/auth.go") {
		t.Fatalf("unexpected markdown format:\n%s", md)
	}

	// Second call with identical inputs MUST hit SQLite llm_cache (httpCalls remains 2!)
	diagCached, err := client.DiagnoseGateFailure(ctx, "fix-jwt-expiry", "--- FAIL: TestExpiredJWT", map[string]string{"auth": "#### Scenario: Reject expired JWT token"})
	if err != nil {
		t.Fatalf("cached DiagnoseGateFailure failed: %v", err)
	}
	if httpCalls != 2 {
		t.Fatalf("expected SQLite llm_cache hit without extra HTTP call, got %d calls", httpCalls)
	}
	if diagCached.RootCause != diag.RootCause {
		t.Fatalf("cached result mismatch: %+v vs %+v", diagCached, diag)
	}

	// Verify llm.extract events were emitted
	evts, err := st.QueryEvents(ctx, store.EventFilter{Event: "llm.extract", ChangeName: "fix-jwt-expiry"})
	if err != nil || len(evts) != 2 {
		t.Fatalf("expected 2 llm.extract events (1 live + 1 cached), got %d (err=%v)", len(evts), err)
	}
}

func TestDiscriminatedUnionQuarantineTriageWithGoAI(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	st, err := store.OpenAt(filepath.Join(tmp, "llm-union.db"), "llm-union", tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeGoAIJSONResponse(w, map[string]any{
			"action_type":         "answer_clarification",
			"question_from_agent": "Should the webhook endpoint require HMAC-SHA256 or mTLS?",
			"suggested_options": []string{
				"Require HMAC-SHA256 signature in X-Webhook-Signature header",
				"Require mutual TLS (mTLS) client certificate",
			},
		})
	}))
	defer srv.Close()

	client := NewClient(Config{
		Enabled:        true,
		Provider:       "openai",
		Model:          "gpt-4o-mini",
		BaseURL:        srv.URL,
		APIKey:         "test-key",
		TimeoutSeconds: 10,
		MaxRetries:     1,
	}, st)

	rec, err := client.TriageQuarantineUnion(ctx, "webhook-auth", "spec_ambiguity", "PLAN", "needs human input", "Should the webhook endpoint require HMAC-SHA256 or mTLS?")
	if err != nil {
		t.Fatalf("TriageQuarantineUnion failed: %v", err)
	}
	clar, ok := rec.(AnswerClarificationRecommendation)
	if !ok || clar.ActionType != "answer_clarification" || len(clar.SuggestedOptions) != 2 {
		t.Fatalf("expected AnswerClarificationRecommendation from union, got %T: %+v", rec, rec)
	}
}
