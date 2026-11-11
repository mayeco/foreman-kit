package proc

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/memory"
)

// TestAgentExecRefreshesTheSearchOfTheMemoryBeforeTheAgentStarts: agent-memory searches an index
// each checkout keeps for itself, built from the files as they were when it last wrote or searched
// there. A file of the memory that changed since without going through it (what a rebase or a
// fast-forward does) is not found until the index is rebuilt, and nothing says so. AgentExec
// rebuilds it before the agent starts. It runs the agent-memory on PATH.
func TestAgentExecRefreshesTheSearchOfTheMemoryBeforeTheAgentStarts(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	ctx := context.Background()
	if _, err := memory.Init(ctx, root, "demo"); err != nil {
		t.Fatal(err)
	}
	search := func() string {
		cmd := exec.Command(memory.Binary, "fetch", "quokka")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("agent-memory fetch: %v\n%s", err, out)
		}
		return string(out)
	}
	// The checkout searched once: it has an index, of the memory without the lesson.
	_ = search()
	pitfalls := filepath.Join(root, filepath.FromSlash(memory.PitfallsFile))
	raw, err := os.ReadFile(pitfalls)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pitfalls, append(raw, []byte("- **Quokka trap**: never feed it (add-zoo)\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(search(), "Quokka trap") {
		t.Skip("this agent-memory follows a file changed behind its back: there is nothing to refresh")
	}
	index := filepath.Join(root, filepath.FromSlash(memory.IndexFile))
	indexBefore, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	agent := "#!/usr/bin/env bash\ncd \"$FOREMANKIT_WORKDIR\" && agent-memory fetch quokka > found.txt\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(agent), 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	res, err := AgentExec(ctx, AgentExecOptions{
		Workdir: root, AppRoot: root, Prompt: "search the memory", Engines: "claude",
		Store: st, SkipEvent: true, Stdout: &out, Stderr: &errOut,
	})
	if err != nil || res == nil || res.ExitCode != 0 {
		t.Fatalf("the agent call failed: res=%+v err=%v\n%s", res, err, errOut.String())
	}
	found, err := os.ReadFile(filepath.Join(root, "found.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(found), "Quokka trap") {
		t.Errorf("the agent searched the memory as it was before the lesson arrived:\n%s", found)
	}
	if indexAfter, _ := os.ReadFile(index); !bytes.Equal(indexAfter, indexBefore) {
		t.Errorf("the refresh rewrote index.md, a file git tracks:\n%s", indexAfter)
	}
}
