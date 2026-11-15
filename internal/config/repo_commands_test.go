package config

import (
	"os"
	"path/filepath"
	"testing"
)

func repoProblemsOfFile(t *testing.T, cfg string) FileProblems {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "repos", "core")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repo.config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return RepoProblems(root, "repos/core")
}

// TestRepoProblemsRefusesACommandThatIsNotText: a gate or a regression command written as a list
// (test: [go, test, ./...]) is an error naming its key, not a usable gate.
func TestRepoProblemsRefusesACommandThatIsNotText(t *testing.T) {
	rep := repoProblemsOfFile(t, "base_branch: main\ngates:\n  lint: \"go vet ./...\"\n  test: [go, test, ./...]\n")
	if !hasProblem(rep.Errors, "gates.test: must be a command (text), not a list") {
		t.Fatalf("a list-valued gate is not an error naming gates.test: %+v", rep)
	}
	if hasProblem(rep.Errors, "gates.lint") {
		t.Fatalf("the string gate gates.lint is flagged: %+v", rep)
	}
	if !hasProblem(rep.Warnings, "no test gate") {
		t.Fatalf("the list-valued gate still counts as the test gate: %+v", rep)
	}

	rep = repoProblemsOfFile(t, "base_branch: main\ngates:\n  test: \"go test ./...\"\nregression:\n  commands:\n    full: [go, test, -count=1, ./...]\n")
	if !hasProblem(rep.Errors, "regression.commands.full: must be a command (text), not a list") {
		t.Fatalf("a list-valued regression command is not an error naming regression.commands.full: %+v", rep)
	}

	rep = repoProblemsOfFile(t, "base_branch: main\ngates:\n  test:\n    run: go test ./...\n")
	if !hasProblem(rep.Errors, "gates.test: must be a command (text), not a section") {
		t.Fatalf("a section-valued gate is not an error naming gates.test: %+v", rep)
	}

	rep = repoProblemsOfFile(t, "base_branch: main\ngates: [go test ./...]\n")
	if !hasProblem(rep.Errors, "gates: must map a name to its command") {
		t.Fatalf("a gates list is not an error: %+v", rep)
	}

	// A regression.commands that is not a section reads as the layout of an older kit: an error too.
	rep = repoProblemsOfFile(t, "base_branch: main\ngates:\n  test: \"go test ./...\"\nregression:\n  commands: [go test ./...]\n")
	if len(rep.Errors) == 0 {
		t.Fatalf("a regression.commands list is not an error: %+v", rep)
	}
}

// TestRepoProblemsAcceptsTextCommands: string gates and regression commands raise no error.
func TestRepoProblemsAcceptsTextCommands(t *testing.T) {
	rep := repoProblemsOfFile(t, "base_branch: main\nsetup: bin/setup\ngates:\n  lint: \"go vet ./...\"\n  test: \"go test ./...\"\nregression:\n  commands:\n    full: \"go test -count=1 ./...\"\n")
	if len(rep.Errors) != 0 {
		t.Fatalf("text commands raise errors: %+v", rep)
	}
}
