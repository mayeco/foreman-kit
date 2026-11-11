package orchestrator

import (
	"sync"
	"time"
)

// Hub wakes the long-lived pipeline loops (Merger.Serve and Dispatcher.Serve) when something they care
// about changed, so a ready change is merged and an eligible backlog item starts without waiting for a
// poll. Signals coalesce: many notifications before a wake-up deliver a single one, and a notification
// never blocks its sender. Each signal has exactly one consumer: the merger reads Ready, the dispatcher
// reads Backlog, and the daemon's cycle loop reads Dispatched.
//
// The Hub also carries what the continuous dispatcher's latest pass left (DispatchLoad): whether the
// pipeline has run out of work is decided by the dispatcher as it schedules, in its own memory, and
// nothing else can work it out.
//
// A nil *Hub is a valid no-op, so the one-shot CLIs keep working unchanged: its notifications do nothing
// and its channels are nil, which block forever in a select and leave the loop on its poll timer.
type Hub struct {
	ready      chan struct{} // a manifest entered the merge queue
	backlog    chan struct{} // the backlog was edited, a change integrated, a worker freed a slot, a quarantine cleared
	dispatched chan struct{} // a pass of the continuous dispatcher ended, or that dispatcher stopped

	loadMu  sync.Mutex
	load    DispatchLoad
	hasLoad bool
}

// DispatchLoad is what a pass of the continuous dispatcher left once it had started every backlog item
// it could.
type DispatchLoad struct {
	// Workers is the number of worker slots (dispatch.workers).
	Workers int
	// Running is how many of them are building.
	Running int
	// Waiting is how many items wait for a worker: the eligible ones the pass did not start (no slot
	// was free, another process holds their lease, every engine of a role is cooling down, or the
	// pass was halted) and the ones an engine outage deferred, which start again by themselves.
	Waiting int
	// Halted says the pass started nothing: the workspace is paused, or every engine is out.
	Halted bool
	// At is when the pass began to read what is queued: an item queued before it is in this load.
	At time.Time
}

// OutOfWork reports whether a worker is free and no eligible item waits for one. More workers would
// change nothing then: only new backlog items would give the free ones something to do.
func (l DispatchLoad) OutOfWork() bool {
	return !l.Halted && l.Running < l.Workers && l.Waiting == 0
}

// NewHub returns a Hub with no pending signal.
func NewHub() *Hub {
	return &Hub{ready: make(chan struct{}, 1), backlog: make(chan struct{}, 1), dispatched: make(chan struct{}, 1)}
}

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default: // a wake-up is already pending: coalesce
	}
}

// ReadyEnqueued wakes the merger: a manifest entered the merge queue, or integration may resume.
func (h *Hub) ReadyEnqueued() {
	if h != nil {
		notify(h.ready)
	}
}

// BacklogChanged wakes the dispatcher: the set of eligible backlog items may have changed.
func (h *Hub) BacklogChanged() {
	if h != nil {
		notify(h.backlog)
	}
}

// DispatchPassed records what a pass of the continuous dispatcher left and wakes the cycle loop.
func (h *Hub) DispatchPassed(l DispatchLoad) {
	if h == nil {
		return
	}
	h.loadMu.Lock()
	h.load, h.hasLoad = l, true
	h.loadMu.Unlock()
	notify(h.dispatched)
}

// DispatchStopped forgets the load: the continuous dispatcher is no longer scheduling (the daemon
// stops, or another process holds the dispatcher lock), so what its last pass left says nothing
// about the pipeline now.
func (h *Hub) DispatchStopped() {
	if h == nil {
		return
	}
	h.loadMu.Lock()
	h.load, h.hasLoad = DispatchLoad{}, false
	h.loadMu.Unlock()
	notify(h.dispatched)
}

// DispatchLoad returns what the latest pass of the continuous dispatcher left. ok is false while no
// such dispatcher has passed, and once it has stopped.
func (h *Hub) DispatchLoad() (load DispatchLoad, ok bool) {
	if h == nil {
		return DispatchLoad{}, false
	}
	h.loadMu.Lock()
	defer h.loadMu.Unlock()
	return h.load, h.hasLoad
}

// Ready delivers the merger's wake-ups. It is nil for a nil Hub.
func (h *Hub) Ready() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.ready
}

// Backlog delivers the dispatcher's wake-ups. It is nil for a nil Hub.
func (h *Hub) Backlog() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.backlog
}

// Dispatched delivers the cycle loop's wake-ups: a pass of the continuous dispatcher ended and
// DispatchLoad has what it left. It is nil for a nil Hub.
func (h *Hub) Dispatched() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.dispatched
}
