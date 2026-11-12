package cycle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

func openTestStore(t *testing.T, root string) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	wsID, canon := store.WorkspaceID(root)
	st, err := store.OpenAt(dbPath, wsID, canon)
	if err != nil {
		t.Fatalf("OpenAt failed: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-b", "master"},
		{"config", "user.name", "Test User"},
		{"config", "user.email", "test@example.com"},
	} {
		if _, err := gitx.RunGit(ctx, dir, args...); err != nil {
			t.Fatalf("git %v failed: %v", args, err)
		}
	}
	baseBranchIs(t, dir, "master")
}

// baseBranchIs makes branch the base branch of an application installed in dir before its
// repository existed: foreman-kit init could not read the branch then, and the seed names another.
func baseBranchIs(t *testing.T, dir, branch string) {
	t.Helper()
	if !fileExists(filepath.Join(dir, filepath.FromSlash(settings.BaseBranch.FilePath()))) {
		return
	}
	if err := config.Set(dir, string(settings.BaseBranch), branch, false, io.Discard); err != nil {
		t.Fatalf("set the base branch: %v", err)
	}
}

func commitAll(t *testing.T, dir, msg string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := gitx.RunGit(ctx, dir, "add", "-A"); err != nil {
		t.Fatalf("git add failed: %v", err)
	}
	if _, err := gitx.RunGit(ctx, dir, "commit", "-m", msg); err != nil {
		t.Fatalf("git commit failed: %v", err)
	}
	out, err := gitx.RunGit(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD failed: %v", err)
	}
	return strings.TrimSpace(out)
}

func TestInitUpgradeVerify(t *testing.T) {
	ctx := context.Background()
	appDir := t.TempDir()

	var out bytes.Buffer
	err := Init(ctx, appDir, KitOptions{
		Agents:    "claude,hermes",
		Roles:     map[string]string{"plan": "claude", "build": "hermes", "cycle": "claude,hermes"},
		Selection: "least_busy",
		Version:   "4.0.0-go",
	}, &out)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if !strings.Contains(out.String(), "kit: installed v4.0.0-go") {
		t.Fatalf("unexpected Init output: %s", out.String())
	}

	// Verify log/.keep was NOT created
	if fileExists(filepath.Join(appDir, "log", ".keep")) {
		t.Fatalf("log/.keep should not be created")
	}

	// Verify .foreman-kit/kit.lock and skills exist
	if !fileExists(filepath.Join(appDir, KitLockFile)) {
		t.Fatalf("missing %s", KitLockFile)
	}
	if !fileExists(filepath.Join(appDir, ".agents/skills/foreman-kit-cycle/SKILL.md")) {
		t.Fatalf("missing shared skill in .agents/skills")
	}
	if !fileExists(filepath.Join(appDir, ".claude/skills/foreman-kit-cycle/SKILL.md")) {
		t.Fatalf("missing claude skill in .claude/skills")
	}
	if !fileExists(filepath.Join(appDir, ".hermes/skills/foreman-kit-cycle/SKILL.md")) {
		t.Fatalf("missing hermes skill in .hermes/skills")
	}

	// Verify passes cleanly
	out.Reset()
	ok, err := Verify(appDir, &out)
	if err != nil || !ok {
		t.Fatalf("Verify failed: ok=%v err=%v out=%s", ok, err, out.String())
	}
	if !strings.Contains(out.String(), "kit: OK") {
		t.Fatalf("expected kit: OK, got: %s", out.String())
	}

	// Introduce drift into a skill file
	skillPath := filepath.Join(appDir, ".claude/skills/foreman-kit-cycle/SKILL.md")
	if err := os.WriteFile(skillPath, []byte("modified in place\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	ok, err = Verify(appDir, &out)
	if err != nil || ok {
		t.Fatalf("expected Verify to detect drift, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(out.String(), "kit: DRIFT") {
		t.Fatalf("expected DRIFT message, got: %s", out.String())
	}

	// Upgrade without --force should refuse
	err = Upgrade(ctx, appDir, KitOptions{Version: "4.1.0-go"}, &out)
	if err == nil {
		t.Fatalf("expected Upgrade without --force to fail on drifted skill")
	}

	// Upgrade with --force restores the skill and updates version
	out.Reset()
	err = Upgrade(ctx, appDir, KitOptions{
		Agents:  "claude",
		Force:   true,
		Version: "4.1.0-go",
	}, &out)
	if err != nil {
		t.Fatalf("Upgrade with --force failed: %v", err)
	}

	// Verify passes again and .hermes/skills was removed
	out.Reset()
	ok, err = Verify(appDir, &out)
	if err != nil || !ok {
		t.Fatalf("Verify after Upgrade failed: ok=%v err=%v out=%s", ok, err, out.String())
	}
	if fileExists(filepath.Join(appDir, ".hermes/skills/foreman-kit-cycle/SKILL.md")) {
		t.Fatalf("expected deselected agent skill to be removed on upgrade")
	}
}

func TestSetupDetectAndRepo(t *testing.T) {
	ctx := context.Background()
	appDir := t.TempDir()
	if err := Init(ctx, appDir, KitOptions{Agents: "claude"}, nil); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Create a Go web child repo with .go-version
	goRepo := filepath.Join(appDir, "repos", "backend")
	if err := os.MkdirAll(filepath.Join(goRepo, "cmd", "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(goRepo, "go.mod"), []byte("module example.com/backend\n\ngo 1.27\nrequire github.com/go-chi/chi v1.5.0\n"), 0o644)
	_ = os.WriteFile(filepath.Join(goRepo, ".go-version"), []byte("1.27.3\n"), 0o644)
	_ = os.WriteFile(filepath.Join(goRepo, "cmd", "server", "main.go"), []byte(`package main
import "net/http"
func main() {
	http.HandleFunc("/healthz", nil)
	http.ListenAndServe(":8090", nil)
}
`), 0o644)

	var out bytes.Buffer
	if err := SetupRepo(appDir, "backend", false, &out); err != nil {
		t.Fatalf("SetupRepo failed: %v", err)
	}
	cfgBytes, err := os.ReadFile(filepath.Join(goRepo, "repo.config.yaml"))
	if err != nil {
		t.Fatalf("missing repo.config.yaml: %v", err)
	}
	cfgText := string(cfgBytes)
	if !strings.Contains(cfgText, `stack: "go-web-chi"`) || !strings.Contains(cfgText, `port: "8090"`) || !strings.Contains(cfgText, `http://localhost:8090/healthz`) {
		t.Fatalf("unexpected rendered repo.config.yaml:\n%s", cfgText)
	}
	if !fileExists(filepath.Join(goRepo, "script", "env.sh")) {
		t.Fatalf("expected script/env.sh to be created for pinned toolchain")
	}

	st := openTestStore(t, appDir)
	rep, err := SetupStatus(ctx, appDir, st)
	if err != nil {
		t.Fatalf("SetupStatus failed: %v", err)
	}
	statusTxt := FormatSetupStatusText(rep)
	if !strings.Contains(statusTxt, "== Setup of") || !strings.Contains(statusTxt, "repos/backend:") {
		t.Fatalf("unexpected SetupStatus text:\n%s", statusTxt)
	}
}

func TestProposalsReviewPromptAndJev(t *testing.T) {
	ctx := context.Background()
	appDir := t.TempDir()
	if err := Init(ctx, appDir, KitOptions{Agents: "claude"}, nil); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	st := openTestStore(t, appDir)

	// Let nobody approve prompt edits (prompt_edits_approved_by) in .foreman-kit/cycle.yaml
	cyclePath := filepath.Join(appDir, ".foreman-kit", "cycle.yaml")
	cycleBytes, _ := os.ReadFile(cyclePath)
	updatedCycle := strings.Replace(string(cycleBytes), "prompt_edits_approved_by: human", "prompt_edits_approved_by: nobody", 1)
	if updatedCycle == string(cycleBytes) {
		t.Fatal("the seed of .foreman-kit/cycle.yaml does not write prompt_edits_approved_by: human")
	}
	_ = os.WriteFile(cyclePath, []byte(updatedCycle), 0o644)

	// Start mock Jev server returning passing answers for proposal_review
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"model": "jev-1.14.0",
			"answers": map[string]any{
				"advances_the_product": map[string]any{"type": "noul", "noul": 0.95},
				"well_scoped":          map[string]any{"type": "noul", "noul": 0.92},
				"not_duplicate":        map[string]any{"type": "noul", "noul": 0.90},
				"safe_to_automate":     map[string]any{"type": "noul", "noul": 0.91},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	settingstest.Give(t, settings.EvaluatorBaseURL, srv.URL)
	t.Setenv("TYPESAFE_API_KEY", "test-key")

	targetLine := "Goal: {{goal}}"

	proposals := []map[string]any{
		{
			"change_name": "add-audit-trail",
			"status":      "proposed",
			"source":      "roadmap",
			"priority":    "high",
			"description": "Add audit trail table and API endpoint",
		},
		{
			"change_name": "improve-plan-prompt",
			"kind":        "prompt",
			"prompt":      "plan",
			"status":      "proposed",
			"source":      "cycle",
			"evidence":    "Clarify goal section",
			"edits": []any{
				map[string]any{
					"old": targetLine,
					"new": targetLine + "\nKeep the plan concise.",
				},
			},
		},
		{
			"change_name": "bad-prompt-edit",
			"kind":        "prompt",
			"prompt":      "plan",
			"status":      "proposed",
			"source":      "cycle",
			"evidence":    "Adds unknown placeholder",
			"edits": []any{
				map[string]any{
					"old": "Keep the plan concise.",
					"new": "Keep the plan concise {{unknown_placeholder}}.",
				},
			},
		},
	}
	if err := WriteProposals(appDir, proposals); err != nil {
		t.Fatal(err)
	}

	judge, err := evaluator.NewJudge(appDir, st)
	if err != nil {
		t.Fatalf("NewJudge failed: %v", err)
	}

	var reviewOut bytes.Buffer
	if err := ReviewProposals(ctx, appDir, st, judge, &reviewOut); err != nil {
		t.Fatalf("ReviewProposals failed: %v", err)
	}

	// Verify add-audit-trail was queued into .foreman-kit/agent_backlog.json
	backlog, err := ReadJSONList(filepath.Join(appDir, BacklogFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(backlog) != 1 || asString(backlog[0]["change_name"]) != "add-audit-trail" {
		t.Fatalf("expected add-audit-trail in backlog, got: %+v", backlog)
	}

	// Verify proposals statuses
	afterProps, err := ReadProposals(appDir)
	if err != nil {
		t.Fatal(err)
	}
	statusBy := make(map[string]string)
	for _, p := range afterProps {
		statusBy[asString(p["change_name"])] = asString(p["status"])
	}
	if statusBy["add-audit-trail"] != "queued" {
		t.Fatalf("expected add-audit-trail to be queued, got %s", statusBy["add-audit-trail"])
	}
	if statusBy["improve-plan-prompt"] != "integrated" {
		t.Fatalf("expected improve-plan-prompt to be integrated, got %s", statusBy["improve-plan-prompt"])
	}
	if statusBy["bad-prompt-edit"] != "rejected" {
		t.Fatalf("expected bad-prompt-edit to be rejected, got %s", statusBy["bad-prompt-edit"])
	}

	// Test CLI list, show, approve, reject
	_ = WriteProposals(appDir, []map[string]any{
		{
			"change_name": "manual-feature",
			"status":      "proposed",
			"source":      "exploration",
			"description": "Manual approval test",
		},
	})
	var cliOut, cliErr bytes.Buffer
	if rc := RunProposalsCLI(ctx, appDir, st, judge, []string{"list"}, &cliOut, &cliErr); rc != 0 {
		t.Fatalf("list failed: %s", cliErr.String())
	}
	if !strings.Contains(cliOut.String(), "manual-feature") {
		t.Fatalf("expected manual-feature in list output: %s", cliOut.String())
	}
	cliOut.Reset()
	if rc := RunProposalsCLI(ctx, appDir, st, judge, []string{"show", "manual-feature"}, &cliOut, &cliErr); rc != 0 {
		t.Fatalf("show failed: %s", cliErr.String())
	}
	if rc := RunProposalsCLI(ctx, appDir, st, judge, []string{"approve", "manual-feature"}, &cliOut, &cliErr); rc != 0 {
		t.Fatalf("approve failed: %s", cliErr.String())
	}
	if rc := RunProposalsCLI(ctx, appDir, st, judge, []string{"reject", "manual-feature"}, &cliOut, &cliErr); rc != 0 {
		t.Fatalf("reject failed: %s", cliErr.String())
	}
}

func TestRebaseAndCarry3Way(t *testing.T) {
	ctx := context.Background()
	originDir := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(originDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.RunGit(ctx, originDir, "init", "--bare", "-b", "master"); err != nil {
		t.Fatalf("init bare failed: %v", err)
	}

	workA := filepath.Join(t.TempDir(), "workA")
	if err := os.MkdirAll(workA, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, workA)
	_, _ = gitx.RunGit(ctx, workA, "remote", "add", "origin", originDir)

	_ = os.WriteFile(memPath(workA), []byte("# Memory\n- base lesson\n"), 0o644)
	_ = WriteJSONList(filepath.Join(workA, BacklogFile), []map[string]any{
		{"change_name": "base-item", "priority": "normal", "description": "Base"},
	})
	_ = WriteJSONList(filepath.Join(workA, ProposalsFile), []map[string]any{
		{"change_name": "shared-prop", "status": "proposed", "source": "cycle"},
	})
	commitAll(t, workA, "initial")
	if _, err := gitx.RunGit(ctx, workA, "push", "-u", "origin", "master"); err != nil {
		t.Fatalf("push initial failed: %v", err)
	}

	// Clone workB to simulate concurrent merger/other session push to origin/master
	workB := filepath.Join(t.TempDir(), "workB")
	if _, err := gitx.RunGit(ctx, t.TempDir(), "clone", originDir, workB); err != nil {
		t.Fatalf("clone workB failed: %v", err)
	}
	_, _ = gitx.RunGit(ctx, workB, "config", "user.name", "Test User")
	_, _ = gitx.RunGit(ctx, workB, "config", "user.email", "test@example.com")

	_ = os.WriteFile(memPath(workB), []byte("# Memory\n- base lesson\n- lesson from B\n"), 0o644)
	_ = WriteJSONList(filepath.Join(workB, BacklogFile), []map[string]any{
		{"change_name": "base-item", "priority": "normal", "description": "Base"},
		{"change_name": "from-b", "priority": "high", "description": "From B"},
	})
	_ = WriteJSONList(filepath.Join(workB, ProposalsFile), []map[string]any{
		{"change_name": "shared-prop", "status": "approved", "source": "cycle"},
	})
	commitAll(t, workB, "chore(memory): session B")
	if _, err := gitx.RunGit(ctx, workB, "push", "origin", "master"); err != nil {
		t.Fatalf("push workB failed: %v", err)
	}

	// Meanwhile in workA, create a local record commit
	_ = os.WriteFile(memPath(workA), []byte("# Memory\n- base lesson\n- lesson from A\n"), 0o644)
	_ = WriteJSONList(filepath.Join(workA, BacklogFile), []map[string]any{
		{"change_name": "base-item", "priority": "normal", "description": "Base"},
		{"change_name": "from-a", "priority": "low", "description": "From A"},
	})
	_ = WriteJSONList(filepath.Join(workA, ProposalsFile), []map[string]any{
		{"change_name": "shared-prop", "status": "queued", "source": "cycle"},
		{"change_name": "prop-a", "status": "proposed", "source": "cycle"},
	})
	commitAll(t, workA, "chore(memory): session A")

	var rebOut bytes.Buffer
	if err := RebaseProposals(ctx, workA, "master", &rebOut); err != nil {
		t.Fatalf("RebaseProposals failed: %v", err)
	}

	// Verify both lessons are in the memory
	memBytes, _ := os.ReadFile(memPath(workA))
	memStr := string(memBytes)
	if !strings.Contains(memStr, "lesson from A") || !strings.Contains(memStr, "lesson from B") {
		t.Fatalf("expected both lessons in the memory, got:\n%s", memStr)
	}

	// Verify backlog has base-item, from-b, and from-a
	bl, _ := ReadJSONList(filepath.Join(workA, BacklogFile))
	if len(bl) != 3 {
		t.Fatalf("expected 3 backlog items after 3-way merge, got %d: %+v", len(bl), bl)
	}

	// Verify shared-prop kept the further-along status ("queued")
	props, _ := ReadProposals(workA)
	foundQueued := false
	for _, p := range props {
		if asString(p["change_name"]) == "shared-prop" && asString(p["status"]) == "queued" {
			foundQueued = true
		}
	}
	if !foundQueued {
		t.Fatalf("expected shared-prop status=queued after 3-way merge, got: %+v", props)
	}

	// Test CarryProposals from workB uncommitted edits into workA
	_ = WriteJSONList(filepath.Join(workB, BacklogFile), []map[string]any{
		{"change_name": "base-item", "priority": "normal", "description": "Base"},
		{"change_name": "from-b", "priority": "high", "description": "From B"},
		{"change_name": "carried-item", "priority": "critical", "description": "Carried"},
	})
	var carryOut bytes.Buffer
	if err := CarryProposals(ctx, workA, workB, &carryOut); err != nil {
		t.Fatalf("CarryProposals failed: %v", err)
	}
	blAfterCarry, _ := ReadJSONList(filepath.Join(workA, BacklogFile))
	hasCarried := false
	for _, it := range blAfterCarry {
		if asString(it["change_name"]) == "carried-item" {
			hasCarried = true
		}
	}
	if !hasCarried {
		t.Fatalf("expected carried-item in workA backlog after CarryProposals")
	}
}

func TestScopeGuardAndRecordSessionUnpublishedFallback(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initGitRepo(t, dir)
	st := openTestStore(t, dir)

	_ = os.WriteFile(memPath(dir), []byte("# Memory\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("# App\n"), 0o644)
	commitAll(t, dir, "initial")

	// Modify allowed scope file -> ScopeGuard passes
	_ = os.WriteFile(memPath(dir), []byte("# Memory\n- new lesson\n"), 0o644)
	var out bytes.Buffer
	if err := ScopeGuard(ctx, dir, "test-cycle", st, &out); err != nil {
		t.Fatalf("expected ScopeGuard to pass for an edit of the memory: %v", err)
	}

	// RecordSession with no remote fails push and saves commit under refs/openspec/unpublished/...
	out.Reset()
	err := RecordSession(ctx, dir, "master", "chore(memory): test session", st, &out)
	if err == nil {
		t.Fatalf("expected RecordSession without remote to return push error")
	}
	refsOut, err := gitx.RunGit(ctx, dir, "for-each-ref", "refs/openspec/unpublished")
	if err != nil || strings.TrimSpace(refsOut) == "" {
		t.Fatalf("expected unpublished ref to be saved, got: %q (err=%v, out=%s)", refsOut, err, out.String())
	}

	// Modify outside-scope file -> ScopeGuard fails and pauses workspace in store
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("# App modified\n"), 0o644)
	out.Reset()
	if err := ScopeGuard(ctx, dir, "test-cycle", st, &out); err == nil {
		t.Fatalf("expected ScopeGuard to fail when README.md is modified")
	}
	ws, err := st.GetWorkspaceState(ctx)
	if err != nil || !ws.Paused || !strings.Contains(ws.PauseReason, "scope violation in test-cycle") {
		t.Fatalf("expected workspace to be paused in store, got: %+v", ws)
	}
}

func TestVisualQAAndProgressAndRunner(t *testing.T) {
	ctx := context.Background()
	appDir := t.TempDir()
	if err := Init(ctx, appDir, KitOptions{Agents: "claude"}, nil); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	initGitRepo(t, appDir)
	st := openTestStore(t, appDir)

	// Commit initial state
	commitAll(t, appDir, "chore: init kit")

	// Add an archived change and commit with OpenSpec-Change trailer
	archDir := filepath.Join(appDir, "openspec", "changes", "archive", "2026-11-08-add-login")
	_ = os.MkdirAll(archDir, 0o755)
	_ = os.WriteFile(filepath.Join(archDir, "tasks.md"), []byte("- [x] 1.1 Implement login\n- [x] 1.2 Add tests\n"), 0o644)
	_ = os.WriteFile(filepath.Join(archDir, "handoff.md"), []byte("Keep session tokens HTTP-only.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(archDir, "repairs.md"), []byte("## Round 1 (apply)\nResult: fixed lint\n"), 0o644)
	baseSHA, _ := gitx.RunGit(ctx, appDir, "rev-parse", "HEAD")
	baseSHA = strings.TrimSpace(baseSHA)
	commitAll(t, appDir, "feat: add login\n\nOpenSpec-Change: add-login")

	// Test CompletedSince
	var compOut bytes.Buffer
	if err := CompletedSince(ctx, appDir, baseSHA, "HEAD", &compOut); err != nil {
		t.Fatalf("CompletedSince failed: %v", err)
	}
	if !strings.Contains(compOut.String(), "add-login") ||
		!strings.Contains(compOut.String(), "2 task(s) complete") ||
		!strings.Contains(compOut.String(), "handoff: Keep session tokens HTTP-only.") ||
		!strings.Contains(compOut.String(), "repair: Round 1 (apply): fixed lint") {
		t.Fatalf("unexpected CompletedSince output:\n%s", compOut.String())
	}

	// Test VisualQA request, begin, status, done, end
	var vqaOut bytes.Buffer
	item, err := VisualQARequest(ctx, appDir, st, "Check login form alignment", "add-login", &vqaOut)
	if err != nil {
		t.Fatalf("VisualQARequest failed: %v", err)
	}
	if err := VisualQABegin(ctx, appDir, st, VisualQAOptions{
		Stamp:            "20261108T120000Z",
		SkipWorktreePrep: true,
	}, &vqaOut); err != nil {
		t.Fatalf("VisualQABegin failed: %v", err)
	}
	if ActiveVisualSession(ctx, st) != "20261108T120000Z" {
		t.Fatalf("expected active visual session 20261108T120000Z, got %q", ActiveVisualSession(ctx, st))
	}
	if err := VisualQADone(ctx, appDir, st, item.ItemID, "verified", "Form looks aligned", &vqaOut); err != nil {
		t.Fatalf("VisualQADone failed: %v", err)
	}
	if err := VisualQAEnd(ctx, appDir, st, VisualQAOptions{
		Stamp:            "20261108T120000Z",
		SkipWorktreePrep: true,
	}, &vqaOut); err != nil {
		t.Fatalf("VisualQAEnd failed: %v", err)
	}
	if ActiveVisualSession(ctx, st) != "" {
		t.Fatalf("expected visual session lock to be released")
	}

	// Test RunCyclePromptCLI
	promptPath := filepath.Join(appDir, "log", "cycle", "prompt-test.md")
	var cpOut, cpErr bytes.Buffer
	if rc := RunCyclePromptCLI(ctx, appDir, []string{promptPath, "--dispatch-running", "1234"}, &cpOut, &cpErr); rc != 0 {
		t.Fatalf("RunCyclePromptCLI failed: %s", cpErr.String())
	}
	promptBytes, err := os.ReadFile(promptPath)
	if err != nil || !strings.Contains(string(promptBytes), "## This cycle (decided by the script)") {
		t.Fatalf("unexpected rendered cycle prompt: %v", err)
	}

	// Test RunCycle with hooks
	agentCalled := false
	dispatchCalled := false
	var cycleOut bytes.Buffer
	err = RunCycle(ctx, appDir, st, CycleOptions{
		Stamp:            "20261108T130000Z",
		SkipWorktreePrep: true,
		AgentExecFunc: func(ctx context.Context, workdir, pf, engine string, timeout time.Duration, out io.Writer) error {
			agentCalled = true
			return nil
		},
		DispatchFunc: func(ctx context.Context, root string, out io.Writer) error {
			dispatchCalled = true
			return nil
		},
	}, &cycleOut)
	if err != nil {
		t.Fatalf("RunCycle failed: %v", err)
	}
	if !agentCalled || !dispatchCalled {
		t.Fatalf("expected AgentExecFunc and DispatchFunc to be called")
	}

	// Test Status and Progress
	var statusOut bytes.Buffer
	if err := Status(ctx, appDir, st, &statusOut); err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if !strings.Contains(statusOut.String(), "== Guards") || !strings.Contains(statusOut.String(), "== Queues") {
		t.Fatalf("unexpected Status output:\n%s", statusOut.String())
	}

	var progOut, progErr bytes.Buffer
	if rc := RunProgressCLI(ctx, appDir, st, []string{"--since", "24h", "--json"}, &progOut, &progErr); rc != 0 {
		t.Fatalf("RunProgressCLI --json failed: %s", progErr.String())
	}
	var progJSON map[string]any
	if err := json.Unmarshal(progOut.Bytes(), &progJSON); err != nil {
		t.Fatalf("invalid progress JSON: %v", err)
	}
	if progJSON["dashboard"] == nil || progJSON["period"] == nil || progJSON["health"] == nil {
		t.Fatalf("missing sections in progress JSON: %+v", progJSON)
	}

	// Test SyncRepos
	var syncOut bytes.Buffer
	if err := SyncRepos(ctx, appDir, st, false, &syncOut); err != nil {
		t.Fatalf("SyncRepos failed: %v", err)
	}
	if !strings.Contains(syncOut.String(), "== Umbrella") {
		t.Fatalf("unexpected SyncRepos output:\n%s", syncOut.String())
	}
}

func TestDaemonForegroundWithWebAndStatusStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appDir := filepath.Join(t.TempDir(), "daemon-app")
	if err := Init(ctx, appDir, KitOptions{Agents: "claude"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenAt(filepath.Join(t.TempDir(), "daemon.db"), "daemon-app", appDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// A daemon started with --set runs with a value that is in no file: its status says so.
	settingstest.Give(t, settings.GateTimeout, "50m")

	webStarted := make(chan string, 1)
	doneCh := make(chan int, 1)

	go func() {
		var out, errBuf bytes.Buffer
		code := StartDaemon(ctx, appDir, st, DaemonOptions{
			Detach:         false,
			Web:            true,
			WebSet:         true,
			Bind:           "127.0.0.1",
			Port:           14567,
			Interval:       10 * time.Minute,
			NoInitialCycle: true,
			StartWebFunc: func(ctx context.Context, root string, st *store.Store, bind string, port int, portExplicit bool) (string, func(context.Context) error, error) {
				u := fmt.Sprintf("http://%s:%d", bind, port)
				webStarted <- u
				return u, func(context.Context) error { return nil }, nil
			},
		}, &out, &errBuf)
		doneCh <- code
	}()

	select {
	case u := <-webStarted:
		if u != "http://127.0.0.1:14567" {
			t.Fatalf("unexpected web URL: %s", u)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for daemon web start")
	}

	// Wait until daemon lock is visible in SQLite
	deadline := time.Now().Add(2 * time.Second)
	var daemonLock *store.LockRecord
	for time.Now().Before(deadline) {
		if dl, _ := st.GetLock(ctx, "daemon"); dl != nil {
			daemonLock = dl
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if daemonLock == nil {
		t.Fatal("the daemon took no lock")
	}
	// What reads the lock from another process (the Web Portal) gets what the daemon was given.
	if got := GivenToDaemon(daemonLock.MetadataJSON); len(got) != 1 || got[0] != "pipeline.gates.timeout=50m" {
		t.Fatalf("GivenToDaemon over the lock of a daemon started with --set = %q, want [pipeline.gates.timeout=50m]", got)
	}

	// Verify StatusDaemon and Status show the running daemon and web URL
	var stOut bytes.Buffer
	if rc := StatusDaemon(ctx, appDir, st, &stOut); rc != 0 || !strings.Contains(stOut.String(), "web=http://127.0.0.1:14567") {
		t.Fatalf("unexpected StatusDaemon output (rc=%d): %s", rc, stOut.String())
	}
	if !strings.Contains(stOut.String(), "daemon: given for this run: --set pipeline.gates.timeout=50m (in no file: it holds until the daemon stops)") {
		t.Fatalf("the status of a daemon started with --set does not say what it was given: %s", stOut.String())
	}

	var fullStatus bytes.Buffer
	if err := Status(ctx, appDir, st, &fullStatus); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fullStatus.String(), "RUNNING: daemon") || !strings.Contains(fullStatus.String(), "http://127.0.0.1:14567, given --set pipeline.gates.timeout=50m)") {
		t.Fatalf("expected RUNNING: daemon with web URL and what it was given in Status output, got:\n%s", fullStatus.String())
	}

	// Verify second StartDaemon call detects running daemon without duplicating
	var dupOut, dupErr bytes.Buffer
	if rc := StartDaemon(ctx, appDir, st, DaemonOptions{Detach: true}, &dupOut, &dupErr); rc != 0 || !strings.Contains(dupOut.String(), "daemon: already running") {
		t.Fatalf("expected already running message, got rc=%d out=%s err=%s", rc, dupOut.String(), dupErr.String())
	}

	cancel()
	select {
	case code := <-doneCh:
		if code != 0 {
			t.Fatalf("expected daemon exit code 0 on cancel, got %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for daemon shutdown")
	}
}

func TestAgentHistoryLogrotateMinifiedOnePerLine(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Build 180 history entries (~1,260 pretty-printed lines > 1,000 lines)
	var batch1 []map[string]any
	for i := 1; i <= 180; i++ {
		batch1 = append(batch1, map[string]any{
			"change_name":   fmt.Sprintf("feat-item-%03d", i),
			"description":   fmt.Sprintf("Description for feature %d", i),
			"priority":      "normal",
			"integrated_at": "2026-11-08T10:00:00Z",
		})
	}

	active1, err := WriteHistoryWithRotation(root, batch1)
	if err != nil {
		t.Fatalf("WriteHistoryWithRotation batch1 failed: %v", err)
	}
	if len(active1) == 0 || len(active1) >= len(batch1) {
		t.Fatalf("expected active history to retain tail items, got %d items", len(active1))
	}

	// Verify .foreman-kit/agent_history.json is <= 1000 lines
	activeBytes, err := os.ReadFile(filepath.Join(root, ".foreman-kit", "agent_history.json"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(activeBytes, []byte("\n")); lines > HistoryMaxLines {
		t.Fatalf("expected active agent_history.json <= %d lines, got %d", HistoryMaxLines, lines)
	}

	// Verify .foreman-kit/agent_history.1.json exists and is minified with 1 change per line
	rot1Bytes, err := os.ReadFile(filepath.Join(root, ".foreman-kit", "agent_history.1.json"))
	if err != nil {
		t.Fatalf("expected .foreman-kit/agent_history.1.json to exist: %v", err)
	}
	rot1Lines := strings.Split(strings.TrimSpace(string(rot1Bytes)), "\n")
	// Format is "[\n" + N minified lines + "\n]" => N + 2 lines
	expectedRot1Items := len(batch1) - len(active1)
	if len(rot1Lines) != expectedRot1Items+2 {
		t.Fatalf("expected %d lines in minified agent_history.1.json (1 per change + brackets), got %d", expectedRot1Items+2, len(rot1Lines))
	}
	for _, line := range rot1Lines[1 : len(rot1Lines)-1] {
		if !strings.HasPrefix(line, `{"`) || strings.Contains(line, "\n  ") {
			t.Fatalf("expected minified single-line JSON object in agent_history.1.json, got: %s", line)
		}
	}

	// Trigger a second rotation by appending 170 more items to active1
	batch2 := append([]map[string]any{}, active1...)
	for i := 181; i <= 350; i++ {
		batch2 = append(batch2, map[string]any{
			"change_name":   fmt.Sprintf("feat-item-%03d", i),
			"description":   fmt.Sprintf("Description for feature %d", i),
			"priority":      "high",
			"integrated_at": "2026-11-08T11:00:00Z",
		})
	}
	active2, err := WriteHistoryWithRotation(root, batch2)
	if err != nil {
		t.Fatalf("WriteHistoryWithRotation batch2 failed: %v", err)
	}
	if len(active2) == 0 {
		t.Fatal("expected non-empty active2")
	}

	// The second rotation fills agent_history.1.json instead of opening a second small file: a
	// rotated file only shifts once it holds HistoryMaxLines lines.
	rotPaths := RotatedHistoryRelPaths(root)
	if len(rotPaths) != 1 || rotPaths[0] != ".foreman-kit/agent_history.1.json" {
		t.Fatalf("expected [.foreman-kit/agent_history.1.json], got %v", rotPaths)
	}
	rot1Again, err := os.ReadFile(filepath.Join(root, ".foreman-kit", "agent_history.1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bytes.Count(rot1Again, []byte("\n")), 350-len(active2)+2; got != want {
		t.Fatalf("expected %d lines in agent_history.1.json after the second rotation, got %d", want, got)
	}

	// Verify ReadAllHistory and SQLite SyncJSONToSQLite read all 350 changes across active + rotated files
	allHist, err := ReadAllHistory(root)
	if err != nil {
		t.Fatalf("ReadAllHistory failed: %v", err)
	}
	if len(allHist) != 350 {
		t.Fatalf("expected 350 total history items across active + rotated files, got %d", len(allHist))
	}

	st, err := store.OpenAt(filepath.Join(t.TempDir(), "hist.db"), "hist-ws", root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	migRep, err := st.MigrateFromFiles(ctx, root, nil)
	if err != nil {
		t.Fatalf("MigrateFromFiles failed: %v", err)
	}
	if migRep.HistoryItems != 350 {
		t.Fatalf("expected 350 integrated changes synced into SQLite from rotated + active history, got %d", migRep.HistoryItems)
	}
}

func TestVisualQABeginDefaultServiceCheckAndPrepareViaCLI(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := Init(ctx, root, KitOptions{Agents: "claude"}, io.Discard); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	initGitRepo(t, root)
	st := openTestStore(t, root)

	repoDir := filepath.Join(root, "repos", "webapp")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	repoCfg := `kind: web
required_services:
  check_db: "true"
visual_qa:
  prepare: "touch .prepared_by_visual_qa"
`
	if err := os.WriteFile(filepath.Join(repoDir, "repo.config.yaml"), []byte(repoCfg), 0o644); err != nil {
		t.Fatalf("WriteFile repo.config.yaml failed: %v", err)
	}
	commitAll(t, root, "chore: add webapp repo")

	var out, errOut bytes.Buffer
	rc := RunVisualQACLI(ctx, root, st, []string{"begin"}, &out, &errOut)
	if rc != 0 {
		t.Fatalf("RunVisualQACLI begin failed (rc=%d): out=%s err=%s", rc, out.String(), errOut.String())
	}

	preparedMarker := filepath.Join(root, ".worktrees", "_visual_qa", "repos", "webapp", ".prepared_by_visual_qa")
	if !fileExists(preparedMarker) {
		t.Fatalf("expected %s to be created by visual_qa.prepare", preparedMarker)
	}
}
