package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// DefaultWaitDelay is the grace period between SIGTERM and SIGKILL when stopping a process group.
const DefaultWaitDelay = 60 * time.Second

// DefaultUTF8Locale returns a UTF-8 locale guaranteed to exist on the host OS
// ("C.UTF-8" on Linux distros/containers, "en_US.UTF-8" on macOS, or an existing UTF-8 locale from env).
func DefaultUTF8Locale() string {
	for _, k := range []string{"LC_ALL", "LANG"} {
		if v := strings.TrimSpace(os.Getenv(k)); strings.Contains(strings.ToUpper(v), "UTF-8") || strings.Contains(strings.ToUpper(v), "UTF8") {
			return v
		}
	}
	if runtime.GOOS == "linux" {
		return "C.UTF-8"
	}
	return "en_US.UTF-8"
}

// EnsureUTF8SubprocessEnv guarantees that every subprocess receives UTF-8 locale and non-interactive git/CI variables
// even when launched from launchd/cron/systemd with an empty locale environment.
func EnsureUTF8SubprocessEnv(baseEnv []string) []string {
	if len(baseEnv) == 0 {
		baseEnv = os.Environ()
	}
	out := make([]string, 0, len(baseEnv)+8)
	hasKey := make(map[string]bool, len(baseEnv))
	for _, kv := range baseEnv {
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			hasKey[kv[:idx]] = true
		}
		out = append(out, kv)
	}
	utf8Loc := DefaultUTF8Locale()
	defaults := [][2]string{
		{"LANG", utf8Loc},
		{"LC_ALL", utf8Loc},
		{"CI", "1"},
		{"NONINTERACTIVE", "1"},
		{"GIT_TERMINAL_PROMPT", "0"},
		{"GIT_MERGE_AUTOEDIT", "no"},
	}
	for _, kv := range defaults {
		if !hasKey[kv[0]] {
			out = append(out, kv[0]+"="+kv[1])
		}
	}
	return out
}

// envLookup returns the value os/exec passes to the child for key: when env holds duplicate keys,
// the last occurrence wins.
func envLookup(env []string, key string) (string, bool) {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return env[i][len(prefix):], true
		}
	}
	return "", false
}

// RunOptions configures a bounded subprocess execution.
type RunOptions struct {
	Dir         string
	Env         []string
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	Timeout     time.Duration
	GracePeriod time.Duration // WaitDelay before SIGKILL (default 60s)
	// ActivityPaths are files whose changes the idle watchdog counts as activity (a detached
	// agent's session store), besides the process tree, the work directory and the output.
	ActivityPaths []string
	IdleTimeout   time.Duration // 0 disables the idle watchdog
	// IdleCPUPercent is the share of one core, in percent, below which the CPU the command's
	// processes use is not activity for the idle watchdog (a frozen CLI still wakes up for its
	// timers); 0 counts any CPU use.
	IdleCPUPercent float64
	// OnIdle, when set, is called by the idle watchdog after each of its polls with how long the
	// command has shown no activity (0: it did something since the previous poll). It runs on the
	// watchdog's goroutine while the command's output is being copied, so it must not write to
	// Stdout or Stderr.
	OnIdle       func(idle time.Duration)
	WatchdogPoll time.Duration // optional custom poll interval for tests
	Store        *store.Store
	ExecID       string
	Stream       string // default "combined"
}

// RunResult holds the outcome of a bounded subprocess execution.
type RunResult struct {
	Output      string
	ExitCode    int
	TimedOut    bool
	IdleStalled bool
	IdleSeconds int
	Duration    time.Duration
	// PID is the process the command ran as (0 when it never started).
	PID int
}

// outputSink is shared by the stdout and stderr collectors of one process. When Stdout and Stderr
// differ, os/exec copies the two pipes from separate goroutines, so every write is serialized here:
// the combined buffer, the caller's (possibly non-thread-safe) writers, and log_chunks order
// numbers all stay consistent.
type outputSink struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	st     *store.Store
	execID string
	stream string
	// logCtx carries the scope of the run (the execution attempt the output belongs to) without its
	// cancellation: output that arrives while the run is being stopped is still recorded.
	logCtx      context.Context
	outputBytes atomic.Int64
}

func (s *outputSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// streamCollector forwards one output stream of a process into its shared outputSink.
type streamCollector struct {
	sink *outputSink
	out  io.Writer
}

func (c *streamCollector) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s := c.sink
	s.outputBytes.Add(int64(len(p)))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Write(p)
	if c.out != nil {
		_, _ = c.out.Write(p)
	}
	if s.st != nil && s.execID != "" {
		_ = s.st.AppendLogChunk(s.logCtx, s.execID, s.stream, string(p))
	}
	return len(p), nil
}

// sameWriter reports whether a and b are the same writer. It tolerates dynamic types that are not
// comparable, for which == would panic.
func sameWriter(a, b io.Writer) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// outputDrainTimeout bounds how long RunBounded keeps reading output after the process group has been
// killed; by then only a descendant that left the group (for example through setsid) can still hold a
// write end of the output pipes.
const outputDrainTimeout = 5 * time.Second

// RunBounded runs a command in its own process group (Setpgid: true), sending SIGTERM to -pid on cancellation
// or timeout and SIGKILL after GracePeriod (60s by default), while streaming output to store.AppendLogChunk and
// optionally monitoring inactivity via the idle watchdog. When the group leader exits, whatever it left running
// in its process group (a backgrounded dev server, a test worker, a watcher) is killed, so it never outlives the
// command, keeps its port, or holds the output pipes open: at once after a normal exit, and after what is left
// of the grace period when the group was being stopped. Real daemons that leave the group (setsid) are not
// touched.
func RunBounded(ctx context.Context, opts RunOptions, name string, args ...string) (*RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	grace := opts.GracePeriod
	if grace <= 0 {
		grace = DefaultWaitDelay
	}
	streamName := opts.Stream
	if streamName == "" {
		streamName = "combined"
	}

	runCtx := ctx
	var cancelTimeout context.CancelFunc
	if opts.Timeout > 0 {
		runCtx, cancelTimeout = context.WithTimeout(ctx, opts.Timeout)
		defer cancelTimeout()
	}

	cmdCtx, cancelCmd := context.WithCancel(runCtx)
	defer cancelCmd()

	cmd := exec.CommandContext(cmdCtx, name, args...)
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}
	cmd.Env = EnsureUTF8SubprocessEnv(opts.Env)
	cmd.Stdin = opts.Stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = grace

	stop := &groupStop{grace: grace, kill: syscall.Kill}

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return stop.terminate(cmd.Process.Pid)
	}

	sink := &outputSink{
		st:     opts.Store,
		execID: opts.ExecID,
		stream: streamName,
		logCtx: context.WithoutCancel(ctx),
	}

	// RunBounded creates the output pipes itself instead of letting os/exec create them, so cmd.Wait
	// returns as soon as the group leader exits. With os/exec-managed pipes, Wait blocks for WaitDelay
	// while any leftover process holds a write end and then reports ErrWaitDelay (exit 1) for a command
	// that succeeded.
	type outPipe struct {
		r   *os.File
		w   *os.File
		dst io.Writer
	}
	var pipes []*outPipe
	closeWriteEnds := func() {
		for _, p := range pipes {
			if p.w != nil {
				_ = p.w.Close()
				p.w = nil
			}
		}
	}
	closeAll := func() {
		closeWriteEnds()
		for _, p := range pipes {
			_ = p.r.Close()
		}
	}
	newPipe := func(dst io.Writer) (*os.File, error) {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		pipes = append(pipes, &outPipe{r: r, w: w, dst: dst})
		return w, nil
	}

	started := time.Now()
	stdoutW, err := newPipe(&streamCollector{sink: sink, out: opts.Stdout})
	if err != nil {
		return &RunResult{ExitCode: 1, Duration: time.Since(started)}, err
	}
	cmd.Stdout = stdoutW
	// The same writer for both streams shares one pipe, which preserves their interleaving.
	cmd.Stderr = stdoutW
	if opts.Stderr != nil && !sameWriter(opts.Stderr, opts.Stdout) {
		stderrW, err := newPipe(&streamCollector{sink: sink, out: opts.Stderr})
		if err != nil {
			closeAll()
			return &RunResult{ExitCode: 1, Duration: time.Since(started)}, err
		}
		cmd.Stderr = stderrW
	}

	if err := cmd.Start(); err != nil {
		closeAll()
		exitCode := 127
		if !errors.Is(err, exec.ErrNotFound) && !os.IsNotExist(err) {
			exitCode = 1
		}
		return &RunResult{
			Output:   sink.String(),
			ExitCode: exitCode,
			Duration: time.Since(started),
		}, err
	}

	// The child holds its own copies of the write ends; closing ours lets EOF arrive once the child and
	// its descendants are gone.
	closeWriteEnds()
	var copyWG sync.WaitGroup
	for _, p := range pipes {
		copyWG.Add(1)
		go func(p *outPipe) {
			defer copyWG.Done()
			_, _ = io.Copy(p.dst, p.r)
		}(p)
	}

	pid := cmd.Process.Pid
	if opts.Store != nil && opts.ExecID != "" {
		// Setpgid: the group id is the pid. Recorded so that if this process dies without stopping the
		// command (kill -9), the next orchestrator stops it instead of letting it keep editing a
		// worktree it then hands to a new agent.
		_ = opts.Store.AddExecutionProcessGroup(context.WithoutCancel(ctx), opts.ExecID, pid)
	}
	var idleStalled atomic.Bool
	var idleSec atomic.Int32
	var watchDone chan struct{}
	var stopWatch context.CancelFunc

	if opts.IdleTimeout > 0 {
		watchDone = make(chan struct{})
		var watchCtx context.Context
		watchCtx, stopWatch = context.WithCancel(context.Background())
		workDir := opts.Dir
		if workDir == "" {
			workDir, _ = os.Getwd()
		}
		childTmpDir, _ := envLookup(cmd.Env, "TMPDIR")
		go func() {
			defer close(watchDone)
			WatchIdle(watchCtx, WatchdogConfig{
				PID:           pid,
				WorkDir:       workDir,
				TmpDir:        privateTmpDir(childTmpDir),
				IdleLimit:     opts.IdleTimeout,
				PollInterval:  opts.WatchdogPoll,
				CPUPercent:    opts.IdleCPUPercent,
				OnPoll:        opts.OnIdle,
				OutputBytes:   func() int64 { return sink.outputBytes.Load() },
				ActivityPaths: opts.ActivityPaths,
				OnStall: func(stalledSeconds int) {
					idleSec.Store(int32(stalledSeconds))
					idleStalled.Store(true)
					cancelCmd()
				},
			})
		}()
	}

	waitErr := cmd.Wait()

	// The leader has exited and been reaped: deal with what it left in its process group.
	stop.leaderReaped(pid)

	copyDone := make(chan struct{})
	go func() {
		copyWG.Wait()
		close(copyDone)
	}()
	select {
	case <-copyDone:
	case <-time.After(outputDrainTimeout):
		// A descendant that left the process group still holds a write end: stop reading.
		closeAll()
		<-copyDone
	}
	closeAll()

	if stopWatch != nil {
		stopWatch()
		<-watchDone
	}

	res := &RunResult{
		PID:      pid,
		Output:   sink.String(),
		ExitCode: 0,
		Duration: time.Since(started),
	}

	if idleStalled.Load() {
		res.IdleStalled = true
		res.IdleSeconds = int(idleSec.Load())
		res.ExitCode = 124
		return res, nil
	}

	if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		res.TimedOut = true
		res.ExitCode = 124
		return res, nil
	}

	if waitErr != nil {
		var exitErr *exec.ExitError
		switch {
		case errors.As(waitErr, &exitErr):
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				res.ExitCode = 128 + int(ws.Signal())
			} else {
				res.ExitCode = exitErr.ExitCode()
			}
		case errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success():
			// The command itself succeeded; only the os/exec stdin copy outlived WaitDelay.
			res.ExitCode = 0
		default:
			res.ExitCode = 1
		}
		if ctx.Err() != nil && res.ExitCode == 137 {
			res.ExitCode = 143
		}
	}

	return res, nil
}

// groupStop carries out the stop of one command's process group for RunBounded: SIGTERM when the
// command is canceled, SIGKILL when the grace period ends.
//
// While the leader is unreaped its PID, which is the group's ID, cannot be given to another process.
// Once it has been reaped the ID is held only by the members still alive and is free as soon as the
// last one is gone: a process that is then given the same PID and leads its own group, as every
// command this kit starts does, would take a signal meant for the old group. So from the reaping on
// the timer no longer fires, and a group that was seen empty is never signaled again.
type groupStop struct {
	grace time.Duration
	kill  func(pid int, sig syscall.Signal) error // syscall.Kill; tests record the signals sent

	mu     sync.Mutex
	sentAt time.Time // when terminate sent SIGTERM; zero while it has not
	timer  *time.Timer
	reaped bool
}

// terminate sends SIGTERM to group pgid and arms the SIGKILL that follows after the grace period for
// as long as the leader has not been reaped.
func (g *groupStop) terminate(pgid int) error {
	err := g.kill(-pgid, syscall.SIGTERM)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timer == nil {
		g.sentAt = time.Now()
		g.timer = time.AfterFunc(g.grace, func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if !g.reaped {
				_ = g.kill(-pgid, syscall.SIGKILL)
			}
		})
	}
	return err
}

// leaderReaped is called once the leader of group pgid has exited and been reaped. It returns when
// the group is gone or has been killed.
//
// After a normal exit, what the command left running is killed at once. When the group was told to
// stop and its leader went first (a shell running a compound command dies on SIGTERM at once), the
// members it leaves got the same signal and keep the rest of the grace period to act on it (delete a
// lock or PID file, stop a browser or a server they started outside the group) before they are killed.
func (g *groupStop) leaderReaped(pgid int) {
	g.mu.Lock()
	g.reaped = true
	sentAt := g.sentAt
	if g.timer != nil {
		g.timer.Stop()
	}
	g.mu.Unlock()

	if !sentAt.IsZero() && waitProcessGroupExit(pgid, sentAt.Add(g.grace)) {
		return
	}
	_ = g.kill(-pgid, syscall.SIGKILL)
}

// waitProcessGroupExit waits until no process of group pgid is running any more and reports true, or
// reports false at deadline while one still is.
func waitProcessGroupExit(pgid int, deadline time.Time) bool {
	for pause := 5 * time.Millisecond; processGroupRunning(pgid); pause = min(2*pause, 200*time.Millisecond) {
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		time.Sleep(min(pause, left))
	}
	return true
}

// processGroupRunning reports whether a process of group pgid is still running. Zombies do not count:
// they have exited, and where nothing reaps the ones this process inherits (it is the init of a
// container) they would hold the group for the whole grace period.
func processGroupRunning(pgid int) bool {
	if pgid <= 0 || syscall.Kill(-pgid, 0) != nil {
		// ESRCH: the group is empty. macOS answers EPERM for a group that holds only zombies.
		return false
	}
	if runtime.GOOS != "linux" {
		return true
	}
	// Linux answers kill for a group of zombies too: look at the state of its members.
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%s/stat", e.Name()))
		if err != nil {
			continue
		}
		if state, pgrp, ok := parseProcStatStateGroup(string(raw)); ok && pgrp == pgid && state != 'Z' && state != 'X' {
			return true
		}
	}
	return false
}

// parseProcStatStateGroup extracts the state (field 3) and the process group (field 5) from the content
// of /proc/<pid>/stat. The command name (field 2) can contain spaces and parentheses, so the fields are
// located after its last closing parenthesis.
func parseProcStatStateGroup(stat string) (state byte, pgrp int, ok bool) {
	closeParen := strings.LastIndexByte(stat, ')')
	if closeParen < 0 {
		return 0, 0, false
	}
	// Fields after ")": 0=state, 1=ppid, 2=pgrp
	fields := strings.Fields(stat[closeParen+1:])
	if len(fields) < 3 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], pgrp, true
}
