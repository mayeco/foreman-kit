package cycle

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/assets"
)

// TestUpdateInstallsAChangedSkillAtTheSameVersion: the kit's skills changed while its version stayed
// (as it does between two builds of one release), and update said "nothing to do", because it
// compared the seeds alone. The skill an application holds was the older one, and its agents followed
// it. Update now installs every kit file that differs from what the lock recorded.
func TestUpdateInstallsAChangedSkillAtTheSameVersion(t *testing.T) {
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "1")
	ctx := context.Background()
	app := filepath.Join(t.TempDir(), "app")
	if err := Init(ctx, app, KitOptions{Agents: "claude"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	// The application holds the skill as an earlier build of this version shipped it.
	const rel = ".agents/skills/foreman-kit-status/SKILL.md"
	older := []byte("# the status skill of an earlier build\n")
	if err := os.WriteFile(filepath.Join(app, filepath.FromSlash(rel)), older, 0o644); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(app, KitLockFile)
	var lock KitLock
	if err := json.Unmarshal([]byte(readText(t, lockPath)), &lock); err != nil {
		t.Fatal(err)
	}
	entry := lock.Files[rel]
	entry.SHA256 = assets.SHA256Bytes(older)
	lock.Files[rel] = entry
	raw, _ := json.MarshalIndent(lock, "", "  ")
	if err := os.WriteFile(lockPath, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, app)
	commitAll(t, app, "an application of an earlier build")

	var out bytes.Buffer
	if err := Upgrade(ctx, app, KitOptions{}, &out); err != nil {
		t.Fatalf("Upgrade: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "nothing to do") {
		t.Fatalf("update left the older skill in place:\n%s", out.String())
	}
	today, err := assets.FS.ReadFile("skills/foreman-kit-status/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := readText(t, filepath.Join(app, filepath.FromSlash(rel))); got != string(today) {
		t.Errorf("%s is not the skill of this build after the update", rel)
	}
	if dirty := gitRun(t, app, "status", "--porcelain"); dirty != "" {
		t.Errorf("the update left changes uncommitted:\n%s", dirty)
	}
	if !strings.Contains(gitRun(t, app, "log", "-1", "--format=%s"), "upgrade the OpenSpec kit") {
		t.Errorf("the update's commit is %q", gitRun(t, app, "log", "-1", "--format=%s"))
	}
	out.Reset()
	if err := Upgrade(ctx, app, KitOptions{}, &out); err != nil || !strings.Contains(out.String(), "nothing to do") {
		t.Errorf("a second update: %v\n%s", err, out.String())
	}
}

func TestUpdateInstallsScratchReproductionRule(t *testing.T) {
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "1")
	ctx := context.Background()
	app := filepath.Join(t.TempDir(), "app")
	if err := Init(ctx, app, KitOptions{Agents: "claude"}, io.Discard); err != nil {
		t.Fatal(err)
	}

	olderFiles := map[string][]byte{
		".agents/skills/foreman-kit-qa/SKILL.md":                         []byte("# OpenSpec QA\n\n## Rules\n\n- Reproduce a defect twice, from a clean start, before recording it.\n"),
		".claude/skills/foreman-kit-qa/SKILL.md":                         []byte("# OpenSpec QA\n\n## Rules\n\n- Reproduce a defect twice, from a clean start, before recording it.\n"),
		".agents/skills/foreman-kit-cycle/references/report.template.md": []byte("# Cycle\n\n## Needs a human\n\n<nothing>\n\n## Commands executed\n"),
		".claude/skills/foreman-kit-cycle/references/report.template.md": []byte("# Cycle\n\n## Needs a human\n\n<nothing>\n\n## Commands executed\n"),
	}

	lockPath := filepath.Join(app, KitLockFile)
	var lock KitLock
	if err := json.Unmarshal([]byte(readText(t, lockPath)), &lock); err != nil {
		t.Fatal(err)
	}
	for rel, content := range olderFiles {
		if err := os.WriteFile(filepath.Join(app, filepath.FromSlash(rel)), content, 0o644); err != nil {
			t.Fatal(err)
		}
		if entry, ok := lock.Files[rel]; ok {
			entry.SHA256 = assets.SHA256Bytes(content)
			lock.Files[rel] = entry
		}
	}
	raw, _ := json.MarshalIndent(lock, "", "  ")
	if err := os.WriteFile(lockPath, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, app)
	commitAll(t, app, "an application before the scratch-instance rule")

	var out bytes.Buffer
	if err := Upgrade(ctx, app, KitOptions{}, &out); err != nil {
		t.Fatalf("Upgrade: %v\n%s", err, out.String())
	}

	for _, rel := range []string{
		".agents/skills/foreman-kit-qa/SKILL.md",
		".claude/skills/foreman-kit-qa/SKILL.md",
	} {
		got := readText(t, filepath.Join(app, filepath.FromSlash(rel)))
		for _, want := range []string{
			"disposable instance",
			"FOREMANKIT_STATE_DIR",
			"FOREMANKIT_DB_PATH",
			"foreman-kit sim",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s after update missing %q", rel, want)
			}
		}
	}

	for _, rel := range []string{
		".agents/skills/foreman-kit-cycle/references/report.template.md",
		".claude/skills/foreman-kit-cycle/references/report.template.md",
	} {
		got := readText(t, filepath.Join(app, filepath.FromSlash(rel)))
		if !strings.Contains(got, "## Scratch instances") {
			t.Errorf("%s after update missing ## Scratch instances", rel)
		}
	}
}
