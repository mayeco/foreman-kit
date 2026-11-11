package cycle

import (
	"bytes"
	"context"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The base branch has no value of the kit's own. A cycle fetches it, rebases onto it and pushes to
// it: an application whose .foreman-kit/pipeline.yaml names none runs no cycle and no visual session,
// instead of working against a branch nobody chose (which the cycle then pushed, creating it on
// origin in an application whose base is another one).
func TestNoCycleWithoutABaseBranch(t *testing.T) {
	for name, yaml := range map[string]string{
		"no file":                 "",
		"the key is not written":  "dispatch:\n  workers: 4\n",
		"the key of an older kit": "base_branch: main\n",
		"a file that is not YAML": "repository:\n  base_branch: main\ndispatch:\n  workers: 4\ndispatch:\n  workers: 2\n",
		"not a branch name":       "repository:\n  base_branch: \"release 2\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if yaml != "" {
				if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "pipeline.yaml"), []byte(yaml), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := ResolveBaseBranch(root); got != "" {
				t.Fatalf("ResolveBaseBranch = %q, want none", got)
			}
			if _, err := RequireBaseBranch(root); err == nil || !strings.Contains(err.Error(), "repository.base_branch in .foreman-kit/pipeline.yaml is missing or not a branch name") ||
				!strings.Contains(err.Error(), "foreman-kit config set pipeline.repository.base_branch <branch>") {
				t.Fatalf("RequireBaseBranch does not say what to set: %v", err)
			}

			judged := false
			var out bytes.Buffer
			err := RunCycle(context.Background(), root, nil, CycleOptions{
				SkipWorktreePrep: true,
				AgentExecFunc: func(context.Context, string, string, string, time.Duration, io.Writer) error {
					judged = true
					return nil
				},
			}, &out)
			if err == nil || judged || !strings.Contains(out.String(), "cycle: repository.base_branch in .foreman-kit/pipeline.yaml is missing") {
				t.Fatalf("a cycle ran without a base branch (judged: %v, err: %v):\n%s", judged, err, out.String())
			}
			out.Reset()
			if err := VisualQABegin(context.Background(), root, nil, VisualQAOptions{SkipWorktreePrep: true}, &out); err == nil ||
				!strings.Contains(out.String(), "visual-qa: repository.base_branch in .foreman-kit/pipeline.yaml is missing") {
				t.Fatalf("a visual session began without a base branch (err: %v):\n%s", err, out.String())
			}
		})
	}

	// The file that names it, and what --set names for one run.
	root := bareApp(t)
	if got, err := RequireBaseBranch(root); err != nil || got != "main" {
		t.Fatalf("RequireBaseBranch = %q, %v", got, err)
	}
	settingstest.Give(t, settings.BaseBranch, "release/2.x")
	if got := ResolveBaseBranch(t.TempDir()); got != "release/2.x" {
		t.Fatalf("the base branch the run was given: %q", got)
	}
}
