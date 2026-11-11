package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestParseChildGateConfigReadsTheKeysOfToday: the gates of a child repository are read from the
// keys repo.config.yaml has today: setup, which runs first like a gate; gates; and the regression
// with its commands and the paths that run them. A file that still holds a key an older foreman-kit
// read is refused whole, because the same words said other things there.
func TestParseChildGateConfigReadsTheKeysOfToday(t *testing.T) {
	cfg, err := parseChildGateConfig([]byte("env_script: script/env.sh\nsetup: bin/setup\ngates:\n  test: bin/test\nregression:\n  commands:\n    full: bin/full\n  when: [\"app/**\", \"!app/assets/**\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnvScript != "script/env.sh" || len(cfg.Gates) != 2 || cfg.Gates["setup"] != "bin/setup" || cfg.Gates["test"] != "bin/test" {
		t.Errorf("gates: %+v", cfg)
	}
	if len(cfg.Regression) != 1 || cfg.Regression["full"] != "bin/full" || strings.Join(cfg.RegressionWhen, " ") != "app/** !app/assets/**" || !cfg.hasRegressionCommands() {
		t.Errorf("regression: %+v", cfg)
	}

	// No setup and no regression: nothing stands in for either.
	cfg, err = parseChildGateConfig([]byte("gates:\n  test: bin/test\n"))
	if err != nil || len(cfg.Gates) != 1 || cfg.hasRegressionCommands() || len(cfg.RegressionWhen) != 0 {
		t.Errorf("a file with gates alone: %+v (%v)", cfg, err)
	}

	for former, names := range map[string]string{
		"gates:\n  setup: bin/setup\n  test: bin/test\n":       "gates.setup",
		"gates:\n  test: bin/test\nregression:\n  full: x\n":   "regression",
		"gates:\n  test: bin/test\nregression_when: [\"a\"]\n": "regression_when",
		"requires: {}\nservice:\n  prepare: bin/prepare\n":     "service.prepare, requires",
	} {
		_, err := parseChildGateConfig([]byte(former))
		if err == nil || !strings.Contains(err.Error(), "holds keys an older foreman-kit read ("+names+"): foreman-kit update rewrites it") {
			t.Errorf("%q: %v", former, err)
		}
	}
}

// TestDispatcherAndMergerWaitForARepoConfigOfAnOlderKit: while the repo.config.yaml of a repository
// of the main checkout holds keys an older foreman-kit read, its gates cannot run as it says: the
// dispatcher and the merger start nothing, and say which file and what rewrites it.
func TestDispatcherAndMergerWaitForARepoConfigOfAnOlderKit(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, nil)
	write := func(content string) {
		t.Helper()
		dir := filepath.Join(root, "repos", "web")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "repo.config.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("base_branch: main\ngates:\n  setup: bin/setup\n  test: bin/test\n")
	want := "repos/web/repo.config.yaml holds keys an older foreman-kit read (gates.setup): foreman-kit update rewrites it"
	cfg := LoadPipelineSettings(root)
	if got := cfg.DispatcherWaits(); got != want+": no change starts until it is mended" {
		t.Fatalf("the dispatcher says %q", got)
	}
	if got := cfg.MergerWaits(); got != want+": nothing is integrated until it is mended" {
		t.Fatalf("the merger says %q", got)
	}

	write("base_branch: main\nsetup: bin/setup\ngates:\n  test: bin/test\n")
	if cfg := LoadPipelineSettings(root); cfg.DispatcherWaits() != "" || cfg.MergerWaits() != "" {
		t.Fatalf("with the file of today: dispatcher %q, merger %q", cfg.DispatcherWaits(), cfg.MergerWaits())
	}
}
