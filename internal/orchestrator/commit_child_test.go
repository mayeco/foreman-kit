package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildCoreChangeWith dispatches one change whose BUILD agent runs build with the directory of the
// core child in its worktree, and returns the quarantine record of the change (nil: none) and whether
// it reached the merge queue.
func (sb *mcfSandbox) buildCoreChangeWith(t *testing.T, change string, build func(core string)) (*string, *string, bool) {
	t.Helper()
	mustWriteFile(t, filepath.Join(sb.umbrella, ".foreman-kit", "agent_backlog.json"),
		`[{"change_name": "`+change+`", "description": "Add a feature file to core", "priority": "high"}]`)
	disp, err := NewDispatcher(DispatcherOptions{
		Root:       sb.umbrella,
		Store:      sb.st,
		MaxWorkers: 1,
		AgentRunner: func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
			chgDir := filepath.Join(req.Worktree, "openspec", "changes", req.Change)
			if req.Role == "plan" {
				mustWriteFile(t, filepath.Join(chgDir, "proposal.md"), "# Proposal\n\n## Why\nCore needs a feature file.\n\n## What Changes\n- Add feature.txt to repos/core.\n")
				mustWriteFile(t, filepath.Join(chgDir, "design.md"), "# Design\n\n## Goals\n- Only core changes.\n")
				mustWriteFile(t, filepath.Join(chgDir, "tasks.md"), "# Tasks\n\n- [ ] 1.1 Create feature.txt in repos/core\n")
				mustWriteFile(t, filepath.Join(chgDir, "specs", req.Change+"-cap", "spec.md"),
					"## Purpose\nDescribes the feature file this test change adds to the core repository and nowhere else.\n\n"+
						"## ADDED Requirements\n\n### Requirement: Core feature file\nThe core repository SHALL provide feature.txt.\n\n"+
						"#### Scenario: File present\n- **WHEN** built\n- **THEN** feature.txt exists\n")
				return AgentRunResult{ExitCode: 0, UsedEngine: "stub"}, nil
			}
			build(filepath.Join(req.Worktree, "repos", "core"))
			mustWriteFile(t, filepath.Join(chgDir, "tasks.md"), "# Tasks\n\n- [x] 1.1 Create feature.txt in repos/core\n")
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
	ctx := context.Background()
	var reason, details *string
	if qs, _ := sb.st.ListQuarantine(ctx, ""); len(qs) > 0 {
		for _, q := range qs {
			if q.ChangeName == change {
				r, d := q.Reason, q.Details
				reason, details = &r, &d
			}
		}
	}
	ready, _ := sb.st.ListReadyQueue(ctx)
	for _, r := range ready {
		if r.ChangeName == change {
			return reason, details, true
		}
	}
	return reason, details, false
}

// dirtyNestedSubmoduleOf initializes core's nested submodule vendor/inner the way a build or a gate
// that ran "git submodule update --init" would, and leaves an untracked file inside it.
func dirtyNestedSubmoduleOf(t *testing.T, core string) {
	t.Helper()
	mcfGit(t, core, "submodule", "update", "--init", "vendor/inner")
	mustWriteFile(t, filepath.Join(core, "vendor", "inner", "build-artifact.o"), "artifact\n")
}

// TestBuildWithADirtyNestedSubmoduleIsNotBlockedAsAHookRejection: core has an initialized nested
// submodule whose only change is an untracked file. "git status --porcelain" prints " M vendor/inner",
// "git add -A" cannot stage it and "git commit" used to exit 1 with "no changes added to commit",
// which the dispatcher reported as "commit hook rejected core" and quarantined as gate_failure.
func TestBuildWithADirtyNestedSubmoduleIsNotBlockedAsAHookRejection(t *testing.T) {
	sb := newMCFSandboxWith(t, mcfOptions{coreNestedSubmodule: true})
	reason, details, queued := sb.buildCoreChangeWith(t, "add-feature", func(core string) {
		dirtyNestedSubmoduleOf(t, core)
		mustWriteFile(t, filepath.Join(core, "feature.txt"), "core payload\n")
	})
	if reason != nil {
		t.Fatalf("the change was quarantined as %s: %s", *reason, *details)
	}
	if !queued {
		t.Fatalf("the change did not reach the merge queue")
	}
	ready, _ := sb.st.ListReadyQueue(context.Background())
	core := filepath.Join(ready[0].Worktree, "repos", "core")
	if got := mcfGit(t, core, "show", "HEAD:feature.txt"); got != "core payload" {
		t.Fatalf("feature.txt is not committed in core: %q", got)
	}
	if got := mcfGit(t, core, "log", "-1", "--format=%s"); !strings.Contains(got, "implement OpenSpec change add-feature") {
		t.Fatalf("core's last commit = %q, want the change's commit", got)
	}
}

// TestResumedWorktreeWhoseOnlyDirtIsANestedSubmoduleIsNotBlocked: the change resumes in a worktree
// whose core child holds nothing but an untracked file inside its initialized nested submodule (an
// earlier round committed the real work). keepLeftovers and commitChildren used to find the child
// dirty, fail to commit and quarantine the change as a hook rejection, every retry the same.
func TestResumedWorktreeWhoseOnlyDirtIsANestedSubmoduleIsNotBlocked(t *testing.T) {
	sb := newMCFSandboxWith(t, mcfOptions{coreNestedSubmodule: true})
	ctx := context.Background()
	disp, err := NewDispatcher(DispatcherOptions{Root: sb.umbrella, Store: sb.st, MaxWorkers: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disp.Close() })
	change := "resume-dirty"
	branch := "feat/spec-" + change
	wt := filepath.Join(sb.umbrella, ".worktrees", "worker-"+change)
	if err := disp.provisionWorktree(ctx, wt, change, branch); err != nil {
		t.Fatalf("provisionWorktree: %v", err)
	}
	core := filepath.Join(wt, "repos", "core")
	mcfGit(t, core, "checkout", "--quiet", "-B", branch)
	dirtyNestedSubmoduleOf(t, core)
	before := mcfGit(t, core, "rev-parse", "HEAD")

	if err := disp.keepLeftovers(ctx, wt, change, branch); err != nil {
		t.Fatalf("keepLeftovers: %v", err)
	}
	if err := disp.commitChildren(ctx, wt, change); err != nil {
		t.Fatalf("commitChildren: %v", err)
	}
	if got := mcfGit(t, core, "rev-parse", "HEAD"); got != before {
		t.Fatalf("a commit was made for a dirty nested submodule: %s -> %s", before, got)
	}
	if events := sb.events(t, "change.kept_edits"); len(events) != 0 {
		t.Fatalf("keepLeftovers reported kept edits though it committed nothing: %+v", events[0].Payload)
	}
}

// TestAPreCommitHookRejectionIsStillReportedAsOne: a real hook that refuses the commit keeps the
// "commit hook rejected" message, with the hook's own output and the name of the hook.
func TestAPreCommitHookRejectionIsStillReportedAsOne(t *testing.T) {
	sb := newMCFSandboxWith(t, mcfOptions{coreNestedSubmodule: true})
	reason, details, queued := sb.buildCoreChangeWith(t, "add-hooked", func(core string) {
		dirtyNestedSubmoduleOf(t, core)
		hook := strings.TrimSpace(mcfGit(t, core, "rev-parse", "--git-path", "hooks/pre-commit"))
		if !filepath.IsAbs(hook) {
			hook = filepath.Join(core, hook)
		}
		mustWriteFile(t, hook, "#!/bin/sh\necho 'lint: forbidden word' >&2\nexit 1\n")
		if err := os.Chmod(hook, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(core, "feature.txt"), "core payload\n")
	})
	if queued || reason == nil {
		t.Fatalf("the change reached the merge queue (%v) with no quarantine (%v), want a block", queued, reason)
	}
	if *reason != "gate_failure" {
		t.Fatalf("reason = %q, want gate_failure", *reason)
	}
	for _, want := range []string{"commit hook rejected repos/core", "pre-commit", "lint: forbidden word"} {
		if !strings.Contains(*details, want) {
			t.Fatalf("details %q do not name %q", *details, want)
		}
	}
}

// newDirtyNestedChild returns a git repository with a committed nested submodule vendor/inner that
// holds an untracked file, the shape of a child whose only dirt is inside a nested submodule.
func newDirtyNestedChild(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(tmp, "empty.gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	inner := filepath.Join(tmp, "inner")
	mustWriteFile(t, filepath.Join(inner, "inner.txt"), "inner\n")
	mcfGit(t, inner, "init", "-b", "main")
	mcfGit(t, inner, "add", "-A")
	mcfGit(t, inner, "commit", "-m", "chore: inner")
	child := filepath.Join(tmp, "child")
	mustWriteFile(t, filepath.Join(child, "README.md"), "# child\n")
	mcfGit(t, child, "init", "-b", "main")
	mcfGit(t, child, "submodule", "add", inner, "vendor/inner")
	mcfGit(t, child, "add", "-A")
	mcfGit(t, child, "commit", "-m", "chore: initial")
	mustWriteFile(t, filepath.Join(child, "vendor", "inner", "build-artifact.o"), "artifact\n")
	if got := mcfGit(t, child, "status", "--porcelain"); got != "M vendor/inner" {
		t.Fatalf("fixture status = %q, want \"M vendor/inner\"", got)
	}
	return child
}

func TestCommitChildSkipsAChildWhoseOnlyDirtIsANestedSubmodule(t *testing.T) {
	child := newDirtyNestedChild(t)
	before := mcfGit(t, child, "rev-parse", "HEAD")
	committed, err := (&Dispatcher{}).commitChild(context.Background(), child, "repos/child", "feat: x")
	if err != nil || committed {
		t.Fatalf("commitChild = (%v, %v), want (false, nil)", committed, err)
	}
	if got := mcfGit(t, child, "rev-parse", "HEAD"); got != before {
		t.Fatalf("a commit was made: %s -> %s", before, got)
	}
}

func TestCommitChildCommitsTheFilesBesideADirtyNestedSubmodule(t *testing.T) {
	child := newDirtyNestedChild(t)
	mustWriteFile(t, filepath.Join(child, "feature.txt"), "payload\n")
	committed, err := (&Dispatcher{}).commitChild(context.Background(), child, "repos/child", "feat: x")
	if err != nil || !committed {
		t.Fatalf("commitChild = (%v, %v), want (true, nil)", committed, err)
	}
	if got := mcfGit(t, child, "show", "HEAD:feature.txt"); got != "payload" {
		t.Fatalf("feature.txt = %q", got)
	}
}

// TestCommitChildNamesTheRealCauseOfACommitFailure: with something staged and no hook, a commit that
// fails (here a signature that cannot be made) is not a hook's rejection, and the failure names the
// dirty nested submodule git cannot commit.
func TestCommitChildNamesTheRealCauseOfACommitFailure(t *testing.T) {
	child := newDirtyNestedChild(t)
	mcfGit(t, child, "config", "commit.gpgsign", "true")
	mcfGit(t, child, "config", "gpg.program", "false")
	mustWriteFile(t, filepath.Join(child, "feature.txt"), "payload\n")
	_, err := (&Dispatcher{}).commitChild(context.Background(), child, "repos/child", "feat: x")
	be, ok := err.(*BlockedError)
	if !ok {
		t.Fatalf("err = %v, want a BlockedError", err)
	}
	if strings.Contains(be.Details, "commit hook rejected") {
		t.Fatalf("a failure with no hook was reported as a hook rejection: %s", be.Details)
	}
	for _, want := range []string{"git commit failed in repos/child", "vendor/inner"} {
		if !strings.Contains(be.Details, want) {
			t.Fatalf("details %q do not name %q", be.Details, want)
		}
	}
}
