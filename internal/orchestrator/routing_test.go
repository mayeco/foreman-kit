package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
)

func TestActionForPicksTheFirstMatchingRuleAndFallsBack(t *testing.T) {
	c := 0.7
	g := evaluator.GatePolicy{Actions: []evaluator.RoutingRule{
		{Question: "scope", Choice: "change_specific", MinConfidence: &c, Action: "block_change"},
		{Question: "scope", Choice: "transient", MinConfidence: &c, Action: "retry_later"},
	}}
	v := evaluator.Verdict{Answers: map[string]map[string]any{"scope": {"choice": "transient", "confidence": 0.9}}}
	if a, rule := evaluator.ActionFor(v, g); a != "retry_later" || rule == nil || evaluator.RuleLabel(*rule) != "scope=transient" {
		t.Fatalf("got %q %+v", a, rule)
	}
	v.Answers["scope"] = map[string]any{"choice": "transient", "confidence": 0.5}
	if a, _ := evaluator.ActionFor(v, g); a != "" {
		t.Fatalf("a low-confidence answer must not route: %q", a)
	}
	if evaluator.FallbackAction(SystemErrorGate) != "pause" || evaluator.FallbackAction(PushRejectionGate) != "retry" ||
		evaluator.FallbackAction(SemanticMergeGate) != "integrate" || evaluator.FallbackAction(AgentOutcomeGate) != "defer" ||
		evaluator.FallbackAction(PolicyViolationGate) != "repair" || evaluator.FallbackAction(FailureTriageGate) != "repair" {
		t.Fatal("each gate's fallback must be the behavior from before the gate")
	}
}

func TestContractRejectsAnUnknownAction(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	pol := `model: jev-1.14.0
gates:
  push_rejection:
    mode: enforce
    actions:
      - { question: kind, choice: policy, action: force_push }
    questions:
      kind:
        type: choice
        instructions: Why was the push rejected?
        criteria:
          policy: A rule refused it.
          transient: A glitch.
`
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "evaluator.yaml"), []byte(pol), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if failures := evaluator.RunContract(context.Background(), root, false, &out); failures == 0 || !strings.Contains(out.String(), "FAIL  routing rules") {
		t.Fatalf("an action outside GateActions must fail the contract:\n%s", out.String())
	}
}

func newTestRepo(t *testing.T) (string, func(args ...string) string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := gitx.RunGit(ctx, dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "t")
	git("config", "user.email", "t@example.com")
	return dir, git
}

func TestRevertOutsidePathsRestoresTheForkAndCommitsWhatTheAgentCommitted(t *testing.T) {
	ctx := context.Background()
	dir, git := newTestRepo(t)
	_ = os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("base\n"), 0o644)
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	fork := git("rev-parse", "HEAD")

	// The agent changes a tracked file, commits a new file, and leaves an untracked one.
	_ = os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("agent edit\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "committed.log"), []byte("log\n"), 0o644)
	git("add", "committed.log")
	git("commit", "-q", "-m", "agent")
	_ = os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{0, 0, 1}, 0o644)

	paths := []string{"keep.txt", "committed.log", ".DS_Store"}
	state := outsidePathState(ctx, dir, fork, paths)
	if state[0]["status"] != "modified" || state[1]["status"] != "added" || state[2]["preview"] != "(binary)" {
		t.Fatalf("unexpected state: %+v", state)
	}
	if err := revertOutsidePaths(ctx, dir, fork, paths); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "keep.txt")); string(b) != "base\n" {
		t.Fatalf("keep.txt not restored: %q", b)
	}
	for _, p := range []string{"committed.log", ".DS_Store"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			t.Fatalf("%s not removed", p)
		}
	}
	if diff := git("diff", "--name-only", fork, "HEAD"); diff != "" {
		t.Fatalf("HEAD still differs from the fork: %s", diff)
	}
	if st := git("status", "--porcelain"); st != "" {
		t.Fatalf("worktree not clean: %s", st)
	}
}

func TestProtectedRootPathsNeverReachJev(t *testing.T) {
	for _, p := range []string{".github/workflows/ci.yml", ".foreman-kit/agents.yaml", "openspec/specs/auth/spec.md", "MEMORY.md", "memory/repos/backend.md", ".gitmodules"} {
		if !isProtectedRootPath(p) {
			t.Errorf("%s must be protected", p)
		}
	}
	for _, p := range []string{".DS_Store", "debug-agent.log", "coverage.out", "scripts/tmp.sh"} {
		if isProtectedRootPath(p) {
			t.Errorf("%s must be judged", p)
		}
	}
	if !judgedSendBack(WorkLossMarker+"\n...") || !judgedSendBack(SemanticConflictMarker+"\n...") || judgedSendBack("repos/backend: CONFLICT") {
		t.Fatal("judgedSendBack must recognize only Jev send-backs")
	}
}

func TestConcurrentCommitsOnFilesKeepsOnlyLaterCommitsOnTheChangesFiles(t *testing.T) {
	ctx := context.Background()
	dir, git := newTestRepo(t)
	commitAt := func(when time.Time, msg string) string {
		t.Helper()
		stamp := when.Format(time.RFC3339)
		t.Setenv("GIT_COMMITTER_DATE", stamp)
		git("add", "-A")
		git("commit", "-q", "--date", stamp, "-m", msg)
		return git("rev-parse", "HEAD")
	}
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	_ = os.WriteFile(filepath.Join(dir, "routes.go"), []byte("a\nb\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "other.go"), []byte("x\n"), 0o644)
	commitAt(start.Add(-time.Hour), "early")
	_ = os.WriteFile(filepath.Join(dir, "routes.go"), []byte("a\nb\n// invariant: a first\n"), 0o644)
	commitAt(start.Add(5*time.Minute), "fix: invariant a first")
	_ = os.WriteFile(filepath.Join(dir, "other.go"), []byte("y\n"), 0o644)
	onto := commitAt(start.Add(6*time.Minute), "chore: other file")
	_ = os.WriteFile(filepath.Join(dir, "routes.go"), []byte("z\na\nb\n// invariant: a first\n"), 0o644)
	rebased := commitAt(start.Add(10*time.Minute), "feat: z first")

	cs, diff, err := ConcurrentCommitsOnFiles(ctx, dir, "repos/backend", onto, rebased, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Message != "fix: invariant a first" || !strings.Contains(cs[0].Diff, "+// invariant: a first") {
		t.Fatalf("expected only the concurrent commit on routes.go, got %+v", cs)
	}
	if !strings.Contains(diff, "+z") || strings.Contains(diff, "other.go") {
		t.Fatalf("change diff should cover routes.go only:\n%s", diff)
	}
}
