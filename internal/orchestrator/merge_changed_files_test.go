package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

// mcfSandbox is an umbrella with two child repositories, core and mobile. Each one's only gate
// appends to $MARKER_DIR/<name>.ran the slot it ran on and the FOREMANKIT_CHANGED_FILES it was given;
// core also has a full regression that runs when a change touches config/locales/**. Core's test gate
// fails when a file named BROKEN sits at its root, and its regression when one exists anywhere in it.
type mcfSandbox struct {
	tmp, umbrella, umbrellaBare, markers string
	childBare                            map[string]string
	st                                   *store.Store
}

func mcfGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return mustGit(t, context.Background(), dir, args...)
}

// mcfOptions customizes newMCFSandboxWith.
type mcfOptions struct {
	// outsideCommits is merge.outside_commits ("": the file does not write it).
	outsideCommits string
	// baseRecheckAfter is merge.base_recheck_after ("": never, so that a base is only checked again
	// when a test asks for it).
	baseRecheckAfter string
	// coreRegressionWhen replaces core's regression.when (default ["config/locales/**"]).
	coreRegressionWhen []string
	// coreTestFailsWhen is the shell condition that makes core's test gate fail (default
	// "[ -e BROKEN ]": a file named BROKEN at core's root). Core's regression fails whenever a file
	// named BROKEN exists anywhere in core.
	coreTestFailsWhen string
	// coreRegressionFailsWhen is the shell condition that makes core's regression fail (default: a
	// file named BROKEN anywhere in core).
	coreRegressionFailsWhen string
}

// newMCFSandbox returns a sandbox whose .foreman-kit/pipeline.yaml writes merge.outside_commits as given
// ("": not at all).
func newMCFSandbox(t *testing.T, outsideCommits string) *mcfSandbox {
	t.Helper()
	return newMCFSandboxWith(t, mcfOptions{outsideCommits: outsideCommits})
}

func newMCFSandboxWith(t *testing.T, opts mcfOptions) *mcfSandbox {
	t.Helper()
	merge := "merge:\n  conflict_rebuilds: 2\n"
	if opts.outsideCommits != "" {
		merge += "  outside_commits: " + opts.outsideCommits + "\n"
	}
	if opts.baseRecheckAfter == "" {
		opts.baseRecheckAfter = "never"
	}
	merge += "  base_recheck_after: " + opts.baseRecheckAfter + "\n"
	regressionWhen := opts.coreRegressionWhen
	if regressionWhen == nil {
		regressionWhen = []string{"config/locales/**"}
	}
	var regressionWhenYAML strings.Builder
	for _, g := range regressionWhen {
		regressionWhenYAML.WriteString("    - \"" + g + "\"\n")
	}
	tmp := t.TempDir()
	sb := &mcfSandbox{tmp: tmp, childBare: map[string]string{}, markers: filepath.Join(tmp, "markers")}
	if err := os.MkdirAll(sb.markers, 0o755); err != nil {
		t.Fatal(err)
	}
	emptyCfg := filepath.Join(tmp, "empty.gitconfig")
	mustWriteFile(t, emptyCfg, "")

	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(tmp, "state"))
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_APP_ROOT", "")
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "1")
	settingstest.Give(t, settings.EvaluatorEnabled, "false")
	t.Setenv("MARKER_DIR", sb.markers)
	t.Setenv("GIT_CONFIG_GLOBAL", emptyCfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	// Each gate appends "slot=... files=..." to <repo>.ran; core's gates also append
	// "gate=<gate> slot=... head=<git rev-parse HEAD> files=..." to core.log, so a test can tell a run
	// on the base alone (head = the base) from one with the change (head = the rebased change).
	ran := func(name string) string {
		return `    printf 'slot=%s files=%s\n' "$FOREMANKIT_SLOT" "$(printf '%s' "$FOREMANKIT_CHANGED_FILES" | tr '\n' ' ')" >> "$MARKER_DIR/` + name + `.ran"` + "\n"
	}
	logged := func(name, gate string) string {
		return `    printf 'gate=` + gate + ` slot=%s head=%s files=%s\n' "$FOREMANKIT_SLOT" "$(git rev-parse HEAD)" "$(printf '%s' "$FOREMANKIT_CHANGED_FILES" | tr '\n' ' ')" >> "$MARKER_DIR/` + name + `.log"` + "\n"
	}
	testFailsWhen := opts.coreTestFailsWhen
	if testFailsWhen == "" {
		testFailsWhen = "[ -e BROKEN ]"
	}
	regressionFailsWhen, regressionFailure := opts.coreRegressionFailsWhen, "core regression: failing"
	if regressionFailsWhen == "" {
		regressionFailsWhen, regressionFailure = `[ -n "$(find . -name BROKEN -not -path './.git/*' | head -n 1)" ]`, "core regression: BROKEN is present"
	}
	// The script of a regression command sits one level deeper than the script of a gate.
	deeper := func(script string) string {
		return "  " + strings.ReplaceAll(strings.TrimSuffix(script, "\n"), "\n", "\n  ") + "\n"
	}
	configs := map[string]string{
		"core": "base_branch: main\ngates:\n  test: |\n" + ran("core") + logged("core", "test") +
			"    if " + testFailsWhen + "; then echo 'core test: failing'; exit 1; fi\n" +
			"regression:\n  commands:\n    full: |\n" +
			deeper(`    printf 'slot=%s\n' "$FOREMANKIT_SLOT" >> "$MARKER_DIR/core.regression"`+"\n"+logged("core", "regression")+
				"    if "+regressionFailsWhen+"; then echo '"+regressionFailure+"'; exit 1; fi\n") +
			"  when:\n" + regressionWhenYAML.String(),
		"mobile": "base_branch: main\ngates:\n  test: |\n" + ran("mobile"),
	}
	for _, name := range []string{"core", "mobile"} {
		bare := filepath.Join(tmp, "remotes", name+".git")
		if err := os.MkdirAll(bare, 0o755); err != nil {
			t.Fatal(err)
		}
		mcfGit(t, bare, "init", "--bare", "-b", "main")
		seed := filepath.Join(tmp, "seed-"+name)
		mustWriteFile(t, filepath.Join(seed, "repo.config.yaml"), configs[name])
		mustWriteFile(t, filepath.Join(seed, "README.md"), "# "+name+"\n")
		mustWriteFile(t, filepath.Join(seed, "config", "locales", "es.yml"), "es:\n  saludo: \"Cancion\"\n")
		mcfGit(t, seed, "init", "-b", "main")
		mcfGit(t, seed, "add", "-A")
		mcfGit(t, seed, "commit", "-m", "chore: initial "+name)
		mcfGit(t, seed, "remote", "add", "origin", bare)
		mcfGit(t, seed, "push", "-u", "origin", "main")
		sb.childBare[name] = bare
	}

	sb.umbrellaBare = filepath.Join(tmp, "remotes", "umbrella.git")
	if err := os.MkdirAll(sb.umbrellaBare, 0o755); err != nil {
		t.Fatal(err)
	}
	mcfGit(t, sb.umbrellaBare, "init", "--bare", "-b", "master")

	sb.umbrella = filepath.Join(tmp, "umbrella")
	for rel, body := range map[string]string{
		".foreman-kit/kit.lock":           "{}\n",
		".gitignore":                      "/.worktrees/\n/log/\n",
		"MEMORY.md":                       "# Memory\n",
		"openspec/config.yaml":            "schema: spec-driven\n",
		".foreman-kit/agent_backlog.json": "[]\n",
		".foreman-kit/agent_history.json": "[]\n",
	} {
		mustWriteFile(t, filepath.Join(sb.umbrella, rel), body)
	}
	settingstest.WriteFiles(t, sb.umbrella, map[string]string{"pipeline": "repository:\n  base_branch: master\ndispatch:\n  workers: 1\nblocked_changes:\n  retries: 0\n" +
		"gates:\n  timeout: 1m\nlocks:\n  stale_after: 5m\n" + merge})
	mcfGit(t, sb.umbrella, "init", "-b", "master")
	for _, name := range []string{"core", "mobile"} {
		mcfGit(t, sb.umbrella, "submodule", "add", "-b", "main", sb.childBare[name], "repos/"+name)
	}
	mcfGit(t, sb.umbrella, "add", "-A")
	mcfGit(t, sb.umbrella, "commit", "-m", "chore: initial umbrella")
	mcfGit(t, sb.umbrella, "remote", "add", "origin", sb.umbrellaBare)
	mcfGit(t, sb.umbrella, "push", "-u", "origin", "master")

	st, err := store.Open(sb.umbrella)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sb.st = st
	return sb
}

// buildCoreChange runs the dispatcher with a stub agent whose change edits only repos/core/feature.txt.
func (sb *mcfSandbox) buildCoreChange(t *testing.T, change string) {
	t.Helper()
	sb.buildCoreChangeWriting(t, change, map[string]string{"feature.txt": "core payload\n"})
}

// buildCoreChangeWriting runs the dispatcher with a stub agent whose change writes files (paths
// relative to repos/core) and nothing else, and checks that the change reached the merge queue.
func (sb *mcfSandbox) buildCoreChangeWriting(t *testing.T, change string, files map[string]string) {
	t.Helper()
	sb.buildCoreChanges(t, map[string]map[string]string{change: files})
}

// buildCoreChanges builds several changes in one dispatch, each writing its own files (paths relative
// to repos/core, or to the umbrella when they start with repos/) and claiming its own capability, and
// checks that all reached the merge queue.
func (sb *mcfSandbox) buildCoreChanges(t *testing.T, changes map[string]map[string]string) {
	t.Helper()
	var names []string
	for name := range changes {
		names = append(names, name)
	}
	sort.Strings(names)
	var items []string
	for _, name := range names {
		items = append(items, `{"change_name": "`+name+`", "description": "Add a feature file to core", "priority": "high"}`)
	}
	mustWriteFile(t, filepath.Join(sb.umbrella, ".foreman-kit", "agent_backlog.json"), "["+strings.Join(items, ", ")+"]")
	disp, err := NewDispatcher(DispatcherOptions{
		Root:       sb.umbrella,
		Store:      sb.st,
		MaxWorkers: len(names),
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
			for rel, content := range changes[req.Change] {
				target := filepath.Join(req.Worktree, "repos", "core", filepath.FromSlash(rel))
				if strings.HasPrefix(rel, "repos/") {
					target = filepath.Join(req.Worktree, filepath.FromSlash(rel))
				}
				mustWriteFile(t, target, content)
			}
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
	ready, _ := sb.st.ListReadyQueue(context.Background())
	for _, name := range names {
		found := false
		for _, r := range ready {
			found = found || r.ChangeName == name
		}
		if !found {
			qs, _ := sb.st.ListQuarantine(context.Background(), "")
			t.Fatalf("expected %s in the merge queue after dispatch, got ready=%+v quarantine=%+v", name, ready, qs)
		}
	}
}

// integrateMobileChange moves master from M0 to M1 the way another change would: a commit in mobile
// and the umbrella's mobile gitlink updated on master.
func (sb *mcfSandbox) integrateMobileChange(t *testing.T) string {
	t.Helper()
	mobile := filepath.Join(sb.tmp, "side-mobile")
	mcfGit(t, sb.tmp, "clone", "-b", "main", sb.childBare["mobile"], mobile)
	mustWriteFile(t, filepath.Join(mobile, "mobile_feature.txt"), "mobile payload\n")
	mcfGit(t, mobile, "add", "-A")
	mcfGit(t, mobile, "commit", "-m", "feat(mobile): another change")
	mcfGit(t, mobile, "push", "origin", "main")

	umbrella := filepath.Join(sb.tmp, "side-umbrella")
	mcfGit(t, sb.tmp, "clone", "-b", "master", sb.umbrellaBare, umbrella)
	mcfGit(t, umbrella, "submodule", "update", "--init", "repos/mobile")
	mcfGit(t, filepath.Join(umbrella, "repos", "mobile"), "fetch", "origin", "main")
	mcfGit(t, filepath.Join(umbrella, "repos", "mobile"), "checkout", "--quiet", "origin/main")
	mcfGit(t, umbrella, "add", "repos/mobile")
	mcfGit(t, umbrella, "commit", "-m", "chore(merge): integrate other-mobile-change\n\nOpenSpec-Change: other-mobile-change")
	mcfGit(t, umbrella, "push", "origin", "master")
	return mcfGit(t, umbrella, "rev-parse", "HEAD")
}

// pushCoreHotfix pushes a one-accent fix straight to core's main, outside the pipeline: the umbrella
// does not record it, and it touches a path of core's regression.when.
func (sb *mcfSandbox) pushCoreHotfix(t *testing.T) string {
	t.Helper()
	return sb.pushCoreCommit(t, "config/locales/es.yml", "es:\n  saludo: \"Canción\"\n", "fix(core): accent in a translation")
}

// pushCoreCommit pushes one commit that writes rel straight to core's main, outside the pipeline.
func (sb *mcfSandbox) pushCoreCommit(t *testing.T, rel, content, message string) string {
	t.Helper()
	return sb.pushCoreChanges(t, map[string]string{rel: content}, nil, message)
}

// pushCoreChanges pushes one commit that writes and removes files straight to core's main, outside
// the pipeline, from a side clone kept up to date across calls. It returns the commit.
func (sb *mcfSandbox) pushCoreChanges(t *testing.T, writes map[string]string, removes []string, message string) string {
	t.Helper()
	core := filepath.Join(sb.tmp, "side-core")
	if _, err := os.Stat(core); err != nil {
		mcfGit(t, sb.tmp, "clone", "-b", "main", sb.childBare["core"], core)
	} else {
		mcfGit(t, core, "pull", "--quiet", "--ff-only", "origin", "main")
	}
	for rel, content := range writes {
		mustWriteFile(t, filepath.Join(core, filepath.FromSlash(rel)), content)
	}
	for _, rel := range removes {
		mcfGit(t, core, "rm", "--quiet", rel)
	}
	mcfGit(t, core, "add", "-A")
	mcfGit(t, core, "commit", "-m", message)
	mcfGit(t, core, "push", "origin", "main")
	return mcfGit(t, core, "rev-parse", "HEAD")
}

func (sb *mcfSandbox) marker(name string) []string {
	raw, err := os.ReadFile(filepath.Join(sb.markers, name))
	if err != nil {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// mergerRun integrates the ready change and returns the gate request and output the merger produced.
func (sb *mcfSandbox) mergerRun(t *testing.T) (GateRunRequest, string) {
	t.Helper()
	var gotReq GateRunRequest
	var gotOut string
	merger, err := NewMerger(MergerOptions{
		Root:  sb.umbrella,
		Store: sb.st,
		GateRunner: func(ctx context.Context, req GateRunRequest) (GateRunResult, error) {
			gotReq = req
			res, err := DefaultGateRunner(sb.st)(ctx, req)
			gotOut = res.Output
			return res, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := merger.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = merger.Close()
	return gotReq, gotOut
}

func (sb *mcfSandbox) requireIntegrated(t *testing.T, change string) {
	t.Helper()
	mcfGit(t, sb.umbrella, "fetch", "origin", "master")
	rootRepo, err := gitx.Open(sb.umbrella)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := rootRepo.HasCommitWithTrailer("origin/master", change); !ok {
		qs, _ := sb.st.ListQuarantine(context.Background(), "")
		t.Fatalf("%s was not integrated; quarantine=%+v", change, qs)
	}
}

func mergerLine(t *testing.T, lines []string) string {
	t.Helper()
	for _, l := range lines {
		if strings.HasPrefix(l, "slot=99 ") {
			return l
		}
	}
	t.Fatalf("core's gates did not run in the merger (slot 99): %v", lines)
	return ""
}

// The merger measures what a change touched against the master it integrates onto (M1), never the
// fork point the change started from (M0): another change integrated meanwhile never makes it run the
// gates of a repository it did not touch.
func TestMergerRunsOnlyTheGatesOfTheRepositoriesTheChangeTouched(t *testing.T) {
	sb := newMCFSandbox(t, "")
	m0 := mcfGit(t, sb.umbrella, "rev-parse", "origin/master")
	sb.buildCoreChange(t, "core-feature")
	m1 := sb.integrateMobileChange(t)

	var sinceM0 []string
	merger, err := NewMerger(MergerOptions{
		Root:  sb.umbrella,
		Store: sb.st,
		GateRunner: func(ctx context.Context, req GateRunRequest) (GateRunResult, error) {
			if req.BaseRef != m1 {
				t.Errorf("gates measured against %s, want the current master M1 %s", req.BaseRef, m1)
			}
			sinceM0, _ = gitx.ChangedPaths(ctx, req.Worktree, m0)
			return DefaultGateRunner(sb.st)(ctx, req)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := merger.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = merger.Close()

	found := false
	for _, p := range sinceM0 {
		found = found || p == "repos/mobile"
	}
	if !found {
		t.Fatalf("the scenario does not move mobile between M0 and M1: changed since M0 = %v", sinceM0)
	}
	if got := sb.marker("mobile.ran"); len(got) != 0 {
		t.Fatalf("mobile's gates ran although the change never touched mobile: %v", got)
	}
	if line := mergerLine(t, sb.marker("core.ran")); line != "slot=99 files=feature.txt" {
		t.Fatalf("core's gates at merge got %q, want only the change's feature.txt", line)
	}
	sb.requireIntegrated(t, "core-feature")
}

// With merge.outside_commits: ignore, a hotfix pushed straight to a repository is integrated with
// the next change that touches it, but neither widens that change's tests nor runs the repository's
// regression: changing an accent never triggers a retest.
func TestMergeChangedFilesChangeKeepsAHotfixOutOfTheChangesTests(t *testing.T) {
	sb := newMCFSandbox(t, "ignore")
	sb.buildCoreChange(t, "core-feature")
	_ = sb.integrateMobileChange(t)
	hotfix := sb.pushCoreHotfix(t)

	req, out := sb.mergerRun(t)
	if req.OutsideCommits != settings.OutsideCommitsIgnore {
		t.Fatalf("merge.outside_commits = %q, want ignore", req.OutsideCommits)
	}
	if req.ChildOnto["repos/core"] != hotfix {
		t.Fatalf("ChildOnto[repos/core] = %q, want core's current main %s", req.ChildOnto["repos/core"], hotfix)
	}
	if line := mergerLine(t, sb.marker("core.ran")); line != "slot=99 files=feature.txt" {
		t.Fatalf("core's gates at merge got %q, want only the change's feature.txt (no hotfix file)", line)
	}
	if got := sb.marker("core.regression"); len(got) != 0 {
		t.Fatalf("the hotfix ran core's full regression: %v", got)
	}
	if list, _ := sb.st.ListBaseVerifications(context.Background(), "repos/core", ""); len(list) != 0 {
		t.Fatalf("change mode verified the base: %+v", list)
	}
	if !strings.Contains(out, "repos/core: 1 commit(s) pushed to the repository outside the pipeline") || !strings.Contains(out, "is ignore, so only the change's own commits count") {
		t.Fatalf("expected the gate output to explain the hotfix, got:\n%s", out)
	}
	if got := sb.marker("mobile.ran"); len(got) != 0 {
		t.Fatalf("mobile's gates ran: %v", got)
	}
	sb.requireIntegrated(t, "core-feature")

	// The hotfix still reaches the umbrella: the pointer it publishes contains it.
	pointer := strings.Fields(mcfGit(t, sb.umbrella, "ls-tree", "origin/master", "repos/core"))[2]
	mcfGit(t, sb.childBare["core"], "merge-base", "--is-ancestor", hotfix, pointer)
}

// With merge.outside_commits: include, the same hotfix counts as changed: its file reaches
// FOREMANKIT_CHANGED_FILES and, because it matches regression.when, the regression runs.
func TestMergeChangedFilesIntegratedTestsTheHotfixWithTheChange(t *testing.T) {
	sb := newMCFSandbox(t, "include")
	sb.buildCoreChange(t, "core-feature")
	_ = sb.pushCoreHotfix(t)

	req, out := sb.mergerRun(t)
	if req.OutsideCommits != settings.OutsideCommitsInclude {
		t.Fatalf("merge.outside_commits = %q, want include", req.OutsideCommits)
	}
	line := mergerLine(t, sb.marker("core.ran"))
	if !strings.Contains(line, "config/locales/es.yml") || !strings.Contains(line, "feature.txt") {
		t.Fatalf("core's gates at merge got %q, want the change's and the hotfix's files", line)
	}
	if got := sb.marker("core.regression"); len(got) != 1 || got[0] != "slot=99" {
		t.Fatalf("expected the regression to run once in the merger, got %v", got)
	}
	if !strings.Contains(out, "is include, so their files count as changed too") {
		t.Fatalf("expected the gate output to explain the hotfix, got:\n%s", out)
	}
	sb.requireIntegrated(t, "core-feature")
}

// With merge.outside_commits: include, a hotfix whose files match no regression.when glob only
// widens FOREMANKIT_CHANGED_FILES: the regression runs for a hotfix of a regression.when path, never for
// one of a file it does not name.
func TestMergeChangedFilesIntegratedRunsTheRegressionOnlyForAHotfixOfARegressionWhenPath(t *testing.T) {
	sb := newMCFSandbox(t, "include")
	sb.buildCoreChange(t, "core-feature")
	_ = sb.pushCoreCommit(t, "notes.txt", "release notes\n", "docs(core): release notes")

	_, out := sb.mergerRun(t)
	line := mergerLine(t, sb.marker("core.ran"))
	if !strings.Contains(line, "notes.txt") || !strings.Contains(line, "feature.txt") {
		t.Fatalf("core's gates at merge got %q, want the change's and the hotfix's files", line)
	}
	if got := sb.marker("core.regression"); len(got) != 0 {
		t.Fatalf("a hotfix of a file regression.when does not name ran the regression: %v", got)
	}
	if !strings.Contains(out, "is include, so their files count as changed too") {
		t.Fatalf("expected the gate output to explain the hotfix, got:\n%s", out)
	}
	sb.requireIntegrated(t, "core-feature")
}
