package sim

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// runCycleOnDemand is the "cycle-on-demand" scenario. It runs the daemon as `foreman-kit daemon` does,
// with its continuous merger and dispatcher linked by one Hub, a tick ten minutes away and an idle delay
// of a few seconds, and verifies that:
//   - the startup cycle queues a change, which the dispatcher builds and the merger integrates;
//   - with the backlog empty and the worker free, the next cycle starts before its tick, says so
//     (cycles.trigger = out_of_work) and never starts sooner than the delay after the one before it;
//   - once two cycles in a row moved nothing, the next cycle waits for its tick.
func runCycleOnDemand(ctx context.Context, st *Stack, dbStore *store.Store, check func(desc string, cond bool) error) error {
	const (
		interval  = 10 * time.Minute
		idleDelay = 3 * time.Second
	)
	WriteAgentsYAML(st.WorkspaceDir,
		[]string{"sim-agent-clean", "sim-agent-cycle"},
		map[string][]string{
			"plan":  {"sim-agent-clean"},
			"build": {"sim-agent-clean"},
			"cycle": {"sim-agent-cycle"},
		},
		"in_order",
		nil,
	)
	commitWorkspaceConfig(ctx, st.WorkspaceDir, "configure cycle agent")

	d := startSimDaemon(ctx, st, dbStore, interval, idleDelay)
	defer d.stop()
	log, began, waitUntil, stop := &d.log, d.began, d.waitUntil, d.stop

	// cycles lists the cycles on record with what each moved, oldest first.
	cycles := func() []store.CycleMovement {
		recent, _ := dbStore.RecentCycleMovement(ctx, 100)
		for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
			recent[i], recent[j] = recent[j], recent[i]
		}
		return recent
	}
	triggerOf := func(stamp string) string {
		if c, _ := dbStore.GetCycle(ctx, stamp); c != nil {
			return c.Trigger
		}
		return ""
	}

	if err := waitUntil("the change the startup cycle queued is integrated", 5*time.Minute, func() bool {
		moved, _ := dbStore.MovementBetween(ctx, began, time.Time{})
		return moved.Built >= 1 && moved.Integrated >= 1
	}); err != nil {
		return err
	}
	if err := check("the startup cycle queued add-audit-trail, and the daemon's dispatcher and merger built and integrated it", true); err != nil {
		return err
	}
	queuedByStartup := false
	if all := cycles(); len(all) > 0 {
		queued, _ := dbStore.ChangesQueuedByCycle(ctx, all[0].Stamp)
		queuedByStartup = len(queued) == 1 && queued[0] == "add-audit-trail"
	}
	if err := check("the registry says the startup cycle is the one that queued add-audit-trail", queuedByStartup); err != nil {
		return err
	}

	if err := waitUntil("a cycle starts before its tick", 5*time.Minute, func() bool {
		for _, c := range cycles() {
			if !c.FinishedAt.IsZero() && triggerOf(c.Stamp) == store.CycleTriggerOutOfWork {
				return true
			}
		}
		return false
	}); err != nil {
		return err
	}
	if err := check("with the backlog empty and the worker free, the next cycle started before its tick and says so (out_of_work)",
		strings.Contains(log.String(), "the cycle runs now instead of at")); err != nil {
		return err
	}

	// Building the change and integrating it are two movements, and the second can come after the
	// brake already engaged: it releases the brake, as any movement does. Nothing else can move after
	// it, so the brake that follows two more unmoved cycles is the one that holds.
	var braked []store.CycleMovement
	if err := waitUntil("the next cycle waits for its tick", 5*time.Minute, func() bool {
		sched, _ := dbStore.CycleSchedule(ctx)
		braked = cycles()
		n := len(braked)
		return sched != nil && sched.Waits == "no_movement" && n >= 3 &&
			!braked[n-1].FinishedAt.IsZero() && !braked[n-1].Any() && !braked[n-2].Any()
	}); err != nil {
		return err
	}
	time.Sleep(3 * idleDelay)
	after := cycles()
	if err := check("after two cycles in a row moved nothing, the next cycle waits for its tick instead of running every few seconds",
		len(after) == len(braked) && strings.Contains(log.String(), "the next cycle waits for its tick at")); err != nil {
		return err
	}

	onlyEarly, spaced := true, true
	for i, c := range after {
		want := store.CycleTriggerOutOfWork
		if i == 0 {
			want = store.CycleTriggerStartup
		}
		if triggerOf(c.Stamp) != want {
			onlyEarly = false
		}
		if i > 0 && c.StartedAt.Sub(after[i-1].StartedAt) < idleDelay {
			spaced = false
		}
	}
	if err := check("every cycle after the startup one was started by a pipeline out of work, never sooner than the idle delay after the one before it",
		onlyEarly && spaced); err != nil {
		return err
	}

	var status bytes.Buffer
	_ = cycle.StatusDaemon(ctx, st.WorkspaceDir, dbStore, &status)
	if err := check("daemon status says when the next cycle runs at the latest and why it does not run sooner",
		strings.Contains(status.String(), "next cycle at ") && strings.Contains(status.String(), "so it waits for the tick")); err != nil {
		return err
	}

	stop()
	return check("add-audit-trail is on origin/master", isIntegrated(ctx, st.WorkspaceDir, "add-audit-trail"))
}
