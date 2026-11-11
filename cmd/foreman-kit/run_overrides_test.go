package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestSetGivesAValueForOneRun: `foreman-kit --set <file>.<key>=<value> <command>` runs the command
// with that value in the place of the one its file holds, and writes no file. A key the catalog does
// not have, or a value the setting does not accept, stops the run before it starts.
func TestSetGivesAValueForOneRun(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, map[string]string{"pipeline": "dispatch:\n  workers: 2\n"})
	t.Setenv("FOREMANKIT_APP_ROOT", root)
	// Restored when the test ends: Run hands the values to the process through this variable.
	t.Setenv(settings.RunOverridesEnv, "")
	before, err := os.ReadFile(root + "/" + settings.Workers.FilePath())
	if err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		code := Run(context.Background(), args, &out, &errOut)
		return code, out.String(), errOut.String()
	}

	if code, out, errOut := run("settings", "get", "dispatch.workers"); code != 0 || strings.TrimSpace(out) != "2" {
		t.Fatalf("without --set: exit %d, %q, %q", code, out, errOut)
	}
	for _, args := range [][]string{
		{"--set", "pipeline.dispatch.workers=7", "settings", "get", "dispatch.workers"},
		{"--set=pipeline.dispatch.workers=7", "settings", "get", "dispatch.workers"},
	} {
		t.Setenv(settings.RunOverridesEnv, "")
		if code, out, errOut := run(args...); code != 0 || strings.TrimSpace(out) != "7" {
			t.Fatalf("%v: exit %d, %q, %q", args, code, out, errOut)
		}
	}
	// What the run started with reaches what it starts: the variable carries it.
	if got := settings.RunOverrides(); len(got) != 1 || got[0] != "pipeline.dispatch.workers=7" {
		t.Fatalf("what the process hands on: %v", got)
	}
	// A second --set of a process started by that run adds to what it was given.
	if code, out, errOut := run("--set", "pipeline.gates.timeout=9m", "settings", "get", "dispatch.workers"); code != 0 || strings.TrimSpace(out) != "7" {
		t.Fatalf("a run started by a run with --set: exit %d, %q, %q", code, out, errOut)
	}
	if code, out, _ := run("settings", "get", "gates.timeout"); code != 0 || strings.TrimSpace(out) != "9m" {
		t.Fatalf("gates.timeout handed on = %q", out)
	}

	t.Setenv(settings.RunOverridesEnv, "")
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--set", "dispatch.workers=7", "settings", "get", "dispatch.workers"}, "--set dispatch.workers: not a setting"},
		{[]string{"--set", "pipeline.dispatch.workers=many", "settings", "get", "dispatch.workers"}, "--set pipeline.dispatch.workers: must be"},
		{[]string{"--set"}, "--set takes <file>.<key>=<value>"},
	} {
		code, out, errOut := run(c.args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "foreman-kit: "+c.says) {
			t.Errorf("%v: exit %d, out %q, err %q", c.args, code, out, errOut)
		}
		if os.Getenv(settings.RunOverridesEnv) != "" {
			t.Errorf("%v: a run that did not start left values behind", c.args)
		}
	}

	// A list is edited in its file: --set gives one value.
	if code, _, errOut := run("--set", "pipeline.repository.protected_paths=[a]", "status"); code != 2 || !strings.Contains(errOut, "it is not one value") {
		t.Errorf("--set of a list: exit %d, %q", code, errOut)
	}

	// The variable is the kit's own: a process that finds in it what --set did not write does not
	// start, instead of running on other values than the ones somebody meant.
	for handed, says := range map[string]string{
		`{"pipeline.dispatch.workers":"many"}`: "FOREMANKIT_SET holds what foreman-kit --set did not write (--set pipeline.dispatch.workers: must be",
		`{"dispatch.workers":"7"}`:             "FOREMANKIT_SET holds what foreman-kit --set did not write (--set dispatch.workers: not a setting",
		"pipeline.dispatch.workers=7":          "FOREMANKIT_SET holds what foreman-kit --set did not write (it is not a list of values)",
	} {
		t.Setenv(settings.RunOverridesEnv, handed)
		code, out, errOut := run("settings", "get", "dispatch.workers")
		if code != 2 || out != "" || !strings.Contains(errOut, "foreman-kit: "+says) {
			t.Errorf("handed %s: exit %d, out %q, err %q", handed, code, out, errOut)
		}
	}
	t.Setenv(settings.RunOverridesEnv, "")

	after, err := os.ReadFile(root + "/" + settings.Workers.FilePath())
	if err != nil || string(after) != string(before) {
		t.Fatalf("--set wrote the file (%v)", err)
	}
}

// TestSetIsAskedOfTheApplicationBeforeTheRun: a value --set gives is checked against its key, and
// then against the application the command runs in. An agent the application does not list stops
// the command before it does anything, with the words `config set` has for it; so does the same
// value handed on by the run that started this process.
func TestSetIsAskedOfTheApplicationBeforeTheRun(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, map[string]string{"pipeline": "dispatch:\n  workers: 2\n"})
	t.Setenv("FOREMANKIT_APP_ROOT", root)
	t.Setenv(settings.RunOverridesEnv, "")
	before, err := os.ReadFile(root + "/" + settings.Agents.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		code := Run(context.Background(), args, &out, &errOut)
		return code, out.String(), errOut.String()
	}

	for value, says := range map[string]string{
		"nonsense":        "foreman-kit: --set agents.roles.build.agents=nonsense: must list agents among ",
		"codex":           "foreman-kit: --set agents.roles.build.agents=codex: codex not among the agents of .foreman-kit/agents.yaml (claude)",
		"[claude, codex]": "foreman-kit: --set agents.roles.build.agents=[claude, codex]: codex not among the agents of .foreman-kit/agents.yaml (claude)",
	} {
		t.Setenv(settings.RunOverridesEnv, "")
		code, out, errOut := run("--set", "agents.roles.build.agents="+value, "settings", "get", "dispatch.workers")
		if code != 2 || out != "" || !strings.Contains(errOut, says) {
			t.Errorf("--set agents.roles.build.agents=%s: exit %d, out %q, err %q", value, code, out, errOut)
		}
	}
	for _, value := range []string{"claude", "[claude]"} {
		t.Setenv(settings.RunOverridesEnv, "")
		if code, out, errOut := run("--set", "agents.roles.build.agents="+value, "settings", "get", "dispatch.workers"); code != 0 || strings.TrimSpace(out) != "2" {
			t.Errorf("--set agents.roles.build.agents=%s: exit %d, out %q, err %q", value, code, out, errOut)
		}
	}

	// A process started by a run that was given such a value does not start either.
	t.Setenv(settings.RunOverridesEnv, `{"agents.roles.build.agents":"codex"}`)
	if code, out, errOut := run("settings", "get", "dispatch.workers"); code != 2 || out != "" || !strings.Contains(errOut, "codex not among the agents of .foreman-kit/agents.yaml (claude)") {
		t.Errorf("handed an agent the application does not list: exit %d, out %q, err %q", code, out, errOut)
	}
	t.Setenv(settings.RunOverridesEnv, "")

	after, err := os.ReadFile(root + "/" + settings.Agents.FilePath())
	if err != nil || string(after) != string(before) {
		t.Fatalf("--set wrote the file (%v)", err)
	}
}

// TestSetIsReadOnlyBeforeTheCommand: an option written after the command is the command's own, so
// `foreman-kit exec backend make --set x=y` hands --set to make. The options of the daemon may
// come before or after it.
func TestSetIsReadOnlyBeforeTheCommand(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	for _, c := range []struct {
		args, rest []string
		given      string
	}{
		{[]string{"exec", "backend", "make", "--set", "x=y"}, []string{"exec", "backend", "make", "--set", "x=y"}, ""},
		{[]string{"--set", "pipeline.dispatch.workers=3", "exec", "backend", "make", "--set", "x=y"}, []string{"exec", "backend", "make", "--set", "x=y"}, "pipeline.dispatch.workers=3"},
		{[]string{"-d", "-w", "--port", "4567", "--set", "pipeline.dispatch.workers=3"}, []string{"-d", "-w", "--port", "4567"}, "pipeline.dispatch.workers=3"},
		{[]string{"--set", "pipeline.dispatch.workers=3", "-d", "--interval=30m"}, []string{"-d", "--interval=30m"}, "pipeline.dispatch.workers=3"},
		{[]string{"status"}, []string{"status"}, ""},
		{nil, []string{}, ""},
	} {
		t.Setenv(settings.RunOverridesEnv, "")
		rest, err := takeRunOverrides(c.args)
		if err != nil || strings.Join(rest, "\x00") != strings.Join(c.rest, "\x00") {
			t.Errorf("%v: left %v (%v), want %v", c.args, rest, err, c.rest)
		}
		if got := strings.Join(settings.RunOverrides(), " "); got != c.given {
			t.Errorf("%v: given %q, want %q", c.args, got, c.given)
		}
	}
}
