package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
)

// seedApp writes, in a new directory, the seven files of today as the kit seeds them, with the
// text of each file edited by edit (file name => old text, new text pairs).
func seedApp(t *testing.T, edits map[string][2]string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range settings.Files() {
		text, err := RenderSeed(f.Name, nil)
		if err != nil {
			t.Fatal(err)
		}
		if pair, ok := edits[f.Name]; ok {
			if !strings.Contains(string(text), pair[0]) {
				t.Fatalf("the seed of %s does not hold %q", f.Path, pair[0])
			}
			text = []byte(strings.Replace(string(text), pair[0], pair[1], 1))
		}
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, text, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A key a person misspelled is not dropped without a word: the note names it, its value and the
// setting it may have meant, with what was written there.
func TestMigrateConfigNamesAKeyItDrops(t *testing.T) {
	root := seedApp(t, map[string][2]string{"pipeline": {"gates:\n  timeout: 45m", "gates:\n  timout: 30m"}})
	m := applyMigration(t, root)
	notes := strings.Join(m.Notes, "\n")
	if !hasLine(m.Notes, "gates.timout", "REVIEW", "30m is not a setting of this file and was dropped", "did you mean gates.timeout?", "the kit's value 45m was written") {
		t.Errorf("the notes do not name gates.timout and its value:\n%s", notes)
	}
	written, err := os.ReadFile(filepath.Join(root, ".foreman-kit", "pipeline.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "timout") || !strings.Contains(string(written), "timeout: 45m") {
		t.Errorf("the file written:\n%s", written)
	}
	if again, err := MigrateConfig(root); err != nil || again.Pending() || len(again.Notes) > 0 {
		t.Errorf("a second migration: %+v, %v", again, err)
	}
}

// A key close to no setting is named as well, with no suggestion.
func TestMigrateConfigNamesAKeyNothingResembles(t *testing.T) {
	root := seedApp(t, map[string][2]string{"pipeline": {"gates:\n  timeout: 45m", "mybot_flag: true\ngates:\n  timeout: 45m"}})
	m := applyMigration(t, root)
	if !hasLine(m.Notes, "mybot_flag: REVIEW", "true is not a setting of this file and was dropped") {
		t.Errorf("the notes do not name mybot_flag:\n%s", strings.Join(m.Notes, "\n"))
	}
	for _, note := range m.Notes {
		if strings.Contains(note, "mybot_flag") && strings.Contains(note, "did you mean") {
			t.Errorf("a key near nothing has a suggestion: %s", note)
		}
	}
}

// A file of valid keys, the content of a file and the keys a former kit read have no dropped key
// to name: the notes do not say "not a setting of this file" of them.
func TestMigrateConfigDoesNotNameWhatItKeeps(t *testing.T) {
	// Valid keys only: the file lacks one key, so it is rewritten, and no key is dropped.
	root := seedApp(t, map[string][2]string{"daemon": {"  port: 4567 # the next free one when it is taken, unless --port names one\n", ""}})
	m := applyMigration(t, root)
	if len(m.Changed) == 0 {
		t.Fatal("the file lacking a key was not rewritten")
	}
	for _, note := range m.Notes {
		if strings.Contains(note, "not a setting of this file") {
			t.Errorf("a file of valid keys has a dropped key: %s", note)
		}
	}

	// The gate of the application's own, and the former keys of an old kit, keep their notes and
	// are not reported as dropped.
	old := formerApp(t, map[string][]string{"evaluator": {"  failure_triage:\n", "  failure_triage_was_here:\n"}})
	om := applyMigration(t, old)
	for _, note := range om.Notes {
		if strings.Contains(note, "not a setting of this file") {
			t.Errorf("content or a former key is reported as dropped: %s", note)
		}
	}
}
