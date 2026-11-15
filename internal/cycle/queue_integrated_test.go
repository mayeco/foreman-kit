package cycle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// newIntegratedQueueApp is a queue test application in a git repository whose base branch, master,
// archives each of onBase under openspec/changes/archive/<date>-<name>/. The working tree no longer
// holds those directories, so only the base says they are integrated; each of onDisk is archived in
// the working tree alone.
func newIntegratedQueueApp(t *testing.T, cycleYAML string, onBase, onDisk []string) queueApp {
	t.Helper()
	a := newQueueApp(t, cycleYAML)
	initGitRepo(t, a.root)
	for _, name := range onBase {
		a.write("openspec/changes/archive/2026-01-01-"+name+"/proposal.md", "archived by hand\n")
	}
	commitAll(t, a.root, "chore: integrate by hand")
	if err := os.RemoveAll(filepath.Join(a.root, "openspec", "changes", "archive")); err != nil {
		t.Fatal(err)
	}
	for _, name := range onDisk {
		a.write("openspec/changes/archive/2026-02-02-"+name+"/proposal.md", "archived here\n")
	}
	return a
}

func (a queueApp) backlog() []map[string]any {
	a.t.Helper()
	items, err := ReadJSONList(filepath.Join(a.root, filepath.FromSlash(BacklogFile)))
	if err != nil {
		a.t.Fatal(err)
	}
	return items
}

// A backlog item whose change is archived on the base (or in the checkout) is no open change, with
// or without a store, and it stays in the backlog.
func TestOpenChangesLeavesOutBacklogItemsArchivedOnTheBase(t *testing.T) {
	a := newIntegratedQueueApp(t, "", []string{"done-by-hand"}, []string{"done-on-disk"})
	a.queue("done-by-hand", "still-open", "done-on-disk")
	ctx := context.Background()

	if n, err := openChanges(ctx, nil, a.root, a.backlog()); err != nil || n != 1 {
		t.Errorf("openChanges without a store = %d, %v; want 1", n, err)
	}
	if n, err := openChanges(ctx, a.store(), a.root, a.backlog()); err != nil || n != 1 {
		t.Errorf("openChanges with a store = %d, %v; want 1", n, err)
	}
	if got, want := a.names(BacklogFile), []string{"done-by-hand", "still-open", "done-on-disk"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the backlog lists %v after counting, want %v", got, want)
	}
}

// Integrated backlog items do not make status, the daemon or progress say the queue is full, and
// status says how many could be taken out of the backlog.
func TestStatusDoesNotCountIntegratedBacklogItems(t *testing.T) {
	a := newIntegratedQueueApp(t, "queue:\n  max_open: 2\n", []string{"archived-one", "archived-two"}, nil)
	a.queue("archived-one", "archived-two", "open-one")
	ctx := context.Background()
	st := a.store()

	var statusOut, progressOut, errOut bytes.Buffer
	if err := Status(ctx, a.root, st, &statusOut); err != nil {
		t.Fatal(err)
	}
	status := statusOut.String()
	if !strings.Contains(status, "open_changes: 1 (queue.max_open: 2)\n") || strings.Contains(status, "QUEUE FULL") {
		t.Errorf("status counts integrated backlog items:\n%s", status)
	}
	if !strings.Contains(status, "backlog_integrated: 2 (integrated on the base and not counted as open; they can be taken out of "+BacklogFile+")\n") {
		t.Errorf("status does not say how many backlog items are integrated:\n%s", status)
	}
	if q, full := queueIsFull(ctx, a.root, st); full || q.Open != 1 {
		t.Errorf("queueIsFull = %+v, %v; want 1 open and not full", q, full)
	}
	if rc := RunProgressCLI(ctx, a.root, st, []string{"--since", "24h"}, &progressOut, &errOut); rc != 0 {
		t.Fatalf("progress failed: %s", errOut.String())
	}
	if progress := progressOut.String(); !strings.Contains(progress, "  open changes: 1 (queue.max_open: 2)\n") {
		t.Errorf("progress counts integrated backlog items:\n%s", progress)
	}
}

// The cycle's quota leaves integrated backlog items out: a backlog of them alone leaves the whole
// number of findings.
func TestQuotaLeavesOutIntegratedBacklogItems(t *testing.T) {
	a := newIntegratedQueueApp(t, "queue:\n  max_open: 3\n  max_findings: 3\n", []string{"archived-one", "archived-two"}, nil)
	a.queue("archived-one", "archived-two")
	var out bytes.Buffer
	quota := planQuota(context.Background(), a.root, a.store(), &out)
	if !quota.Counted || quota.Open != 0 || quota.Findings != quota.MaxFindings || quota.MaxFindings == 0 {
		t.Errorf("planQuota = %+v; want 0 open and all %d findings\n%s", quota, quota.MaxFindings, out.String())
	}
}
