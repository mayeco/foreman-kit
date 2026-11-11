package llm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// llmTypeErrorRoot writes .foreman-kit/pipeline.yaml under a new root.
func llmTypeErrorRoot(t *testing.T, pipeline string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "pipeline.yaml"), []byte(pipeline), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// One value the diagnostics: section cannot use (here a word where a time goes) is read as the value the
// kit ships, with a note, and leaves every other value alone. LoadConfig once treated it as an unreadable
// file and kept none of them: `enabled: true` was ignored, and a run that turned the diagnostics on
// fell back to the provider the kit ships, sending the gate output to OpenAI instead of the provider the
// file names.
func TestLoadConfigKeepsTheOtherValuesWhenOneHasTheWrongType(t *testing.T) {
	t.Setenv("FOREMANKIT_DIAGNOSTICS_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	root := llmTypeErrorRoot(t, "repository:\n  base_branch: main\ndiagnostics:\n  enabled: true\n  provider: anthropic\n  model: claude-test\n  timeout: soon\n  retries: \"3\"\n")

	cfg := LoadConfig(root)
	// A quoted number is the number, as for every other key of the file.
	if !cfg.Enabled || cfg.Provider != "anthropic" || cfg.Model != "claude-test" || cfg.MaxRetries != 3 {
		t.Fatalf("the valid diagnostics settings were discarded: enabled=%v provider=%s model=%s retries=%d", cfg.Enabled, cfg.Provider, cfg.Model, cfg.MaxRetries)
	}
	if cfg.APIKey != "sk-ant" {
		t.Fatalf("the client would not use the key of the configured provider")
	}
	if cfg.TimeoutSeconds != 20 {
		t.Fatalf("timeout = %ds, want the 20s the kit ships for a value that is not a time", cfg.TimeoutSeconds)
	}

	settingstest.Give(t, settings.DiagnosticsEnabled, "true")
	if cfg := LoadConfig(root); cfg.Provider != "anthropic" || cfg.APIKey != "sk-ant" {
		t.Fatalf("turned on for one run, the client must still use the configured provider, got %s", cfg.Provider)
	}
}

// A file that is not YAML at all leaves the values the kit ships: diagnostics off.
func TestLoadConfigKeepsTheDefaultsForAFileThatIsNotYAML(t *testing.T) {
	for _, k := range []string{"FOREMANKIT_DIAGNOSTICS_API_KEY", "OPENAI_API_KEY"} {
		t.Setenv(k, "")
	}
	root := llmTypeErrorRoot(t, "diagnostics:\n  enabled: true\n   provider: anthropic\n")
	if cfg := LoadConfig(root); cfg.Enabled || cfg.Provider != "openai" || cfg.Model != "gpt-4o-mini" {
		t.Fatalf("expected the values the kit ships for a file that does not parse, got enabled=%v provider=%s model=%s", cfg.Enabled, cfg.Provider, cfg.Model)
	}
}
