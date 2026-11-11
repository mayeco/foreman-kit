package sim

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/orchestrator"
	"github.com/mayeco/foreman-kit/internal/proc"
	"github.com/mayeco/foreman-kit/internal/store"
)

// lockedBuffer collects the output of a daemon whose loops write while the scenario reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// simDaemon is a daemon running over the workspace of a stack as `foreman-kit daemon` runs: its
// continuous merger and dispatcher linked by one Hub, one worker, and the cycle judged by the agent
// of the cycle role.
type simDaemon struct {
	log      lockedBuffer
	began    time.Time
	cancel   context.CancelFunc
	exited   chan struct{}
	exitCode int
	stopped  bool
}

// startSimDaemon starts the daemon with a tick every interval and the idle delay given, without the
// portal. The caller stops it.
func startSimDaemon(ctx context.Context, st *Stack, dbStore *store.Store, interval, idleDelay time.Duration) *simDaemon {
	hub := orchestrator.NewHub()
	daemonCtx, cancel := context.WithCancel(ctx)
	d := &simDaemon{began: time.Now(), cancel: cancel, exited: make(chan struct{})}
	go func() {
		defer close(d.exited)
		d.exitCode = cycle.StartDaemon(daemonCtx, st.WorkspaceDir, dbStore, cycle.DaemonOptions{
			WebSet:    true,
			Web:       false,
			Interval:  interval,
			IdleDelay: idleDelay,
			CycleOpts: cycle.CycleOptions{
				SkipWorktreePrep: true,
				PreflightFunc: func(ctx context.Context, r string, w io.Writer) error {
					_, err := proc.RunPreflight(ctx, proc.PreflightOptions{Root: r, Store: dbStore, Stdout: w, Stderr: w})
					return err
				},
				AgentExecFunc: func(ctx context.Context, workdir, pf, engine string, timeout time.Duration, w io.Writer) error {
					r, err := proc.AgentExec(ctx, proc.AgentExecOptions{
						Workdir:    workdir,
						AppRoot:    st.WorkspaceDir,
						PromptFile: pf,
						Engines:    engine,
						Role:       "cycle",
						Phase:      "cycle",
						Store:      dbStore,
						Stdout:     w,
						Stderr:     w,
					})
					if err != nil || r.ExitCode != 0 {
						return fmt.Errorf("cycle agent exited %d", r.ExitCode)
					}
					return nil
				},
			},
			ServeMergerFunc: func(ctx context.Context, r string, out io.Writer) error {
				m, err := orchestrator.NewMerger(orchestrator.MergerOptions{Root: r, Store: dbStore, Hub: hub, Out: out})
				if err != nil {
					return err
				}
				defer m.Close()
				return m.Serve(ctx)
			},
			ServeDispatcherFunc: func(ctx context.Context, r string, out io.Writer) error {
				disp, err := orchestrator.NewDispatcher(orchestrator.DispatcherOptions{
					Root: r, Store: dbStore, Hub: hub, Out: out, MaxWorkers: 1, RepairAttempts: 2,
				})
				if err != nil {
					return err
				}
				defer disp.Close()
				return disp.Serve(ctx)
			},
			Notifier: hub,
			DispatchLoadFunc: func() (cycle.DispatchReport, bool) {
				load, ok := hub.DispatchLoad()
				return cycle.DispatchReport(load), ok
			},
			DispatchSignal: hub.Dispatched(),
		}, &d.log, &d.log)
	}()
	return d
}

// stop stops the daemon and waits until it has exited. Stopping it twice is harmless.
func (d *simDaemon) stop() {
	if !d.stopped {
		d.stopped = true
		d.cancel()
		<-d.exited
	}
}

// waitUntil waits until cond holds, at most limit. It fails when the daemon exits first.
func (d *simDaemon) waitUntil(what string, limit time.Duration, cond func() bool) error {
	deadline := time.Now().Add(limit)
	for !cond() {
		select {
		case <-d.exited:
			return fmt.Errorf("the daemon exited (%d) before %s\ndaemon log:\n%s", d.exitCode, what, d.log.String())
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting until %s\ndaemon log:\n%s", what, d.log.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}
