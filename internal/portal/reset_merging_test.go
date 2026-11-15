package portal

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// mergingChange queues add-cart in the merge queue with a worktree of its own under root and opens
// a merge attempt on it, as the merger does while it integrates the change. It returns the worktree
// and the attempt.
func mergingChange(t *testing.T, st *store.Store, root string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	wt := filepath.Join(root, ".worktrees", "worker-add-cart")
	if err := os.MkdirAll(filepath.Join(wt, "repos", "shop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "repos", "shop", "cart.go"), []byte("package shop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueReady(ctx, store.ReadyManifest{ChangeName: "add-cart", Branch: "feat/spec-add-cart", Worktree: wt, HeadSHA: "abc1234def5678"}); err != nil {
		t.Fatal(err)
	}
	attempt, err := st.BeginMergeAttempt(ctx, "add-cart")
	if err != nil {
		t.Fatal(err)
	}
	if state, err := st.ChangeState(ctx, "add-cart"); err != nil || state != store.ChangeMerging {
		t.Fatalf("add-cart is %q (%v) with an open merge attempt, want merging", state, err)
	}
	return wt, attempt
}

// TestResetOfAMergingChangeIsRefused: while the merger integrates a change, POST /api/changes/<name>/reset
// answers 409 and touches nothing (the merger fetches the child commits from that worktree); once the
// attempt ends the same reset works.
func TestResetOfAMergingChangeIsRefused(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	ctx := context.Background()
	wt, attempt := mergingChange(t, st, root)

	code, body, raw, _ := doReq(t, srv, http.MethodPost, "/api/changes/add-cart/reset", map[string]any{})
	if c, msg := errorOf(body); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, "merger is integrating add-cart") {
		t.Fatalf("resetting a change under an open merge attempt = %d %s, want 409 conflict naming the merger", code, raw)
	}
	if !isFile(filepath.Join(wt, "repos", "shop", "cart.go")) {
		t.Error("the refused reset removed the worktree of the change the merger integrates")
	}
	if m, err := st.OpenMergeAttempt(ctx); err != nil || m == nil || m.ID != attempt || m.ChangeName != "add-cart" {
		t.Errorf("the merge attempt after the refused reset = %+v (%v), want attempt %d still open", m, err, attempt)
	}
	if state, _ := st.ChangeState(ctx, "add-cart"); state != store.ChangeMerging {
		t.Errorf("the refused reset left add-cart %q, want merging", state)
	}
	if lease, _ := st.GetLock(ctx, store.WorkerLeaseKey("add-cart")); lease != nil {
		t.Errorf("the refused reset left the worker lease held: %+v", lease)
	}

	if err := st.FinishMergeAttempt(ctx, attempt, store.MergeSentBack, "the base moved"); err != nil {
		t.Fatal(err)
	}
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/changes/add-cart/reset", map[string]any{})
	if code != http.StatusOK || body["change_name"] != "add-cart" {
		t.Fatalf("resetting add-cart after its merge attempt ended = %d %s, want 200", code, raw)
	}
	if isDir(wt) {
		t.Error("the reset after the attempt ended left the worktree")
	}
	if lease, _ := st.GetLock(ctx, store.WorkerLeaseKey("add-cart")); lease != nil {
		t.Errorf("the reset kept the lease it took: %+v", lease)
	}
}

// TestIntervenePageOfAMergingChangeDoesNotOfferReset: the page of a change the merger is integrating
// says so instead of offering the reset the API would refuse; with the attempt over, the button is back.
func TestIntervenePageOfAMergingChangeDoesNotOfferReset(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	ctx := context.Background()
	_, attempt := mergingChange(t, st, root)

	_, body := changePage(t, srv, "/changes/add-cart/intervene")
	wantOnPage(t, "intervene in a merging change", body, `data-post="/api/changes/add-cart/validate"`, "merger is integrating")
	wantNotOnPage(t, "intervene in a merging change", body, `/reset"`)

	if err := st.FinishMergeAttempt(ctx, attempt, store.MergeSentBack, "the base moved"); err != nil {
		t.Fatal(err)
	}
	_, body = changePage(t, srv, "/changes/add-cart/intervene")
	wantOnPage(t, "intervene after the merge attempt ended", body, `data-post="/api/changes/add-cart/reset"`)
	wantNotOnPage(t, "intervene after the merge attempt ended", body, "merger is integrating")
}
