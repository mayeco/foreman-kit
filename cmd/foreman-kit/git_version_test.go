package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

func setupGitVersionCLITestApp(t *testing.T) (string, *store.Store, string) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	root := filepath.Join(tmp, "app")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREMANKIT_STATE_DIR", stateDir)
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_APP_ROOT", root)

	for _, args := range [][]string{
		{"init", "-b", "master"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
	} {
		if _, err := gitx.RunGit(ctx, root, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	settingstest.WriteFiles(t, root, nil)
	settingstest.Give(t, settings.BaseBranch, "master")
	settingstest.Give(t, settings.EvaluatorEnabled, "false")

	if err := config.Write(root, []string{"claude", "hermes"}, map[string][]string{
		"plan":  {"claude", "hermes"},
		"build": {"claude", "hermes"},
		"cycle": {"claude"},
	}, "in_order"); err != nil {
		t.Fatal(err)
	}

	if _, err := memory.Init(ctx, root, "demo"); err != nil {
		t.Fatal(err)
	}

	skills, err := assets.WalkEmbedded("skills")
	if err != nil {
		t.Fatal(err)
	}
	var firstSkillRel string
	var firstSkillBytes []byte
	for k, v := range skills {
		firstSkillRel = ".agents/skills/" + k
		firstSkillBytes = v
		break
	}
	skillDiskPath := filepath.Join(root, filepath.FromSlash(firstSkillRel))
	if err := os.MkdirAll(filepath.Dir(skillDiskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillDiskPath, firstSkillBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	lockData := map[string]any{
		"version": assets.Version,
		"agents":  []string{"claude", "hermes"},
		"files": map[string]any{
			firstSkillRel: map[string]any{
				"class":  "kit",
				"sha256": assets.SHA256Bytes(firstSkillBytes),
			},
		},
	}
	lockBytes, _ := json.MarshalIndent(lockData, "", "  ")
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "kit.lock"), lockBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	backlog := []map[string]any{
		{
			"change_name": "queued-item",
			"priority":    "high",
			"description": "Must stay queued when git check fails",
		},
	}
	if err := cycle.WriteJSONList(filepath.Join(root, cycle.BacklogFile), backlog); err != nil {
		t.Fatal(err)
	}

	_, _ = gitx.RunGit(ctx, root, "add", "-A")
	_, _ = gitx.RunGit(ctx, root, "commit", "-m", "initial")

	toolsDir := filepath.Join(tmp, "tools")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"openspec", "claude", "hermes"} {
		script := "#!/bin/sh\nprintf '%s 1.0.0 pong\\n' '" + name + "'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(toolsDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return root, st, toolsDir
}

func installFakeGitOnPath(t *testing.T, basePath, scriptBody string) (fakeGitPath, gitCallLog string) {
	t.Helper()
	binDir := t.TempDir()
	fakeGitPath = filepath.Join(binDir, "git")
	gitCallLog = filepath.Join(binDir, "git-calls.log")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"--version\" ]; then\n" +
		"  printf '%s\\n' \"$*\" >> '" + gitCallLog + "'\n" +
		"fi\n" +
		scriptBody + "\n"
	if err := os.WriteFile(fakeGitPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+basePath)
	return fakeGitPath, gitCallLog
}

func assertNoWorktreeAndItemStillQueued(t *testing.T, ctx context.Context, root string, st *store.Store, gitCallLog string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".worktrees")); !os.IsNotExist(err) {
		t.Fatalf("expected no .worktrees directory to be created, stat err=%v", err)
	}
	if calls, err := os.ReadFile(gitCallLog); err == nil && strings.TrimSpace(string(calls)) != "" {
		t.Fatalf("expected no git subcommands other than --version to run, got:\n%s", string(calls))
	}
	items, err := cycle.ReadJSONList(filepath.Join(root, cycle.BacklogFile))
	if err != nil || len(items) != 1 || fmt.Sprint(items[0]["change_name"]) != "queued-item" {
		t.Fatalf("expected queued-item to remain in %s, got %+v (err=%v)", cycle.BacklogFile, items, err)
	}
	if blocked, _ := st.ListQuarantine(ctx, ""); len(blocked) != 0 {
		t.Fatalf("expected no quarantined changes, got %+v", blocked)
	}
	if ready, _ := st.ListReadyQueue(ctx); len(ready) != 0 {
		t.Fatalf("expected ready queue to remain empty, got %+v", ready)
	}
	if dl, _ := st.GetLock(ctx, "daemon"); dl != nil {
		t.Fatalf("expected no daemon lock to be held, got %+v", dl)
	}
}

func TestCLICommandsFailOnGit2166(t *testing.T) {
	ctx := context.Background()
	root, st, toolsDir := setupGitVersionCLITestApp(t)
	basePath := toolsDir + string(os.PathListSeparator) + os.Getenv("PATH")

	fakeGit, gitCallLog := installFakeGitOnPath(t, basePath, "printf 'git version 2.16.6\\n'\nexit 0")

	// 1. foreman-kit preflight
	var out, errBuf bytes.Buffer
	if code := Run(ctx, []string{"preflight"}, &out, &errBuf); code == 0 {
		t.Fatalf("expected preflight to exit non-zero on git 2.16.6, got 0:\n%s\n%s", out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	wantToolchain := fmt.Sprintf("  git: 2.16.6 (minimum %s) (%s)", gitx.MinVersion, fakeGit)
	if !strings.Contains(combined, wantToolchain) {
		t.Fatalf("expected preflight toolchain line %q, got:\n%s", wantToolchain, combined)
	}
	for _, want := range []string{"2.16.6", gitx.MinVersion, fakeGit} {
		if !strings.Contains(combined, want) {
			t.Fatalf("preflight output missing %q:\n%s", want, combined)
		}
	}

	// 2. foreman-kit dispatch, merge, cycle, -d, and foreman-kit alone (foreground daemon)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "dispatch", args: []string{"dispatch"}},
		{name: "merge", args: []string{"merge"}},
		{name: "cycle", args: []string{"cycle"}},
		{name: "-d", args: []string{"-d"}},
		{name: "foreground-daemon", args: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(ctx, tc.args, &stdout, &stderr)
			output := stdout.String() + stderr.String()
			if code == 0 {
				t.Fatalf("expected foreman-kit %v to exit non-zero on git 2.16.6, got 0:\n%s", tc.args, output)
			}
			for _, want := range []string{"2.16.6", gitx.MinVersion, fakeGit} {
				if !strings.Contains(output, want) {
					t.Fatalf("foreman-kit %v output missing %q:\n%s", tc.args, want, output)
				}
			}
			assertNoWorktreeAndItemStillQueued(t, ctx, root, st, gitCallLog)
		})
	}
}

func TestCLICommandsFailOnUnreadableGitVersion(t *testing.T) {
	ctx := context.Background()
	root, st, toolsDir := setupGitVersionCLITestApp(t)
	basePath := toolsDir + string(os.PathListSeparator) + os.Getenv("PATH")

	for _, verOut := range []struct {
		name   string
		script string
	}{
		{name: "empty-output", script: "exit 0"},
		{name: "no-version-line", script: "printf 'custom wrapper without version\\n'\nexit 0"},
	} {
		t.Run(verOut.name, func(t *testing.T) {
			fakeGit, gitCallLog := installFakeGitOnPath(t, basePath, verOut.script)

			for _, cmdArgs := range [][]string{
				{"preflight"},
				{"dispatch"},
				{"merge"},
				{"cycle"},
				{"-d"},
			} {
				var stdout, stderr bytes.Buffer
				code := Run(ctx, cmdArgs, &stdout, &stderr)
				output := stdout.String() + stderr.String()
				if code == 0 {
					t.Fatalf("expected foreman-kit %v to exit non-zero when git version cannot be read, got 0:\n%s", cmdArgs, output)
				}
				for _, want := range []string{"cannot be read", gitx.MinVersion, fakeGit} {
					if !strings.Contains(output, want) {
						t.Fatalf("foreman-kit %v output missing %q:\n%s", cmdArgs, want, output)
					}
				}
				assertNoWorktreeAndItemStillQueued(t, ctx, root, st, gitCallLog)
			}
		})
	}
}

func TestSupportedGitVersionsPassCheckAndPreflight(t *testing.T) {
	ctx := context.Background()
	_, _, toolsDir := setupGitVersionCLITestApp(t)
	basePath := toolsDir + string(os.PathListSeparator) + os.Getenv("PATH")

	for _, tc := range []struct {
		raw     string
		wantVer string
	}{
		{raw: "2.17.0", wantVer: "2.17.0"},
		{raw: "2.39.5 (Apple Git-154)", wantVer: "2.39.5"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			fakeGit, _ := installFakeGitOnPath(t, basePath, "printf 'git version %s\\n' '"+tc.raw+"'\nexit 0")

			if err := gitx.CheckVersion(ctx); err != nil {
				t.Fatalf("expected CheckVersion to pass for %q, got: %v", tc.raw, err)
			}

			var stdout, stderr bytes.Buffer
			code := Run(ctx, []string{"preflight"}, &stdout, &stderr)
			output := stdout.String() + stderr.String()
			if code != 0 {
				t.Fatalf("expected preflight to pass for %q (exit=%d):\n%s", tc.raw, code, output)
			}
			wantLine := fmt.Sprintf("  git: %s (minimum %s) (%s)", tc.wantVer, gitx.MinVersion, fakeGit)
			if !strings.Contains(output, wantLine) {
				t.Fatalf("expected preflight output to contain %q, got:\n%s", wantLine, output)
			}
		})
	}
}
