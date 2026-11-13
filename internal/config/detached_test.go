package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// The kit ships hermes as detached, without a stop command (it has none for one session), and
// opencode with a private server instead of its background service.
func TestShippedCommandsOfAgentsThatHandTheirWorkToAService(t *testing.T) {
	hermes, _ := ShippedCommands("hermes")
	if !hermes.Detached || hermes.Stop != nil || !hermes.HoldsNoRole() {
		t.Fatalf("hermes = %+v, want detached without a stop command", hermes)
	}
	opencode, _ := ShippedCommands("opencode")
	for _, command := range []any{opencode.Run, opencode.Ping} {
		if !strings.Contains(command.(string), "--standalone") {
			t.Errorf("opencode runs %q, without --standalone", command)
		}
	}
	if opencode.Detached {
		t.Error("opencode runs in process with --standalone: it is not detached")
	}
	for _, name := range SupportedOrder {
		if name == "hermes" {
			continue
		}
		if spec, _ := ShippedCommands(name); spec.Detached {
			t.Errorf("%s is shipped detached; only hermes hands its calls to a service it cannot leave", name)
		}
	}
}

// What update writes for a detached agent reads back the same.
func TestRenderCommandsWritesTheKeysOfADetachedAgent(t *testing.T) {
	spec := AgentCommandSpec{Run: "gw run {{prompt}} {{args}}", Detached: true, Stop: "gw stop {{pid}}", Activity: []string{"~/.gw/state.db", "~/.gw/agent.log"}}
	text := renderCommands(map[string]any{"gw": spec}, []string{"gw"})
	var data map[string]any
	if err := yaml.Unmarshal([]byte(text), &data); err != nil {
		t.Fatalf("not valid YAML: %v\n%s", err, text)
	}
	got, ok := ParseCommandSpec(data["commands"].(map[string]any)["gw"])
	if !ok || !got.Detached || got.Stop != "gw stop {{pid}}" || strings.Join(got.Activity, ",") != "~/.gw/state.db,~/.gw/agent.log" {
		t.Fatalf("read back %+v from:\n%s", got, text)
	}
}

func TestCheckRefusesMisusedKeysOfADetachedAgent(t *testing.T) {
	setting, _ := settings.Lookup(settings.AgentCommands)
	for _, c := range []struct {
		entry map[string]any
		want  string
	}{
		{map[string]any{"run": "gw {{args}}", "stop": "gw stop"}, "only a detached agent"},
		{map[string]any{"run": "gw {{args}}", "activity": []any{"~/.gw/log"}}, "only the calls of a detached agent"},
		{map[string]any{"run": "gw {{args}}", "detached": "yes"}, "must be true or false"},
		{map[string]any{"run": "gw {{args}}", "detached": true, "activity": "~/.gw/log"}, "must be a list of files"},
		{map[string]any{"run": "gw {{args}}", "detached": true, "stop": "gw stop {{prompt}}"}, "not a placeholder of a stop command"},
	} {
		got := checkSettingTypeProblem("", setting, map[string]any{"gw": c.entry})
		if !strings.Contains(got, c.want) {
			t.Errorf("%v: problem %q, want one saying %q", c.entry, got, c.want)
		}
	}
	good := map[string]any{"run": "gw {{args}}", "detached": true, "stop": "gw stop {{workdir}} {{pid}}", "activity": []any{"~/.gw/state.db"}}
	if got := checkSettingTypeProblem("", setting, map[string]any{"gw": good}); got != "" {
		t.Errorf("a well written detached agent is refused: %s", got)
	}
}

// editAgentsCommands rewrites commands.<name> of the application's .foreman-kit/agents.yaml, and its
// build role.
func editAgentsCommands(t *testing.T, root string, build []string, entries map[string]map[string]any) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(AgentsFile))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := yaml.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	commands := data["commands"].(map[string]any)
	for name, entry := range entries {
		commands[name] = entry
	}
	if build != nil {
		data["roles"].(map[string]any)["build"].(map[string]any)["agents"] = build
	}
	out, _ := yaml.Marshal(data)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// An application whose file still holds the commands an earlier kit shipped for hermes and opencode
// gets today's from update: hermes detached, opencode with --standalone. With hermes in a role the
// rewrite still succeeds, and says that the role must change.
func TestUpdateReplacesCommandsAnEarlierKitShipped(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, nil)
	editAgentsCommands(t, root, []string{"hermes"}, map[string]map[string]any{
		"hermes":   {"run": "hermes -z {{prompt}} --yolo --accept-hooks {{args}}", "ping": "hermes -z {{prompt}} {{args}}"},
		"opencode": {"run": "opencode run --format json {{args}} {{prompt}}", "ping": "opencode run {{args}} {{prompt}}"},
	})
	if !ConfigNeedsRewrite(root) {
		t.Fatal("update does not rewrite a file with commands an earlier kit shipped")
	}
	m := applyMigration(t, root)
	notes := strings.Join(m.Notes, "\n")
	for _, want := range []string{"commands.hermes: the commands an earlier foreman-kit shipped", "commands.opencode: the commands an earlier foreman-kit shipped", "REVIEW: roles.build.agents: hermes is detached"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes do not say %q:\n%s", want, notes)
		}
	}
	cfg := Load(root)
	if !cfg.Commands["hermes"].Detached {
		t.Errorf("hermes = %+v, want detached", cfg.Commands["hermes"])
	}
	if !strings.Contains(cfg.Commands["opencode"].Run.(string), "--standalone") {
		t.Errorf("opencode = %+v, want --standalone", cfg.Commands["opencode"])
	}
	// The role is the person's to change: a second update has nothing to rewrite.
	if ConfigNeedsRewrite(root) {
		t.Error("update keeps rewriting a file whose only problem is a role a person must change")
	}
	rep := Check(root)[AgentsFile]
	if !strings.Contains(strings.Join(rep.Errors, "\n"), "roles.build.agents: hermes is detached") {
		t.Errorf("config check does not refuse hermes in the build role: %+v", rep)
	}
	if why := settings.Unusable(root, settings.DispatcherFiles...); !strings.Contains(why, "hermes is detached") {
		t.Errorf("the dispatcher would start work with hermes in the build role: %q", why)
	}
}

// A command a person edited is theirs: update keeps it, even for an agent whose shipped commands
// changed.
func TestUpdateKeepsCommandsAPersonEdited(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, nil)
	edited := "opencode run --format json --model mine {{args}} {{prompt}}"
	editAgentsCommands(t, root, nil, map[string]map[string]any{"opencode": {"run": edited}})
	if ConfigNeedsRewrite(root) {
		applyMigration(t, root)
	}
	if got := Load(root).Commands["opencode"].Run; got != edited {
		t.Fatalf("opencode = %v, want the command the person wrote", got)
	}
}
