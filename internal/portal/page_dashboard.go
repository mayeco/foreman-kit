package portal

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// Dashboard: what the kit is doing at this instant, and what it closed last. / draws it and
// /api/dashboard returns it. It lists facts: the rows that are open, and the latest that closed.
// What waits for a person is counted here and listed in Needs you.

// nextCycle is what the daemon says about its next cycle: a forecast it keeps with its lock, not a
// fact. The portal shows it and works nothing out of it.
type nextCycle struct {
	// Latest is when the cycle starts at the latest: the tick of the daemon.
	Latest string `json:"latest"`
	// Earliest is from when it may start before its tick, "" when it cannot.
	Earliest string `json:"earliest,omitempty"`
	// IntervalSeconds is the interval of the daemon; IdleDelaySeconds, how long after a cycle began
	// the next one may start early (0: only on its tick).
	IntervalSeconds  int `json:"interval_seconds"`
	IdleDelaySeconds int `json:"idle_delay_seconds,omitempty"`
	// Waits is the daemon's word for what keeps the cycle from starting before its tick.
	Waits string `json:"waits,omitempty"`

	// EarliestPassed says the instant from which it may start early is behind the page's.
	EarliestPassed bool `json:"-"`
}

// waitingCounts says how many things of each kind wait for a person.
type waitingCounts struct {
	Total       int  `json:"total"`
	Urgent      bool `json:"urgent"`
	Paused      bool `json:"paused"`
	Blocked     int  `json:"blocked"`
	Questions   int  `json:"questions"`
	BrokenBases int  `json:"broken_bases"`
	LostRuns    int  `json:"lost_runs"`
	Proposals   int  `json:"proposals"`
}

// runningRun is an open worker run as the page draws it.
type runningRun struct {
	store.RunningRun
	// Name is what the run is on now ("BUILD", "PLAN", "BUILD repair 1"), "" between two executions.
	Name string
	Idle *store.IdleAgent
}

// EnginesTried lists the engines that worked on the execution the run is on.
func (r runningRun) EnginesTried() []string {
	if r.Execution == nil {
		return nil
	}
	return executionRow{ExecutionDetail: *r.Execution}.EnginesTried()
}

// stage is one stage of the pipeline with how many changes are in it.
type stage struct {
	State string
	Label string
	Icon  string
	Count int
}

// dashboardPage is what the kit is doing.
type dashboardPage struct {
	store.Dashboard
	// NextCycle is nil when no daemon runs, or when the one that does has said nothing yet.
	NextCycle *nextCycle    `json:"next_cycle,omitempty"`
	Waiting   waitingCounts `json:"waiting"`
	// ConfigWaits lists the processes of the kit that start nothing new because of a configuration
	// file they cannot use, each as the process itself says it. It stops them the way a pause does,
	// and nothing on record says it: the kit works it out from the files.
	ConfigWaits []ProcessWait `json:"config_waits"`
	// BacklogProblem says what keeps the file of the backlog from being read, "" when nothing does.
	// A backlog that is missing reads as an empty queue: nothing is dispatched, and nothing says why.
	BacklogProblem string `json:"backlog_problem"`
	BacklogPath    string `json:"-"`
	// QueuedUnlisted lists the changes a cycle queued that the backlog of the main checkout has never
	// listed. They are in no state of the pipeline, like a change taken out of the backlog, and the
	// page counts them apart: for the time it takes the main checkout to list them, nothing is wrong.
	QueuedUnlisted []store.UnlistedQueued `json:"queued_unlisted"`
	// UnlistedHref leads to the cycle that queued them when one cycle queued them all, else to the
	// list of the changes that are in no state of the pipeline.
	UnlistedHref string `json:"-"`
	// DaemonGiven lists what the daemon that runs now was started with `foreman-kit --set`, each as
	// "<file>.<key>=<value>": values that are in no file and hold until that daemon stops. Settings
	// shows what the files say, so the first page says what the daemon reads instead.
	DaemonGiven []string `json:"daemon_given"`

	// What the page draws from the same facts.
	Rail   []stage        `json:"-"`
	Aside  []stage        `json:"-"`
	Open   []runningRun   `json:"-"`
	Others []executionRow `json:"-"`
	// CycleRun is the execution of the cycle that runs now: the page draws it with its cycle, once.
	CycleRun *executionRow `json:"-"`
}

// Busy reports whether anything runs now.
func (p *dashboardPage) Busy() bool {
	return len(p.Runs) > 0 || p.Merge != nil || p.Cycle != nil || len(p.Executions) > 0
}

func (p *dashboardPage) head(v *pageView) {
	v.Title, v.Section = "Dashboard", "/"
	v.Live = 8
}

// The stages of the pipeline, in the order a change goes through them, and the states outside it.
var (
	railStates = []stage{
		{State: store.ChangeQueued, Label: "Queued", Icon: "inbox"},
		{State: store.ChangeBuilding, Label: "Building", Icon: "hammer"},
		{State: store.ChangeReady, Label: "Ready", Icon: "package-check"},
		{State: store.ChangeMerging, Label: "Merging", Icon: "git-merge"},
		{State: store.ChangeIntegrated, Label: "Integrated", Icon: "archive"},
	}
	asideStates = []stage{
		{State: store.ChangeBlocked, Icon: "octagon-alert"},
		{State: store.ChangeRebuildRequested, Icon: "rotate-ccw"},
		{State: store.ChangeDiscarded, Icon: "trash-2"},
		{State: store.ChangeUnknown, Icon: "circle-help"},
	}
)

// daemonGiven returns what the daemon that runs now was started with `foreman-kit --set`: values
// that are in no file and hold until it stops. The list is empty when no daemon runs, and when the
// one that does was given nothing. The daemon keeps it with its lock, outside the snapshot of a
// page, like its forecast; how the lock records it is for the kit to know.
func (s *Server) daemonGiven(ctx context.Context) ([]string, error) {
	lock, err := s.store.LiveLock(ctx, "daemon")
	if err != nil || lock == nil {
		return []string{}, err
	}
	if given := cycle.GivenToDaemon(lock.MetadataJSON); len(given) > 0 {
		return given, nil
	}
	return []string{}, nil
}

func (s *Server) readDashboard(ctx context.Context, db *store.View, _ *http.Request) (*dashboardPage, error) {
	now, err := db.Dashboard(ctx, 12)
	if err != nil {
		return nil, err
	}
	w, err := s.readWaiting(ctx, db)
	if err != nil {
		return nil, err
	}
	p := &dashboardPage{Dashboard: now, ConfigWaits: s.work.waits(), BacklogProblem: s.work.BacklogProblem(), BacklogPath: cycle.BacklogFile, Waiting: waitingCounts{
		Total: w.Count(), Urgent: w.Urgent(), Paused: w.Pause != nil,
		Blocked: len(w.Blocked), Questions: len(w.unblockedQuestions()), BrokenBases: len(w.BrokenBases),
		LostRuns: len(w.LostRuns), Proposals: len(w.Proposals),
	}}

	// The daemon keeps its forecast with its lock, outside the snapshot of the page: it is what the
	// daemon said last, and the page says so.
	sched, err := s.store.CycleSchedule(ctx)
	if err != nil {
		return nil, err
	}
	if p.DaemonGiven, err = s.daemonGiven(ctx); err != nil {
		return nil, err
	}
	if sched != nil && !sched.Latest.IsZero() {
		p.NextCycle = &nextCycle{
			Latest:           sched.Latest.UTC().Format(time.RFC3339),
			IntervalSeconds:  int(sched.Interval / time.Second),
			IdleDelaySeconds: int(sched.IdleDelay / time.Second),
			Waits:            sched.Waits,
		}
		if !sched.Earliest.IsZero() {
			p.NextCycle.Earliest = sched.Earliest.UTC().Format(time.RFC3339)
			p.NextCycle.EarliestPassed = !sched.Earliest.After(db.At())
		}
	}

	for _, st := range railStates {
		st.Count = now.Counts[st.State]
		p.Rail = append(p.Rail, st)
	}
	if p.QueuedUnlisted, err = db.ListUnlistedQueued(ctx, store.MaxPageSize); err != nil {
		return nil, err
	}
	p.UnlistedHref = "/changes?state=" + store.ChangeUnknown
	if n := len(p.QueuedUnlisted); n > 0 && p.QueuedUnlisted[0].CycleStamp == p.QueuedUnlisted[n-1].CycleStamp {
		// The list is ordered by cycle: the same one at both ends is the same one throughout.
		p.UnlistedHref = "/cycles/" + url.PathEscape(p.QueuedUnlisted[0].CycleStamp)
	}
	for _, st := range asideStates {
		st.Count = now.Counts[st.State]
		if st.State == store.ChangeUnknown {
			// What a cycle queued is said on its own, below.
			st.Count = max(0, st.Count-len(p.QueuedUnlisted))
		}
		if st.Count > 0 {
			st.Label = wordingOf(vocChangeState, st.State).Text
			p.Aside = append(p.Aside, st)
		}
	}
	idle := map[string]*store.IdleAgent{}
	for i := range now.Idle {
		idle[now.Idle[i].ExecID] = &now.Idle[i]
	}
	for _, r := range now.Runs {
		row := runningRun{RunningRun: r}
		if r.Execution != nil {
			row.Name = executionName(r.Execution.Execution)
			row.Idle = idle[r.Execution.ExecID]
		}
		p.Open = append(p.Open, row)
	}
	for _, ex := range now.Executions {
		row := executionRow{ExecutionDetail: ex, Name: executionName(ex.Execution), About: executionAbout(ex.Execution), Idle: idle[ex.ExecID]}
		if now.Cycle != nil && ex.Kind == "cycle" && ex.CycleStamp == now.Cycle.Stamp && p.CycleRun == nil {
			p.CycleRun = &row
			continue
		}
		p.Others = append(p.Others, row)
	}
	return p, nil
}
