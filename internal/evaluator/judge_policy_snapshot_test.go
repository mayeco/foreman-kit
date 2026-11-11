package evaluator

import (
	"context"
	"encoding/json"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// judgeSnapshotPolicy is a one-gate .foreman-kit/evaluator.yaml whose only question and assert use question.
func judgeSnapshotPolicy(question string) string {
	return `model: jev-1.14.0
gates:
  plan_review:
    mode: enforce
    assert:
      - "` + question + ` >= 0.6"
    questions:
      ` + question + `:
        type: noul
        instructions: Does the change pursue a single goal?
        criteria:
          "true": Every part of the proposal serves the same goal.
          "false": The change bundles unrelated goals.
`
}

func judgeSnapshotWritePolicy(t *testing.T, root, content string) {
	t.Helper()
	p := filepath.Join(root, ".foreman-kit", "evaluator.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A Judge lives as long as the daemon's dispatcher and merger. Its mode, asserts and routing rules come
// from the policy it loaded when it was created, so the questions it asks must come from that policy too:
// read again from disk on every call, a question renamed in .foreman-kit/evaluator.yaml while the daemon runs
// no longer matched the asserts in memory, and every enforce gate failed as a configuration error.
func TestJudgeSnapshotQuestionsFollowTheJudgesPolicy(t *testing.T) {
	var mu sync.Mutex
	var asked [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		ids := make([]string, 0, len(req.Questions))
		answers := make(map[string]any, len(req.Questions))
		for id := range req.Questions {
			ids = append(ids, id)
			answers[id] = map[string]any{"type": "noul", "noul": 0.9}
		}
		sort.Strings(ids)
		mu.Lock()
		asked = append(asked, ids)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.14.0", "answers": answers})
	}))
	defer srv.Close()

	t.Setenv("TYPESAFE_API_KEY", "ts-test-key")
	settingstest.Give(t, settings.EvaluatorBaseURL, srv.URL)

	root := t.TempDir()
	judgeSnapshotWritePolicy(t, root, judgeSnapshotPolicy("single_concern"))
	judge, err := NewJudge(root, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The operator renames the question and its assert while the Judge is alive.
	judgeSnapshotWritePolicy(t, root, judgeSnapshotPolicy("one_goal"))

	state := map[string]any{"change": "add-login", "goal": "Add user login"}
	if _, code, err := judge.EvaluateTemplate(context.Background(), "plan_review", state, nil, true); err != nil || code != 0 {
		t.Errorf("dry run after the policy file changed: code %d, err %v", code, err)
	}

	verdict, err := judge.Ask(context.Background(), "plan_review", state, "add-login")
	if err != nil {
		t.Fatalf("Ask after the policy file changed: %v", err)
	}
	if verdict.Status != "passed" {
		t.Fatalf("verdict %+v, want passed", verdict)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || strings.Join(asked[0], ",") != "single_concern" {
		t.Fatalf("the Judge asked %v, want the question of its own policy (single_concern) once", asked)
	}
}
