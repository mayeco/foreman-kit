package orchestrator

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// A hotfix that moves a file out of a regression.when path changes that path like a deletion does:
// git diff detects renames by default and lists only the destination, so the outside files must be read
// with --no-renames for the move to trigger the regression on the base alone.
func TestMergeVerifyHotfixMovingAFileOutOfRegressionWhenRunsTheRegression(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.buildCoreChange(t, "core-feature")
	moved := sb.pushCoreChanges(t, map[string]string{"docs/es.yml": "es:\n  saludo: \"Cancion\"\n"}, []string{"config/locales/es.yml"}, "chore(core): move the Spanish translation")

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	sb.requireIntegrated(t, "core-feature")
	runs := sb.coreRuns(t, "regression")
	if len(runs) != 1 || runs[0].head != moved || !strings.Contains(runs[0].files, "config/locales/es.yml") || !strings.Contains(runs[0].files, "docs/es.yml") {
		t.Fatalf("expected one regression on the base alone with both sides of the move, got %+v", runs)
	}
}

// diffNames lists both sides of a rename, unquoted.
func TestDiffNamesListsBothSidesOfARenameUnquoted(t *testing.T) {
	sb := newMCFSandbox(t, "")
	core := sb.tmp + "/rename-core"
	mcfGit(t, sb.tmp, "clone", "-b", "main", sb.childBare["core"], core)
	from := mcfGit(t, core, "rev-parse", "HEAD")
	mcfGit(t, core, "mv", "config/locales/es.yml", "config/locales/español.yml")
	mcfGit(t, core, "commit", "-m", "chore: rename")
	to := mcfGit(t, core, "rev-parse", "HEAD")

	got := diffNames(context.Background(), core, from, to)
	want := []string{"config/locales/es.yml", "config/locales/español.yml"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diffNames = %q, want %q", got, want)
	}
}

// A change's own files reach its gates unquoted: git quotes a non-ASCII path in plain --name-only
// output ("config/locales/caf\303\251.yml", quotes included), which matches no regression.when glob and
// reaches FOREMANKIT_CHANGED_FILES mangled.
func TestMergeGatesSeeTheChangesNonASCIIPathsUnquoted(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.buildCoreChangeWriting(t, "core-locale", map[string]string{"config/locales/café.yml": "fr:\n  saludo: \"Salut\"\n"})

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	sb.requireIntegrated(t, "core-locale")
	runs := sb.coreRuns(t, "regression")
	if len(runs) != 1 || runs[0].files != "config/locales/café.yml" {
		t.Fatalf("expected the merge to run the regression with config/locales/café.yml, got %+v", runs)
	}
}
