package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/gitx"
)

// runCoreDispatch runs one dispatch of change with a stub agent whose BUILD writes files under
// repos/core, and returns how many BUILD calls the agent got.
func (sb *mcfSandbox) runCoreDispatch(t *testing.T, change string, files map[string]string) int {
	t.Helper()
	mustWriteFile(t, filepath.Join(sb.umbrella, ".foreman-kit", "agent_backlog.json"),
		`[{"change_name": "`+change+`", "description": "Greet in Spanish", "priority": "high"}]`)
	return sb.runCoreDispatchFromCheckout(t, files)
}

// runCoreDispatchFromCheckout runs one dispatch of whatever backlog the main checkout holds, with
// a stub agent whose BUILD writes files under repos/core, and returns the BUILD calls it got.
func (sb *mcfSandbox) runCoreDispatchFromCheckout(t *testing.T, files map[string]string) int {
	t.Helper()
	builds := 0
	disp, err := NewDispatcher(DispatcherOptions{
		Root:       sb.umbrella,
		Store:      sb.st,
		MaxWorkers: 1,
		AgentRunner: func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
			chgDir := filepath.Join(req.Worktree, "openspec", "changes", req.Change)
			if req.Role == "plan" {
				mustWriteFile(t, filepath.Join(chgDir, "proposal.md"), "# Proposal\n\n## Why\nCore greets in Spanish.\n\n## What Changes\n- Edit config/locales/es.yml in repos/core.\n")
				mustWriteFile(t, filepath.Join(chgDir, "design.md"), "# Design\n\n## Goals\n- Only core changes.\n")
				mustWriteFile(t, filepath.Join(chgDir, "tasks.md"), "# Tasks\n\n- [ ] 1.1 Edit the Spanish greeting in repos/core\n")
				mustWriteFile(t, filepath.Join(chgDir, "specs", req.Change+"-cap", "spec.md"),
					"## Purpose\nDescribes the Spanish greeting that the core repository ships in its locale file.\n\n"+
						"## ADDED Requirements\n\n### Requirement: Spanish greeting of "+req.Change+"\nThe core repository SHALL greet in Spanish.\n\n"+
						"#### Scenario: Greeting present\n- **WHEN** built\n- **THEN** es.yml greets\n")
				return AgentRunResult{ExitCode: 0, UsedEngine: "stub"}, nil
			}
			builds++
			for rel, content := range files {
				target := filepath.Join(req.Worktree, "repos", "core", filepath.FromSlash(rel))
				if strings.HasPrefix(rel, "repos/") {
					target = filepath.Join(req.Worktree, filepath.FromSlash(rel))
				}
				mustWriteFile(t, target, content)
			}
			mustWriteFile(t, filepath.Join(chgDir, "tasks.md"), "# Tasks\n\n- [x] 1.1 Edit the Spanish greeting in repos/core\n")
			return AgentRunResult{ExitCode: 0, UsedEngine: "stub"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = disp.Close()
	return builds
}

// TestRebuildOverAnOutsideCommitRebuildsOnTheChildsRemoteBase reproduces a real run: a change
// rewrote core's es.yml while a person pushed an accent fix to the same line straight to core's
// main. The merger rebases onto core's origin/main and conflicts. The rebuild used to rebase onto
// the umbrella's gitlink, which does not record the fix: a no-op, so the same commit came back
// ready, conflicted again, and every conflict_rebuilds was spent without one agent call. Now the
// rebuild starts from what the merger rebases onto, the agent builds again, and it integrates.
func TestRebuildOverAnOutsideCommitRebuildsOnTheChildsRemoteBase(t *testing.T) {
	sb := newMCFSandbox(t, "verify")
	ctx := context.Background()
	greeting := map[string]string{"config/locales/es.yml": "es:\n  saludo: \"Hola\"\n  despedida: \"Adios\"\n"}

	if n := sb.runCoreDispatch(t, "add-greeting", greeting); n != 1 {
		t.Fatalf("first dispatch: %d BUILD calls, want 1", n)
	}
	// Mobile, which the change does not touch, also gets a commit pushed outside the pipeline.
	mobile := filepath.Join(sb.tmp, "side-mobile-outside")
	mcfGit(t, sb.tmp, "clone", "-q", "-b", "main", sb.childBare["mobile"], mobile)
	mustWriteFile(t, filepath.Join(mobile, "MOBILE_HOTFIX.txt"), "x\n")
	mcfGit(t, mobile, "add", "-A")
	mcfGit(t, mobile, "commit", "-qm", "fix(mobile): pushed by a person")
	mcfGit(t, mobile, "push", "-q", "origin", "main")
	mobileLink := mcfGit(t, sb.umbrella, "rev-parse", "HEAD:repos/mobile")

	hotfix := sb.pushCoreChanges(t, map[string]string{
		"config/locales/es.yml": "es:\n  saludo: \"Canción\"\n",
		"HOTFIX.txt":            "pushed by a person\n",
	}, nil, "fix(core): accent in a translation")

	sb.mergerRun(t)
	q, _ := sb.st.GetQuarantine(ctx, "add-greeting")
	if q == nil || q.Status != "rebuild_requested" {
		t.Fatalf("after the conflicting merge: quarantine = %+v, want rebuild_requested", q)
	}

	n2 := sb.runCoreDispatch(t, "add-greeting", greeting)
	if n := n2; n != 1 {
		t.Fatalf("rebuild dispatch: %d BUILD calls, want 1 (the rebuild must call the agent)", n)
	}
	var rebuilds int
	_ = sb.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE event = 'change.rebuild' AND change_name = 'add-greeting'`).Scan(&rebuilds)
	if rebuilds != 1 {
		t.Fatalf("change.rebuild events = %d, want 1", rebuilds)
	}

	// The rebuilt worker's gates count only the change's own commits, not the person's.
	var workerRuns []string
	for _, l := range sb.marker("core.ran") {
		if strings.HasPrefix(l, "slot=1 ") {
			workerRuns = append(workerRuns, l)
		}
	}
	if len(workerRuns) != 2 || strings.Contains(workerRuns[1], "HOTFIX.txt") || !strings.Contains(workerRuns[1], "config/locales/es.yml") {
		t.Fatalf("worker gate runs = %q, want the rebuild to see only config/locales/es.yml", workerRuns)
	}

	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-greeting")
	mcfGit(t, sb.umbrella, "fetch", "-q", "origin", "master")
	if got := mcfGit(t, sb.umbrella, "rev-parse", "origin/master:repos/mobile"); got != mobileLink {
		t.Fatalf("the rebuild moved mobile's gitlink to %s (its outside commit), want it kept at %s", got, mobileLink)
	}

	side := filepath.Join(sb.tmp, "check-core")
	mcfGit(t, sb.tmp, "clone", "-q", "-b", "main", sb.childBare["core"], side)
	if _, err := gitx.RunGit(ctx, side, "merge-base", "--is-ancestor", hotfix, "HEAD"); err != nil {
		t.Fatal("core's main lost the hotfix")
	}
	raw, _ := os.ReadFile(filepath.Join(side, "config", "locales", "es.yml"))
	if !strings.Contains(string(raw), "Hola") {
		t.Fatalf("es.yml = %q, want the change's greeting", raw)
	}
}
