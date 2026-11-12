package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/history"
	"github.com/mayeco/foreman-kit/internal/llm"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

const maxMergePushAttempts = 3

type pushRejectedError struct {
	msg string
}

func (e *pushRejectedError) Error() string { return e.msg }

type sendBackError struct {
	msg string
	// reason is why the change goes back ("rebase_conflict" when empty).
	reason string
}

func (e *sendBackError) Error() string { return e.msg }

// MergerOptions configures a Merger instance.
type MergerOptions struct {
	Root  string
	Store *store.Store
	// Judge asks evaluator gates during integration (concurrent_work_loss); one is created from the policy when nil.
	Judge          *evaluator.Judge
	Watch          bool
	Batch          bool
	Interval       time.Duration
	GateRunner     GateRunnerFunc
	OpenSpecRunner OpenSpecRunnerFunc
	NowFunc        func() time.Time
	Out            io.Writer
	// Hub, when set, is notified when a change leaves the merge queue (its dependants may become eligible,
	// or it must be rebuilt), and its Ready signal wakes Run and Serve as soon as a manifest is enqueued.
	Hub *Hub
	// FallbackInterval is how often Serve re-reads the merge queue without a signal, a safety net for a
	// missed one (default 30s).
	FallbackInterval time.Duration
	// BaseCheckRunner runs a check of a child repository's base alone (merge.outside_commits:
	// verify); DefaultBaseCheckRunner when nil.
	BaseCheckRunner BaseCheckRunnerFunc
	// BaseRecheckInterval is how often the remote base of a repository whose base is broken is read
	// again, to see whether it moved (default 5 minutes).
	BaseRecheckInterval time.Duration
}

// Merger implements the single-writer integration state machine that consumes store.ListReadyQueue.
type Merger struct {
	opts          MergerOptions
	root          string
	mainRoot      string
	store         *store.Store
	ownsStore     bool
	llmClient     *llm.Client
	judge         *evaluator.Judge
	settings      PipelineSettings
	nowFunc       func() time.Time
	out           io.Writer
	stopRequested atomic.Bool
	stopWake      chan struct{} // RequestStop wakes a watching Run without waiting for its poll
	// rechecks holds, per child repository with a broken base, when its remote base was last read.
	rechecks map[string]recheckState
	// movedOff holds, per repository, the broken shas resolved because its base moved off them, until
	// the new base is found healthy and merge.base_recovered announces them.
	movedOff map[string][]string
	// holdsLock is true while Run or Serve holds the merger lock; ownsLock then checks that no other
	// process took it over before each integration, before each push and before acting on a failure.
	holdsLock bool
	// attemptID is the merge attempt in progress (0 between attempts), and attemptIntegrated whether
	// it produced the integration commit rather than finding the change integrated already.
	attemptID         int64
	attemptIntegrated bool
	// attemptCommit is the integration commit the attempt in progress produced ("" until it does).
	attemptCommit string
}

// DefaultMergeInterval is how often a watching merger looks for ready changes, unless
// `foreman-kit merge --watch --interval 30s` says otherwise. The daemon's merger is woken as soon as a
// change is ready, and looks on its own this often as a safety net.
const DefaultMergeInterval = 10 * time.Second

// NewMerger creates and initializes a Merger.
func NewMerger(opts MergerOptions) (*Merger, error) {
	root := store.AppRoot(opts.Root)
	mainRoot := store.MainRoot(root)
	settings := LoadPipelineSettings(root)

	st := opts.Store
	ownsStore := false
	if st == nil {
		var err error
		st, err = store.Open(root)
		if err != nil {
			return nil, err
		}
		ownsStore = true
	}

	if opts.Interval <= 0 {
		opts.Interval = DefaultMergeInterval
	}
	if opts.GateRunner == nil {
		opts.GateRunner = DefaultGateRunner(st)
	}
	if opts.OpenSpecRunner == nil {
		opts.OpenSpecRunner = DefaultOpenSpecRunner()
	}
	if opts.BaseCheckRunner == nil {
		opts.BaseCheckRunner = DefaultBaseCheckRunner(st)
	}
	nowFn := opts.NowFunc
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}

	judge := opts.Judge
	if judge == nil {
		// A policy that cannot load only disables the merger's evaluator gates; the dispatcher reports it.
		judge, _ = evaluator.NewJudge(root, st)
	}

	return &Merger{
		judge:     judge,
		opts:      opts,
		root:      root,
		mainRoot:  mainRoot,
		store:     st,
		ownsStore: ownsStore,
		llmClient: llm.NewClientForRoot(root, st),
		settings:  settings,
		nowFunc:   nowFn,
		out:       out,
		stopWake:  make(chan struct{}, 1),
		rechecks:  make(map[string]recheckState),
		movedOff:  make(map[string][]string),
	}, nil
}

// Close closes the underlying Store if the Merger opened it.
func (m *Merger) Close() error {
	if m.ownsStore && m.store != nil {
		return m.store.Close()
	}
	return nil
}

// RequestStop asks a watching Merger to drain the queue once more and exit cleanly between changes.
func (m *Merger) RequestStop() {
	m.stopRequested.Store(true)
	notify(m.stopWake)
}

func (m *Merger) worktreeBase() string {
	return settings.WorktreesBase(m.mainRoot)
}

func (m *Merger) integrationWorktree() string {
	return filepath.Join(m.worktreeBase(), "_integration")
}

// Run acquires the merger lock, recovers any interrupted state, and processes the ready queue.
func (m *Merger) Run(ctx context.Context) error {
	if why := m.settings.MergerWaits(); why != "" {
		return errors.New("merger: " + why)
	}
	leaseTTL := m.settings.LocksStaleAfter
	_, err := m.store.AcquireLock(ctx, "merger", store.LockOptions{
		OwnerType: "merger",
		PID:       os.Getpid(),
		Slot:      MergerSlot,
		TTL:       leaseTTL,
	})
	if errors.Is(err, store.ErrLockHeld) {
		if m.out != nil {
			_, _ = fmt.Fprint(m.out, "merger: already running\n")
		}
		return nil
	}
	if err != nil {
		return err
	}
	m.holdsLock = true
	stopHB := m.store.StartLockHeartbeat(ctx, "merger", os.Getpid(), 2*time.Hour)
	defer func() {
		stopHB()
		_ = m.store.ReleaseLock(context.Background(), "merger", os.Getpid())
		m.holdsLock = false
	}()

	m.recoverState(ctx)
	if m.store != nil {
		_ = m.store.SyncJSONToSQLite(ctx, m.root)
	}

	for {
		if ctx.Err() != nil {
			break
		}
		if !m.isPaused(ctx) {
			m.ProcessQueue(ctx)
		}
		if !m.opts.Watch {
			break
		}
		if m.isStopRequested(ctx) {
			if !m.isPaused(ctx) {
				m.ProcessQueue(ctx)
			}
			m.clearStopPID(ctx)
			break
		}
		_ = m.store.HeartbeatLock(ctx, "merger", os.Getpid())
		select {
		case <-ctx.Done():
			return nil
		case <-m.opts.Hub.Ready():
		case <-m.stopWake:
		case <-time.After(m.opts.Interval):
		}
	}
	return nil
}

// Serve integrates ready changes as soon as they appear, until ctx is cancelled. It is the daemon's
// continuous merger: it holds the merger lock for its whole life (heartbeating it in the background),
// drains the merge queue on every Hub.Ready signal, and re-reads it every FallbackInterval in case a signal
// was missed. While the workspace is paused it integrates nothing and keeps waiting for a signal, so a
// resume takes effect on the next one.
//
// It returns store.ErrLockHeld when another merger (a cron cycle or a CLI) owns integration; the caller
// retries later, so two mergers never run at once.
func (m *Merger) Serve(ctx context.Context) error {
	pid := os.Getpid()
	if why := m.settings.MergerWaits(); why != "" {
		return errors.New("merger: " + why)
	}
	ttl := m.settings.LocksStaleAfter
	if _, err := m.store.AcquireLock(ctx, "merger", store.LockOptions{
		OwnerType: "merger",
		PID:       pid,
		Slot:      MergerSlot,
		TTL:       ttl,
		Metadata:  map[string]any{"mode": "continuous"},
	}); err != nil {
		return err
	}
	m.holdsLock = true

	// Integrations run under workCtx. The heartbeat below ends it as soon as the lock row is no longer
	// this process's: store.IsLockStale lets another process take a lock whose heartbeat is older than
	// its TTL even while its owner lives (a laptop asleep for longer than locks.stale_after, a
	// suspended VM), and an integration that went on would run its gates and push in the integration
	// worktree the new owner is resetting.
	workCtx, stopWork := context.WithCancel(ctx)
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		every := 15 * time.Second
		if ttl > 0 && ttl/3 < every {
			every = ttl / 3
		}
		if every <= 0 {
			every = time.Second
		}
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-tick.C:
				if errors.Is(m.store.HeartbeatLock(workCtx, "merger", pid, ttl), store.ErrLockHeld) {
					stopWork()
					return
				}
			}
		}
	}()
	defer func() {
		stopWork()
		<-hbDone
		_ = m.store.ReleaseLock(context.WithoutCancel(ctx), "merger", pid)
		m.holdsLock = false
	}()

	m.recoverState(ctx)
	_ = m.store.SyncJSONToSQLite(ctx, m.root)

	every := m.opts.FallbackInterval
	if every <= 0 {
		every = 30 * time.Second
	}
	fallback := time.NewTicker(every)
	defer fallback.Stop()
	unusable := "" // why the settings cannot be used, while they cannot
	for {
		if ctx.Err() != nil {
			return nil
		}
		if workCtx.Err() != nil || !m.ownsLock(ctx) {
			m.logf("merger: another process took the merger lock (this process's heartbeat had expired); integrating nothing until it is free again")
			return store.ErrLockHeld
		}
		// Serve lives as long as the daemon: read .foreman-kit/pipeline.yaml again on every wake-up, so an
		// edit (gates.timeout, merge.outside_commits, the base branch) applies to the next integration
		// without a restart. Only this goroutine reads m.settings.
		m.settings = LoadPipelineSettings(m.root)
		// A file edited while the daemon runs may stop holding what the merger reads from it:
		// nothing is integrated then, the merger says so once, and goes on as soon as the file is
		// mended.
		if why := m.settings.MergerWaits(); why != "" {
			if why != unusable {
				m.logf("merger: %s", why)
			}
			unusable = why
		} else if unusable = ""; !m.isPaused(workCtx) {
			if n := m.drainQueue(workCtx); n > 0 {
				m.logf("merger: handled %d ready change(s)", n)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-workCtx.Done(): // the lock was taken over: the check above returns
		case <-m.opts.Hub.Ready():
			m.logf("merger: woke (ready)")
		case <-fallback.C:
		}
	}
}

// ownsLock refreshes the merger lock this process holds and reports whether it still holds it. Another
// process may take a lock whose heartbeat is older than its TTL even while its owner lives
// (store.IsLockStale checks the TTL first: a laptop asleep for longer than locks.stale_after, a
// suspended VM or container), and the background heartbeat ignores the takeover, so the merger asks
// before each integration, before it pushes and before it acts on a failed attempt. It is true when the
// merger was not started through Run or Serve (no lock to lose).
func (m *Merger) ownsLock(ctx context.Context) bool {
	if !m.holdsLock {
		return true
	}
	// Detached from cancellation: a stopped integration still learns whether it lost the lock.
	return !errors.Is(m.store.HeartbeatLock(context.WithoutCancel(ctx), "merger", os.Getpid()), store.ErrLockHeld)
}

// logf writes one timestamped line to the merger's output (the daemon log under Serve).
func (m *Merger) logf(format string, args ...any) {
	if m.out == nil {
		return
	}
	_, _ = fmt.Fprintf(m.out, "[%s] %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func (m *Merger) isPaused(ctx context.Context) bool {
	ws, err := m.store.GetWorkspaceState(ctx)
	if err == nil && ws.Paused {
		return true
	}
	return fileExists(store.RuntimePath(m.root, "PAUSE"))
}

func (m *Merger) isStopRequested(ctx context.Context) bool {
	if m.stopRequested.Load() {
		return true
	}
	ws, err := m.store.GetWorkspaceState(ctx)
	if err == nil && ws.MergerStopPID == os.Getpid() {
		return true
	}
	return false
}

func (m *Merger) clearStopPID(ctx context.Context) {
	_ = m.store.SetMergerStopPID(ctx, 0)
}

// updateState records the state the merge attempt in progress entered. The merger is idle when no
// attempt is open: an attempt ends with finishAttempt, never with a state.
func (m *Merger) updateState(ctx context.Context, state, change string) {
	if state == MergerStateIdle || state == "" {
		return
	}
	_ = m.store.SetMergeAttemptState(ctx, m.attemptID, state)
}

// finishAttempt closes the merge attempt in progress with its result (a store.Merge* constant).
func (m *Merger) finishAttempt(ctx context.Context, result, reason string) {
	if m.attemptID == 0 {
		return
	}
	_ = m.store.FinishMergeAttempt(context.WithoutCancel(ctx), m.attemptID, result, cutText(reason, 4000))
	m.attemptID = 0
}

// recoverState closes the merge attempt an earlier merger left open: it died, or was stopped, in the
// middle of a change, which stays in the queue and is integrated from the start again.
func (m *Merger) recoverState(ctx context.Context) {
	open, err := m.store.OpenMergeAttempt(ctx)
	if err != nil || open == nil {
		return
	}
	if open.State == MergerStateCompleted {
		// Everything was pushed: only the bookkeeping after it is missing, and the next pass over
		// the change's manifest finds it integrated.
		_ = m.store.FinishMergeAttempt(ctx, open.ID, store.MergeIntegrated, "recovered after the push")
		return
	}
	_ = m.store.EmitEvent(ctx, store.EventInput{
		Event:          "merger.recover",
		Change:         open.ChangeName,
		MergeAttemptID: open.ID,
		Payload: map[string]any{
			"state":  open.State,
			"change": open.ChangeName,
		},
	})
	_ = m.store.FinishMergeAttempt(ctx, open.ID, store.MergeStopped, "left open by an earlier merger at "+open.State)
}

// ProcessQueue drains all currently queued items in store.ListReadyQueue in FIFO order.
func (m *Merger) ProcessQueue(ctx context.Context) {
	seen := make(map[string]bool)
	for {
		if n := m.processOnePass(ctx, seen); !m.opts.Batch || n == 0 {
			return
		}
	}
}

// drainQueue integrates, sends back or blocks everything in the merge queue, re-reading the queue after
// each pass until a pass finds nothing new: a manifest enqueued during a 40-minute gate run is handled
// in the same drain, not on the next wake-up. It returns how many manifests it handled.
func (m *Merger) drainQueue(ctx context.Context) int {
	seen := make(map[string]bool)
	total := 0
	for {
		n := m.processOnePass(ctx, seen)
		total += n
		if n == 0 || ctx.Err() != nil || m.isPaused(ctx) {
			return total
		}
	}
}

// processOnePass reads the merge queue once and handles, in FIFO order, every manifest that seen does not
// list yet, stopping early on pause or cancellation. A manifest held by a broken base
// (heldByBrokenBase) is skipped cheaply and not counted, so held changes never trigger another pass.
// It returns how many manifests it handled.
func (m *Merger) processOnePass(ctx context.Context, seen map[string]bool) int {
	manifests, err := m.store.ListReadyQueue(ctx)
	if err != nil || len(manifests) == 0 {
		return 0
	}
	handled := 0
	for _, manifest := range manifests {
		if m.isPaused(ctx) || ctx.Err() != nil || !m.ownsLock(ctx) {
			return handled
		}
		if seen[manifest.ChangeName] {
			continue
		}
		if m.heldByBrokenBase(ctx, manifest) {
			continue
		}
		seen[manifest.ChangeName] = true
		handled++
		m.integrate(ctx, manifest)
	}
	return handled
}

func (m *Merger) integrate(ctx context.Context, manifest store.ReadyManifest) {
	change := strings.TrimSpace(manifest.ChangeName)
	if !ChangeNameRe.MatchString(change) {
		_ = m.store.EmitEvent(ctx, store.EventInput{
			Event:  "merge.invalid_manifest",
			Change: manifest.ChangeName,
			Payload: map[string]any{
				"change": change,
			},
		})
		_ = m.store.CloseReady(ctx, manifest.ChangeName, store.ManifestInvalid)
		return
	}

	// base is the context of the whole pass over the manifest; ctx, of the attempt in progress: what
	// the attempt writes (its gates, its evaluator answers, a block, the pushes) points at it.
	base := ctx
	attempts := 0
	for {
		if base.Err() != nil {
			// Stopped between two attempts: the manifest stays in the queue.
			return
		}
		attempts++
		m.attemptIntegrated, m.attemptCommit = false, ""
		m.attemptID, _ = m.store.BeginMergeAttempt(base, change)
		ctx = store.WithMergeAttempt(base, m.attemptID, change)
		err := m.integrateOnce(ctx, &manifest)
		if err != nil && !m.ownsLock(ctx) {
			// Another merger took over integration while this attempt ran: the integration worktree, the
			// merger state and this manifest are its own now. Block, send back or pause nothing: it
			// closed this attempt when it began its own.
			m.attemptID = 0
			return
		}
		if err != nil && m.stoppedAttempt(ctx, change) {
			return
		}
		if err == nil {
			m.cleanupWorker(ctx, manifest)
			result, outcome := store.MergeAlreadyIntegrated, store.ManifestAlreadyIntegrated
			if m.attemptIntegrated {
				result, outcome = store.MergeIntegrated, store.ManifestIntegrated
			}
			m.finishAttempt(ctx, result, "")
			_ = m.store.CloseReady(ctx, change, outcome)
			_ = m.store.MarkChangeIntegrated(ctx, change, m.attemptCommit)
			_ = m.store.ResolveQuarantine(ctx, change, store.QuarantineResolution{Resolution: store.BlockIntegrated, Forget: true})
			// Its dependants may be eligible now.
			m.opts.Hub.BacklogChanged()
			return
		}

		var heldErr *baseHeldError
		if errors.As(err, &heldErr) {
			// Its manifest stays in the queue, unblocked and with its retries untouched, until the base
			// moves or a human accepts it: the merger alerted once for that base (merge.base_broken).
			m.finishAttempt(ctx, store.MergeHeld, err.Error())
			return
		}

		var sbErr *sendBackError
		if errors.As(err, &sbErr) {
			m.sendBack(ctx, manifest, sbErr.reason, sbErr.Error())
			return
		}

		var prErr *pushRejectedError
		if errors.As(err, &prErr) {
			_ = m.store.EmitEvent(ctx, store.EventInput{
				Event:  "merge.push_rejected",
				Change: change,
				Payload: map[string]any{
					"attempt": attempts,
					"error":   tailLines(prErr.Error(), 5),
				},
			})
			// push_rejection: a rejection by rule (protected branch, hook, permissions) cannot pass by
			// retrying: it goes to a human at once instead of after every retry.
			state := map[string]any{
				"change":        change,
				"target":        strings.SplitN(prErr.msg, ":", 2)[0],
				"attempt":       attempts,
				"remote_output": tailLines(prErr.msg, 40),
			}
			action, v := routeGate(ctx, m.judge, m.store, PushRejectionGate, change, state)
			if m.stoppedAttempt(ctx, change) {
				return
			}
			if action == "needs_human" {
				m.blockChange(ctx, manifest, NewBlockedError("push_rejected", prErr.Error()+"\n\n"+answerSummary(v)))
				return
			}
			if attempts < maxMergePushAttempts {
				m.finishAttempt(ctx, store.MergePushRejected, prErr.Error())
				continue
			}
			m.blockChange(ctx, manifest, NewBlockedError("push_failed", prErr.Error()))
			return
		}

		var blocked *BlockedError
		if errors.As(err, &blocked) {
			m.blockChange(ctx, manifest, blocked)
			return
		}

		sysErr := NewBlockedError("orchestrator_error", err.Error())
		ws, _ := m.store.GetWorkspaceState(ctx)
		action, v := triageSystemError(ctx, m.judge, m.store, change, ws.MergerState, sysErr.Details, m.nowFunc())
		if m.stoppedAttempt(ctx, change) {
			return
		}
		switch action {
		case "block_change":
			m.logf("merger: %s: the error is this change's alone (evaluator): blocking it without pausing the workspace", change)
			m.blockChange(ctx, manifest, NewBlockedError("orchestrator_error", sysErr.Details+"\n\n"+answerSummary(v)))
			return
		case "retry_later":
			m.logf("merger: %s: the error looks transient (evaluator): it is retried later", change)
			m.blockChange(ctx, manifest, NewBlockedError("transient_error", sysErr.Details+"\n\n"+answerSummary(v)))
			return
		}
		firstLine := strings.TrimSpace(strings.SplitN(sysErr.Details, "\n", 2)[0])
		reasonMsg := fmt.Sprintf("%s (%s): %s", sysErr.Reason, change, firstLine)
		// The pause and its system.paused event are one write, and it goes with the block that follows:
		// a stop arriving now does not leave one without the other.
		_, _ = m.store.Pause(context.WithoutCancel(ctx), reasonMsg, "merger", change)
		m.blockChange(ctx, manifest, sysErr)
		return
	}
}

// stoppedAttempt reports whether the merger was stopped while the attempt in progress ran (a daemon
// stop, an interrupted CLI) and, if so, closes that attempt as stopped. What failed then is git, a
// gate or a push that was cancelled, which says nothing about the change: it is not blocked, sent back
// or held, the workspace is not paused, and its manifest stays in the queue for the next merger, which
// integrates it from the start. A worker of the dispatcher does the same (fail in processChange).
func (m *Merger) stoppedAttempt(ctx context.Context, change string) bool {
	if ctx.Err() == nil {
		return false
	}
	m.logf("merger: %s: stopped with the merger; nothing is recorded against it and it stays in the merge queue", change)
	m.finishAttempt(ctx, store.MergeStopped, "the merger was stopped")
	return true
}

func (m *Merger) sendBack(ctx context.Context, manifest store.ReadyManifest, reason, details string) {
	// The decision is taken: its rows are written together even when the stop arrives meanwhile.
	ctx = context.WithoutCancel(ctx)
	change := manifest.ChangeName
	if reason == "" {
		reason = "rebase_conflict"
	}
	details = cutText(details, 4000)
	rebuilds, _ := m.store.RequestRebuildFor(ctx, change, manifest.Branch, reason, details)
	_ = m.store.CloseReady(ctx, change, store.ManifestSentBack)
	_ = m.store.EmitEvent(ctx, store.EventInput{
		Event:  "merge.sent_back",
		Change: change,
		Payload: map[string]any{
			"reason":   reason,
			"rebuilds": rebuilds,
		},
	})
	why := "conflicts with the current base"
	if reason == WorkLossGate {
		why = "removes a teammate's concurrent work (evaluator)"
	}
	m.logf("merger: %s: %s: sent back to the dispatcher for rebuild %d of %d", change, why, rebuilds, m.settings.ConflictRebuilds)
	m.finishAttempt(ctx, store.MergeSentBack, reason)
	// The dispatcher rebuilds it on the current base.
	m.opts.Hub.BacklogChanged()
}

// cutText returns s cut to at most max bytes, never inside a multi-byte character: a cut there leaves
// bytes that are not valid UTF-8 in what is stored and later handed to an agent as an argument.
func cutText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func (m *Merger) rebuildAllowed(ctx context.Context, change string) bool {
	if m.settings.ConflictRebuilds <= 0 {
		return false
	}
	q, err := m.store.GetQuarantine(ctx, change)
	if err != nil || q == nil {
		return true
	}
	return q.Rebuilds < m.settings.ConflictRebuilds
}

func (m *Merger) blockChange(ctx context.Context, manifest store.ReadyManifest, blocked *BlockedError) {
	// The decision is taken: the block, its event, the manifest leaving the queue and the attempt's
	// result are written together even when the stop arrives meanwhile, never the result alone.
	ctx = context.WithoutCancel(ctx)
	change := manifest.ChangeName
	ws, _ := m.store.GetWorkspaceState(ctx)
	qPrev, _ := m.store.GetQuarantine(ctx, change)
	retries := 0
	rebuilds := 0
	approved := false
	if qPrev != nil {
		retries = qPrev.Retries
		rebuilds = qPrev.Rebuilds
		approved = qPrev.ApprovedRisk
	}
	_ = m.store.BlockChange(ctx, store.QuarantineRecord{
		ChangeName:   change,
		Reason:       blocked.Reason,
		Details:      blocked.Details,
		Stage:        ws.MergerState,
		Worktree:     manifest.Worktree,
		Branch:       manifest.Branch,
		Retries:      retries,
		Rebuilds:     rebuilds,
		ApprovedRisk: approved,
		BlockedAt:    m.nowFunc().Format(time.RFC3339),
	})
	_ = m.store.EmitEvent(ctx, store.EventInput{
		Event:  "change.blocked",
		Change: change,
		Payload: map[string]any{
			"reason": blocked.Reason,
			"stage":  ws.MergerState,
		},
	})
	m.logf("merger: %s: blocked (%s at %s): foreman-kit status", change, blocked.Reason, ws.MergerState)
	_ = m.store.CloseReady(ctx, change, store.ManifestBlocked)
	result := store.MergeBlocked
	if blocked.Reason == "push_rejected" || blocked.Reason == "push_failed" {
		result = store.MergePushRejected
	}
	m.finishAttempt(ctx, result, blocked.Reason)
	// It left the merge queue: a retry may become due, and what waited on it is re-evaluated.
	m.opts.Hub.BacklogChanged()
}

func (m *Merger) resolveBaseRef(ctx context.Context, dir string) string {
	remoteRef := m.settings.Remote + "/" + m.settings.BaseBranch
	if _, err := gitx.RunGit(ctx, dir, "rev-parse", "--verify", "--quiet", remoteRef); err == nil {
		return remoteRef
	}
	if _, err := gitx.RunGit(ctx, dir, "rev-parse", "--verify", "--quiet", m.settings.BaseBranch); err == nil {
		return m.settings.BaseBranch
	}
	return "HEAD"
}

func (m *Merger) integrateOnce(ctx context.Context, manifest *store.ReadyManifest) error {
	change := manifest.ChangeName
	head := strings.TrimSpace(manifest.HeadSHA)
	if manifest.ChildrenPushed == nil {
		manifest.ChildrenPushed = make(map[string]string)
	}
	m.updateState(ctx, MergerStateClaimed, change)

	rootRepo, err := gitx.Open(m.root)
	if err != nil {
		return err
	}
	_ = rootRepo.Fetch(ctx, m.settings.Remote, m.settings.BaseBranch)
	baseRef := m.resolveBaseRef(ctx, m.root)

	already, err := rootRepo.HasCommitWithTrailer(baseRef, change)
	if err == nil && already {
		_ = m.store.EmitEvent(ctx, store.EventInput{
			Event:  "merge.already_integrated",
			Change: change,
		})
		return nil
	}

	branchTipOut, err := gitx.RunGit(ctx, m.root, "rev-parse", "--verify", "--quiet", manifest.Branch)
	branchTip := strings.TrimSpace(branchTipOut)
	if err != nil || branchTip == "" {
		return NewBlockedError("manifest_stale", fmt.Sprintf("%s is at %s, manifest says %s", manifest.Branch, branchTip, head))
	}
	if branchTip != head {
		// Auto-heal manifest head_sha if the branch still carries openspec/changes/<change>
		if _, treeErr := gitx.RunGit(ctx, m.root, "rev-parse", "--verify", "--quiet", branchTip+":openspec/changes/"+change); treeErr == nil {
			oldHead := head
			head = branchTip
			manifest.HeadSHA = branchTip
			_ = m.store.UpdateReadyHead(ctx, change, branchTip)
			// Invalidate any prior risk approval file or flag since new unreviewed commits were added
			_ = os.Remove(filepath.Join(m.root, "log", "approved", change))
			if m.mainRoot != "" && m.mainRoot != m.root {
				_ = os.Remove(filepath.Join(m.mainRoot, "log", "approved", change))
			}
			if m.store != nil {
				_ = m.store.RevokeRiskApproval(ctx, change, "head_updated")
			}
			_ = m.store.EmitEvent(ctx, store.EventInput{
				Event:  "merge.head_updated",
				Change: change,
				Payload: map[string]any{
					"old_head": oldHead,
					"new_head": branchTip,
				},
			})
		} else {
			return NewBlockedError("manifest_stale", fmt.Sprintf("%s is at %s, manifest says %s", manifest.Branch, branchTip, head))
		}
	}

	if err := m.prepareIntegrationWorktree(ctx, baseRef); err != nil {
		return err
	}
	wt := m.integrationWorktree()
	wtRepo, err := gitx.Open(wt)
	if err != nil {
		return err
	}
	integrationBaseSHA, err := wtRepo.RevParse("HEAD")
	if err != nil || integrationBaseSHA == "" {
		integrationBaseSHA = baseRef
	}
	forkPoint, err := wtRepo.MergeBase(baseRef, head)
	if err != nil {
		return err
	}

	// 1. Policy check on umbrella branch
	m.updateState(ctx, MergerStatePolicyCheck, change)
	if err := CheckRootPolicy(wtRepo, change, forkPoint, head); err != nil {
		return err
	}

	// 2. Rebase each touched child repository onto its own base branch (including child policy check)
	m.updateState(ctx, MergerStateRebasingChildren, change)
	children, _ := wtRepo.ChildPaths()
	var touched []string
	for _, cp := range children {
		h1, err1 := wtRepo.TreeEntryHash(forkPoint, cp)
		h2, err2 := wtRepo.TreeEntryHash(head, cp)
		if err1 == nil && err2 == nil && h1 != h2 {
			touched = append(touched, cp)
		}
	}
	if err := m.checkWorkerChildrenRecorded(ctx, *manifest, wtRepo, children, head); err != nil {
		return err
	}

	allChildrenAlreadyPushed := len(touched) > 0
	for _, cp := range touched {
		if manifest.ChildrenPushed[cp] == "" {
			allChildrenAlreadyPushed = false
			break
		}
	}

	// The commit each rebased child now sits on: its base as origin has it, which the gates
	// compare with the gitlink the umbrella records (merge.outside_commits).
	childOnto := make(map[string]string, len(touched))
	childRebased := make(map[string]string, len(touched))
	for _, cp := range touched {
		if pushedSHA := manifest.ChildrenPushed[cp]; pushedSHA != "" {
			// Child was already rebased, verified by gates, and pushed to origin in a prior attempt
			childDir := filepath.Join(wt, filepath.FromSlash(cp))
			childBase := wtRepo.ChildBaseBranch(cp)
			_, _ = gitx.RunGit(ctx, childDir, "fetch", "--quiet", m.settings.Remote, childBase)
			// The base may record more of the child by now: after a restart, a lost lock or a hold on
			// another repository's base, a later integration (here or in another clone) built on the
			// pushed commit. Recording that commit again would move the gitlink back and drop it, so
			// the child stays where the base has it, which already holds the change's commits.
			if baseLink, err := wtRepo.TreeEntryHash(integrationBaseSHA, cp); err == nil && baseLink != pushedSHA {
				if _, err := gitx.RunGit(ctx, childDir, "merge-base", "--is-ancestor", pushedSHA, baseLink); err == nil {
					continue
				}
			}
			if _, err := gitx.RunGit(ctx, childDir, "checkout", "--quiet", "--force", "--detach", pushedSHA); err != nil {
				return err
			}
			continue
		}
		rebased, onto, err := m.rebaseChild(ctx, *manifest, wtRepo, cp, head)
		if err != nil {
			return err
		}
		childOnto[cp] = onto
		childRebased[cp] = rebased
	}

	// 2b. Concurrent work loss: the rebased children must not silently drop what teammates landed on
	// the base after the change started (evaluator gate concurrent_work_loss).
	m.updateState(ctx, MergerStateCheckingWorkLoss, change)
	if err := m.checkConcurrentWorkLoss(ctx, *manifest, wt, childOnto, childRebased); err != nil {
		return err
	}
	// 2c. Semantic merge conflict: the change must not contradict what concurrent commits on the same
	// files establish, even when the rebase was clean (evaluator gate semantic_merge_conflict).
	if err := m.checkSemanticMergeConflict(ctx, *manifest, wt, childOnto, childRebased); err != nil {
		return err
	}

	// 3. Assemble: checkout change artifacts from worker branch and stage rebased child pointers
	m.updateState(ctx, MergerStateAssembling, change)
	if _, err := gitx.RunGit(ctx, wt, "checkout", head, "--", "openspec/changes/"+change); err != nil {
		return err
	}
	// Not "git add <child>": git skips, without an error, a child .gitmodules marks "ignore = all".
	if err := gitx.StageGitlinks(ctx, wt, touched...); err != nil {
		return err
	}

	// 4. Task audit & strict spec validation
	m.updateState(ctx, MergerStateAuditingTasks, change)
	if !m.auditTasksComplete(ctx, wt, change) {
		done, tot := CountTasksInFile(filepath.Join(wt, "openspec", "changes", change, "tasks.md"))
		return NewBlockedError("incomplete_tasks", fmt.Sprintf("tasks %d/%d", done, tot))
	}
	staleRes, _ := m.opts.OpenSpecRunner(ctx, wt, "validate", change, "--strict", "--json")
	if !staleRes.OK {
		return NewBlockedError("spec_conflict", tailLines(staleRes.Output, 80))
	}

	// 5. Run quality gates on the assembled integration worktree (skipped if recovering from PUSHING_ROOT rejection)
	if !allChildrenAlreadyPushed {
		// merge.outside_commits: verify tests the commits pushed outside the pipeline on the base
		// alone; a broken base holds the change (baseHeldError) instead of failing it.
		var bases map[string]*childBase
		if m.verifyMode() {
			bases, err = m.verifyBases(ctx, change, wtRepo, integrationBaseSHA, childOnto, childRebased)
			if err != nil {
				return err
			}
		}
		m.updateState(ctx, MergerStateRunningGates, change)
		gateTimeout := m.settings.GateTimeout
		// Pass integrationBaseSHA (current origin/<base_branch>), never stale forkPoint
		gateRes, err := m.opts.GateRunner(ctx, GateRunRequest{
			Change:         change,
			Worktree:       wt,
			BaseRef:        integrationBaseSHA,
			Slot:           MergerSlot,
			Timeout:        gateTimeout,
			ChildOnto:      childOnto,
			OutsideCommits: m.settings.OutsideCommits,
		})
		if err != nil {
			return err
		}
		if !gateRes.OK {
			if len(bases) == 0 {
				return NewBlockedError("gate_failure", tailLines(gateRes.Output, 120))
			}
			// nil: every failing gate also fails on a base a human accepted, so the change integrates.
			if err := m.attributeGateFailure(ctx, change, gateRes, bases); err != nil {
				return err
			}
		}
	}

	// 6. Archive change and validate canonical specs
	m.updateState(ctx, MergerStateArchiving, change)
	archRes, _ := m.opts.OpenSpecRunner(ctx, wt, "archive", change, "--yes", "--json")
	var archData map[string]any
	_ = parseOpenSpecJSON(archRes.Output, &archData)
	archObj, _ := archData["archive"].(map[string]any)
	if !archRes.OK || archObj == nil {
		reason := "archive_failed"
		if statuses, ok := archData["status"].([]any); ok {
			for _, st := range statuses {
				if sm, ok := st.(map[string]any); ok && fmt.Sprint(sm["code"]) == "archive_spec_update_failed" {
					reason = "spec_conflict"
					break
				}
			}
		}
		return NewBlockedError(reason, archRes.Output)
	}

	specsRes, _ := m.opts.OpenSpecRunner(ctx, wt, "validate", "--specs", "--strict", "--json")
	if !specsRes.OK {
		return NewBlockedError("spec_invalid", tailLines(specsRes.Output, 80))
	}

	// 7. Commit umbrella integration
	m.updateState(ctx, MergerStateCommitting, change)
	totals, _ := archObj["totals"].(map[string]any)
	added, _ := toInt(totals["added"])
	modified, _ := toInt(totals["modified"])
	removed, _ := toInt(totals["removed"])
	renamed, _ := toInt(totals["renamed"])
	archivedAs := fmt.Sprint(archObj["archivedAs"])

	archivedAt := m.nowFunc().Format(time.RFC3339)
	itemMap := manifest.Item
	if len(itemMap) == 0 {
		itemMap = map[string]any{"change_name": change}
	}
	if err := ArchiveBacklogInDir(wt, itemMap, archivedAt); err != nil {
		// The change is integrated all the same: the archive and the commit's trailer record it.
		m.logf("merger: %s: %v", change, err)
	}

	stagePaths := []string{"openspec"}
	for _, rel := range []string{".foreman-kit/agent_backlog.json", ".foreman-kit/agent_history.json"} {
		if fileExists(filepath.Join(wt, filepath.FromSlash(rel))) {
			stagePaths = append(stagePaths, rel)
		}
	}
	if rotMatches, err := filepath.Glob(filepath.Join(wt, ".foreman-kit", "agent_history.*.json")); err == nil {
		for _, m := range rotMatches {
			stagePaths = append(stagePaths, ".foreman-kit/"+filepath.Base(m))
		}
	}
	addArgs := append([]string{"add", "-A", "--"}, stagePaths...)
	if _, err := gitx.RunGit(ctx, wt, addArgs...); err != nil {
		return err
	}
	if err := gitx.StageGitlinks(ctx, wt, touched...); err != nil {
		return err
	}

	var assessmentLine string
	if len(manifest.Assessment) > 0 {
		var keys []string
		for k := range manifest.Assessment {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var kvParts []string
		for _, k := range keys {
			kvParts = append(kvParts, fmt.Sprintf("%s=%v", k, manifest.Assessment[k]))
		}
		assessmentLine = "\nRisk-Assessment: " + strings.Join(kvParts, " ")
	}
	reposLine := "none"
	if len(touched) > 0 {
		reposLine = strings.Join(touched, ", ")
	}
	commitMsg := fmt.Sprintf(
		"chore(merge): integrate %s\n\nSpecs: +%d ~%d -%d >%d\nArchived as: %s\nRepos: %s\n\nOpenSpec-Change: %s%s\n",
		change, added, modified, removed, renamed, archivedAs, reposLine, change, assessmentLine,
	)
	if _, err := gitx.RunGit(ctx, wt, "commit", "--quiet", "-m", commitMsg); err != nil {
		return err
	}

	// 8. Push children first, then root, unless another merger took integration over meanwhile: two
	// mergers must never push.
	if !m.ownsLock(ctx) {
		return fmt.Errorf("merger: %w: another process took integration over before %s was pushed", store.ErrLockHeld, change)
	}
	m.updateState(ctx, MergerStatePushingChildren, change)
	for _, cp := range touched {
		if manifest.ChildrenPushed[cp] != "" {
			continue
		}
		childDir := filepath.Join(wt, filepath.FromSlash(cp))
		childBase := wtRepo.ChildBaseBranch(cp)
		childRepo, err := gitx.Open(childDir)
		if err != nil {
			return err
		}
		if err := childRepo.Push(ctx, m.settings.Remote, "HEAD:refs/heads/"+childBase); err != nil {
			return &pushRejectedError{msg: fmt.Sprintf("%s: %v", cp, err)}
		}
		if childSHA, err := childRepo.RevParse("HEAD"); err == nil && childSHA != "" {
			manifest.ChildrenPushed[cp] = childSHA
			_ = m.store.RecordChildPushed(ctx, change, cp, childSHA)
		}
	}

	m.updateState(ctx, MergerStatePushingRoot, change)
	if err := wtRepo.Push(ctx, m.settings.Remote, "HEAD:refs/heads/"+m.settings.BaseBranch); err != nil {
		return &pushRejectedError{msg: fmt.Sprintf("umbrella: %v", err)}
	}

	commitSHA, err := wtRepo.RevParse("HEAD")
	if err != nil {
		return err
	}

	// Keep root remote-tracking ref and backlog files synchronized
	_ = rootRepo.Fetch(ctx, m.settings.Remote, m.settings.BaseBranch)
	m.syncRootBacklog(ctx, *manifest, commitSHA, archivedAt)

	totalsJSON, _ := json.Marshal(totals)
	_ = m.store.MarkMergeAttemptIntegrated(ctx, m.attemptID, commitSHA, string(totalsJSON))
	m.attemptIntegrated, m.attemptCommit = true, commitSHA
	_ = m.store.EmitEvent(ctx, store.EventInput{
		Event:  "merge.integrated",
		Change: change,
		Payload: map[string]any{
			"commit": commitSHA,
			"specs":  string(totalsJSON),
			"repos":  strings.Join(touched, ","),
		},
	})
	m.logf("merger: %s: integrated as %s (%s)", change, shortSHA(commitSHA), strings.Join(touched, ", "))
	return nil
}

// checkWorkerChildrenRecorded refuses a change whose worker holds, in a child repository's checkout,
// commits that neither the change records (the child's gitlink at head) nor the child's remote base has.
// The merger integrates what the change records and then deletes the worker's worktree: those commits,
// which exist nowhere else, would be lost while the change is reported as integrated. It happens when the
// gitlink was never committed on the change's branch (git 2.54 and later skip "git add <child>", exit
// status 0, for a child .gitmodules marks "ignore = all") or the child moved after the change was built.
func (m *Merger) checkWorkerChildrenRecorded(ctx context.Context, manifest store.ReadyManifest, wtRepo *gitx.Repo, children []string, head string) error {
	if manifest.Worktree == "" {
		return nil
	}
	for _, cp := range children {
		workerChild := filepath.Join(manifest.Worktree, filepath.FromSlash(cp))
		if !gitx.IsWorktreeRoot(ctx, workerChild) {
			continue
		}
		out, err := gitx.RunGit(ctx, workerChild, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
		if err != nil {
			continue
		}
		workerHead := strings.TrimSpace(out)
		recorded, err := wtRepo.TreeEntryHash(head, cp)
		if err != nil || recorded == workerHead {
			continue
		}
		remoteBase := m.settings.Remote + "/" + wtRepo.ChildBaseBranch(cp)
		held := false
		for _, holder := range []string{recorded, remoteBase} {
			if _, err := gitx.RunGit(ctx, workerChild, "merge-base", "--is-ancestor", workerHead, holder); err == nil {
				held = true
				break
			}
		}
		if !held {
			return NewBlockedError("missing_child_commits", fmt.Sprintf(
				"%s: the worker's checkout is at %s, but the change records %s and %s does not have it: integrating would publish the change without those commits and delete them with the worktree %s",
				cp, shortSHA(workerHead), shortSHA(recorded), remoteBase, manifest.Worktree))
		}
	}
	return nil
}

// CheckRootPolicy verifies with gitx.ChangedFiles that between forkPoint and headSHA the worker branch
// only modified files under openspec/changes/<change>/ or child submodule pointers under repos/*.
func CheckRootPolicy(wtRepo *gitx.Repo, change, forkPoint, headSHA string) error {
	children, err := wtRepo.ChildPaths()
	if err != nil {
		return err
	}
	childSet := make(map[string]bool, len(children))
	for _, c := range children {
		childSet[c] = true
	}

	changed, err := wtRepo.ChangedFiles(forkPoint, headSHA)
	if err != nil {
		return err
	}
	prefix := "openspec/changes/" + change + "/"
	var outside []string
	for _, p := range changed {
		if !strings.HasPrefix(p, prefix) && !childSet[p] {
			outside = append(outside, p)
		}
	}
	if len(outside) > 0 {
		return NewBlockedError("policy_violation", "branch modifies "+strings.Join(outside, ", "))
	}
	return nil
}

// CheckChildPolicy verifies with gitx.ChangedFiles and gitx.ReadBlob that between forkPoint and workerSHA
// the child repository did not modify any path in PROTECTED_CHILD_PATHS or its env_script read from repo.config.yaml at forkPoint.
func CheckChildPolicy(childRepo *gitx.Repo, childPath, forkPoint, workerSHA string) error {
	protected := ProtectedChildPathsFromBlob(childRepo, forkPoint)
	changed, err := childRepo.ChangedFiles(forkPoint, workerSHA)
	if err != nil {
		return err
	}
	var tampered []string
	for _, f := range changed {
		if IsProtectedChildPath(f, protected) {
			tampered = append(tampered, f)
		}
	}
	if len(tampered) > 0 {
		return NewBlockedError("policy_violation", fmt.Sprintf("%s: protected paths changed: %s", childPath, strings.Join(tampered, ", ")))
	}
	return nil
}

// PolicyCheck performs both the umbrella tree policy check and every touched child repository's policy check.
func PolicyCheck(ctx context.Context, wtDir, change, forkPoint, headSHA, workerWorktree string) error {
	wtRepo, err := gitx.Open(wtDir)
	if err != nil {
		return err
	}
	if err := CheckRootPolicy(wtRepo, change, forkPoint, headSHA); err != nil {
		return err
	}
	children, err := wtRepo.ChildPaths()
	if err != nil {
		return err
	}
	for _, cp := range children {
		h1, err1 := wtRepo.TreeEntryHash(forkPoint, cp)
		h2, err2 := wtRepo.TreeEntryHash(headSHA, cp)
		if err1 != nil || err2 != nil || h1 == h2 {
			continue
		}
		childDir := filepath.Join(wtDir, filepath.FromSlash(cp))
		if workerWorktree != "" && workerWorktree != wtDir {
			_, _ = gitx.RunGit(ctx, childDir, "-c", "protocol.file.allow=always", "fetch", "--quiet", filepath.Join(workerWorktree, filepath.FromSlash(cp)), h2)
		}
		childRepo, err := gitx.Open(childDir)
		if err != nil {
			return err
		}
		if err := CheckChildPolicy(childRepo, cp, h1, h2); err != nil {
			return err
		}
	}
	return nil
}

// staleIndexLockAge is how long an index.lock of the integration worktree must have gone untouched to be
// taken for the leftover of a git that was killed.
const staleIndexLockAge = 10 * time.Minute

// removeStaleIndexLocks deletes the index.lock files a killed git left in the git directory of the
// integration worktree wt and of its child repositories (a crash, a power loss, the SIGKILL that follows
// an ignored SIGTERM). Every checkout, reset and commit there fails on such a file until a person deletes
// it. Only the merger, under the merger lock, runs git there, and none of its commands is running now:
// a lock no git has touched for staleIndexLockAge is a leftover.
func (m *Merger) removeStaleIndexLocks(ctx context.Context, wt string) {
	out, err := gitx.RunGit(ctx, wt, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return
	}
	_ = filepath.WalkDir(strings.TrimSpace(out), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// The object store of a git directory is large and never holds the lock.
			if d.Name() == "objects" && fileExists(filepath.Join(filepath.Dir(p), "HEAD")) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "index.lock" {
			return nil
		}
		if info, err := d.Info(); err == nil && time.Since(info.ModTime()) >= staleIndexLockAge && os.Remove(p) == nil {
			m.logf("merger: removed %s, which no git had touched for %s", p, staleIndexLockAge)
		}
		return nil
	})
}

func (m *Merger) prepareIntegrationWorktree(ctx context.Context, baseRef string) error {
	wt := m.integrationWorktree()
	rootRepo, err := gitx.Open(m.root)
	if err != nil {
		return err
	}
	if dirExists(wt) && !gitx.IsWorktreeRoot(ctx, wt) {
		// A moved or remounted umbrella leaves a gitfile pointing at a missing admin dir, and a directory
		// without .git would make the forced checkout and reset below act on the main checkout (discarding
		// the operator's uncommitted work): repair it, else recreate it.
		_, _ = gitx.RunGit(ctx, m.root, "worktree", "repair", wt)
		if !gitx.IsWorktreeRoot(ctx, wt) {
			if err := rootRepo.RemoveWorktree(ctx, wt); err != nil {
				return err
			}
		}
	}
	if !dirExists(wt) {
		if err := rootRepo.AddWorktree(ctx, wt, "", baseRef, true); err != nil {
			return err
		}
	}
	if !gitx.IsWorktreeRoot(ctx, wt) {
		return fmt.Errorf("merger: %s is not a git worktree of %s", wt, m.root)
	}
	m.removeStaleIndexLocks(ctx, wt)
	gitx.AbortInterruptedRebase(ctx, wt)
	if _, err := gitx.RunGit(ctx, wt, "checkout", "--quiet", "--force", "--detach", baseRef); err != nil {
		return err
	}
	_, _ = gitx.RunGit(ctx, wt, "reset", "--hard", baseRef)
	if _, err := gitx.RunGit(ctx, wt, "clean", "-fdq"); err != nil {
		return err
	}
	wtRepo, err := gitx.Open(wt)
	if err != nil {
		return err
	}
	children, _ := wtRepo.ChildPaths()
	if len(children) > 0 {
		// Abort interrupted child rebases BEFORE the forced checkout: "rebase --abort" afterwards puts the
		// child back on integrate/<previous-change> at the previous worker commit.
		for _, cp := range children {
			childDir := filepath.Join(wt, filepath.FromSlash(cp))
			if _, err := os.Stat(filepath.Join(childDir, ".git")); err == nil {
				gitx.AbortInterruptedRebase(ctx, childDir)
			}
		}
		if _, err := gitx.RunGit(ctx, wt, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive", "--force"); err != nil {
			return err
		}
		for _, cp := range children {
			childDir := filepath.Join(wt, filepath.FromSlash(cp))
			_, _ = gitx.RunGit(ctx, childDir, "clean", "-fdq")
			if refsOut, err := gitx.RunGit(ctx, childDir, "for-each-ref", "--format=%(refname:short)", "refs/heads/integrate/"); err == nil {
				for _, b := range strings.Fields(refsOut) {
					_, _ = gitx.RunGit(ctx, childDir, "branch", "-D", b)
				}
			}
		}
	}
	return nil
}

// rebaseChild rebases the change's commits of child repository cp onto the repository's current base
// branch in the integration worktree. It returns the rebased head and onto, the commit it rebased onto:
// the repository's base as origin has it now, which may hold commits pushed outside the pipeline that
// the umbrella has not recorded yet.
func (m *Merger) rebaseChild(ctx context.Context, manifest store.ReadyManifest, wtRepo *gitx.Repo, cp, headSHA string) (rebased, onto string, err error) {
	wt := m.integrationWorktree()
	childDir := filepath.Join(wt, filepath.FromSlash(cp))
	baseBranch := wtRepo.ChildBaseBranch(cp)
	workerSHA, err := wtRepo.TreeEntryHash(headSHA, cp)
	if err != nil {
		return "", "", err
	}

	childRepo, err := gitx.Open(childDir)
	if err != nil {
		return "", "", err
	}
	_ = childRepo.Fetch(ctx, m.settings.Remote, baseBranch)

	workerChildDir := filepath.Join(manifest.Worktree, filepath.FromSlash(cp))
	if fetchOut, err := gitx.RunGit(ctx, childDir, "-c", "protocol.file.allow=always", "fetch", "--quiet", workerChildDir, workerSHA); err != nil {
		return "", "", NewBlockedError("missing_child_commits", fmt.Sprintf("%s@%s: %s", cp, workerSHA, tailLines(fetchOut, 5)))
	}

	childBaseRef := m.settings.Remote + "/" + baseBranch
	if _, err := gitx.RunGit(ctx, childDir, "rev-parse", "--verify", "--quiet", childBaseRef); err != nil {
		childBaseRef = baseBranch
	}

	fork, err := childRepo.MergeBase(childBaseRef, workerSHA)
	if err != nil {
		return "", "", NewBlockedError("rebase_failed", fmt.Sprintf("%s: cannot find merge-base between %s and %s: %v", cp, childBaseRef, workerSHA, err))
	}

	if err := CheckChildPolicy(childRepo, cp, fork, workerSHA); err != nil {
		return "", "", err
	}

	if ontoOut, err := gitx.RunGit(ctx, childDir, "rev-parse", "--verify", "--quiet", childBaseRef+"^{commit}"); err == nil {
		onto = strings.TrimSpace(ontoOut)
	}
	if _, err := gitx.RunGit(ctx, childDir, "checkout", "--quiet", "-B", "integrate/"+manifest.ChangeName, workerSHA); err != nil {
		return "", "", err
	}
	reb := gitx.RebaseBranch(ctx, childDir, childBaseRef)
	if !reb.OK {
		if !reb.ContentConflict {
			return "", "", NewBlockedError("rebase_failed", fmt.Sprintf("%s: %s", cp, tailLines(reb.Output, 40)))
		}
		if m.rebuildAllowed(ctx, manifest.ChangeName) {
			conflictMsg := fmt.Sprintf("%s: %s", cp, tailLines(reb.Output, 40))
			// Use Case 5 (internal/llm): Synthesize RebaseConflictBrief for {{rebuild}} prompt via zendev-sh/goai
			if m.llmClient != nil && m.llmClient.Available() {
				if brief, err := m.llmClient.SummarizeRebaseConflict(ctx, manifest.ChangeName, cp, reb.Output); err == nil && brief != nil {
					conflictMsg = conflictMsg + "\n\n" + brief.FormatMarkdown()
				}
			}
			return "", "", &sendBackError{msg: conflictMsg}
		}
		return "", "", NewBlockedError("rebase_conflict", fmt.Sprintf("%s: %s", cp, tailLines(reb.Output, 40)))
	}

	rebased, err = childRepo.RevParse("HEAD")
	return rebased, onto, err
}

func (m *Merger) auditTasksComplete(ctx context.Context, wt, change string) bool {
	tasksPath := filepath.Join(wt, "openspec", "changes", change, "tasks.md")
	raw := readFileOrEmpty(tasksPath)
	if raw == "" || uncheckedTaskRe.MatchString(raw) {
		return false
	}
	res, _ := m.opts.OpenSpecRunner(ctx, wt, "list", "--json")
	if res.OK {
		var listing map[string]any
		if parseOpenSpecJSON(res.Output, &listing) == nil {
			if chgs, ok := listing["changes"].([]any); ok {
				for _, rawChg := range chgs {
					if entry, ok := rawChg.(map[string]any); ok && fmt.Sprint(entry["name"]) == change {
						comp, ok1 := toInt(entry["completedTasks"])
						tot, ok2 := toInt(entry["totalTasks"])
						if ok1 && ok2 {
							return tot > 0 && comp == tot
						}
					}
				}
			}
		}
	}
	comp, tot := CountTasksInFile(tasksPath)
	return tot > 0 && comp == tot
}

func (m *Merger) syncRootBacklog(ctx context.Context, manifest store.ReadyManifest, ref, at string) {
	_ = ref
	if m.store != nil {
		host, _ := os.Hostname()
		nowUnix := m.nowFunc().Unix()
		for _, lockKey := range []string{"cycle", "visual_qa"} {
			if l, err := m.store.GetLock(ctx, lockKey); err == nil && l != nil && !store.IsLockStale(*l, host, nowUnix) {
				return
			}
		}
	}
	if dirExists(filepath.Join(m.root, "log", "cycle.lock")) {
		return
	}
	itemMap := manifest.Item
	if len(itemMap) == 0 {
		itemMap = map[string]any{"change_name": manifest.ChangeName}
	}
	if err := ArchiveBacklogInDir(m.root, itemMap, at); err != nil {
		m.logf("merger: %s: %v", manifest.ChangeName, err)
	}
}

func (m *Merger) cleanupWorker(ctx context.Context, manifest store.ReadyManifest) {
	if manifest.Worktree != "" {
		// O(1) os.Rename to trash directory, then git forgets the worktree, then synchronous removal
		trashDir := ""
		if dirExists(manifest.Worktree) {
			trashDir = filepath.Join(filepath.Dir(manifest.Worktree), fmt.Sprintf(".trash-%s-%d", manifest.ChangeName, time.Now().UnixNano()))
			if err := os.Rename(manifest.Worktree, trashDir); err != nil {
				trashDir = ""
				if rootRepo, err := gitx.Open(m.root); err == nil {
					_ = rootRepo.RemoveWorktree(ctx, manifest.Worktree)
				}
			}
		}
		if !dirExists(manifest.Worktree) {
			// This worktree's registration only (it holds the branch deleted below), never "git worktree
			// prune": that deletes the administrative directory, and the child commits it holds, of
			// every worktree that cannot be reached right now (a moved umbrella, an unmounted volume).
			_, _ = gitx.RunGit(ctx, m.root, "worktree", "remove", "--force", manifest.Worktree)
		}
		if trashDir != "" {
			_ = os.RemoveAll(trashDir)
		}
	}
	if manifest.ChangeName != "" {
		// Clean up the runtime tmp/<change> scratch directory
		scratch := store.RuntimePath(m.mainRoot, "tmp", manifest.ChangeName)
		_ = os.RemoveAll(scratch)
	}
	if manifest.Branch != "" {
		_, _ = gitx.RunGit(ctx, m.root, "branch", "-D", manifest.Branch)
	}
}

// ArchiveBacklogInDir moves integrated items from .foreman-kit/agent_backlog.json to .foreman-kit/agent_history.json in dir.
// It returns an error, and changes neither file, when the history is there but cannot be read as a list of
// changes (it does not parse, or holds another JSON value): written back from what was read, it would
// lose every entry it holds and keep only the ones moved now. The items then stay in the backlog, and the
// first call after the history is repaired moves them.
func ArchiveBacklogInDir(dir string, item map[string]any, at string) error {
	if at == "" {
		at = time.Now().UTC().Format(time.RFC3339)
	}
	archived := make(map[string]bool)
	archDir := filepath.Join(dir, "openspec", "changes", "archive")
	datePrefixRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-`)
	if entries, err := os.ReadDir(archDir); err == nil {
		for _, e := range entries {
			name := datePrefixRe.ReplaceAllString(e.Name(), "")
			if name != "" {
				archived[name] = true
			}
		}
	}
	if item != nil {
		if chg := strings.TrimSpace(fmt.Sprint(item["change_name"])); chg != "" {
			archived[chg] = true
		}
	}

	backlogPath := filepath.Join(dir, ".foreman-kit", "agent_backlog.json")
	historyPath := filepath.Join(dir, ".foreman-kit", "agent_history.json")
	backlogItems := readRawMapsFromFile(backlogPath)
	historyItems, err := readRawMaps(historyPath)
	if err != nil {
		return fmt.Errorf("%w: the history and the backlog are left as they are until it is repaired", err)
	}

	var done, kept []map[string]any
	for _, entry := range backlogItems {
		name := strings.TrimSpace(fmt.Sprint(entry["change_name"]))
		if archived[name] {
			done = append(done, entry)
		} else {
			kept = append(kept, entry)
		}
	}
	backlogArchived := len(done) > 0
	if item != nil {
		name := strings.TrimSpace(fmt.Sprint(item["change_name"]))
		if ChangeNameRe.MatchString(name) && archived[name] {
			found := false
			for _, d := range done {
				if fmt.Sprint(d["change_name"]) == name {
					found = true
					break
				}
			}
			if !found {
				done = append(done, item)
			}
		}
	}

	histSeen := make(map[string]bool, len(historyItems))
	for _, h := range historyItems {
		histSeen[strings.TrimSpace(fmt.Sprint(h["change_name"]))] = true
	}
	if rotMatches, err := filepath.Glob(filepath.Join(dir, ".foreman-kit", "agent_history.*.json")); err == nil {
		for _, m := range rotMatches {
			for _, h := range readRawMapsFromFile(m) {
				histSeen[strings.TrimSpace(fmt.Sprint(h["change_name"]))] = true
			}
		}
	}
	added := 0
	for _, entry := range done {
		name := strings.TrimSpace(fmt.Sprint(entry["change_name"]))
		if name == "" || histSeen[name] {
			continue
		}
		histSeen[name] = true
		copied := make(map[string]any, len(entry)+1)
		for k, v := range entry {
			copied[k] = v
		}
		if copied["integrated_at"] == nil || copied["integrated_at"] == "" {
			copied["integrated_at"] = at
		}
		historyItems = append(historyItems, copied)
		added++
	}

	if kept == nil {
		kept = []map[string]any{}
	}
	if backlogArchived {
		_ = writeJSONAtomic(backlogPath, kept)
	}
	if added > 0 || len(historyItems) > 1 {
		if historyItems == nil {
			historyItems = []map[string]any{}
		}
		_ = writeHistoryWithRotationInDir(dir, historyItems)
	}
	return nil
}

func writeHistoryWithRotationInDir(dir string, items []map[string]any) error {
	const historyMaxLines = history.MaxLines
	historyPath := filepath.Join(dir, ".foreman-kit", "agent_history.json")

	for len(items) > 1 {
		b, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return err
		}
		b = append(b, '\n')
		if bytes.Count(b, []byte("\n")) <= historyMaxLines {
			break
		}

		split := len(items) - 1
		for split > 1 {
			prefixBytes, err := json.MarshalIndent(items[:split], "", "  ")
			if err == nil && bytes.Count(prefixBytes, []byte("\n"))+1 <= historyMaxLines {
				break
			}
			split--
		}

		toRotate := items[:split]
		items = items[split:]

		if err := history.Append(dir, toRotate); err != nil {
			return err
		}
	}
	return writeJSONAtomic(historyPath, items)
}

func readRawMapsFromFile(path string) []map[string]any {
	out, _ := readRawMaps(path)
	return out
}

// readRawMaps reads the changes a backlog or history file lists: a JSON list, or an object with a
// "changes" list. A missing or empty file lists none. It returns an error for a file that is there but
// holds something else (it does not parse, or is another JSON value): one a caller must never write back
// from what it read.
func readRawMaps(path string) ([]map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("%s does not parse as JSON (%v)", path, err)
	}
	var list []any
	switch v := generic.(type) {
	case nil:
	case []any:
		list = v
	case map[string]any:
		chgs, ok := v["changes"].([]any)
		if !ok && len(v) > 0 {
			return nil, fmt.Errorf("%s holds a JSON object without a \"changes\" list", path)
		}
		list = chgs
	default:
		return nil, fmt.Errorf("%s holds neither a JSON list nor an object with a \"changes\" list", path)
	}
	var out []map[string]any
	for _, el := range list {
		if m, ok := el.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return writeFileAtomic(path, b)
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), time.Now().UTC().UnixNano())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// CoordinatedOptions configures RunCoordinated where Dispatcher and Merger run in the same process.
type CoordinatedOptions struct {
	Root           string
	Store          *store.Store
	Judge          *evaluator.Judge
	BacklogFile    string
	MaxWorkers     int
	RepairAttempts int
	NoRepairs      bool
	WaitTimeout    time.Duration
	PollInterval   time.Duration
	MergeInterval  time.Duration
	EnginePlan     string
	EngineBuild    string
	AgentRunner    AgentRunnerFunc
	GateRunner     GateRunnerFunc
	OpenSpecRunner OpenSpecRunnerFunc
	NowFunc        func() time.Time
}

// RunCoordinated runs the Dispatcher and Merger concurrently in the same Go process,
// draining any pre-existing ready queue items, dispatching workers while the Merger watches
// and integrates ready changes, and draining the ready queue one final time before returning.
// A process-local Hub links the two: a change marked ready is merged at once, and a change whose
// dependency was just integrated starts at once, so the poll intervals are only a safety net.
func RunCoordinated(ctx context.Context, opts CoordinatedOptions) error {
	root := store.AppRoot(opts.Root)
	st := opts.Store
	if st == nil {
		var err error
		st, err = store.Open(root)
		if err != nil {
			return err
		}
		defer st.Close()
	}

	mergeInterval := opts.MergeInterval
	if mergeInterval <= 0 {
		mergeInterval = time.Second
	}
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	hub := NewHub()

	merger, err := NewMerger(MergerOptions{
		Root:           root,
		Store:          st,
		Judge:          opts.Judge,
		Watch:          true,
		Interval:       mergeInterval,
		GateRunner:     opts.GateRunner,
		OpenSpecRunner: opts.OpenSpecRunner,
		NowFunc:        opts.NowFunc,
		Hub:            hub,
	})
	if err != nil {
		return err
	}

	dispatcher, err := NewDispatcher(DispatcherOptions{
		Root:           root,
		Store:          st,
		Judge:          opts.Judge,
		BacklogFile:    opts.BacklogFile,
		MaxWorkers:     opts.MaxWorkers,
		RepairAttempts: opts.RepairAttempts,
		NoRepairs:      opts.NoRepairs,
		Wait:           true,
		WaitTimeout:    opts.WaitTimeout,
		PollInterval:   pollInterval,
		EnginePlan:     opts.EnginePlan,
		EngineBuild:    opts.EngineBuild,
		AgentRunner:    opts.AgentRunner,
		GateRunner:     opts.GateRunner,
		OpenSpecRunner: opts.OpenSpecRunner,
		NowFunc:        opts.NowFunc,
		Hub:            hub,
	})
	if err != nil {
		return err
	}

	mergeErrCh := make(chan error, 1)
	go func() {
		mergeErrCh <- merger.Run(ctx)
	}()

	dispErr := dispatcher.Run(ctx)
	merger.RequestStop()
	mergeErr := <-mergeErrCh
	if dispErr != nil {
		return dispErr
	}
	return mergeErr
}

// cliSeconds reads a time given on the command line: written with its unit (30s, 15m, 2h), or a bare
// number of seconds.
func cliSeconds(text string) (int, bool) {
	if d, ok := settings.ParseDuration(text); ok {
		return int(d / time.Second), true
	}
	n, err := strconv.Atoi(strings.TrimSpace(text))
	return n, err == nil && n >= 0
}

func parseCLIArgs(fs *flag.FlagSet, args []string) error {
	var normalized []string
	hasWaitSec := fs.Lookup("wait") != nil && fs.Lookup("wait-seconds") != nil
	for i := 0; i < len(args); i++ {
		a := args[i]
		// --wait may say how long: a time with its unit (30m), or a number of seconds.
		if hasWaitSec && (a == "--wait" || a == "-wait") {
			if i+1 < len(args) {
				if seconds, ok := cliSeconds(args[i+1]); ok {
					normalized = append(normalized, "--wait", "--wait-seconds", strconv.Itoa(seconds))
					i++
					continue
				}
			}
		} else if hasWaitSec && (strings.HasPrefix(a, "--wait=") || strings.HasPrefix(a, "-wait=")) {
			val := a[strings.IndexByte(a, '=')+1:]
			if seconds, ok := cliSeconds(val); ok {
				normalized = append(normalized, "--wait", "--wait-seconds="+strconv.Itoa(seconds))
				continue
			}
		}
		normalized = append(normalized, a)
	}

	rem := normalized
	for len(rem) > 0 {
		if err := fs.Parse(rem); err != nil {
			return err
		}
		rem = fs.Args()
		if len(rem) > 0 {
			if rem[0] == "--" {
				break
			}
			rem = rem[1:]
		}
	}
	return nil
}

// RunDispatcherCLI is an alias for RunDispatchCLI.
func RunDispatcherCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return RunDispatchCLI(ctx, args, stdout, stderr)
}

// RunDispatchCLI parses CLI flags for `foreman-kit dispatch` and executes the Dispatcher.
func RunDispatchCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	out := stderr
	if out == nil {
		out = stdout
	}
	if out == nil {
		out = os.Stderr
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	root := store.AppRoot("")
	settings := LoadPipelineSettings(root)

	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	workers := fs.Int("workers", settings.Workers, "Max concurrent workers")
	fs.IntVar(workers, "w", settings.Workers, "Max concurrent workers (shorthand)")
	repairs := fs.Int("repairs", settings.RepairRounds, "Repair attempts per phase")
	fs.IntVar(repairs, "r", settings.RepairRounds, "Repair attempts per phase (shorthand)")
	queuePath := fs.String("queue", filepath.Join(root, ".foreman-kit", "agent_backlog.json"), "Backlog JSON path")
	fs.StringVar(queuePath, "q", filepath.Join(root, ".foreman-kit", "agent_backlog.json"), "Backlog JSON path (shorthand)")
	enginePlan := fs.String("engine-plan", LoadAgentRoleEngine(root, "plan"), "Engine for PLAN phase")
	engineBuild := fs.String("engine-build", LoadAgentRoleEngine(root, "build"), "Engine for BUILD phase")
	waitFlag := fs.Bool("wait", false, "Wait while changes wait on merges")
	waitSec := fs.Int("wait-seconds", int(DefaultDispatchWait/time.Second), "Wait timeout in seconds")
	dryRun := fs.Bool("dry-run", false, "Print what would start next and why each pending item waits")
	jsonOut := fs.Bool("json", false, "With --dry-run: output as JSON")

	if err := parseCLIArgs(fs, args); err != nil {
		return err
	}

	disp, err := NewDispatcher(DispatcherOptions{
		Root:           root,
		BacklogFile:    *queuePath,
		MaxWorkers:     *workers,
		RepairAttempts: *repairs,
		NoRepairs:      *repairs == 0,
		Wait:           *waitFlag,
		WaitTimeout:    time.Duration(*waitSec) * time.Second,
		DryRun:         *dryRun,
		JSON:           *jsonOut,
		EnginePlan:     *enginePlan,
		EngineBuild:    *engineBuild,
		Out:            out,
	})
	if err != nil {
		return err
	}
	defer disp.Close()

	if *dryRun {
		rep, err := disp.DryRun(ctx)
		if err != nil {
			return err
		}
		if *jsonOut {
			s, err := rep.FormatJSON()
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, s)
		} else {
			fmt.Fprintln(stdout, rep.FormatText(*workers, 0))
		}
		return nil
	}
	return disp.Run(ctx)
}

// RunMergerCLI is an alias for RunMergeCLI.
func RunMergerCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return RunMergeCLI(ctx, args, stdout, stderr)
}

// RunMergeCLI parses CLI flags for `foreman-kit merge` and executes the Merger.
func RunMergeCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	out := stderr
	if out == nil {
		out = stdout
	}
	if out == nil {
		out = os.Stderr
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	root := store.AppRoot("")

	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	watch := fs.Bool("watch", false, "Keep polling the ready queue")
	batch := fs.Bool("batch", false, "Stack and merge ready changes in batch mode")
	interval := fs.String("interval", settings.FormatDuration(DefaultMergeInterval), "How often a watching merger looks for ready changes (10s, 1m)")

	if err := parseCLIArgs(fs, args); err != nil {
		return err
	}
	intervalSeconds, ok := cliSeconds(*interval)
	if !ok || intervalSeconds <= 0 {
		return fmt.Errorf("--interval takes a time with its unit (10s, 1m), not %q", *interval)
	}

	merger, err := NewMerger(MergerOptions{
		Root:     root,
		Watch:    *watch,
		Batch:    *batch,
		Interval: time.Duration(intervalSeconds) * time.Second,
		Out:      out,
	})
	if err != nil {
		return err
	}
	defer merger.Close()
	return merger.Run(ctx)
}
