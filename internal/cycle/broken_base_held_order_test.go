package cycle

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// TestBrokenBasesListsTheHeldChangesByName: the changes a broken base holds were listed in the merge queue
// order, which is the second each was enqueued and only then the name. Two changes built by parallel
// workers land in either order, so the line foreman-kit status prints for the same held changes
// differed from one run to the next (and a test that expected them by name failed now and then).
func TestBrokenBasesListsTheHeldChangesByName(t *testing.T) {
	ctx := context.Background()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": empty, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "protocol.file.allow", "GIT_CONFIG_VALUE_0": "always",
	} {
		t.Setenv(k, v)
	}

	base := t.TempDir()
	coreOrigin := filepath.Join(base, "core-origin")
	root := filepath.Join(base, "umbrella")
	for _, dir := range []string{coreOrigin, root} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, coreOrigin, "init", "-q", "-b", "main")
	var coreCommits []string
	for _, msg := range []string{"core: base", "core: zulu's work", "core: alpha's work"} {
		gitIn(t, coreOrigin, "commit", "-q", "--allow-empty", "-m", msg)
		coreCommits = append(coreCommits, strings.TrimSpace(gitIn(t, coreOrigin, "rev-parse", "HEAD")))
	}

	// The umbrella records core's first commit; each change's branch records one of the later ones.
	gitIn(t, root, "init", "-q", "-b", "master")
	gitIn(t, root, "submodule", "--quiet", "add", coreOrigin, "repos/core")
	core := filepath.Join(root, "repos", "core")
	record := func(coreCommit, message string) string {
		t.Helper()
		gitIn(t, core, "checkout", "-q", coreCommit)
		gitIn(t, root, "add", "repos/core")
		gitIn(t, root, "commit", "-q", "-m", message)
		return strings.TrimSpace(gitIn(t, root, "rev-parse", "HEAD"))
	}
	record(coreCommits[0], "umbrella: add repos/core")
	heads := map[string]string{}
	for i, change := range []string{"zulu", "alpha"} {
		gitIn(t, root, "checkout", "-q", "-b", "feat/spec-"+change, "master")
		heads[change] = record(coreCommits[i+1], "feat(spec): implement "+change)
	}
	gitIn(t, root, "checkout", "-q", "master")

	st, err := store.OpenAt(filepath.Join(t.TempDir(), "state.db"), "ws-held-order", root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// zulu was enqueued first: the merge queue lists it before alpha.
	enqueued := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, change := range []string{"zulu", "alpha"} {
		if err := st.EnqueueReady(ctx, store.ReadyManifest{
			ChangeName: change,
			Branch:     "feat/spec-" + change,
			HeadSHA:    heads[change],
			EnqueuedAt: enqueued.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if ready, _ := st.ListReadyQueue(ctx); len(ready) != 2 || ready[0].ChangeName != "zulu" {
		t.Fatalf("the merge queue = %+v, want zulu first", ready)
	}
	if _, err := st.RecordBaseVerification(ctx, store.BaseVerification{
		RepoPath: "repos/core", BaseSHA: coreCommits[2], CheckName: "regression",
		Status: store.BaseVerdictFailed, ExitCode: 1, OutsideCommits: 1, CreatedAt: enqueued.Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	bases := BrokenBases(ctx, root, st)
	if len(bases) != 1 {
		t.Fatalf("broken bases = %+v, want repos/core", bases)
	}
	if want := []string{"alpha", "zulu"}; !reflect.DeepEqual(bases[0].Held, want) {
		t.Fatalf("held = %v, want them by name %v whatever the order they were enqueued in", bases[0].Held, want)
	}
	if line := bases[0].StatusLine(); !strings.Contains(line, "holding: alpha, zulu;") {
		t.Fatalf("status line = %q", line)
	}
}
