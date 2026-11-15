package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// abandonRun runs `foreman-kit abandon change appDir` and returns its exit code and what it printed.
func abandonRun(t *testing.T, appDir, change string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := Run(context.Background(), []string{"abandon", change, appDir}, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// abandonQueue lists the changes in the backlog file of the application.
func abandonQueue(t *testing.T, appDir string, changes ...string) {
	t.Helper()
	var items []map[string]any
	for _, c := range changes {
		items = append(items, map[string]any{"change_name": c, "priority": "normal", "description": "test change " + c})
	}
	if err := cycle.WriteJSONList(filepath.Join(appDir, cycle.BacklogFile), items); err != nil {
		t.Fatal(err)
	}
}

func abandonInBacklog(t *testing.T, appDir, change string) bool {
	t.Helper()
	items, err := cycle.ReadJSONList(filepath.Join(appDir, cycle.BacklogFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if strings.TrimSpace(asString(it["change_name"])) == change {
			return true
		}
	}
	return false
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// withAbandonStore opens the registry of the application for fn and closes it before the command runs.
func withAbandonStore(t *testing.T, appDir string, fn func(ctx context.Context, st *store.Store)) {
	t.Helper()
	st, err := store.Open(appDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fn(context.Background(), st)
}

// TestAbandonOfAnUnknownNameFailsAndWritesNothing: a name the kit has never met is refused with a
// non-zero exit, and the registry does not get a discarded row for it that would hide the change
// when it is queued later.
func TestAbandonOfAnUnknownNameFailsAndWritesNothing(t *testing.T) {
	appDir := needsHumanPathWorkspace(t)
	code, out, errOut := abandonRun(t, appDir, "never-queued-change")
	if code == 0 {
		t.Fatalf("abandon of an unknown name exits 0: %s %s", out, errOut)
	}
	if !strings.Contains(errOut, "the kit knows no change named never-queued-change") || strings.Contains(out, "abandoned:") {
		t.Errorf("abandon of an unknown name said %q on stdout and %q on stderr", out, errOut)
	}
	withAbandonStore(t, appDir, func(ctx context.Context, st *store.Store) {
		if state, err := st.ChangeState(ctx, "never-queued-change"); err != nil || state != "" {
			t.Errorf("a changes row was written for an unknown name: state %q, %v", state, err)
		}
	})

	// Queued afterwards, the change is open as any other, not discarded.
	abandonQueue(t, appDir, "never-queued-change")
	withAbandonStore(t, appDir, func(ctx context.Context, st *store.Store) {
		if err := st.SyncJSONToSQLite(ctx, appDir); err != nil {
			t.Fatal(err)
		}
		if state, err := st.ChangeState(ctx, "never-queued-change"); err != nil || state != store.ChangeQueued {
			t.Errorf("the change queued after the refused abandon is %q (%v), want queued", state, err)
		}
	})
}

// TestAbandonOfAChangeAWorkerBuildsOrTheMergerIntegratesIsRefused: an open change run or an open
// merge attempt keeps the abandon from writing anything: the manifest stays open, the change is not
// discarded and the backlog still lists it.
func TestAbandonOfAChangeAWorkerBuildsOrTheMergerIntegratesIsRefused(t *testing.T) {
	appDir := needsHumanPathWorkspace(t)
	abandonQueue(t, appDir, "builds", "merges")
	withAbandonStore(t, appDir, func(ctx context.Context, st *store.Store) {
		if err := st.SyncJSONToSQLite(ctx, appDir); err != nil {
			t.Fatal(err)
		}
		if _, err := st.BeginChangeRun(ctx, "builds", 1, "/wt/builds", "feat/spec-builds"); err != nil {
			t.Fatal(err)
		}
		if err := st.EnqueueReady(ctx, store.ReadyManifest{ChangeName: "merges", Branch: "feat/spec-merges", Worktree: "/wt/merges", HeadSHA: "abc123"}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.BeginMergeAttempt(ctx, "merges"); err != nil {
			t.Fatal(err)
		}
	})

	for change, want := range map[string]string{
		"builds": "a worker is building builds: it can be abandoned once its run ends",
		"merges": "the merger is integrating merges: it can be abandoned once that attempt ends",
	} {
		code, out, errOut := abandonRun(t, appDir, change)
		if code == 0 || !strings.Contains(errOut, want) || strings.Contains(out, "abandoned:") {
			t.Errorf("abandon %s = %d, stdout %q, stderr %q; want a refusal that says %q", change, code, out, errOut, want)
		}
		if !abandonInBacklog(t, appDir, change) {
			t.Errorf("the refused abandon of %s took it out of the backlog", change)
		}
	}

	withAbandonStore(t, appDir, func(ctx context.Context, st *store.Store) {
		for change, want := range map[string]string{"builds": store.ChangeBuilding, "merges": store.ChangeMerging} {
			if state, err := st.ChangeState(ctx, change); err != nil || state != want {
				t.Errorf("%s is %q (%v) after the refused abandon, want %q", change, state, err, want)
			}
		}
		open, err := st.OpenMergeAttempt(ctx)
		if err != nil || open == nil || open.ChangeName != "merges" {
			t.Errorf("the merge attempt of merges is no longer open: %+v, %v", open, err)
		}
	})
}

// TestAbandonOfAChangeThatIsDoneIsRefused: an integrated change has its commits on the base
// branches, and a discarded one is abandoned already; both are refused with a non-zero exit.
func TestAbandonOfAChangeThatIsDoneIsRefused(t *testing.T) {
	appDir := needsHumanPathWorkspace(t)
	withAbandonStore(t, appDir, func(ctx context.Context, st *store.Store) {
		if err := st.MarkChangeIntegrated(ctx, "lands", "abc123"); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkChangeDiscarded(ctx, "goes"); err != nil {
			t.Fatal(err)
		}
	})
	for change, want := range map[string]string{
		"lands": "lands is integrated: its commits are on the base branches",
		"goes":  "goes is already discarded",
	} {
		if code, out, errOut := abandonRun(t, appDir, change); code == 0 || !strings.Contains(errOut, want) {
			t.Errorf("abandon %s = %d, stdout %q, stderr %q; want a refusal that says %q", change, code, out, errOut, want)
		}
	}
	withAbandonStore(t, appDir, func(ctx context.Context, st *store.Store) {
		if state, _ := st.ChangeState(ctx, "lands"); state != store.ChangeIntegrated {
			t.Errorf("the refused abandon changed lands to %q", state)
		}
	})
}

// TestAbandonStillTakesAQueuedOrBlockedChangeOutOfTheBacklog: a change that waits, or that is
// blocked, with nothing running on it, leaves the backlog and is discarded, as before; so does a
// change that is queued in the backlog file and that no pass has mirrored into the registry yet.
func TestAbandonStillTakesAQueuedOrBlockedChangeOutOfTheBacklog(t *testing.T) {
	appDir := needsHumanPathWorkspace(t)
	abandonQueue(t, appDir, "waits", "is-blocked", "ready-one", "unmirrored", "stays")
	withAbandonStore(t, appDir, func(ctx context.Context, st *store.Store) {
		if err := st.SyncJSONToSQLite(ctx, appDir); err != nil {
			t.Fatal(err)
		}
		if err := st.BlockChange(ctx, store.QuarantineRecord{ChangeName: "is-blocked", Reason: "gate_failure"}); err != nil {
			t.Fatal(err)
		}
		if err := st.EnqueueReady(ctx, store.ReadyManifest{ChangeName: "ready-one", Branch: "feat/spec-ready-one", Worktree: "/wt/ready-one", HeadSHA: "abc123"}); err != nil {
			t.Fatal(err)
		}
	})
	// A change listed in the file after the last mirror.
	abandonQueue(t, appDir, "waits", "is-blocked", "ready-one", "unmirrored", "stays", "just-added")

	for _, change := range []string{"waits", "is-blocked", "ready-one", "just-added"} {
		if code, out, errOut := abandonRun(t, appDir, change); code != 0 || !strings.Contains(out, "abandoned: "+change) {
			t.Fatalf("abandon %s = %d, stdout %q, stderr %q", change, code, out, errOut)
		}
		if abandonInBacklog(t, appDir, change) {
			t.Errorf("%s is still in the backlog after it was abandoned", change)
		}
	}
	if !abandonInBacklog(t, appDir, "stays") || !abandonInBacklog(t, appDir, "unmirrored") {
		t.Errorf("abandoning four changes took another one out of the backlog")
	}
	withAbandonStore(t, appDir, func(ctx context.Context, st *store.Store) {
		for _, change := range []string{"waits", "is-blocked", "ready-one", "just-added"} {
			if state, err := st.ChangeState(ctx, change); err != nil || state != store.ChangeDiscarded {
				t.Errorf("%s is %q (%v), want discarded", change, state, err)
			}
		}
		if state, _ := st.ChangeState(ctx, "stays"); state != store.ChangeQueued {
			t.Errorf("stays is %q, want queued", state)
		}
	})
}
