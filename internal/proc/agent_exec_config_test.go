package proc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An agent call started by hand is new work: `foreman-kit agent-exec` does not run on a
// .foreman-kit/agents.yaml that lacks a key. It says which key keeps it and runs no agent, instead of
// calling one with a limit nobody wrote.
func TestAgentExecCLIDoesNotStartOnAnAgentsFileThatLacksAKey(t *testing.T) {
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

	agentsYAML := filepath.Join(root, ".foreman-kit", "agents.yaml")
	whole, err := os.ReadFile(agentsYAML)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(whole), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "idle_cpu_percent:") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(agentsYAML, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := RunAgentExecCLI(context.Background(), st, []string{root, prompt, "stub"}, &out, &errOut)
	if code != ExitConfig || !strings.Contains(errOut.String(), "agent_exec: .foreman-kit/agents.yaml: limits.idle_cpu_percent: missing (the kit ships 5)") ||
		!strings.Contains(errOut.String(), "no agent runs until it is mended") {
		t.Fatalf("agent-exec on a .foreman-kit/agents.yaml that lacks a key: code %d\n%s%s", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("an agent ran on a .foreman-kit/agents.yaml that lacks a key")
	}

	// With the file whole again the call runs.
	if err := os.WriteFile(agentsYAML, whole, 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := RunAgentExecCLI(context.Background(), st, []string{root, prompt, "stub"}, &out, &errOut); code != 0 {
		t.Fatalf("agent-exec on whole files: code %d\n%s%s", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(ran); err != nil {
		t.Fatalf("the agent did not run on whole files:\n%s%s", out.String(), errOut.String())
	}
}
