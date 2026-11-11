package settings_test

import (
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestAFormerVariableKeepsWhatReadItsFileFromStarting: a variable an older foreman-kit read to
// override a key changes nothing any more. Whoever still exports AGENT_PERMISSIONS=restricted, or
// JEV_DISABLED=1, expects it to hold, so it is not ignored in silence: the processes that read the
// file of its key start nothing, and say which variable, what does its job now, and how it leaves
// the environment (a shell exports it, or the .env file writes it).
// A process that does not read that file is not kept.
func TestAFormerVariableKeepsWhatReadItsFileFromStarting(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, nil)
	for _, files := range [][]string{settings.DispatcherFiles, settings.MergerFiles, settings.CycleFiles, settings.AgentCallFiles, settings.DaemonFiles} {
		if why := settings.Unusable(root, files...); why != "" {
			t.Fatalf("on whole files and a clean environment, %v wait: %s", files, why)
		}
	}
	if set := settings.FormerVariablesSet(); len(set) != 0 {
		t.Fatalf("former variables set in a clean environment: %v", set)
	}

	t.Setenv("AGENT_PERMISSIONS", "restricted")
	want := "the environment: AGENT_PERMISSIONS=restricted is set and no longer read: write permissions in .foreman-kit/agents.yaml, or give it to one run with --set agents.permissions=<value>, and take it out of the environment (unset it, or delete its line from .env)"
	for _, files := range [][]string{settings.DispatcherFiles, settings.CycleFiles, settings.AgentCallFiles} {
		if why := settings.Unusable(root, files...); !strings.Contains(why, want) {
			t.Errorf("%v with AGENT_PERMISSIONS set: %q", files, why)
		}
	}
	// The merger calls no agent, and the daemon reads its own file: neither is kept.
	for _, files := range [][]string{settings.MergerFiles, settings.DaemonFiles} {
		if why := settings.Unusable(root, files...); why != "" {
			t.Errorf("%v wait for a variable of a file they do not read: %s", files, why)
		}
	}
	// The file itself is whole: it is the environment that is not.
	if problems := settings.At(root).Problems("agents"); len(problems) != 0 {
		t.Errorf("the file was blamed for the variable: %v", problems)
	}
	if why := settings.WrittenAt(root).Unusable(settings.AgentCallFiles...); why != "" {
		t.Errorf("the files alone: %s", why)
	}
	t.Setenv("AGENT_PERMISSIONS", "")

	t.Setenv("JEV_DISABLED", "1")
	want = "the environment: JEV_DISABLED=1 is set and no longer read: write enabled: false in .foreman-kit/evaluator.yaml, or give it to one run with --set evaluator.enabled=false, and take it out of the environment (unset it, or delete its line from .env)"
	for _, files := range [][]string{settings.DispatcherFiles, settings.MergerFiles, settings.CycleFiles} {
		if why := settings.Unusable(root, files...); !strings.Contains(why, want) {
			t.Errorf("%v with JEV_DISABLED set: %q", files, why)
		}
	}
	if why := settings.Unusable(root, settings.AgentCallFiles...); why != "" {
		t.Errorf("an agent call waits for a variable of the evaluator: %s", why)
	}
	t.Setenv("JEV_DISABLED", "")

	// A key that is not one value takes no --set: the variable that overrode it names its file.
	t.Setenv("OPENSPEC_PROTECTED_PATHS", "a,b")
	set := settings.FormerVariablesSet()
	if len(set) != 1 || !strings.HasSuffix(set[0].Text, "OPENSPEC_PROTECTED_PATHS=a,b is set and no longer read: write repository.protected_paths in .foreman-kit/pipeline.yaml, and take it out of the environment (unset it, or delete its line from .env)") {
		t.Fatalf("FormerVariablesSet() = %v", set)
	}
	t.Setenv("OPENSPEC_PROTECTED_PATHS", "")

	// A variable that held a key is named, never shown.
	t.Setenv("OPENSPEC_LLM_API_KEY", "sk-very-secret")
	set = settings.FormerVariablesSet()
	if len(set) != 1 || !strings.Contains(set[0].Text, "OPENSPEC_LLM_API_KEY is set and no longer read") || !strings.Contains(set[0].Text, "FOREMANKIT_DIAGNOSTICS_API_KEY") {
		t.Fatalf("FormerVariablesSet() = %v", set)
	}
	if why := settings.Unusable(root, settings.DispatcherFiles...); strings.Contains(why, "sk-very-secret") || !strings.Contains(why, "OPENSPEC_LLM_API_KEY is set") {
		t.Errorf("the dispatcher with the former key variable: %q", why)
	}

	t.Setenv("OPENSPEC_LLM_API_KEY", "")

	// A list of tools that restricted an agent from the environment restricts nothing now that
	// each command is written whole: a call to an agent does not start as if it still did.
	t.Setenv("CLAUDE_DISALLOWED_TOOLS", "Bash(rm *)")
	want = "the environment: CLAUDE_DISALLOWED_TOOLS=Bash(rm *) is set and no longer read: the tools an agent may use are written in its own command: edit commands in .foreman-kit/agents.yaml, and take it out of the environment (unset it, or delete its line from .env) (foreman-kit config check)"
	for _, files := range [][]string{settings.AgentCallFiles, settings.DispatcherFiles, settings.CycleFiles} {
		if why := settings.Unusable(root, files...); why != want {
			t.Errorf("%v with CLAUDE_DISALLOWED_TOOLS set: %q", files, why)
		}
	}
	if why := settings.Unusable(root, settings.MergerFiles...); why != "" {
		t.Errorf("the merger calls no agent and waits on a list of tools: %s", why)
	}
	t.Setenv("CLAUDE_DISALLOWED_TOOLS", "")

	// The other name the key of the evaluator had: with it alone the evaluator has no key.
	t.Setenv("JEV_API_KEY", "ts-very-secret")
	if why := settings.Unusable(root, settings.MergerFiles...); strings.Contains(why, "ts-very-secret") ||
		!strings.Contains(why, "JEV_API_KEY is set and no longer read: the key of the evaluator is read from TYPESAFE_API_KEY") {
		t.Errorf("the merger with the former name of the evaluator's key: %q", why)
	}
	t.Setenv("JEV_API_KEY", "")

	// A variable that was no setting keeps no process from starting, and is still said.
	t.Setenv("OPENSPEC_RECORD_LOCK_SECONDS", "5")
	set = settings.FormerVariablesSet()
	if len(set) != 1 || !strings.Contains(set[0].Text, "OPENSPEC_RECORD_LOCK_SECONDS=5 is set and no longer read: how long a process waits for the lock") {
		t.Fatalf("FormerVariablesSet() = %v", set)
	}
	for _, files := range [][]string{settings.DispatcherFiles, settings.MergerFiles, settings.CycleFiles, settings.AgentCallFiles, settings.DaemonFiles} {
		if why := settings.Unusable(root, files...); why != "" {
			t.Errorf("%v wait on a variable that was no setting: %s", files, why)
		}
	}
	t.Setenv("OPENSPEC_RECORD_LOCK_SECONDS", "")

	// Every former variable says what does its job now, and the keys it names are settings.
	for _, v := range settings.FormerVariables() {
		if (len(v.Keys) == 0 && v.Now == "") || v.Instead("x") == "" {
			t.Errorf("%s says nothing about what does its job now", v.Name)
		}
		for _, key := range v.Keys {
			if _, known := settings.Lookup(key); !known {
				t.Errorf("%s names %s, which is not a setting", v.Name, key)
			}
		}
	}
}
