package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/llm"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

const repairsHeaderFormat = `# Repair rounds of %s

Written by the dispatcher, never by an agent: each round that deterministic verification sent
back, what it was asked to fix, what the round changed and whether verification then passed.
It is the evidence behind the lessons in handoff.md, and the merger archives it with the change.
`

var mergeConflictFileRe = regexp.MustCompile(`Merge conflict in (\S+)`)

type failureInfo struct {
	Reason  string
	Details string
	// GateRoundID is the gate round the failure comes from (0 for a failure no gate reported).
	GateRoundID int64
}

type repairRound struct {
	Phase  string
	Engine string
	// ExecID is the execution of the agent that did the repair.
	ExecID     string
	Asked      failureInfo
	HasChanged bool
	Changed    string
}

type workerOutcome struct {
	Change string
	Result string // "ready", "blocked", "busy", "deferred", "held", "paused", "lost", "stopped"
}

// DispatcherOptions configures a Dispatcher instance.
type DispatcherOptions struct {
	Root           string
	Store          *store.Store
	Judge          *evaluator.Judge
	BacklogFile    string
	MaxWorkers     int
	RepairAttempts int
	NoRepairs      bool
	Wait           bool
	WaitTimeout    time.Duration
	PollInterval   time.Duration
	DryRun         bool
	JSON           bool
	EnginePlan     string
	EngineBuild    string
	AgentRunner    AgentRunnerFunc
	GateRunner     GateRunnerFunc
	OpenSpecRunner OpenSpecRunnerFunc
	NowFunc        func() time.Time
	Out            io.Writer
	// Hub, when set, is notified when a change enters the merge queue or a worker frees its slot, and its
	// Backlog signal wakes Run and Serve as soon as the set of eligible backlog items may have changed.
	Hub *Hub
	// FallbackInterval is how often Serve re-evaluates the backlog without a signal, a safety net for a
	// missed one (default 30s).
	FallbackInterval time.Duration
	// EngineBackoff is the cooldown Serve applies when every engine of a role is out (exit 75): it starts
	// no worker until it ends, doubles on each further outage up to EngineBackoffMax, and resets on the
	// next successful agent call (60s, and outage.max_wait of .foreman-kit/agents.yaml).
	EngineBackoff    time.Duration
	EngineBackoffMax time.Duration
}

// Dispatcher coordinates parallel worker goroutines across git worktrees for PLAN and BUILD phases.
type Dispatcher struct {
	opts      DispatcherOptions
	root      string
	mainRoot  string
	store     *store.Store
	ownsStore bool
	judge     *evaluator.Judge
	llmClient *llm.Client
	startedAt string
	// runID is this dispatcher's row of dispatcher_runs while Run or Serve is in progress.
	runID int64
	// lastExec holds, per change, the execution of the agent this dispatcher last invoked for it.
	lastExec map[string]string
	nowFunc  func() time.Time
	out      io.Writer

	// config is the dispatchConfig in effect (cfg); reloadConfig replaces it. The from* flags say which
	// values come from the configuration files rather than from DispatcherOptions.
	config            atomic.Pointer[dispatchConfig]
	workersFromConfig bool
	// repairsFromConfig and backoffMaxFromConfig say build.repair_rounds and outage.max_wait are read
	// from the files on every reload, rather than fixed by the caller.
	repairsFromConfig    bool
	backoffMaxFromConfig bool
	planFromConfig       bool
	buildFromConfig      bool

	gitMu     sync.Mutex
	stateMu   sync.Mutex
	active    map[string]int      // change -> slot
	building  map[string][]string // change -> held capabilities
	held      map[string][]string // change -> conflicting capabilities at claim time
	results   map[string]string   // change -> result
	deferrals map[string]int      // change -> consecutive deferrals
	seen      map[string]string   // change -> first seen ISO8601 timestamp
	reported  map[string]bool
	// rollbackMoved records, per change, the children refreshPhase moved past a rolled-back gitlink:
	// child path -> {gitlink before the rollback, the rolled-back gitlink}.
	rollbackMoved    map[string]map[string][2]string
	enginesDn        bool
	lastFetch        time.Time
	archived         map[string]bool
	lastValidBacklog []BacklogItem
	lastInvalidItems []map[string]any
	workerDone       chan workerOutcome

	// Continuous mode (Serve). serving is written before any worker starts and never changes after.
	serving bool
	// Engine cooldowns, per agent role ("plan", "build"): an outage of every engine of a role opens a
	// window during which no worker starts; one outage opens one window, however many workers hit it.
	cooldownUntil  map[string]time.Time     // role -> end of its current window
	cooldownLength map[string]time.Duration // role -> length of its current window
	nextBackoff    map[string]time.Duration // role -> length of its next window
	lastDeferral   map[string]time.Time     // change -> when its last counted deferral happened
	archivedSHA    string                   // base commit archivedGit was computed for
	archivedGit    map[string]bool          // changes archived or integrated on archivedSHA
	healthNoted    time.Time                // engine cooldown (engines.cooldown_until) end already logged
}

// DefaultDispatchWait is how long a one-pass dispatch waits while changes wait on merges, unless
// `foreman-kit dispatch --wait 30m` says otherwise. The daemon does not wait: it runs for good.
const DefaultDispatchWait = time.Hour

// idleFetchInterval is how often a dispatcher with nothing to start fetches origin.
const idleFetchInterval = 10 * time.Second

// deferralLimitText writes the number of outages a change waits through, for a log line.
func deferralLimitText(limit int) string {
	if limit <= 0 {
		return settings.Never
	}
	return strconv.Itoa(limit)
}

// NewDispatcher creates and initializes a Dispatcher.
func NewDispatcher(opts DispatcherOptions) (*Dispatcher, error) {
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

	judge := opts.Judge
	if judge == nil {
		var err error
		judge, err = evaluator.NewJudge(root, st)
		if err != nil {
			if ownsStore {
				_ = st.Close()
			}
			return nil, err
		}
	}
	if err := judge.Policy().RequireAPIKeyForActiveGates(); err != nil && opts.Judge == nil && !opts.DryRun {
		// The fix may already be on origin (the gates turned off, pushed from another clone): the main
		// checkout is only fast-forwarded once a dispatcher runs, so without this a daemon would refuse
		// every 10 s forever on a policy nobody holds any more.
		if r, ferr := gitx.Open(root); ferr == nil {
			_ = r.Fetch(context.Background(), settings.Remote, settings.BaseBranch)
		}
		if gitx.FastForwardCheckout(context.Background(), root, settings.Remote, settings.BaseBranch, mainCheckoutRestorable).Pulled > 0 {
			if again, jerr := evaluator.NewJudge(root, st); jerr == nil {
				judge = again
			}
		}
	}
	if err := judge.Policy().RequireAPIKeyForActiveGates(); err != nil {
		if ownsStore {
			_ = st.Close()
		}
		return nil, err
	}

	if opts.BacklogFile == "" {
		opts.BacklogFile = filepath.Join(root, ".foreman-kit", "agent_backlog.json")
	}
	workersFromConfig, planFromConfig, buildFromConfig := opts.MaxWorkers <= 0, opts.EnginePlan == "", opts.EngineBuild == ""
	if opts.MaxWorkers <= 0 {
		opts.MaxWorkers = settings.Workers
	}
	// The rounds come from build.repair_rounds unless the caller fixed them; NoRepairs is 0 rounds.
	repairsFromConfig := opts.RepairAttempts <= 0 && !opts.NoRepairs
	if repairsFromConfig {
		opts.RepairAttempts = settings.RepairRounds
	}
	if opts.WaitTimeout <= 0 {
		opts.WaitTimeout = DefaultDispatchWait
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 200 * time.Millisecond
	}
	if opts.FallbackInterval <= 0 {
		opts.FallbackInterval = 30 * time.Second
	}
	if opts.EngineBackoff <= 0 {
		opts.EngineBackoff = time.Minute
	}
	backoffMaxFromConfig := opts.EngineBackoffMax < opts.EngineBackoff
	if backoffMaxFromConfig {
		opts.EngineBackoffMax = settings.OutageMaxWait
		if opts.EngineBackoffMax < opts.EngineBackoff {
			opts.EngineBackoffMax = opts.EngineBackoff
		}
	}
	if opts.EnginePlan == "" {
		opts.EnginePlan = LoadAgentRoleEngine(root, "plan")
	}
	if opts.EngineBuild == "" {
		opts.EngineBuild = LoadAgentRoleEngine(root, "build")
	}
	if opts.AgentRunner == nil {
		opts.AgentRunner = DefaultAgentRunner(st)
	}
	if opts.GateRunner == nil {
		opts.GateRunner = DefaultGateRunner(st)
	}
	if opts.OpenSpecRunner == nil {
		opts.OpenSpecRunner = DefaultOpenSpecRunner()
	}
	nowFn := opts.NowFunc
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}

	d := &Dispatcher{
		opts:       opts,
		root:       root,
		mainRoot:   mainRoot,
		store:      st,
		ownsStore:  ownsStore,
		judge:      judge,
		llmClient:  llm.NewClientForRoot(root, st),
		nowFunc:    nowFn,
		out:        out,
		active:     make(map[string]int),
		building:   make(map[string][]string),
		held:       make(map[string][]string),
		results:    make(map[string]string),
		deferrals:  make(map[string]int),
		seen:       make(map[string]string),
		reported:   make(map[string]bool),
		archived:   make(map[string]bool),
		workerDone: make(chan workerOutcome, 64),

		lastDeferral:   make(map[string]time.Time),
		cooldownUntil:  make(map[string]time.Time),
		cooldownLength: make(map[string]time.Duration),
		nextBackoff:    make(map[string]time.Duration),

		workersFromConfig: workersFromConfig,
		planFromConfig:    planFromConfig,
		buildFromConfig:   buildFromConfig,
	}
	d.repairsFromConfig, d.backoffMaxFromConfig = repairsFromConfig, backoffMaxFromConfig
	d.config.Store(&dispatchConfig{PipelineSettings: settings, workers: opts.MaxWorkers, enginePlan: opts.EnginePlan, engineBuild: opts.EngineBuild,
		repairRounds: opts.RepairAttempts, backoffMax: opts.EngineBackoffMax})
	d.initOrchestratorTables(context.Background())
	return d, nil
}

// dispatchConfig is what a Dispatcher reads from .foreman-kit/pipeline.yaml and .foreman-kit/agents.yaml: the
// pipeline settings, the worker count and the plan and build engines. Run reads it once. Serve runs for
// the daemon's whole life, so it reloads it before every pass (reloadConfig): an edit, such as a longer
// gate timeout or an engine switched during a quota outage, applies to the next worker it starts and
// to the next step of a running one, without restarting the daemon. A worker reads whichever snapshot
// is current, never a half-written one.
type dispatchConfig struct {
	PipelineSettings
	workers     int
	enginePlan  string
	engineBuild string
	// repairRounds is how many times a phase is sent back to its agent, and backoffMax the longest
	// no worker starts after an engine outage.
	repairRounds int
	backoffMax   time.Duration
}

// cfg returns the configuration in effect.
func (d *Dispatcher) cfg() *dispatchConfig { return d.config.Load() }

// waits says why the dispatcher starts no change with this configuration ("" when it can): what
// PipelineSettings.DispatcherWaits says, or a role that lists no agent. Nothing runs in the place of
// an agent nobody named.
func (c *dispatchConfig) waits() string {
	if why := c.DispatcherWaits(); why != "" {
		return why
	}
	for _, role := range []struct{ name, engines string }{{"plan", c.enginePlan}, {"build", c.engineBuild}} {
		if strings.TrimSpace(role.engines) == "" {
			return fmt.Sprintf("%s lists no agent: no change starts until it names the agent that %s", settings.RoleAgents(role.name).Where(), settings.RoleDoes[role.name])
		}
	}
	return ""
}

// reloadConfig re-reads .foreman-kit/pipeline.yaml and .foreman-kit/agents.yaml. Values the caller fixed in
// DispatcherOptions stay as they were. When the engines of a role change, the engine cooldown ends at
// once: the old engines were out, and the new ones get their chance on this pass.
func (d *Dispatcher) reloadConfig() {
	cur := d.cfg()
	next := &dispatchConfig{PipelineSettings: LoadPipelineSettings(d.root), workers: cur.workers, enginePlan: cur.enginePlan, engineBuild: cur.engineBuild,
		repairRounds: cur.repairRounds, backoffMax: cur.backoffMax}
	if d.workersFromConfig && next.Workers > 0 {
		next.workers = next.Workers
	}
	if d.repairsFromConfig {
		next.repairRounds = next.RepairRounds
	}
	if d.backoffMaxFromConfig && next.OutageMaxWait >= d.opts.EngineBackoff {
		next.backoffMax = next.OutageMaxWait
	}
	if d.planFromConfig {
		next.enginePlan = LoadAgentRoleEngine(d.root, "plan")
	}
	if d.buildFromConfig {
		next.engineBuild = LoadAgentRoleEngine(d.root, "build")
	}
	d.config.Store(next)
	if next.enginePlan == cur.enginePlan && next.engineBuild == cur.engineBuild {
		return
	}
	d.stateMu.Lock()
	if next.enginePlan != cur.enginePlan {
		d.endCooldownLocked("plan")
	}
	if next.engineBuild != cur.engineBuild {
		d.endCooldownLocked("build")
	}
	d.stateMu.Unlock()
	d.logf("dispatcher: engines changed (plan %s, build %s): any engine cooldown ends", next.enginePlan, next.engineBuild)
}

func (d *Dispatcher) initOrchestratorTables(ctx context.Context) {
	if d.store == nil || d.store.DB() == nil {
		return
	}
	d.loadPersistedState(ctx)
	_ = d.store.SyncJSONToSQLite(ctx, d.root)
}

func (d *Dispatcher) loadPersistedState(ctx context.Context) {
	if d.store == nil || d.store.DB() == nil {
		return
	}
	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	if counts, err := d.store.DeferralCounts(ctx); err == nil {
		d.deferrals = make(map[string]int)
		d.lastDeferral = make(map[string]time.Time)
		for chg, c := range counts {
			if c.Count <= 0 {
				continue
			}
			d.deferrals[chg] = c.Count
			if t, err := time.Parse(time.RFC3339, c.LastAt); err == nil {
				d.lastDeferral[chg] = t
			}
		}
	}
	if seen, err := d.store.ChangesFirstSeen(ctx); err == nil {
		for chg, seenAt := range seen {
			if seenAt != "" {
				d.seen[chg] = seenAt
			}
		}
	}
}

// Close closes the underlying Store if the Dispatcher opened it.
func (d *Dispatcher) Close() error {
	if d.ownsStore && d.store != nil {
		return d.store.Close()
	}
	return nil
}

func (d *Dispatcher) worktreeBase() string {
	return settings.WorktreesBase(d.mainRoot)
}

// DryRun inspects the backlog, locks, ready queue, and quarantine state without modifying anything,
// returning a DryRunReport explaining what is ready, building, merging, blocked, held, or waiting.
func (d *Dispatcher) DryRun(ctx context.Context) (*DryRunReport, error) {
	d.loadPersistedState(ctx)
	d.fetchOriginQuiet(ctx)
	archived := d.computeArchivedOnBase(ctx)
	d.stateMu.Lock()
	d.archived = archived
	d.stateMu.Unlock()

	backlog, invalidItems, waited := d.loadBacklog(ctx, false)
	ws, _ := d.store.GetWorkspaceState(ctx)
	running := d.runningDispatchInfo(ctx)
	mainBehind := 0
	if out, err := gitx.RunGit(ctx, d.root, "rev-list", "--count", "HEAD.."+d.cfg().Remote+"/"+d.cfg().BaseBranch); err == nil {
		mainBehind, _ = strconv.Atoi(strings.TrimSpace(out))
	}

	locks, _ := d.store.ListLocks(ctx)
	nowUnix := d.nowFunc().Unix()
	host, _ := os.Hostname()
	buildingSet := make(map[string]bool)
	var buildingList []string
	for _, lk := range locks {
		if chg, ok := store.WorkerLeaseChange(lk.LockKey); ok && !store.IsLockStale(lk, host, nowUnix) {
			if !buildingSet[chg] {
				buildingSet[chg] = true
				buildingList = append(buildingList, chg)
			}
		}
	}

	readyManifests, _ := d.store.ListReadyQueue(ctx)
	readySet := make(map[string]bool, len(readyManifests))
	inFlightCapsMap := make(map[string]bool)
	for _, rm := range readyManifests {
		readySet[rm.ChangeName] = true
		for _, c := range rm.Capabilities {
			inFlightCapsMap[c] = true
		}
	}

	byName := make(map[string]BacklogItem, len(backlog))
	for _, it := range backlog {
		byName[it.ChangeName] = it
	}
	for _, chg := range buildingList {
		for _, c := range d.claimedCapabilities(chg, byName[chg]) {
			inFlightCapsMap[c] = true
		}
	}

	quarantines, _ := d.store.ListQuarantine(ctx, "")
	qByName := make(map[string]store.QuarantineRecord, len(quarantines))
	for _, q := range quarantines {
		qByName[q.ChangeName] = q
	}

	freeWorkers := d.cfg().workers
	if running != nil {
		freeWorkers = running.Workers - len(buildingList)
		if freeWorkers < 0 {
			freeWorkers = 0
		}
	}

	var rows []DryRunItem
	for _, inv := range invalidItems {
		name := strings.TrimSpace(fmt.Sprint(inv["change_name"]))
		if name == "" || name == "<nil>" {
			name = "(missing change_name)"
		}
		prio := strings.TrimSpace(fmt.Sprint(inv["priority"]))
		if prio == "" || prio == "<nil>" {
			prio = "normal"
		}
		rows = append(rows, DryRunItem{
			Change:      name,
			Priority:    prio,
			CountsAs:    "-",
			WaitedHours: nil,
			State:       "invalid",
			Next:        false,
			Why:         "invalid change_name: use 1-63 lowercase letters, digits or hyphens; start with a letter or digit",
		})
	}

	now := d.nowFunc()
	for _, item := range backlog {
		if archived[item.ChangeName] {
			continue
		}
		rows = append(rows, d.buildDryRunRow(item, waited, running, buildingSet, readySet, inFlightCapsMap, qByName, byName, archived, now))
	}

	if held := heldByBrokenBases(ctx, d.root, d.store, readyManifests, d.cfg().OutsideCommits); len(held) > 0 {
		for i := range rows {
			if why, ok := held[rows[i].Change]; ok && rows[i].State == "merging" {
				rows[i].Why = why
			}
		}
	}

	readyIdx := 0
	for i := range rows {
		if rows[i].State != "ready" {
			continue
		}
		rows[i].Next = readyIdx < freeWorkers
		if ws.Paused {
			rows[i].Why = "starts once the workspace is resumed"
		} else if readyIdx < freeWorkers {
			if running != nil {
				rows[i].Why = "next: starts as soon as this dispatch looks again"
			} else {
				rows[i].Why = "next: starts in the next dispatch"
			}
		} else {
			rows[i].Why = "ready: starts when a worker is free"
		}
		readyIdx++
	}

	if rows == nil {
		rows = []DryRunItem{}
	}
	return &DryRunReport{
		Paused:       ws.Paused,
		PauseReason:  ws.PauseReason,
		Dispatch:     running,
		FreeWorkers:  freeWorkers,
		Items:        rows,
		MainBehind:   mainBehind,
		Upstream:     d.cfg().Remote + "/" + d.cfg().BaseBranch,
		BacklogError: config.BacklogProblem(d.opts.BacklogFile),
	}, nil
}

func (d *Dispatcher) buildDryRunRow(
	item BacklogItem,
	waited map[string]*float64,
	running *RunningDispatchInfo,
	building map[string]bool,
	ready map[string]bool,
	inFlightCaps map[string]bool,
	quarantined map[string]store.QuarantineRecord,
	byName map[string]BacklogItem,
	archived map[string]bool,
	now time.Time,
) DryRunItem {
	change := item.ChangeName
	prio := item.Priority
	if prio == "" {
		prio = "normal"
	}
	hrs := waited[change]
	effRank := d.priorityOf(item, hrs)
	countsAs := PriorityNames[effRank]
	if countsAs == "" {
		countsAs = "normal"
	}
	var roundedHours *float64
	if hrs != nil {
		r := math.Round(*hrs*10) / 10
		roundedHours = &r
	}

	var state, why string
	q, hasQ := quarantined[change]
	isBlocked := hasQ && q.Status == "blocked"

	var unarchivedDeps []string
	for _, dep := range item.DependsOn {
		if !archived[dep] {
			unarchivedDeps = append(unarchivedDeps, dep)
		}
	}

	var sharedCaps []string
	for _, c := range d.claimedCapabilities(change, item) {
		if inFlightCaps[c] {
			sharedCaps = append(sharedCaps, c)
		}
	}

	switch {
	case building[change]:
		state = "building"
		why = "building now"
	case ready[change]:
		state = "merging"
		why = "built: waiting for the merger"
	case isBlocked:
		state = "blocked"
		qInfo := BuildQuarantineInfo(q, d.cfg().BlockedRetries, d.cfg().BlockedRetryAfter, now)
		if qInfo.RetryNote != "" {
			why = fmt.Sprintf("blocked: %s (%s)", q.Reason, qInfo.RetryNote)
		} else {
			why = fmt.Sprintf("blocked: %s", q.Reason)
		}
	case len(unarchivedDeps) > 0:
		state = "waiting"
		var parts []string
		for _, dep := range unarchivedDeps {
			parts = append(parts, fmt.Sprintf("%s (%s)", dep, d.dependencyState(dep, building, ready, quarantined, byName)))
		}
		why = "waits for " + strings.Join(parts, ", ")
	case len(sharedCaps) > 0:
		state = "held"
		why = "held: shares " + strings.Join(sharedCaps, ", ") + " with a change in flight"
	case running != nil && running.Results[change] != "":
		state = "next_dispatch"
		why = fmt.Sprintf("starts in the next dispatch: this one already ended it (%s)", running.Results[change])
	default:
		state = "ready"
	}

	d.stateMu.Lock()
	defCount := d.deferrals[change]
	d.stateMu.Unlock()
	if why != "" && defCount > 0 {
		why = fmt.Sprintf("%s; no agent could work on it %d time(s) in a row", why, defCount)
	}

	return DryRunItem{
		Change:      change,
		Priority:    prio,
		CountsAs:    countsAs,
		WaitedHours: roundedHours,
		State:       state,
		Why:         why,
		Next:        false,
	}
}

func (d *Dispatcher) dependencyState(dep string, building, ready map[string]bool, quarantined map[string]store.QuarantineRecord, byName map[string]BacklogItem) string {
	if building[dep] {
		return "building"
	}
	if ready[dep] {
		return "built, waiting for the merger"
	}
	if q, ok := quarantined[dep]; ok && q.Status == "blocked" {
		return "blocked"
	}
	if _, ok := byName[dep]; ok {
		return "not built yet"
	}
	return "not in the backlog"
}

func (d *Dispatcher) runningDispatchInfo(ctx context.Context) *RunningDispatchInfo {
	lk, err := d.store.GetLock(ctx, "dispatcher")
	if err != nil || lk == nil {
		return nil
	}
	host, _ := os.Hostname()
	if store.IsLockStale(*lk, host, d.nowFunc().Unix()) {
		return nil
	}
	info := &RunningDispatchInfo{
		PID:     lk.PID,
		Workers: d.cfg().workers,
		Results: make(map[string]string),
	}
	if lk.MetadataJSON != "" {
		var meta struct {
			StartedAt string            `json:"started_at"`
			Workers   int               `json:"workers"`
			Results   map[string]string `json:"results"`
		}
		if json.Unmarshal([]byte(lk.MetadataJSON), &meta) == nil {
			info.StartedAt = meta.StartedAt
			if meta.Workers > 0 {
				info.Workers = meta.Workers
			}
			if meta.Results != nil {
				info.Results = meta.Results
			}
		}
	}
	return info
}

// Run executes the dispatcher scheduling loop until all eligible changes are processed or paused.
func (d *Dispatcher) Run(ctx context.Context) error {
	if d.opts.DryRun {
		rep, err := d.DryRun(ctx)
		if err != nil {
			return err
		}
		if d.opts.JSON {
			s, err := rep.FormatJSON()
			if err != nil {
				return err
			}
			fmt.Println(s)
		} else {
			// What would start does not start while the dispatcher waits for a file: say so first.
			if why := d.cfg().waits(); why != "" {
				fmt.Println("dispatcher: " + why)
			}
			fmt.Println(rep.FormatText(d.cfg().workers, 0))
		}
		return nil
	}
	if why := d.cfg().waits(); why != "" {
		return errors.New("dispatcher: " + why)
	}

	d.loadPersistedState(ctx)
	d.stateMu.Lock()
	d.enginesDn = false
	d.results = make(map[string]string)
	d.stateMu.Unlock()

	d.startedAt = d.nowFunc().Format(time.RFC3339)
	leaseTTL := d.cfg().LocksStaleAfter
	_, err := d.store.AcquireLock(ctx, "dispatcher", store.LockOptions{
		OwnerType: "dispatcher",
		PID:       os.Getpid(),
		TTL:       leaseTTL,
		Metadata: map[string]any{
			"started_at": d.startedAt,
			"workers":    d.cfg().workers,
			"results":    map[string]string{},
		},
	})
	if errors.Is(err, store.ErrLockHeld) {
		if d.out != nil {
			_, _ = fmt.Fprint(d.out, "dispatcher: already running\n")
		}
		return nil
	}
	if err != nil {
		return err
	}
	stopHB := d.store.StartLockHeartbeat(ctx, "dispatcher", os.Getpid(), 2*time.Hour)
	defer func() {
		stopHB()
		_ = d.store.ReleaseLock(context.Background(), "dispatcher", os.Getpid())
	}()
	// Before any worker reclaims a change lease a dead dispatcher left: stop the agents it left running.
	reapOrphanAgents(ctx, d.store, d.logf)

	// This run is a row of dispatcher_runs; what it starts and emits points at it through ctx.
	d.runID, _ = d.store.BeginDispatcherRun(ctx, "once", d.cfg().workers, 0)
	ctx = store.WithDispatcherRun(ctx, d.runID)
	endReason := "finished"
	defer func() {
		_ = d.store.FinishDispatcherRun(context.Background(), d.runID, endReason)
		d.runID = 0
	}()

	backlog, _, _ := d.loadBacklog(ctx, true)
	_ = d.store.SetDispatcherRunBacklog(ctx, d.runID, len(backlog))
	_ = d.store.EmitEvent(ctx, store.EventInput{
		Event: "dispatcher.start",
		Payload: map[string]any{
			"workers": d.cfg().workers,
			"backlog": len(backlog),
		},
	})

	var waitDeadline time.Time
	for {
		if ctx.Err() != nil {
			break
		}
		p := d.pass(ctx)
		backlog = p.backlog
		paused := p.paused

		if p.halted && d.activeCount() == 0 {
			evt := "dispatcher.engines_down"
			endReason = "engines_down"
			if paused {
				evt = "dispatcher.paused"
				endReason = "paused"
			}
			_ = d.store.EmitEvent(ctx, store.EventInput{Event: evt})
			break
		}

		if d.activeCount() == 0 && (len(p.eligible) == 0 || paused) {
			var waiting []string
			for _, it := range p.pending {
				if d.waitingOnFlight(ctx, it, p.byName, map[string]bool{}) {
					waiting = append(waiting, it.ChangeName)
				}
			}
			readyQueue, _ := d.store.ListReadyQueue(ctx)
			if !d.opts.Wait || paused || len(waiting) == 0 || len(readyQueue) == 0 {
				break
			}
			if waitDeadline.IsZero() {
				waitDeadline = d.nowFunc().Add(d.opts.WaitTimeout)
			}
			if !d.nowFunc().Before(waitDeadline) {
				_ = d.store.EmitEvent(ctx, store.EventInput{
					Event: "dispatcher.wait_timeout",
					Payload: map[string]any{
						"seconds": int(d.opts.WaitTimeout.Seconds()),
						"waiting": strings.Join(waiting, ","),
					},
				})
				endReason = "wait_timeout"
				break
			}
		} else {
			waitDeadline = time.Time{}
		}

		select {
		case <-ctx.Done():
			endReason = "stopped"
			goto doneLoop
		case out := <-d.workerDone:
			d.recordWorkerOutcome(ctx, out)
		case <-d.opts.Hub.Backlog():
		case <-time.After(d.opts.PollInterval):
		}
	}

doneLoop:
	// Wait for any remaining active workers if context was cancelled
	for {
		d.stateMu.Lock()
		rem := len(d.active)
		d.stateMu.Unlock()
		if rem == 0 {
			break
		}
		out := <-d.workerDone
		d.recordWorkerOutcome(ctx, out)
	}

	byName := make(map[string]BacklogItem, len(backlog))
	for _, it := range backlog {
		byName[it.ChangeName] = it
	}
	var waitingNames []string
	for _, it := range backlog {
		if d.waitingOnFlight(ctx, it, byName, map[string]bool{}) {
			waitingNames = append(waitingNames, it.ChangeName)
		}
	}
	d.stateMu.Lock()
	resCopy := make(map[string]string, len(d.results))
	for k, v := range d.results {
		resCopy[k] = v
	}
	d.stateMu.Unlock()
	d.reconcileResults(context.Background(), resCopy)

	_ = d.store.EmitEvent(context.Background(), store.EventInput{
		Event:           "dispatcher.finish",
		DispatcherRunID: d.runID,
		Payload: map[string]any{
			"results": resCopy,
			"waiting": strings.Join(waitingNames, ","),
		},
	})
	d.logf("dispatcher: finished: %s", summarizeResults(resCopy, waitingNames))
	return nil
}

// summarizeResults renders a dispatch's outcomes for the log: each result with its changes, then
// the changes still waiting on another.
func summarizeResults(results map[string]string, waiting []string) string {
	byResult := map[string][]string{}
	for change, result := range results {
		byResult[result] = append(byResult[result], change)
	}
	var keys []string
	for k := range byResult {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		sort.Strings(byResult[k])
		parts = append(parts, fmt.Sprintf("%s %s", k, strings.Join(byResult[k], ", ")))
	}
	if len(parts) == 0 {
		parts = append(parts, "nothing built")
	}
	if len(waiting) > 0 {
		parts = append(parts, "waiting "+strings.Join(waiting, ", "))
	}
	return strings.Join(parts, "; ")
}

// reconcileResults corrects the outcome a worker recorded with what happened to the change
// since: a change it left "ready" that the merger then quarantined (a rebase conflict its
// rebuilds could not resolve, a gate failure at merge) ends this dispatch blocked, not ready.
func (d *Dispatcher) reconcileResults(ctx context.Context, results map[string]string) {
	for change, result := range results {
		if result != "ready" {
			continue
		}
		q, err := d.store.GetQuarantine(ctx, change)
		if err != nil || q == nil {
			continue
		}
		switch q.Status {
		case "blocked":
			if q.Reason != "" {
				results[change] = "blocked:" + q.Reason
			} else {
				results[change] = "blocked"
			}
		case "rebuild_requested":
			results[change] = "rebuild_requested"
		}
	}
}

// passResult is what one scheduling pass saw.
type passResult struct {
	backlog  []BacklogItem
	byName   map[string]BacklogItem
	pending  []BacklogItem // neither running nor already ended by this dispatcher
	eligible []BacklogItem // pending items whose dependencies, quarantine and capabilities allow a start
	paused   bool
	halted   bool // paused, or every engine of a role is out: the pass started no worker
}

// pass runs one scheduling pass, shared by Run and Serve: it records finished workers, refreshes the
// base, reloads the backlog, revives blocked changes whose retry is due, re-queues changes the merger
// sent back, and starts a worker for each eligible item while a slot is free.
func (d *Dispatcher) pass(ctx context.Context) passResult {
	// This pass observes whatever a pending signal announced (its own finished workers included).
	select {
	case <-d.opts.Hub.Backlog():
	default:
	}
	// From here on the pass reads what is queued: an item queued before this instant is in what the
	// pass reports to the Hub when it ends.
	passAt := d.nowFunc()
	d.reapWorkers(ctx)

	// Read the merge queue before the base: a change the merger removed from it after integrating it is
	// then already on the base read below, so forgetting its result can never build it twice.
	var readySnap map[string]bool
	readyOK := false
	if d.serving {
		readySnap, readyOK = d.readyNames(ctx)
	}
	d.refreshRemote(ctx, d.activeCount() == 0)
	if d.serving && readyOK {
		d.forgetSettledResults(ctx, readySnap)
	}

	backlog, _, _ := d.loadBacklog(ctx, true)
	byName := make(map[string]BacklogItem, len(backlog))
	for _, it := range backlog {
		byName[it.ChangeName] = it
	}

	paused := d.isPaused(ctx)
	if !paused && !d.isEnginesDown() {
		d.retryBlocked(ctx, backlog)
	}

	// Re-queue changes sent back by the merger for conflict rebuild
	d.requeueRequestedRebuilds(ctx)

	var pending []BacklogItem
	deferred := 0 // parked by an engine outage: they start again by themselves once its cooldown ends
	d.stateMu.Lock()
	for _, item := range backlog {
		if _, running := d.active[item.ChangeName]; running {
			continue
		}
		if res, done := d.results[item.ChangeName]; done {
			if res == "deferred" {
				deferred++
			}
			continue
		}
		pending = append(pending, item)
	}
	d.stateMu.Unlock()

	var eligible []BacklogItem
	for _, item := range pending {
		if d.isEligible(ctx, item) {
			eligible = append(eligible, item)
		}
	}

	_ = d.store.HeartbeatLock(ctx, "dispatcher", os.Getpid())

	halted := paused || d.isEnginesDown()
	started := 0
	if !halted {
		startable := eligible
		if d.serving {
			startable = d.startable(ctx, eligible)
			if len(startable) > 0 {
				if cooling, until := d.enginesCoolingDown(ctx); cooling {
					if !until.Equal(d.healthNoted) {
						d.healthNoted = until
						d.logf("dispatcher: every engine of a role is cooling down until %s (engines.cooldown_until); starting nothing", until.Format(time.RFC3339))
					}
					startable = nil
				}
			}
		}
		d.stateMu.Lock()
		freeSlots := d.cfg().workers - len(d.active)
		d.stateMu.Unlock()
		for i := 0; i < freeSlots && i < len(startable); i++ {
			d.startWorker(ctx, startable[i])
			started++
		}
	}

	if d.serving {
		// What this pass left, for the daemon's cycle loop: with a worker free and no eligible item
		// left to start, only new backlog items give the pipeline something to do, and those come
		// from the cycle. An item an engine outage deferred still waits for a worker: it is work the
		// pipeline has, not work it lacks.
		d.opts.Hub.DispatchPassed(DispatchLoad{
			Workers: d.cfg().workers,
			Running: d.activeCount(),
			Waiting: len(eligible) - started + deferred,
			Halted:  halted,
			At:      passAt,
		})
	}

	return passResult{
		backlog:  backlog,
		byName:   byName,
		pending:  pending,
		eligible: eligible,
		paused:   paused,
		halted:   halted,
	}
}

// Serve dispatches continuously until ctx is cancelled. It is the daemon's dispatcher: unlike Run, it
// never exits when nothing is eligible. It runs a pass when a worker finishes, on every Hub.Backlog
// signal (a backlog edit, an integration, an unblock), and every FallbackInterval in case a signal was
// missed, so an item that becomes eligible starts as soon as a worker slot is free.
//
// An engine outage (exit 75) never stops it and never pauses the workspace: it starts no worker for a
// cooldown of EngineBackoff, doubling up to EngineBackoffMax, and counts at most one deferral per change
// per cooldown window. On cancellation it waits for its workers, whose context is cancelled too.
//
// It returns store.ErrLockHeld when another dispatcher (a cron cycle or a CLI) holds the dispatcher lock;
// the caller retries later.
func (d *Dispatcher) Serve(ctx context.Context) error {
	pid := os.Getpid()
	if why := d.cfg().waits(); why != "" {
		return errors.New("dispatcher: " + why)
	}
	d.loadPersistedState(ctx)
	d.stateMu.Lock()
	d.serving = true
	d.enginesDn = false
	d.results = make(map[string]string)
	d.stateMu.Unlock()

	d.startedAt = d.nowFunc().Format(time.RFC3339)
	ttl := d.cfg().LocksStaleAfter
	if _, err := d.store.AcquireLock(ctx, "dispatcher", store.LockOptions{
		OwnerType: "dispatcher",
		PID:       pid,
		TTL:       ttl,
		Metadata: map[string]any{
			"started_at": d.startedAt,
			"workers":    d.cfg().workers,
			"results":    map[string]string{},
			"mode":       "continuous",
		},
	}); err != nil {
		return err
	}
	stopHB := d.store.StartLockHeartbeat(ctx, "dispatcher", pid, ttl)
	defer func() {
		stopHB()
		_ = d.store.ReleaseLock(context.WithoutCancel(ctx), "dispatcher", pid)
	}()
	reapOrphanAgents(ctx, d.store, d.logf)

	// This run is a row of dispatcher_runs; what it starts and emits points at it through ctx.
	d.runID, _ = d.store.BeginDispatcherRun(ctx, "continuous", d.cfg().workers, 0)
	ctx = store.WithDispatcherRun(ctx, d.runID)

	// Workers run under workCtx: when another process takes the dispatcher lock over (below), they stop
	// with the scheduling loop, so two dispatchers never build at once.
	workCtx, stopWork := context.WithCancel(ctx)
	defer stopWork()
	finish := func() {
		// No more passes: what the last one left stops saying anything about the pipeline.
		d.opts.Hub.DispatchStopped()
		done := context.WithoutCancel(ctx)
		d.drainWorkers(done)
		_ = d.store.EmitEvent(done, store.EventInput{
			Event:   "dispatcher.finish",
			Payload: map[string]any{"results": d.resultsCopy(), "mode": "continuous"},
		})
		_ = d.store.FinishDispatcherRun(done, d.runID, "stopped")
		d.runID = 0
	}

	backlog, _, _ := d.loadBacklog(ctx, true)
	_ = d.store.SetDispatcherRunBacklog(ctx, d.runID, len(backlog))
	_ = d.store.EmitEvent(ctx, store.EventInput{
		Event: "dispatcher.start",
		Payload: map[string]any{
			"workers": d.cfg().workers,
			"backlog": len(backlog),
			"mode":    "continuous",
		},
	})

	fallback := time.NewTicker(d.opts.FallbackInterval)
	defer fallback.Stop()
	unusable := "" // why the settings cannot be used, while they cannot
	for {
		// store.IsLockStale lets another process take a lock whose heartbeat is older than its TTL even
		// while its owner lives (a laptop asleep for longer than locks.stale_after, a suspended VM),
		// and the background heartbeat ignores the takeover: check before every pass.
		if errors.Is(d.store.HeartbeatLock(ctx, "dispatcher", pid), store.ErrLockHeld) {
			d.logf("dispatcher: another process took the dispatcher lock (this process's heartbeat had expired); stopping its workers and starting nothing until the lock is free")
			stopWork()
			finish()
			return store.ErrLockHeld
		}
		d.reloadConfig()
		// A file edited while the daemon runs may stop naming what the dispatcher cannot do without:
		// it then starts nothing new, says so once, and goes on as soon as the file is mended.
		if why := d.cfg().waits(); why != "" {
			if why != unusable {
				d.logf("dispatcher: %s", why)
			}
			unusable = why
		} else {
			unusable = ""
			d.pass(workCtx)
		}
		select {
		case <-ctx.Done():
			finish()
			return nil
		case out := <-d.workerDone:
			d.recordWorkerOutcome(ctx, out)
			d.logf("dispatcher: woke (worker %s: %s)", out.Change, out.Result)
		case <-d.opts.Hub.Backlog():
			d.logf("dispatcher: woke (backlog)")
		case <-fallback.C:
		}
	}
}

// drainWorkers records the outcome of every active worker, waiting for each to finish.
func (d *Dispatcher) drainWorkers(ctx context.Context) {
	for d.activeCount() > 0 {
		d.recordWorkerOutcome(ctx, <-d.workerDone)
	}
}

func (d *Dispatcher) activeCount() int {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	return len(d.active)
}

func (d *Dispatcher) resultsCopy() map[string]string {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	out := make(map[string]string, len(d.results))
	for k, v := range d.results {
		out[k] = v
	}
	return out
}

// readyNames returns the changes in the merge queue; ok is false when it could not be read.
func (d *Dispatcher) readyNames(ctx context.Context) (map[string]bool, bool) {
	items, err := d.store.ListReadyQueue(ctx)
	if err != nil {
		return nil, false
	}
	out := make(map[string]bool, len(items))
	for _, it := range items {
		out[it.ChangeName] = true
	}
	return out, true
}

// forgetSettledResults lets Serve start a change again once what ended it has changed. Run remembers
// every result until it exits ("this dispatch already ended it"); a dispatcher that never exits forgets a
// result unless the change still waits on the merger (in ready, as read before the base), is blocked
// (a human or a due retry must act first), or was deferred and the engines are still cooling down.
// Results of changes already on the base are forgotten too: such a change is never eligible again.
func (d *Dispatcher) forgetSettledResults(ctx context.Context, ready map[string]bool) {
	now := d.nowFunc()
	d.stateMu.Lock()
	results := make(map[string]string, len(d.results))
	for k, v := range d.results {
		results[k] = v
	}
	cooling := d.coolingLocked(now)
	d.stateMu.Unlock()

	for change, res := range results {
		d.stateMu.Lock()
		archived := d.archived[change]
		d.stateMu.Unlock()
		if !archived {
			if ready[change] || (res == "deferred" && cooling) || d.isBlocked(ctx, change) {
				continue
			}
		}
		d.stateMu.Lock()
		if d.results[change] == res {
			delete(d.results, change)
		}
		d.stateMu.Unlock()
	}
}

// startable drops the eligible items whose worker lease is held elsewhere (another dispatcher, a portal
// reset): starting one would only end "busy". The lease is retried on the next pass.
func (d *Dispatcher) startable(ctx context.Context, eligible []BacklogItem) []BacklogItem {
	var out []BacklogItem
	for _, it := range eligible {
		if l, err := d.store.LiveLock(ctx, store.WorkerLeaseKey(it.ChangeName)); err == nil && l != nil {
			continue
		}
		out = append(out, it)
	}
	return out
}

// enginesCoolingDown reports whether every engine of the plan role or of the build role is in its
// engine cooldown (engines.cooldown_until), and when the first of those cooldowns ends. A worker started then would only
// defer, so Serve starts none, and no deferral is counted.
func (d *Dispatcher) enginesCoolingDown(ctx context.Context) (bool, time.Time) {
	health, err := d.store.ListEngineHealth(ctx)
	if err != nil || len(health) == 0 {
		return false, time.Time{}
	}
	now := d.nowFunc().Unix()
	for _, engines := range []string{d.cfg().enginePlan, d.cfg().engineBuild} {
		var names []string
		for _, e := range strings.Split(engines, ",") {
			if e = strings.TrimSpace(e); e != "" {
				names = append(names, e)
			}
		}
		if len(names) == 0 {
			continue
		}
		all := true
		var earliest int64
		for _, name := range names {
			h, ok := health[name]
			if !ok || h.CooldownUntil <= now {
				all = false
				break
			}
			if earliest == 0 || h.CooldownUntil < earliest {
				earliest = h.CooldownUntil
			}
		}
		if all {
			return true, time.Unix(earliest, 0).UTC()
		}
	}
	return false, time.Time{}
}

// logf writes one timestamped line to the dispatcher's output (the daemon log under Serve).
func (d *Dispatcher) logf(format string, args ...any) {
	if d.out == nil {
		return
	}
	_, _ = fmt.Fprintf(d.out, "[%s] %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func (d *Dispatcher) reapWorkers(ctx context.Context) {
	for {
		select {
		case out := <-d.workerDone:
			d.recordWorkerOutcome(ctx, out)
		default:
			return
		}
	}
}

func (d *Dispatcher) recordWorkerOutcome(ctx context.Context, out workerOutcome) {
	d.stateMu.Lock()
	// "lost": another dispatcher took the worker's lease and builds the change now.
	if out.Result != "held" && out.Result != "paused" && out.Result != "lost" {
		d.results[out.Change] = out.Result
	}
	delete(d.building, out.Change)
	delete(d.active, out.Change)
	if out.Result == "held" {
		d.logf("dispatcher: %s: held: it changes what a change in flight changes; it starts once that one ends", out.Change)
	}
	resCopy := make(map[string]string, len(d.results))
	for k, v := range d.results {
		resCopy[k] = v
	}
	d.stateMu.Unlock()
	// A slot is free.
	d.opts.Hub.BacklogChanged()

	if d.store != nil && d.store.DB() != nil {
		meta := map[string]any{
			"started_at": d.startedAt,
			"workers":    d.cfg().workers,
			"results":    resCopy,
		}
		if d.serving {
			// Keep what Serve wrote at acquisition: progress and a cron cycle read it to tell the
			// daemon's dispatcher from one started by hand.
			meta["mode"] = "continuous"
		}
		if d.runID != 0 {
			meta["dispatcher_run_id"] = d.runID
		}
		_ = d.store.SetLockMetadata(ctx, "dispatcher", os.Getpid(), meta)
	}

	// The event names the run it closes: the latest one of the change, which the worker finished with
	// this result before it reported it. A worker that found the change busy had no run.
	var runID int64
	if out.Result != "busy" {
		if runs, err := d.store.ListChangeRuns(ctx, out.Change, 1); err == nil && len(runs) == 1 && runs[0].Result == out.Result {
			runID = runs[0].ID
		}
	}
	_ = d.store.EmitEvent(ctx, store.EventInput{
		Event:       "change.finished",
		Change:      out.Change,
		ChangeRunID: runID,
		Payload: map[string]any{
			"result": out.Result,
		},
	})
}

func (d *Dispatcher) startWorker(ctx context.Context, item BacklogItem) {
	change := item.ChangeName
	d.stateMu.Lock()
	slot := 1
	for {
		used := false
		for _, s := range d.active {
			if s == slot {
				used = true
				break
			}
		}
		if !used {
			break
		}
		slot++
	}
	d.active[change] = slot
	d.stateMu.Unlock()

	go func() {
		res := d.processChange(ctx, item, slot)
		d.workerDone <- workerOutcome{Change: change, Result: res}
	}()
}

// backlogUnreadable reports, once per distinct error, a backlog file that cannot be read or parsed:
// as an event, and in the log, since nothing new starts from it (the last valid backlog, if this
// process read one, keeps being used) and it otherwise only showed in SQLite.
func (d *Dispatcher) backlogUnreadable(ctx context.Context, cause error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	key := "backlog.unreadable.logged:" + msg
	d.stateMu.Lock()
	first := !d.reported[key]
	d.reported[key] = true
	d.stateMu.Unlock()
	if first {
		d.logf("dispatcher: BACKLOG UNREADABLE: %s: %s; nothing new starts from it until it is valid JSON", d.opts.BacklogFile, msg)
	}
	d.reportOnce(ctx, "backlog.unreadable", map[string]any{"file": d.opts.BacklogFile, "error": msg})
}

func (d *Dispatcher) loadBacklog(ctx context.Context, recordSeen bool) ([]BacklogItem, []map[string]any, map[string]*float64) {
	rawBytes, err := os.ReadFile(d.opts.BacklogFile)
	if err != nil {
		if !os.IsNotExist(err) {
			d.backlogUnreadable(ctx, err)
			d.stateMu.Lock()
			cached := append([]BacklogItem(nil), d.lastValidBacklog...)
			cachedInv := append([]map[string]any(nil), d.lastInvalidItems...)
			d.stateMu.Unlock()
			if len(cached) > 0 {
				waited := d.computeWaitedHours(cached, recordSeen)
				return cached, cachedInv, waited
			}
		}
		return nil, nil, map[string]*float64{}
	}

	// A backlog is a JSON list, or the legacy {"changes": [...]} wrapper around one: the two shapes
	// internal/cycle reads (parseJSONListBytes). Anything else that parses (an object without that list:
	// items wrapped under another key, or one item without the list around it; a scalar) is no more an
	// empty backlog than a file that does not parse: read as one, the pipeline idled without a word.
	var generic any
	var rawItems []any
	isBacklog := false
	if err := json.Unmarshal(rawBytes, &generic); err == nil {
		switch v := generic.(type) {
		case map[string]any:
			rawItems, isBacklog = v["changes"].([]any)
		case []any:
			rawItems, isBacklog = v, true
		}
	}
	if !isBacklog {
		d.backlogUnreadable(ctx, err)
		if len(strings.TrimSpace(string(rawBytes))) > 0 {
			d.stateMu.Lock()
			cached := append([]BacklogItem(nil), d.lastValidBacklog...)
			cachedInv := append([]map[string]any(nil), d.lastInvalidItems...)
			d.stateMu.Unlock()
			if len(cached) > 0 {
				waited := d.computeWaitedHours(cached, recordSeen)
				return cached, cachedInv, waited
			}
		}
		return nil, nil, map[string]*float64{}
	}

	var valid []BacklogItem
	var invalid []map[string]any
	counts := make(map[string]int)

	for _, ri := range rawItems {
		m, ok := ri.(map[string]any)
		if !ok {
			invalid = append(invalid, map[string]any{"change_name": fmt.Sprint(ri)})
			d.reportOnce(ctx, "backlog.invalid_name", map[string]any{"item": fmt.Sprint(ri)})
			continue
		}
		name := strings.TrimSpace(fmt.Sprint(m["change_name"]))
		if !ChangeNameRe.MatchString(name) {
			invalid = append(invalid, m)
			d.reportOnce(ctx, "backlog.invalid_name", map[string]any{"change": name})
			continue
		}
		counts[name]++
		valid = append(valid, BacklogItemFromMap(m))
	}

	for name, cnt := range counts {
		if cnt > 1 {
			d.reportOnce(ctx, "backlog.duplicate_name", map[string]any{"change": name, "count": cnt})
		}
	}

	waited := d.computeWaitedHours(valid, recordSeen)
	type indexedItem struct {
		item BacklogItem
		idx  int
		rank int
	}
	indexed := make([]indexedItem, len(valid))
	for i, it := range valid {
		indexed[i] = indexedItem{
			item: it,
			idx:  i,
			rank: d.priorityOf(it, waited[it.ChangeName]),
		}
	}
	sort.SliceStable(indexed, func(i, j int) bool {
		if indexed[i].rank != indexed[j].rank {
			return indexed[i].rank < indexed[j].rank
		}
		if indexed[i].item.ImpactScore != indexed[j].item.ImpactScore {
			return indexed[i].item.ImpactScore > indexed[j].item.ImpactScore
		}
		return indexed[i].idx < indexed[j].idx
	})

	seenNames := make(map[string]bool, len(indexed))
	var deduped []BacklogItem
	for _, ix := range indexed {
		if !seenNames[ix.item.ChangeName] {
			seenNames[ix.item.ChangeName] = true
			deduped = append(deduped, ix.item)
		}
	}
	d.stateMu.Lock()
	d.lastValidBacklog = append([]BacklogItem(nil), deduped...)
	d.lastInvalidItems = append([]map[string]any(nil), invalid...)
	d.stateMu.Unlock()
	return deduped, invalid, waited
}

func (d *Dispatcher) priorityOf(item BacklogItem, waited *float64) int {
	base, ok := PriorityRanks[item.Priority]
	if !ok {
		base = 2
	}
	every := d.cfg().PriorityBumpEvery
	if every <= 0 || waited == nil {
		return base
	}
	aged := base - int(time.Duration(*waited*float64(time.Hour))/every)
	if aged < 0 {
		aged = 0
	}
	return aged
}

func (d *Dispatcher) computeWaitedHours(items []BacklogItem, record bool) map[string]*float64 {
	now := d.nowFunc()
	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	if record {
		nowStr := now.Format(time.RFC3339)
		for _, it := range items {
			if _, ok := d.seen[it.ChangeName]; !ok {
				d.seen[it.ChangeName] = nowStr
				if d.store != nil && d.store.DB() != nil {
					_ = d.store.SetChangeFirstSeen(context.Background(), it.ChangeName, now)
				}
			}
		}
	}

	out := make(map[string]*float64, len(items))
	for _, it := range items {
		var since time.Time
		for _, candidate := range []string{it.QueuedAt, d.seen[it.ChangeName]} {
			if candidate == "" {
				continue
			}
			if t, err := time.Parse(time.RFC3339, candidate); err == nil {
				since = t
				break
			}
		}
		if !since.IsZero() {
			hrs := now.Sub(since).Hours()
			if hrs < 0 {
				hrs = 0
			}
			out[it.ChangeName] = &hrs
		}
	}
	return out
}

func (d *Dispatcher) reportOnce(ctx context.Context, event string, fields map[string]any) {
	b, _ := json.Marshal(fields)
	key := event + ":" + string(b)
	d.stateMu.Lock()
	if d.reported[key] {
		d.stateMu.Unlock()
		return
	}
	d.reported[key] = true
	d.stateMu.Unlock()

	if d.opts.DryRun {
		return
	}
	_ = d.store.EmitEvent(ctx, store.EventInput{
		Event:   event,
		Payload: fields,
	})
}

func (d *Dispatcher) fetchOriginQuiet(ctx context.Context) {
	d.gitMu.Lock()
	defer d.gitMu.Unlock()
	if r, err := gitx.Open(d.root); err == nil {
		_ = r.Fetch(ctx, d.cfg().Remote, d.cfg().BaseBranch)
	}
}

// mainCheckoutRestorable are the files the merger writes into the main checkout after it publishes
// them: the backlog without the integrated item, the history with it, and the files a rotation of the
// history creates (.foreman-kit/agent_history.<N>.json). Without those, the first rotation left an untracked
// file in the main checkout, which was then never fast-forwarded again.
var mainCheckoutRestorable = []string{".foreman-kit/agent_backlog.json", ".foreman-kit/agent_history.json", ".foreman-kit/agent_history.*.json"}

// fastForwardMainCheckout brings the main checkout up to origin/<base> before the backlog is read
// from it: a backlog item a person pushed from another clone, or a setting, is otherwise invisible
// until someone runs foreman-kit sync. It never moves a checkout with commits or edits of its
// own, nor one the cycle or the visual QA session is using; it says once why it left it behind.
func (d *Dispatcher) fastForwardMainCheckout(ctx context.Context) {
	if d.opts.DryRun || d.store == nil {
		return
	}
	if rel, err := filepath.Rel(d.root, d.opts.BacklogFile); err != nil || strings.HasPrefix(rel, "..") {
		return
	}
	host, _ := os.Hostname()
	nowUnix := d.nowFunc().Unix()
	for _, lockKey := range []string{"cycle", "visual_qa"} {
		if l, err := d.store.GetLock(ctx, lockKey); err == nil && l != nil && !store.IsLockStale(*l, host, nowUnix) {
			return
		}
	}
	d.gitMu.Lock()
	res := gitx.FastForwardCheckout(ctx, d.root, d.cfg().Remote, d.cfg().BaseBranch, mainCheckoutRestorable)
	d.gitMu.Unlock()
	upstream := d.cfg().Remote + "/" + d.cfg().BaseBranch
	if res.Pulled > 0 {
		d.logf("dispatcher: main checkout fast-forwarded %d commit(s) to %s (backlog and settings pushed there)", res.Pulled, upstream)
		_ = d.store.EmitEvent(ctx, store.EventInput{
			Event:   "main.fast_forwarded",
			Payload: map[string]any{"commits": res.Pulled, "upstream": upstream},
		})
	} else if res.Skipped != "" {
		d.stateMu.Lock()
		first := !d.reported["main.behind:"+res.Skipped]
		d.stateMu.Unlock()
		if first {
			d.logf("dispatcher: main checkout is behind %s but was left as it is (%s): the backlog read from it may be stale; foreman-kit sync", upstream, res.Skipped)
		}
		d.reportOnce(ctx, "main.behind", map[string]any{"upstream": upstream, "reason": res.Skipped})
		d.stateMu.Lock()
		d.reported["main.behind:"+res.Skipped] = true
		d.stateMu.Unlock()
	}
}

func (d *Dispatcher) refreshRemote(ctx context.Context, idle bool) {
	now := d.nowFunc()
	interval := 30 * time.Second
	if idle {
		interval = idleFetchInterval
	}
	if d.serving && interval < 5*time.Second {
		// Serve passes on every signal: a burst of them must not become a burst of fetches. The merger
		// fetches the base after each push, so an integration is visible without one.
		interval = 5 * time.Second
	}
	if now.Sub(d.lastFetch) >= interval {
		d.fetchOriginQuiet(ctx)
		d.lastFetch = now
		d.fastForwardMainCheckout(ctx)
	}
	arch := d.computeArchivedOnBase(ctx)
	d.stateMu.Lock()
	d.archived = arch
	d.stateMu.Unlock()
}

func (d *Dispatcher) computeArchivedOnBase(ctx context.Context) map[string]bool {
	baseRef := d.resolveBaseRef(ctx, d.root)
	// The base part walks the whole history: compute it once per base commit, since Serve passes on
	// every signal. The history files are read every time (they change without a new base commit).
	sha := ""
	if shaOut, err := gitx.RunGit(ctx, d.root, "rev-parse", "--verify", "--quiet", baseRef+"^{commit}"); err == nil {
		sha = strings.TrimSpace(shaOut)
	}
	d.stateMu.Lock()
	cached := sha != "" && sha == d.archivedSHA
	onBase := d.archivedGit
	d.stateMu.Unlock()
	if !cached {
		onBase = d.archivedOnBaseRef(ctx, baseRef)
		d.stateMu.Lock()
		d.archivedSHA, d.archivedGit = sha, onBase
		d.stateMu.Unlock()
	}

	out := make(map[string]bool, len(onBase))
	for name := range onBase {
		out[name] = true
	}
	histFile := filepath.Join(d.root, ".foreman-kit", "agent_history.json")
	for _, it := range readBacklogItemsFromFile(histFile) {
		if it.ChangeName != "" {
			out[it.ChangeName] = true
		}
	}
	if rotMatches, err := filepath.Glob(filepath.Join(d.root, ".foreman-kit", "agent_history.*.json")); err == nil {
		for _, m := range rotMatches {
			for _, it := range readBacklogItemsFromFile(m) {
				if it.ChangeName != "" {
					out[it.ChangeName] = true
				}
			}
		}
	}
	return out
}

// archivedOnBaseRef returns the changes archived under openspec/changes/archive/ on baseRef or carried
// by an integration commit in its history.
func (d *Dispatcher) archivedOnBaseRef(ctx context.Context, baseRef string) map[string]bool {
	out := make(map[string]bool)
	if lsOut, err := gitx.RunGit(ctx, d.root, "ls-tree", "--name-only", baseRef, "openspec/changes/archive/"); err == nil {
		datePrefixRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-`)
		for _, line := range strings.Split(lsOut, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			base := filepath.Base(line)
			name := datePrefixRe.ReplaceAllString(base, "")
			if name != "" {
				out[name] = true
			}
		}
	}
	if r, err := gitx.Open(d.root); err == nil {
		if completed, err := r.CompletedSince("", baseRef); err == nil {
			for _, c := range completed {
				if c.ChangeName != "" {
					out[c.ChangeName] = true
				}
			}
		}
	}
	return out
}

func (d *Dispatcher) resolveBaseRef(ctx context.Context, dir string) string {
	remoteRef := d.cfg().Remote + "/" + d.cfg().BaseBranch
	if _, err := gitx.RunGit(ctx, dir, "rev-parse", "--verify", "--quiet", remoteRef); err == nil {
		return remoteRef
	}
	if _, err := gitx.RunGit(ctx, dir, "rev-parse", "--verify", "--quiet", d.cfg().BaseBranch); err == nil {
		return d.cfg().BaseBranch
	}
	return "HEAD"
}

func (d *Dispatcher) isPaused(ctx context.Context) bool {
	ws, err := d.store.GetWorkspaceState(ctx)
	if err == nil && ws.Paused {
		return true
	}
	return fileExists(store.RuntimePath(d.root, "PAUSE"))
}

func (d *Dispatcher) isEnginesDown() bool {
	now := d.nowFunc()
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	return d.enginesDn || d.coolingLocked(now)
}

// coolingLocked reports whether the engine cooldown of some role is still in force at now. The caller
// holds stateMu.
func (d *Dispatcher) coolingLocked(now time.Time) bool {
	for _, until := range d.cooldownUntil {
		if now.Before(until) {
			return true
		}
	}
	return false
}

// endCooldownLocked ends role's engine cooldown and starts its backoff over. The caller holds stateMu.
func (d *Dispatcher) endCooldownLocked(role string) {
	delete(d.cooldownUntil, role)
	delete(d.cooldownLength, role)
	delete(d.nextBackoff, role)
}

func (d *Dispatcher) isBlocked(ctx context.Context, change string) bool {
	q, err := d.store.GetQuarantine(ctx, change)
	return err == nil && q != nil && q.Status == "blocked"
}

func (d *Dispatcher) isInReadyQueue(ctx context.Context, change string) bool {
	items, err := d.store.ListReadyQueue(ctx)
	if err != nil {
		return false
	}
	for _, it := range items {
		if it.ChangeName == change {
			return true
		}
	}
	return false
}

func (d *Dispatcher) retryBlocked(ctx context.Context, backlog []BacklogItem) {
	if d.cfg().BlockedRetries <= 0 {
		return
	}
	now := d.nowFunc()
	for _, item := range backlog {
		change := item.ChangeName
		d.stateMu.Lock()
		_, running := d.active[change]
		isArch := d.archived[change]
		d.stateMu.Unlock()
		if running || isArch {
			continue
		}

		q, err := d.store.GetQuarantine(ctx, change)
		if err != nil || q == nil || q.Status != "blocked" {
			continue
		}
		if !IsRetryableReason(q.Reason) || q.Retries >= d.cfg().BlockedRetries {
			continue
		}

		blockedAt, err := time.Parse(time.RFC3339, q.BlockedAt)
		if err != nil {
			if t, err2 := time.Parse(time.RFC3339, q.UpdatedAt); err2 == nil {
				blockedAt = t
			} else {
				blockedAt = time.Unix(0, 0).UTC()
			}
		}
		dueAt := blockedAt.Add(d.cfg().BlockedRetryAfter)
		if now.Before(dueAt) {
			continue
		}

		// The rebuilds the merger asked for keep counting across the retry, unless what is retried is
		// the block for having run out of them (rebase_conflict): the store derives both.
		updated, err := d.store.AdvanceQuarantineRetry(ctx, change)
		if err != nil || updated == nil {
			continue
		}
		d.stateMu.Lock()
		delete(d.results, change)
		d.stateMu.Unlock()

		_ = d.store.EmitEvent(ctx, store.EventInput{
			Event:  "change.retry",
			Change: change,
			Payload: map[string]any{
				"reason": q.Reason,
				"retry":  updated.Retries,
				"limit":  d.cfg().BlockedRetries,
			},
		})
	}
}

func (d *Dispatcher) requeueRequestedRebuilds(ctx context.Context) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	for chg, res := range d.results {
		switch res {
		case "ready":
			q, err := d.store.GetQuarantine(ctx, chg)
			if err == nil && q != nil && q.Status == "rebuild_requested" {
				delete(d.results, chg)
			}
		case "blocked":
			if !d.isBlocked(ctx, chg) {
				delete(d.results, chg)
			}
		}
	}
}

func (d *Dispatcher) isEligible(ctx context.Context, item BacklogItem) bool {
	change := item.ChangeName
	if d.isBlocked(ctx, change) || d.isInReadyQueue(ctx, change) {
		return false
	}
	d.stateMu.Lock()
	isArch := d.archived[change]
	for _, dep := range item.DependsOn {
		if !d.archived[dep] {
			d.stateMu.Unlock()
			return false
		}
	}
	heldCaps := d.held[change]
	d.stateMu.Unlock()

	if isArch {
		return false
	}
	if len(heldCaps) == 0 {
		return true
	}
	inFlight := d.capabilitiesInFlight(ctx, change)
	for _, hc := range heldCaps {
		if inFlight[hc] {
			return false
		}
	}
	return true
}

func (d *Dispatcher) waitingOnFlight(ctx context.Context, item BacklogItem, byName map[string]BacklogItem, seen map[string]bool) bool {
	change := item.ChangeName
	d.stateMu.Lock()
	res := d.results[change]
	isArch := d.archived[change]
	d.stateMu.Unlock()

	if seen[change] || d.isBlocked(ctx, change) || res == "blocked" || isArch {
		return false
	}
	nextSeen := make(map[string]bool, len(seen)+1)
	for k, v := range seen {
		nextSeen[k] = v
	}
	nextSeen[change] = true

	for _, dep := range item.DependsOn {
		d.stateMu.Lock()
		depArch := d.archived[dep]
		d.stateMu.Unlock()
		if depArch || d.isInReadyQueue(ctx, dep) {
			continue
		}
		depItem, ok := byName[dep]
		if !ok || !d.waitingOnFlight(ctx, depItem, byName, nextSeen) {
			return false
		}
	}
	return true
}

func (d *Dispatcher) capabilitiesInFlight(ctx context.Context, except string) map[string]bool {
	out := make(map[string]bool)
	readyList, _ := d.store.ListReadyQueue(ctx)
	for _, m := range readyList {
		for _, c := range m.Capabilities {
			out[c] = true
		}
	}
	d.stateMu.Lock()
	for chg, caps := range d.building {
		if chg == except {
			continue
		}
		for _, c := range caps {
			out[c] = true
		}
	}
	d.stateMu.Unlock()
	return out
}

// claimCapabilities checks caps against every capability in flight and records the claim in one
// stateMu critical section, so two workers can never both claim the same capability.
func (d *Dispatcher) claimCapabilities(ctx context.Context, change string, caps []string) bool {
	d.stateMu.Lock()
	// The ready queue is read under stateMu on purpose: recordWorkerOutcome drops a finished
	// worker's capabilities from d.building (under this lock) only after that worker enqueued them,
	// so every capability in flight is visible either in d.building or in the ready queue.
	inFlight := make(map[string]bool)
	if readyList, err := d.store.ListReadyQueue(ctx); err == nil {
		for _, m := range readyList {
			for _, c := range m.Capabilities {
				inFlight[c] = true
			}
		}
	}
	for chg, held := range d.building {
		if chg == change {
			continue
		}
		for _, c := range held {
			inFlight[c] = true
		}
	}
	var conflicts []string
	for _, c := range caps {
		if inFlight[c] {
			conflicts = append(conflicts, c)
		}
	}
	if len(conflicts) > 0 {
		d.held[change] = caps
		d.stateMu.Unlock()
		_ = d.store.EmitEvent(ctx, store.EventInput{
			Event:  "change.held",
			Change: change,
			Payload: map[string]any{
				"conflicts": strings.Join(conflicts, ","),
			},
		})
		return false
	}
	delete(d.held, change)
	d.building[change] = caps
	d.stateMu.Unlock()
	return true
}

func (d *Dispatcher) claimedCapabilities(change string, item BacklogItem) []string {
	wt := filepath.Join(d.worktreeBase(), "worker-"+change)
	return d.capabilitiesWithLocks(wt, change, item.Locks)
}

func (d *Dispatcher) capabilities(wt, change string) []string {
	specsDir := filepath.Join(wt, "openspec", "changes", change, "specs")
	files := listSpecFiles(specsDir)
	var caps []string
	for _, f := range files {
		rel, err := filepath.Rel(specsDir, filepath.Dir(f))
		if err == nil {
			caps = append(caps, filepath.ToSlash(rel))
		}
	}
	sort.Strings(caps)
	return caps
}

func (d *Dispatcher) capabilitiesWithLocks(wt, change string, locks []string) []string {
	caps := d.capabilities(wt, change)
	for _, l := range locks {
		if l != "" {
			caps = append(caps, "lock:"+l)
		}
	}
	return caps
}

func (d *Dispatcher) processChange(ctx context.Context, item BacklogItem, slot int) (result string) {
	change := item.ChangeName
	leaseKey := store.WorkerLeaseKey(change)
	leaseTTL := d.cfg().LocksStaleAfter
	pid := os.Getpid()
	_, err := d.store.AcquireLock(ctx, leaseKey, store.LockOptions{
		OwnerType: "worker",
		PID:       pid,
		Slot:      slot,
		TTL:       leaseTTL,
	})
	if err != nil {
		return "busy"
	}

	// The lease is held: this pass over the change is a run, a row of change_runs that everything the
	// worker writes from here on points at through the context (its executions, gate rounds, repair
	// rounds, evaluator answers, the block that ends it, its manifest, its events). It closes with the
	// result of the pass.
	wt := filepath.Join(d.worktreeBase(), "worker-"+change)
	branch := "feat/spec-" + change
	runID, _ := d.store.BeginChangeRun(ctx, change, slot, wt, branch)
	ctx = store.WithChangeRun(ctx, runID, change)
	defer func() {
		_ = d.store.FinishChangeRun(context.WithoutCancel(ctx), runID, result)
	}()

	// The change's work runs under workCtx. store.IsLockStale lets another dispatcher take a lease whose
	// heartbeat is older than its TTL even while its owner lives (a laptop asleep for longer than
	// locks.stale_after, a suspended VM): the heartbeat then finds the row is no longer this
	// process's and ends workCtx, which stops the agent or the gates in flight, and ownsLease tells the
	// worker to record nothing, since the new owner builds the change now.
	workCtx, stopWork := context.WithCancel(ctx)
	var lost atomic.Bool
	hbDone := make(chan struct{})
	defer func() {
		stopWork()
		<-hbDone
		// Deletes only this process's row: a lease another dispatcher took stays as it is.
		_ = d.store.ReleaseLock(context.Background(), leaseKey, pid)
	}()
	go func() {
		defer close(hbDone)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if errors.Is(d.store.HeartbeatLock(workCtx, leaseKey, pid), store.ErrLockHeld) {
					lost.Store(true)
					stopWork()
					return
				}
			}
		}
	}()
	// ownsLease refreshes the lease and reports whether this worker still holds it; the worker asks
	// before it records any outcome for the change.
	ownsLease := func() bool {
		if !lost.Load() && !errors.Is(d.store.HeartbeatLock(context.WithoutCancel(ctx), leaseKey, pid), store.ErrLockHeld) {
			return true
		}
		lost.Store(true)
		stopWork()
		d.logf("dispatcher: %s: another dispatcher took this worker's lease (its heartbeat had expired); the worker stops and records nothing", change)
		return false
	}

	fail := func(err error) string {
		if !ownsLease() {
			return "lost"
		}
		if ctx.Err() != nil {
			// The dispatcher is stopping (a daemon stop, or another process took its lock): the agent,
			// the gates and git failed because they were cancelled, which says nothing about the change.
			// It is neither blocked nor deferred, and the next dispatcher resumes it where it stopped.
			d.logf("dispatcher: %s: stopped with the dispatcher; nothing is recorded against it", change)
			return "stopped"
		}
		return d.handleChangeError(ctx, change, err, wt, branch)
	}
	// A panic from here on is this change's failure, not the daemon's (workerPanicked). It is handled
	// before the deferred release above runs, so fail still finds the lease held.
	defer func() {
		if r := recover(); r != nil {
			result = d.workerPanicked(change, r, fail)
		}
	}()
	wasRebuildReq, rebuildDetails := d.pendingRebuildRequest(workCtx, change)
	_ = d.store.EmitEvent(workCtx, store.EventInput{
		Event:  "change.start",
		Change: change,
		Payload: map[string]any{
			"worktree": wt,
			"slot":     slot,
		},
	})
	if wasRebuildReq {
		d.logf("dispatcher: %s: started on slot %d (a rebuild on the current base)", change, slot)
	} else {
		d.logf("dispatcher: %s: started on slot %d", change, slot)
	}

	if err := d.provisionWorktree(workCtx, wt, change, branch); err != nil {
		return fail(err)
	}
	if err := d.keepLeftovers(workCtx, wt, change, branch); err != nil {
		return fail(err)
	}

	retried := d.pendingRetry(workCtx, change)
	if retried != nil {
		d.recordRetry(wt, change, retried)
	}

	if err := d.planPhase(workCtx, item, wt, slot, &retried); err != nil {
		return fail(err)
	}

	if !ownsLease() {
		return "lost"
	}
	caps := d.capabilitiesWithLocks(wt, change, item.Locks)
	if !d.claimCapabilities(workCtx, change, caps) {
		return "held"
	}

	rebuildFeedback, err := d.refreshPhase(workCtx, item, wt, branch, slot, wasRebuildReq, &retried)
	if err != nil {
		return fail(err)
	}
	if wasRebuildReq && judgedSendBack(rebuildDetails) {
		// The merger sent it back on a judgment of the evaluator (lost or contradicted concurrent work), not for a
		// conflict: the rebase is clean, so the BUILD agent must be given the feedback explicitly.
		if rebuildFeedback == "" {
			if err := d.reopenTasks(workCtx, wt, change); err != nil {
				return fail(err)
			}
			rebuildFeedback = rebuildDetails
		} else {
			rebuildFeedback += "\n\n" + rebuildDetails
		}
	}

	if err := d.buildPhase(workCtx, item, wt, branch, slot, rebuildFeedback, &retried); err != nil {
		return fail(err)
	}

	assessment, err := d.assessRisk(workCtx, item, wt, change, caps)
	if err != nil {
		return fail(err)
	}

	if !ownsLease() {
		return "lost"
	}
	if wasRebuildReq {
		// The rebuild the merger asked for is built: its request is consumed only now, before the change
		// is queued again (a request pending on a queued change means the merger sent it back once more).
		d.takeRebuildRequest(ctx, change)
	}
	if err := d.enqueueReady(ctx, change, wt, branch, caps, assessment, item); err != nil {
		return fail(err)
	}
	return "ready"
}

// workerPanicLockWait is how long workerPanicked waits for stateMu before it concludes that the panic
// left it locked. It is a variable so tests can shorten it.
var workerPanicLockWait = 30 * time.Second

// workerPanicked contains a panic raised under change's worker. Uncontained, it ends the whole process:
// the resident daemon with its portal and merger, and every other worker's agent keeps running with
// nobody left to stop it. The panic is logged with its stack and then handled by fail like any other
// orchestrator error of the worker: the change is blocked and the system-error handling decides whether
// the workspace pauses. The worker then returns as usual, so its lease is released, its outcome reaches
// the scheduling loop, and its slot and capabilities are freed.
//
// One case cannot be contained. A panic raised between stateMu.Lock and an Unlock that is not deferred
// leaves stateMu locked for good, and this path and the scheduling loop both need it: the dispatcher
// would hang without a word. The panic is then raised again, which ends the process as it did before.
func (d *Dispatcher) workerPanicked(change string, r any, fail func(error) string) string {
	stack := debug.Stack()
	d.logf("dispatcher: %s: panic in its worker: %v\n%s", change, r, stack)
	deadline := time.Now().Add(workerPanicLockWait)
	for !d.stateMu.TryLock() {
		if !time.Now().Before(deadline) {
			d.logf("dispatcher: %s: the panic left the dispatcher's state locked: it cannot be contained", change)
			panic(r)
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.stateMu.Unlock()
	return fail(fmt.Errorf("panic in the worker of %s: %v\n%s", change, r, stack))
}

func (d *Dispatcher) handleChangeError(ctx context.Context, change string, err error, wt, branch string) string {
	var engDown *EnginesDownError
	deferralLimit := false
	if errors.As(err, &engDown) {
		if d.recordDeferral(ctx, change, engDown.Role, engDown.Output) {
			return "deferred"
		}
		deferralLimit = true
		attempts := "dispatches"
		if d.serving {
			attempts = "engine cooldowns"
		}
		err = NewBlockedError("engine_unavailable", fmt.Sprintf(
			"Every agent of the role failed without changing anything in %d %s in a row:\n%s",
			d.cfg().OutageGiveUpAfter, attempts, engDown.Output,
		))
	}

	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		blocked = NewBlockedError("orchestrator_error", err.Error())
	}

	// system_error_triage: the evaluator decides whether the error is this change's alone (block only it), passes
	// on its own (retry it later), or breaks every change (pause, as without the evaluator).
	contained := false
	if blocked.Reason == "orchestrator_error" {
		action, v := triageSystemError(ctx, d.judge, d.store, change, "dispatch", blocked.Details, d.nowFunc())
		switch action {
		case "block_change":
			contained = true
			blocked = NewBlockedError("orchestrator_error", blocked.Details+"\n\n"+answerSummary(v))
			d.logf("dispatcher: %s: the error is this change's alone (evaluator): blocking it without pausing the workspace", change)
		case "retry_later":
			blocked = NewBlockedError("transient_error", blocked.Details+"\n\n"+answerSummary(v))
			d.logf("dispatcher: %s: the error looks transient (evaluator): it is retried later", change)
		}
	}

	// A continuous dispatcher never pauses the workspace over an engine outage: it blocks only the change
	// that hit the deferral limit and keeps backing off for the others.
	if IsSystemReason(blocked.Reason) && !contained && !(deferralLimit && d.serving) {
		firstLine := strings.TrimSpace(strings.SplitN(blocked.Details, "\n", 2)[0])
		reasonMsg := fmt.Sprintf("%s (%s): %s", blocked.Reason, change, firstLine)
		if !d.pauseFirst(ctx, reasonMsg) {
			return "paused"
		}
	}

	qPrev, _ := d.store.GetQuarantine(ctx, change)
	retries := 0
	rebuilds := 0
	approved := false
	if qPrev != nil {
		retries = qPrev.Retries
		rebuilds = qPrev.Rebuilds
		approved = qPrev.ApprovedRisk
	}
	_ = d.store.BlockChange(ctx, store.QuarantineRecord{
		ChangeName:   change,
		Reason:       blocked.Reason,
		Details:      blocked.Details,
		Worktree:     wt,
		Branch:       branch,
		Retries:      retries,
		Rebuilds:     rebuilds,
		ApprovedRisk: approved,
		BlockedAt:    d.nowFunc().Format(time.RFC3339),
	})
	_ = d.store.EmitEvent(ctx, store.EventInput{
		Event:  "change.blocked",
		Change: change,
		Payload: map[string]any{
			"reason": blocked.Reason,
		},
	})
	d.logf("dispatcher: %s: blocked (%s): foreman-kit status", change, blocked.Reason)
	return "blocked"
}

func (d *Dispatcher) pauseFirst(ctx context.Context, reason string) bool {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	ws, err := d.store.GetWorkspaceState(ctx)
	if err == nil && ws.Paused {
		return false
	}
	// The pause and its system.paused event are one write.
	_, _ = d.store.Pause(ctx, reason, "dispatcher", store.ScopeFrom(ctx).Change)
	return true
}

// recordDeferral counts an engine outage (exit 75) against change and reports whether the change is
// deferred (true) or waited through outage.give_up_after of them and must be blocked (false). With
// that key at never the change waits as long as it takes.
//
// Run stops dispatching for the rest of its run, so each run counts one deferral. Serve keeps running:
// it starts a cooldown (EngineBackoff, doubling up to outage.max_wait) and counts a deferral only when
// the change's previous counted one is at least that cooldown old, so the limit means as many outages
// spread over growing windows (about half an hour with the values the kit ships), never a few retries
// in a minute.
func (d *Dispatcher) recordDeferral(ctx context.Context, change, role, output string) bool {
	now := d.nowFunc()
	limit, backoffMax := d.cfg().OutageGiveUpAfter, d.cfg().backoffMax
	reached := func(n int) bool { return limit > 0 && n >= limit }
	d.stateMu.Lock()
	counted := true
	var retryAt time.Time
	if d.serving {
		window := d.cooldownLength[role]
		if until := d.cooldownUntil[role]; now.Before(until) {
			// A worker that was already running hit the outage that opened the role's window: the same
			// outage, so neither the window nor the next one grows.
			retryAt = until
		} else {
			window = d.nextBackoff[role]
			if window <= 0 {
				window = d.opts.EngineBackoff
			}
			retryAt = now.Add(window)
			d.cooldownUntil[role] = retryAt
			d.cooldownLength[role] = window
			next := window * 2
			if next > backoffMax {
				next = backoffMax
			}
			d.nextBackoff[role] = next
		}
		if last, ok := d.lastDeferral[change]; ok && now.Sub(last) < window {
			counted = false
		}
	} else {
		d.enginesDn = true
	}
	n := d.deferrals[change]
	if counted {
		n++
		d.lastDeferral[change] = now
		if reached(n) {
			delete(d.deferrals, change)
			delete(d.lastDeferral, change)
		} else {
			d.deferrals[change] = n
		}
	}
	d.stateMu.Unlock()

	// Every deferral is on record, counted or not; reaching the limit closes them, as the change is
	// blocked for it.
	if d.store != nil && d.store.DB() != nil {
		_ = d.store.RecordDeferral(ctx, change, role, counted, now, retryAt, tailLines(output, 3))
		if counted && reached(n) {
			_ = d.store.ClearDeferrals(ctx, change)
		}
	}

	if reached(n) {
		return false
	}
	payload := map[string]any{
		"deferrals": n,
		"limit":     limit,
		"output":    tailLines(output, 3),
	}
	if d.serving {
		payload["counted"] = counted
		payload["retry_at"] = retryAt.Format(time.RFC3339)
		d.logf("dispatcher: %s deferred: every engine is out; no worker starts before %s (deferrals %d/%s)", change, retryAt.Format(time.RFC3339), n, deferralLimitText(limit))
	}
	_ = d.store.EmitEvent(ctx, store.EventInput{
		Event:   "change.deferred",
		Change:  change,
		Payload: payload,
	})
	return true
}

// clearDeferrals forgets change's deferrals after an agent call of role that did not find every engine
// out, which also ends that role's cooldown: an engine of the role works again. The other role's
// cooldown stands: this call says nothing about its engines.
func (d *Dispatcher) clearDeferrals(change, role string) {
	d.stateMu.Lock()
	delete(d.deferrals, change)
	delete(d.lastDeferral, change)
	d.endCooldownLocked(role)
	if d.store != nil && d.store.DB() != nil {
		_ = d.store.ClearDeferrals(context.Background(), change)
	}
	d.stateMu.Unlock()
}

// pendingRebuildRequest reports whether the merger sent change back for a rebuild, with its details, and
// leaves the request pending. A worker consumes it (takeRebuildRequest) only when it queues the rebuilt
// change: an attempt that ends before that (held, deferred, paused, stopped, its lease lost, the process
// killed) leaves it to the next attempt, which is then still a rebuild, counted once against
// merge.conflict_rebuilds, and whose BUILD agent still gets the merger's feedback.
func (d *Dispatcher) pendingRebuildRequest(ctx context.Context, change string) (bool, string) {
	q, err := d.store.GetQuarantine(ctx, change)
	if err != nil || q == nil || q.Status != "rebuild_requested" {
		return false, ""
	}
	return true, q.ConflictDetails
}

// takeRebuildRequest consumes a pending rebuild request and returns the merger's details with it.
func (d *Dispatcher) takeRebuildRequest(ctx context.Context, change string) (bool, string) {
	q, err := d.store.GetQuarantine(ctx, change)
	if err != nil || q == nil || q.Status != "rebuild_requested" {
		return false, ""
	}
	_ = d.store.ClearQuarantine(ctx, change, false)
	return true, q.ConflictDetails
}

func (d *Dispatcher) pendingRetry(ctx context.Context, change string) map[string]any {
	q, err := d.store.GetQuarantine(ctx, change)
	if err != nil || q == nil || q.Retries <= 0 || q.RetryTold || q.LastRetryJSON == "" {
		return nil
	}
	var last store.QuarantineRecord
	if err := json.Unmarshal([]byte(q.LastRetryJSON), &last); err != nil {
		return nil
	}
	return map[string]any{
		"reason":     last.Reason,
		"details":    last.Details,
		"blocked_at": last.BlockedAt,
		"retry":      q.Retries,
		"limit":      d.cfg().BlockedRetries,
	}
}

func (d *Dispatcher) markRetryTold(ctx context.Context, change string) {
	_ = d.store.MarkRetryTold(ctx, change)
}

func (d *Dispatcher) provisionWorktree(ctx context.Context, wt, change, branch string) error {
	d.gitMu.Lock()
	defer d.gitMu.Unlock()

	if dirExists(wt) && !gitx.IsWorktreeRoot(ctx, wt) {
		// A worker worktree may hold work: repair a moved umbrella's stale gitfile, but never delete it.
		_, _ = gitx.RunGit(ctx, d.root, "worktree", "repair", wt)
		if !gitx.IsWorktreeRoot(ctx, wt) {
			return NewBlockedError("orchestrator_error", fmt.Sprintf(
				"%s exists but is not a git worktree of %s (moved umbrella or broken .git); run `git worktree repair %s` or remove it",
				wt, d.root, wt))
		}
	}

	if !dirExists(wt) {
		if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
			return err
		}
		// Forget a registration left for wt, whose directory is gone, and only that one (git finds it
		// by its path, so the parent directory is created first). "git worktree prune" deletes the
		// administrative directory, and the child commits it holds, of every worktree that cannot be
		// reached right now: all of them after the umbrella was moved or mounted at another path, or a
		// person's own on a volume that is not mounted.
		_, _ = gitx.RunGit(ctx, d.root, "worktree", "remove", "--force", wt)
		_, branchErr := gitx.RunGit(ctx, d.root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
		exists := branchErr == nil
		if exists {
			if _, err := gitx.RunGit(ctx, d.root, "worktree", "add", wt, branch); err != nil {
				return err
			}
			if err := d.abortLostBuild(ctx, wt, change, branch); err != nil {
				return err
			}
		} else {
			baseRef := d.resolveBaseRef(ctx, d.root)
			if _, err := gitx.RunGit(ctx, d.root, "worktree", "add", "-b", branch, wt, baseRef); err != nil {
				return err
			}
		}
	}

	wtRepo, err := gitx.Open(wt)
	if err != nil {
		return err
	}
	children, _ := wtRepo.ChildPaths()
	// A rebase killed by a shutdown leaves HEAD detached while feat/spec-<change> still points at the
	// pre-rebase tip; resuming on top of it would commit BUILD work that the merger later discards.
	gitx.AbortInterruptedRebase(ctx, wt)
	for _, cp := range children {
		childDir := filepath.Join(wt, filepath.FromSlash(cp))
		if _, err := os.Stat(filepath.Join(childDir, ".git")); err == nil {
			gitx.AbortInterruptedRebase(ctx, childDir)
		}
	}
	needsSubmoduleInit := false
	for _, cp := range children {
		if _, err := os.Stat(filepath.Join(wt, filepath.FromSlash(cp), ".git")); err != nil {
			needsSubmoduleInit = true
			break
		}
	}
	if needsSubmoduleInit {
		if err := d.populateSubmodules(ctx, wt, children); err != nil {
			return err
		}
	}
	d.linkLocalChildConfigs(wt, children)
	return nil
}

// populateSubmodules initializes any unpopulated child submodules in wt, using local --reference when available,
// and links local git-ignored config files.
func (d *Dispatcher) populateSubmodules(ctx context.Context, wt string, children []string) error {
	for _, cp := range children {
		if _, err := os.Stat(filepath.Join(wt, filepath.FromSlash(cp), ".git")); err == nil {
			continue
		}
		refDir := filepath.Join(d.mainRoot, filepath.FromSlash(cp))
		if _, err := os.Stat(filepath.Join(refDir, ".git")); err == nil {
			if _, err := gitx.RunGit(ctx, wt, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--reference", refDir, "--", cp); err == nil {
				continue
			}
		}
		if _, err := gitx.RunGit(ctx, wt, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive", "--", cp); err != nil {
			return err
		}
	}
	return nil
}

// linkLocalChildConfigs links local git-ignored config files (such as config/application.yml) from mainRoot into wt.
// localChildConfigs are a child repository's git-ignored local config files: workers get them linked
// from the main checkout, and a check of a base alone never cleans them away (the integration
// worktree's copies may have been placed by hand).
var localChildConfigs = []string{"config/application.yml", ".env.local"}

func (d *Dispatcher) linkLocalChildConfigs(wt string, children []string) {
	if d.mainRoot == "" || d.mainRoot == wt {
		return
	}
	for _, cp := range children {
		for _, rel := range localChildConfigs {
			src := filepath.Join(d.mainRoot, filepath.FromSlash(cp), filepath.FromSlash(rel))
			dst := filepath.Join(wt, filepath.FromSlash(cp), filepath.FromSlash(rel))
			if fileExists(src) && !fileExists(dst) {
				_ = os.MkdirAll(filepath.Dir(dst), 0o755)
				if err := os.Symlink(src, dst); err != nil {
					if b, err := os.ReadFile(src); err == nil {
						_ = os.WriteFile(dst, b, 0o600)
					}
				}
			}
		}
	}
}

func (d *Dispatcher) abortLostBuild(ctx context.Context, wt, change, branch string) error {
	wtRepo, err := gitx.Open(wt)
	if err != nil {
		return err
	}
	children, _ := wtRepo.ChildPaths()
	baseRef := d.resolveBaseRef(ctx, wt)
	fork, err := wtRepo.MergeBase("HEAD", baseRef)
	if err != nil {
		fork = baseRef
	}
	moved := childGitlinksMoved(ctx, wt, fork, children)
	if !moved && !d.tasksComplete(ctx, wt, change) {
		return nil
	}
	_, _ = gitx.RunGit(ctx, d.root, "worktree", "remove", "--force", wt)
	return NewBlockedError("missing_child_commits", fmt.Sprintf(
		"The worktree of %s was deleted after BUILD, so its child commits are lost. Abandon the change (git branch -D %s) and rebuild it from scratch.",
		change, branch,
	))
}

func (d *Dispatcher) keepLeftovers(ctx context.Context, wt, change, branch string) error {
	message := fmt.Sprintf("chore(%s): keep edits found uncommitted when the change resumed", change)
	var kept []string
	folder := "openspec/changes/" + change
	if dirExists(filepath.Join(wt, filepath.FromSlash(folder))) {
		if out, err := gitx.RunGit(ctx, wt, "status", "--porcelain", "--", folder); err == nil && strings.TrimSpace(out) != "" {
			if err := d.commitUmbrella(ctx, wt, message, []string{folder}); err != nil {
				return err
			}
			kept = append(kept, folder)
		}
	}
	if wtRepo, err := gitx.Open(wt); err == nil {
		children, _ := wtRepo.ChildPaths()
		for _, cp := range children {
			childDir := filepath.Join(wt, filepath.FromSlash(cp))
			stOut, _ := gitx.RunGit(ctx, childDir, "status", "--porcelain")
			if strings.TrimSpace(stOut) == "" {
				continue
			}
			curBranch, _ := gitx.RunGit(ctx, childDir, "branch", "--show-current")
			if strings.TrimSpace(curBranch) != branch {
				continue
			}
			if err := d.commitChild(ctx, childDir, cp, message); err != nil {
				return err
			}
			kept = append(kept, cp)
		}
	}
	if len(kept) > 0 {
		_ = d.store.EmitEvent(ctx, store.EventInput{
			Event:  "change.kept_edits",
			Change: change,
			Payload: map[string]any{
				"paths": strings.Join(kept, ","),
			},
		})
	}
	return nil
}

func (d *Dispatcher) planPhase(ctx context.Context, item BacklogItem, wt string, slot int, retriedPtr *map[string]any) error {
	change := item.ChangeName
	chgDir := filepath.Join(wt, "openspec", "changes", change)
	if !dirExists(chgDir) {
		args := []string{"new", "change", change, "--json"}
		if desc := sanitizeBacklogText(item.Description); desc != "" {
			args = append(args, "--description", desc)
		}
		res, _ := d.opts.OpenSpecRunner(ctx, wt, args...)
		if !res.OK {
			return NewBlockedError("planning_failed", res.Output)
		}
	}

	attempts := 0
	var round *repairRound
	for {
		failure, err := d.checkPlanFailure(ctx, wt, change)
		if err != nil {
			return err
		}
		// A context that ended meanwhile (the dispatcher is stopping, or the worker lost its lease)
		// makes the checks fail on their own: such a verdict is never recorded as a repair round.
		if err := ctx.Err(); err != nil {
			return err
		}
		if round != nil {
			d.recordRepair(ctx, wt, change, *round, failure)
		}
		if failure == nil {
			return d.commitUmbrella(ctx, wt, "docs(openspec): plan "+change, []string{"openspec/changes/" + change})
		}
		if attempts > d.cfg().repairRounds {
			return NewBlockedError(failure.Reason, failure.Details)
		}

		fresh := !fileExists(filepath.Join(chgDir, "proposal.md"))
		phase := "plan"
		var fb string
		if !fresh {
			phase = fmt.Sprintf("plan-repair-%d", attempts)
			fb = failure.Details
		}
		prompt, promptVer := d.buildPlanPrompt(item, wt, fb, *retriedPtr)
		usedEngine, err := d.invokeAgent(ctx, change, phase, "plan", d.cfg().enginePlan, wt, prompt, promptVer, slot, retriedPtr)
		if err != nil {
			return err
		}
		if fresh {
			round = nil
		} else {
			round = &repairRound{
				Phase:  phase,
				Engine: usedEngine,
				ExecID: d.lastAgentExecution(change),
				Asked:  *failure,
			}
		}
		attempts++
	}
}

func (d *Dispatcher) checkPlanFailure(ctx context.Context, wt, change string) (*failureInfo, error) {
	val, _ := d.opts.OpenSpecRunner(ctx, wt, "validate", change, "--strict", "--json")
	if !val.OK {
		details := "openspec validate failed:\n" + ExtractOpenSpecInvalidItems(val.Output)
		if ok, msg := ValidateOpenSpecNative(wt, change); !ok && !strings.Contains(details, msg) {
			details += "\n" + msg
		}
		return &failureInfo{
			Reason:  "spec_invalid",
			Details: details,
		}, nil
	}

	stRes, _ := d.opts.OpenSpecRunner(ctx, wt, "status", "--change", change, "--json")
	var stData map[string]any
	if !stRes.OK || parseOpenSpecJSON(stRes.Output, &stData) != nil {
		return &failureInfo{
			Reason:  "spec_invalid",
			Details: "openspec status unreadable:\n" + tailLines(stRes.Output, 80),
		}, nil
	}
	if artifacts, ok := stData["artifacts"].([]any); ok {
		var pending []string
		for _, rawArt := range artifacts {
			if art, ok := rawArt.(map[string]any); ok {
				id := fmt.Sprint(art["id"])
				status := fmt.Sprint(art["status"])
				if status != "done" && status != "skipped" {
					pending = append(pending, id+"="+status)
				}
			}
		}
		if len(pending) > 0 {
			return &failureInfo{
				Reason:  "spec_invalid",
				Details: "Artifacts not complete: " + strings.Join(pending, ", "),
			}, nil
		}
	}

	if ok, msg := ValidateOpenSpecNative(wt, change); !ok {
		return &failureInfo{
			Reason: "spec_invalid",
			Details: "Once archived, this change would leave canonical specs that fail `openspec validate --specs --strict`. " +
				"Fix it in the delta specs only, never in openspec/specs/: the delta of a new capability must start with " +
				"a `## Purpose` section of at least 50 characters, which becomes the canonical Purpose.\n" + msg,
		}, nil
	}

	if preview := d.archivePreview(ctx, wt, change); preview != "" {
		return &failureInfo{
			Reason:  "spec_invalid",
			Details: preview,
		}, nil
	}

	// Use Case 3 (internal/llm): Optional pre-BUILD semantic audit via zendev-sh/goai GenerateObject[T] (advisory/diagnostic only)
	if d.llmClient != nil && d.llmClient.Available() {
		chgDir := filepath.Join(wt, "openspec", "changes", change)
		if audit, err := d.llmClient.AuditPlanSemantics(
			ctx, change,
			readFileOrEmpty(filepath.Join(chgDir, "proposal.md")),
			readFileOrEmpty(filepath.Join(chgDir, "design.md")),
			readFileOrEmpty(filepath.Join(chgDir, "tasks.md")),
			d.deltaSpecs(wt, change),
			ProtectedChildPaths(""),
		); err == nil && audit != nil && !audit.Valid {
			_ = d.store.EmitEvent(ctx, store.EventInput{
				Event:  "llm.plan_audit",
				Change: change,
				Payload: map[string]any{
					"valid":   false,
					"details": audit.FormatMarkdown(),
				},
			})
		}
	}

	if d.judge != nil {
		pState := d.planState(wt, change)
		verdict, err := d.judge.Ask(ctx, "plan_review", pState, change)
		if err != nil {
			if strings.HasPrefix(err.Error(), "judge_unavailable:") {
				return nil, NewBlockedError("judge_unavailable", strings.TrimPrefix(err.Error(), "judge_unavailable: "))
			}
			return nil, err
		}
		if verdict != nil && verdict.Blocking() {
			fb := evaluator.Feedback(d.root, d.judge.Policy(), *verdict)
			// Use Case 2 (internal/llm): Translate numeric rejection of the evaluator into actionable item breakdown
			if d.llmClient != nil && d.llmClient.Available() {
				if bd, err := d.llmClient.ExplainRejection(ctx, change, "plan_review", verdict.Failed, pState); err == nil && bd != nil {
					fb = fb + "\n\n" + bd.FormatMarkdown()
				}
			}
			return &failureInfo{
				Reason:  "plan_review",
				Details: fb,
			}, nil
		}
	}
	return nil, nil
}

func (d *Dispatcher) archivePreview(ctx context.Context, wt, change string) string {
	tmp, err := os.MkdirTemp("", "openspec-preview-*")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(tmp)

	src := filepath.Join(wt, "openspec")
	dst := filepath.Join(tmp, "openspec")
	if err := os.MkdirAll(filepath.Join(dst, "changes"), 0o755); err != nil {
		return ""
	}
	if err := os.MkdirAll(filepath.Join(dst, "specs"), 0o755); err != nil {
		return ""
	}
	// Selective archivePreview — copy only config.yaml and the capabilities touched by this change
	if fileExists(filepath.Join(src, "config.yaml")) {
		_ = copyPathRecursive(filepath.Join(src, "config.yaml"), filepath.Join(dst, "config.yaml"))
	}
	for _, capName := range d.capabilities(wt, change) {
		canonCapDir := filepath.Join(src, "specs", filepath.FromSlash(capName))
		if dirExists(canonCapDir) {
			_ = copyPathRecursive(canonCapDir, filepath.Join(dst, "specs", filepath.FromSlash(capName)))
		}
	}
	_ = copyPathRecursive(filepath.Join(src, "changes", change), filepath.Join(dst, "changes", change))

	arch, _ := d.opts.OpenSpecRunner(ctx, tmp, "archive", change, "--yes", "--json")
	if !arch.OK {
		return fmt.Sprintf("A trial `openspec archive %s` failed:\n%s", change, tailLines(arch.Output, 80))
	}

	specs, _ := d.opts.OpenSpecRunner(ctx, tmp, "validate", "--specs", "--strict", "--json")
	if specs.OK {
		return ""
	}
	var data map[string]any
	if parseOpenSpecJSON(specs.Output, &data) == nil {
		if items, ok := data["items"].([]any); ok && len(items) > 0 {
			caps := make(map[string]bool)
			for _, c := range d.capabilities(wt, change) {
				caps[c] = true
			}
			ownFailed := false
			for _, rawItem := range items {
				if it, ok := rawItem.(map[string]any); ok {
					valid, _ := it["valid"].(bool)
					id := fmt.Sprint(it["id"])
					if !valid && caps[id] {
						ownFailed = true
						break
					}
				}
			}
			if !ownFailed {
				return ""
			}
		}
	}
	return "Once archived, this change would leave canonical specs that fail `openspec validate --specs --strict`. " +
		"Fix it in the delta specs only, never in openspec/specs/: the delta of a new capability must start with " +
		"a `## Purpose` section of at least 50 characters, which becomes the canonical Purpose.\n" + ExtractOpenSpecInvalidItems(specs.Output)
}

func (d *Dispatcher) refreshPhase(ctx context.Context, item BacklogItem, wt, branch string, slot int, wasRebuildReq bool, retriedPtr *map[string]any) (string, error) {
	change := item.ChangeName
	func() {
		// Unlocked by a defer: go-git fetches under gitMu, and a panic there that workerPanicked contains
		// must not leave gitMu locked, or every later fetch and worktree would wait for it forever.
		d.gitMu.Lock()
		defer d.gitMu.Unlock()
		if r, err := gitx.Open(wt); err == nil {
			_ = r.Fetch(ctx, d.cfg().Remote, d.cfg().BaseBranch)
		}
	}()

	baseRef := d.resolveBaseRef(ctx, wt)
	wtRepo, err := gitx.Open(wt)
	if err != nil {
		return "", err
	}
	children, _ := wtRepo.ChildPaths()

	fork, err := wtRepo.MergeBase("HEAD", baseRef)
	if err != nil {
		fork = baseRef
	}
	baseSHA, _ := wtRepo.RevParse(baseRef)
	_, specsDiffErr := gitx.RunGit(ctx, wt, "diff", "--quiet", fork, baseRef, "--", "openspec/specs")
	specsMoved := specsDiffErr != nil

	if fork != "" && baseSHA != "" && fork != baseSHA {
		if len(children) > 0 {
			if childGitlinksMoved(ctx, wt, fork, children) {
				if _, err := gitx.RunGit(ctx, wt, "reset", "--soft", fork); err != nil {
					return "", err
				}
				resetArgs := append([]string{"reset", fork, "--"}, children...)
				if _, err := gitx.RunGit(ctx, wt, resetArgs...); err != nil {
					return "", err
				}
				if err := d.commitUmbrella(ctx, wt, "docs(openspec): plan "+change, []string{"openspec/changes/" + change}); err != nil {
					return "", err
				}
			}
		}

		reb := gitx.RebaseBranch(ctx, wt, baseRef)
		if !reb.OK {
			reason := "rebase_failed"
			if reb.ContentConflict {
				reason = "spec_conflict"
			}
			return "", NewBlockedError(reason, reb.Output)
		}
		// Re-read ChildPaths and populate any new submodules added on origin/<base_branch> after rebase
		if wtRepoAfter, err := gitx.Open(wt); err == nil {
			children, _ = wtRepoAfter.ChildPaths()
			_ = d.populateSubmodules(ctx, wt, children)
			d.linkLocalChildConfigs(wt, children)
		}
		if specsMoved {
			if err := d.planPhase(ctx, item, wt, slot, retriedPtr); err != nil {
				return "", err
			}
		}
	}

	var rebuilt []string
	for _, cp := range children {
		childDir := filepath.Join(wt, filepath.FromSlash(cp))
		target, err := wtRepo.TreeEntryHash("HEAD", cp)
		if err != nil {
			return "", NewBlockedError("rebase_failed", fmt.Sprintf("%s: cannot resolve HEAD:%s: %v", cp, cp, err))
		}
		if childRepo, err := gitx.Open(childDir); err == nil {
			_ = childRepo.Fetch(ctx, d.cfg().Remote)
		}

		if rb := d.pointerRollback(ctx, wt, baseRef, cp); rb != nil && rb.To != rb.From && isAncestorIn(ctx, childDir, target, rb.From) {
			// A person's commit moved the gitlink back (a stale submodule checkout committed with git
			// commit -a): building on it would rebuild what the pipeline already integrated there.
			// Build on the gitlink from before (a branch started on the old one is rebased onto it);
			// the merge records it forward again.
			d.logf("dispatcher: %s: %s is built on %s, its gitlink before umbrella commit %s moved it back to %s", change, cp, shortSHA(rb.From), shortSHA(rb.Commit), shortSHA(rb.To))
			target = rb.From
			d.stateMu.Lock()
			if d.rollbackMoved == nil {
				d.rollbackMoved = map[string]map[string][2]string{}
			}
			if d.rollbackMoved[change] == nil {
				d.rollbackMoved[change] = map[string][2]string{}
			}
			d.rollbackMoved[change][cp] = [2]string{rb.From, rb.To}
			d.stateMu.Unlock()
		}
		_, branchErr := gitx.RunGit(ctx, childDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
		if branchErr == nil && wasRebuildReq {
			// The merger sent the change back because its commits conflict with the child's
			// remote base branch, which it rebases onto. When commits were pushed there outside
			// the pipeline (a hotfix the umbrella's gitlink does not record yet), rebasing onto
			// the gitlink alone is a no-op: the same commits come back ready and conflict again,
			// so every rebuild would be spent without one. Rebase onto what the merger does.
			// (target is the worker's own gitlink, which already records the change's commits:
			// the outside commits are measured from the base's gitlink.)
			// Only a child the change has commits in: one it does not touch keeps its gitlink, so the
			// rebuild never drags another repository's outside commits (and their checks) into it.
			baseLink, _ := wtRepo.TreeEntryHash(baseRef, cp)
			if baseLink != "" && target != baseLink {
				if onto := childRemoteOnto(ctx, childDir, d.cfg().Remote, wtRepo.ChildBaseBranch(cp), baseLink); onto != "" {
					target = onto
				}
			}
		}
		if branchErr == nil {
			if _, err := d.childGit(ctx, childDir, cp, "switch", "--quiet", branch); err != nil {
				return "", err
			}
			if _, err := gitx.RunGit(ctx, childDir, "merge-base", "--is-ancestor", target, "HEAD"); err == nil {
				continue
			}
			reb := gitx.RebaseBranch(ctx, childDir, target)
			if reb.OK {
				continue
			}
			if !reb.ContentConflict {
				return "", NewBlockedError("rebase_failed", fmt.Sprintf("%s: git rebase %s failed without a conflict:\n%s", cp, target, tailLines(reb.Output, 20)))
			}
			msg, err := d.rebuildChild(ctx, change, cp, childDir, branch, target, reb.Output, wasRebuildReq)
			if err != nil {
				return "", err
			}
			rebuilt = append(rebuilt, msg)
		} else {
			if _, err := d.childGit(ctx, childDir, cp, "switch", "--quiet", "--detach", target); err != nil {
				return "", err
			}
			if _, err := d.childGit(ctx, childDir, cp, "switch", "--quiet", "-c", branch); err != nil {
				return "", err
			}
		}
	}

	if len(rebuilt) == 0 {
		return "", nil
	}
	if err := d.reopenTasks(ctx, wt, change); err != nil {
		return "", err
	}
	return strings.Join(rebuilt, "\n"), nil
}

func (d *Dispatcher) rebuildChild(ctx context.Context, change, cp, childDir, branch, target, conflict string, wasRebuildReq bool) (string, error) {
	q, _ := d.store.GetQuarantine(ctx, change)
	count := 0
	if q != nil {
		count = q.Rebuilds
	}
	// If store.RequestRebuild already incremented rebuilds, count reflects this rebuild (1..conflict_rebuilds)
	if wasRebuildReq {
		if count > d.cfg().ConflictRebuilds || d.cfg().ConflictRebuilds <= 0 {
			return "", NewBlockedError("rebase_conflict", cp+": "+conflict)
		}
	} else {
		if count >= d.cfg().ConflictRebuilds {
			return "", NewBlockedError("rebase_conflict", cp+": "+conflict)
		}
		count++
		_, _ = d.store.RequestRebuild(ctx, change, branch, conflict)
		_ = d.store.ClearQuarantine(ctx, change, false)
	}

	number := count
	for {
		keptCandidate := fmt.Sprintf("%s-conflict-%d", branch, number)
		if _, err := gitx.RunGit(ctx, childDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+keptCandidate); err != nil {
			break
		}
		number++
	}
	kept := fmt.Sprintf("%s-conflict-%d", branch, number)

	matches := mergeConflictFileRe.FindAllStringSubmatch(conflict, -1)
	seenF := make(map[string]bool)
	var files []string
	for _, m := range matches {
		if len(m) == 2 && !seenF[m[1]] {
			seenF[m[1]] = true
			files = append(files, m[1])
		}
	}

	if _, err := d.childGit(ctx, childDir, cp, "branch", "-f", kept, branch); err != nil {
		return "", err
	}
	if _, err := d.childGit(ctx, childDir, cp, "switch", "--quiet", "--detach", target); err != nil {
		return "", err
	}
	if _, err := d.childGit(ctx, childDir, cp, "branch", "-f", branch, target); err != nil {
		return "", err
	}
	if _, err := d.childGit(ctx, childDir, cp, "switch", "--quiet", branch); err != nil {
		return "", err
	}

	_ = d.store.EmitEvent(ctx, store.EventInput{
		Event:  "change.rebuild",
		Change: change,
		Payload: map[string]any{
			"repo":  cp,
			"kept":  kept,
			"files": strings.Join(files, ","),
		},
	})

	inFiles := ""
	if len(files) > 0 {
		inFiles = " in " + strings.Join(files, ", ")
	}
	return fmt.Sprintf(
		"%s: the previous implementation, kept on branch %s of that repository, conflicted with work integrated since%s. "+
			"Implement the tasks again on the current code: reuse what still applies (git -C %s diff %s %s) and keep what the integrated work changed.",
		cp, kept, inFiles, cp, target, kept,
	), nil
}

// childRemoteOnto returns the commit of the child's <remote>/<baseBranch> when it holds commits
// the umbrella's gitlink does not (pushed to the repository outside the pipeline) and descends
// from it, as the merger's rebase target; otherwise "".
func childRemoteOnto(ctx context.Context, childDir, remote, baseBranch, gitlink string) string {
	if remote == "" {
		remote = "origin"
	}
	out, err := gitx.RunGit(ctx, childDir, "rev-parse", "--verify", "--quiet", remote+"/"+baseBranch+"^{commit}")
	if err != nil {
		return ""
	}
	onto := strings.TrimSpace(out)
	if onto == "" || onto == gitlink {
		return ""
	}
	if gitlink != "" {
		if _, err := gitx.RunGit(ctx, childDir, "merge-base", "--is-ancestor", gitlink, onto); err != nil {
			return ""
		}
	}
	return onto
}

// workerChildOnto maps each child whose branch was rebased past the gitlink at baseRef onto its
// remote base branch (a rebuild over commits pushed outside the pipeline) to that commit, so the
// worker's gates measure the change's own commits from it, as the merger's do.
func (d *Dispatcher) workerChildOnto(ctx context.Context, wt, baseRef string) map[string]string {
	wtRepo, err := gitx.Open(wt)
	if err != nil {
		return nil
	}
	children, _ := wtRepo.ChildPaths()
	out := map[string]string{}
	for _, cp := range children {
		pointer, err := wtRepo.TreeEntryHash(baseRef, cp)
		if err != nil || pointer == "" {
			continue
		}
		childDir := filepath.Join(wt, filepath.FromSlash(cp))
		onto := childRemoteOnto(ctx, childDir, d.cfg().Remote, wtRepo.ChildBaseBranch(cp), pointer)
		if onto != "" && isAncestorIn(ctx, childDir, onto, "HEAD") {
			out[cp] = onto
			continue
		}
		// Built on the gitlink from before a rollback: what the pipeline integrated up to there is
		// not the change's either.
		if rb := d.pointerRollback(ctx, wt, baseRef, cp); rb != nil && isAncestorIn(ctx, childDir, pointer, rb.From) && isAncestorIn(ctx, childDir, rb.From, "HEAD") {
			out[cp] = rb.From
		}
	}
	return out
}

// pointerRollback returns the rollback of child cp's gitlink at baseRef in the worktree wt
// (gitx.FindPointerRollback), reported once as repo.pointer_rolled_back, or nil.
func (d *Dispatcher) pointerRollback(ctx context.Context, wt, baseRef, cp string) *gitx.PointerRollback {
	rb := gitx.FindPointerRollback(ctx, wt, baseRef, cp, 200)
	if rb == nil {
		return nil
	}
	d.reportOnce(ctx, "repo.pointer_rolled_back", map[string]any{
		"repo": rb.Child, "commit": rb.Commit, "from": rb.From, "to": rb.To, "lost_commits": rb.Lost,
	})
	return rb
}

func isAncestorIn(ctx context.Context, dir, a, b string) bool {
	_, err := gitx.RunGit(ctx, dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

func (d *Dispatcher) childGit(ctx context.Context, childDir, cp string, args ...string) (string, error) {
	out, err := gitx.RunGit(ctx, childDir, args...)
	if err != nil {
		return "", NewBlockedError("rebase_failed", fmt.Sprintf("%s: git %s failed:\n%s", cp, strings.Join(args, " "), tailLines(out, 20)))
	}
	return strings.TrimSpace(out), nil
}

func (d *Dispatcher) reopenTasks(ctx context.Context, wt, change string) error {
	tasksPath := filepath.Join(wt, "openspec", "changes", change, "tasks.md")
	raw, err := os.ReadFile(tasksPath)
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`(?m)^(\s*(?:[-*+]|\d{1,9}[.)])\s+)\[[xX]\]`)
	reopened := re.ReplaceAllString(string(raw), "${1}[ ]")
	if err := os.WriteFile(tasksPath, []byte(reopened), 0o644); err != nil {
		return err
	}
	return d.commitUmbrella(ctx, wt, fmt.Sprintf("docs(openspec): reopen %s for a rebuild on the new base", change), []string{"openspec/changes/" + change})
}

func (d *Dispatcher) buildPhase(ctx context.Context, item BacklogItem, wt, branch string, slot int, rebuildFeedback string, retriedPtr *map[string]any) error {
	change := item.ChangeName
	attempts := 0
	var failure *failureInfo
	reran := false

	wtRepo, err := gitx.Open(wt)
	if err != nil {
		return err
	}
	children, _ := wtRepo.ChildPaths()

	if d.tasksComplete(ctx, wt, change) {
		if err := d.commitChildren(ctx, wt, change); err != nil {
			return err
		}
		failure, err = d.verifyBuild(ctx, wt, change, branch, slot)
		if err != nil {
			return err
		}
		if failure != nil && failure.Reason == "gate_failure" {
			failure, reran, err = d.triageFailure(ctx, change, wt, branch, slot, failure, reran)
			if err != nil {
				return err
			}
		}
		// A context that ended meanwhile (the dispatcher is stopping, or the worker lost its lease)
		// makes git and the gates fail on their own: such a verdict never starts a repair round.
		if err := ctx.Err(); err != nil {
			return err
		}
		if failure == nil {
			if err := d.unmoveUntouchedChildren(ctx, wt, change, branch); err != nil {
				return err
			}
			paths := append([]string{"openspec/changes/" + change}, children...)
			return d.commitUmbrella(ctx, wt, "feat(spec): implement "+change, paths)
		}
		attempts = 1
		if attempts > d.cfg().repairRounds {
			return NewBlockedError(failure.Reason, failure.Details)
		}
	}

	for {
		phase := "apply"
		if attempts > 0 {
			phase = fmt.Sprintf("repair-%d", attempts)
		}
		// Any new agent edit invalidates a prior human risk_review approval
		d.SetApprovedRisk(ctx, change, false)

		asked := failure
		heads := d.childHeads(ctx, wt)
		var fb string
		if failure != nil {
			fb = failure.Details
		}
		rebArg := ""
		if attempts == 0 {
			rebArg = rebuildFeedback
		}
		prompt, promptVer := d.buildApplyPrompt(item, wt, branch, fb, rebArg, *retriedPtr)
		usedEngine, err := d.invokeAgent(ctx, change, phase, "build", d.cfg().engineBuild, wt, prompt, promptVer, slot, retriedPtr)
		if err != nil {
			return err
		}
		if err := d.commitChildren(ctx, wt, change); err != nil {
			return err
		}
		failure, err = d.verifyBuild(ctx, wt, change, branch, slot)
		if err != nil {
			return err
		}
		if failure != nil && failure.Reason == "gate_failure" {
			failure, reran, err = d.triageFailure(ctx, change, wt, branch, slot, failure, reran)
			if err != nil {
				return err
			}
		}
		// As above: a verdict reached under an ended context is never recorded as a repair round.
		if err := ctx.Err(); err != nil {
			return err
		}
		if asked != nil {
			d.recordRepair(ctx, wt, change, repairRound{
				Phase:      phase,
				Engine:     usedEngine,
				ExecID:     d.lastAgentExecution(change),
				Asked:      *asked,
				HasChanged: true,
				Changed:    d.childDiffstat(ctx, wt, heads),
			}, failure)
		}
		if failure == nil {
			break
		}

		attempts++
		if attempts > d.cfg().repairRounds {
			return NewBlockedError(failure.Reason, failure.Details)
		}
		if asked != nil {
			if err := d.checkProgress(ctx, change, wt, *asked, *failure); err != nil {
				return err
			}
		}
	}

	if err := d.unmoveUntouchedChildren(ctx, wt, change, branch); err != nil {
		return err
	}
	paths := append([]string{"openspec/changes/" + change}, children...)
	return d.commitUmbrella(ctx, wt, "feat(spec): implement "+change, paths)
}

// unmoveUntouchedChildren puts back on its rolled-back gitlink every child refreshPhase moved past a
// rollback in which the change made no commit: the change was built against the right code, but it
// does not touch that repository, so it must not record its gitlink (and bring that repository's
// outside commits and base checks into its merge). A change in that repository records it forward.
func (d *Dispatcher) unmoveUntouchedChildren(ctx context.Context, wt, change, branch string) error {
	d.stateMu.Lock()
	moved := d.rollbackMoved[change]
	delete(d.rollbackMoved, change)
	d.stateMu.Unlock()
	for cp, heads := range moved {
		childDir := filepath.Join(wt, filepath.FromSlash(cp))
		head, err := gitx.RunGit(ctx, childDir, "rev-parse", "HEAD")
		if err != nil || strings.TrimSpace(head) != heads[0] {
			continue
		}
		if _, err := d.childGit(ctx, childDir, cp, "switch", "--quiet", "-C", branch, heads[1]); err != nil {
			return err
		}
		d.logf("dispatcher: %s: %s keeps its gitlink %s (no commit there; a change in it records %s again)", change, cp, shortSHA(heads[1]), shortSHA(heads[0]))
	}
	return nil
}

func (d *Dispatcher) verifyBuild(ctx context.Context, wt, change, branch string, slot int) (*failureInfo, error) {
	wtRepo, err := gitx.Open(wt)
	if err != nil {
		return nil, err
	}
	children, _ := wtRepo.ChildPaths()
	baseRef := d.resolveBaseRef(ctx, wt)
	fork, err := wtRepo.MergeBase("HEAD", baseRef)
	if err != nil {
		fork = baseRef
	}

	outsidePaths := func() []string {
		// -z output: unquoted paths (spaces, non-ASCII) and both sides of renames.
		touched, _ := gitx.ChangedPaths(ctx, wt, fork)
		var outside []string
		for _, p := range touched {
			allowed := strings.HasPrefix(p, "openspec/changes/"+change+"/")
			if !allowed {
				for _, c := range children {
					if p == c || strings.HasPrefix(p, c+"/") {
						allowed = true
						break
					}
				}
			}
			if !allowed {
				outside = append(outside, p)
			}
		}
		return outside
	}
	if outside := outsidePaths(); len(outside) > 0 {
		fail, err := d.triageOutsidePaths(ctx, wt, change, fork, outside)
		if err != nil || fail != nil {
			return fail, err
		}
		// Reverted as stray artifacts: anything still outside goes back to the agent.
		if outside := outsidePaths(); len(outside) > 0 {
			return &failureInfo{
				Reason:  "policy_violation",
				Details: "Revert changes outside the allowed paths: " + strings.Join(outside, ", "),
			}, nil
		}
	}

	var offBranch []string
	for _, c := range children {
		cur, _ := gitx.RunGit(ctx, filepath.Join(wt, filepath.FromSlash(c)), "branch", "--show-current")
		if strings.TrimSpace(cur) != branch {
			offBranch = append(offBranch, c)
		}
	}
	if len(offBranch) > 0 {
		return &failureInfo{
			Reason:  "policy_violation",
			Details: "Child repos left " + branch + ": " + strings.Join(offBranch, ", "),
		}, nil
	}

	if !d.tasksComplete(ctx, wt, change) {
		_, _, details := UncheckedTasksDetails(filepath.Join(wt, "openspec", "changes", change, "tasks.md"), change)
		return &failureInfo{
			Reason:  "incomplete_tasks",
			Details: details,
		}, nil
	}

	gateTimeout := d.cfg().GateTimeout
	gateRes, err := d.opts.GateRunner(ctx, GateRunRequest{
		Change:         change,
		Worktree:       wt,
		BaseRef:        fork,
		Slot:           slot,
		Timeout:        gateTimeout,
		ChildOnto:      d.workerChildOnto(ctx, wt, fork),
		OutsideCommits: d.cfg().OutsideCommits,
	})
	if err != nil {
		return nil, err
	}
	if !gateRes.OK {
		if ok, gates, _ := failuresAcceptedOnBase(ctx, d.store, wt, fork, gateRes); ok {
			// The base this change builds on fails these tests on its own and a human accepted it.
			d.logf("dispatcher: %s: %s fail(s) only with the tests its accepted base fails: passed", change, strings.Join(gates, " "))
			_ = d.store.EmitEvent(ctx, store.EventInput{
				Event:   "change.gates_accepted",
				Change:  change,
				Payload: map[string]any{"gates": gates},
			})
			gateRes.OK = true
		}
	}
	if !gateRes.OK {
		details := tailLines(gateRes.Output, 120)
		if _, _, why := failuresAcceptedOnBase(ctx, d.store, wt, fork, gateRes); len(why) > 0 {
			details += "\n---- [info] " + strings.Join(why, "\n---- [info] ")
		}
		// Use Case 1 (internal/llm): Surgical gate failure diagnosis via zendev-sh/goai GenerateObject[T]
		if d.llmClient != nil && d.llmClient.Available() {
			if diag, err := d.llmClient.DiagnoseGateFailure(ctx, change, details, d.deltaSpecs(wt, change)); err == nil && diag != nil {
				details = diag.FormatMarkdown() + "\n" + details
			}
		}
		return &failureInfo{
			Reason:      "gate_failure",
			Details:     details,
			GateRoundID: gateRes.RoundID,
		}, nil
	}

	return d.checkCoverageFailure(ctx, wt, change)
}

// triageOutsidePaths asks policy_violation_triage about files a BUILD left outside its allowed paths:
// stray artifacts are reverted and the build continues (nil, nil), tampering is quarantined at once, and
// anything else (or a protected path) goes back to the agent as a policy_violation repair, as before.
func (d *Dispatcher) triageOutsidePaths(ctx context.Context, wt, change, fork string, outside []string) (*failureInfo, error) {
	def := &failureInfo{
		Reason:  "policy_violation",
		Details: "Revert changes outside the allowed paths: " + strings.Join(outside, ", "),
	}
	for _, p := range outside {
		if isProtectedRootPath(p) {
			return def, nil
		}
	}
	state := map[string]any{"change": change, "outside": outsidePathState(ctx, wt, fork, outside)}
	action, v := routeGate(ctx, d.judge, d.store, PolicyViolationGate, change, state)
	switch action {
	case "revert_and_continue":
		if err := revertOutsidePaths(ctx, wt, fork, outside); err != nil {
			d.logf("dispatcher: %s: could not revert stray files (%v): asking the agent", change, err)
			return def, nil
		}
		_ = d.store.EmitEvent(ctx, store.EventInput{
			Event:   "change.stray_reverted",
			Change:  change,
			Payload: map[string]any{"paths": strings.Join(outside, ",")},
		})
		d.logf("dispatcher: %s: reverted stray files outside its paths (evaluator): %s", change, strings.Join(outside, ", "))
		return nil, nil
	case "quarantine":
		return nil, NewBlockedError("policy_violation", def.Details+"\n\n"+answerSummary(v))
	}
	return def, nil
}

func (d *Dispatcher) triageFailure(ctx context.Context, change, wt, branch string, slot int, failure *failureInfo, reran bool) (*failureInfo, bool, error) {
	out, reranOut, err := d.triageGateFailure(ctx, change, wt, branch, slot, failure, reran)
	// Whatever the triage adds to the failure, it is still the failure of that gate round.
	if out != nil && out.GateRoundID == 0 && failure != nil {
		out.GateRoundID = failure.GateRoundID
	}
	return out, reranOut, err
}

func (d *Dispatcher) triageGateFailure(ctx context.Context, change, wt, branch string, slot int, failure *failureInfo, reran bool) (*failureInfo, bool, error) {
	if d.judge == nil {
		return failure, reran, nil
	}
	state := map[string]any{
		"change":      change,
		"gate_output": failure.Details,
		"delta_specs": d.deltaSpecs(wt, change),
	}
	verdict, err := d.judge.Ask(ctx, "failure_triage", state, change)
	if err != nil {
		if strings.HasPrefix(err.Error(), "judge_unavailable:") {
			return nil, reran, NewBlockedError("judge_unavailable", strings.TrimPrefix(err.Error(), "judge_unavailable: "))
		}
		return nil, reran, err
	}
	if verdict == nil {
		return failure, reran, nil
	}
	cause, ok := verdict.Answers["cause"]
	if !ok {
		return failure, reran, nil
	}
	choice := fmt.Sprint(cause["choice"])
	conf := cause["confidence"]
	hint := fmt.Sprintf("Failure triage by the evaluator: most likely cause is %s (confidence %v).", choice, conf)

	pol := d.judge.Policy().Gates["failure_triage"]
	if verdict.Enforce() {
		if !reran && evaluator.MatchesRule(*verdict, pol.RerunWhen) {
			emitRoute(ctx, d.store, change, verdict, map[string]any{
				"action":     "rerun_gates",
				"cause":      choice,
				"confidence": conf,
			})
			nextFail, err := d.verifyBuild(ctx, wt, change, branch, slot)
			if err == nil && nextFail != nil && nextFail.Reason == "gate_failure" {
				// Still failing after the re-run: triage it again (its actions may hold it).
				return d.triageFailure(ctx, change, wt, branch, slot, nextFail, true)
			}
			return nextFail, true, err
		}
		if evaluator.MatchesRule(*verdict, pol.EscalateWhen) {
			emitRoute(ctx, d.store, change, verdict, map[string]any{
				"action":     "escalate",
				"cause":      choice,
				"confidence": conf,
			})
			return nil, reran, NewBlockedError("spec_ambiguity", hint+"\n"+failure.Details)
		}
		if action, rule := evaluator.ActionFor(*verdict, pol); action == "hold_environment" {
			emitRoute(ctx, d.store, change, verdict, map[string]any{
				"gate":       FailureTriageGate,
				"action":     action,
				"rule":       evaluator.RuleLabel(*rule),
				"cause":      choice,
				"confidence": conf,
				"reran":      reran,
			})
			d.logf("dispatcher: %s: gates fail because of the environment (evaluator): held without repair rounds until it recovers", change)
			return nil, reran, NewBlockedError("environment_hold", hint+" The environment is failing, not the code: held without repair rounds.\n"+failure.Details)
		}
	}
	return &failureInfo{
		Reason:  failure.Reason,
		Details: hint + "\n" + failure.Details,
	}, reran, nil
}

func (d *Dispatcher) checkProgress(ctx context.Context, change, wt string, asked, failure failureInfo) error {
	if d.judge == nil || asked.Reason != "gate_failure" || failure.Reason != "gate_failure" {
		return nil
	}
	state := map[string]any{
		"change":               change,
		"previous_gate_output": asked.Details,
		"gate_output":          failure.Details,
		"delta_specs":          d.deltaSpecs(wt, change),
	}
	verdict, err := d.judge.Ask(ctx, "repair_progress", state, change)
	if err != nil {
		if strings.HasPrefix(err.Error(), "judge_unavailable:") {
			return NewBlockedError("judge_unavailable", strings.TrimPrefix(err.Error(), "judge_unavailable: "))
		}
		return err
	}
	if verdict != nil && verdict.Blocking() {
		emitRoute(ctx, d.store, change, verdict, map[string]any{
			"action": "repair_stalled",
		})
		return NewBlockedError("repair_stalled", evaluator.Feedback(d.root, d.judge.Policy(), *verdict)+"\n"+failure.Details)
	}
	return nil
}

func (d *Dispatcher) checkCoverageFailure(ctx context.Context, wt, change string) (*failureInfo, error) {
	if d.judge == nil {
		return nil, nil
	}
	pol := d.judge.Policy()
	if pol.State.IncludeDiffs != nil && !*pol.State.IncludeDiffs {
		return nil, nil
	}
	repos := d.childChanges(ctx, wt)
	if len(repos) == 0 {
		return nil, nil
	}
	state := map[string]any{
		"change":      change,
		"delta_specs": d.deltaSpecs(wt, change),
		"tasks":       readFileOrEmpty(filepath.Join(wt, "openspec", "changes", change, "tasks.md")),
		"repos":       repos,
	}
	verdict, err := d.judge.Ask(ctx, "scenario_coverage", state, change)
	if err != nil {
		if strings.HasPrefix(err.Error(), "judge_unavailable:") {
			return nil, NewBlockedError("judge_unavailable", strings.TrimPrefix(err.Error(), "judge_unavailable: "))
		}
		return nil, err
	}
	if verdict != nil && verdict.Blocking() {
		fb := evaluator.Feedback(d.root, pol, *verdict)
		// Use Case 2 (internal/llm): Translate numeric scenario_coverage rejection into actionable item breakdown
		if d.llmClient != nil && d.llmClient.Available() {
			if bd, err := d.llmClient.ExplainRejection(ctx, change, "scenario_coverage", verdict.Failed, state); err == nil && bd != nil {
				fb = fb + "\n\n" + bd.FormatMarkdown()
			}
		}
		return &failureInfo{
			Reason:  "scenario_coverage",
			Details: fb,
		}, nil
	}
	return nil, nil
}

func (d *Dispatcher) assessRisk(ctx context.Context, item BacklogItem, wt, change string, caps []string) (map[string]any, error) {
	if d.judge == nil {
		return nil, nil
	}
	state := map[string]any{
		"change":       change,
		"goal":         sanitizeBacklogText(item.Description),
		"proposal":     readFileOrEmpty(filepath.Join(wt, "openspec", "changes", change, "proposal.md")),
		"capabilities": caps,
		"repos":        d.childChanges(ctx, wt),
	}
	verdict, err := d.judge.Ask(ctx, "change_risk", state, change)
	if err != nil {
		if strings.HasPrefix(err.Error(), "judge_unavailable:") {
			return nil, NewBlockedError("judge_unavailable", strings.TrimPrefix(err.Error(), "judge_unavailable: "))
		}
		return nil, err
	}
	if verdict == nil || len(verdict.Answers) == 0 {
		return nil, nil
	}

	summary := evaluator.SummarizeAnswers(verdict.Answers)
	summary["model"] = verdict.Model
	if verdict.EvaluationID != 0 {
		// The manifest points at the evaluation that assessed its risk.
		summary["evaluation_id"] = verdict.EvaluationID
	}

	pol := d.judge.Policy().Gates["change_risk"]
	var reasons []string
	for i := range pol.BlockWhen {
		rule := pol.BlockWhen[i]
		if evaluator.MatchesRule(*verdict, &rule) {
			if rule.Choice != "" {
				reasons = append(reasons, fmt.Sprintf("%s=%s", rule.Question, rule.Choice))
			} else if rule.Min != nil {
				reasons = append(reasons, fmt.Sprintf("%s>=%v", rule.Question, *rule.Min))
			}
		}
	}

	q, _ := d.store.GetQuarantine(ctx, change)
	approved := (q != nil && q.ApprovedRisk) ||
		fileExists(filepath.Join(d.root, "log", "approved", change)) ||
		(d.mainRoot != "" && fileExists(filepath.Join(d.mainRoot, "log", "approved", change)))

	if len(reasons) > 0 {
		action := "flagged_advisory"
		if approved {
			action = "approved_by_operator"
		} else if verdict.Enforce() {
			action = "risk_review"
		}
		emitRoute(ctx, d.store, change, verdict, map[string]any{
			"action": action,
			"rules":  strings.Join(reasons, ","),
		})
	}
	if verdict.Enforce() && len(reasons) > 0 && !approved {
		sumBytes, _ := json.Marshal(summary)
		return nil, NewBlockedError("risk_review", fmt.Sprintf(
			"The evaluator flagged %s (%s). Review and approve risk to requeue.",
			strings.Join(reasons, ", "), string(sumBytes),
		))
	}
	summary["approved_by_operator"] = approved && len(reasons) > 0
	return summary, nil
}

func (d *Dispatcher) tasksComplete(ctx context.Context, wt, change string) bool {
	tasksPath := filepath.Join(wt, "openspec", "changes", change, "tasks.md")
	raw := readFileOrEmpty(tasksPath)
	if raw == "" || uncheckedTaskRe.MatchString(raw) {
		return false
	}
	res, _ := d.opts.OpenSpecRunner(ctx, wt, "list", "--json")
	if res.OK {
		var listing map[string]any
		if parseOpenSpecJSON(res.Output, &listing) == nil {
			if chgs, ok := listing["changes"].([]any); ok {
				for _, rawChg := range chgs {
					if m, ok := rawChg.(map[string]any); ok && fmt.Sprint(m["name"]) == change {
						comp, ok1 := toInt(m["completedTasks"])
						tot, ok2 := toInt(m["totalTasks"])
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

func (d *Dispatcher) commitChildren(ctx context.Context, wt, change string) error {
	wtRepo, err := gitx.Open(wt)
	if err != nil {
		return err
	}
	children, _ := wtRepo.ChildPaths()
	for _, cp := range children {
		childDir := filepath.Join(wt, filepath.FromSlash(cp))
		stOut, _ := gitx.RunGit(ctx, childDir, "status", "--porcelain")
		if strings.TrimSpace(stOut) == "" {
			continue
		}
		if err := d.commitChild(ctx, childDir, cp, fmt.Sprintf("feat(%s): implement OpenSpec change %s", change, change)); err != nil {
			return err
		}
	}
	return nil
}

func (d *Dispatcher) commitChild(ctx context.Context, childDir, cp, message string) error {
	if _, err := gitx.RunGit(ctx, childDir, "add", "-A"); err != nil {
		return err
	}
	if out, err := gitx.RunGit(ctx, childDir, "commit", "--quiet", "-m", message); err != nil {
		return NewBlockedError("gate_failure", fmt.Sprintf("commit hook rejected %s:\n%s", cp, out))
	}
	return nil
}

func (d *Dispatcher) commitUmbrella(ctx context.Context, wt, message string, paths []string) error {
	addArgs := append([]string{"add", "-A", "--"}, paths...)
	if _, err := gitx.RunGit(ctx, wt, addArgs...); err != nil {
		return err
	}
	// A child .gitmodules marks "ignore = all" is skipped by that "git add", exit status 0, and its
	// staged gitlink is hidden from "git diff" (git 2.54 and later). The gitlinks among paths are
	// staged by name and the staged changes read with --ignore-submodules=none, or the change's branch
	// never records the commits its agent made in that child and the merger gets a change without them.
	if links := checkedOutChildren(wt, paths); len(links) > 0 {
		if err := gitx.StageGitlinks(ctx, wt, links...); err != nil {
			return err
		}
	}
	if _, err := gitx.RunGit(ctx, wt, "diff", "--cached", "--quiet", "--ignore-submodules=none"); err == nil {
		return nil
	}
	_, err := gitx.RunGit(ctx, wt, "commit", "--quiet", "-m", message)
	return err
}

// checkedOutChildren returns the entries of paths that are child repositories of the umbrella worktree wt
// and have a directory there.
func checkedOutChildren(wt string, paths []string) []string {
	repo, err := gitx.Open(wt)
	if err != nil {
		return nil
	}
	children, _ := repo.ChildPaths()
	isChild := make(map[string]bool, len(children))
	for _, c := range children {
		isChild[c] = true
	}
	var out []string
	for _, p := range paths {
		if isChild[p] && dirExists(filepath.Join(wt, filepath.FromSlash(p))) {
			out = append(out, p)
		}
	}
	return out
}

// childGitlinksMoved reports whether the commit recorded for one of children differs between fork and
// HEAD in the umbrella worktree wt. --ignore-submodules=none: for a child .gitmodules marks
// "ignore = all", git diff reports no change of its gitlink, and a branch that records commits of that
// child read as one that records none.
func childGitlinksMoved(ctx context.Context, wt, fork string, children []string) bool {
	if len(children) == 0 {
		return false
	}
	args := append([]string{"diff", "--quiet", "--ignore-submodules=none", fork, "HEAD", "--"}, children...)
	_, err := gitx.RunGit(ctx, wt, args...)
	return err != nil
}

func (d *Dispatcher) enqueueReady(ctx context.Context, change, wt, branch string, caps []string, assessment map[string]any, item BacklogItem) error {
	wtRepo, err := gitx.Open(wt)
	if err != nil {
		return err
	}
	headSHA, err := wtRepo.RevParse("HEAD")
	if err != nil {
		return err
	}
	now := d.nowFunc().Format(time.RFC3339)
	if err := d.store.EnqueueReady(ctx, store.ReadyManifest{
		ChangeName:   change,
		Branch:       branch,
		Worktree:     wt,
		HeadSHA:      headSHA,
		Capabilities: caps,
		Assessment:   assessment,
		Item:         item.ToMap(),
		EnqueuedAt:   now,
	}); err != nil {
		return err
	}
	d.opts.Hub.ReadyEnqueued()
	d.logf("dispatcher: %s: built, gates passed: ready to merge", change)
	_ = d.store.EmitEvent(ctx, store.EventInput{
		Event:  "change.ready",
		Change: change,
		Payload: map[string]any{
			"capabilities": strings.Join(caps, ","),
		},
	})
	return nil
}

// SetApprovedRisk updates approved_risk in SQLite and removes any legacy log/approved/<change> file when cleared.
func (d *Dispatcher) SetApprovedRisk(ctx context.Context, change string, approved bool) {
	if d.store != nil && d.store.DB() != nil {
		if approved {
			_ = d.store.ApproveChangeRiskBy(ctx, change, "dispatcher")
		} else {
			// New work by an agent: what a person approved was the change as it stood before it.
			_ = d.store.RevokeRiskApproval(ctx, change, "new_agent_work")
		}
	}
	if !approved {
		if d.mainRoot != "" {
			_ = os.Remove(filepath.Join(d.mainRoot, "log", "approved", change))
		}
		if d.root != "" && d.root != d.mainRoot {
			_ = os.Remove(filepath.Join(d.root, "log", "approved", change))
		}
	}
}

func (d *Dispatcher) saveWorkerPrompt(change, phase, prompt string) string {
	baseRoot := d.mainRoot
	if baseRoot == "" {
		baseRoot = d.root
	}
	workersDir := store.RuntimePath(baseRoot, "workers")
	_ = os.MkdirAll(workersDir, 0o755)
	promptFile := filepath.Join(workersDir, fmt.Sprintf("%s-%s.prompt.md", change, phase))
	_ = os.WriteFile(promptFile, []byte(prompt), 0o644)
	if d.root != "" && d.root != baseRoot {
		altDir := store.RuntimePath(d.root, "workers")
		_ = os.MkdirAll(altDir, 0o755)
		_ = os.WriteFile(filepath.Join(altDir, fmt.Sprintf("%s-%s.prompt.md", change, phase)), []byte(prompt), 0o644)
	}
	return promptFile
}

func (d *Dispatcher) invokeAgent(ctx context.Context, change, phase, role, engine, wt, prompt, promptVer string, slot int, retriedPtr *map[string]any) (string, error) {
	baseRoot := d.mainRoot
	if baseRoot == "" {
		baseRoot = d.root
	}
	scratchDir := store.RuntimePath(baseRoot, "tmp", change)
	_ = os.MkdirAll(scratchDir, 0o755)
	if role != "" && role != phase {
		_ = d.saveWorkerPrompt(change, role, prompt)
	}
	promptFile := d.saveWorkerPrompt(change, phase, prompt)

	memVer, _ := MemoryFingerprintAndPromptBlock(wt, d.capabilities(wt, change))

	// The execution is recorded here, before the agent runs, with what is asked of it; the runner
	// writes the output and the attempts under it; and how it ended is recorded below together with
	// its agent.exit event, also when the worker was stopped meanwhile.
	execID := NewWorkerExecID(change, phase)
	_ = d.store.CreateExecution(ctx, store.Execution{
		ExecID:           execID,
		Kind:             "worker",
		ChangeName:       change,
		Phase:            phase,
		Role:             role,
		EnginesRequested: engine,
		Status:           "running",
		PID:              os.Getpid(),
		PromptText:       prompt,
		PromptVersion:    promptVer,
		MemoryVersion:    memVer,
	})
	d.stateMu.Lock()
	if d.lastExec == nil {
		d.lastExec = make(map[string]string)
	}
	d.lastExec[change] = execID
	d.stateMu.Unlock()
	endExecution := func(exitCode int, used string) {
		_, _ = d.store.EndExecution(context.WithoutCancel(ctx), execID, store.ExecutionEnd{
			Status:     AgentExecutionStatus(ctx, exitCode),
			ExitCode:   exitCode,
			EngineUsed: used,
			AgentExit:  true,
		})
	}

	attempts := len(strings.Split(engine, ",")) * (d.cfg().AgentRetries + 1)
	if attempts < 1 {
		attempts = 1
	}
	// Every attempt may take the limit of one call of its role, plus the waits between them.
	callLimit := d.cfg().BuildTimeout
	if role == "plan" {
		callLimit = d.cfg().PlanTimeout
	}
	budgetSec := attempts*(int(callLimit/time.Second)+60) + (attempts-1)*int(d.cfg().AgentRetryAfter/time.Second) + 120

	res, err := d.opts.AgentRunner(ctx, AgentRunRequest{
		Change:     change,
		Phase:      phase,
		Role:       role,
		Engine:     engine,
		Worktree:   wt,
		Prompt:     prompt,
		PromptFile: promptFile,
		Slot:       slot,
		ScratchDir: scratchDir,
		Timeout:    time.Duration(budgetSec) * time.Second,
		ExecID:     execID,
	})
	if err != nil {
		endExecution(1, "")
		return "", err
	}
	// The context ended while the agent ran (the dispatcher is stopping, or the worker lost its lease):
	// its exit status is the cancellation's and says nothing about the engines or about the change.
	if err := ctx.Err(); err != nil {
		endExecution(res.ExitCode, res.UsedEngine)
		return "", err
	}

	used := res.UsedEngine
	if used == "" {
		used = engine
		if used == "" {
			used = "default"
		}
	}
	// What is recorded is an engine that ran: with no engine reported, the first one asked for.
	recorded := res.UsedEngine
	if recorded == "" {
		recorded = strings.TrimSpace(strings.Split(engine, ",")[0])
	}
	endExecution(res.ExitCode, recorded)

	if res.ExitCode == 127 {
		return "", NewBlockedError("engine_unavailable", res.Output)
	}
	if res.ExitCode == EnginesDownExitCode {
		// agent_outcome: an agent that gave up goes to a human, a broken environment pauses; anything else
		// (or no decision) is an engine outage the change waits out, as before.
		switch action, v := d.triageAgentOutcome(ctx, change, phase, role, engine, res.Output); action {
		case "needs_human":
			d.logf("dispatcher: %s: the agent gave up (evaluator): waiting for a human", change)
			return "", NewBlockedError("agent_gave_up", agentErrorLine(res.Output)+"\n"+answerSummary(v)+"\n\n"+tailLines(res.Output, 40))
		case "pause":
			return "", NewBlockedError("environment_unavailable", agentErrorLine(res.Output)+"\n"+answerSummary(v)+"\n\n"+tailLines(res.Output, 40))
		}
		return "", &EnginesDownError{Output: res.Output, Role: role}
	}

	d.clearDeferrals(change, role)
	if retriedPtr != nil && *retriedPtr != nil {
		*retriedPtr = nil
		d.markRetryTold(ctx, change)
	}

	needsHuman := filepath.Join(wt, "openspec", "changes", change, ".needs-human")
	if fileExists(needsHuman) {
		return "", NewBlockedError("spec_ambiguity", readFileOrEmpty(needsHuman))
	}
	return used, nil
}

// triageAgentOutcome asks agent_outcome why every engine failed without changing anything, unless a
// quota or authentication pattern already explained it.
func (d *Dispatcher) triageAgentOutcome(ctx context.Context, change, phase, role, engines, output string) (string, *evaluator.Verdict) {
	if strings.Contains(output, "is out of quota or over a usage limit") ||
		strings.Contains(output, "has an authentication or provider configuration failure") {
		return evaluator.FallbackAction(AgentOutcomeGate), nil
	}
	state := map[string]any{
		"change":       change,
		"phase":        phase,
		"role":         role,
		"engines":      engines,
		"agent_output": tailLines(output, 80),
	}
	return routeGate(ctx, d.judge, d.store, AgentOutcomeGate, change, state)
}

func (d *Dispatcher) buildPlanPrompt(item BacklogItem, wt, feedback string, retried map[string]any) (string, string) {
	change := item.ChangeName
	goal := sanitizeBacklogText(item.Description)
	if goal == "" {
		goal = sanitizeBacklogText(item.Why)
	}
	if goal == "" {
		goal = change
	}
	_, memBlock := MemoryFingerprintAndPromptBlock(wt, d.capabilities(wt, change))
	rules, _ := assets.ReadTemplate(d.root, "authoring_rules.md")
	rendered, ver := RenderPrompt(d.root, "plan", map[string]string{
		"change":          change,
		"goal":            goal,
		"memory":          memBlock,
		"evaluator_hint":  d.evaluatorHint(),
		"authoring_rules": string(rules),
		"feedback":        d.feedbackBlock(feedback, retried),
	})
	_ = d.saveWorkerPrompt(change, "plan", rendered)
	return rendered, ver
}

func (d *Dispatcher) buildApplyPrompt(item BacklogItem, wt, branch, feedback, rebuild string, retried map[string]any) (string, string) {
	change := item.ChangeName
	_, memBlock := MemoryFingerprintAndPromptBlock(wt, d.capabilities(wt, change))
	rules, _ := assets.ReadTemplate(d.root, "authoring_rules.md")
	rebBlock := ""
	if rebuild != "" {
		rebBlock = "\nThis change is being built again on the current base branch:\n" + rebuild
	}
	rendered, ver := RenderPrompt(d.root, "build", map[string]string{
		"change":          change,
		"branch":          branch,
		"memory":          memBlock,
		"evaluator_hint":  d.evaluatorHint(),
		"authoring_rules": string(rules),
		"feedback":        d.feedbackBlock(feedback, retried),
		"rebuild":         rebBlock,
	})
	_ = d.saveWorkerPrompt(change, "build", rendered)
	return rendered, ver
}

func (d *Dispatcher) evaluatorHint() string {
	if settings.EvaluatorOffForThisRun() || d.judge == nil || !d.judge.Available() {
		return ""
	}
	return fmt.Sprintf(
		"\nWhen a yes/no judgment cannot be settled by running code (for example: does this test assert the THEN "+
			"clause of that scenario?), ask the evaluator, a calibrated judge that answers with a probability instead of text:\n"+
			"`jev noul \"<question>\" --true \"<what yes means>\" --false \"<what no means>\" --state-file <file> --model %s --field noul`\n"+
			"0.7 or more means yes, 0.3 or less means no, anything in between means it cannot tell. Never pass "+
			"secrets, .env files or credentials as state: the state is sent to the TypeSafe API.\n",
		d.judge.Policy().EffectiveModel(),
	)
}

// maxFeedbackBytes bounds the failure quoted in a repair prompt. Every engine command receives the whole
// prompt as one command-line argument, and execve refuses a single argument of 128 KiB on Linux
// (MAX_ARG_STRLEN) and about 1 MiB of arguments and environment on macOS: the engine then never starts
// ("argument list too long"), is retried, and its role ends as an engine outage. The failure is bounded in
// lines only (the last 120 of a gate's output), so one very long line, a minified bundle or a base64 blob
// in a failed assertion, was enough. The rest of a prompt (template, rules, memory) is a few KiB.
const maxFeedbackBytes = 32 << 10

// boundedFeedback returns feedback, or its last maxFeedbackBytes cut at a character boundary: the end of
// a gate's output is what names the failure.
func boundedFeedback(feedback string) string {
	if len(feedback) <= maxFeedbackBytes {
		return feedback
	}
	cut := len(feedback) - maxFeedbackBytes
	for cut < len(feedback) && !utf8.RuneStart(feedback[cut]) {
		cut++
	}
	return fmt.Sprintf("[%d bytes of earlier output omitted]\n%s", cut, feedback[cut:])
}

func (d *Dispatcher) feedbackBlock(feedback string, retried map[string]any) string {
	var block string
	if feedback != "" {
		block = fmt.Sprintf("\nThe previous attempt failed deterministic verification. Fix exactly this:\n```\n%s\n```", boundedFeedback(feedback))
	}
	if retried == nil {
		return block
	}
	return fmt.Sprintf(
		"%s\nThis change was quarantined (%v) and is now retried automatically (retry %v of %v), from where it stopped. What stopped it:\n%sFind the cause instead of repeating what the last attempts did.",
		block, retried["reason"], retried["retry"], retried["limit"],
		FencedCodeBlock(RedactExcerpt(fmt.Sprint(retried["details"]), 80, 4000)),
	)
}

func (d *Dispatcher) recordRepair(ctx context.Context, wt, change string, round repairRound, result *failureInfo) {
	file := filepath.Join(wt, "openspec", "changes", change, "repairs.md")
	var b strings.Builder
	fmt.Fprintf(&b, "\n## %s, %s, %s\n\n", round.Phase, d.nowFunc().Format(time.RFC3339), round.Engine)
	fmt.Fprintf(&b, "Asked to fix (%s):\n\n%s\n", round.Asked.Reason, FencedCodeBlock(RedactExcerpt(round.Asked.Details, 40, 4000)))
	if round.HasChanged {
		if round.Changed == "" {
			b.WriteString("Changed: nothing in the child repositories.\n\n")
		} else {
			fmt.Fprintf(&b, "Changed:\n\n%s\n", FencedCodeBlock(round.Changed))
		}
	}
	if result != nil {
		fmt.Fprintf(&b, "Result: failed again (%s).\n", result.Reason)
	} else {
		b.WriteString("Result: passed.\n")
	}

	if d.store != nil {
		nextReason := ""
		if result != nil {
			nextReason = result.Reason
		}
		// In the worker run of ctx, detached from its cancellation: the round happened.
		_ = d.store.RecordRepairRound(context.WithoutCancel(ctx), store.RepairRoundRecord{
			ChangeName:       change,
			Phase:            round.Phase,
			Engine:           round.Engine,
			ExecID:           round.ExecID,
			AskedReason:      round.Asked.Reason,
			AskedDetails:     RedactExcerpt(round.Asked.Details, 40, 4000),
			AskedGateRoundID: round.Asked.GateRoundID,
			ChangedDiffstat:  round.Changed,
			ResultPassed:     result == nil,
			ResultNextReason: nextReason,
			CreatedAt:        d.nowFunc().Format(time.RFC3339),
		})
	}

	if !fileExists(file) {
		_ = os.MkdirAll(filepath.Dir(file), 0o755)
		_ = os.WriteFile(file, []byte(fmt.Sprintf(repairsHeaderFormat, change)), 0o644)
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err == nil {
		_, _ = f.WriteString(b.String())
		_ = f.Close()
	}
}

func (d *Dispatcher) recordRetry(wt, change string, retried map[string]any) {
	file := filepath.Join(wt, "openspec", "changes", change, "repairs.md")
	if !dirExists(filepath.Dir(file)) {
		return
	}
	heading := fmt.Sprintf("## retry-%v, ", retried["retry"])
	existing := readFileOrEmpty(file)
	if strings.Contains(existing, "\n"+heading) {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s%s, dispatcher\n\n", heading, d.nowFunc().Format(time.RFC3339))
	fmt.Fprintf(&b, "Quarantined (%v) at %v:\n\n%s\n", retried["reason"], retried["blocked_at"], FencedCodeBlock(RedactExcerpt(fmt.Sprint(retried["details"]), 40, 4000)))
	fmt.Fprintf(&b, "Result: retried automatically (%v of %v).\n", retried["retry"], retried["limit"])

	if !fileExists(file) {
		_ = os.WriteFile(file, []byte(fmt.Sprintf(repairsHeaderFormat, change)), 0o644)
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err == nil {
		_, _ = f.WriteString(b.String())
		_ = f.Close()
	}
}

func (d *Dispatcher) deltaSpecs(wt, change string) map[string]string {
	specsDir := filepath.Join(wt, "openspec", "changes", change, "specs")
	files := listSpecFiles(specsDir)
	out := make(map[string]string, len(files))
	for _, f := range files {
		rel, err := filepath.Rel(specsDir, filepath.Dir(f))
		if err == nil {
			out[filepath.ToSlash(rel)] = readFileOrEmpty(f)
		}
	}
	return out
}

func (d *Dispatcher) planState(wt, change string) map[string]any {
	dir := filepath.Join(wt, "openspec", "changes", change)
	deltas := d.deltaSpecs(wt, change)
	canonical := make(map[string]string, len(deltas))
	for capName := range deltas {
		canonical[capName] = readFileOrEmpty(filepath.Join(wt, "openspec", "specs", filepath.FromSlash(capName), "spec.md"))
	}
	return map[string]any{
		"change":          change,
		"proposal":        readFileOrEmpty(filepath.Join(dir, "proposal.md")),
		"design":          readFileOrEmpty(filepath.Join(dir, "design.md")),
		"tasks":           readFileOrEmpty(filepath.Join(dir, "tasks.md")),
		"delta_specs":     deltas,
		"canonical_specs": canonical,
	}
}

func (d *Dispatcher) childChanges(ctx context.Context, wt string) map[string]any {
	wtRepo, err := gitx.Open(wt)
	if err != nil {
		return map[string]any{}
	}
	baseRef := d.resolveBaseRef(ctx, wt)
	fork, err := wtRepo.MergeBase("HEAD", baseRef)
	if err != nil {
		fork = baseRef
	}
	children, _ := wtRepo.ChildPaths()
	out := make(map[string]any)
	includeDiffs := true
	if d.judge != nil && d.judge.Policy().State.IncludeDiffs != nil {
		includeDiffs = *d.judge.Policy().State.IncludeDiffs
	}

	for _, cp := range children {
		childDir := filepath.Join(wt, filepath.FromSlash(cp))
		from, err1 := wtRepo.TreeEntryHash(fork, cp)
		childRepo, err2 := gitx.Open(childDir)
		if err1 != nil || err2 != nil {
			continue
		}
		to, err3 := childRepo.RevParse("HEAD")
		if err3 != nil || from == to {
			continue
		}
		files, _ := childRepo.ChangedFiles(from, to)
		statOut, _ := gitx.RunGit(ctx, childDir, "diff", "--stat", from, to)
		entry := map[string]any{
			"files":    files,
			"diffstat": statOut,
		}
		if includeDiffs {
			diffOut, _ := gitx.RunGit(ctx, childDir, "diff", from, to)
			entry["diff"] = diffOut
		}
		out[cp] = entry
	}
	return out
}

func (d *Dispatcher) childHeads(ctx context.Context, wt string) map[string]string {
	wtRepo, err := gitx.Open(wt)
	if err != nil {
		return nil
	}
	children, _ := wtRepo.ChildPaths()
	out := make(map[string]string, len(children))
	for _, cp := range children {
		if sha, err := gitx.RunGit(ctx, filepath.Join(wt, filepath.FromSlash(cp)), "rev-parse", "HEAD"); err == nil {
			out[cp] = strings.TrimSpace(sha)
		}
	}
	return out
}

func (d *Dispatcher) childDiffstat(ctx context.Context, wt string, heads map[string]string) string {
	var parts []string
	var paths []string
	for cp := range heads {
		paths = append(paths, cp)
	}
	sort.Strings(paths)
	for _, cp := range paths {
		from := heads[cp]
		if from == "" {
			continue
		}
		stat, err := gitx.RunGit(ctx, filepath.Join(wt, filepath.FromSlash(cp)), "diff", "--stat=100", from, "HEAD")
		if err != nil || strings.TrimSpace(stat) == "" {
			continue
		}
		lines := strings.Split(strings.TrimRight(stat, "\n"), "\n")
		if len(lines) > 11 {
			lines = append(lines[:10], " ...", lines[len(lines)-1])
		}
		parts = append(parts, fmt.Sprintf("%s:\n%s", cp, strings.Join(lines, "\n")))
	}
	return strings.Join(parts, "\n")
}

func readFileOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func copyPathRecursive(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, info.Mode())
	}
	if err := os.MkdirAll(dst, info.Mode()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyPathRecursive(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func readBacklogItemsFromFile(path string) []BacklogItem {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var generic any
	if json.Unmarshal(raw, &generic) != nil {
		return nil
	}
	var list []any
	switch v := generic.(type) {
	case map[string]any:
		if chgs, ok := v["changes"].([]any); ok {
			list = chgs
		}
	case []any:
		list = v
	}
	var out []BacklogItem
	for _, el := range list {
		if m, ok := el.(map[string]any); ok {
			out = append(out, BacklogItemFromMap(m))
		}
	}
	return out
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

// parseOpenSpecJSON unmarshals JSON from openspec CLI output, tolerating leading or trailing
// plain-text warnings (including warnings containing unbalanced or non-JSON braces/brackets)
// by extracting balanced JSON object/array candidates.
func parseOpenSpecJSON(raw string, dst any) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return errors.New("empty openspec output")
	}
	firstErr := json.Unmarshal([]byte(trimmed), dst)
	if firstErr == nil {
		return nil
	}
	candidates := extractBalancedJSONCandidates(trimmed)
	for i := len(candidates) - 1; i >= 0; i-- {
		if err := json.Unmarshal([]byte(candidates[i]), dst); err == nil {
			return nil
		}
	}
	return firstErr
}

// extractBalancedJSONCandidates scans raw for balanced {...} and [...] substrings that are valid JSON.
func extractBalancedJSONCandidates(raw string) []string {
	var out []string
	n := len(raw)
	for i := 0; i < n; i++ {
		ch := raw[i]
		if ch != '{' && ch != '[' {
			continue
		}
		if end := findBalancedJSONEnd(raw, i); end > i {
			candidate := raw[i : end+1]
			if json.Valid([]byte(candidate)) {
				out = append(out, candidate)
				i = end
			}
		}
	}
	return out
}

func findBalancedJSONEnd(s string, start int) int {
	var stack []byte
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				return -1
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i
			}
		}
	}
	return -1
}

// lastAgentExecution returns the execution of the agent this dispatcher last invoked for change.
func (d *Dispatcher) lastAgentExecution(change string) string {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	return d.lastExec[change]
}
