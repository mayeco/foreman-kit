package evaluator

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

func TestOfflineEvaluatorContractWithoutBinary(t *testing.T) {
	// Ensure external `jev` CLI is not needed on PATH
	t.Setenv("PATH", "/usr/bin:/bin")

	var buf bytes.Buffer
	failures := RunContract(context.Background(), "", false, &buf)
	if failures != 0 {
		t.Fatalf("expected 0 offline contract failures without jev binary, got %d:\n%s", failures, buf.String())
	}
	if !strings.Contains(buf.String(), "evaluator contract: PASS") {
		t.Fatalf("expected PASS output, got:\n%s", buf.String())
	}
}

func TestJudgeAskAndSQLiteCacheWithMockServer(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"model": "jev-1.14.0",
			"answers": map[string]any{
				"problem_is_concrete": map[string]any{
					"type": "noul",
					"noul": 0.91,
				},
				"scenarios_are_testable": map[string]any{
					"type": "noul",
					"noul": 0.88,
				},
				"tasks_cover_scenarios": map[string]any{
					"type": "noul",
					"noul": 0.95,
				},
				"consistent_with_canonical_specs": map[string]any{
					"type": "noul",
					"noul": 0.92,
				},
				"single_concern": map[string]any{
					"type": "noul",
					"noul": 0.89,
				},
			},
			"usage": map[string]any{
				"input_tokens":  120,
				"output_tokens": 25,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	t.Setenv("TYPESAFE_API_KEY", "ts-test-key")
	settingstest.Give(t, settings.EvaluatorBaseURL, srv.URL)

	tmp := t.TempDir()
	st, err := store.OpenAt(filepath.Join(tmp, "state.db"), "test-1234567890ab", tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	judge, err := NewJudge("", st)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	state := map[string]any{"change": "add-login", "goal": "Add user login"}
	v1, err := judge.Ask(ctx, "plan_review", state, "add-login")
	if err != nil {
		t.Fatalf("first Ask failed: %v", err)
	}
	if v1.Status != "passed" || v1.Cached {
		t.Fatalf("unexpected first verdict: %+v", v1)
	}
	if calls != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", calls)
	}

	// Second call with identical state must come from SQLite evaluation_cache without hitting HTTP server
	v2, err := judge.Ask(ctx, "plan_review", state, "add-login")
	if err != nil {
		t.Fatalf("second Ask failed: %v", err)
	}
	if v2.Status != "passed" || !v2.Cached {
		t.Fatalf("expected cached passed verdict on second Ask, got: %+v", v2)
	}
	if calls != 1 {
		t.Fatalf("expected HTTP call count to remain 1, got %d", calls)
	}

	rep, err := BuildReport(ctx, "", st, time.Now().Add(-time.Hour), "plan_review", func(string) string { return "integrated" })
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Gates) != 1 || rep.Gates[0].Answers != 2 || rep.Gates[0].Cached != 1 {
		t.Fatalf("unexpected evaluation report: %+v", rep)
	}
}

func TestGlobalByteBudgetSizeQuestionRetryAndAdvisorySafety(t *testing.T) {
	// 1. Verify ClipState enforces DefaultMaxStateTotalBytes across multiple 24KB strings
	hugeState := map[string]any{
		"cap1": strings.Repeat("A", 24000),
		"cap2": strings.Repeat("B", 24000),
		"cap3": strings.Repeat("C", 24000),
		"cap4": strings.Repeat("D", 24000),
		"cap5": strings.Repeat("E", 24000),
	}
	clipped := ClipState(hugeState, 24000)
	b, err := json.Marshal(clipped)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > DefaultMaxStateTotalBytes {
		t.Fatalf("expected clipped state JSON <= %d bytes, got %d bytes", DefaultMaxStateTotalBytes, len(b))
	}

	// 2. Verify size-question auto-retry and advisory safety (never returns error on advisory gate)
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":{"rule":"size-question","message":"state plus the longest question is too large: 46620 tokens"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-1.14.0",
			"answers": map[string]any{
				"problem_is_concrete":             map[string]any{"type": "noul", "noul": 0.91},
				"scenarios_are_testable":          map[string]any{"type": "noul", "noul": 0.88},
				"tasks_cover_scenarios":           map[string]any{"type": "noul", "noul": 0.95},
				"consistent_with_canonical_specs": map[string]any{"type": "noul", "noul": 0.92},
				"single_concern":                  map[string]any{"type": "noul", "noul": 0.89},
			},
		})
	}))
	defer srv.Close()

	t.Setenv("TYPESAFE_API_KEY", "ts-test-key")
	settingstest.Give(t, settings.EvaluatorBaseURL, srv.URL)

	tmp := t.TempDir()
	st, err := store.OpenAt(filepath.Join(tmp, "state.db"), "test-budget", tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	judge, err := NewJudge("", st)
	if err != nil {
		t.Fatal(err)
	}
	judge.Policy().Retries = 0 // disable transport retries so Ask's size-question retry is tested directly

	v, err := judge.Ask(context.Background(), "plan_review", hugeState, "budget-change")
	if err != nil {
		t.Fatalf("expected Ask to recover from size-question without error, got %v", err)
	}
	if v.Status != "passed" || attempts != 2 {
		t.Fatalf("expected passed verdict on retry attempt 2, got status=%s attempts=%d", v.Status, attempts)
	}
}
