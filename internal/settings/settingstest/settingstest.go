// Package settingstest writes the configuration files of an application for a test.
//
// A process of the kit starts no new work on a file that lacks a key (settings.Reader.Unusable), so
// a test that runs a dispatcher, a merger, a cycle or a daemon gives its application the seven files
// whole: the ones the kit ships, with the values the test needs different written over them.
package settingstest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/settings"
)

// WriteFiles writes every configuration file of the application at root as the kit ships it.
// overrides maps the name of a file ("pipeline") to YAML holding the keys the test wants different:
// each one replaces the value the kit ships for its key, and a key the YAML does not write keeps it.
// A file the application already has is written again from the seed: what it held is lost.
func WriteFiles(t testing.TB, root string, overrides map[string]string) {
	t.Helper()
	// A variable an older foreman-kit read keeps a process from starting when it is set: one left
	// in the environment the tests run in is not the test's business.
	for _, v := range settings.FormerVariables() {
		if os.Getenv(v.Name) != "" {
			t.Setenv(v.Name, "")
		}
	}
	for name := range overrides {
		if _, ok := settings.FileNamed(name); !ok {
			t.Fatalf("settingstest: %q is not a configuration file", name)
		}
	}
	for _, f := range settings.Files() {
		WriteFile(t, root, f.Name, overrides[f.Name])
	}
}

// WriteFile writes the configuration file called name of the application at root as the kit ships
// it, with the keys that override (YAML, "" for none) writes in place of the shipped values.
func WriteFile(t testing.TB, root, name, override string) {
	t.Helper()
	f, ok := settings.FileNamed(name)
	if !ok {
		t.Fatalf("settingstest: %q is not a configuration file", name)
	}
	raw, err := assets.ReadSeed(f.Path)
	if err != nil {
		t.Fatalf("settingstest: %s has no seed: %v", f.Path, err)
	}
	var data map[string]any
	if err := yaml.Unmarshal(raw, &data); err != nil {
		t.Fatalf("settingstest: the seed of %s is not valid YAML: %v", f.Path, err)
	}
	if override != "" {
		var over map[string]any
		if err := yaml.Unmarshal([]byte(override), &over); err != nil {
			t.Fatalf("settingstest: the override of %s is not valid YAML: %v", f.Path, err)
		}
		merge(data, over)
	}
	out, err := yaml.Marshal(data)
	if err != nil {
		t.Fatalf("settingstest: %s: %v", f.Path, err)
	}
	path := filepath.Join(root, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("settingstest: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("settingstest: %v", err)
	}
}

// WriteFormerFiles writes in root the configuration files an older foreman-kit wrote: its five
// files with the keys it read, kept as internal/config/testdata/former, and neither daemon.yaml
// nor visual_qa.yaml, which it did not have. It is what a worktree cut before `foreman-kit update`
// holds, and what an application nobody has updated holds.
func WriteFormerFiles(t testing.TB, root string) {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("settingstest: cannot find its own source file")
	}
	former := filepath.Join(filepath.Dir(here), "..", "..", "config", "testdata", "former", "config")
	entries, err := os.ReadDir(former)
	if err != nil {
		t.Fatalf("settingstest: %v", err)
	}
	dir := filepath.Join(root, ".foreman-kit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("settingstest: %v", err)
	}
	written := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(former, e.Name()))
		if err != nil {
			t.Fatalf("settingstest: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), raw, 0o644); err != nil {
			t.Fatalf("settingstest: %v", err)
		}
		written[e.Name()] = true
	}
	for _, f := range settings.Files() {
		if name := filepath.Base(f.Path); !written[name] {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				t.Fatalf("settingstest: %v", err)
			}
		}
	}
}

// Give gives the test's run a value for key, the way `foreman-kit --set <key>=<text>` gives one
// to a run: every reader of the test, and every process it starts, reads it in the place of what
// the file holds, until the test ends. A value the setting does not accept fails the test.
func Give(t testing.TB, key settings.Key, text string) {
	t.Helper()
	checked, value, err := settings.ParseOverride(string(key) + "=" + text)
	if err != nil {
		t.Fatalf("settingstest: %v", err)
	}
	given := settings.DecodeOverrides(os.Getenv(settings.RunOverridesEnv))
	if given == nil {
		given = map[settings.Key]string{}
	}
	given[checked] = value
	t.Setenv(settings.RunOverridesEnv, settings.EncodeOverrides(given))
}

// GiveAgents gives the test's run the agents of every role, in order: what a test that runs one
// stand-in agent for everything needs.
func GiveAgents(t testing.TB, agents ...string) {
	t.Helper()
	for _, role := range settings.Roles {
		Give(t, settings.RoleAgents(role), "["+strings.Join(agents, ", ")+"]")
	}
}

// Take takes back the value the test's run was given for key: its file is read again.
func Take(t testing.TB, key settings.Key) {
	t.Helper()
	given := settings.DecodeOverrides(os.Getenv(settings.RunOverridesEnv))
	delete(given, key)
	t.Setenv(settings.RunOverridesEnv, settings.EncodeOverrides(given))
}

// merge writes the keys of over into data: a section is merged key by key, anything else replaces
// what data held.
func merge(data, over map[string]any) {
	for key, value := range over {
		section, isSection := value.(map[string]any)
		held, holdsSection := data[key].(map[string]any)
		if isSection && holdsSection {
			merge(held, section)
			continue
		}
		data[key] = value
	}
}
