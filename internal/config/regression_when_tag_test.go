package config

import (
	"testing"
)

// An unquoted exclusion in a block list is a YAML tag: config check names it and says to quote it.
func TestUnquotedRegressionWhenExclusionIsReported(t *testing.T) {
	raw := []byte("regression:\n  when:\n    - config/**\n    - !config/locales/**\n    - \"!db/seeds/**\"\n")
	got := unquotedExclusions(raw)
	if len(got) != 1 || got[0] != "!config/locales/**" {
		t.Fatalf("unquotedExclusions = %q", got)
	}
	if got := unquotedExclusions([]byte("regression:\n  when: [\"config/**\", \"!config/locales/**\"]\n")); len(got) != 0 {
		t.Fatalf("quoted exclusions reported: %q", got)
	}
}
