package cycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mayeco/foreman-kit/internal/proc"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// DaemonConfig holds what the resident process reads from .foreman-kit/daemon.yaml when it starts.
type DaemonConfig struct {
	// Web says the daemon serves the Web Portal (portal.enabled), on Bind and Port.
	Web  bool   `json:"web"`
	Bind string `json:"bind"`
	Port int    `json:"port"`
	// Every is the longest the daemon waits between the start of two cycles (cycle.every).
	Every time.Duration `json:"every"`
	// EveryWhenOutOfWork is how long after a cycle began the next one may start before that, once
	// the pipeline has run out of work and the queue has room (cycle.every_when_out_of_work).
	// 0: never, the cycle only runs every Every.
	EveryWhenOutOfWork time.Duration `json:"every_when_out_of_work"`
}

// LoadDaemonConfig reads <root>/.foreman-kit/daemon.yaml. Without a root it returns the values the kit
// ships.
func LoadDaemonConfig(root string) DaemonConfig {
	r := settings.At(root)
	if root == "" {
		r.Use(settings.CycleEvery.File(), nil)
		r.Stderr = io.Discard
	}
	return DaemonConfig{
		Web:                r.Bool(settings.DaemonPortal),
		Bind:               r.String(settings.DaemonBind),
		Port:               r.Int(settings.DaemonPort),
		Every:              r.Duration(settings.CycleEvery),
		EveryWhenOutOfWork: r.Duration(settings.CycleEveryWhenOutOfWork),
	}
}

// DaemonOptions configures a project daemon execution.
//
// The daemon runs the pipeline continuously: a merger integrates each change as soon as it is ready and
// a dispatcher starts each backlog item as soon as it is eligible and a worker slot is free, both woken by
// a Notifier rather than a poll. Only the cycle's judgment (verify, explore, curate memory) is periodic.
type DaemonOptions struct {
	Detach bool
	// Console runs the daemon for a person at a terminal (`foreman-kit` alone): its log goes to the
	// terminal as well as to daemon.log, and Started is told where the portal and the log are.
	Console bool
	// Started, when set, is called once the foreground daemon holds its lock and its portal listens,
	// with the portal's URL ("" without a portal) and the path of its log.
	Started        func(webURL, logPath string)
	Web            bool
	WebSet         bool
	Bind           string
	Port           int
	PortExplicit   bool
	Interval       time.Duration
	NoInitialCycle bool
	CycleOpts      CycleOptions
	// StartWebFunc starts the embedded Web Portal HTTP server on an available port and returns
	// (boundURL, shutdownFunc, error).
	StartWebFunc func(ctx context.Context, root string, st *store.Store, bind string, port int, portExplicit bool) (string, func(context.Context) error, error)
	// ServeMergerFunc runs the continuous merger (orchestrator.Merger.Serve) until ctx is cancelled,
	// logging to out. When set, the cycle no longer drains the merge queue first. It returns store.ErrLockHeld
	// while a cron cycle or a CLI owns integration; the daemon then tries again every LoopRetryDelay.
	ServeMergerFunc func(ctx context.Context, root string, out io.Writer) error
	// ServeDispatcherFunc runs the continuous dispatcher (orchestrator.Dispatcher.Serve) likewise. When
	// set, the cycle no longer dispatches after judging.
	ServeDispatcherFunc func(ctx context.Context, root string, out io.Writer) error
	// Notifier wakes the continuous loops (the *orchestrator.Hub their Serve functions listen to). The
	// daemon calls BacklogChanged when .foreman-kit/agent_backlog.json changes and when a cycle ends, and both
	// methods when the workspace is resumed.
	Notifier PipelineNotifier
	// BacklogWatchInterval is how often the daemon checks .foreman-kit/agent_backlog.json and the pause state
	// (default 2s).
	BacklogWatchInterval time.Duration
	// LoopRetryDelay is how long a stopped merger or dispatcher loop waits before it starts again: its
	// lock is held elsewhere, or it failed (default 10s).
	LoopRetryDelay time.Duration
	// IdleDelay is how long after a cycle began the next one may start before its tick, once the
	// pipeline has run out of work and the queue has room. 0: what settings.CycleEveryWhenOutOfWork
	// says, which is 15 minutes unless the application sets another delay, or never before the
	// tick.
	IdleDelay time.Duration
	// DispatchLoadFunc returns what the latest pass of the continuous dispatcher left
	// (orchestrator.Hub.DispatchLoad); ok is false while no such dispatcher is passing. Without it
	// the cycle only runs on its tick: nothing else can tell that the pipeline has run out of work.
	DispatchLoadFunc func() (report DispatchReport, ok bool)
	// DispatchSignal wakes the cycle loop after each pass of the continuous dispatcher
	// (orchestrator.Hub.Dispatched).
	DispatchSignal <-chan struct{}
}

// PipelineNotifier wakes the daemon's continuous merger (ReadyEnqueued) and dispatcher (BacklogChanged).
// *orchestrator.Hub implements it.
type PipelineNotifier interface {
	ReadyEnqueued()
	BacklogChanged()
}

// FindAvailablePort returns `preferredPort` if free on `bind`, or probes up to 50 consecutive ports
// when `explicit` is false so multiple project daemons with `--web` never collide on the default port.
func FindAvailablePort(bind string, preferredPort int, explicit bool) (int, error) {
	if bind == "" {
		bind, _ = settings.Of(settings.DaemonBind).Default.(string)
	}
	if preferredPort <= 0 {
		preferredPort, _ = settings.Of(settings.DaemonPort).Default.(int)
	}
	maxTries := 50
	if explicit {
		maxTries = 1
	}
	for i := 0; i < maxTries; i++ {
		p := preferredPort + i
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, p))
		if err == nil {
			_ = ln.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no available TCP port starting at %s:%d", bind, preferredPort)
}

// daemonFileUnusable says why a daemon of the application at root does not start ("" when it can):
// what keeps .foreman-kit/daemon.yaml from being used (settings.Reader.Unusable).
func daemonFileUnusable(root string) string {
	if root == "" {
		return ""
	}
	return settings.Unusable(root, settings.DaemonFiles...)
}

// StartDaemon starts the per-project daemon either in the background (when opts.Detach == true)
// or in the foreground (`foreman-kit daemon run`).
func StartDaemon(ctx context.Context, root string, st *store.Store, opts DaemonOptions, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}

	// Check if a daemon is already running for this project
	if st != nil {
		if dl := liveDaemonLock(ctx, st); dl != nil {
			meta := parseDaemonMetadata(dl.MetadataJSON)
			webNote := ""
			if u := asString(meta["web_url"]); u != "" {
				webNote = " web=" + u
			}
			fmt.Fprintf(stdout, "daemon: already running (pid=%d%s log=%s)\n", dl.PID, webNote, store.RuntimePath(root, "daemon.log"))
			return 0
		}
	}

	// The daemon reads its file once, when it starts: it does not start on one it cannot use. What
	// it runs (the dispatcher, the merger, the cycle) reads its own files on every pass and waits
	// for them.
	if why := daemonFileUnusable(root); why != "" {
		fmt.Fprintf(stderr, "daemon: %s: it does not start until it is mended\n", why)
		return 1
	}

	cfg := LoadDaemonConfig(root)
	webEnabled := cfg.Web
	if opts.WebSet {
		webEnabled = opts.Web
	}
	bind := cfg.Bind
	if opts.Bind != "" {
		bind = opts.Bind
	}
	port := cfg.Port
	if opts.Port > 0 {
		port = opts.Port
	}
	interval := cfg.Every
	if opts.Interval > 0 {
		interval = opts.Interval
	}
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	if opts.IdleDelay <= 0 {
		opts.IdleDelay = cfg.EveryWhenOutOfWork
	}

	if opts.Detach {
		pid, exited, err := spawnDetachedDaemonProcess(root, webEnabled, bind, port, opts.PortExplicit, interval, opts.NoInitialCycle)
		if err != nil {
			fmt.Fprintf(stderr, "daemon: failed to detach: %v\n", err)
			return 1
		}
		// Wait briefly for the detached child to acquire the "daemon" lock in SQLite and record its bound web URL
		webURL := ""
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if st != nil {
				if dl, err := st.GetLock(ctx, "daemon"); err == nil && dl != nil && dl.PID == pid {
					meta := parseDaemonMetadata(dl.MetadataJSON)
					webURL = asString(meta["web_url"])
					break
				}
			}
			select {
			case waitErr := <-exited:
				// The child is gone before it took the lock: no daemon started (its port is taken,
				// the database could not be opened), unless another daemon won the lock meanwhile.
				return reportDaemonExitedAtStartup(ctx, root, st, pid, waitErr, stdout, stderr)
			case <-time.After(40 * time.Millisecond):
			}
		}
		if webEnabled && webURL != "" {
			fmt.Fprintf(stdout, "daemon: started pid=%d web=%s log=%s\n", pid, webURL, store.RuntimePath(root, "daemon.log"))
		} else if webEnabled {
			fmt.Fprintf(stdout, "daemon: started pid=%d web=http://%s:%d log=%s\n", pid, bind, port, store.RuntimePath(root, "daemon.log"))
		} else {
			fmt.Fprintf(stdout, "daemon: started pid=%d log=%s\n", pid, store.RuntimePath(root, "daemon.log"))
		}
		if given := givenForTheRun(settings.RunOverrides()); given != "" {
			fmt.Fprintf(stdout, "daemon: given for this run: %s (in no file: it holds until the daemon stops)\n", given)
		}
		return 0
	}

	return runDaemonForeground(ctx, root, st, webEnabled, bind, port, opts.PortExplicit, interval, opts.NoInitialCycle, opts, stdout, stderr)
}

// kitExecutable locates the foreman-kit binary a detached daemon or cycle runs. It is a variable so
// tests can point it at a stand-in.
var kitExecutable = os.Executable

// startDetached starts cmd, a child meant to outlive this call, and returns its PID. The child is waited
// for in the background instead of being released: the caller can be the daemon or the portal, which run
// for weeks, and a child nobody waits for stays a zombie, holding its PID, for as long as its parent
// lives. A caller that exits first simply leaves the child to init. exited receives the result of that
// wait, should the child exit while the caller still runs.
func startDetached(cmd *exec.Cmd) (pid int, exited <-chan error, err error) {
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	pid = cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return pid, done, nil
}

// reportDaemonExitedAtStartup tells the caller of `daemon start` that the detached daemon pid exited,
// with waitErr, before it took the daemon lock, and returns the exit code of the command.
func reportDaemonExitedAtStartup(ctx context.Context, root string, st *store.Store, pid int, waitErr error, stdout, stderr io.Writer) int {
	logPath := store.RuntimePath(root, "daemon.log")
	if dl := liveDaemonLock(ctx, st); dl != nil {
		fmt.Fprintf(stdout, "daemon: already running (pid=%d log=%s)\n", dl.PID, logPath)
		return 0
	}
	status := "exit status 0"
	if waitErr != nil {
		status = waitErr.Error()
	}
	fmt.Fprintf(stderr, "daemon: failed to start: pid %d exited at once (%s); see %s\n", pid, status, logPath)
	return 1
}

func spawnDetachedDaemonProcess(root string, web bool, bind string, port int, portExplicit bool, interval time.Duration, noInitialCycle bool) (int, <-chan error, error) {
	exe, err := kitExecutable()
	if err != nil {
		return 0, nil, err
	}
	logDir := store.RuntimeDir(root)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return 0, nil, err
	}
	logPath := filepath.Join(logDir, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, nil, err
	}
	defer logFile.Close()

	args := []string{
		"daemon", "run",
		"--bind", bind,
		"--port", strconv.Itoa(port),
		"--interval", interval.String(),
	}
	if web {
		args = append(args, "--web=true")
	} else {
		args = append(args, "--web=false")
	}
	if portExplicit {
		args = append(args, "--port-explicit")
	}
	if noInitialCycle {
		args = append(args, "--no-initial-cycle")
	}

	cmd := exec.Command(exe, args...)
	cmd.Dir = root
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "FOREMANKIT_APP_ROOT=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err == nil {
		defer devNull.Close()
		cmd.Stdin = devNull
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	return startDetached(cmd)
}

func runDaemonForeground(
	ctx context.Context,
	root string,
	st *store.Store,
	webEnabled bool,
	bind string,
	port int,
	portExplicit bool,
	interval time.Duration,
	noInitialCycle bool,
	opts DaemonOptions,
	stdout, stderr io.Writer,
) int {
	logDir := store.RuntimeDir(root)
	_ = os.MkdirAll(logDir, 0o755)
	logFile, err := os.OpenFile(filepath.Join(logDir, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	var logOut io.Writer = stdout
	if err == nil {
		defer logFile.Close()
		if stdout != os.Stdout || opts.Console {
			logOut = io.MultiWriter(stdout, logFile)
		} else {
			logOut = logFile
		}
	}
	logOut = &syncWriter{w: logOut}

	var webURL string
	var shutdownWeb func(context.Context) error
	if webEnabled && opts.StartWebFunc != nil {
		u, sd, err := opts.StartWebFunc(ctx, root, st, bind, port, portExplicit)
		if err != nil {
			fmt.Fprintf(stderr, "daemon: failed to start web portal: %v\n", err)
			return 1
		}
		webURL = u
		shutdownWeb = sd
	}
	defer func() {
		if shutdownWeb != nil {
			sdCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = shutdownWeb(sdCtx)
		}
	}()

	pid := os.Getpid()
	pidFile := filepath.Join(logDir, "daemon.pid")
	if st != nil {
		meta := map[string]any{
			"web":      webEnabled,
			"web_url":  webURL,
			"interval": interval.String(),
			"log":      store.RuntimePath(root, "daemon.log"),
			"root":     root,
		}
		// What --set gave this daemon is in no file: its lock records it, so that whoever asks
		// for its status from another process reads what it runs with.
		if given := settings.RunOverrides(); len(given) > 0 {
			meta["set"] = given
		}
		_, err := st.AcquireLock(ctx, "daemon", store.LockOptions{
			OwnerType: "daemon",
			PID:       pid,
			TTL:       24 * time.Hour,
			Metadata:  meta,
		})
		if errors.Is(err, store.ErrLockHeld) {
			fmt.Fprintln(stderr, "daemon: already running")
			return 0
		}
		if err != nil {
			fmt.Fprintf(stderr, "daemon: lock failed: %v\n", err)
			return 1
		}
		defer func() {
			_ = st.ReleaseLock(context.Background(), "daemon", pid)
			_ = os.Remove(pidFile)
		}()
	}
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o644)

	// Heartbeat goroutine for the daemon lock
	stopHB := st.StartLockHeartbeat(ctx, "daemon", pid, 2*time.Hour)
	defer stopHB()

	if opts.Started != nil {
		opts.Started(webURL, store.RuntimePath(root, "daemon.log"))
	}
	fmt.Fprintf(logOut, "[%s] daemon: started pid=%d root=%s interval=%s web=%v %s\n",
		time.Now().UTC().Format(time.RFC3339), pid, root, interval, webEnabled, webURL)

	fmt.Fprintf(logOut, "[%s] daemon: %s\n", daemonStamp(), daemonPipelineLine(opts, interval))
	if given := givenForTheRun(settings.RunOverrides()); given != "" {
		fmt.Fprintf(logOut, "[%s] daemon: given for this run: %s (in no file: it holds until the daemon stops)\n", daemonStamp(), given)
	}

	retry := opts.LoopRetryDelay
	if retry <= 0 {
		retry = 10 * time.Second
	}
	watchEvery := opts.BacklogWatchInterval
	if watchEvery <= 0 {
		watchEvery = 2 * time.Second
	}

	// Independent loops: a two-hour dispatch never delays the judgment, a long judgment never delays a
	// merge, and each loop reads and writes through the same store (one writer pool).
	var wg sync.WaitGroup
	runLoop := func(name string, fn func(context.Context, io.Writer) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			waiting := false // a held lock is logged once, not on every retry
			failed := ""     // so is a loop that keeps stopping for the same reason
			for ctx.Err() == nil {
				err := guardDaemonLoop(name, logOut, func() error { return fn(ctx, logOut) })
				switch {
				case ctx.Err() != nil:
				case errors.Is(err, store.ErrLockHeld):
					if !waiting {
						holder := ""
						if st != nil {
							if l, _ := st.GetLock(ctx, name); l != nil {
								holder = fmt.Sprintf(" (pid %d)", l.PID)
							}
						}
						fmt.Fprintf(logOut, "[%s] daemon: %s waits: another process holds the %s lock%s, a cron cycle or a CLI; trying again every %s\n",
							daemonStamp(), name, name, holder, retry)
					}
					waiting = true
				case err != nil:
					waiting = false
					if err.Error() != failed {
						fmt.Fprintf(logOut, "[%s] daemon: %s stopped: %v; restarting in %s\n", daemonStamp(), name, err, retry)
					}
					failed = err.Error()
				default:
					waiting, failed = false, ""
					fmt.Fprintf(logOut, "[%s] daemon: %s stopped; restarting in %s\n", daemonStamp(), name, retry)
				}
				select { // never spin
				case <-ctx.Done():
				case <-time.After(retry):
				}
			}
		}()
	}
	if opts.ServeMergerFunc != nil {
		runLoop("merger", func(ctx context.Context, out io.Writer) error { return opts.ServeMergerFunc(ctx, root, out) })
	}
	if opts.ServeDispatcherFunc != nil {
		runLoop("dispatcher", func(ctx context.Context, out io.Writer) error { return opts.ServeDispatcherFunc(ctx, root, out) })
	}
	if opts.Notifier != nil {
		runLoop("backlog-watch", func(ctx context.Context, _ io.Writer) error {
			return watchPipelineInputs(ctx, root, st, watchEvery, opts.Notifier)
		})
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		cycleLoop(ctx, root, st, opts, interval, noInitialCycle, logOut)
	}()

	wg.Wait()
	fmt.Fprintf(logOut, "[%s] daemon: stopped pid=%d\n", daemonStamp(), pid)
	return 0
}

func daemonStamp() string { return time.Now().UTC().Format(time.RFC3339) }

// guardDaemonLoop runs one pass of a daemon loop and turns a panic in it into an error, after writing
// the panic value and the stack to the daemon log. The loops are independent: a bug in one of them
// must not end the whole process, which would take the other loops and the portal down with it and
// leave every agent process group running with no daemon left to stop it. The deferred functions of
// the pass release its locks and stop its work while the panic unwinds, and the caller starts the
// loop again after its usual delay.
func guardDaemonLoop(name string, logOut io.Writer, pass func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(logOut, "[%s] daemon: %s panicked: %v\n%s", daemonStamp(), name, r, debug.Stack())
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return pass()
}

// idleDelayKey names the setting that lets a cycle start before its tick, as the daemon's log and
// `foreman-kit daemon status` say it.
var idleDelayKey = settings.CycleEveryWhenOutOfWork.Where()

// daemonPipelineLine says in the daemon log what runs continuously and what runs on the interval.
func daemonPipelineLine(opts DaemonOptions, interval time.Duration) string {
	switch {
	case opts.ServeMergerFunc != nil && opts.ServeDispatcherFunc != nil:
		line := fmt.Sprintf("merging and dispatching continuously; the cycle judges every %s", interval)
		if advancesCycle(opts) {
			line += fmt.Sprintf(", or %s after the last one began once a worker is free with nothing to start and the queue has room (%s, %s)", opts.IdleDelay, idleDelayKey, settings.QueueMaxOpen.Where())
		}
		return line
	case opts.ServeMergerFunc != nil:
		return fmt.Sprintf("merging continuously; the cycle judges and dispatches every %s", interval)
	default:
		return fmt.Sprintf("the cycle judges, merges and dispatches every %s", interval)
	}
}

// advancesCycle reports whether this daemon may start a cycle before its tick: the setting is on and a
// continuous dispatcher tells it what each pass left.
func advancesCycle(opts DaemonOptions) bool {
	return opts.IdleDelay > 0 && opts.ServeDispatcherFunc != nil && opts.DispatchLoadFunc != nil
}

// sameCycleSchedule reports whether a and b say the same about the next cycle.
func sameCycleSchedule(a, b store.CycleSchedule) bool {
	return a.Interval == b.Interval && a.IdleDelay == b.IdleDelay && a.Waits == b.Waits &&
		a.Latest.Equal(b.Latest) && a.Earliest.Equal(b.Earliest)
}

// cycleLoop runs the cycle's judgment until ctx is cancelled: one cycle at start (unless noInitialCycle),
// one per interval and, with an idle delay, one before the tick whenever the pipeline has run out of
// work (decideAdvance). With a continuous merger and dispatcher the cycle only judges and records
// (SkipMerge, SkipDispatch) and wakes the dispatcher when it ends, so what it queued starts at once.
// A cycle still running when the ticker fires drops that tick; the "cycle" lock RunCycle takes keeps a
// cron cycle and this loop from judging at the same time.
//
// The tick restarts when a cycle starts before it: the interval is the longest the daemon waits between
// the start of two cycles, so no tick falls a few minutes after an early cycle.
func cycleLoop(ctx context.Context, root string, st *store.Store, opts DaemonOptions, interval time.Duration, noInitialCycle bool, logOut io.Writer) {
	cOpts := opts.CycleOpts
	cOpts.Detach = false
	if opts.ServeMergerFunc != nil {
		cOpts.SkipMerge = true
	}

	// What the loop remembers of the last cycle it started, for decideAdvance: its stamp, when the
	// loop tried to start it, and when it handed what it queued to the dispatcher (when it ended, for
	// a cycle that did not get that far).
	var ownStamp string
	var triedAt, queuedAt time.Time
	notified := false
	if opts.ServeDispatcherFunc != nil {
		cOpts.SkipDispatch = true
		cOpts.DispatchFunc = nil
		if opts.Notifier != nil {
			cOpts.Notify = func() {
				queuedAt, notified = time.Now(), true
				opts.Notifier.BacklogChanged()
			}
		}
	}

	runOne := func(trigger string) {
		if ctx.Err() != nil {
			return
		}
		// The delay before an early cycle counts from this try even when the cycle does not get to
		// record itself, so a cycle that cannot start is not tried again at once.
		triedAt = time.Now()
		if st != nil {
			if ws, err := st.GetWorkspaceState(ctx); err == nil && ws.Paused {
				fmt.Fprintf(logOut, "[%s] daemon: workspace paused (%s); skipping cycle tick\n", daemonStamp(), ws.PauseReason)
				return
			}
		}
		if fileExists(store.RuntimePath(root, "PAUSE")) {
			fmt.Fprintf(logOut, "[%s] daemon: PAUSE file present; skipping cycle tick\n", daemonStamp())
			return
		}
		c := cOpts
		c.Trigger = trigger
		if c.Stamp == "" {
			c.Stamp = newCycleStamp()
		}
		ownStamp, notified = c.Stamp, false
		// A cycle that panics ends there; the next tick runs the next one.
		_ = guardDaemonLoop("cycle", logOut, func() error { return RunCycle(ctx, root, st, c, logOut) })
		if !notified {
			queuedAt = time.Now()
		}
	}

	advancing := st != nil && advancesCycle(opts)
	watchEvery := opts.BacklogWatchInterval
	if watchEvery <= 0 {
		watchEvery = 2 * time.Second
	}
	// decide asks whether the next cycle starts now, before its tick. queue is how full it found the
	// queue.
	var queue queueState
	decide := func() advanceDecision {
		if !advancing {
			return advanceDecision{State: advanceOff}
		}
		in := advanceInput{Now: time.Now(), IdleDelay: opts.IdleDelay, LastStart: triedAt, QueuedAt: queuedAt}
		recent, err := st.RecentCycleMovement(ctx, 2)
		if err != nil {
			return advanceDecision{State: advanceNoCycle}
		}
		in.Report, in.HaveReport = opts.DispatchLoadFunc()
		if len(recent) > 0 {
			latest := recent[0]
			if latest.StartedAt.After(in.LastStart) {
				in.LastStart = latest.StartedAt
			}
			if latest.Stamp != ownStamp {
				// A cycle this loop did not run: someone asked for it from the command line, a
				// scheduler or the portal. The dispatcher learns of what it queued when the daemon's
				// watcher sees the backlog change, and while it still judges no report has seen
				// what it will queue.
				in.QueuedAt = latest.FinishedAt.Add(watchEvery)
				if latest.FinishedAt.IsZero() {
					in.HaveReport = false
				}
			}
			in.Moved = latest.Any() || (len(recent) > 1 && recent[1].Any())
		}
		// The queue is counted as the next cycle would count it, which reads the backlog and the
		// store: only once nothing cheaper to tell holds the cycle back. When it cannot be counted,
		// nothing says it is full.
		if pre := decideAdvance(in).State; pre == advanceNow || pre == advanceNoMovement {
			queue, in.QueueFull = queueIsFull(ctx, root, st)
		}
		d := decideAdvance(in)
		if d.State == advanceNow && pipelinePaused(ctx, root, st) {
			d.State = advanceBusy // a paused pipeline starts nothing, and the cycle would be skipped
		}
		return d
	}

	// The schedule the daemon keeps with its lock, for `foreman-kit daemon status` and the portal.
	// It is rewritten only when it changes.
	var nextTick time.Time
	var published store.CycleSchedule
	publish := func(d advanceDecision) {
		if st == nil || nextTick.IsZero() {
			return
		}
		sched := store.CycleSchedule{Interval: interval, Latest: nextTick}
		if advancing {
			sched.IdleDelay, sched.Earliest, sched.Waits = opts.IdleDelay, d.NotBefore, string(d.State)
		}
		if sameCycleSchedule(sched, published) {
			return
		}
		if st.SetCycleSchedule(ctx, os.Getpid(), sched) == nil {
			published = sched
		}
	}

	if !noInitialCycle {
		runOne(store.CycleTriggerStartup)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	nextTick = time.Now().Add(interval)
	// run starts a cycle. While it runs the schedule says what follows it: the delay counts from its
	// start.
	run := func(trigger string) {
		d := advanceDecision{State: advanceOff}
		if advancing {
			d = advanceDecision{State: advanceDelay, NotBefore: time.Now().Add(opts.IdleDelay)}
		}
		publish(d)
		runOne(trigger)
		select { // a tick that fired during a long cycle is dropped, not run at once
		case <-ticker.C:
		default:
		}
		for now := time.Now(); !nextTick.After(now); {
			nextTick = nextTick.Add(interval)
		}
	}
	var wake <-chan struct{}
	if advancing {
		wake = opts.DispatchSignal
	}
	var braked, full time.Time // the cycle start for which each brake is already in the log
	for ctx.Err() == nil {
		d := decide()
		if d.State == advanceNow {
			fmt.Fprintf(logOut, "[%s] daemon: a worker is free with nothing to start and the last cycle began %s ago (%s: %s): the cycle runs now instead of at %s\n",
				daemonStamp(), time.Since(d.NotBefore.Add(-opts.IdleDelay)).Round(time.Second), idleDelayKey, opts.IdleDelay, nextTick.UTC().Format(time.RFC3339))
			ticker.Reset(interval)
			nextTick = time.Now().Add(interval)
			run(store.CycleTriggerOutOfWork)
			continue
		}
		publish(d)
		if d.State == advanceQueueFull && !d.NotBefore.Equal(full) {
			full = d.NotBefore
			fmt.Fprintf(logOut, "[%s] daemon: a worker is free with nothing to start, but the queue takes no more (%s): the next cycle waits for its tick at %s\n",
				daemonStamp(), queue, nextTick.UTC().Format(time.RFC3339))
		}
		if d.State == advanceNoMovement && !d.NotBefore.Equal(braked) {
			braked = d.NotBefore
			fmt.Fprintf(logOut, "[%s] daemon: a worker is free with nothing to start, but nothing was built, blocked or integrated in the last two cycles: the next cycle waits for its tick at %s\n",
				daemonStamp(), nextTick.UTC().Format(time.RFC3339))
		}
		var delay *time.Timer
		var delayed <-chan time.Time
		if d.State == advanceDelay {
			delay = time.NewTimer(time.Until(d.NotBefore))
			delayed = delay.C
		}
		select {
		case <-ctx.Done():
		case tick := <-ticker.C:
			nextTick = tick.Add(interval)
			run(store.CycleTriggerInterval)
		case <-wake:
		case <-delayed:
		}
		if delay != nil {
			delay.Stop()
		}
	}
}

// watchPipelineInputs wakes the continuous loops when an input they do not observe themselves changes:
// .foreman-kit/agent_backlog.json, edited by a human, a cycle or the portal (its stat signature: inode, mtime and
// size, like the Ruby @backlog_sig; a stat every few seconds is cheap and needs no file-watching
// dependency), the pause state, which a resume from the CLI clears without signaling anyone, and
// .foreman-kit/pipeline.yaml and .foreman-kit/agents.yaml, which both loops read again when they wake up.
func watchPipelineInputs(ctx context.Context, root string, st *store.Store, every time.Duration, n PipelineNotifier) error {
	backlogPath := filepath.Join(root, filepath.FromSlash(BacklogFile))
	sig := statSignature(backlogPath)
	configPaths := []string{filepath.Join(root, ".foreman-kit", "pipeline.yaml"), filepath.Join(root, ".foreman-kit", "agents.yaml")}
	configSig := func() string {
		var parts []string
		for _, p := range configPaths {
			parts = append(parts, statSignature(p))
		}
		return strings.Join(parts, "|")
	}
	cfgSig := configSig()
	wasPaused := pipelinePaused(ctx, root, st)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if cur := statSignature(backlogPath); cur != sig {
			sig = cur
			n.BacklogChanged()
		}
		if cur := configSig(); cur != cfgSig {
			cfgSig = cur
			n.ReadyEnqueued()
			n.BacklogChanged()
		}
		paused := pipelinePaused(ctx, root, st)
		if wasPaused && !paused {
			n.ReadyEnqueued()
			n.BacklogChanged()
		}
		wasPaused = paused
	}
}

// statSignature identifies a file's current content cheaply; it is empty when the file is missing.
func statSignature(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	var ino uint64
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = uint64(sys.Ino)
	}
	return fmt.Sprintf("%d:%d:%d", ino, fi.ModTime().UnixNano(), fi.Size())
}

func pipelinePaused(ctx context.Context, root string, st *store.Store) bool {
	if st != nil {
		if ws, err := st.GetWorkspaceState(ctx); err == nil && ws.Paused {
			return true
		}
	}
	return fileExists(store.RuntimePath(root, "PAUSE"))
}

// syncWriter serializes writes from the daemon's concurrent loops to one log.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// daemonStopTimeout is how long StopDaemon waits after SIGTERM before SIGKILL. On SIGTERM the daemon
// cancels its dispatcher and merger (whose agents and gates get proc.DefaultWaitDelay to stop their process
// groups) and then drains the jobs of its web portal (which get the same grace); killing the daemon sooner
// leaves those process groups running with nobody left to stop them.
const daemonStopTimeout = 2*proc.DefaultWaitDelay + 15*time.Second

// liveDaemonLock returns the "daemon" lock row only when its owner is alive and is still the process that
// took it: store.IsLockStale rejects rows whose heartbeat TTL expired, whose PID is dead, and whose PID now
// belongs to a process started after the row was written (PID reuse after a crash or reboot, including a
// container that restarts the daemon with the same PID as before).
func liveDaemonLock(ctx context.Context, st *store.Store) *store.LockRecord {
	if st == nil {
		return nil
	}
	dl, err := st.GetLock(ctx, "daemon")
	if err != nil || dl == nil || dl.PID <= 0 {
		return nil
	}
	if store.IsLockStale(*dl, "", time.Now().UTC().Unix()) {
		return nil
	}
	return dl
}

// StopDaemon stops the running project daemon for `root`.
func StopDaemon(ctx context.Context, root string, st *store.Store, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	pidFile := store.RuntimePath(root, "daemon.pid")
	now := time.Now().UTC().Unix()

	pid := 0
	haveRow := false
	if st != nil {
		if dl, err := st.GetLock(ctx, "daemon"); err == nil && dl != nil {
			haveRow = true
			// Judge the row as local (pass its own host): only a local PID can be signaled, and macOS
			// changes os.Hostname() with the network.
			switch {
			case dl.PID <= 0 || store.IsLockStale(*dl, dl.Host, now):
				_ = st.ReleaseLock(ctx, "daemon", dl.PID)
			case dl.PID == os.Getpid():
				fmt.Fprintln(stderr, "daemon: this process is the daemon; not signaling itself")
				return 1
			default:
				pid = dl.PID
			}
		}
	}
	if !haveRow {
		// Without a lock row, trust the runtime daemon.pid only if that PID still belongs to a process that was
		// already running when the file was written, which rules out a PID reused after a crash or reboot.
		if raw, err := os.ReadFile(pidFile); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && p > 0 && p != os.Getpid() {
				if info, err := os.Stat(pidFile); err == nil {
					rec := store.LockRecord{LockKey: "daemon", OwnerType: "daemon", PID: p, AcquiredAt: info.ModTime().Unix()}
					if !store.IsLockStale(rec, "", now) {
						pid = p
					}
				}
			}
		}
	}
	if pid <= 0 {
		_ = os.Remove(pidFile)
		fmt.Fprintln(stdout, "daemon: not running")
		return 0
	}

	_ = syscall.Kill(pid, syscall.SIGTERM)
	fmt.Fprintf(stdout, "daemon: stopping pid=%d (waiting up to %s for its agents to stop)\n", pid, daemonStopTimeout)
	deadline := time.Now().Add(daemonStopTimeout)
	for store.IsPIDAlive(pid) && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			fmt.Fprintf(stderr, "daemon: pid %d is still stopping its agents; not forcing it\n", pid)
			return 1
		case <-time.After(100 * time.Millisecond):
		}
	}
	if store.IsPIDAlive(pid) {
		fmt.Fprintf(stderr, "daemon: pid %d did not exit %s after SIGTERM; killing it\n", pid, daemonStopTimeout)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if st != nil {
		_ = st.ReleaseLock(ctx, "daemon", pid)
	}
	_ = os.Remove(pidFile)
	fmt.Fprintf(stdout, "daemon: stopped (pid=%d)\n", pid)
	return 0
}

// StatusDaemon prints the status of the project daemon for `root`.
func StatusDaemon(ctx context.Context, root string, st *store.Store, stdout io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if st != nil {
		if dl := liveDaemonLock(ctx, st); dl != nil {
			meta := parseDaemonMetadata(dl.MetadataJSON)
			started := time.Unix(dl.AcquiredAt, 0).UTC().Format(time.RFC3339)
			interval := asString(meta["interval"])
			if interval == "" {
				interval = "1h0m0s"
			}
			webURL := asString(meta["web_url"])
			if webURL != "" {
				fmt.Fprintf(stdout, "daemon: running (pid=%d started=%s interval=%s web=%s log=%s)\n",
					dl.PID, started, interval, webURL, store.RuntimePath(root, "daemon.log"))
			} else {
				fmt.Fprintf(stdout, "daemon: running (pid=%d started=%s interval=%s web=off log=%s)\n",
					dl.PID, started, interval, store.RuntimePath(root, "daemon.log"))
			}
			if given := givenForTheRun(GivenToDaemon(dl.MetadataJSON)); given != "" {
				fmt.Fprintf(stdout, "daemon: given for this run: %s (in no file: it holds until the daemon stops)\n", given)
			}
			if sched, err := st.CycleSchedule(ctx); err == nil && sched != nil && !sched.Latest.IsZero() {
				fmt.Fprintf(stdout, "daemon: %s\n", cycleScheduleLine(*sched))
			}
			return 0
		}
	}
	fmt.Fprintln(stdout, "daemon: stopped")
	return 0
}

// cycleScheduleLine says when the daemon runs its next cycle, as `foreman-kit daemon status` prints it.
func cycleScheduleLine(sched store.CycleSchedule) string {
	line := fmt.Sprintf("next cycle at %s at the latest", sched.Latest.UTC().Format(time.RFC3339))
	if sched.IdleDelay <= 0 {
		return line
	}
	line += fmt.Sprintf("; with a worker free and nothing to start, %s after the last one began (%s)", sched.IdleDelay, idleDelayKey)
	if !sched.Earliest.IsZero() {
		line += fmt.Sprintf(", from %s", sched.Earliest.UTC().Format(time.RFC3339))
	}
	switch advanceState(sched.Waits) {
	case advanceDelay:
		line += ": that delay has not passed"
	case advanceNoReport:
		line += ": the dispatcher has not looked at the backlog since the last cycle"
	case advanceBusy:
		line += ": the workers have work"
	case advanceQueueFull:
		line += fmt.Sprintf(": as many changes are open as %s allows, so it waits for the tick", queueMaxOpenKey)
	case advanceNoMovement:
		line += ": nothing was built, blocked or integrated in the last two cycles, so it waits for the tick"
	case advanceNoCycle:
		line += ": no cycle has run yet"
	}
	return line
}

// RunDaemonCLI handles `foreman-kit daemon [start|run|stop|status|restart] [flags...]`.
func RunDaemonCLI(ctx context.Context, root string, st *store.Store, baseOpts DaemonOptions, args []string, stdout, stderr io.Writer) int {
	sub := "start"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}

	opts := baseOpts
	if sub == "start" || sub == "restart" {
		opts.Detach = true
	} else if sub == "run" {
		opts.Detach = false
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-d" || a == "--daemon":
			opts.Detach = true
		case a == "--foreground":
			opts.Detach = false
		case a == "-w" || a == "--web" || a == "--web=true":
			opts.Web = true
			opts.WebSet = true
		case a == "--web=false" || a == "--no-web":
			opts.Web = false
			opts.WebSet = true
		case a == "-dw" || a == "-wd":
			opts.Detach = true
			opts.Web = true
			opts.WebSet = true
		case a == "--port" && i+1 < len(args):
			i++
			p, err := strconv.Atoi(args[i])
			if err != nil || p <= 0 {
				fmt.Fprintf(stderr, "daemon: invalid port %q\n", args[i])
				return 2
			}
			opts.Port = p
			opts.PortExplicit = true
		case strings.HasPrefix(a, "--port="):
			val := strings.TrimPrefix(a, "--port=")
			p, err := strconv.Atoi(val)
			if err != nil || p <= 0 {
				fmt.Fprintf(stderr, "daemon: invalid port %q\n", val)
				return 2
			}
			opts.Port = p
			opts.PortExplicit = true
		case a == "--port-explicit":
			opts.PortExplicit = true
		case a == "--bind" && i+1 < len(args):
			i++
			opts.Bind = args[i]
		case strings.HasPrefix(a, "--bind="):
			opts.Bind = strings.TrimPrefix(a, "--bind=")
		case a == "--interval" && i+1 < len(args):
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				if mins, err2 := strconv.Atoi(args[i]); err2 == nil && mins > 0 {
					d = time.Duration(mins) * time.Minute
				} else {
					fmt.Fprintf(stderr, "daemon: invalid interval %q\n", args[i])
					return 2
				}
			}
			opts.Interval = d
		case strings.HasPrefix(a, "--interval="):
			val := strings.TrimPrefix(a, "--interval=")
			d, err := time.ParseDuration(val)
			if err != nil {
				if mins, err2 := strconv.Atoi(val); err2 == nil && mins > 0 {
					d = time.Duration(mins) * time.Minute
				} else {
					fmt.Fprintf(stderr, "daemon: invalid interval %q\n", val)
					return 2
				}
			}
			opts.Interval = d
		case a == "--no-initial-cycle":
			opts.NoInitialCycle = true
		default:
			fmt.Fprintf(stderr, "Usage: foreman-kit daemon [start|run|stop|status|restart] [-d|--daemon] [-w|--web] [--port 4567] [--bind 127.0.0.1] [--interval 60m]\n")
			return 2
		}
	}

	switch sub {
	case "status":
		return StatusDaemon(ctx, root, st, stdout)
	case "stop":
		return StopDaemon(ctx, root, st, stdout, stderr)
	case "restart":
		// A daemon that would not start again is not stopped: the one that is running goes on with
		// what it read when it started.
		if why := daemonFileUnusable(root); why != "" {
			fmt.Fprintf(stderr, "daemon: %s: not restarted, and the daemon that is running was left as it is\n", why)
			return 1
		}
		_ = StopDaemon(ctx, root, st, stdout, stderr)
		return StartDaemon(ctx, root, st, opts, stdout, stderr)
	case "start", "run":
		return StartDaemon(ctx, root, st, opts, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "Usage: foreman-kit daemon [start|run|stop|status|restart] [-d|--daemon] [-w|--web] [--port 4567] [--bind 127.0.0.1] [--interval 60m]\n")
		return 2
	}
}

// givenForTheRun writes what --set gave a run the way its command line did ("" for nothing):
// "--set pipeline.dispatch.workers=4 --set evaluator.enabled=false".
func givenForTheRun(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return "--set " + strings.Join(values, " --set ")
}

// GivenToDaemon returns what the daemon whose lock holds metadataJSON was started with
// `foreman-kit --set`: each value as "<file>.<key>=<value>", sorted by key, and nil when it was
// given nothing. Those values are in no file and hold until that daemon stops, so whatever shows
// the daemon (status, the Web Portal) says them; how the lock records them is this function's to
// know.
func GivenToDaemon(metadataJSON string) []string {
	list, _ := parseDaemonMetadata(metadataJSON)["set"].([]any)
	var values []string
	for _, item := range list {
		if text, ok := item.(string); ok && text != "" {
			values = append(values, text)
		}
	}
	return values
}

func parseDaemonMetadata(raw string) map[string]any {
	var m map[string]any
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m
}
