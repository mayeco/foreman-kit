package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mayeco/foreman-kit/internal/gitx"
)

// A check of the base alone never sees what the change's gates left in ignored files (installed
// dependencies, build output): core's test gate builds build/CHANGE_ONLY when the change's feature.txt
// is there, and fails when that artifact meets the hotfix's notes.txt. On the base alone it passes, so
// the change is quarantined and the base is not broken.
func TestBaseCheckDoesNotSeeTheChangesIgnoredFiles(t *testing.T) {
	cond := `{ [ -e feature.txt ] && mkdir -p build && : > build/CHANGE_ONLY; true; } && [ -e build/CHANGE_ONLY ] && [ -e notes.txt ]`
	sb := newMCFSandboxWith(t, mcfOptions{coreTestFailsWhen: cond})
	sb.pushCoreChanges(t, map[string]string{".gitignore": "build/\n"}, nil, "chore(core): ignore build output")
	sb.buildCoreChange(t, "core-build")
	sb.pushCoreCommit(t, "notes.txt", "notes\n", "docs(core): notes")

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	if evs := sb.events(t, "merge.base_broken"); len(evs) != 0 {
		t.Fatalf("the change's ignored build output made the base look broken: %+v", evs)
	}
	if q, _ := sb.st.GetQuarantine(context.Background(), "core-build"); q == nil || q.Reason != "gate_failure" {
		t.Fatalf("expected core-build quarantined as gate_failure, got %+v", q)
	}
}

// The clean around a check of a base alone removes ignored build output but keeps the local config
// files the gates need (localChildConfigs), which may have been placed by hand.
func TestBaseCheckCleanKeepsLocalConfigFiles(t *testing.T) {
	dir := t.TempDir()
	mustGit(t, context.Background(), dir, "init", "-q")
	mustWriteFile(t, filepath.Join(dir, ".gitignore"), "build/\n.env.local\nconfig/application.yml\n")
	for _, rel := range []string{"build/out.o", ".env.local", "config/application.yml"} {
		mustWriteFile(t, filepath.Join(dir, filepath.FromSlash(rel)), "x\n")
	}
	if _, err := gitx.RunGit(context.Background(), dir, cleanIgnoredArgs()...); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "build", "out.o")); err == nil {
		t.Fatal("ignored build output survived the clean")
	}
	for _, rel := range localChildConfigs {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("the clean deleted %s: %v", rel, err)
		}
	}
}
