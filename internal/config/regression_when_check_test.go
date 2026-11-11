package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repoProblemsFor(t *testing.T, regressionWhen string) FileProblems {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "repos", "core")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "base_branch: main\ngates:\n  test: \"go test ./...\"\nregression:\n  commands:\n    full: \"go test -count=1 ./...\"\n  when: " + regressionWhen + "\n"
	if err := os.WriteFile(filepath.Join(dir, "repo.config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return RepoProblems(root, "repos/core")
}

func hasProblem(list []string, sub string) bool {
	for _, p := range list {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func TestRepoProblemsRegressionWhenExclusions(t *testing.T) {
	rep := repoProblemsFor(t, `["config/**", "!config/locales/**"]`)
	if len(rep.Errors) > 0 || hasProblem(rep.Warnings, "regression.when") {
		t.Fatalf("a positive entry with an exclusion should check clean, got %+v", rep)
	}

	for _, bad := range []string{`["config/**", "!"]`, `["config/**", "!!config/locales/**"]`, `["config/**", "! "]`} {
		rep = repoProblemsFor(t, bad)
		if !hasProblem(rep.Errors, "is not an exclusion") {
			t.Fatalf("regression.when %s: expected an error, got %+v", bad, rep)
		}
	}

	rep = repoProblemsFor(t, `["!config/locales/**", "!docs/**"]`)
	if len(rep.Errors) > 0 {
		t.Fatalf("only exclusions is not an error, got %+v", rep)
	}
	if !hasProblem(rep.Warnings, "regression.when has only exclusions: nothing ever triggers the regression") {
		t.Fatalf("expected the only-exclusions warning, got %+v", rep)
	}
}
