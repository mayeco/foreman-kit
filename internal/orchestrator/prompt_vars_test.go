package orchestrator

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/evaluator"
)

func TestPlanGoalFallsBackFromTheDescriptionToTheWhyToTheChangeName(t *testing.T) {
	long := strings.Repeat("x", maxBacklogTextBytes+100)
	for _, c := range []struct{ name, description, why, want string }{
		{"description", "  Add the widget endpoint \n", "Because", "Add the widget endpoint"},
		{"why", "  \n", " Widgets are slow ", "Widgets are slow"},
		{"change name", "", "\t", "add-widget"},
		{"cut like any backlog text", long, "", sanitizeBacklogText(long)},
	} {
		if got := PlanGoal("add-widget", c.description, c.why); got != c.want {
			t.Errorf("%s: PlanGoal = %.60q, want %.60q", c.name, got, c.want)
		}
	}
	if got := PlanGoal("add-widget", long, ""); len(got) > maxBacklogTextBytes || !strings.HasSuffix(got, "... (truncated)") {
		t.Errorf("a description over %d bytes is not cut: %d bytes", maxBacklogTextBytes, len(got))
	}
}

func TestChangeCapabilitiesAreTheSpecsOfTheChangeInItsWorktree(t *testing.T) {
	wt := t.TempDir()
	for _, capability := range []string{"search", "api/v1"} {
		dir := filepath.Join(wt, "openspec", "changes", "add-widget", "specs", filepath.FromSlash(capability))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "spec.md"), []byte("## ADDED Requirements\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"api/v1", "search"}
	if got := ChangeCapabilities(wt, "add-widget"); !reflect.DeepEqual(got, want) {
		t.Errorf("ChangeCapabilities = %v, want %v", got, want)
	}
	d := &Dispatcher{}
	if got := d.capabilities(wt, "add-widget"); !reflect.DeepEqual(got, want) {
		t.Errorf("the dispatcher's capabilities = %v, want the same %v", got, want)
	}
	if got := ChangeCapabilities(wt, "no-such-change"); len(got) != 0 {
		t.Errorf("a change with no specs has capabilities %v", got)
	}
}

func TestEvaluatorHintIsGivenOnlyWhenTheJudgeIsAvailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("FOREMANKIT_SET", "")
	root := t.TempDir()
	judge, err := evaluator.NewJudge(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := EvaluatorHint(nil); got != "" {
		t.Errorf("a nil judge gives the hint %q", got)
	}
	if got := EvaluatorHint(judge); got != "" {
		t.Errorf("a judge with no API key gives the hint %q", got)
	}
	t.Setenv("TYPESAFE_API_KEY", "test-key")
	got := EvaluatorHint(judge)
	if !strings.Contains(got, "jev noul") || !strings.Contains(got, "--model "+judge.Policy().EffectiveModel()) {
		t.Errorf("an available judge gives the hint %q", got)
	}
	if d := (&Dispatcher{judge: judge}); d.evaluatorHint() != got {
		t.Errorf("the dispatcher's evaluator hint differs from EvaluatorHint")
	}
}

// TestThePlanPromptOfTheDispatcherIsBuiltFromWorkerPromptVars: what the dispatcher gives a worker and
// what the portal shows as the prompt a worker would get now come from one helper, so neither can drift:
// the PLAN prompt is the template rendered with the helper's variables plus the feedback of the run.
func TestThePlanPromptOfTheDispatcherIsBuiltFromWorkerPromptVars(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TYPESAFE_API_KEY", "test-key")
	t.Setenv("FOREMANKIT_SET", "")
	t.Setenv("FOREMANKIT_HOME", t.TempDir())
	root := t.TempDir()
	wt := filepath.Join(root, "worktree")
	spec := filepath.Join(wt, "openspec", "changes", "add-widget", "specs", "widgets")
	if err := os.MkdirAll(spec, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec, "spec.md"), []byte("## ADDED Requirements\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(wt, ".agent-memory", "modules", "spec-widgets.md")
	if err := os.MkdirAll(filepath.Dir(note), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(note, []byte("# widgets\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	judge, err := evaluator.NewJudge(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{root: root, mainRoot: root, judge: judge}
	item := BacklogItem{ChangeName: "add-widget", Description: "Add the widget endpoint"}

	got, _ := d.buildPlanPrompt(item, wt, "", nil)
	vars := WorkerPromptVars(root, wt, "plan", item, judge)
	vars["feedback"] = ""
	want, _ := RenderPrompt(root, "plan", vars)
	if got != want {
		t.Errorf("the dispatcher's PLAN prompt differs from the one WorkerPromptVars renders:\n%s\n---\n%s", got, want)
	}
	for _, fact := range []string{"Goal: Add the widget endpoint", "jev noul", "read these before you start, for the capabilities this change modifies: .agent-memory/modules/spec-widgets.md"} {
		if !strings.Contains(got, fact) {
			t.Errorf("the PLAN prompt lacks %q:\n%s", fact, got)
		}
	}

	build, _ := d.buildApplyPrompt(item, wt, "feat/spec-add-widget", "", "", nil)
	vars = WorkerPromptVars(root, wt, "build", item, judge)
	vars["branch"], vars["feedback"], vars["rebuild"] = "feat/spec-add-widget", "", ""
	wantBuild, _ := RenderPrompt(root, "build", vars)
	if build != wantBuild {
		t.Errorf("the dispatcher's BUILD prompt differs from the one WorkerPromptVars renders")
	}
	if _, ok := vars["goal"]; ok {
		t.Errorf("the BUILD prompt has a goal variable")
	}
}
