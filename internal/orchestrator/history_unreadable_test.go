package orchestrator

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// historyUnreadableDir returns a directory with the given .foreman-kit/agent_backlog.json and
// .foreman-kit/agent_history.json (an empty content writes no file) in which the changes named in archived
// are archived.
func historyUnreadableDir(t *testing.T, backlog, history string, archived ...string) (dir, backlogPath, historyPath string) {
	t.Helper()
	dir = t.TempDir()
	backlogPath = filepath.Join(dir, ".foreman-kit", "agent_backlog.json")
	historyPath = filepath.Join(dir, ".foreman-kit", "agent_history.json")
	if backlog != "" {
		mustWriteFile(t, backlogPath, backlog)
	}
	if history != "" {
		mustWriteFile(t, historyPath, history)
	}
	for _, name := range archived {
		mustWriteFile(t, filepath.Join(dir, "openspec", "changes", "archive", "2026-01-02-"+name, "proposal.md"), "# "+name+"\n")
	}
	return dir, backlogPath, historyPath
}

func historyUnreadableNames(t *testing.T, path string) []string {
	t.Helper()
	var names []string
	for _, entry := range readRawMapsFromFile(path) {
		names = append(names, entry["change_name"].(string))
	}
	return names
}

const historyUnreadableBacklog = `[
  {"change_name": "new-one", "description": "The change integrated now"},
  {"change_name": "still-queued"}
]
`

// A history that does not parse (here, a merge someone committed with its conflict markers) was read as
// an empty one: the integrated item was appended to nothing and the file written back as a one-entry
// list, which the merger then committed and pushed. Every entry the file held was gone from the base.
func TestArchiveBacklogInDirLeavesAnUnreadableHistoryAlone(t *testing.T) {
	conflicted := `[
  {"change_name": "old-one", "integrated_at": "2026-01-01T00:00:00Z"},
<<<<<<< HEAD
  {"change_name": "old-two", "integrated_at": "2026-01-01T01:00:00Z"}
=======
  {"change_name": "old-three", "integrated_at": "2026-01-01T02:00:00Z"}
>>>>>>> theirs
]
`
	dir, backlogPath, historyPath := historyUnreadableDir(t, historyUnreadableBacklog, conflicted, "new-one")
	item := map[string]any{"change_name": "new-one"}

	err := ArchiveBacklogInDir(dir, item, "2026-01-02T00:00:00Z")
	if err == nil || !strings.Contains(err.Error(), "agent_history.json") {
		t.Fatalf("ArchiveBacklogInDir = %v, want an error naming the history it cannot read", err)
	}
	if got, _ := os.ReadFile(historyPath); string(got) != conflicted {
		t.Fatalf("the history that does not parse was rewritten, losing its entries:\n%s", got)
	}
	if got, _ := os.ReadFile(backlogPath); string(got) != historyUnreadableBacklog {
		t.Fatalf("the backlog was rewritten although its item could not be moved to the history:\n%s", got)
	}

	// Once a person repairs the history, the next call moves the item with what the backlog says of it.
	mustWriteFile(t, historyPath, `[{"change_name": "old-one"}, {"change_name": "old-two"}]`+"\n")
	if err := ArchiveBacklogInDir(dir, item, "2026-01-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if got, want := historyUnreadableNames(t, historyPath), []string{"old-one", "old-two", "new-one"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %v, want %v", got, want)
	}
	if moved := readRawMapsFromFile(historyPath)[2]; moved["description"] != "The change integrated now" || moved["integrated_at"] != "2026-01-02T00:00:00Z" {
		t.Fatalf("the moved entry lost what the backlog said of it: %+v", moved)
	}
	if got, want := historyUnreadableNames(t, backlogPath), []string{"still-queued"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("backlog = %v, want %v", got, want)
	}
}

func TestArchiveBacklogInDirTellsAnEmptyHistoryFromAnUnreadableOne(t *testing.T) {
	item := map[string]any{"change_name": "new-one"}
	for name, history := range map[string]string{
		"not JSON":                 "integrated: old-one, old-two\n",
		"a truncated list":         `[{"change_name": "old-one"}, {"change_name": "old-`,
		"a byte order mark":        string([]byte{0xEF, 0xBB, 0xBF}) + `[{"change_name": "old-one"}]`,
		"an object of another use": `{"entries": [{"change_name": "old-one"}]}`,
		"a JSON string":            `"old-one"`,
	} {
		dir, backlogPath, historyPath := historyUnreadableDir(t, historyUnreadableBacklog, history, "new-one")
		if err := ArchiveBacklogInDir(dir, item, ""); err == nil {
			t.Errorf("%s: no error for a history that lists no changes", name)
		}
		if got, _ := os.ReadFile(historyPath); string(got) != history {
			t.Errorf("%s: the history was rewritten:\n%s", name, got)
		}
		if got, _ := os.ReadFile(backlogPath); string(got) != historyUnreadableBacklog {
			t.Errorf("%s: the backlog was rewritten:\n%s", name, got)
		}
	}

	// A history that holds nothing, or lists changes in either supported shape, takes the item.
	for name, history := range map[string]string{
		"no file":                "",
		"an empty file":          "\n",
		"an empty list":          "[]\n",
		"an empty object":        "{}\n",
		"null":                   "null\n",
		"a list":                 `[{"change_name": "old-one"}]`,
		"an object with changes": `{"changes": [{"change_name": "old-one"}]}`,
	} {
		dir, backlogPath, historyPath := historyUnreadableDir(t, historyUnreadableBacklog, history, "new-one")
		if err := ArchiveBacklogInDir(dir, item, ""); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := historyUnreadableNames(t, historyPath); len(got) == 0 || got[len(got)-1] != "new-one" {
			t.Errorf("%s: history = %v, want it to end with new-one", name, got)
		}
		if got, want := historyUnreadableNames(t, backlogPath), []string{"still-queued"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: backlog = %v, want %v", name, got, want)
		}
	}
}

// A backlog that does not parse is never rewritten either, and does not stop the history from taking
// the item the merger hands over.
func TestArchiveBacklogInDirLeavesAnUnreadableBacklogAlone(t *testing.T) {
	broken := `[{"change_name": "new-one"}, {"change_name": "still-queued"`
	dir, backlogPath, historyPath := historyUnreadableDir(t, broken, `[{"change_name": "old-one"}]`, "new-one")
	if err := ArchiveBacklogInDir(dir, map[string]any{"change_name": "new-one"}, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(backlogPath); string(got) != broken {
		t.Fatalf("the backlog that does not parse was rewritten:\n%s", got)
	}
	if got, want := historyUnreadableNames(t, historyPath), []string{"old-one", "new-one"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %v, want %v", got, want)
	}
}

// The same through a real integration: the history on the base does not parse, the change integrates
// and the base keeps the file byte for byte, as does the main checkout.
func TestMergerLeavesAnUnreadableHistoryOnTheBaseAlone(t *testing.T) {
	sb := newMCFSandbox(t, "")
	conflicted := "[\n<<<<<<< HEAD\n  {\"change_name\": \"old-two\"}\n=======\n  {\"change_name\": \"old-three\"}\n>>>>>>> theirs\n]\n"
	historyPath := filepath.Join(sb.umbrella, ".foreman-kit", "agent_history.json")
	mustWriteFile(t, historyPath, conflicted)
	mcfGit(t, sb.umbrella, "commit", "--quiet", "-am", "chore: a merge committed with its conflict markers")
	mcfGit(t, sb.umbrella, "push", "--quiet", "origin", "master")

	sb.runCoreDispatch(t, "add-a", map[string]string{"a.txt": "a\n"})
	var mergerLog bytes.Buffer
	runMergerOnce(t, sb.newMerger(t, MergerOptions{Out: &mergerLog}))
	sb.requireIntegrated(t, "add-a")

	if got := mcfGit(t, sb.umbrella, "show", "origin/master:.foreman-kit/agent_history.json"); got != strings.TrimSpace(conflicted) {
		t.Fatalf("the merge of add-a rewrote the history on the base, losing what it held:\n%s", got)
	}
	if got, _ := os.ReadFile(historyPath); string(got) != conflicted {
		t.Fatalf("the merge of add-a rewrote the history of the main checkout:\n%s", got)
	}
	if !strings.Contains(mergerLog.String(), "agent_history.json does not parse as JSON") {
		t.Fatalf("the merger did not say that it left the history alone:\n%s", mergerLog.String())
	}
}
