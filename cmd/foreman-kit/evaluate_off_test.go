package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/settings"
)

// evaluateOffApp is a scratch application whose evaluator.yaml points at a capture server, with
// the gate task_priority in the given mode and the evaluator enabled or not.
type evaluateOffApp struct {
	t        *testing.T
	root     string
	requests *atomic.Int64
}

func newEvaluateOffApp(t *testing.T, enabled bool, taskPriorityMode string) *evaluateOffApp {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "sample-app")
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(tmp, "state"))
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_APP_ROOT", root)
	t.Setenv("TYPESAFE_API_KEY", "ts-dummy-key")
	t.Setenv(settings.RunOverridesEnv, "")

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "capture server: no answer", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	app := &evaluateOffApp{t: t, root: root, requests: &requests}
	if code, out, errOut := app.run("init", root, "--agents", "claude"); code != 0 {
		t.Fatalf("init failed (%d): %s %s", code, out, errOut)
	}
	path := filepath.Join(root, ".foreman-kit", "evaluator.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for from, to := range map[string]string{
		"enabled: true #":  "enabled: " + map[bool]string{true: "true", false: "false"}[enabled] + " #",
		"base_url: \"\" #": "base_url: \"" + srv.URL + "\" #",
		"retries: 2 #":     "retries: 0 #",
	} {
		if !strings.Contains(text, from) {
			t.Fatalf("the seed of evaluator.yaml has no %q", from)
		}
		text = strings.Replace(text, from, to, 1)
	}
	head, tail, ok := strings.Cut(text, "\n  task_priority:\n    mode: advisory\n")
	if !ok {
		t.Fatal("the seed of evaluator.yaml has no gate task_priority in advisory")
	}
	text = head + "\n  task_priority:\n    mode: " + taskPriorityMode + "\n" + tail
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "draft.json"), []byte(`{"change_name":"fix-x","description":"x is broken: run y, see z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return app
}

func (a *evaluateOffApp) run(args ...string) (int, string, string) {
	a.t.Helper()
	var out, errBuf bytes.Buffer
	code := Run(context.Background(), args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func (a *evaluateOffApp) evalTaskPriority(extra ...string) (int, string, string) {
	a.t.Helper()
	args := append(extra, "evaluate", "eval", "-f", "task_priority", "--state-file", filepath.Join(a.root, "draft.json"))
	return a.run(args...)
}

func (a *evaluateOffApp) noul() (int, string, string) {
	a.t.Helper()
	return a.run("evaluate", "noul", "Is it ready?", "--state-file", filepath.Join(a.root, "draft.json"))
}

func TestEvaluateEvalDoesNotAskADisabledEvaluator(t *testing.T) {
	app := newEvaluateOffApp(t, false, "advisory")
	code, out, errOut := app.evalTaskPriority()
	if code != evaluator.ExitOff {
		t.Fatalf("exit %d, want %d: %s %s", code, evaluator.ExitOff, out, errOut)
	}
	if n := app.requests.Load(); n != 0 {
		t.Errorf("a disabled evaluator was sent %d requests", n)
	}
	if !strings.Contains(errOut, "the evaluator is off") || !strings.Contains(errOut, "enabled") {
		t.Errorf("stderr does not say the evaluator is off: %q", errOut)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("stdout is not empty: %q", out)
	}
}

func TestEvaluateEvalDoesNotAskAGateWhoseModeIsOff(t *testing.T) {
	app := newEvaluateOffApp(t, true, "off")
	code, out, errOut := app.evalTaskPriority()
	if code != evaluator.ExitOff {
		t.Fatalf("exit %d, want %d: %s %s", code, evaluator.ExitOff, out, errOut)
	}
	if n := app.requests.Load(); n != 0 {
		t.Errorf("a gate in mode off was sent %d requests", n)
	}
	if !strings.Contains(errOut, "task_priority") || !strings.Contains(errOut, "is off") {
		t.Errorf("stderr does not name the gate and say it is off: %q", errOut)
	}
}

func TestEvaluateEvalDoesNotAskAnEvaluatorTurnedOffForTheRun(t *testing.T) {
	app := newEvaluateOffApp(t, true, "advisory")
	code, out, errOut := app.evalTaskPriority("--set", "evaluator.enabled=false")
	if code != evaluator.ExitOff {
		t.Fatalf("exit %d, want %d: %s %s", code, evaluator.ExitOff, out, errOut)
	}
	if n := app.requests.Load(); n != 0 {
		t.Errorf("an evaluator turned off with --set was sent %d requests", n)
	}
}

func TestEvaluateEvalAsksAnAdvisoryOrEnforceGate(t *testing.T) {
	for _, mode := range []string{"advisory", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			app := newEvaluateOffApp(t, true, mode)
			code, out, errOut := app.evalTaskPriority()
			if n := app.requests.Load(); n == 0 {
				t.Fatalf("a gate in %s was not asked (exit %d): %s %s", mode, code, out, errOut)
			}
			if code == evaluator.ExitOff {
				t.Errorf("a gate in %s exited %d, the code of an evaluator that is off: %s", mode, code, errOut)
			}
		})
	}
}

func TestEvaluateEvalDryRunStillValidatesWhileTheEvaluatorIsOff(t *testing.T) {
	app := newEvaluateOffApp(t, false, "advisory")
	code, out, errOut := app.run("evaluate", "eval", "-f", "task_priority", "--state-file", filepath.Join(app.root, "draft.json"), "--dry-run")
	if code != 0 {
		t.Fatalf("dry run exit %d, want 0: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, `"passed": true`) {
		t.Errorf("dry run output: %q", out)
	}
	if n := app.requests.Load(); n != 0 {
		t.Errorf("a dry run sent %d requests", n)
	}
}

func TestEvaluateNoulDoesNotAskADisabledEvaluator(t *testing.T) {
	app := newEvaluateOffApp(t, false, "advisory")
	code, out, errOut := app.noul()
	if code != evaluator.ExitOff {
		t.Fatalf("exit %d, want %d: %s %s", code, evaluator.ExitOff, out, errOut)
	}
	if n := app.requests.Load(); n != 0 {
		t.Errorf("a disabled evaluator was sent %d requests", n)
	}
	if !strings.Contains(errOut, "the evaluator is off") {
		t.Errorf("stderr does not say the evaluator is off: %q", errOut)
	}
}

func TestEvaluateNoulAsksAnEnabledEvaluator(t *testing.T) {
	app := newEvaluateOffApp(t, true, "off")
	code, out, errOut := app.noul()
	if n := app.requests.Load(); n == 0 {
		t.Fatalf("an enabled evaluator was not asked (exit %d): %s %s", code, out, errOut)
	}
	if code == evaluator.ExitOff {
		t.Errorf("exit %d is the code of an evaluator that is off: %s", code, errOut)
	}
}
