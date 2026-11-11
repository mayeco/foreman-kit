package llm

import (
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestLoadConfigUsesTheConfiguredProvidersKey verifies that a key is never taken from another vendor's
// variable: with both OPENAI_API_KEY and ANTHROPIC_API_KEY exported, an anthropic client used to send the
// OpenAI secret to Anthropic.
func TestLoadConfigUsesTheConfiguredProvidersKey(t *testing.T) {
	t.Setenv("FOREMANKIT_DIAGNOSTICS_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	settingstest.Give(t, settings.DiagnosticsProvider, "anthropic")
	if got := LoadConfig(t.TempDir()).APIKey; got != "sk-ant" {
		t.Fatalf("anthropic provider got key %q, want the ANTHROPIC_API_KEY", got)
	}
	settingstest.Give(t, settings.DiagnosticsProvider, "compat")
	if got := LoadConfig(t.TempDir()).APIKey; got != "" {
		t.Fatalf("a compat endpoint must not receive another vendor's key, got %q", got)
	}
	t.Setenv("FOREMANKIT_DIAGNOSTICS_API_KEY", "sk-explicit")
	if got := LoadConfig(t.TempDir()).APIKey; got != "sk-explicit" {
		t.Fatalf("FOREMANKIT_DIAGNOSTICS_API_KEY must win, got %q", got)
	}
}
