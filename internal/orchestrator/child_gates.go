package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/proc"
	"github.com/mayeco/foreman-kit/internal/store"
)

// childGateConfig is what the gates read from a child repository's repo.config.yaml.
type childGateConfig struct {
	EnvScript string
	// Gates holds the gates of the file and, under "setup", the command that prepares the
	// environment before them (the top-level key setup): it runs first, like a gate, and is recorded
	// as one.
	Gates map[string]string
	// Regression holds the commands of the regression (regression.commands), and RegressionWhen the
	// paths whose change runs them after the gates (regression.when).
	Regression     map[string]string
	RegressionWhen []string
}

// parseChildGateConfig reads raw, a repo.config.yaml. A file that still holds keys an older
// foreman-kit read is refused: those keys said other things, and none is read as one of today.
func parseChildGateConfig(raw []byte) (childGateConfig, error) {
	var data map[string]any
	if err := yaml.Unmarshal(raw, &data); err != nil {
		return childGateConfig{}, err
	}
	if older := config.RepoOfAnOlderKit(data); older != "" {
		return childGateConfig{}, errors.New(older)
	}
	var file struct {
		EnvScript  string            `yaml:"env_script"`
		Setup      string            `yaml:"setup"`
		Gates      map[string]string `yaml:"gates"`
		Regression struct {
			Commands map[string]string `yaml:"commands"`
			When     []string          `yaml:"when"`
		} `yaml:"regression"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return childGateConfig{}, err
	}
	cfg := childGateConfig{EnvScript: file.EnvScript, Gates: file.Gates, Regression: file.Regression.Commands, RegressionWhen: file.Regression.When}
	if strings.TrimSpace(file.Setup) != "" {
		if cfg.Gates == nil {
			cfg.Gates = map[string]string{}
		}
		cfg.Gates["setup"] = file.Setup
	}
	return cfg, nil
}

// envScriptRel returns the env_script the gates source, relative to the repository, or "" when there is
// none or it is still a {{...}} placeholder.
func (c childGateConfig) envScriptRel() string {
	es := strings.TrimSpace(c.EnvScript)
	if es == "" || strings.Contains(es, "{{") {
		return ""
	}
	return es
}

// orderedGateKeys lists the gates in the order they run: setup, lint, typecheck, test and build, then
// the other gates sorted by name.
func (c childGateConfig) orderedGateKeys() []string {
	standardOrder := []string{"setup", "lint", "typecheck", "test", "build"}
	seenKeys := make(map[string]bool)
	var orderedKeys []string
	for _, k := range standardOrder {
		if _, ok := c.Gates[k]; ok {
			seenKeys[k] = true
			orderedKeys = append(orderedKeys, k)
		}
	}
	var extraKeys []string
	for k := range c.Gates {
		if !seenKeys[k] {
			extraKeys = append(extraKeys, k)
		}
	}
	sort.Strings(extraKeys)
	return append(orderedKeys, extraKeys...)
}

// hasRegressionCommands reports whether the repository has a regression command the gates can run.
func (c childGateConfig) hasRegressionCommands() bool {
	for _, v := range c.Regression {
		if v = strings.TrimSpace(v); v != "" && !strings.Contains(v, "{{") {
			return true
		}
	}
	return false
}

// childGateRun is how the gate commands of one child repository run: the same command line and
// environment for the change's gates (DefaultGateRunner) and for checks of the base alone
// (DefaultBaseCheckRunner).
type childGateRun struct {
	Worktree  string // the umbrella worktree; BuildSlotEnv puts its bin/ on PATH
	ChildDir  string
	EnvScript string // env_script relative to ChildDir; "" for none
	Slot      int
	Changed   []string // FOREMANKIT_CHANGED_FILES
	Env       map[string]string
	Timeout   time.Duration // limit of a command when ctx has no deadline
	Store     *store.Store
	// RegressionLockHeld says the caller already holds heavyRegressionMu (sync.Mutex is not reentrant).
	RegressionLockHeld bool
}

// gateRunOutcome is what one gate command did.
type gateRunOutcome struct {
	Output   string
	ExitCode int // 124 when the deadline stopped it
	OK       bool
	Duration time.Duration
	// Started is false when the process could not be created (execve failed, the deadline had already
	// passed): the command said nothing about the code it would have tested.
	Started bool
}

// run executes one gate command through bash, after sourcing env_script, with FOREMANKIT_SLOT and
// FOREMANKIT_CHANGED_FILES set, bounded by proc.RunBounded (a process group, TERM then KILL) until ctx's
// deadline. A regression command (a key starting with "regression:") holds heavyRegressionMu, so two
// regression suites never run at once. It returns the command's output, its exit status (124 when the
// deadline stopped it) and whether it passed.
func (x childGateRun) run(ctx context.Context, gateKey, cmdStr string) (rawOut string, rc int, ok bool, dur time.Duration) {
	o := x.exec(ctx, gateKey, cmdStr)
	return o.Output, o.ExitCode, o.OK, o.Duration
}

// exec is run, reporting also whether the command started at all.
func (x childGateRun) exec(ctx context.Context, gateKey, cmdStr string) gateRunOutcome {
	shellCmd := cmdStr
	if x.EnvScript != "" {
		envScriptPath := filepath.Join(x.ChildDir, filepath.FromSlash(x.EnvScript))
		shellCmd = fmt.Sprintf(". %q && %s", envScriptPath, cmdStr)
	}
	isReg := strings.HasPrefix(gateKey, "regression:")
	if isReg && !x.RegressionLockHeld {
		// Global semaphore so multiple regression suites never saturate PostgreSQL simultaneously
		heavyRegressionMu.Lock()
		defer heavyRegressionMu.Unlock()
	}
	tGate := time.Now()
	baseEnv := append([]string{}, os.Environ()...)
	// OPENSPEC_CHANGED_FILES: the name a script written before the kit was called Foreman reads.
	baseEnv = append(baseEnv, "FOREMANKIT_CHANGED_FILES="+strings.Join(x.Changed, "\n"), "OPENSPEC_CHANGED_FILES="+strings.Join(x.Changed, "\n"))
	for k, v := range x.Env {
		baseEnv = append(baseEnv, k+"="+v)
	}
	env := proc.EnsureUTF8SubprocessEnv(proc.BuildSlotEnv(baseEnv, x.Worktree, x.Slot))
	remTimeout := x.Timeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 {
			remTimeout = rem
		}
	}
	res, runErr := proc.RunBounded(ctx, proc.RunOptions{
		Dir:     x.ChildDir,
		Env:     env,
		Timeout: remTimeout,
		Store:   x.Store,
	}, "bash", "-c", shellCmd)
	// RunBounded returns an error only when the process could not be started.
	o := gateRunOutcome{ExitCode: 1, Duration: time.Since(tGate), Started: runErr == nil}
	if res != nil {
		o.Output, o.ExitCode = res.Output, res.ExitCode
	}
	if runErr != nil && o.Output == "" {
		o.Output = fmt.Sprintf("repo_exec: cannot start the command: %v\n", runErr)
	}
	if ctx.Err() == context.DeadlineExceeded && o.ExitCode != 0 {
		o.ExitCode = 124
	}
	o.OK = runErr == nil && o.ExitCode == 0
	return o
}
