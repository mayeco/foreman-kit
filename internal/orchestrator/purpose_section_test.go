package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

// A "## Purpose" heading with nothing under it used to pass: the text measured ran on into the next
// section (its heading and its requirements). A new capability without a purpose then went through PLAN
// and BUILD, and `openspec validate --specs --strict` rejected its archived spec at merge.
func TestPurposeSectionEmptyBeforeTheNextSectionIsRejected(t *testing.T) {
	root := t.TempDir()
	const change = "add-export"
	specFile := filepath.Join(root, "openspec", "changes", change, "specs", "billing", "spec.md")
	if err := os.MkdirAll(filepath.Dir(specFile), 0o755); err != nil {
		t.Fatal(err)
	}
	requirements := "## ADDED Requirements\n\n### Requirement: Invoice export\nThe system SHALL export invoices as CSV for a billing period.\n\n#### Scenario: Valid period\n- **WHEN** GET /invoices/export?period=2026-10\n- **THEN** the response is 200 text/csv\n"
	for _, tc := range []struct {
		name    string
		purpose string
		valid   bool
	}{
		{"empty", "## Purpose\n\n", false},
		{"empty, next section on the next line", "## Purpose\n", false},
		{"too short", "## Purpose\nExports.\n\n", false},
		{"placeholder", "## Purpose\nTBD\n\n", false},
		{"written", "## Purpose\nGoverns how invoices are exported so finance can reconcile a billing period.\n\n", true},
	} {
		if err := os.WriteFile(specFile, []byte("# billing Specification\n\n"+tc.purpose+requirements), 0o644); err != nil {
			t.Fatal(err)
		}
		if ok, msg := ValidateOpenSpecNative(root, change); ok != tc.valid {
			t.Errorf("%s: ValidateOpenSpecNative = %v (%q), want %v", tc.name, ok, msg, tc.valid)
		}
	}
}
