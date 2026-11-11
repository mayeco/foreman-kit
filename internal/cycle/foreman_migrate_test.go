package cycle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// installedAsOpenspecKit turns a fresh installation into one made before the kit was called
// Foreman: its files in config/, its lock in .openspec-kit.lock, its skills named openspec-kit-*,
// and its guides, prompts and agents' commands naming openspec-kit. It commits the result.
func installedAsOpenspecKit(t *testing.T, ctx context.Context, app string) {
	t.Helper()
	if err := Init(ctx, app, KitOptions{Agents: "claude"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(app, KitLockFile), filepath.Join(app, store.LegacyLockFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(app, ForemanDir), filepath.Join(app, "config")); err != nil {
		t.Fatal(err)
	}
	old := func(text string) string {
		for _, pair := range [][2]string{
			{".foreman-kit/kit.lock", ".openspec-kit.lock"}, {"foreman-kit", "openspec-kit"},
			{".foreman-kit/", "config/"}, {"FOREMANKIT_", "OPENSPEC_"},
		} {
			text = strings.ReplaceAll(text, pair[0], pair[1])
		}
		return text
	}
	// The lock as it was: the seeds under config/, the skills under openspec-kit-*.
	raw := readText(t, filepath.Join(app, store.LegacyLockFile))
	writeText(t, filepath.Join(app, store.LegacyLockFile), old(raw))
	for _, dir := range SkillsDirs([]string{"claude"}) {
		entries, _ := os.ReadDir(filepath.Join(app, filepath.FromSlash(dir)))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "foreman-kit-") {
				from := filepath.Join(app, filepath.FromSlash(dir), e.Name())
				if err := os.Rename(from, filepath.Join(app, filepath.FromSlash(dir), "openspec-kit-"+strings.TrimPrefix(e.Name(), "foreman-kit-"))); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, rel := range []string{"AGENTS.md", "README.md", "config/agents.yaml", "config/prompts/build.md"} {
		full := filepath.Join(app, filepath.FromSlash(rel))
		writeText(t, full, old(readText(t, full)))
	}
	initGitRepo(t, app)
	commitAll(t, app, "an application of openspec-kit")
}

func writeText(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestUpdateMovesAnApplicationOfOpenspecKitIntoForeman: update moves the kit's files from config/ and
// .openspec-kit.lock into .foreman-kit/ with git, so their history follows; installs the foreman-kit-* skills
// in place of the openspec-kit-* ones; and rewrites the former names where the application's own
// files use them, so its agents run foreman-kit. Nothing is left uncommitted, and a second update has
// nothing to do.
func TestUpdateMovesAnApplicationOfOpenspecKitIntoForeman(t *testing.T) {
	t.Setenv("OPENSPEC_FORCE_BUILTIN", "1")
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "1")
	ctx := context.Background()
	app := filepath.Join(t.TempDir(), "app")
	installedAsOpenspecKit(t, ctx, app)
	if !foremanLayoutPending(app) {
		t.Fatal("the application of openspec-kit is not seen as one")
	}

	var out bytes.Buffer
	if err := Upgrade(ctx, app, KitOptions{}, &out); err != nil {
		t.Fatalf("Upgrade: %v\n%s", err, out.String())
	}
	said := out.String()
	for _, want := range []string{"openspec-kit is Foreman now", "moved the kit's files from config/ and .openspec-kit.lock into .foreman-kit/"} {
		if !strings.Contains(said, want) {
			t.Errorf("update does not say %q:\n%s", want, said)
		}
	}
	for _, gone := range []string{"config", store.LegacyLockFile, ".claude/skills/openspec-kit-cycle", ".agents/skills/openspec-kit-cycle"} {
		if exists(filepath.Join(app, filepath.FromSlash(gone))) {
			t.Errorf("%s is still there", gone)
		}
	}
	for _, there := range []string{KitLockFile, ".foreman-kit/pipeline.yaml", ".foreman-kit/agent_backlog.json", ".foreman-kit/prompts/build.md", ".agents/skills/foreman-kit-cycle/SKILL.md", ".claude/skills/foreman-kit-cycle/SKILL.md"} {
		if !exists(filepath.Join(app, filepath.FromSlash(there))) {
			t.Errorf("%s is missing", there)
		}
	}
	for _, rel := range []string{"AGENTS.md", "README.md", ".foreman-kit/agents.yaml", ".foreman-kit/prompts/build.md"} {
		text := readText(t, filepath.Join(app, filepath.FromSlash(rel)))
		if strings.Contains(text, "openspec-kit") || strings.Contains(text, "OPENSPEC_") {
			t.Errorf("%s still names openspec-kit", rel)
		}
	}
	if !strings.Contains(readText(t, filepath.Join(app, ".foreman-kit", "agents.yaml")), "Bash(foreman-kit gates *)") {
		t.Error("the restricted agents may not run foreman-kit gates")
	}
	if dirty := gitRun(t, app, "status", "--porcelain"); dirty != "" {
		t.Errorf("the update left changes uncommitted:\n%s", dirty)
	}
	// git followed the move: the backlog's history reaches the commit of openspec-kit.
	if log := gitRun(t, app, "log", "--follow", "--format=%s", "--", ".foreman-kit/agent_backlog.json"); !strings.Contains(log, "an application of openspec-kit") {
		t.Errorf("the history of the backlog does not follow the move:\n%s", log)
	}
	out.Reset()
	if err := Upgrade(ctx, app, KitOptions{}, &out); err != nil || !strings.Contains(out.String(), "nothing to do") {
		t.Errorf("a second update: %v\n%s", err, out.String())
	}
}
