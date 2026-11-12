package history

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func changes(from, n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{"change_name": fmt.Sprintf("c-%04d", from+i), "note": "a<b>&c"}
	}
	return out
}

func lines(t *testing.T, root string, n int) int {
	t.Helper()
	b, err := os.ReadFile(rotatedPath(root, n))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(b, []byte("\n"))
}

func names(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	idx := indexes(root)
	for i := len(idx) - 1; i >= 0; i-- {
		entries, err := readEntries(rotatedPath(root, idx[i]))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			out = append(out, string(e))
		}
	}
	return out
}

// Rotating many small batches fills agent_history.1.json up to MaxLines lines before a new one
// opens, and keeps the changes in order.
func TestAppendFillsTheNewestFileFirst(t *testing.T) {
	root := t.TempDir()
	total := 0
	for total < 2500 {
		if err := Append(root, changes(total, 40)); err != nil {
			t.Fatal(err)
		}
		total += 40
	}
	idx := indexes(root)
	if len(idx) != 3 {
		t.Fatalf("want 3 rotated files for %d changes, got %v", total, idx)
	}
	for _, n := range idx[1:] {
		if got := lines(t, root, n); got != MaxLines {
			t.Fatalf("agent_history.%d.json has %d lines, want %d", n, got, MaxLines)
		}
	}
	all := names(t, root)
	if len(all) != total {
		t.Fatalf("want %d changes, got %d", total, len(all))
	}
	for i, e := range all {
		want := fmt.Sprintf(`{"change_name":"c-%04d","note":"a<b>&c"}`, i)
		if e != want {
			t.Fatalf("entry %d = %s, want %s", i, e, want)
		}
	}
}

// update regroups the small files an older kit left, oldest in the highest N, and removes the rest.
func TestCompactRegroupsSmallFiles(t *testing.T) {
	root := t.TempDir()
	// The layout of the older kit: one file of 33 changes per rotation, newest in .1.
	const files, per = 50, 33
	for i := 0; i < files; i++ {
		n := files - i
		if err := writeAtomic(rotatedPath(root, n), format(nil)); err != nil {
			t.Fatal(err)
		}
		var entries [][]byte
		for _, c := range changes(i*per, per) {
			entries = append(entries, []byte(fmt.Sprintf(`{"change_name":%q}`, c["change_name"])))
		}
		if err := writeAtomic(rotatedPath(root, n), format(entries)); err != nil {
			t.Fatal(err)
		}
	}
	if !NeedsCompaction(root) {
		t.Fatal("small rotated files need compaction")
	}
	touched, err := Compact(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(touched) != files {
		t.Fatalf("want every file touched, got %d", len(touched))
	}
	idx := indexes(root)
	if len(idx) != 2 {
		t.Fatalf("want 2 files for %d changes, got %v", files*per, idx)
	}
	if got := lines(t, root, 2); got != MaxLines {
		t.Fatalf("agent_history.2.json has %d lines, want %d", got, MaxLines)
	}
	all := names(t, root)
	if len(all) != files*per {
		t.Fatalf("want %d changes, got %d", files*per, len(all))
	}
	for i, e := range all {
		if want := fmt.Sprintf(`{"change_name":"c-%04d"}`, i); e != want {
			t.Fatalf("entry %d = %s, want %s", i, e, want)
		}
	}
	if NeedsCompaction(root) {
		t.Fatal("compacted files still need compaction")
	}
	if again, err := Compact(root); err != nil || len(again) != 0 {
		t.Fatalf("a second compaction touched %v (%v)", again, err)
	}
}

func TestCompactLeavesAnUnreadableFile(t *testing.T) {
	root := t.TempDir()
	if err := writeAtomic(rotatedPath(root, 1), format([][]byte{[]byte(`{"change_name":"a"}`)})); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir(root), "agent_history.3.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Compact(root); err == nil {
		t.Fatal("want an error for a file that does not parse")
	}
	if b, _ := os.ReadFile(filepath.Join(cfgDir(root), "agent_history.3.json")); string(b) != "not json" {
		t.Fatal("the unreadable file was changed")
	}
}
