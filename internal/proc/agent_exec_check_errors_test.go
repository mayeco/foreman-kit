package proc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// An agent call started by hand does not run on an agents.yaml that `foreman-kit config check`
// rejects, though the catalog finds every key in place: a role naming an agent with no commands.
func TestAgentExecCLIDoesNotStartOnAnAgentsFileThatConfigCheckRejects(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	stub := filepath.Join(t.TempDir(), "stub.sh")
	ran := filepath.Join(t.TempDir(), "ran")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho ran > "+ran+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_STUB", stub)
	prompt := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(prompt, []byte("do the work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	settingstest.WriteFile(t, root, "agents", "roles:\n  plan:\n    agents: [ghost-agent]\n")

	var out, errOut bytes.Buffer
	code := RunAgentExecCLI(context.Background(), st, []string{root, prompt, "stub"}, &out, &errOut)
	if code != ExitConfig || !strings.Contains(errOut.String(), "agent_exec: .foreman-kit/agents.yaml: ") ||
		!strings.Contains(errOut.String(), "ghost-agent") || !strings.Contains(errOut.String(), "no agent runs until it is mended") {
		t.Fatalf("agent-exec on an agents.yaml that config check rejects: code %d\n%s%s", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("an agent ran on an agents.yaml that config check rejects")
	}
}
