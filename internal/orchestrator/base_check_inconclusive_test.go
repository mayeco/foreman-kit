package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

func baseCheckChild(t *testing.T, cfg string) string {
	t.Helper()
	wt := t.TempDir()
	child := filepath.Join(wt, "repos", "core")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "repo.config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return wt
}

// Waiting for another regression suite of the process (a worker's BUILD verification in the daemon)
// must not count against the base check's time limit.
func TestBaseCheckWaitForTheRegressionLockIsNotCountedAgainstTheCheck(t *testing.T) {
	wt := baseCheckChild(t, "setup: \"true\"\nregression:\n  commands:\n    full: \"sleep 0.3; echo regression-ran\"\n")
	heavyRegressionMu.Lock()
	released := make(chan struct{})
	go func() {
		time.Sleep(1500 * time.Millisecond)
		heavyRegressionMu.Unlock()
		close(released)
	}()
	res, err := DefaultBaseCheckRunner(nil)(context.Background(), BaseCheckRequest{
		Change: "x", Worktree: wt, Child: "repos/core", Check: BaseCheckRegression, Slot: MergerSlot, Timeout: time.Second,
	})
	<-released
	if err != nil || !res.OK || !strings.Contains(res.Output, "regression-ran\n") {
		t.Fatalf("want the regression to run and pass after the wait, got err=%v %+v", err, res)
	}
}

// A command that cannot start (here, an environment larger than execve accepts) says nothing about the
// base: the result is Inconclusive and says why.
func TestBaseCheckThatCannotStartIsInconclusive(t *testing.T) {
	wt := baseCheckChild(t, "setup: \"true\"\nregression:\n  commands:\n    full: \"true\"\n")
	files := make([]string, 0, 60000)
	for i := 0; i < 60000; i++ {
		files = append(files, "app/assets/vendor/some/deep/path/file_"+strings.Repeat("x", 20)+"_"+strconv.Itoa(i)+".js")
	}
	res, err := DefaultBaseCheckRunner(nil)(context.Background(), BaseCheckRequest{
		Change: "x", Worktree: wt, Child: "repos/core", Check: BaseCheckRegression, Slot: MergerSlot, Timeout: 10 * time.Second,
		ChangedFiles: files,
	})
	if err != nil || res.OK || !res.Inconclusive || !strings.Contains(res.Output, "cannot start the command") {
		t.Fatalf("want an inconclusive check that names the start failure, got err=%v ok=%v inconclusive=%v output=%q", err, res.OK, res.Inconclusive, res.Output)
	}
}

// End to end: a harmless hotfix whose base regression waits for a worker's suite longer than
// gates.timeout is not declared broken, and the change integrates.
func TestMergeVerifyQueuedBaseRegressionDoesNotBreakTheBase(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.buildCoreChange(t, "core-feature")
	hotfix := sb.pushCoreHotfix(t)
	settingstest.Give(t, settings.GateTimeout, "2s")

	heavyRegressionMu.Lock() // a worker's BUILD verification running its regression in the daemon
	released := make(chan struct{})
	go func() {
		time.Sleep(3 * time.Second)
		heavyRegressionMu.Unlock()
		close(released)
	}()
	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	<-released

	if v := sb.verdict(t, hotfix, BaseCheckRegression); v != nil && v.Status == store.BaseVerdictFailed {
		t.Fatalf("a regression that waited for the lock was cached as failed: %+v", v)
	}
	if evs := sb.events(t, "merge.base_broken"); len(evs) != 0 {
		t.Fatalf("unexpected merge.base_broken: %+v", evs)
	}
	sb.requireIntegrated(t, "core-feature")
}

// A base check that could not run records no verdict: the change goes on with its own tests.
func TestMergeVerifyInconclusiveBaseCheckRecordsNoVerdict(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.buildCoreChange(t, "core-feature")
	hotfix := sb.pushCoreHotfix(t)
	runner := func(ctx context.Context, req BaseCheckRequest) (BaseCheckResult, error) {
		return BaseCheckResult{Output: "repo_exec: cannot start the command: argument list too long\n", ExitCode: 1, Inconclusive: true}, nil
	}

	runMergerOnce(t, sb.newMerger(t, MergerOptions{BaseCheckRunner: runner}))
	if v := sb.verdict(t, hotfix, BaseCheckRegression); v != nil {
		t.Fatalf("an inconclusive check was recorded: %+v", v)
	}
	if bases, _ := sb.st.ListBrokenBases(context.Background()); len(bases) != 0 {
		t.Fatalf("expected no broken base, got %+v", bases)
	}
	sb.requireIntegrated(t, "core-feature")
}

// End to end: a change's own regression that waits for a worker's suite (the daemon runs the merger
// and the dispatcher's workers in one process) longer than gates.timeout still runs, and the change
// integrates: the wait for heavyRegressionMu never counts against the gates' time limit.
func TestMergeQueuedChangeRegressionDoesNotTimeOut(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.buildCoreChangeWriting(t, "core-locale", map[string]string{"config/locales/es.yml": "es:\n  saludo: \"Canción\"\n"})
	settingstest.Give(t, settings.GateTimeout, "2s")

	heavyRegressionMu.Lock() // a worker's BUILD verification running its regression in the daemon
	released := make(chan struct{})
	go func() {
		time.Sleep(3 * time.Second)
		heavyRegressionMu.Unlock()
		close(released)
	}()
	_, out := sb.mergerRun(t)
	<-released

	if strings.Contains(out, "exit 124") {
		t.Fatalf("the regression timed out while it waited for the lock:\n%s", out)
	}
	ran := false
	for _, l := range sb.marker("core.regression") {
		ran = ran || l == "slot=99"
	}
	if !ran {
		t.Fatalf("core's regression did not run in the merger: %v\n%s", sb.marker("core.regression"), out)
	}
	sb.requireIntegrated(t, "core-locale")
}
