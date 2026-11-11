package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The preview Jev gets of a path a change touched outside its allowed ones is that file's diff against the
// fork. The path was handed to git diff as a pathspec, where "pages/[id].go" is also a pattern: the diff
// of pages/i.go came with it, and the judge read another file's edit as the stray path's.
func TestOutsidePathStatePreviewsOnlyThePathItNames(t *testing.T) {
	ctx := context.Background()
	dir, git := newTestRepo(t)
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pages/[id].go", "route\n")
	write("pages/i.go", "index\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	fork := git("rev-parse", "HEAD")

	write("pages/[id].go", "route, edited by the agent\n")
	write("pages/i.go", "index, edited as well\n")

	states := outsidePathState(ctx, dir, fork, []string{"pages/[id].go"})
	if len(states) != 1 || states[0]["status"] != "modified" {
		t.Fatalf("want one modified path, got %+v", states)
	}
	preview, _ := states[0]["preview"].(string)
	if !strings.Contains(preview, "+route, edited by the agent") {
		t.Fatalf("the preview lacks the path's own edit:\n%s", preview)
	}
	if strings.Contains(preview, "pages/i.go") || strings.Contains(preview, "index, edited as well") {
		t.Fatalf("the preview of pages/[id].go carries the diff of another file:\n%s", preview)
	}
}
