package cycle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

func TestVisualQARequestChangeNameValidation(t *testing.T) {
	root := t.TempDir()
	cfgDir := filepath.Join(root, ".foreman-kit")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Disable visual QA to verify that change name validation runs before the enabled check.
	if err := os.WriteFile(filepath.Join(cfgDir, "visual_qa.yaml"), []byte("enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenAt(filepath.Join(root, "log", "openspec.db"), "ws-vqa-req-name", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	for _, bad := range []string{"../etc", "  ../etc  ", "Not A Change", "-leading-dash"} {
		var out bytes.Buffer
		item, err := VisualQARequest(ctx, root, st, "probe", bad, &out)
		if err == nil {
			t.Fatalf("expected error for invalid change name %q, got item %+v", bad, item)
		}
		trimmed := strings.TrimSpace(bad)
		want := `"` + trimmed + `" is not the name of a change`
		if err.Error() != want {
			t.Fatalf("for %q: expected error %q, got %q", bad, want, err.Error())
		}
		if out.Len() != 0 {
			t.Fatalf("for %q: expected empty output, got %q", bad, out.String())
		}
	}

	items, err := st.ListVisualQA(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items in store after refused requests, got %d", len(items))
	}

	// Enable visual QA and verify an empty change name and a valid change name both queue.
	if err := os.WriteFile(filepath.Join(cfgDir, "visual_qa.yaml"), []byte("enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	emptyItem, err := VisualQARequest(ctx, root, st, "the footer shows the year", "   ", &out)
	if err != nil {
		t.Fatalf("expected empty change name request to succeed, got %v", err)
	}
	if emptyItem.ChangeName != "" || emptyItem.What != "the footer shows the year" {
		t.Fatalf("unexpected item for empty change name: %+v", emptyItem)
	}

	registerTestChange(t, st, "add-billing")
	out.Reset()
	validItem, err := VisualQARequest(ctx, root, st, "check the totals", "  add-billing  ", &out)
	if err != nil {
		t.Fatalf("expected valid change name request to succeed, got %v", err)
	}
	if validItem.ChangeName != "add-billing" || validItem.What != "check the totals" {
		t.Fatalf("unexpected item for valid change name: %+v", validItem)
	}

	pending, err := st.ListVisualQA(ctx, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ChangeName != "" || pending[1].ChangeName != "add-billing" {
		t.Fatalf("unexpected pending items in store: %+v", pending)
	}
}

// registerTestChange puts a change on record in the store's registry, as the backlog sync or a run would.
func registerTestChange(t *testing.T, st *store.Store, name string) {
	t.Helper()
	if err := st.EmitEvent(context.Background(), store.EventInput{TS: time.Now(), Event: "test.registered", Change: name}); err != nil {
		t.Fatalf("registering change %s: %v", name, err)
	}
}

// TestVisualQARequestRefusesUnknownChange: the command line refuses a well-formed name of a change
// the kit has no record of, and writes no row to the queue or the registry.
func TestVisualQARequestRefusesUnknownChange(t *testing.T) {
	root := t.TempDir()
	cfgDir := filepath.Join(root, ".foreman-kit")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "visual_qa.yaml"), []byte("enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenAt(filepath.Join(root, "log", "openspec.db"), "ws-vqa-req-unknown", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	code := RunVisualQACLI(ctx, root, st, []string{"request", "check x", "--change", "no-such-change"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("request for an unknown change exited 0; stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "no-such-change") {
		t.Errorf("stderr does not name the refused value: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "queued") {
		t.Errorf("stdout reports a queued request: %q", stdout.String())
	}
	if state, err := st.ChangeState(ctx, "no-such-change"); err != nil || state != "" {
		t.Errorf("the refused change is on record as %q (%v)", state, err)
	}
	if items, err := st.ListVisualQA(ctx, ""); err != nil || len(items) != 0 {
		t.Fatalf("the queue holds %d items after a refused request (%v)", len(items), err)
	}

	// A request without a change and one for a change on record still queue.
	stdout.Reset()
	stderr.Reset()
	if code := RunVisualQACLI(ctx, root, st, []string{"request", "the footer shows the year"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "queued") {
		t.Fatalf("request without a change = %d; stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	registerTestChange(t, st, "add-billing")
	stdout.Reset()
	stderr.Reset()
	if code := RunVisualQACLI(ctx, root, st, []string{"request", "check the totals", "--change", "add-billing"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "queued") {
		t.Fatalf("request for a known change = %d; stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	pending, err := st.ListVisualQA(ctx, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ChangeName != "" || pending[1].ChangeName != "add-billing" {
		t.Fatalf("unexpected pending items: %+v", pending)
	}
}
