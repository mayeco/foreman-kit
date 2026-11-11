package cycle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// onDemandDispatcher stands in for the daemon's continuous dispatcher: it holds what its latest pass
// left and signals each pass, as orchestrator.Hub does for the real one.
type onDemandDispatcher struct {
	mu     sync.Mutex
	report DispatchReport
	have   bool
	// auto makes it pass, and find both workers free with nothing to start, whenever a cycle hands
	// over what it queued.
	auto   bool
	signal chan struct{}
}

func newOnDemandDispatcher() *onDemandDispatcher {
	return &onDemandDispatcher{signal: make(chan struct{}, 1)}
}

// pass is a pass that begins now and leaves running workers out of two, and waiting items.
func (d *onDemandDispatcher) pass(running, waiting int) {
	d.mu.Lock()
	d.report, d.have = DispatchReport{Workers: 2, Running: running, Waiting: waiting, At: time.Now()}, true
	d.mu.Unlock()
	select {
	case d.signal <- struct{}{}:
	default:
	}
}

func (d *onDemandDispatcher) load() (DispatchReport, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.report, d.have
}

func (d *onDemandDispatcher) passOnBacklog(on bool) {
	d.mu.Lock()
	d.auto = on
	d.mu.Unlock()
}

func (d *onDemandDispatcher) ReadyEnqueued() {}

func (d *onDemandDispatcher) BacklogChanged() {
	d.mu.Lock()
	auto := d.auto
	d.mu.Unlock()
	if auto {
		d.pass(0, 0)
	}
}

// onDemandLoop is a cycle loop running over a workspace of its own, with a stand-in dispatcher.
type onDemandLoop struct {
	t      *testing.T
	ctx    context.Context
	root   string
	st     *store.Store
	disp   *onDemandDispatcher
	log    daemonLoopPanicLog
	cancel context.CancelFunc
	done   chan struct{}
}

// startOnDemandLoop runs cycleLoop as the daemon does: under the daemon lock, with a continuous merger
// and dispatcher, and a cycle whose judgment is a stand-in. Cycles get a stamp of their own even when
// two start within one second.
func startOnDemandLoop(t *testing.T, idleDelay, interval time.Duration, noInitialCycle bool, prepare func(l *onDemandLoop)) *onDemandLoop {
	t.Helper()
	root := bareApp(t)
	st, err := store.OpenAt(filepath.Join(root, "log", "openspec.db"), "ws-on-demand-cycle", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	l := &onDemandLoop{t: t, ctx: ctx, root: root, st: st, disp: newOnDemandDispatcher(), cancel: cancel, done: make(chan struct{})}

	realStamp := newCycleStamp
	var cycles atomic.Int64
	newCycleStamp = func() string {
		at := time.Date(2026, 11, 10, 5, 0, 0, 0, time.UTC).Add(time.Duration(cycles.Add(1)) * time.Second)
		return at.Format("20060102T150405Z")
	}
	if _, err := st.AcquireLock(ctx, "daemon", store.LockOptions{OwnerType: "daemon", PID: os.Getpid(), TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(l)
	}

	serve := func(ctx context.Context, root string, out io.Writer) error { <-ctx.Done(); return nil }
	opts := DaemonOptions{
		IdleDelay: idleDelay,
		CycleOpts: CycleOptions{
			SkipWorktreePrep: true,
			AgentExecFunc: func(ctx context.Context, workdir, promptFile, engine string, timeout time.Duration, out io.Writer) error {
				return nil
			},
		},
		ServeMergerFunc:     serve,
		ServeDispatcherFunc: serve,
		Notifier:            l.disp,
		DispatchLoadFunc:    l.disp.load,
		DispatchSignal:      l.disp.signal,
	}
	go func() {
		defer close(l.done)
		cycleLoop(ctx, root, st, opts, interval, noInitialCycle, &l.log)
	}()
	t.Cleanup(func() {
		l.stop()
		newCycleStamp = realStamp
	})
	return l
}

func (l *onDemandLoop) stop() {
	l.cancel()
	select {
	case <-l.done:
	case <-time.After(30 * time.Second):
		l.t.Error("the cycle loop did not stop")
	}
}

// wait waits until cond holds.
func (l *onDemandLoop) wait(what string, cond func() bool) {
	l.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			l.t.Fatalf("timed out waiting until %s; schedule %+v; cycles %v; log:\n%s", what, l.schedule(), l.triggers(), l.log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// schedule is what the daemon says about its next cycle; the zero value while it has said nothing.
func (l *onDemandLoop) schedule() store.CycleSchedule {
	sched, err := l.st.CycleSchedule(context.Background())
	if err != nil || sched == nil {
		return store.CycleSchedule{}
	}
	return *sched
}

// triggers lists what started each finished cycle, oldest first.
func (l *onDemandLoop) triggers() []string {
	cycles, err := l.st.ListCycles(context.Background(), 0)
	if err != nil {
		l.t.Fatal(err)
	}
	var out []string
	for i := len(cycles) - 1; i >= 0; i-- {
		if cycles[i].FinishedAt != "" {
			out = append(out, cycles[i].Trigger)
		}
	}
	return out
}

// waitCycles waits until the cycles that finished were started by exactly want, oldest first.
func (l *onDemandLoop) waitCycles(want ...string) {
	l.t.Helper()
	l.wait("the cycles are "+strings.Join(want, ", "), func() bool {
		got := l.triggers()
		if len(got) > len(want) {
			l.t.Fatalf("more cycles ran than expected: %v, want %v; log:\n%s", got, want, l.log.String())
		}
		return strings.Join(got, ",") == strings.Join(want, ",")
	})
}

func (l *onDemandLoop) waitWaits(state advanceState) {
	l.t.Helper()
	l.wait("the next cycle waits on "+string(state), func() bool { return l.schedule().Waits == string(state) })
}

// someoneRanACycle records a cycle asked for from the command line that began ago before now and took
// a minute.
func (l *onDemandLoop) someoneRanACycle(ago time.Duration) time.Time {
	l.t.Helper()
	const stamp = "20261110T040000Z"
	began := time.Now().Add(-ago)
	if err := l.st.BeginCycle(l.ctx, store.Cycle{Stamp: stamp, Trigger: store.CycleTriggerManual, StartedAt: began.UTC().Format(time.RFC3339Nano)}); err != nil {
		l.t.Fatal(err)
	}
	if err := l.st.FinishCycle(l.ctx, stamp, "done", began.Add(time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		l.t.Fatal(err)
	}
	return began
}

// build records that a worker built change.
func (l *onDemandLoop) build(change string) {
	l.t.Helper()
	id, err := l.st.BeginChangeRun(l.ctx, change, 1, "/wt/"+change, "feat/spec-"+change)
	if err != nil {
		l.t.Fatal(err)
	}
	if err := l.st.FinishChangeRun(l.ctx, id, "ready"); err != nil {
		l.t.Fatal(err)
	}
}

// With an idle delay the daemon starts the next cycle before its tick once a worker is free with
// nothing to start: not while the workers build, not on a report older than what the last cycle queued,
// and not a third time when two cycles in a row moved nothing. Each early cycle says what started it and
// restarts the tick.
func TestDaemonStartsTheCycleBeforeItsTickWhenThePipelineRunsOutOfWork(t *testing.T) {
	const idleDelay = 300 * time.Millisecond
	var began time.Time
	l := startOnDemandLoop(t, idleDelay, time.Hour, true, func(l *onDemandLoop) {
		began = l.someoneRanACycle(10 * time.Minute)
		l.build("built-since-that-cycle")
		l.disp.pass(2, 0) // both workers build
	})
	manual, early := store.CycleTriggerManual, store.CycleTriggerOutOfWork

	// The delay passed long ago and a change was built, but no worker is free.
	l.waitWaits(advanceBusy)
	busy := l.schedule()
	if busy.Interval != time.Hour || busy.IdleDelay != idleDelay || busy.Latest.IsZero() {
		t.Fatalf("the schedule while the workers build: %+v", busy)
	}
	if want := began.Add(idleDelay).Truncate(time.Second); !busy.Earliest.Equal(want) {
		t.Fatalf("the earliest start counts from the start of the last cycle: got %s, want %s", busy.Earliest, want)
	}
	l.waitCycles(manual)

	// A worker comes free with nothing to start: the cycle runs now, and the tick restarts with it.
	// The schedule keeps instants to the second, so the tick is given a second to show that it moved.
	time.Sleep(1100 * time.Millisecond)
	l.disp.pass(1, 0)
	l.waitCycles(manual, early)
	if restarted := l.schedule().Latest; !restarted.After(busy.Latest) {
		t.Fatalf("the tick did not restart with the early cycle: it was due at %s and is due at %s", busy.Latest, restarted)
	}

	// The dispatcher has not passed since that cycle queued its work: its report says nothing yet.
	l.waitWaits(advanceNoReport)
	l.waitCycles(manual, early)

	// It passes and finds the workers free. That cycle moved nothing, but the one before it did.
	l.disp.passOnBacklog(true)
	l.disp.pass(0, 0)
	l.waitCycles(manual, early, early)

	// Two cycles in a row moved nothing: the next one waits for its tick, however often the
	// dispatcher passes.
	l.waitWaits(advanceNoMovement)
	for i := 0; i < 4; i++ {
		time.Sleep(idleDelay / 2)
		l.disp.pass(0, 0)
	}
	l.waitCycles(manual, early, early)
	if l.schedule().Waits != string(advanceNoMovement) {
		t.Fatalf("the brake was released without movement: %+v", l.schedule())
	}
	var status bytes.Buffer
	if rc := StatusDaemon(l.ctx, l.root, l.st, &status); rc != 0 || !strings.Contains(status.String(), "so it waits for the tick") ||
		!strings.Contains(status.String(), "next cycle at "+l.schedule().Latest.UTC().Format(time.RFC3339)+" at the latest") {
		t.Fatalf("daemon status does not say when the next cycle runs and what it waits for (rc=%d):\n%s", rc, status.String())
	}

	// A change is blocked: something moved, and the cycle runs early again. The dispatcher does not
	// pass after it, so that no further cycle follows before the loop is stopped.
	if err := l.st.BlockChange(l.ctx, store.QuarantineRecord{ChangeName: "blocked-after-the-brake", Reason: "gate_failure"}); err != nil {
		t.Fatal(err)
	}
	l.disp.passOnBacklog(false)
	l.disp.pass(0, 0)
	l.waitCycles(manual, early, early, early)
	l.waitWaits(advanceNoReport)

	l.stop()
	log := l.log.String()
	if n := strings.Count(log, "the cycle runs now instead of at"); n != 3 {
		t.Fatalf("expected the daemon log to say three times that the cycle ran before its tick, got %d:\n%s", n, log)
	}
	if n := strings.Count(log, "nothing was built, blocked or integrated in the last two cycles: the next cycle waits for its tick"); n != 1 {
		t.Fatalf("expected the daemon log to say once that the cycle waits for its tick, got %d:\n%s", n, log)
	}
}

// Without an idle delay the cycle only runs on its tick, as it always did, whatever the dispatcher
// reports.
func TestDaemonRunsTheCycleOnlyOnItsTickWithoutAnIdleDelay(t *testing.T) {
	l := startOnDemandLoop(t, 0, 400*time.Millisecond, false, func(l *onDemandLoop) {
		l.build("built-before-the-daemon")
		l.disp.passOnBacklog(true)
		l.disp.pass(0, 0)
	})
	startup, tick := store.CycleTriggerStartup, store.CycleTriggerInterval

	l.wait("the startup cycle and two ticks ran", func() bool { return len(l.triggers()) >= 3 })
	for i, got := range l.triggers() {
		want := tick
		if i == 0 {
			want = startup
		}
		if got != want {
			t.Fatalf("cycle %d was started by %q, want %q: %v", i+1, got, want, l.triggers())
		}
	}

	sched := l.schedule()
	if sched.Interval != 400*time.Millisecond || sched.IdleDelay != 0 || !sched.Earliest.IsZero() || sched.Waits != "" {
		t.Fatalf("with no idle delay the schedule only says the tick: %+v", sched)
	}
	if line := cycleScheduleLine(sched); strings.Contains(line, idleDelayKey) {
		t.Fatalf("daemon status mentions an idle delay that is off: %s", line)
	}
	l.stop()
	if log := l.log.String(); strings.Contains(log, "the cycle runs now instead of at") {
		t.Fatalf("a cycle ran before its tick although the idle delay is off:\n%s", log)
	}
}

// A paused workspace starts nothing, so the daemon does not start a cycle early on it; it does once
// the pause is lifted.
func TestDaemonDoesNotStartACycleEarlyOnAPausedWorkspace(t *testing.T) {
	var pauseFile string
	l := startOnDemandLoop(t, 200*time.Millisecond, time.Hour, true, func(l *onDemandLoop) {
		l.someoneRanACycle(10 * time.Minute)
		l.build("built-since-that-cycle")
		pauseFile = store.RuntimePath(l.root, "PAUSE")
		if err := os.MkdirAll(filepath.Dir(pauseFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pauseFile, []byte("paused by a test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		l.disp.passOnBacklog(true)
		l.disp.pass(0, 0)
	})

	l.waitWaits(advanceBusy)
	l.waitCycles(store.CycleTriggerManual)
	if log := l.log.String(); strings.Contains(log, "skipping cycle tick") || strings.Contains(log, "the cycle runs now") {
		t.Fatalf("the daemon tried to run a cycle early on a paused workspace:\n%s", log)
	}

	if err := os.Remove(pauseFile); err != nil {
		t.Fatal(err)
	}
	l.disp.passOnBacklog(false) // one early cycle, and no report after it to start another
	l.disp.pass(0, 0)
	l.waitCycles(store.CycleTriggerManual, store.CycleTriggerOutOfWork)
}

// When the cycle may start early is a key of .foreman-kit/daemon.yaml: 15 minutes as the seed writes it,
// never to keep the cycle on its tick. The daemon that reads it says in its log when the cycle runs.
func TestDaemonReadsWhenTheCycleRunsFromItsFile(t *testing.T) {
	ctx, cancel, appDir, st := daemonLoopPanicSetup(t)
	key := settings.CycleEveryWhenOutOfWork
	file := filepath.Join(appDir, filepath.FromSlash(key.FilePath()))
	seed, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	leaf := key.Parts()
	name := leaf[len(leaf)-1]
	seedLine := name + ": 15m\n"
	if strings.Count(string(seed), seedLine) != 1 {
		t.Fatalf("the seed of %s does not write %q once", key.FilePath(), seedLine)
	}
	// set writes the file with another value and returns what the daemon then reads.
	set := func(value string) time.Duration {
		t.Helper()
		if err := os.WriteFile(file, []byte(strings.Replace(string(seed), seedLine, name+": "+value+"\n", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		return LoadDaemonConfig(appDir).EveryWhenOutOfWork
	}
	if got := LoadDaemonConfig(appDir); got.EveryWhenOutOfWork != 15*time.Minute || got.Every != time.Hour {
		t.Fatalf("a new application starts a cycle every %s, or %s after the last one when out of work; want the 60m and 15m of the seed", got.Every, got.EveryWhenOutOfWork)
	}
	if got := set("never"); got != 0 {
		t.Fatalf("the application keeps the cycle on its tick, the daemon reads %s", got)
	}
	if got := set("20m"); got != 20*time.Minute {
		t.Fatalf("the application sets 20m, the daemon reads %s", got)
	}

	serve := func(ctx context.Context, root string, out io.Writer) error { <-ctx.Done(); return nil }
	var log, errLog daemonLoopPanicLog
	done := make(chan int, 1)
	go func() {
		done <- StartDaemon(ctx, appDir, st, DaemonOptions{
			Interval:            time.Hour,
			NoInitialCycle:      true,
			ServeMergerFunc:     serve,
			ServeDispatcherFunc: serve,
			DispatchLoadFunc:    func() (DispatchReport, bool) { return DispatchReport{}, false },
		}, &log, &errLog)
	}()
	want := "the cycle judges every 1h0m0s, or 20m0s after the last one began once a worker is free with nothing to start and the queue has room (" +
		settings.CycleEveryWhenOutOfWork.Where() + ", " + settings.QueueMaxOpen.Where() + ")"
	daemonLoopPanicWait(t, "the daemon says when the cycle runs", done, &log, func() bool { return strings.Contains(log.String(), want) })
	daemonLoopPanicWait(t, "the daemon says what the next cycle waits for", done, &log, func() bool {
		sched, _ := st.CycleSchedule(ctx)
		return sched != nil && sched.IdleDelay == 20*time.Minute && sched.Interval == time.Hour
	})

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("daemon exit code %d after cancel; log:\n%s", code, log.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop")
	}
}
