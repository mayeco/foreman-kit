// Package history keeps the rotated files of an application's history
// (.foreman-kit/agent_history.<N>.json): the changes that left the active agent_history.json, one
// minified change per line, newest in agent_history.1.json and oldest in the highest N.
//
// A rotated file is filled up to MaxLines lines before a new one is opened, so that every rotated
// file but agent_history.1.json holds MaxLines lines. Compact regroups the small files an older kit
// left (one per rotation) the same way.
package history

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// MaxLines is the number of lines of agent_history.json, and of a full rotated file.
const MaxLines = 1000

// perFile is the number of changes a full rotated file holds: one per line, between "[" and "]".
const perFile = MaxLines - 2

var rotatedName = regexp.MustCompile(`^agent_history\.(\d+)\.json$`)

func cfgDir(root string) string { return filepath.Join(root, ".foreman-kit") }

func rotatedPath(root string, n int) string {
	return filepath.Join(cfgDir(root), fmt.Sprintf("agent_history.%d.json", n))
}

// indexes returns the N of every rotated file under root, ascending.
func indexes(root string) []int {
	entries, err := os.ReadDir(cfgDir(root))
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if m := rotatedName.FindStringSubmatch(e.Name()); len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil && n >= 1 {
				out = append(out, n)
			}
		}
	}
	sort.Ints(out)
	return out
}

// readEntries reads the changes a rotated file lists, each compacted to one line. A missing file
// lists none.
func readEntries(path string) ([][]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make([][]byte, 0, len(raw))
	for _, r := range raw {
		var buf bytes.Buffer
		if err := json.Compact(&buf, r); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, buf.Bytes())
	}
	return out, nil
}

func format(entries [][]byte) []byte {
	if len(entries) == 0 {
		return []byte("[]\n")
	}
	var buf bytes.Buffer
	buf.WriteString("[\n")
	for i, e := range entries {
		buf.Write(e)
		if i < len(entries)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("]\n")
	return buf.Bytes()
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// shift renames every agent_history.<N>.json to agent_history.<N+1>.json, highest first.
func shift(root string) error {
	idx := indexes(root)
	for i := len(idx) - 1; i >= 0; i-- {
		if err := os.Rename(rotatedPath(root, idx[i]), rotatedPath(root, idx[i]+1)); err != nil {
			return err
		}
	}
	return nil
}

// Append moves older, the changes that left agent_history.json (oldest first), into the rotated
// files: it fills agent_history.1.json up to MaxLines lines and only then shifts the rotated files
// and opens a new agent_history.1.json.
func Append(root string, older []map[string]any) error {
	if len(older) == 0 {
		return nil
	}
	add := make([][]byte, 0, len(older))
	for _, it := range older {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(it); err != nil {
			return err
		}
		add = append(add, bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	}
	current, err := readEntries(rotatedPath(root, 1))
	if err != nil {
		// A first rotated file that does not parse is left as it is: the new changes open another.
		if err := shift(root); err != nil {
			return err
		}
		current = nil
	}
	for len(add) > 0 {
		room := perFile - len(current)
		if room <= 0 {
			if err := shift(root); err != nil {
				return err
			}
			current, room = nil, perFile
		}
		if room > len(add) {
			room = len(add)
		}
		current = append(current, add[:room]...)
		add = add[room:]
		if err := writeAtomic(rotatedPath(root, 1), format(current)); err != nil {
			return err
		}
	}
	return nil
}

// NeedsCompaction says whether a rotated file other than the newest holds fewer than MaxLines
// lines, or the numbering has gaps: the layout an older kit left, one small file per rotation.
func NeedsCompaction(root string) bool {
	idx := indexes(root)
	for i, n := range idx {
		if n != i+1 {
			return true
		}
		if n == 1 {
			continue
		}
		entries, err := readEntries(rotatedPath(root, n))
		if err != nil {
			return false
		}
		if len(entries) < perFile {
			return true
		}
	}
	return false
}

// Compact regroups the rotated files so that each holds MaxLines lines, oldest first, and the
// newest, agent_history.1.json, holds the rest. It returns the paths, relative to root, it wrote or
// removed. A rotated file that does not parse leaves every file as it was.
func Compact(root string) ([]string, error) {
	if !NeedsCompaction(root) {
		return nil, nil
	}
	idx := indexes(root)
	var all [][]byte
	for i := len(idx) - 1; i >= 0; i-- {
		entries, err := readEntries(rotatedPath(root, idx[i]))
		if err != nil {
			return nil, err
		}
		all = append(all, entries...)
	}
	var chunks [][][]byte
	for len(all) > 0 {
		n := perFile
		if n > len(all) {
			n = len(all)
		}
		chunks = append(chunks, all[:n])
		all = all[n:]
	}
	touched := map[string]bool{}
	rel := func(n int) string { return fmt.Sprintf(".foreman-kit/agent_history.%d.json", n) }
	// chunks[0] is the oldest: it becomes the highest N.
	for i, chunk := range chunks {
		n := len(chunks) - i
		if err := writeAtomic(rotatedPath(root, n), format(chunk)); err != nil {
			return nil, err
		}
		touched[rel(n)] = true
	}
	for _, n := range idx {
		if n > len(chunks) {
			if err := os.Remove(rotatedPath(root, n)); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			touched[rel(n)] = true
		}
	}
	out := make([]string, 0, len(touched))
	for p := range touched {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
