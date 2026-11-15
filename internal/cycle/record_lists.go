package cycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/store"
)

// rejectedList is a JSON list file a session record did not publish because it does not parse as a
// list: the file as the record names it, why it was rejected, and where its content was kept.
type rejectedList struct {
	// Path is the file relative to the checkout, with forward slashes (".foreman-kit/agent_backlog.json").
	Path string
	// Problem is the parse error, or what the document is instead of a list.
	Problem string
	// Copy is the absolute path the rejected bytes were copied to; "" when they could not be kept.
	Copy string
	// CopyErr says why the bytes could not be kept, when Copy is "".
	CopyErr string
	// Removed is true when the checkout's commit holds no such file, so it was removed instead of restored.
	Removed bool
}

// jsonListProblem says why b is not a JSON list the kit reads, or nil when it is one. A list is one
// JSON document that is an array, or an object whose "changes" member is an array: the shape the
// dispatcher reads a backlog in (internal/orchestrator, loadBacklog), the strictest of the kit's
// readers, so a file that passes is readable by all of them. An empty file and newline-delimited
// objects, which parseJSONListBytes tolerates, do not pass: the dispatcher refuses both.
func jsonListProblem(b []byte) error {
	if len(bytes.TrimSpace(b)) == 0 {
		return errors.New("the file is empty")
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case []any:
		return nil
	case map[string]any:
		if _, ok := v["changes"].([]any); ok {
			return nil
		}
		return errors.New(`a JSON object without a "changes" list`)
	case string:
		return errors.New("a JSON string, not a list")
	case float64:
		return errors.New("a JSON number, not a list")
	case bool:
		return errors.New("a JSON boolean, not a list")
	default:
		return errors.New("a JSON null, not a list")
	}
}

// recordedJSONLists returns the JSON list files a record of the checkout at dir would publish: the
// three lists of the kit, in a fixed order, then the rotated history files that exist there.
func recordedJSONLists(dir string) []string {
	paths := make([]string, 0, len(JSONListPaths))
	for p := range JSONListPaths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return append(paths, RotatedHistoryRelPaths(dir)...)
}

// restoreInvalidLists checks every JSON list file of the record that exists in the checkout at dir and
// differs from its HEAD, and takes out of the record each one that does not parse as a list: its bytes
// are copied to the workspace's runtime directory (rejected/<stamp>-<file name>), and the file is
// restored to the version HEAD holds, or removed when HEAD holds none. Each rejection writes one
// "record:" line to out. The record then publishes the rest as it would have. An error means a file
// could not be taken out of the record, which must then not publish it.
func restoreInvalidLists(ctx context.Context, dir, stamp string, out io.Writer) ([]rejectedList, error) {
	var rejected []rejectedList
	for _, rel := range recordedJSONLists(dir) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			// A deleted list is recorded as a deletion, as before; nothing else here is a list file.
			continue
		}
		status, err := gitx.RunGit(ctx, dir, "status", "--porcelain", "-uall", "--", rel)
		if err != nil {
			return rejected, fmt.Errorf("record: cannot tell whether %s changed: %w", rel, err)
		}
		if strings.TrimSpace(status) == "" {
			continue
		}
		content, err := os.ReadFile(full)
		if err != nil {
			return rejected, fmt.Errorf("record: cannot read %s to check it: %w", rel, err)
		}
		problem := jsonListProblem(content)
		if problem == nil {
			continue
		}

		r := rejectedList{Path: rel, Problem: problem.Error()}
		keep := store.RuntimePath(dir, "rejected", stamp+"-"+filepath.Base(full))
		if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
			r.CopyErr = err.Error()
		} else if err := os.WriteFile(keep, content, 0o644); err != nil {
			r.CopyErr = err.Error()
		} else {
			r.Copy = keep
		}

		if _, err := gitx.RunGit(ctx, dir, "cat-file", "-e", "HEAD:"+rel); err == nil {
			if _, err := gitx.RunGit(ctx, dir, "checkout", "-q", "HEAD", "--", rel); err != nil {
				return rejected, fmt.Errorf("record: %s is not a valid JSON list (%s) and could not be restored to the version HEAD holds: %w", rel, r.Problem, err)
			}
		} else {
			r.Removed = true
			// A list the session added to the index leaves it too, so that nothing stages it again.
			if _, err := gitx.RunGit(ctx, dir, "rm", "-q", "--cached", "--ignore-unmatch", "--", rel); err != nil {
				return rejected, fmt.Errorf("record: %s is not a valid JSON list (%s) and could not be taken out of the index: %w", rel, r.Problem, err)
			}
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				return rejected, fmt.Errorf("record: %s is not a valid JSON list (%s) and could not be removed: %w", rel, r.Problem, err)
			}
		}
		fmt.Fprintln(out, r.logLine())
		rejected = append(rejected, r)
	}
	return rejected, nil
}

// logLine is the line the record writes for r.
func (r rejectedList) logLine() string {
	what := "restored to the version HEAD holds"
	if r.Removed {
		what = "removed, since HEAD holds no such file"
	}
	return fmt.Sprintf("record: %s is not a valid JSON list (%s); not recorded: %s, %s", r.Path, r.Problem, what, r.keptAs())
}

// keptAs says where the rejected content was kept, or why it could not be.
func (r rejectedList) keptAs() string {
	if r.Copy != "" {
		return "the rejected content is kept as " + r.Copy
	}
	return "the rejected content could not be kept (" + r.CopyErr + ")"
}

// notRecordedSection is the section the cycle appends to its report for the files its record rejected.
func notRecordedSection(rejected []rejectedList) string {
	var b strings.Builder
	b.WriteString("\n## Not recorded\n\n")
	b.WriteString("The record did not publish these files because they are not valid JSON lists; each was restored to the version the base branch holds.\n\n")
	for _, r := range rejected {
		fmt.Fprintf(&b, "- `%s`: %s; %s\n", r.Path, r.Problem, r.keptAs())
	}
	return b.String()
}

// appendNotRecorded appends the "Not recorded" section for rejected to the report at path. Nothing is
// written when nothing was rejected or there is no report.
func appendNotRecorded(path string, rejected []rejectedList) error {
	if len(rejected) == 0 || path == "" {
		return nil
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	section := notRecordedSection(rejected)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		section = "\n" + section
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(section); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
