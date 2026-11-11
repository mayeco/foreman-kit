package cycle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
)

// TestUpdateRewritesTheRepoConfigOfEachRepository: `foreman-kit update` rewrites the
// repo.config.yaml of each child repository whose keys an older kit read, in the repository, where
// it is committed, and says how. Until it has, the cycle starts nothing; afterwards nothing is
// pending, whatever else the repository holds uncommitted.
func TestUpdateRewritesTheRepoConfigOfEachRepository(t *testing.T) {
	ctx := context.Background()
	appDir := t.TempDir()
	if err := Init(ctx, appDir, KitOptions{Agents: "claude", Version: "9.0.0-test"}, io.Discard); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := gitx.RunGit(ctx, dir, args...)
		if err != nil {
			t.Fatalf("git %v in %s: %v", args, dir, err)
		}
		return out
	}

	// Three repositories: web and api, whose files an older kit wrote, and docs, whose file is of
	// today. api is on no branch, as a submodule is after a clone, and its regression had no command.
	former, err := os.ReadFile(filepath.Join("..", "config", "testdata", "former", "repos", "web.repo.config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ofToday := "base_branch: trunk\nkind: cli\ngates:\n  test: \"true\"\n"
	formerOfAPI := "base_branch: trunk\ngates:\n  setup: bin/setup\n  test: bin/test\n"
	for name, content := range map[string]string{"web": string(former), "docs": ofToday, "api": formerOfAPI} {
		dir := filepath.Join(appDir, "repos", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, config.RepoConfigFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		// The env_script the file of web names.
		if err := os.MkdirAll(filepath.Join(dir, "script"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "script", "env.sh"), []byte("# shellcheck shell=bash\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(dir, "init", "-q", "-b", "main")
		git(dir, "config", "user.email", "test@example.com")
		git(dir, "config", "user.name", "Test")
		git(dir, "add", "-A")
		git(dir, "commit", "-qm", "the repository")
		if name == "api" {
			git(dir, "checkout", "-q", "--detach")
		}
	}
	git(appDir, "init", "-q", "-b", "main")
	git(appDir, "config", "user.email", "test@example.com")
	git(appDir, "config", "user.name", "Test")
	git(appDir, "add", "-A")
	git(appDir, "commit", "-qm", "an application with three repositories")
	firstCommit := strings.TrimSpace(git(appDir, "rev-parse", "HEAD"))

	// The first one found is named: config check lists them all.
	older := "repos/api/repo.config.yaml holds keys an older foreman-kit read (gates.setup): foreman-kit update rewrites it"
	if got := config.RepoConfigsUnusable(appDir); got != older {
		t.Fatalf("RepoConfigsUnusable = %q", got)
	}
	if err := RequireUsable(appDir, settings.CycleFiles...); err == nil || !strings.Contains(err.Error(), older) {
		t.Fatalf("the cycle would start on a repo.config.yaml of an older kit: %v", err)
	}

	// What a repository holds uncommitted is its own: it does not stop the update.
	if err := os.WriteFile(filepath.Join(appDir, "repos", "web", "notes.txt"), []byte("work in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Upgrade(ctx, appDir, KitOptions{Version: "9.0.0-test"}, &out); err != nil {
		t.Fatalf("Upgrade: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"  rewrote repos/web/repo.config.yaml with the keys of this foreman-kit",
		"    required_services: was requires",
		"    setup: was gates.setup",
		"    visual_qa.prepare: was service.prepare",
		"    regression.commands: was regression",
		"    regression.when: was regression_when",
		"repo.config.yaml was rewritten in repos/api, repos/web. Each one is its own repository: commit the file there and push it to its base branch.",
		`  git -C repos/web commit -m "chore: repo.config.yaml with the keys of foreman-kit v9.0.0-test" repo.config.yaml && git -C repos/web push origin HEAD:main`,
		// A repository on no branch is told to switch to its base first.
		"  git -C repos/api switch trunk    (it is on no branch: foreman-kit sync would leave a commit made there behind)\n" +
			`  git -C repos/api commit -m "chore: repo.config.yaml with the keys of foreman-kit v9.0.0-test" repo.config.yaml && git -C repos/api push origin HEAD:trunk`,
		// Its regression ran its gates: they are written as its commands, and the person is told.
		"  rewrote repos/api/repo.config.yaml with the keys of this foreman-kit\n    setup: was gates.setup\n    regression.commands: REVIEW: it had no command of its own, so its regression ran the gates",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("update does not say %q:\n%s", want, out.String())
		}
	}
	// The umbrella commits what is its own, and nothing of a repository: the file of each one is
	// committed there.
	if names := git(appDir, "diff", "--name-only", firstCommit, "HEAD"); strings.Contains(names, "repos/") {
		t.Errorf("the umbrella committed something of a repository:\n%s", names)
	}
	if strings.Contains(out.String(), "repos/docs") {
		t.Errorf("update names a repository whose file is of today:\n%s", out.String())
	}

	written, err := os.ReadFile(filepath.Join(appDir, "repos", "web", config.RepoConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := os.ReadFile(filepath.Join("..", "config", "testdata", "former", "repos", "web.repo.config.migrated.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(migrated) {
		t.Fatalf("the file written in the repository is not the expected one:\n%s", written)
	}
	if api, _ := os.ReadFile(filepath.Join(appDir, "repos", "api", config.RepoConfigFile)); string(api) != "base_branch: trunk\nsetup: bin/setup\n\ngates:\n  test: bin/test\n\nregression:\n  commands:\n    test: bin/test\n" {
		t.Fatalf("the file written in repos/api:\n%s", api)
	}
	if strings.Contains(out.String(), "git -C repos/web switch") {
		t.Errorf("update tells a repository that is on a branch to switch:\n%s", out.String())
	}
	if docs, _ := os.ReadFile(filepath.Join(appDir, "repos", "docs", config.RepoConfigFile)); string(docs) != ofToday {
		t.Fatalf("the file of today was rewritten:\n%s", docs)
	}
	// The file is written in the repository, and committing it there is for a person: the commit
	// the command of the update makes is the whole of it.
	if status := strings.TrimSpace(git(filepath.Join(appDir, "repos", "web"), "status", "--porcelain")); status != "M repo.config.yaml\n?? notes.txt" && status != " M repo.config.yaml\n?? notes.txt" {
		t.Fatalf("the repository after the update:\n%s", status)
	}

	if got := config.RepoConfigsUnusable(appDir); got != "" {
		t.Fatalf("after the update: %q", got)
	}
	if err := RequireUsable(appDir, settings.CycleFiles...); err != nil {
		t.Fatalf("after the update the cycle still waits: %v", err)
	}
	if problems := config.RepoProblems(appDir, "repos/web"); len(problems.Errors) != 0 {
		t.Fatalf("config check of the rewritten file: %v", problems.Errors)
	}

	// Nothing is left to do, with the rewritten file still uncommitted in its repository.
	out.Reset()
	if err := Upgrade(ctx, appDir, KitOptions{Version: "9.0.0-test"}, &out); err != nil || !strings.Contains(out.String(), "nothing to do") {
		t.Fatalf("a second update: %v\n%s", err, out.String())
	}
}

// TestUpdateLeavesARepoConfigItCannotFollow: a repo.config.yaml written in a way the rewrite does
// not follow is left as it is, and the update says which keys to rename by hand. The cycle goes on
// waiting for it.
func TestUpdateLeavesARepoConfigItCannotFollow(t *testing.T) {
	ctx := context.Background()
	appDir := t.TempDir()
	if err := Init(ctx, appDir, KitOptions{Agents: "claude", Version: "9.0.0-test"}, io.Discard); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	dir := filepath.Join(appDir, "repos", "api")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	onOneLine := "base_branch: main\ngates: {setup: bin/setup, test: bin/test}\n"
	if err := os.WriteFile(filepath.Join(dir, config.RepoConfigFile), []byte(onOneLine), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Upgrade(ctx, appDir, KitOptions{Version: "9.0.0-test"}, &out); err != nil {
		t.Fatalf("Upgrade: %v\n%s", err, out.String())
	}
	if want := "  REVIEW: repos/api/repo.config.yaml could not be rewritten (gates is written on one line); rename its keys by hand: gates.setup is setup"; !strings.Contains(out.String(), want) {
		t.Fatalf("update does not say %q:\n%s", want, out.String())
	}
	if strings.Contains(out.String(), "was rewritten in") {
		t.Fatalf("update says a file was rewritten:\n%s", out.String())
	}
	if left, _ := os.ReadFile(filepath.Join(dir, config.RepoConfigFile)); string(left) != onOneLine {
		t.Fatalf("the file was changed:\n%s", left)
	}
	if err := RequireUsable(appDir, settings.CycleFiles...); err == nil {
		t.Fatal("the cycle would start on the file the update could not rewrite")
	}
}

// TestNothingStartsOnARepoConfigOfAnOlderKit: while the repo.config.yaml of a repository holds keys
// an older kit read, its gates would not run as it says. The cycle and the visual session start
// nothing and say which file and what rewrites it, and so does `foreman-kit status`.
func TestNothingStartsOnARepoConfigOfAnOlderKit(t *testing.T) {
	ctx := context.Background()
	root := bareApp(t)
	dir := filepath.Join(root, "repos", "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.RepoConfigFile), []byte("kind: web\nservice:\n  prepare: bin/prepare\ngates:\n  test: bin/test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	older := "repos/web/repo.config.yaml holds keys an older foreman-kit read (service.prepare): foreman-kit update rewrites it"

	judged := false
	var out bytes.Buffer
	err := RunCycle(ctx, root, openTestStore(t, root), CycleOptions{
		SkipWorktreePrep: true,
		AgentExecFunc: func(context.Context, string, string, string, time.Duration, io.Writer) error {
			judged = true
			return nil
		},
	}, &out)
	if err == nil || judged || !strings.Contains(out.String(), "cycle: "+older+": nothing new starts until it is mended") {
		t.Errorf("a cycle ran (judged: %v, err: %v):\n%s", judged, err, out.String())
	}

	out.Reset()
	if err := VisualQABegin(ctx, root, openTestStore(t, root), VisualQAOptions{SkipWorktreePrep: true}, &out); err == nil || !strings.Contains(out.String(), "visual-qa: "+older) {
		t.Errorf("a visual session began (err: %v):\n%s", err, out.String())
	}

	out.Reset()
	if err := Status(ctx, root, openTestStore(t, root), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "CONFIG: "+older+"; what reads that file starts nothing new until it is mended\n") {
		t.Errorf("status does not say it:\n%s", out.String())
	}
}
