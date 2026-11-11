package main

import (
	"bytes"
	"context"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"os"
	"path/filepath"
	"testing"
)

// needsHumanPathWorkspace initializes an application and returns its directory.
func needsHumanPathWorkspace(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	appDir := filepath.Join(tmp, "sample-app")
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(tmp, "state"))
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_APP_ROOT", "")
	var out, errBuf bytes.Buffer
	if code := Run(context.Background(), []string{"init", appDir, "--agents", "claude"}, &out, &errBuf); code != 0 {
		t.Fatalf("init failed (%d): %s %s", code, out.String(), errBuf.String())
	}
	return appDir
}

// needsHumanPathPlant writes the question a worker left for a human in change's worktree under base.
func needsHumanPathPlant(t *testing.T, base, change string) string {
	t.Helper()
	file := filepath.Join(base, "worker-"+change, "openspec", "changes", change, ".needs-human")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("Which currency does the report use?\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

// TestUnblockApproveAbandonRemoveNeedsHumanWhereTheDispatcherReadsIt keeps the three commands that
// answer a blocked change in step with the dispatcher, which blocks the change again for as long as
// the worker's .needs-human file exists. The file lives under repository.worktrees_dir when a run is given one;
// the commands looked only under <app>/.worktrees and left it behind.
func TestUnblockApproveAbandonRemoveNeedsHumanWhereTheDispatcherReadsIt(t *testing.T) {
	appDir := needsHumanPathWorkspace(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		base func() string // gives the run its repository.worktrees_dir and returns the directory it names
	}{
		{"default base", func() string {
			return filepath.Join(appDir, ".worktrees")
		}},
		{"absolute base", func() string {
			base := filepath.Join(t.TempDir(), "trees")
			settingstest.Give(t, settings.WorktreesDir, base)
			return base
		}},
		{"base relative to the application", func() string {
			settingstest.Give(t, settings.WorktreesDir, "build/trees")
			return filepath.Join(appDir, "build", "trees")
		}},
	} {
		base := tc.base()
		for _, command := range []string{"unblock", "approve", "abandon"} {
			change := "report-currency"
			file := needsHumanPathPlant(t, base, change)
			var out, errBuf bytes.Buffer
			if code := Run(ctx, []string{command, change, appDir}, &out, &errBuf); code != 0 {
				t.Fatalf("%s (%s) failed (%d): %s %s", command, tc.name, code, out.String(), errBuf.String())
			}
			if _, err := os.Stat(file); err == nil {
				t.Errorf("%s (%s) left %s behind: the dispatcher blocks the change again on it", command, tc.name, file)
			}
		}
	}
}
