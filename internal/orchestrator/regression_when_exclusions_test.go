package orchestrator

import (
	"strings"
	"testing"
)

// regression.when exclusions hold at BUILD and at merge: a change that only fixes a translation under
// an excluded path never runs the regression, one that edits another path the positive glob names does.
func TestRegressionWhenExclusionsAtBuildAndMerge(t *testing.T) {
	opts := mcfOptions{coreRegressionWhen: []string{"config/**", "!config/locales/**"}}

	t.Run("excluded path runs no regression", func(t *testing.T) {
		sb := newMCFSandboxWith(t, opts)
		sb.buildCoreChangeWriting(t, "core-locales", map[string]string{"config/locales/es.yml": "es:\n  saludo: \"Canción\"\n"})
		_, out := sb.mergerRun(t)
		if got := sb.marker("core.regression"); len(got) != 0 {
			t.Fatalf("a change of an excluded path ran the regression: %v", got)
		}
		if line := mergerLine(t, sb.marker("core.ran")); line != "slot=99 files=config/locales/es.yml" {
			t.Fatalf("core's gates at merge got %q, want the change's translation", line)
		}
		if !strings.Contains(out, "---- [info] repos/core: config/locales/es.yml matches regression.when but !config/locales/** excludes it") {
			t.Fatalf("expected the gate output to name the exclusion, got:\n%s", out)
		}
		sb.requireIntegrated(t, "core-locales")
	})

	t.Run("path outside the exclusion runs the regression", func(t *testing.T) {
		sb := newMCFSandboxWith(t, opts)
		sb.buildCoreChangeWriting(t, "core-initializer", map[string]string{"config/initializers/x.rb": "X = 1\n"})
		_, out := sb.mergerRun(t)
		got := sb.marker("core.regression")
		atBuild, atMerge := 0, 0
		for _, l := range got {
			if l == "slot=99" {
				atMerge++
			} else {
				atBuild++
			}
		}
		if atBuild != 1 || atMerge != 1 {
			t.Fatalf("expected the regression once at BUILD and once at merge, got %v", got)
		}
		if !strings.Contains(out, "repos/core: config/initializers/x.rb matches regression.when") || strings.Contains(out, "excludes it") {
			t.Fatalf("expected the gate output to name the triggering file and no exclusion, got:\n%s", out)
		}
		sb.requireIntegrated(t, "core-initializer")
	})
}
