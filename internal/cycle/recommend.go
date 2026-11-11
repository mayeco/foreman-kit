package cycle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// The kit recommends values for its own settings from what it recorded: the agents and gates a limit
// cut, the retries that helped and the ones that did not, the time workers sat with nothing to start.
// It is arithmetic over rows (store.SettingEvidence), it uses no agent, and it changes nothing: a row
// says which setting, its value now, the value the records argue for, why, and what would be gained.
// A person changes the setting with `foreman-kit config set`, and commits it, as any other change.
//
// Three rules keep a recommendation from doing harm. A rule that lacks the cases it asks for gives no
// row. A recommended value is never more than double nor less than half the value now: the next
// stretch says whether that was enough. And nothing is recommended before the pipeline has run for
// recommendMinimumHistory.

// The thresholds of the evaluation. They are starting values, fixed here rather than settings: the
// doc of the on-demand cycle says to review them with a month of records.
const (
	// recommendMinimumHistory is how long a dispatcher must have run, in all, before the kit
	// recommends anything.
	recommendMinimumHistory = 24 * time.Hour
	// settleAfterFree is how long the dispatcher is given to start what waits once a worker comes
	// free: a worker is free with nothing to start only when nothing took its place by then.
	settleAfterFree = 30 * time.Second
	// slotGrace is how soon after a worker finished a run must start to count as one that was
	// waiting for that slot.
	slotGrace = time.Minute

	// The on-demand cycle: the delays compared, the cycles that must have left a worker with
	// nothing to start, and the share of the shortest delay's gain the chosen one must keep.
	idleDelayMinCycles = 5
	idleDelayKeeps     = 0.9

	// A limit is raised by this factor when it cut work that was cut again when repeated, at least
	// limitMinRepeats times.
	limitRaise      = 1.5
	limitMinRepeats = 2

	// The wait before a blocked change is retried is halved when at least blockRetryMin retries ran
	// and this share of them ended built.
	blockRetryMin  = 5
	blockRetryGood = 0.8

	// The retries with the same agent are cut back when at least agentRetryMin ran and fewer than
	// this share of them ended well.
	agentRetryMin  = 10
	agentRetryGood = 0.2

	// A worker is added when all of them were busy for this share of the time and at least
	// workersMinStarts runs started as soon as one came free.
	workersBusyShare = 0.5
	workersMinStarts = 10

	// The idle limit is kept a multiple of this.
	idleLimitStep = 5 * time.Minute
)

// idleDelayCandidates are the delays of the on-demand cycle the evaluation compares, in minutes.
var idleDelayCandidates = []int{10, 15, 20, 30}

// RecommendWindow is a stretch the recommendations can be asked over, ending now.
type RecommendWindow struct {
	Name   string        `json:"name"`
	Length time.Duration `json:"-"`
	// Available says the kit has records from before the stretch begins.
	Available bool `json:"available"`
}

// RecommendWindows lists the stretches Recommend can be asked over, the default first, by name and
// length. Whether the kit has records from before each one is for Recommend to say.
func RecommendWindows() []RecommendWindow {
	return recommendWindows()
}

// recommendWindows lists the stretches, the default first.
func recommendWindows() []RecommendWindow {
	day := 24 * time.Hour
	return []RecommendWindow{{Name: "24h", Length: day}, {Name: "7d", Length: 7 * day}, {Name: "15d", Length: 15 * day}, {Name: "30d", Length: 30 * day}}
}

// Recommendation is one row: a setting the records argue for changing.
type Recommendation struct {
	// Key is the setting as `foreman-kit config set` takes it, Option as its file writes it, and
	// File that file.
	Key    string `json:"key"`
	Option string `json:"option"`
	File   string `json:"file"`
	// Current is the value in effect and Recommended the one the records argue for, both written
	// the way the file holds them (20m, 3).
	Current     string `json:"current"`
	Recommended string `json:"recommended"`
	// Why says what the kit saw, and Improvement what the change would have gained in the stretch.
	Why         string `json:"why"`
	Improvement string `json:"improvement"`
	// Command is how a person applies it.
	Command string `json:"command"`
}

// SettingMeasure is what the evaluation saw for one setting, whether or not it recommends anything.
type SettingMeasure struct {
	Key    string `json:"key"`
	Option string `json:"option"`
	Seen   string `json:"seen"`
}

// Recommendations is the answer of the evaluation over one stretch.
type Recommendations struct {
	Window string    `json:"window"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	// HistoryHours is how long a dispatcher has run on record, and MinimumHours what the kit asks
	// for before it recommends anything. Ready says the first reaches the second: without it Rows
	// is empty, whatever the records say.
	HistoryHours float64 `json:"history_hours"`
	MinimumHours float64 `json:"minimum_hours"`
	Ready        bool    `json:"ready"`
	// Windows are the stretches that can be asked for.
	Windows  []RecommendWindow `json:"windows"`
	Rows     []Recommendation  `json:"rows"`
	Measures []SettingMeasure  `json:"measures"`
}

// readSettingEvidence reads the evidence of a stretch. It is a variable so that a test can give the
// evaluation a history the store's clock would not let it write.
var readSettingEvidence = func(ctx context.Context, st *store.Store, from, to time.Time) (store.SettingEvidence, error) {
	return st.SettingEvidence(ctx, from, to)
}

// Recommend evaluates the settings of the application at root over the stretch called window ("24h",
// "7d", "15d", "30d"; "": the last 24 hours) that ends now.
func Recommend(ctx context.Context, root string, st *store.Store, window string) (Recommendations, error) {
	windows := recommendWindows()
	chosen := windows[0]
	if window != "" {
		found := false
		for _, w := range windows {
			if w.Name == window {
				chosen, found = w, true
			}
		}
		if !found {
			var names []string
			for _, w := range windows {
				names = append(names, w.Name)
			}
			return Recommendations{}, fmt.Errorf("no window %q: it is one of %s", window, strings.Join(names, ", "))
		}
	}
	if st == nil {
		return Recommendations{}, fmt.Errorf("the recommendations need the state database")
	}
	now := time.Now().UTC()
	ev, err := readSettingEvidence(ctx, st, now.Add(-chosen.Length), now)
	if err != nil {
		return Recommendations{}, err
	}
	// The values in effect are read without the notes about values that cannot be used: those are
	// `config check`'s to give.
	out := evaluateSettings(&settings.Reader{Root: root, Stderr: io.Discard}, ev)
	out.Window = chosen.Name
	for i := range windows {
		windows[i].Available = !ev.FirstDispatch.IsZero() && !ev.FirstDispatch.After(ev.To.Add(-windows[i].Length))
	}
	out.Windows = windows
	return out, nil
}

// ruleResult is what one rule says of its setting: what it saw, always, and, when the records argue
// for another value, that value with the reason and the gain.
type ruleResult struct {
	Key settings.Key
	// Current and Recommended count Unit: minutes for the wait of the cycle, seconds for a limit.
	// A zero Unit is a plain count (workers, retries).
	Current, Recommended int
	Unit                 time.Duration
	Seen                 string
	Why, Improvement     string
}

// written returns n of the rule's unit the way the file of its key holds the value: a time with its
// unit, never where the key says so for none, or the number itself.
func (res ruleResult) written(n int) string {
	if res.Unit <= 0 {
		return strconv.Itoa(n)
	}
	if n <= 0 && settings.Of(res.Key).Never {
		return settings.Never
	}
	return settings.FormatDuration(time.Duration(n) * res.Unit)
}

// evaluateSettings applies every rule to the evidence of a stretch, with the values r reads as the
// ones in effect.
func evaluateSettings(r *settings.Reader, ev store.SettingEvidence) Recommendations {
	out := Recommendations{
		From: ev.From, To: ev.To,
		HistoryHours: ev.Dispatched.Hours(), MinimumHours: recommendMinimumHistory.Hours(),
		Ready: ev.Dispatched >= recommendMinimumHistory,
		Rows:  []Recommendation{}, Measures: []SettingMeasure{},
	}
	facts := newPipelineFacts(ev, r.Int(settings.Workers))
	for _, res := range []ruleResult{
		ruleIdleDelay(r, ev, facts),
		ruleIdleLimit(r, ev),
		ruleLimit(settings.RoleTimeout("plan"), r, ev.AgentTimeoutsByRole["plan"], "PLAN call", "the next PLAN call on the same change"),
		ruleLimit(settings.RoleTimeout("build"), r, ev.AgentTimeoutsByRole["build"], "BUILD call", "the next BUILD call on the same change"),
		ruleLimit(settings.GateTimeout, r, ev.GateTimeouts, "gate", "the next run of the same gate for the same change"),
		ruleBlockRetry(r, ev),
		ruleAgentRetries(r, ev),
		ruleWorkers(r, ev, facts),
	} {
		out.Measures = append(out.Measures, SettingMeasure{Key: string(res.Key), Option: res.Key.Path(), Seen: res.Seen})
		if !out.Ready || res.Why == "" || res.Recommended == res.Current {
			continue
		}
		out.Rows = append(out.Rows, Recommendation{
			Key: string(res.Key), Option: res.Key.Path(), File: res.Key.FilePath(),
			Current: res.written(res.Current), Recommended: res.written(res.Recommended), Why: res.Why, Improvement: res.Improvement,
			Command: fmt.Sprintf("foreman-kit config set %s %s", res.Key, res.written(res.Recommended)),
		})
	}
	return out
}

// oneStep keeps a recommended value within one step of the value now: at most its double, at least
// its half (rounded up, so that a step never undoes itself).
func oneStep(current, recommended int) int {
	if current <= 0 {
		return recommended
	}
	if recommended > 2*current {
		return 2 * current
	}
	if half := (current + 1) / 2; recommended < half {
		return half
	}
	return recommended
}

func minutesOf(d time.Duration) int { return int((d + 30*time.Second) / time.Minute) }

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// ---- What the workers had to do

// pipelineFacts answers, from the records, what the dispatcher knew at an instant: how many workers
// it had, how many were building, and whether it was passing at all.
type pipelineFacts struct {
	ev      store.SettingEvidence
	workers int // the setting in effect, for an instant no dispatcher run covers
	// starts and ends are the instants the worker runs began and ended, each sorted. A run that
	// ended held is among them: it kept a worker for as long as it lasted, a few seconds when the
	// change waited behind another from the start, minutes when planning it showed that it would.
	starts, ends []time.Time
}

func newPipelineFacts(ev store.SettingEvidence, workers int) *pipelineFacts {
	f := &pipelineFacts{ev: ev, workers: workers}
	for _, run := range ev.Runs {
		end := run.FinishedAt
		if end.IsZero() {
			end = ev.To
		}
		f.starts = append(f.starts, run.StartedAt)
		f.ends = append(f.ends, end)
	}
	sort.Slice(f.starts, func(i, j int) bool { return f.starts[i].Before(f.starts[j]) })
	sort.Slice(f.ends, func(i, j int) bool { return f.ends[i].Before(f.ends[j]) })
	return f
}

// running is how many workers had a run in progress at t.
func (f *pipelineFacts) running(t time.Time) int {
	started := sort.Search(len(f.starts), func(i int) bool { return f.starts[i].After(t) })
	ended := sort.Search(len(f.ends), func(i int) bool { return f.ends[i].After(t) })
	return started - ended
}

// dispatcherAt returns the dispatcher run that was passing at t.
func (f *pipelineFacts) dispatcherAt(t time.Time) (store.EvidenceDispatcher, bool) {
	for i := len(f.ev.Dispatchers) - 1; i >= 0; i-- {
		d := f.ev.Dispatchers[i]
		if !d.StartedAt.After(t) && d.EndedAt.After(t) {
			return d, true
		}
	}
	return store.EvidenceDispatcher{}, false
}

// reportingAt reports whether a continuous dispatcher, the one that tells the daemon what each of its
// passes left, was passing at t.
func (f *pipelineFacts) reportingAt(t time.Time) bool {
	d, ok := f.dispatcherAt(t)
	return ok && d.Mode == "continuous"
}

// workersAt is how many workers the dispatcher had at t.
func (f *pipelineFacts) workersAt(t time.Time) int {
	if d, ok := f.dispatcherAt(t); ok && d.Workers > 0 {
		return d.Workers
	}
	return f.workers
}

func covered(spans []store.EvidenceSpan, t time.Time) bool {
	for _, s := range spans {
		if !s.From.After(t) && (s.To.IsZero() || s.To.After(t)) {
			return true
		}
	}
	return false
}

// outOfWork reports whether a worker was free at t with nothing to start, as far as the records tell:
// fewer runs than workers, nothing started in its place by the time the dispatcher had passed
// (never looking past until), no pause, and no change waiting for an engine to come back.
func (f *pipelineFacts) outOfWork(t, until time.Time) bool {
	later := t.Add(settleAfterFree)
	if later.After(until) {
		later = until
	}
	w := f.workersAt(t)
	return f.running(t) < w && f.running(later) < w && !covered(f.ev.Pauses, t) && !covered(f.ev.Deferrals, t)
}

// ---- cycle.every_when_out_of_work

// cycleReplay is one cycle as the on-demand rule would have seen it, taken on its own: from its
// start to the start of the cycle after it, with the tick it would have waited for.
type cycleReplay struct {
	facts              *pipelineFacts
	cycle              store.EvidenceCycle
	start, queued, end time.Time
	tick               time.Time
	// instants are the moments at which what the rule looks at can change, sorted.
	instants []time.Time
}

// free returns the first instant, once the cycle had queued its work, at which a worker was free
// with nothing to start before the next cycle began.
func (c cycleReplay) free() (time.Time, bool) {
	for _, t := range c.instants {
		if t.Before(c.queued) || !t.Before(c.end) {
			continue
		}
		if c.facts.outOfWork(t, c.end) {
			return t, true
		}
	}
	return time.Time{}, false
}

// advanceAt returns when the next cycle would have started before its tick with the delay given, by
// the rule the daemon uses (decideAdvance), and whether it would have at all.
func (c cycleReplay) advanceAt(delay time.Duration) (time.Time, bool) {
	notBefore := c.start.Add(delay)
	candidates := []time.Time{notBefore}
	for _, t := range c.instants {
		if t.After(notBefore) {
			candidates = append(candidates, t)
		}
	}
	for _, at := range candidates {
		if !at.Before(c.tick) {
			break
		}
		// Past the start of the cycle that did follow, the records are about that cycle: the state
		// this one left is what it would have gone on with.
		probe := at
		if !probe.Before(c.end) {
			probe = c.end.Add(-time.Millisecond)
		}
		workers := c.facts.workersAt(probe)
		report := DispatchReport{Workers: workers, Running: workers, At: at}
		if c.facts.outOfWork(probe, c.end) {
			report.Running = workers - 1
		}
		in := advanceInput{
			Now: at, IdleDelay: delay, LastStart: c.start, QueuedAt: c.queued,
			Report: report, HaveReport: c.facts.reportingAt(probe),
			QueueFull: c.cycle.QueueFull,
			Moved:     c.cycle.PreviousMoved || (!c.cycle.FirstMove.IsZero() && !c.cycle.FirstMove.After(probe)),
		}
		if decideAdvance(in).State == advanceNow {
			return at, true
		}
	}
	return time.Time{}, false
}

// replayCycles returns the cycles of the evidence the rule can be replayed over: the ones that
// ended, and whose stretch up to the next cycle, or up to their tick, is all on record.
func replayCycles(ev store.SettingEvidence, facts *pipelineFacts, interval time.Duration) []cycleReplay {
	var out []cycleReplay
	for i, cy := range ev.Cycles {
		if cy.FinishedAt.IsZero() {
			continue
		}
		c := cycleReplay{facts: facts, cycle: cy, start: cy.StartedAt, queued: cy.FinishedAt, end: cy.NextStart, tick: cy.StartedAt.Add(interval)}
		if c.end.IsZero() {
			if c.tick.After(ev.To) {
				continue // the latest cycle, whose hour is not over
			}
			c.end = ev.To
		}
		// A cycle found the queue full when the one after it says so: an early cycle would have
		// counted the same changes.
		c.cycle.QueueFull = i+1 < len(ev.Cycles) && ev.Cycles[i+1].QueueFull

		seen := map[int64]bool{}
		add := func(t time.Time) {
			if t.IsZero() || t.Before(c.start) || !t.Before(c.end) || seen[t.UnixMilli()] {
				return
			}
			seen[t.UnixMilli()] = true
			c.instants = append(c.instants, t)
		}
		add(c.queued)
		add(cy.FirstMove)
		// The runs that ended during the cycle: each end may have left a worker free.
		first := sort.Search(len(facts.ends), func(i int) bool { return !facts.ends[i].Before(c.start) })
		for _, t := range facts.ends[first:] {
			if !t.Before(c.end) {
				break
			}
			add(t)
		}
		for _, s := range ev.Pauses {
			add(s.To)
		}
		for _, s := range ev.Deferrals {
			add(s.To)
		}
		sort.Slice(c.instants, func(a, b int) bool { return c.instants[a].Before(c.instants[b]) })
		out = append(out, c)
	}
	return out
}

func ruleIdleDelay(r *settings.Reader, ev store.SettingEvidence, facts *pipelineFacts) (res ruleResult) {
	res.Key, res.Unit = settings.CycleEveryWhenOutOfWork, time.Minute
	current := int(r.Duration(res.Key) / time.Minute)
	res.Current, res.Recommended = current, current
	interval := r.Duration(settings.CycleEvery)
	intervalMinutes := int(interval / time.Minute)

	cycles := replayCycles(ev, facts, interval)
	free := 0
	for _, c := range cycles {
		if _, ok := c.free(); ok {
			free++
		}
	}
	// removed is what each delay gains over the timer: from when the next cycle would have started
	// with it to the tick it would otherwise have waited for, cycle by cycle. In a stretch that ran
	// on the timer alone that is the waiting an earlier cycle removes. In one that already ran with
	// early cycles the ticks of consecutive cycles overlap, and the sum is how much longer the same
	// cycles would have taken on the timer: it may exceed the stretch.
	removed := func(minutes int) time.Duration {
		var total time.Duration
		for _, c := range cycles {
			if at, ok := c.advanceAt(time.Duration(minutes) * time.Minute); ok {
				total += c.tick.Sub(at)
			}
		}
		return total
	}
	var candidates []int
	var parts []string
	gain := map[int]time.Duration{}
	for _, minutes := range idleDelayCandidates {
		if minutes >= intervalMinutes {
			continue
		}
		candidates = append(candidates, minutes)
		gain[minutes] = removed(minutes)
		parts = append(parts, fmt.Sprintf("%d min with a delay of %d", minutesOf(gain[minutes]), minutes))
	}
	res.Seen = fmt.Sprintf("%s on record that ended; a worker was free with nothing to start before the next one in %d", plural(len(cycles), "cycle", "cycles"), free)
	if len(parts) > 0 {
		res.Seen += "; how much sooner than its timer the next cycle would start, added up: " + strings.Join(parts, ", ")
	}
	if free < idleDelayMinCycles || len(candidates) == 0 || gain[candidates[0]] <= 0 {
		return res
	}
	// The longest delay that keeps most of what the shortest one removes: it costs fewer judgments.
	best := candidates[0]
	for _, minutes := range candidates {
		if float64(gain[minutes]) >= idleDelayKeeps*float64(gain[candidates[0]]) {
			best = minutes
		}
	}
	recommended := oneStep(current, best)
	if recommended == current {
		return res
	}
	res.Recommended = recommended
	if _, known := gain[recommended]; !known {
		gain[recommended] = removed(recommended)
	}
	perHour := func(minutes int) int {
		if minutes <= 0 || minutes > intervalMinutes {
			minutes = intervalMinutes
		}
		return 60 / minutes
	}
	res.Why = fmt.Sprintf("in %d of %s a worker was free with nothing to start before the next cycle began", free, plural(len(cycles), "cycle", "cycles"))
	switch {
	case current <= 0:
		res.Improvement = fmt.Sprintf("%d minutes less waiting in the stretch, with up to %s more an hour",
			minutesOf(gain[recommended]), plural(perHour(recommended)-perHour(0), "judgment", "judgments"))
	case recommended < current:
		res.Improvement = fmt.Sprintf("%d minutes less waiting in the stretch than with %d, with up to %s more an hour",
			minutesOf(gain[recommended]-removed(current)), current, plural(perHour(recommended)-perHour(current), "judgment", "judgments"))
	default:
		res.Improvement = fmt.Sprintf("up to %s fewer an hour, and it keeps %d of the %d minutes that %d gains over the timer",
			plural(perHour(current)-perHour(recommended), "judgment", "judgments"), minutesOf(gain[recommended]), minutesOf(removed(current)), current)
	}
	return res
}

// ---- limits.idle_timeout

func ruleIdleLimit(r *settings.Reader, ev store.SettingEvidence) (res ruleResult) {
	res.Key, res.Unit = settings.IdleTimeout, time.Second
	current := r.Seconds(res.Key)
	res.Current, res.Recommended = current, current
	idle := ev.Idle
	res.Seen = fmt.Sprintf("%s stopped for inactivity; %s an agent came back from", plural(idle.Stalled, "agent", "agents"), plural(idle.Resumed, "pause", "pauses"))
	if idle.Resumed > 0 {
		res.Seen += fmt.Sprintf(", the longest %s", idle.LongestResumed.Round(time.Second))
	}
	if current <= 0 {
		res.Seen += "; the limit is off"
		return res
	}
	limit := time.Duration(current) * time.Second
	// The limit that fits: twice the longest pause an agent came back from. A pause is on record
	// from a quarter of the limit on, so when none is, half the limit is still twice the longest.
	fits := limit / 2
	if idle.LongestResumed > 0 {
		fits = 2 * idle.LongestResumed
	}
	fits = ((fits + idleLimitStep - 1) / idleLimitStep) * idleLimitStep
	target := oneStep(current, int(fits/time.Second))
	switch {
	case idle.Stalled > 0 && target < current:
		res.Recommended = target
		res.Why = fmt.Sprintf("%s stopped for inactivity after waiting out the whole limit", plural(idle.Stalled, "agent was", "agents were"))
		if idle.LongestResumed > 0 {
			res.Why += fmt.Sprintf("; the longest pause an agent came back from lasted %s", idle.LongestResumed.Round(time.Second))
		} else {
			res.Why += "; no agent that paused for a quarter of the limit or more came back"
		}
		res.Improvement = fmt.Sprintf("%d minutes of worker time: %s, each %d minutes sooner",
			idle.Stalled*(current-target)/60, plural(idle.Stalled, "stop", "stops"), (current-target)/60)
	case idle.LongestResumed > limit/2 && target > current:
		res.Recommended = target
		res.Why = fmt.Sprintf("an agent paused for %s and went on working: more than half the limit", idle.LongestResumed.Round(time.Second))
		res.Improvement = "no time: it keeps the limit at twice the longest pause an agent came back from, so that one that pauses a little longer is not stopped"
	}
	return res
}

// ---- roles.<role>.timeout and gates.timeout

// ruleLimit raises a hard limit that cut work which was cut again when it was repeated. A hard limit
// is only ever recommended upwards: lowering it risks cutting good work.
func ruleLimit(key settings.Key, r *settings.Reader, cut store.LimitEvidence, what, next string) (res ruleResult) {
	res.Key, res.Unit = key, time.Second
	current := r.Seconds(key)
	res.Current, res.Recommended = current, current
	res.Seen = fmt.Sprintf("%s reached the limit; %s reached it too in %d", plural(cut.Cut, what, what+"s"), next, cut.CutAgain)
	if cut.CutAgain < limitMinRepeats || current <= 0 {
		return res
	}
	res.Recommended = oneStep(current, roundLimit(int(float64(current)*limitRaise)))
	res.Why = fmt.Sprintf("%s reached the limit, and %s reached it too in %d of them", plural(cut.Cut, what, what+"s"), next, cut.CutAgain)
	res.Improvement = fmt.Sprintf("up to %d minutes: the %s that a longer limit could have spared", minutesOf(cut.Repeated), plural(cut.CutAgain, "repetition", "repetitions"))
	return res
}

// roundLimit rounds a limit in seconds up to a time worth writing: whole minutes, and five of them
// from ten minutes on.
func roundLimit(seconds int) int {
	step := 60
	if seconds >= 600 {
		step = 300
	}
	return (seconds + step - 1) / step * step
}

// ---- blocked_changes.retry_after

func ruleBlockRetry(r *settings.Reader, ev store.SettingEvidence) (res ruleResult) {
	res.Key, res.Unit = settings.BlockedRetryAfter, time.Minute
	current := int(r.Duration(res.Key) / time.Minute)
	res.Current, res.Recommended = current, current
	retries := ev.BlockRetries
	res.Seen = fmt.Sprintf("%s of blocked changes ran; %d ended built", plural(retries.Retried, "automatic retry", "automatic retries"), retries.Ready)
	if retries.Retried < blockRetryMin || float64(retries.Ready) < blockRetryGood*float64(retries.Retried) {
		return res
	}
	half := oneStep(current, current/2)
	if current <= 0 || half >= current {
		res.Seen += "; the wait cannot be made shorter"
		return res
	}
	res.Recommended = half
	sooner := time.Duration(current-half) * time.Minute
	res.Why = fmt.Sprintf("%d of %d automatic retries of a blocked change ended built", retries.Ready, retries.Retried)
	res.Improvement = fmt.Sprintf("%s sooner in all: %s, each %s sooner",
		settings.FormatDuration(time.Duration(retries.Ready)*sooner), plural(retries.Ready, "retry that worked", "retries that worked"), settings.FormatDuration(sooner))
	return res
}

// ---- limits.retries

func ruleAgentRetries(r *settings.Reader, ev store.SettingEvidence) (res ruleResult) {
	res.Key = settings.AgentRetries
	current := r.Int(res.Key)
	res.Current, res.Recommended = current, current
	retries := ev.AgentRetries
	res.Seen = fmt.Sprintf("%s with the same agent; %d ended well", plural(retries.Retries, "retry", "retries"), retries.Succeeded)
	if current <= 0 || retries.Retries < agentRetryMin || float64(retries.Succeeded) >= agentRetryGood*float64(retries.Retries) {
		return res
	}
	// Towards none, one step at a time: the next agent of the role takes over sooner. From one
	// retry the only step left is none.
	res.Recommended = 0
	if current > 1 {
		res.Recommended = oneStep(current, 0)
	}
	res.Why = fmt.Sprintf("%d of %d retries with the same agent ended well", retries.Succeeded, retries.Retries)
	res.Improvement = fmt.Sprintf("up to %d minutes: what the retries that failed took, and the waits before them", minutesOf(retries.Failed+retries.Waited))
	return res
}

// ---- dispatch.workers

func ruleWorkers(r *settings.Reader, ev store.SettingEvidence, facts *pipelineFacts) (res ruleResult) {
	res.Key = settings.Workers
	current := r.Int(res.Key)
	res.Current, res.Recommended = current, current

	// Every instant at which the number of workers building, or the dispatcher passing, can change.
	marks := []time.Time{ev.From, ev.To}
	inside := func(t time.Time) bool { return t.After(ev.From) && t.Before(ev.To) }
	for _, t := range facts.starts {
		if inside(t) {
			marks = append(marks, t)
		}
	}
	for _, t := range facts.ends {
		if inside(t) {
			marks = append(marks, t)
		}
	}
	for _, d := range ev.Dispatchers {
		if inside(d.StartedAt) {
			marks = append(marks, d.StartedAt)
		}
		if inside(d.EndedAt) {
			marks = append(marks, d.EndedAt)
		}
	}
	sort.Slice(marks, func(i, j int) bool { return marks[i].Before(marks[j]) })

	// How long a dispatcher passed, how long of it every worker was busy, and where each stretch
	// with every worker busy ended, with when it had begun.
	type stretch struct{ end, since time.Time }
	var dispatched, busy time.Duration
	var full []stretch
	var fullFrom time.Time
	for i := 0; i+1 < len(marks); i++ {
		from, to := marks[i], marks[i+1]
		if !to.After(from) {
			continue
		}
		d, dispatching := facts.dispatcherAt(from)
		if dispatching {
			dispatched += to.Sub(from)
		}
		if !dispatching || d.Workers <= 0 || facts.running(from) < d.Workers {
			fullFrom = time.Time{}
			continue
		}
		busy += to.Sub(from)
		if fullFrom.IsZero() {
			fullFrom = from
		}
		full = append(full, stretch{end: to, since: fullFrom})
	}

	// The runs that started as soon as a worker came free, with every worker busy until then. A
	// run that ended held was waiting for another change, not for a worker.
	starts := 0
	var waited time.Duration
	for _, run := range ev.Runs {
		if run.Result == "held" || run.StartedAt.Before(ev.From) {
			continue
		}
		i := sort.Search(len(full), func(i int) bool { return full[i].end.After(run.StartedAt) }) - 1
		if i < 0 || run.StartedAt.Sub(full[i].end) > slotGrace {
			continue
		}
		starts++
		since := full[i].since
		if run.QueuedAt.After(since) {
			since = run.QueuedAt
		}
		if run.StartedAt.After(since) {
			waited += run.StartedAt.Sub(since)
		}
	}

	share := 0.0
	if dispatched > 0 {
		share = float64(busy) / float64(dispatched)
	}
	res.Seen = fmt.Sprintf("every worker was busy %d%% of the %.1f hours a dispatcher ran; %s started as soon as a worker came free",
		int(share*100+0.5), dispatched.Hours(), plural(starts, "run", "runs"))
	if share < workersBusyShare || starts < workersMinStarts {
		return res
	}
	res.Recommended = oneStep(current, current+1)
	res.Why = fmt.Sprintf("every worker was busy %d%% of the time, and %d runs started as soon as one came free", int(share*100+0.5), starts)
	res.Improvement = fmt.Sprintf("up to %d minutes: how long those %d runs waited for a worker", minutesOf(waited), starts)
	return res
}

// ---- What a cycle does with them

// recommendForCycle evaluates the last 24 hours as a cycle begins, writes what it recommends in the
// cycle's log and returns it for the judgment. It returns nothing, and logs nothing, when the kit has
// too little history, when nothing argues for a change, and when the evaluation fails: a cycle does
// not depend on it.
func recommendForCycle(ctx context.Context, root string, st *store.Store, out io.Writer) []Recommendation {
	if st == nil {
		return nil
	}
	rec, err := Recommend(ctx, root, st, "")
	if err != nil {
		fmt.Fprintf(out, "settings: the recommendations could not be worked out (%v)\n", err)
		return nil
	}
	for _, row := range rec.Rows {
		fmt.Fprintf(out, "settings: the last %s argue for %s %s -> %s (%s; %s)\n", rec.Window, row.Option, row.Current, row.Recommended, row.Why, row.Improvement)
	}
	return rec.Rows
}

// recommendedLine is the "Settings" line of "This cycle": what the kit recommends changing, for the
// judgment to pass on. It is empty when there is nothing.
func recommendedLine(rows []Recommendation) string {
	if len(rows) == 0 {
		return ""
	}
	var parts []string
	for _, row := range rows {
		parts = append(parts, fmt.Sprintf("`%s` in `%s` from %s to %s (%s; it would gain %s; `%s`)", row.Option, row.File, row.Current, row.Recommended, row.Why, row.Improvement, row.Command))
	}
	return "- **Settings**: from what it recorded in the last 24 hours, the kit recommends changing " + strings.Join(parts, "; ") +
		". Never change a setting yourself: list each one in the report under \"Needs a human\", with its reason and its command.\n"
}

// ---- The command

// FormatRecommendations writes the recommendations as `foreman-kit config recommend` prints them.
// With evidence it also says what each rule saw, whether or not it recommends anything.
func FormatRecommendations(rec Recommendations, evidence bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Settings the records argue for changing, last %s (%s to %s)\n", rec.Window,
		rec.From.UTC().Format(time.RFC3339), rec.To.UTC().Format(time.RFC3339))
	for _, w := range rec.Windows {
		if w.Name == rec.Window && !w.Available && rec.Ready {
			b.WriteString("The records do not go back that far: this is what there is of that stretch.\n")
		}
	}
	switch {
	case !rec.Ready:
		fmt.Fprintf(&b, "\nnone yet: a dispatcher has run for %.1f of the %.0f hours the kit asks for before it recommends anything\n", rec.HistoryHours, rec.MinimumHours)
	case len(rec.Rows) == 0:
		b.WriteString("\nnone: nothing on record argues for another value\n")
	}
	for _, row := range rec.Rows {
		fmt.Fprintf(&b, "\n%s (%s): %s -> %s\n", row.Option, row.File, row.Current, row.Recommended)
		fmt.Fprintf(&b, "  why: %s\n", row.Why)
		fmt.Fprintf(&b, "  gain: %s\n", row.Improvement)
		fmt.Fprintf(&b, "  to apply it: %s\n", row.Command)
	}
	if len(rec.Rows) > 0 {
		b.WriteString("\nNothing is changed for you: apply what you agree with, then commit and push it.\n")
	}
	if evidence {
		b.WriteString("\nWhat each rule saw:\n")
		for _, m := range rec.Measures {
			fmt.Fprintf(&b, "  %s: %s\n", m.Option, m.Seen)
		}
	}
	return b.String()
}

// RunRecommendCLI implements `foreman-kit config recommend [--window 24h|7d|15d|30d] [--evidence] [--json]`.
func RunRecommendCLI(ctx context.Context, root string, st *store.Store, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	const usage = "Usage: foreman-kit config recommend [--window 24h|7d|15d|30d] [--evidence] [--json]"
	window, evidence, asJSON := "", false, false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--evidence":
			evidence = true
		case args[i] == "--json":
			asJSON = true
		case args[i] == "--window" && i+1 < len(args):
			window = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--window="):
			window = strings.TrimPrefix(args[i], "--window=")
		default:
			fmt.Fprintln(stderr, usage)
			return 2
		}
	}
	rec, err := Recommend(ctx, root, st, window)
	if err != nil {
		fmt.Fprintf(stderr, "config recommend: %v\n", err)
		return 2
	}
	if asJSON {
		b, _ := json.MarshalIndent(rec, "", "  ")
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprint(stdout, FormatRecommendations(rec, evidence))
	return 0
}
