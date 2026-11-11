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

func TestRemovedLinesNumbersLinesOnTheBase(t *testing.T) {
	diff := `diff --git a/routes.go b/routes.go
index 1..2 100644
--- a/routes.go
+++ b/routes.go
@@ -3,2 +3 @@ var RouteRegistry = []string{
-	"baseline",
-	"teammate-hotfix",
+	"route-from-alpha",
@@ -9 +8,0 @@
-// trailing
diff --git a/new.go b/new.go
new file mode 100644
--- /dev/null
+++ b/new.go
@@ -0,0 +1 @@
+package x
`
	got := removedLines(diff)
	lines := got["routes.go"]
	if len(lines) != 3 || lines[0].line != 3 || lines[1].line != 4 || lines[1].text != "\t\"teammate-hotfix\"," || lines[2].line != 9 {
		t.Fatalf("unexpected removed lines: %+v", got)
	}
	if _, ok := got["new.go"]; ok {
		t.Fatalf("a new file loses no lines: %+v", got)
	}
}

func TestDetectConcurrentWorkLossFindsOnlyCommitsAfterTheStart(t *testing.T) {
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
	write := func(content string) { _ = os.WriteFile(filepath.Join(dir, "routes.go"), []byte(content), 0o644) }
	// commitAt commits with both dates at when: blame reports the committer time.
	commitAt := func(when time.Time, author, msg string) string {
		t.Helper()
		stamp := when.Format(time.RFC3339)
		t.Setenv("GIT_COMMITTER_DATE", stamp)
		git("add", "-A")
		git("-c", "user.name="+author, "-c", "user.email="+author+"@example.com", "commit", "-q", "--date", stamp, "-m", msg)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q", "-b", "main")

	start := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	write("package x\n\nvar R = []string{\n\t\"baseline\",\n\t\"old-route\",\n}\n")
	commitAt(start.Add(-time.Hour), "early", "feat: old route")

	write("package x\n\nvar R = []string{\n\t\"baseline\",\n\t\"old-route\",\n\t\"teammate-hotfix\",\n}\n")
	onto := commitAt(start.Add(10*time.Minute), "teammate", "HOTFIX: production down\n\nRestore the payments route.")

	// The change rewrites the file, dropping both the old route (not concurrent) and the hotfix.
	write("package x\n\nvar R = []string{\n\t\"baseline\",\n\t\"route-from-alpha\",\n}\n")
	rebased := commitAt(start.Add(20*time.Minute), "agent", "feat: alpha")

	lost, err := DetectConcurrentWorkLoss(ctx, dir, "repos/backend", onto, rebased, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 {
		t.Fatalf("expected the hotfix commit only, got %+v", lost)
	}
	lc := lost[0]
	if lc.Commit != onto || lc.Author != "teammate" || !strings.HasPrefix(lc.Message, "HOTFIX: production down") || lc.TotalLost != 1 ||
		len(lc.LostLines) != 1 || !strings.Contains(lc.LostLines[0], "routes.go:6:") || !strings.Contains(lc.LostLines[0], "teammate-hotfix") {
		t.Fatalf("unexpected lost commit: %+v", lc)
	}

	report := FormatWorkLossReport(lost, []string{"importance=critical"}, map[string]any{"importance": "critical"})
	if !strings.Contains(report, "importance=critical") || !strings.Contains(report, `"HOTFIX: production down"`) || !strings.Contains(report, "teammate-hotfix") {
		t.Fatalf("report misses details:\n%s", report)
	}

	if none, _ := DetectConcurrentWorkLoss(ctx, dir, "repos/backend", onto, rebased, start.Add(time.Hour)); len(none) != 0 {
		t.Fatalf("commits before the start are not concurrent: %+v", none)
	}
}

func TestBlockingRulesHonorsAllowWhen(t *testing.T) {
	minConf := 0.4
	allowMin := 0.8
	g := evaluator.GatePolicy{
		BlockWhen: []evaluator.RoutingRule{{Question: "importance", Choice: "critical", MinConfidence: &minConf}},
		AllowWhen: []evaluator.RoutingRule{{Question: "superseded_by_change", Min: &allowMin}},
	}
	critical := map[string]any{"type": "choice", "choice": "critical", "confidence": 0.9}
	v := evaluator.Verdict{Answers: map[string]map[string]any{"importance": critical, "superseded_by_change": {"noul": 0.1}}}
	if rules := evaluator.BlockingRules(v, g); len(rules) != 1 || evaluator.RuleLabel(rules[0]) != "importance=critical" {
		t.Fatalf("critical loss must block: %+v", rules)
	}
	v.Answers["superseded_by_change"] = map[string]any{"noul": 0.95}
	if evaluator.ShouldBlock(v, g) {
		t.Fatal("allow_when (superseded) must override block_when")
	}
	v.Answers = map[string]map[string]any{"importance": {"choice": "trivial", "confidence": 0.95}, "superseded_by_change": {"noul": 0.1}}
	if evaluator.ShouldBlock(v, g) {
		t.Fatal("a trivial loss must not block")
	}
}
