package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const builtinArchiveWritePurpose = "## Purpose\nThis capability governs authentication flows, session tokens, and login behavior across services.\n\n"

// builtinArchiveWriteChange creates a complete change whose delta spec for capability is delta.
func builtinArchiveWriteChange(t *testing.T, root, change, capability, delta string) string {
	t.Helper()
	chgDir := filepath.Join(root, "openspec", "changes", change)
	if err := os.MkdirAll(filepath.Join(chgDir, "specs", capability), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"proposal.md": "# Proposal\n\n## Why\nSessions must expire.\n",
		"design.md":   "# Design\n\n## Goals\n- Expire sessions.\n",
		"tasks.md":    "# Tasks\n\n- [x] 1.1 Expire sessions\n",
		filepath.Join("specs", capability, "spec.md"): delta,
	} {
		if err := os.WriteFile(filepath.Join(chgDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return chgDir
}

// builtinArchiveWriteAssertNotArchived fails when the archive reported success or moved the change away.
func builtinArchiveWriteAssertNotArchived(t *testing.T, root, chgDir string, res OpenSpecResult) {
	t.Helper()
	if res.OK || res.ExitCode == 0 {
		t.Errorf("archive reported success although the canonical spec could not be written: %+v", res)
	}
	if _, err := os.Stat(chgDir); err != nil {
		t.Errorf("the change was archived although its spec was not applied: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "openspec", "changes", "archive")); len(entries) > 0 {
		t.Errorf("openspec/changes/archive holds %d entry(ies), want none", len(entries))
	}
}

// The canonical spec is what an archive is for. When it cannot be written, the archive must fail and
// leave the change where it is: it used to drop the error, move the change to the archive and report
// success, so the merger integrated a change whose requirements never reached openspec/specs/.
func TestBuiltinArchiveWriteFailureOfAnExistingSpecFailsTheArchive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes read-only files")
	}
	root := t.TempDir()
	canonPath := filepath.Join(root, "openspec", "specs", "auth", "spec.md")
	if err := os.MkdirAll(filepath.Dir(canonPath), 0o755); err != nil {
		t.Fatal(err)
	}
	canon := builtinArchiveWritePurpose + "## Requirements\n\n### Requirement: Password login\nThe system SHALL authenticate users with a password.\n\n#### Scenario: Valid credentials\n- **WHEN** a user submits valid credentials\n- **THEN** a session is established\n"
	if err := os.WriteFile(canonPath, []byte(canon), 0o444); err != nil {
		t.Fatal(err)
	}
	delta := "## ADDED Requirements\n\n### Requirement: Session expiry\nThe system SHALL expire a session after 30 minutes.\n\n#### Scenario: Idle session\n- **WHEN** a session is idle for 30 minutes\n- **THEN** the next request returns 401\n"
	chgDir := builtinArchiveWriteChange(t, root, "expire-sessions", "auth", delta)

	res, err := BuiltinOpenSpec(context.Background(), root, "archive", "expire-sessions", "--yes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	builtinArchiveWriteAssertNotArchived(t, root, chgDir, res)
	if got, _ := os.ReadFile(canonPath); string(got) != canon {
		t.Errorf("the read-only canonical spec changed:\n%s", got)
	}
}

// The same for a capability the change introduces: its canonical spec does not exist yet.
func TestBuiltinArchiveWriteFailureOfANewSpecFailsTheArchive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only directories")
	}
	root := t.TempDir()
	canonDir := filepath.Join(root, "openspec", "specs", "billing")
	if err := os.MkdirAll(canonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(canonDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(canonDir, 0o755) })
	delta := builtinArchiveWritePurpose + "## ADDED Requirements\n\n### Requirement: Invoice export\nThe system SHALL export invoices as CSV.\n\n#### Scenario: Valid period\n- **WHEN** GET /invoices/export?period=2026-10\n- **THEN** the response is 200 text/csv\n"
	chgDir := builtinArchiveWriteChange(t, root, "export-invoices", "billing", delta)

	res, err := BuiltinOpenSpec(context.Background(), root, "archive", "export-invoices", "--yes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	builtinArchiveWriteAssertNotArchived(t, root, chgDir, res)
	if !strings.Contains(res.Output, "spec.md") {
		t.Errorf("the archive failure does not name the spec it could not write: %q", res.Output)
	}
}
