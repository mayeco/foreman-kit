package cycle

import "time"

// The on-demand cycle. The daemon dispatches continuously, so a backlog is built within minutes of
// being queued, and only the cycle's judgment queues more: on a fixed timer the workers then sit with
// nothing to start until the next tick. With an idle delay (settings.CycleEveryWhenOutOfWork), the
// daemon starts the next cycle before its tick once the pipeline has run out of work, the delay since
// the last cycle began has passed, the queue has room for what the cycle would add
// (settings.QueueMaxOpen), and something moved in either of the last two cycles. This file holds that
// rule; the cycle loop feeds it.

// DispatchReport is what the latest pass of the continuous dispatcher left, as the cycle loop needs it.
// It is orchestrator.DispatchLoad, which this package cannot import.
type DispatchReport struct {
	// Workers is the number of worker slots, Running how many of them are building, and Waiting how
	// many items wait for one.
	Workers int
	Running int
	Waiting int
	// Halted says the pass started nothing: the workspace is paused, or every engine is out.
	Halted bool
	// At is when the pass began to read what is queued.
	At time.Time
}

// outOfWork reports whether a worker is free and nothing waits for one: only new backlog items would
// give the free workers something to do.
func (r DispatchReport) outOfWork() bool {
	return !r.Halted && r.Running < r.Workers && r.Waiting == 0
}

// advanceState says that the next cycle starts before its tick, or what keeps it from starting.
type advanceState string

const (
	// advanceNow: every condition holds.
	advanceNow advanceState = "now"
	// advanceOff: the idle delay is 0, so the cycle only runs on its tick.
	advanceOff advanceState = "off"
	// advanceNoCycle: no cycle is on record to count the delay from.
	advanceNoCycle advanceState = "no_cycle"
	// advanceDelay: the delay since the last cycle began has not passed.
	advanceDelay advanceState = "delay"
	// advanceNoReport: the dispatcher has not passed since the last cycle queued its work, so nothing
	// says yet whether that work left a worker free.
	advanceNoReport advanceState = "no_report"
	// advanceBusy: every worker is building, an item waits for one, or the pipeline is halted.
	advanceBusy advanceState = "busy"
	// advanceQueueFull: as many changes are open as the application allows (settings.QueueMaxOpen).
	// The cycle would queue nothing, so the free workers gain nothing from an early one.
	advanceQueueFull advanceState = "queue_full"
	// advanceNoMovement: nothing was built, blocked or integrated in the last two cycles. Another
	// judgment would find the same queue, which waits for a person.
	advanceNoMovement advanceState = "no_movement"
)

// advanceInput is what decideAdvance looks at.
type advanceInput struct {
	Now time.Time
	// IdleDelay is how long after the start of a cycle the next one may start before its tick
	// (settings.CycleEveryWhenOutOfWork). 0: never.
	IdleDelay time.Duration
	// LastStart is when the latest cycle on record began; zero when there is none.
	LastStart time.Time
	// QueuedAt is when that cycle handed what it queued to the dispatcher, or ended. A pass reads
	// what is queued after it begins: the report of one that began before QueuedAt may not have
	// seen that work, and does not count.
	QueuedAt time.Time
	// Report is the dispatcher's latest report. HaveReport is false while no continuous dispatcher
	// is passing: the lock is held elsewhere, or the dispatcher stopped.
	Report     DispatchReport
	HaveReport bool
	// QueueFull says that as many changes are open as settings.QueueMaxOpen allows: the cycle would
	// queue nothing.
	QueueFull bool
	// Moved says whether a change was built, blocked or integrated in either of the last two
	// cycles, each counted from its start to the start of the next one.
	Moved bool
}

// advanceDecision is the answer of decideAdvance.
type advanceDecision struct {
	State advanceState
	// NotBefore is when the delay has passed: the earliest the next cycle may start before its
	// tick. It is zero when the advance is off or no cycle is on record.
	NotBefore time.Time
}

// decideAdvance says whether the daemon starts the next cycle now, before its tick. The conditions are
// checked from the cheapest to tell to the one that needs the most: the delay, a report of the
// dispatcher that is newer than the work the last cycle queued, a free worker with nothing to start,
// room in the queue, and movement in either of the last two cycles.
//
// The last two are the brakes. A cycle that finds the queue full queues nothing, so it gives the free
// workers nothing to start: it waits for its tick, when it still verifies and reports. And a cycle
// that moved nothing gets one more advance, because each cycle explores with another strategy; after
// two in a row the judgment waits for its tick, so it does not run every few minutes while everything
// waits for a person.
func decideAdvance(in advanceInput) advanceDecision {
	if in.IdleDelay <= 0 {
		return advanceDecision{State: advanceOff}
	}
	if in.LastStart.IsZero() {
		return advanceDecision{State: advanceNoCycle}
	}
	d := advanceDecision{NotBefore: in.LastStart.Add(in.IdleDelay)}
	switch {
	case in.Now.Before(d.NotBefore):
		d.State = advanceDelay
	case !in.HaveReport || in.Report.At.Before(in.QueuedAt):
		d.State = advanceNoReport
	case !in.Report.outOfWork():
		d.State = advanceBusy
	case in.QueueFull:
		d.State = advanceQueueFull
	case !in.Moved:
		d.State = advanceNoMovement
	default:
		d.State = advanceNow
	}
	return d
}
