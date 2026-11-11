package proc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestRepoExecCLIKeepsPipedStdinFromTheEnvScript: the env_script of a repository is sourced in the shell
// that then runs the command, so it shared the stdin `foreman-kit exec` hands on. An env_script that
// reads its input (a `read`, a tool that takes a token on stdin) took the first lines of
// `foreman-kit exec backend psql < seed.sql` for itself, and the command got the rest with no error.
func TestRepoExecCLIKeepsPipedStdinFromTheEnvScript(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	repo := filepath.Join(root, "repos", "greedy")
	if err := os.MkdirAll(filepath.Join(repo, "script"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The env_script reads a line, and what it exports must still reach the command.
	envScript := "read -r swallowed\nexport FROM_ENV=\"env_script read: [$swallowed]\"\n"
	if err := os.WriteFile(filepath.Join(repo, "script", "env.sh"), []byte(envScript), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "repo.config.yaml"), []byte("env_script: script/env.sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("first line\nsecond line\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	defer r.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	var out, errOut bytes.Buffer
	code := RunRepoExecCLI(context.Background(), root, st, []string{"greedy", "sh", "-c", `cat; printf '%s\n' "$FROM_ENV"`}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if want := "first line\nsecond line\nenv_script read: []\n"; out.String() != want {
		t.Fatalf("the command did not get the whole piped stdin:\n got %q\nwant %q", out.String(), want)
	}
}
