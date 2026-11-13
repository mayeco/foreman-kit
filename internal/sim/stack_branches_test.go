package sim

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/gitx"
)

func TestInitStackKeepsItsBranchNames(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	st, err := InitStack(ctx, t.TempDir(), &out)
	if err != nil {
		t.Fatalf("InitStack: %v\noutput:\n%s", err, out.String())
	}
	for remote, want := range map[string]string{
		"backend.git":  "refs/heads/main",
		"umbrella.git": "refs/heads/master",
	} {
		head, err := gitx.RunGit(ctx, filepath.Join(st.RemotesDir, remote), "symbolic-ref", "HEAD")
		if err != nil {
			t.Fatalf("symbolic-ref HEAD of %s: %v", remote, err)
		}
		if got := strings.TrimSpace(head); got != want {
			t.Fatalf("%s HEAD = %q, want %q", remote, got, want)
		}
	}
	if branch, ok := gitx.CurrentBranch(ctx, st.WorkspaceDir); !ok || branch != "master" {
		t.Fatalf("workspace branch = %q (ok=%v), want master", branch, ok)
	}
}
