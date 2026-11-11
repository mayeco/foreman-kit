package sim

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// runQueueFull is the "queue-full" scenario. It runs the daemon as cycle-on-demand does, over a queue
// that takes two changes (queue.max_open: 2) and a backlog of two: broken-gate, whose build never
// passes its gate, and waits-for-nobody, which depends on a change nobody builds. It verifies that:
//   - the startup cycle counts two open changes of two, tells its judgment that the queue is full,
//     queues nothing, and leaves the proposal of that judgment approved and waiting for room;
//   - once broken-gate is blocked, a worker is free with nothing to start and something moved, yet no
//     cycle starts before its tick, because the queue is full, and the daemon says so;
//   - once broken-gate is abandoned, the next cycle starts before its tick and queues the proposal
//     that waited, which the dispatcher builds and the merger integrates.
func runQueueFull(ctx context.Context, st *Stack, dbStore *store.Store, check func(desc string, cond bool) error) error {
	const (
		interval  = 10 * time.Minute
		idleDelay = 3 * time.Second
		blocked   = "broken-gate"
		stuck     = "waits-for-nobody"
		proposed  = "add-audit-trail"
	)
	WriteAgentsYAML(st.WorkspaceDir,
		[]string{"sim-agent-router", "sim-agent-cycle"},
		map[string][]string{
			"plan":  {"sim-agent-router"},
			"build": {"sim-agent-router"},
			"cycle": {"sim-agent-cycle"},
		},
		"in_order",
		nil,
	)
	if err := config.Set(st.WorkspaceDir, string(settings.QueueMaxOpen), "2", false, io.Discard); err != nil {
		return fmt.Errorf("config set %s: %w", settings.QueueMaxOpen, err)
	}
	_, _ = gitx.RunGit(ctx, st.WorkspaceDir, "add", settings.QueueMaxOpen.FilePath())
	commitWorkspaceConfig(ctx, st.WorkspaceDir, "configure cycle agent and a queue of two")
	writeBacklog(st.WorkspaceDir, []map[string]any{
		{"change_name": blocked, "priority": "normal", "description": "Always fails unit test gate"},
		{"change_name": stuck, "priority": "low", "description": "Waits for a change nobody builds", "depends_on": []string{"never-built"}},
	})

	d := startSimDaemon(ctx, st, dbStore, interval, idleDelay)
	defer d.stop()

	// cycles lists the cycles on record, oldest first.
	cycles := func() []store.Cycle {
		list, _ := dbStore.ListCycles(ctx, 0)
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
		return list
	}
	statusOf := func(name string) string {
		items, _ := cycle.ReadProposals(st.WorkspaceDir)
		for _, it := range items {
			if fmt.Sprint(it["change_name"]) == name {
				return fmt.Sprint(it["status"])
			}
		}
		return ""
	}
	inBacklog := func(name string) bool {
		items, _ := cycle.ReadJSONList(filepath.Join(st.WorkspaceDir, filepath.FromSlash(cycle.BacklogFile)))
		for _, it := range items {
			if fmt.Sprint(it["change_name"]) == name {
				return true
			}
		}
		return false
	}

	// The startup cycle finds the queue full.
	if err := d.waitUntil("the startup cycle ends", 5*time.Minute, func() bool {
		list := cycles()
		return len(list) >= 1 && list[0].FinishedAt != ""
	}); err != nil {
		return err
	}
	first := cycles()[0]
	if err := check("the startup cycle counted two open changes of two and queued nothing new",
		first.Trigger == store.CycleTriggerStartup &&
			strings.Contains(d.log.String(), "queue: 2 changes open (queue.max_open: 2): this cycle queues nothing new; it verifies and reports")); err != nil {
		return err
	}
	limited, _ := dbStore.QueryEvents(ctx, store.EventFilter{Event: "cycle.queue_limited"})
	if err := check("it said so in an event of its own, with the count", len(limited) == 1 && fmt.Sprint(limited[0].Payload["open"]) == "2"); err != nil {
		return err
	}
	if err := check("the proposal its judgment made was approved and waits for room instead of entering the backlog",
		statusOf(proposed) == "approved" && !inBacklog(proposed) &&
			strings.Contains(d.log.String(), "approved and waiting for room: "+proposed)); err != nil {
		return err
	}

	// broken-gate ends blocked: the worker is free, the other item cannot start, and something moved.
	if err := d.waitUntil(blocked+" is blocked and the next cycle waits because the queue is full", 5*time.Minute, func() bool {
		q, _ := dbStore.GetQuarantine(ctx, blocked)
		sched, _ := dbStore.CycleSchedule(ctx)
		return q != nil && sched != nil && sched.Waits == "queue_full"
	}); err != nil {
		return err
	}
	moved, _ := dbStore.MovementBetween(ctx, d.began, time.Time{})
	time.Sleep(3 * idleDelay)
	sched, _ := dbStore.CycleSchedule(ctx)
	if err := check("with a worker free, nothing to start and a change blocked since the last cycle, no cycle started before its tick while the queue was full",
		moved.Blocked >= 1 && len(cycles()) == 1 && sched != nil && sched.Waits == "queue_full" &&
			strings.Contains(d.log.String(), "the queue takes no more (2 changes open (queue.max_open: 2)): the next cycle waits for its tick at")); err != nil {
		return err
	}
	var status bytes.Buffer
	_ = cycle.StatusDaemon(ctx, st.WorkspaceDir, dbStore, &status)
	if err := check("daemon status says that the queue is full and that the cycle waits for its tick",
		strings.Contains(status.String(), "as many changes are open as queue.max_open allows, so it waits for the tick")); err != nil {
		return err
	}

	stillWaiting, _ := dbStore.QueryEvents(ctx, store.EventFilter{Event: "proposal.queued", ChangeName: proposed})
	queuedBefore := len(stillWaiting)

	// An operator abandons the blocked change, as `foreman-kit abandon broken-gate` does: it leaves
	// the backlog and the merge queue, its block is closed, and the registry holds it as discarded.
	backlogPath := filepath.Join(st.WorkspaceDir, filepath.FromSlash(cycle.BacklogFile))
	items, err := cycle.ReadJSONList(backlogPath)
	if err != nil {
		return err
	}
	var kept []map[string]any
	for _, it := range items {
		if fmt.Sprint(it["change_name"]) != blocked {
			kept = append(kept, it)
		}
	}
	if err := cycle.WriteJSONList(backlogPath, kept); err != nil {
		return err
	}
	_ = dbStore.CloseReady(ctx, blocked, store.ManifestRemoved)
	_ = dbStore.ResolveQuarantine(ctx, blocked, store.QuarantineResolution{Resolution: store.BlockAbandoned, By: "cli", Forget: true})
	_ = dbStore.MarkChangeDiscarded(ctx, blocked)

	// One change open of two: the cycle runs before its tick and queues the proposal that waited.
	if err := d.waitUntil("a cycle starts before its tick and ends", 5*time.Minute, func() bool {
		list := cycles()
		return len(list) >= 2 && list[1].FinishedAt != ""
	}); err != nil {
		return err
	}
	if err := check("once the blocked change was abandoned, the next cycle started before its tick (out_of_work) and counted one open change of two",
		cycles()[1].Trigger == store.CycleTriggerOutOfWork &&
			strings.Contains(d.log.String(), "queue: 1 change open (queue.max_open: 2): this cycle may queue 1 finding")); err != nil {
		return err
	}
	if err := d.waitUntil(proposed+" is integrated", 5*time.Minute, func() bool {
		state, _ := dbStore.ChangeState(ctx, proposed)
		return state == store.ChangeIntegrated
	}); err != nil {
		return err
	}
	queued, _ := dbStore.QueryEvents(ctx, store.EventFilter{Event: "proposal.queued", ChangeName: proposed})
	if err := check("the proposal that waited for room was queued only after the queue had room, and then built and integrated",
		queuedBefore == 0 && len(queued) >= 1); err != nil {
		return err
	}

	d.stop()
	// The main checkout follows origin, as the next cycle would have it.
	_ = cycle.SyncRepos(ctx, st.WorkspaceDir, dbStore, false, io.Discard)
	return check(proposed+" is on origin/master, and "+stuck+" still waits", isIntegrated(ctx, st.WorkspaceDir, proposed) && inBacklog(stuck))
}
