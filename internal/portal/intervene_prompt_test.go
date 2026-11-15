package portal

import (
	"context"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedIntervenePrompts queues the changes the Intervene prompts are rendered for:
//
//	add-widget   a description, a worktree that specifies the capability widgets
//	only-why     no description, a why
//	bare-change  neither
//	long-goal    a description over the limit of a backlog text
func seedIntervenePrompts(t *testing.T, srv *Server, root string) {
	t.Helper()
	backlog := `[
		{"change_name":"add-widget","description":"Add the widget endpoint","queued_at":"2026-11-02T09:00:00Z"},
		{"change_name":"only-why","why":"Widgets are slow to list","queued_at":"2026-11-02T10:00:00Z"},
		{"change_name":"bare-change","queued_at":"2026-11-02T11:00:00Z"},
		{"change_name":"long-goal","description":"` + strings.Repeat("x", 9000) + `","queued_at":"2026-11-02T12:00:00Z"}
	]`
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "agent_backlog.json"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SyncJSONToSQLite(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(root, ".worktrees", "worker-add-widget", "openspec", "changes", "add-widget", "specs", "widgets")
	if err := os.MkdirAll(spec, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec, "spec.md"), []byte(searchSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"only-why", "bare-change", "long-goal"} {
		if err := os.MkdirAll(filepath.Join(root, ".worktrees", "worker-"+change), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// intervenePrompt returns the prompt the Intervene tab of a change starts from, as a person reads it.
func intervenePrompt(t *testing.T, srv *Server, change, from string) string {
	t.Helper()
	_, body := changePage(t, srv, "/changes/"+change+"/intervene?from="+from)
	return html.UnescapeString(body)
}

// TestTheInterveneTabStartsFromThePlanPromptWithTheGoalOfTheChange: the PLAN prompt "a worker would get
// now" carries the goal the dispatcher gives it: the description of the change, else its why, else its
// name, cut like any text of the backlog.
func TestTheInterveneTabStartsFromThePlanPromptWithTheGoalOfTheChange(t *testing.T) {
	srv, _, root := setupTestPortal(t)
	seedIntervenePrompts(t, srv, root)

	if got := intervenePrompt(t, srv, "add-widget", "plan"); !strings.Contains(got, "Goal: Add the widget endpoint\n") {
		t.Errorf("the PLAN prompt of add-widget has no goal from its description:\n%s", got)
	}
	if got := intervenePrompt(t, srv, "only-why", "plan"); !strings.Contains(got, "Goal: Widgets are slow to list\n") {
		t.Errorf("the PLAN prompt of a change with only a why has no goal from it:\n%s", got)
	}
	if got := intervenePrompt(t, srv, "bare-change", "plan"); !strings.Contains(got, "Goal: bare-change\n") {
		t.Errorf("the PLAN prompt of a change with neither has no goal from its name:\n%s", got)
	}
	got := intervenePrompt(t, srv, "long-goal", "plan")
	goal := ""
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Goal: ") {
			goal = strings.TrimSpace(line)
			break
		}
	}
	if len(goal) > 8*1024+len("Goal: ") || !strings.HasSuffix(goal, "... (truncated)") {
		t.Errorf("the goal of a change with a long description is not cut as the dispatcher cuts it: %d bytes, ends %q", len(goal), goal[max(0, len(goal)-40):])
	}
}

// TestTheInterveneTabStartsFromTheBuildPromptOfTheChange: the BUILD prompt still renders, with the
// branch of the change and without a goal (the BUILD prompt has none).
func TestTheInterveneTabStartsFromTheBuildPromptOfTheChange(t *testing.T) {
	srv, _, root := setupTestPortal(t)
	seedIntervenePrompts(t, srv, root)

	got := intervenePrompt(t, srv, "add-widget", "build")
	if !strings.Contains(got, "the BUILD prompt a worker would get now") || !strings.Contains(got, "feat/spec-add-widget") {
		t.Errorf("the BUILD prompt of add-widget does not render with its branch:\n%s", got)
	}
	if strings.Contains(got, "{{") {
		t.Errorf("the BUILD prompt of add-widget leaves a placeholder unfilled:\n%s", got)
	}
}

// TestTheInterveneTabPromptHasTheMemoryAndTheEvaluatorHintOfTheDispatcher: the memory block names first
// the notes of the capabilities the change specifies in its worktree, and the evaluator hint is there
// whenever the judge is available, as for a worker the dispatcher starts.
func TestTheInterveneTabPromptHasTheMemoryAndTheEvaluatorHintOfTheDispatcher(t *testing.T) {
	srv, _, root := setupTestPortal(t)
	seedIntervenePrompts(t, srv, root)
	// No credentials of the evaluator on this machine count here: the key is the one the test gives.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TYPESAFE_API_KEY", "")
	for _, f := range []string{"spec-widgets.md", "spec-billing.md"} {
		path := filepath.Join(root, ".worktrees", "worker-add-widget", ".agent-memory", "modules", f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := intervenePrompt(t, srv, "add-widget", "plan")
	if want := "for the capabilities this change modifies: .agent-memory/modules/spec-widgets.md"; !strings.Contains(got, want) {
		t.Errorf("the memory block of the PLAN prompt does not put the notes of widgets first (want %q):\n%s", want, got)
	}
	if strings.Contains(got, "jev noul") {
		t.Errorf("the PLAN prompt gives the evaluator hint with no API key:\n%s", got)
	}

	t.Setenv("TYPESAFE_API_KEY", "test-key")
	for _, role := range []string{"plan", "build"} {
		if got := intervenePrompt(t, srv, "add-widget", role); !strings.Contains(got, "jev noul") {
			t.Errorf("the %s prompt has no evaluator hint though the judge is available:\n%s", role, got)
		}
	}
}
