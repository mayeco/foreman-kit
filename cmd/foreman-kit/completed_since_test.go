package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCompletedSinceRefusesAnUnknownRevision: completed-since <base> [head] behaves like `git log base..head`:
// a base or head that names no commit is an error naming it (exit 1, nothing on stdout), and a valid base
// still lists only the changes integrated after it.
func TestCompletedSinceRefusesAnUnknownRevision(t *testing.T) {
	tmp := t.TempDir()
	appDir := filepath.Join(tmp, "sample-app")
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(tmp, "state"))
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_APP_ROOT", appDir)
	ctx := context.Background()

	run := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errBuf bytes.Buffer
		code := Run(ctx, args, &out, &errBuf)
		return code, out.String(), errBuf.String()
	}
	if code, out, errOut := run("init", appDir, "--agents", "claude"); code != 0 {
		t.Fatalf("init failed (%d): %s %s", code, out, errOut)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = appDir
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	git("add", "-A")
	git("commit", "--quiet", "-m", "chore: init kit")
	for _, name := range []string{"alpha", "beta", "gamma"} {
		git("commit", "--quiet", "--allow-empty", "-m", fmt.Sprintf("feat: %s\n\nOpenSpec-Change: %s", name, name))
	}

	t.Run("a valid base lists only the newer changes", func(t *testing.T) {
		code, out, errOut := run("completed-since", "HEAD~1")
		if code != 0 {
			t.Fatalf("exit %d, stderr=%q", code, errOut)
		}
		if !strings.Contains(out, "gamma") || strings.Contains(out, "alpha") || strings.Contains(out, "beta") {
			t.Fatalf("expected only gamma, got:\n%s", out)
		}
	})

	refused := []struct {
		name string
		args []string
		want string
	}{
		{"mistyped base", []string{"completed-since", "HEAD~1x"}, `"HEAD~1x"`},
		{"base commit that is gone", []string{"completed-since", "0123456789abcdef0123456789abcdef01234567"}, `"0123456789abcdef0123456789abcdef01234567"`},
		{"unknown head", []string{"completed-since", "HEAD~1", "nosuchhead"}, `"nosuchhead"`},
		{"empty base", []string{"completed-since", ""}, "base revision is empty"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := run(tc.args...)
			if code != 1 {
				t.Fatalf("expected exit 1, got %d (stdout=%q stderr=%q)", code, out, errOut)
			}
			if !strings.Contains(errOut, "completed-since: ") || !strings.Contains(errOut, tc.want) {
				t.Fatalf("stderr should start with completed-since and name %s, got %q", tc.want, errOut)
			}
			if out != "" {
				t.Fatalf("expected nothing on stdout, got %q", out)
			}
		})
	}
}
