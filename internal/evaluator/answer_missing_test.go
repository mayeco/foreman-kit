package evaluator

import (
	"context"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// answerMissingJudge returns a Judge whose policy has one enforce gate that asserts on single_concern.
func answerMissingJudge(t *testing.T, whenUnavailable string) *Judge {
	t.Helper()
	root := t.TempDir()
	policy := `model: jev-1.14.0
when_unavailable: ` + whenUnavailable + `
gates:
  plan_review:
    mode: enforce
    assert:
      - "single_concern >= 0.6"
    questions:
      single_concern:
        type: noul
        instructions: Does the change pursue a single goal?
        criteria:
          "true": Every part of the proposal serves the same goal.
          "false": The change bundles unrelated goals.
`
	if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "evaluator.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	judge, err := NewJudge(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return judge
}

// The asserts are checked against the questions before anything is sent. A 200 response that lacks an
// asserted answer (a gateway's own envelope, a partial answer set) is therefore the service failing, not
// a configuration error: an enforce gate used to return it as a plain error, which the dispatcher
// records as orchestrator_error and pauses the workspace for, whatever when_unavailable says.
func TestAnswerMissingFromA200ResponseIsJevUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.14.0","answers":{}}`))
	}))
	defer srv.Close()
	t.Setenv("TYPESAFE_API_KEY", "ts-test-key")
	settingstest.Give(t, settings.EvaluatorBaseURL, srv.URL)
	state := map[string]any{"change": "add-login", "goal": "Add user login"}

	verdict, err := answerMissingJudge(t, "pass").Ask(context.Background(), "plan_review", state, "add-login")
	if err != nil {
		t.Fatalf("when_unavailable: pass must let a gate Jev did not answer pass, got error %v", err)
	}
	if verdict == nil || verdict.Status != "unavailable" || verdict.Blocking() {
		t.Fatalf("verdict %+v, want unavailable and not blocking", verdict)
	}

	// block and stop both block the change of an enforce gate; stop also fails the preflight.
	for _, mode := range []string{"block", "stop"} {
		judge := answerMissingJudge(t, mode)
		verdict, err = judge.Ask(context.Background(), "plan_review", state, "add-login")
		if err == nil || !strings.HasPrefix(err.Error(), "judge_unavailable:") {
			t.Fatalf("when_unavailable: %s must report judge_unavailable, got verdict %+v, err %v", mode, verdict, err)
		}
		if judge.Policy().StopsWhenUnavailable() != (mode == "stop") {
			t.Fatalf("when_unavailable: %s: the preflight stops: %v", mode, judge.Policy().StopsWhenUnavailable())
		}
	}
}
