package orchestrator

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestDispatcherPullsAJevPolicyFixBeforeRefusing reproduces a real daemon session: the Jev gates were
// active with no API key, so the daemon's dispatcher refused to start; a person turned the gates off
// and pushed from another clone, but the main checkout is only fast-forwarded by a running dispatcher,
// so it refused every 10 s on a policy origin no longer had.
func TestDispatcherPullsAJevPolicyFixBeforeRefusing(t *testing.T) {
	sb := newMCFSandbox(t, "")
	// The sandbox runs with the evaluator off for the run: here the file says whether it is on.
	settingstest.Take(t, settings.EvaluatorEnabled)
	t.Setenv("TYPESAFE_API_KEY", "")
	seed, err := assets.ReadSeed(".foreman-kit/evaluator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(sb.umbrella, ".foreman-kit", "evaluator.yaml"), string(seed))
	mcfGit(t, sb.umbrella, "add", ".foreman-kit/evaluator.yaml")
	mcfGit(t, sb.umbrella, "commit", "-qm", "chore: jev policy")
	mcfGit(t, sb.umbrella, "push", "-q", "origin", "master")

	if _, err := NewDispatcher(DispatcherOptions{Root: sb.umbrella, Store: sb.st}); err == nil || !strings.Contains(err.Error(), "are active without TYPESAFE_API_KEY") {
		t.Fatalf("with active gates and no key: err = %v, want the refusal", err)
	}

	mate := filepath.Join(sb.tmp, "mate-umbrella")
	mcfGit(t, sb.tmp, "clone", "-q", "-b", "master", sb.umbrellaBare, mate)
	off := regexp.MustCompile(`(?m)^(\s+mode:\s*)(advisory|enforce)`).ReplaceAllString(string(seed), "${1}off")
	mustWriteFile(t, filepath.Join(mate, ".foreman-kit", "evaluator.yaml"), off)
	mcfGit(t, mate, "commit", "-qam", "chore(jev): turn the gates off")
	mcfGit(t, mate, "push", "-q", "origin", "master")

	d, err := NewDispatcher(DispatcherOptions{Root: sb.umbrella, Store: sb.st})
	if err != nil {
		t.Fatalf("after the fix was pushed: %v", err)
	}
	_ = d.Close()
	if raw, _ := os.ReadFile(filepath.Join(sb.umbrella, ".foreman-kit", "evaluator.yaml")); strings.Contains(string(raw), "mode: advisory") {
		t.Fatal("the main checkout was not fast-forwarded to the pushed policy")
	}
}

// TestAnUnreadableBacklogIsReportedWhereAPersonLooks: a malformed .foreman-kit/agent_backlog.json stopped
// everything new silently: only a backlog.unreadable row in SQLite, while dispatch --dry-run said
// "backlog: nothing pending" and neither status nor the daemon log mentioned it.
func TestAnUnreadableBacklogIsReportedWhereAPersonLooks(t *testing.T) {
	sb := newMCFSandbox(t, "")
	mustWriteFile(t, filepath.Join(sb.umbrella, ".foreman-kit", "agent_backlog.json"), `[{"change_name": "x",, }]`)
	var out bytes.Buffer
	d, err := NewDispatcher(DispatcherOptions{Root: sb.umbrella, Store: sb.st, Out: &out, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := d.DryRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.BacklogError == "" || !strings.Contains(rep.FormatText(1, 0), "BACKLOG UNREADABLE: .foreman-kit/agent_backlog.json") {
		t.Fatalf("dry run does not say the backlog is unreadable:\n%s", rep.FormatText(1, 0))
	}
	_ = d.Close()

	out.Reset()
	d, err = NewDispatcher(DispatcherOptions{Root: sb.umbrella, Store: sb.st, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	if n := strings.Count(out.String(), "BACKLOG UNREADABLE"); n != 1 {
		t.Fatalf("the dispatcher log mentions the unreadable backlog %d times, want once:\n%s", n, out.String())
	}
}
