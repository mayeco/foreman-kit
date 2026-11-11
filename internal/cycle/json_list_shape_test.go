package cycle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jsonListShapeWrite writes content to root/rel, creating its directory.
func jsonListShapeWrite(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A JSON object is a list file only when it carries the legacy "changes" list. Any other object (an
// agent wrapping its items in {"proposals": [...]}, or writing one item without the list around it) is
// not an empty list: reading it as one let every writer replace the file with what was left.
func TestJSONListShapeObjectWithoutChangesIsRejected(t *testing.T) {
	for _, doc := range []string{
		`{"proposals":[{"change_name":"add-export","status":"proposed"}]}`,
		`{"change_name":"add-export","status":"proposed"}`,
		`{"changes":null}`,
		`{"changes":{"change_name":"add-export"}}`,
		`{}`,
	} {
		if items, err := parseJSONListBytes([]byte(doc)); err == nil {
			t.Errorf("parseJSONListBytes(%s) = %v, nil; want an error", doc, items)
		}
	}

	for doc, want := range map[string]int{
		``:                               0,
		`[]`:                             0,
		`[{"change_name":"add-export"}]`: 1,
		`{"changes":[]}`:                 0,
		`{"changes":[{"change_name":"a"},{"change_name":"b"}]}`: 2,
	} {
		items, err := parseJSONListBytes([]byte(doc))
		if err != nil {
			t.Errorf("parseJSONListBytes(%s): %v", doc, err)
			continue
		}
		if len(items) != want {
			t.Errorf("parseJSONListBytes(%s) has %d item(s), want %d", doc, len(items), want)
		}
	}
}

// ReviewProposals rewrites .foreman-kit/agent_proposals.json on every run. With a proposals file that is not a
// list it must fail and leave the file as it is, as the cycle reports ("proposals left as they were"),
// instead of replacing the agent's proposals with [].
func TestJSONListShapeReviewLeavesMisShapedProposalsUntouched(t *testing.T) {
	root := t.TempDir()
	jsonListShapeWrite(t, root, CycleFile, "proposals:\n  approved_by: human\n")
	jsonListShapeWrite(t, root, BacklogFile, "[]\n")
	jsonListShapeWrite(t, root, HistoryFile, "[]\n")
	misShaped := `{"proposals":[{"change_name":"add-export","status":"proposed","description":"Export invoices as CSV"}]}` + "\n"
	propPath := jsonListShapeWrite(t, root, ProposalsFile, misShaped)

	var out bytes.Buffer
	if err := ReviewProposals(context.Background(), root, nil, nil, &out); err == nil {
		t.Errorf("ReviewProposals accepted a proposals file that is not a list (said %q)", strings.TrimSpace(out.String()))
	}
	got, err := os.ReadFile(propPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != misShaped {
		t.Fatalf("ReviewProposals rewrote the proposals file:\n got: %s\nwant: %s", got, misShaped)
	}
}

// A mis-shaped backlog must not be replaced by the proposals ReviewProposals queues.
func TestJSONListShapeReviewLeavesMisShapedBacklogUntouched(t *testing.T) {
	root := t.TempDir()
	jsonListShapeWrite(t, root, CycleFile, "proposals:\n  approved_by: nobody\n")
	jsonListShapeWrite(t, root, HistoryFile, "[]\n")
	jsonListShapeWrite(t, root, ProposalsFile, `[{"change_name":"add-export","status":"proposed","description":"Export invoices as CSV"}]`+"\n")
	misShaped := `{"backlog":[{"change_name":"fix-login","priority":"high","description":"Sessions expire too early"}]}` + "\n"
	backlogPath := jsonListShapeWrite(t, root, BacklogFile, misShaped)

	if err := ReviewProposals(context.Background(), root, nil, nil, nil); err == nil {
		t.Errorf("ReviewProposals accepted a backlog that is not a list")
	}
	got, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != misShaped {
		t.Fatalf("ReviewProposals rewrote the backlog:\n got: %s\nwant: %s", got, misShaped)
	}
}

// The 3-way merge reads "absent from local" as "deleted by local". A local file that is not a list must
// therefore not merge as an empty list, which deleted every item of the base from the target.
func TestJSONListShapeMergeDoesNotDeleteTheBaseForAMisShapedLocal(t *testing.T) {
	base := `[{"change_name":"fix-login"},{"change_name":"add-search"}]`
	local := `{"backlog":[{"change_name":"fix-login"},{"change_name":"add-search"},{"change_name":"add-export"}]}`
	target := base

	if merged, ok := MergeJSONList3Way(BacklogFile, &base, &local, &target, nil); ok {
		t.Fatalf("MergeJSONList3Way merged a local file that is not a list into %v", merged)
	}

	root := t.TempDir()
	backlogPath := jsonListShapeWrite(t, root, BacklogFile, target)
	if err := Reconcile3Way(context.Background(), root, map[string]*string{BacklogFile: &base}, map[string]*string{BacklogFile: &local}); err != nil {
		t.Fatalf("Reconcile3Way: %v", err)
	}
	got, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fix-login", "add-search", "add-export"} {
		if !strings.Contains(string(got), name) {
			t.Fatalf("Reconcile3Way lost %s from the backlog; the file is now:\n%s", name, got)
		}
	}
}
