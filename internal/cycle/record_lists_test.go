package cycle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

// TestJSONListProblem: a recorded list passes only in the shapes the dispatcher reads a backlog in.
func TestJSONListProblem(t *testing.T) {
	written := filepath.Join(t.TempDir(), "list.json")
	if err := WriteJSONList(written, []map[string]any{{"change_name": "add-a", "description": "a"}, {"change_name": "add-b"}}); err != nil {
		t.Fatal(err)
	}
	asWritten, err := os.ReadFile(written)
	if err != nil {
		t.Fatal(err)
	}
	full := `[{"change_name": "add-a", "description": "what add-a is about"}, {"change_name": "add-b", "description": "what add-b is about"}]`
	for _, tc := range []struct {
		name    string
		content string
		want    string // "" when the content passes, else a part of the problem
	}{
		{"an indented array as WriteJSONList writes it", string(asWritten), ""},
		{"the legacy changes wrapper", `{"changes": [{"change_name": "add-a"}]}`, ""},
		{"an array cut off mid-item", full[:strings.Index(full, "add-b")+3], "unexpected end of JSON input"},
		{"an object wrapping its items under another key", `{"proposals": [{"change_name": "add-a"}]}`, `without a "changes" list`},
		{"one item without the list around it", `{"change_name": "add-a", "description": "a"}`, `without a "changes" list`},
		{"a scalar", `42`, "a JSON number, not a list"},
		{"an empty file", ``, "the file is empty"},
		{"a file of whitespace", " \n\t\n", "the file is empty"},
		{"newline-delimited objects", "{\"change_name\": \"add-a\"}\n{\"change_name\": \"add-b\"}\n", "after top-level value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := jsonListProblem([]byte(tc.content))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("jsonListProblem(%q) = %v, want it to pass", tc.content, err)
			case tc.want != "" && err == nil:
				t.Fatalf("jsonListProblem(%q) passed, want %q", tc.content, tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("jsonListProblem(%q) = %v, want it to say %q", tc.content, err, tc.want)
			}
		})
	}
}

// listsRepo is a session checkout whose base, on origin, holds the memory and a valid proposals file.
func listsRepo(t *testing.T) (checkout, bare, proposals string) {
	t.Helper()
	t.Setenv(settings.RunOverridesEnv, "")
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	checkout, bare = memoryRepo(t)
	proposals = "[\n  {\n    \"change_name\": \"proposed-before\",\n    \"status\": \"proposed\"\n  }\n]\n"
	writeRel(t, checkout, ProposalsFile, proposals)
	gitRun(t, checkout, "add", "-A")
	gitRun(t, checkout, "commit", "--quiet", "-m", "proposals")
	gitRun(t, checkout, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	gitRun(t, checkout, "fetch", "--quiet", "origin")
	return checkout, bare, proposals
}

func writeRel(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func listsStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.OpenAt(filepath.Join(t.TempDir(), "state.db"), "ws-record-lists", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// keptCopies returns the copies the record kept of rejected files named base, for the checkout at dir.
func keptCopies(t *testing.T, dir, base string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(store.RuntimePath(dir, "rejected"), "*-"+base))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

const truncatedProposals = "[\n  {\n    \"change_name\": \"proposed-before\",\n    \"status\": \"proposed\"\n  },\n  {\n    \"change_name\": \"proposed-n"

// A truncated proposals file is not published: origin keeps the previous one and still receives the
// memory edit of the same session, the log names the file and its parse error, and the bytes are kept.
func TestRecordSessionRestoresATruncatedListAndRecordsTheRest(t *testing.T) {
	ctx := context.Background()
	wt, bare, proposals := listsRepo(t)
	st := listsStore(t, wt)
	pitfall(t, wt, "- **Truncated lists**: the record restores them (fix-cycle-records-invalid-json-lists)")
	writeRel(t, wt, ProposalsFile, truncatedProposals)
	before := gitRun(t, bare, "rev-parse", "refs/heads/main")

	var out bytes.Buffer
	if err := RecordSession(ctx, wt, "main", "chore(memory): record", st, &out); err != nil {
		t.Fatalf("RecordSession: %v\n%s", err, out.String())
	}
	if after := gitRun(t, bare, "rev-parse", "refs/heads/main"); after == before {
		t.Fatalf("the memory edit was not published:\n%s", out.String())
	}
	if got := gitRun(t, bare, "show", "refs/heads/main:"+ProposalsFile); got != strings.TrimSpace(proposals) {
		t.Fatalf("origin holds the proposals file\n%s\nwant the one it held before the record:\n%s", got, proposals)
	}
	if got := gitRun(t, bare, "show", "refs/heads/main:"+memory.PitfallsFile); !strings.Contains(got, "**Truncated lists**") {
		t.Fatalf("origin did not receive the memory edit:\n%s", got)
	}
	if changed := gitRun(t, bare, "show", "--name-only", "--format=", "refs/heads/main"); strings.Contains(changed, ProposalsFile) {
		t.Fatalf("the record commit changes %s:\n%s", ProposalsFile, changed)
	}
	kept := keptCopies(t, wt, "agent_proposals.json")
	if len(kept) != 1 {
		t.Fatalf("want one kept copy of the proposals file, got %v", kept)
	}
	if b, err := os.ReadFile(kept[0]); err != nil || string(b) != truncatedProposals {
		t.Fatalf("the kept copy %s does not hold the exact bytes the session left: %q (err %v)", kept[0], b, err)
	}
	want := "record: " + ProposalsFile + " is not a valid JSON list (unexpected end of JSON input); not recorded: restored to the version HEAD holds, the rejected content is kept as " + kept[0] + "\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("expected %q in the output:\n%s", want, out.String())
	}
	if got, err := os.ReadFile(filepath.Join(wt, filepath.FromSlash(ProposalsFile))); err != nil || string(got) != proposals {
		t.Fatalf("the checkout's proposals file was not restored: %q (err %v)", got, err)
	}
}

// An invalid list HEAD does not track is removed, and the record commits nothing of it.
func TestRecordSessionRemovesAnInvalidListHEADDoesNotTrack(t *testing.T) {
	ctx := context.Background()
	wt, bare, _ := listsRepo(t)
	st := listsStore(t, wt)
	pitfall(t, wt, "- **Untracked lists**: removed when invalid (fix-cycle-records-invalid-json-lists)")
	writeRel(t, wt, BacklogFile, `{"proposals": [{"change_name": "add-a"}]}`)
	// The session even staged it.
	gitRun(t, wt, "add", "--", BacklogFile)

	var out bytes.Buffer
	if err := RecordSession(ctx, wt, "main", "chore(memory): record", st, &out); err != nil {
		t.Fatalf("RecordSession: %v\n%s", err, out.String())
	}
	if listed := gitRun(t, bare, "ls-tree", "-r", "--name-only", "refs/heads/main"); strings.Contains(listed, BacklogFile) {
		t.Fatalf("origin received the invalid backlog:\n%s", listed)
	}
	if got := gitRun(t, bare, "show", "refs/heads/main:"+memory.PitfallsFile); !strings.Contains(got, "**Untracked lists**") {
		t.Fatalf("origin did not receive the memory edit:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(wt, filepath.FromSlash(BacklogFile))); !os.IsNotExist(err) {
		t.Fatalf("the invalid backlog HEAD does not track is still in the checkout (err %v)", err)
	}
	if !strings.Contains(out.String(), "record: "+BacklogFile+` is not a valid JSON list (a JSON object without a "changes" list); not recorded: removed, since HEAD holds no such file`) {
		t.Fatalf("the output does not say the backlog was removed:\n%s", out.String())
	}
	if kept := keptCopies(t, wt, "agent_backlog.json"); len(kept) != 1 {
		t.Fatalf("want one kept copy of the backlog, got %v", kept)
	}
}

// A session whose only change is an invalid list has nothing to record, and pushes nothing.
func TestRecordSessionWithOnlyAnInvalidListRecordsNothing(t *testing.T) {
	ctx := context.Background()
	wt, bare, _ := listsRepo(t)
	st := listsStore(t, wt)
	writeRel(t, wt, HistoryFile, "  \n")
	writeRel(t, wt, ProposalsFile, truncatedProposals)
	before := gitRun(t, bare, "rev-parse", "refs/heads/main")

	var out bytes.Buffer
	if err := RecordSession(ctx, wt, "main", "chore(memory): record", st, &out); err != nil {
		t.Fatalf("RecordSession: %v\n%s", err, out.String())
	}
	if after := gitRun(t, bare, "rev-parse", "refs/heads/main"); after != before {
		t.Fatalf("a session with nothing valid to record pushed %s:\n%s", after, out.String())
	}
	for _, want := range []string{
		"record: " + HistoryFile + " is not a valid JSON list (the file is empty)",
		"record: " + ProposalsFile + " is not a valid JSON list (unexpected end of JSON input)",
		"record: nothing to record\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("expected %q in the output:\n%s", want, out.String())
		}
	}
	if dirty := gitRun(t, wt, "status", "--porcelain"); dirty != "" {
		t.Fatalf("the checkout is left dirty:\n%s", dirty)
	}
}

// A valid list edit is committed and pushed as the session left it, with no rejection line.
func TestRecordSessionRecordsAValidListUnchanged(t *testing.T) {
	ctx := context.Background()
	wt, bare, _ := listsRepo(t)
	st := listsStore(t, wt)
	edited := "[\n  {\n    \"change_name\": \"proposed-before\",\n    \"status\": \"proposed\"\n  },\n  {\n    \"change_name\": \"proposed-now\",\n    \"status\": \"proposed\"\n  }\n]\n"
	writeRel(t, wt, ProposalsFile, edited)

	var out bytes.Buffer
	if err := RecordSession(ctx, wt, "main", "chore(memory): record", st, &out); err != nil {
		t.Fatalf("RecordSession: %v\n%s", err, out.String())
	}
	if got := gitRun(t, bare, "show", "refs/heads/main:"+ProposalsFile); got != strings.TrimSpace(edited) {
		t.Fatalf("origin holds\n%s\nwant the session's proposals unchanged:\n%s", got, edited)
	}
	if strings.Contains(out.String(), "is not a valid JSON list") {
		t.Fatalf("a valid list was reported as rejected:\n%s", out.String())
	}
	if kept := keptCopies(t, wt, "agent_proposals.json"); len(kept) != 0 {
		t.Fatalf("a valid list was kept as rejected: %v", kept)
	}
}

// visual-qa end records through the same check: origin keeps its valid proposals file and receives the
// session's memory edit, and the output names the rejected file.
func TestVisualQAEndRestoresAnInvalidList(t *testing.T) {
	stopTestGit(t)
	t.Setenv(settings.RunOverridesEnv, "")
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", bare)
	root := filepath.Join(base, "root")
	gitIn(t, base, "clone", "-q", bare, root)
	settingstest.WriteFiles(t, root, nil)
	if _, err := memory.Init(ctx, root, "demo"); err != nil {
		t.Fatal(err)
	}
	proposals := "[\n  {\n    \"change_name\": \"proposed-before\",\n    \"description\": \"an improvement\",\n    \"status\": \"proposed\"\n  }\n]\n"
	writeRel(t, root, ProposalsFile, proposals)
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "an application with one proposal")
	gitIn(t, root, "push", "-q", "origin", "HEAD:main")
	gitIn(t, root, "branch", "-q", "--set-upstream-to=origin/main")
	st := openTestStore(t, root)

	// The session, without a worktree of its own, leaves its findings in the main checkout.
	pitfall(t, root, "- **Seen in the browser**: the lists are checked (fix-cycle-records-invalid-json-lists)")
	writeRel(t, root, ProposalsFile, truncatedProposals)

	var out bytes.Buffer
	if err := VisualQAEnd(ctx, root, st, VisualQAOptions{Stamp: "20261115T080000Z"}, &out); err != nil {
		t.Fatalf("VisualQAEnd: %v\n%s", err, out.String())
	}
	if got := gitIn(t, bare, "show", "main:"+ProposalsFile); got != proposals {
		t.Fatalf("origin holds the proposals file\n%s\nwant the valid one it held:\n%s", got, proposals)
	}
	if got := gitIn(t, bare, "show", "main:"+memory.PitfallsFile); !strings.Contains(got, "**Seen in the browser**") {
		t.Fatalf("origin did not receive the session's memory edit:\n%s\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "record: "+ProposalsFile+" is not a valid JSON list (unexpected end of JSON input)") {
		t.Fatalf("the output does not name the rejected file:\n%s", out.String())
	}
}
