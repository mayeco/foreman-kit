package main

import (
	"context"
	"fmt"
	"io"
	"os/signal"
	"time"

	"github.com/spf13/cobra"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// foregroundFlags are the root options foreman-kit alone hands its daemon.
type foregroundFlags struct {
	port           int
	portSet        bool
	bind           string
	interval       time.Duration
	noInitialCycle bool
}

// runHere is foreman-kit with neither -d nor -w. In a Foreman application it runs the application's
// daemon in the foreground with its portal: the cycle, the dispatcher and the merger write their log
// to the terminal (and to daemon.log), and it runs until Ctrl+C. Anywhere else it prints the help.
func runHere(ctx context.Context, cmd *cobra.Command, stdout, stderr io.Writer, build func(*store.Store) cycle.DaemonOptions, f foregroundFlags) error {
	root := config.AppRoot("")
	if !store.IsApplication(root) {
		writeBanner(stderr)
		fmt.Fprintf(stderr, "foreman-kit: %s is not a Foreman application (no .foreman-kit/kit.lock): `foreman-kit init` makes one.\n\n", root)
		return cmd.Usage()
	}
	writeBanner(stderr)
	st, err := store.Open(root)
	if err != nil {
		fmt.Fprintf(stderr, "foreman-kit: %v\n", err)
		return exitCodeError{code: 1}
	}
	defer st.Close()

	opts := build(st)
	opts.Detach = false
	opts.Console = true
	opts.Web, opts.WebSet = true, true
	opts.Port, opts.PortExplicit = f.port, f.portSet
	opts.Bind = f.bind
	opts.Interval = f.interval
	opts.NoInitialCycle = f.noInitialCycle
	opts.Started = func(webURL, logPath string) {
		rows := []bannerRow{{"application", root}}
		if webURL != "" {
			s := styleFor(stderr)
			rows = append(rows, bannerRow{"portal", s.bold(s.green(webURL)) + s.dim("  (port open)")})
		}
		rows = append(rows, bannerRow{"log", "this terminal, and " + logPath}, bannerRow{"stop", "Ctrl+C"})
		writeServing(stderr, rows)
	}

	// The first Ctrl+C stops the daemon as `daemon stop` would: it stops its agents and waits for
	// its loops. A second one, while it waits, ends the process at once.
	go func() {
		<-ctx.Done()
		fmt.Fprintln(stderr, "\nforeman-kit: stopping: the agents are being stopped and the loops are ending (Ctrl+C again to quit at once)")
		signal.Reset(shutdownSignals()...)
	}()

	code := cycle.StartDaemon(ctx, root, st, opts, stdout, stderr)
	if code == 0 {
		fmt.Fprintln(stderr, "foreman-kit: stopped")
		return nil
	}
	return exitCodeError{code: code}
}
