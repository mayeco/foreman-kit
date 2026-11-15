package orchestrator

import (
	"strings"
	"testing"
)

// TestParseChildGateConfigNamesACommandThatIsNotText: a gate, a regression command or the setup
// written as a list is refused with the key named (the same words as foreman-kit config check and
// foreman-kit gates), not with the YAML decoder's "cannot unmarshal !!seq into string".
func TestParseChildGateConfigNamesACommandThatIsNotText(t *testing.T) {
	for raw, want := range map[string]string{
		"gates:\n  lint: \"go vet ./...\"\n  test: [go, test, ./...]\n":                     "gates.test: must be a command (text), not a list",
		"gates:\n  test: bin/test\nregression:\n  commands:\n    full: [go, test, ./...]\n": "regression.commands.full: must be a command (text), not a list",
		"setup: [bin/setup]\ngates:\n  test: bin/test\n":                                    "setup: must be a command (text)",
	} {
		_, err := parseChildGateConfig([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "cannot unmarshal") {
			t.Errorf("%q: %v, want %q", raw, err, want)
		}
	}
}
