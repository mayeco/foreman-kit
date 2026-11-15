package cycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// outcomeCycle runs one cycle of a with a judgment that calls judge, and returns the cycle's log, the
// error RunCycle ended on and the row the store holds for it. mod changes the options before the run.
func outcomeCycle(t *testing.T, a listsCycleApp, stamp string, judge func(workdir string, out io.Writer), mod func(*CycleOptions)) (string, error, *store.Cycle) {
	t.Helper()
	opts := CycleOptions{
		Stamp:        stamp,
		SkipMerge:    true,
		SkipDispatch: true,
		AgentExecFunc: func(ctx context.Context, workdir, promptFile, engine string, timeout time.Duration, out io.Writer) error {
			if judge != nil {
				judge(workdir, out)
			}
			return nil
		},
	}
	if mod != nil {
		mod(&opts)
	}
	var out bytes.Buffer
	err := RunCycle(context.Background(), a.root, a.st, opts, &out)
	row, gerr := a.st.GetCycle(context.Background(), stamp)
	if gerr != nil || row == nil {
		t.Fatalf("cycle %s has no row (err %v)\n%s", stamp, gerr, out.String())
	}
	return out.String(), err, row
}

// pickedStrategy is the strategy the cycle printed before its judgment started: the first line of the
// log that starts with `strategy: `.
func pickedStrategyOf(t *testing.T, log string) string {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "strategy: ") {
			return strings.Fields(strings.TrimPrefix(line, "strategy: "))[0]
		}
	}
	t.Fatalf("the cycle picked no strategy:\n%s", log)
	return ""
}

// What the judgment prints must not decide how its cycle is recorded.
func TestACycleRecordsDoneWhateverItsJudgmentPrints(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"preflight failure", "preflight: FAIL (git 2.16.6 is older than 2.17.0)"},
		{"pause", "cycle: paused (scope violation in cycle X)"},
		{"scope sentence", "the earlier agent wrote outside its scope and the cycle paused"},
		{"strategy line", "strategy: none-of-yours"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newListsCycleApp(t, "proposals:\n  approved_by: human\n")
			log, err, row := outcomeCycle(t, a, "20261115T110000Z", func(workdir string, out io.Writer) {
				fmt.Fprintln(out, tc.line)
			}, nil)
			if err != nil {
				t.Fatalf("RunCycle: %v\n%s", err, log)
			}
			if row.Outcome != "done" {
				t.Fatalf("outcome is %q, want done\n%s", row.Outcome, log)
			}
			picked := pickedStrategyOf(t, log)
			if picked == "none-of-yours" || row.Strategy != picked {
				t.Fatalf("strategy is %q, want the picked %q\n%s", row.Strategy, picked, log)
			}
		})
	}
}

func TestACycleRecordsAFailedPreflight(t *testing.T) {
	a := newListsCycleApp(t, "proposals:\n  approved_by: human\n")
	log, err, row := outcomeCycle(t, a, "20261115T111000Z", nil, func(o *CycleOptions) {
		o.PreflightFunc = func(ctx context.Context, root string, out io.Writer) error {
			return errors.New("git is too old")
		}
	})
	if err == nil {
		t.Fatalf("RunCycle succeeded with a failing preflight\n%s", log)
	}
	if row.Outcome != "preflight failed" {
		t.Fatalf("outcome is %q, want preflight failed\n%s", row.Outcome, log)
	}
}

func TestACycleRecordsAScopeViolation(t *testing.T) {
	a := newListsCycleApp(t, "proposals:\n  approved_by: human\n")
	log, err, row := outcomeCycle(t, a, "20261115T112000Z", func(workdir string, out io.Writer) {
		writeRel(t, workdir, "outside-the-scope.txt", "the judgment wrote this\n")
	}, nil)
	if !errors.Is(err, ErrScopeViolation) {
		t.Fatalf("RunCycle ended on %v, want a scope violation\n%s", err, log)
	}
	if row.Outcome != "scope violation" {
		t.Fatalf("outcome is %q, want scope violation\n%s", row.Outcome, log)
	}
	if ws, _ := a.st.GetWorkspaceState(context.Background()); !ws.Paused {
		t.Fatalf("the workspace is not paused after a scope violation\n%s", log)
	}
}

func TestACycleRecordsAPauseTheJudgmentMade(t *testing.T) {
	a := newListsCycleApp(t, "proposals:\n  approved_by: human\n")
	log, err, row := outcomeCycle(t, a, "20261115T113000Z", func(workdir string, out io.Writer) {
		if _, err := a.st.Pause(context.Background(), "the judgment asked for a human", "test", ""); err != nil {
			t.Errorf("Pause: %v", err)
		}
	}, nil)
	if err != nil {
		t.Fatalf("RunCycle: %v\n%s", err, log)
	}
	if row.Outcome != "paused" {
		t.Fatalf("outcome is %q, want paused\n%s", row.Outcome, log)
	}
}

func TestACycleRecordsADispatchStillRunning(t *testing.T) {
	a := newListsCycleApp(t, "proposals:\n  approved_by: human\n")
	// Another live process, the one that started this test, holds the dispatch lock.
	if _, err := a.st.AcquireLock(context.Background(), "dispatch", store.LockOptions{OwnerType: "dispatch", PID: os.Getppid(), TTL: time.Hour}); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	log, err, row := outcomeCycle(t, a, "20261115T114000Z", nil, func(o *CycleOptions) {
		o.SkipDispatch = false
	})
	if err != nil {
		t.Fatalf("RunCycle: %v\n%s", err, log)
	}
	if row.Outcome != "judged; an earlier dispatch was still running" {
		t.Fatalf("outcome is %q\n%s", row.Outcome, log)
	}
}

// progressCycles lists the cycles `progress` reports, by stamp.
func progressCycles(t *testing.T, a listsCycleApp) map[string]map[string]any {
	t.Helper()
	rep, err := Progress(context.Background(), a.root, a.st, time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	period, _ := rep["period"].(map[string]any)
	cycles, _ := period["cycles"].([]map[string]any)
	byStamp := map[string]map[string]any{}
	for _, c := range cycles {
		byStamp[asString(c["stamp"])] = c
	}
	return byStamp
}

func TestProgressShowsWhatAFinishedCycleRecorded(t *testing.T) {
	a := newListsCycleApp(t, "proposals:\n  approved_by: human\n")
	stamp := time.Now().UTC().Add(-time.Minute).Format("20060102T150405Z")
	log, err, row := outcomeCycle(t, a, stamp, func(workdir string, out io.Writer) {
		fmt.Fprintln(out, "preflight: FAIL (quoted from an earlier cycle)")
		fmt.Fprintln(out, "strategy: none-of-yours")
	}, nil)
	if err != nil || row.Outcome != "done" {
		t.Fatalf("RunCycle: %v, outcome %q\n%s", err, row.Outcome, log)
	}
	got := progressCycles(t, a)[stamp]
	if got == nil {
		t.Fatalf("progress lists no cycle %s", stamp)
	}
	if got["outcome"] != "done" {
		t.Fatalf("progress shows outcome %v, want done", got["outcome"])
	}
	if got["strategy"] != pickedStrategyOf(t, log) {
		t.Fatalf("progress shows strategy %v, want %q", got["strategy"], pickedStrategyOf(t, log))
	}
}

func TestProgressReadsTheLogOfACycleWithNoRecord(t *testing.T) {
	a := newListsCycleApp(t, "proposals:\n  approved_by: human\n")
	stamp := time.Now().UTC().Add(-2 * time.Minute).Format("20060102T150405Z")
	logPath := store.RuntimePath(a.root, "cycle", "cycle-"+stamp+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("== cycle "+stamp+"\nstrategy: defects (defects)\n== done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := progressCycles(t, a)[stamp]
	if got == nil || got["outcome"] != "done" || got["strategy"] != "defects" {
		t.Fatalf("progress shows %v for a log with no record, want done and defects", got)
	}
}
