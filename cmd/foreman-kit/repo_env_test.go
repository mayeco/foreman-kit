package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// repoEnvHelperEnv makes the test binary run main() as `foreman-kit agent-exec` instead of the tests.
const repoEnvHelperEnv = "FOREMANKIT_KIT_REPO_ENV_HELPER"

// repoEnvStub is the agent the helper runs: it records the git variables it was started with.
const repoEnvStub = `#!/bin/bash
env | grep '^GIT_' | sort > "$REPO_ENV_DIR/agent.env"
echo "stub: done"
`

// TestRepoLocalGitEnvDoesNotReachAgents: git exports GIT_DIR, GIT_INDEX_FILE, GIT_WORK_TREE and GIT_PREFIX
// to hooks, "!" aliases and "git rebase --exec". An foreman-kit started from one handed them on to the
// agents and gates it runs, whose own git commands then acted on that repository and its index instead
// of the worktree they were started in. The "git -c" settings, which travel in GIT_CONFIG_*, still pass.
func TestRepoLocalGitEnvDoesNotReachAgents(t *testing.T) {
	if os.Getenv(repoEnvHelperEnv) == "1" {
		dir := os.Getenv("REPO_ENV_DIR")
		os.Args = []string{"foreman-kit", "agent-exec", filepath.Join(dir, "work"), filepath.Join(dir, "prompt.md"), "stub"}
		main() // exits the process
		return
	}

	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	// An agent call does not start on a .foreman-kit/agents.yaml that lacks a key.
	settingstest.WriteFiles(t, work, nil)
	stub := filepath.Join(dir, "stub.sh")
	if err := os.WriteFile(stub, []byte(repoEnvStub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("do the work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hooked := filepath.Join(dir, "hooked")
	var out bytes.Buffer
	kit := exec.Command(os.Args[0], "-test.run=^TestRepoLocalGitEnvDoesNotReachAgents$")
	kit.Dir = work
	kit.Env = append(os.Environ(),
		repoEnvHelperEnv+"=1",
		"REPO_ENV_DIR="+dir,
		"AGENT_STUB="+stub,
		settings.RunOverridesEnv+"="+settings.EncodeOverrides(map[settings.Key]string{settings.AgentRetries: "0"}),
		"FOREMANKIT_STATE_DIR="+filepath.Join(dir, "state"),
		"FOREMANKIT_DB_PATH=",
		"FOREMANKIT_APP_ROOT=",
		// What a commit hook of the repository "hooked" exports.
		"GIT_DIR="+filepath.Join(hooked, ".git"),
		"GIT_INDEX_FILE="+filepath.Join(hooked, ".git", "index"),
		"GIT_WORK_TREE="+hooked,
		"GIT_PREFIX=",
		// What "git -c openspec.probe=kept" exports.
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=openspec.probe",
		"GIT_CONFIG_VALUE_0=kept",
	)
	kit.Stdout = &out
	kit.Stderr = &out
	if err := kit.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- kit.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("foreman-kit agent-exec failed: %v\n%s", err, out.String())
		}
	case <-time.After(90 * time.Second):
		_ = kit.Process.Kill()
		t.Fatalf("foreman-kit agent-exec did not finish\n%s", out.String())
	}

	raw, err := os.ReadFile(filepath.Join(dir, "agent.env"))
	if err != nil {
		t.Fatalf("the agent did not run: %v\n%s", err, out.String())
	}
	seen := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			seen[name] = value
		}
	}
	for _, name := range []string{"GIT_DIR", "GIT_INDEX_FILE", "GIT_WORK_TREE", "GIT_PREFIX"} {
		if value, ok := seen[name]; ok {
			t.Errorf("the agent was started with %s=%q, the repository of the hook that started foreman-kit", name, value)
		}
	}
	if seen["GIT_CONFIG_COUNT"] != "1" || seen["GIT_CONFIG_KEY_0"] != "openspec.probe" || seen["GIT_CONFIG_VALUE_0"] != "kept" {
		t.Errorf("the agent lost the git configuration passed through the environment: %v", seen)
	}
}
