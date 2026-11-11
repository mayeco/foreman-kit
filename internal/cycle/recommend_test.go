package cycle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// recommendSettings returns a reader over settings written for a test: pipeline holds what
// .foreman-kit/pipeline.yaml would and daemon what .foreman-kit/daemon.yaml would; agents, when given, what
// .foreman-kit/agents.yaml would. A key they do not write reads as the value the kit ships.
func recommendSettings(t *testing.T, pipeline, daemon map[string]any, agents ...map[string]any) *settings.Reader {
	t.Helper()
	r := &settings.Reader{Root: t.TempDir(), FilesAlone: true, Stderr: io.Discard}
	r.Use(settings.Workers.File(), pipeline).Use(settings.CycleEvery.File(), daemon).Use(settings.IdleTimeout.File(), nil)
	if len(agents) > 0 {
		r.Use(settings.IdleTimeout.File(), agents[0])
	}
	return r
}

// daemonSettings is .foreman-kit/daemon.yaml with a cycle every interval minutes, and early minutes after
// the last one began when the pipeline is out of work (0: never).
func daemonSettings(interval, early int) map[string]any {
	whenOutOfWork := settings.Never
	if early > 0 {
		whenOutOfWork = fmt.Sprintf("%dm", early)
	}
	return map[string]any{"cycle": map[string]any{"every": fmt.Sprintf("%dm", interval), "every_when_out_of_work": whenOutOfWork}}
}

// measuredEvidence is the window measured in real_test on 2026-11-10 (measuredWindow) as the store
// would hand it over: eight cycles an hour apart under a continuous dispatcher with two workers, and
// the runs that kept those workers busy until the minute each hour's table says one came free.
func measuredEvidence() store.SettingEvidence {
	start := time.Date(2026, 11, 10, 5, 18, 20, 0, time.UTC)
	at := func(hour int, minutes float64) time.Time {
		return start.Add(time.Duration(hour)*time.Hour + time.Duration(minutes*float64(time.Minute)))
	}
	ev := store.SettingEvidence{
		From: start, To: at(len(measuredWindow), 0),
		FirstDispatch: start.Add(-30 * time.Hour), Dispatched: 38 * time.Hour,
		Dispatchers: []store.EvidenceDispatcher{{Mode: "continuous", Workers: 2, StartedAt: start.Add(-time.Hour), EndedAt: at(len(measuredWindow), 60)}},
	}
	for i, h := range measuredWindow {
		c := store.EvidenceCycle{
			Stamp: h.cycle, Trigger: store.CycleTriggerInterval,
			StartedAt: at(i, 0), FinishedAt: at(i, h.queued), PreviousMoved: h.previousMoved,
		}
		if i+1 < len(measuredWindow) {
			c.NextStart = at(i+1, 0)
		}
		if h.moved >= 0 {
			c.FirstMove = at(i, h.moved)
		}
		ev.Cycles = append(ev.Cycles, c)

		// Both workers take what the cycle queued. One is done at the minute a worker came free,
		// the other when the hour's first change moved, if that was later.
		last := h.free
		if h.moved > last {
			last = h.moved
		}
		for _, until := range []float64{h.free, last} {
			if until <= h.queued {
				continue
			}
			result := "stopped"
			if until == h.moved {
				result = "ready"
			}
			ev.Runs = append(ev.Runs, store.EvidenceRun{
				Change: fmt.Sprintf("change-%d-%.0f", i, until*100), Origin: "backlog", Result: result,
				QueuedAt: at(i, h.queued), StartedAt: at(i, h.queued), FinishedAt: at(i, until),
			})
		}
	}
	return ev
}

func rowFor(rec Recommendations, key settings.Key) *Recommendation {
	for i := range rec.Rows {
		if rec.Rows[i].Key == string(key) {
			return &rec.Rows[i]
		}
	}
	return nil
}

func seenFor(rec Recommendations, key settings.Key) string {
	for _, m := range rec.Measures {
		if m.Key == string(key) {
			return m.Seen
		}
	}
	return ""
}

// A stretch that already ran with early cycles, each one 15 minutes after the one before it, is
// still replayed cycle by cycle against the timer each one would otherwise have waited for. The sum
// the evidence shows is then what a delay gains over the timer, not waiting that is still there: it
// exceeds the stretch itself, because the timers of consecutive cycles overlap. It argues for no
// change: the delay in effect is the one the same records would recommend.
func TestAStretchThatAlreadyRanWithEarlyCyclesArguesForNoChange(t *testing.T) {
	start := time.Date(2026, 11, 10, 21, 56, 0, 0, time.UTC)
	at := func(minutes float64) time.Time { return start.Add(time.Duration(minutes * float64(time.Minute))) }
	const cycles = 8
	stretch := at(cycles * 15).Sub(start)
	ev := store.SettingEvidence{
		From: start, To: at(cycles * 15),
		FirstDispatch: start.Add(-30 * time.Hour), Dispatched: 32 * time.Hour,
		Dispatchers: []store.EvidenceDispatcher{{Mode: "continuous", Workers: 2, StartedAt: start.Add(-time.Hour), EndedAt: at(cycles*15 + 60)}},
	}
	for i := 0; i < cycles; i++ {
		begin := float64(i * 15)
		c := store.EvidenceCycle{
			Stamp: fmt.Sprintf("early-%d", i), Trigger: store.CycleTriggerOutOfWork,
			StartedAt: at(begin), FinishedAt: at(begin + 3), FirstMove: at(begin + 12), PreviousMoved: true,
		}
		if i+1 < cycles {
			c.NextStart = at(begin + 15)
		}
		ev.Cycles = append(ev.Cycles, c)
		// Both workers build what the cycle queued, and are done twelve minutes after it began.
		for w := 0; w < 2; w++ {
			ev.Runs = append(ev.Runs, store.EvidenceRun{
				Change: fmt.Sprintf("change-%d-%d", i, w), Origin: "backlog", Result: "ready",
				QueuedAt: at(begin + 3), StartedAt: at(begin + 3), FinishedAt: at(begin + 12),
			})
		}
	}

	rec := evaluateSettings(recommendSettings(t, map[string]any{"dispatch": map[string]any{"workers": 2}}, daemonSettings(60, 15)), ev)
	// The latest cycle is left out: its hour is not over. Each of the other seven gains 48, 45, 40
	// and 30 minutes over its timer with a delay of 10, 15, 20 and 30.
	wantSeen := "7 cycles on record that ended; a worker was free with nothing to start before the next one in 7; " +
		"how much sooner than its timer the next cycle would start, added up: 336 min with a delay of 10, 315 min with a delay of 15, 280 min with a delay of 20, 210 min with a delay of 30"
	if got := seenFor(rec, settings.CycleEveryWhenOutOfWork); got != wantSeen {
		t.Errorf("what the rule saw:\n got %s\nwant %s", got, wantSeen)
	}
	if gained := 315 * time.Minute; gained <= stretch {
		t.Fatalf("the test is meant for a sum longer than its stretch of %s", stretch)
	}
	if row := rowFor(rec, settings.CycleEveryWhenOutOfWork); row != nil {
		t.Errorf("the delay in effect is the one the records argue for, yet the rule recommends: %+v", *row)
	}
}

// Over the window measured in real_test, the evaluation finds what the proposal worked out by hand:
// a delay of 10, 15, 20 and 30 minutes would have removed 373, 350, 315 and 240 minutes of waiting,
// and 15 is the longest that keeps nine tenths of what 10 removes.
func TestRecommendationsOverTheMeasuredWindow(t *testing.T) {
	ev := measuredEvidence()
	pipeline := map[string]any{"dispatch": map[string]any{"workers": 2}}

	rec := evaluateSettings(recommendSettings(t, pipeline, daemonSettings(60, 0)), ev)
	if !rec.Ready {
		t.Fatalf("38 hours of dispatching are enough history: %+v", rec)
	}
	wantSeen := "8 cycles on record that ended; a worker was free with nothing to start before the next one in 8; " +
		"how much sooner than its timer the next cycle would start, added up: 373 min with a delay of 10, 350 min with a delay of 15, 315 min with a delay of 20, 240 min with a delay of 30"
	if got := seenFor(rec, settings.CycleEveryWhenOutOfWork); got != wantSeen {
		t.Errorf("what the rule saw:\n got %s\nwant %s", got, wantSeen)
	}
	row := rowFor(rec, settings.CycleEveryWhenOutOfWork)
	if row == nil || len(rec.Rows) != 1 {
		t.Fatalf("want one row, for the idle delay: %+v", rec.Rows)
	}
	want := Recommendation{
		Key: "daemon.cycle.every_when_out_of_work", Option: "cycle.every_when_out_of_work", File: ".foreman-kit/daemon.yaml",
		Current: "never", Recommended: "15m",
		Why:         "in 8 of 8 cycles a worker was free with nothing to start before the next cycle began",
		Improvement: "350 minutes less waiting in the stretch, with up to 3 judgments more an hour",
		Command:     "foreman-kit config set daemon.cycle.every_when_out_of_work 15m",
	}
	if *row != want {
		t.Errorf("the row:\n got %+v\nwant %+v", *row, want)
	}

	// With the delay the records argue for already set, there is nothing to say.
	if rec := evaluateSettings(recommendSettings(t, pipeline, daemonSettings(60, 15)), ev); len(rec.Rows) != 0 {
		t.Errorf("a delay of 15 is what the window argues for: %+v", rec.Rows)
	}
	// From a shorter delay: fewer judgments for nearly the same gain.
	shorter := rowFor(evaluateSettings(recommendSettings(t, pipeline, daemonSettings(60, 10)), ev), settings.CycleEveryWhenOutOfWork)
	if shorter == nil || shorter.Recommended != "15m" || shorter.Current != "10m" ||
		shorter.Improvement != "up to 2 judgments fewer an hour, and it keeps 350 of the 373 minutes that 10 gains over the timer" {
		t.Errorf("from a delay of 10: %+v", shorter)
	}
	// From a longer one: more of the wait removed.
	longer := rowFor(evaluateSettings(recommendSettings(t, pipeline, daemonSettings(60, 30)), ev), settings.CycleEveryWhenOutOfWork)
	if longer == nil || longer.Recommended != "15m" ||
		longer.Improvement != "110 minutes less waiting in the stretch than with 30, with up to 2 judgments more an hour" {
		t.Errorf("from a delay of 30: %+v", longer)
	}
	// One step at a time: from 5 minutes the recommendation stops at its double.
	if row := rowFor(evaluateSettings(recommendSettings(t, pipeline, daemonSettings(60, 5)), ev), settings.CycleEveryWhenOutOfWork); row == nil || row.Recommended != "10m" {
		t.Errorf("from a delay of 5: %+v", row)
	}
	// A delay is only worth comparing below the interval.
	if rec := evaluateSettings(recommendSettings(t, pipeline, daemonSettings(10, 0)), ev); len(rec.Rows) != 0 {
		t.Errorf("with a cycle every 10 minutes no delay is shorter than the interval: %+v", rec.Rows)
	}
}

// The replay takes what was recorded as it is: a cycle that found the queue full, a paused workspace,
// a change waiting for an engine, or no daemon dispatching, leaves no wait an earlier cycle removes.
func TestIdleDelayReplayUsesOnlyWhatWasRecorded(t *testing.T) {
	pipeline := map[string]any{"dispatch": map[string]any{"workers": 2}}
	settingsOff := func() *settings.Reader { return recommendSettings(t, pipeline, daemonSettings(60, 0)) }
	base := seenFor(evaluateSettings(settingsOff(), measuredEvidence()), settings.CycleEveryWhenOutOfWork)
	if !strings.Contains(base, "350 min with a delay of 15") {
		t.Fatalf("the window as measured: %s", base)
	}

	// Every cycle after the first found the queue full: the cycle before each would not have been
	// followed by an early one. Only the last cycle of the window, with no cycle after it on record
	// to say so, keeps its 35 minutes, which a delay of 20 removes as well as one of 10.
	full := measuredEvidence()
	for i := 1; i < len(full.Cycles); i++ {
		full.Cycles[i].QueueFull = true
	}
	if got := seenFor(evaluateSettings(settingsOff(), full), settings.CycleEveryWhenOutOfWork); !strings.Contains(got, "35 min with a delay of 15") {
		t.Errorf("with the queue full: %s", got)
	}
	if rec := evaluateSettings(settingsOff(), full); len(rec.Rows) != 1 || rec.Rows[0].Recommended != "20m" ||
		rec.Rows[0].Improvement != "35 minutes less waiting in the stretch, with up to 2 judgments more an hour" {
		t.Errorf("with the queue full, the row: %+v", rec.Rows)
	}

	// The workspace was paused through the whole window: nothing was free to start anything.
	paused := measuredEvidence()
	paused.Pauses = []store.EvidenceSpan{{From: paused.From.Add(-time.Hour)}}
	if rec := evaluateSettings(settingsOff(), paused); len(rec.Rows) != 0 || !strings.Contains(seenFor(rec, settings.CycleEveryWhenOutOfWork), "before the next one in 0") {
		t.Errorf("paused: %+v / %s", rec.Rows, seenFor(rec, settings.CycleEveryWhenOutOfWork))
	}

	// A change waited for an engine to come back: that is work the pipeline had.
	deferred := measuredEvidence()
	deferred.Deferrals = []store.EvidenceSpan{{From: deferred.From.Add(-time.Hour)}}
	if rec := evaluateSettings(settingsOff(), deferred); len(rec.Rows) != 0 {
		t.Errorf("with a change deferred all along: %+v", rec.Rows)
	}

	// The cycles ran from a scheduler, each followed by a one-pass dispatch: no daemon would have
	// started a cycle early, however free the workers were.
	cron := measuredEvidence()
	cron.Dispatchers[0].Mode = "once"
	if rec := evaluateSettings(settingsOff(), cron); len(rec.Rows) != 0 || !strings.Contains(seenFor(rec, settings.CycleEveryWhenOutOfWork), "0 min with a delay of 15") {
		t.Errorf("without a daemon: %+v / %s", rec.Rows, seenFor(rec, settings.CycleEveryWhenOutOfWork))
	}

	// Fewer than five cycles with a free worker are not enough to say.
	few := measuredEvidence()
	few.Cycles = few.Cycles[:4]
	few.Cycles[3].NextStart = time.Time{}
	few.To = few.Cycles[3].StartedAt.Add(time.Hour)
	if rec := evaluateSettings(settingsOff(), few); len(rec.Rows) != 0 || !strings.Contains(seenFor(rec, settings.CycleEveryWhenOutOfWork), "4 cycles on record") {
		t.Errorf("four cycles: %+v / %s", rec.Rows, seenFor(rec, settings.CycleEveryWhenOutOfWork))
	}

	// The latest cycle, whose hour is not over, is left out: its workers may yet find work.
	open := measuredEvidence()
	open.To = open.Cycles[7].StartedAt.Add(20 * time.Minute)
	if got := seenFor(evaluateSettings(settingsOff(), open), settings.CycleEveryWhenOutOfWork); !strings.Contains(got, "7 cycles on record") {
		t.Errorf("with the last hour still running: %s", got)
	}
}

// Each of the other rules, on evidence that meets what it asks for and on evidence that falls short.
func TestRecommendationRulesForLimitsAndRetries(t *testing.T) {
	history := 30 * time.Hour
	retryAfter := func(wait string) map[string]any {
		return map[string]any{"dispatch": map[string]any{"workers": 2}, "blocked_changes": map[string]any{"retry_after": wait}}
	}
	limits := func(values map[string]any) map[string]any { return map[string]any{"limits": values} }
	for _, tc := range []struct {
		name     string
		pipeline map[string]any
		agents   map[string]any
		ev       store.SettingEvidence
		key      settings.Key
		want     string // "": no row
		why      string
		gain     string
	}{
		{
			name: "agents stopped for inactivity, and the longest pause one came back from is short",
			ev:   store.SettingEvidence{Idle: store.IdleEvidence{Stalled: 3, Resumed: 5, LongestResumed: 4 * time.Minute}},
			key:  settings.IdleTimeout, want: "10m",
			why:  "3 agents were stopped for inactivity after waiting out the whole limit; the longest pause an agent came back from lasted 4m0s",
			gain: "30 minutes of worker time: 3 stops, each 10 minutes sooner",
		},
		{
			name: "an agent stopped, and no pause long enough to be on record",
			ev:   store.SettingEvidence{Idle: store.IdleEvidence{Stalled: 1}},
			key:  settings.IdleTimeout, want: "10m",
			why:  "1 agent was stopped for inactivity after waiting out the whole limit; no agent that paused for a quarter of the limit or more came back",
			gain: "10 minutes of worker time: 1 stop, each 10 minutes sooner",
		},
		{
			name: "an agent stopped, but another came back from a pause that twice over is the limit",
			ev:   store.SettingEvidence{Idle: store.IdleEvidence{Stalled: 2, Resumed: 1, LongestResumed: 10 * time.Minute}},
			key:  settings.IdleTimeout,
		},
		{
			name: "an agent came back from a pause of more than half the limit",
			ev:   store.SettingEvidence{Idle: store.IdleEvidence{Resumed: 2, LongestResumed: 11*time.Minute + 40*time.Second}},
			key:  settings.IdleTimeout, want: "25m",
			why:  "an agent paused for 11m40s and went on working: more than half the limit",
			gain: "no time: it keeps the limit at twice the longest pause an agent came back from, so that one that pauses a little longer is not stopped",
		},
		{
			name: "pauses, and nobody stopped",
			ev:   store.SettingEvidence{Idle: store.IdleEvidence{Resumed: 9, LongestResumed: 5 * time.Minute}},
			key:  settings.IdleTimeout,
		},
		{
			name:   "the idle limit is off",
			agents: limits(map[string]any{"idle_timeout": "never"}),
			ev:     store.SettingEvidence{Idle: store.IdleEvidence{Stalled: 4}},
			key:    settings.IdleTimeout,
		},
		{
			name: "BUILD calls cut by the limit, and cut again",
			ev:   store.SettingEvidence{AgentTimeoutsByRole: map[string]store.LimitEvidence{"build": {Cut: 5, CutAgain: 2, Repeated: 4 * time.Hour}}},
			key:  settings.RoleTimeout("build"), want: "3h",
			why:  "5 BUILD calls reached the limit, and the next BUILD call on the same change reached it too in 2 of them",
			gain: "up to 240 minutes: the 2 repetitions that a longer limit could have spared",
		},
		{
			name: "PLAN calls cut by their own limit, and cut again",
			ev:   store.SettingEvidence{AgentTimeoutsByRole: map[string]store.LimitEvidence{"plan": {Cut: 3, CutAgain: 2, Repeated: 50 * time.Minute}}},
			key:  settings.RoleTimeout("plan"), want: "45m",
			why:  "3 PLAN calls reached the limit, and the next PLAN call on the same change reached it too in 2 of them",
			gain: "up to 50 minutes: the 2 repetitions that a longer limit could have spared",
		},
		{
			name: "BUILD calls cut by the limit, once repeated",
			ev:   store.SettingEvidence{AgentTimeoutsByRole: map[string]store.LimitEvidence{"build": {Cut: 7, CutAgain: 1, Repeated: 2 * time.Hour}}},
			key:  settings.RoleTimeout("build"),
		},
		{
			name: "gates cut by the limit, and cut again",
			ev:   store.SettingEvidence{GateTimeouts: store.LimitEvidence{Cut: 4, CutAgain: 2, Repeated: 90 * time.Minute}},
			key:  settings.GateTimeout, want: "1h10m", // half as much again as 45m, in whole minutes worth writing
			why:  "4 gates reached the limit, and the next run of the same gate for the same change reached it too in 2 of them",
			gain: "up to 90 minutes: the 2 repetitions that a longer limit could have spared",
		},
		{
			name:     "retries of blocked changes that mostly work",
			pipeline: retryAfter("6h"),
			ev:       store.SettingEvidence{BlockRetries: store.BlockRetryEvidence{Retried: 5, Ready: 4}},
			key:      settings.BlockedRetryAfter, want: "3h",
			why:  "4 of 5 automatic retries of a blocked change ended built",
			gain: "12h sooner in all: 4 retries that worked, each 3h sooner",
		},
		{
			name:     "retries of blocked changes, too few",
			pipeline: retryAfter("6h"),
			ev:       store.SettingEvidence{BlockRetries: store.BlockRetryEvidence{Retried: 4, Ready: 4}},
			key:      settings.BlockedRetryAfter,
		},
		{
			name:     "retries of blocked changes that mostly fail again",
			pipeline: retryAfter("6h"),
			ev:       store.SettingEvidence{BlockRetries: store.BlockRetryEvidence{Retried: 10, Ready: 7}},
			key:      settings.BlockedRetryAfter,
		},
		{
			name: "retries of blocked changes after the hour the kit ships: half an hour is a value now",
			ev:   store.SettingEvidence{BlockRetries: store.BlockRetryEvidence{Retried: 8, Ready: 8}},
			key:  settings.BlockedRetryAfter, want: "30m",
			why:  "8 of 8 automatic retries of a blocked change ended built",
			gain: "4h sooner in all: 8 retries that worked, each 30m sooner",
		},
		{
			name:     "retries of blocked changes that already run at the next dispatch",
			pipeline: retryAfter("0s"),
			ev:       store.SettingEvidence{BlockRetries: store.BlockRetryEvidence{Retried: 8, Ready: 8}},
			key:      settings.BlockedRetryAfter,
		},
		{
			name: "retries with the same agent that almost never work",
			ev:   store.SettingEvidence{AgentRetries: store.AgentRetryEvidence{Retries: 10, Succeeded: 1, Failed: 50 * time.Minute, Waited: 18 * time.Minute}},
			key:  settings.AgentRetries, want: "0",
			why:  "1 of 10 retries with the same agent ended well",
			gain: "up to 68 minutes: what the retries that failed took, and the waits before them",
		},
		{
			name:   "retries with the same agent, from three: one step",
			agents: limits(map[string]any{"retries": 3}),
			ev:     store.SettingEvidence{AgentRetries: store.AgentRetryEvidence{Retries: 12, Succeeded: 0}},
			key:    settings.AgentRetries, want: "2",
			why:  "0 of 12 retries with the same agent ended well",
			gain: "up to 0 minutes: what the retries that failed took, and the waits before them",
		},
		{
			name: "retries with the same agent that work one time in five",
			ev:   store.SettingEvidence{AgentRetries: store.AgentRetryEvidence{Retries: 10, Succeeded: 2}},
			key:  settings.AgentRetries,
		},
	} {
		tc.ev.Dispatched = history
		pipeline := tc.pipeline
		if pipeline == nil {
			pipeline = map[string]any{"dispatch": map[string]any{"workers": 2}}
		}
		rec := evaluateSettings(recommendSettings(t, pipeline, daemonSettings(60, 15), tc.agents), tc.ev)
		row := rowFor(rec, tc.key)
		if tc.want == "" {
			if row != nil || len(rec.Rows) != 0 {
				t.Errorf("%s: want no row, got %+v", tc.name, rec.Rows)
			}
			continue
		}
		if row == nil || len(rec.Rows) != 1 {
			t.Errorf("%s: want one row for %s, got %+v (it saw: %s)", tc.name, tc.key, rec.Rows, seenFor(rec, tc.key))
			continue
		}
		if row.Recommended != tc.want || row.Why != tc.why || row.Improvement != tc.gain {
			t.Errorf("%s:\n got %s, %q, %q\nwant %s, %q, %q", tc.name, row.Recommended, row.Why, row.Improvement, tc.want, tc.why, tc.gain)
		}
		if !strings.HasSuffix(row.Command, " "+tc.want) || row.File != tc.key.FilePath() {
			t.Errorf("%s: the command %q does not set %s in %s", tc.name, row.Command, tc.want, tc.key.FilePath())
		}
	}
}

// A worker is added when every worker was busy at least half the time a dispatcher ran and ten runs
// or more started the moment one came free.
func TestRecommendationForWorkers(t *testing.T) {
	start := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	at := func(minutes float64) time.Time { return start.Add(time.Duration(minutes * float64(time.Minute))) }
	busy := func(queued bool) store.SettingEvidence {
		ev := store.SettingEvidence{
			From: start, To: at(310), Dispatched: 30 * time.Hour,
			Dispatchers: []store.EvidenceDispatcher{{Mode: "continuous", Workers: 2, StartedAt: start, EndedAt: at(310)}},
		}
		// Two workers, each building one change after another for five hours: twelve runs each, of
		// twenty-five minutes, the next one starting ten seconds after the one before it ended.
		for worker := 0; worker < 2; worker++ {
			for i := 0; i < 12; i++ {
				from := float64(i)*25 + float64(worker)*3
				run := store.EvidenceRun{Change: fmt.Sprintf("w%d-%d", worker, i), Origin: "backlog", Result: "ready", StartedAt: at(from + 1.0/6), FinishedAt: at(from + 25)}
				if queued {
					run.QueuedAt = at(from - 15) // it had waited a quarter of an hour for that worker
				}
				ev.Runs = append(ev.Runs, run)
			}
		}
		return ev
	}
	pipeline := map[string]any{"dispatch": map[string]any{"workers": 2}}

	rec := evaluateSettings(recommendSettings(t, pipeline, daemonSettings(60, 15)), busy(true))
	row := rowFor(rec, settings.Workers)
	if row == nil || row.Recommended != "3" || row.Current != "2" {
		t.Fatalf("want a third worker: %+v (it saw: %s)", rec.Rows, seenFor(rec, settings.Workers))
	}
	if row.Why != "every worker was busy 95% of the time, and 22 runs started as soon as one came free" {
		t.Errorf("why: %s", row.Why)
	}
	if !strings.HasPrefix(row.Improvement, "up to ") || !strings.HasSuffix(row.Improvement, " minutes: how long those 22 runs waited for a worker") {
		t.Errorf("gain: %s", row.Improvement)
	}

	// The same runs under a dispatcher that ran eight times as long: the workers were mostly free.
	spread := busy(true)
	spread.To = at(2400)
	spread.Dispatchers[0].EndedAt = at(2400)
	if rec := evaluateSettings(recommendSettings(t, pipeline, daemonSettings(60, 15)), spread); rowFor(rec, settings.Workers) != nil {
		t.Errorf("workers free most of the time: %+v", rec.Rows)
	}

	// Runs that ended held kept their workers busy, but they waited for another change, not for a
	// worker: one more worker would have changed nothing for them.
	held := busy(true)
	for i := range held.Runs {
		held.Runs[i].Result = "held"
	}
	if rec := evaluateSettings(recommendSettings(t, pipeline, daemonSettings(60, 15)), held); rowFor(rec, settings.Workers) != nil ||
		!strings.Contains(seenFor(rec, settings.Workers), "every worker was busy 95% of the 5.2 hours a dispatcher ran; 0 runs started as soon as a worker came free") {
		t.Errorf("held runs: %+v / %s", rec.Rows, seenFor(rec, settings.Workers))
	}
}

// Nothing is recommended before a dispatcher has run for a day, whatever the records say; what each
// rule saw is still there for whoever asks.
func TestNoRecommendationBeforeTheMinimumHistory(t *testing.T) {
	ev := measuredEvidence()
	ev.Dispatched = 23*time.Hour + 59*time.Minute
	rec := evaluateSettings(recommendSettings(t, nil, daemonSettings(60, 0)), ev)
	if rec.Ready || len(rec.Rows) != 0 || rec.MinimumHours != 24 {
		t.Fatalf("with less than a day of dispatching: %+v", rec)
	}
	if len(rec.Measures) != 8 || !strings.Contains(seenFor(rec, settings.CycleEveryWhenOutOfWork), "350 min with a delay of 15") {
		t.Fatalf("what the rules saw is missing: %+v", rec.Measures)
	}
	text := FormatRecommendations(rec, false)
	if !strings.Contains(text, "none yet: a dispatcher has run for 24.0 of the 24 hours the kit asks for before it recommends anything") || strings.Contains(text, "What each rule saw") {
		t.Errorf("the answer before the minimum:\n%s", text)
	}
	if text := FormatRecommendations(rec, true); !strings.Contains(text, "What each rule saw:\n  cycle.every_when_out_of_work: 8 cycles on record") ||
		!strings.Contains(text, "  dispatch.workers: every worker was busy") {
		t.Errorf("the evidence before the minimum:\n%s", text)
	}
}

func TestOneStepKeepsARecommendationNearTheValueNow(t *testing.T) {
	for _, tc := range []struct{ current, asked, want int }{
		{1200, 600, 600}, {1200, 300, 600}, {1200, 3000, 2400}, {1200, 1500, 1500},
		{3, 0, 2}, {2, 0, 1}, {5, 15, 10}, {0, 15, 15}, {7, 3, 4},
	} {
		if got := oneStep(tc.current, tc.asked); got != tc.want {
			t.Errorf("from %d towards %d: got %d, want %d", tc.current, tc.asked, got, tc.want)
		}
	}
}

// `foreman-kit config recommend` prints the rows, says how to apply each, and with --evidence what
// every rule saw.
func TestRecommendCommand(t *testing.T) {
	root := t.TempDir()
	st, err := store.OpenAt(filepath.Join(t.TempDir(), "openspec.db"), "ws-recommend", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app := queueApp{t: t, root: root}
	app.write(settings.CycleEvery.FilePath(), "cycle:\n  every: 60m\n  every_when_out_of_work: never\n")

	realRead := readSettingEvidence
	t.Cleanup(func() { readSettingEvidence = realRead })
	var askedFor time.Duration
	readSettingEvidence = func(ctx context.Context, st *store.Store, from, to time.Time) (store.SettingEvidence, error) {
		askedFor = to.Sub(from)
		ev := measuredEvidence()
		ev.AgentTimeoutsByRole = map[string]store.LimitEvidence{"build": {Cut: 3, CutAgain: 2, Repeated: 3 * time.Hour}}
		return ev, nil
	}
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := RunRecommendCLI(context.Background(), root, st, args, &out, &errOut)
		return code, out.String(), errOut.String()
	}

	code, out, _ := run()
	if code != 0 || askedFor != 24*time.Hour {
		t.Fatalf("config recommend: code %d over %s\n%s", code, askedFor, out)
	}
	for _, want := range []string{
		"Settings the records argue for changing, last 24h (2026-11-10T05:18:20Z to 2026-11-10T13:18:20Z)\n",
		"\ncycle.every_when_out_of_work (.foreman-kit/daemon.yaml): never -> 15m\n",
		"  why: in 8 of 8 cycles a worker was free with nothing to start before the next cycle began\n",
		"  gain: 350 minutes less waiting in the stretch, with up to 3 judgments more an hour\n",
		"  to apply it: foreman-kit config set daemon.cycle.every_when_out_of_work 15m\n",
		"\nroles.build.timeout (.foreman-kit/agents.yaml): 2h -> 3h\n",
		"  to apply it: foreman-kit config set agents.roles.build.timeout 3h\n",
		"\nNothing is changed for you: apply what you agree with, then commit and push it.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "What each rule saw") {
		t.Errorf("the evidence was printed without being asked for:\n%s", out)
	}

	if strings.Contains(out, "The records do not go back that far") {
		t.Errorf("the last 24 hours are all on record:\n%s", out)
	}

	if code, out, _ := run("--window", "7d", "--evidence"); code != 0 || askedFor != 7*24*time.Hour ||
		!strings.Contains(out, "last 7d") || !strings.Contains(out, "What each rule saw:\n") ||
		!strings.Contains(out, "The records do not go back that far: this is what there is of that stretch.\n") ||
		!strings.Contains(out, "  roles.build.timeout: 3 BUILD calls reached the limit; the next BUILD call on the same change reached it too in 2\n") {
		t.Errorf("config recommend --window 7d --evidence: code %d over %s\n%s", code, askedFor, out)
	}

	code, out, _ = run("--window=30d", "--json")
	var rec Recommendations
	if err := json.Unmarshal([]byte(out), &rec); code != 0 || err != nil {
		t.Fatalf("config recommend --json: code %d, %v\n%s", code, err, out)
	}
	if rec.Window != "30d" || !rec.Ready || len(rec.Rows) != 2 || len(rec.Measures) != 8 || rec.Rows[0].Command != "foreman-kit config set daemon.cycle.every_when_out_of_work 15m" {
		t.Errorf("config recommend --json: %+v", rec)
	}
	// The first dispatcher of that history began 30 hours before the stretch ended: only the last
	// 24 hours have records from before they begin.
	available := map[string]bool{}
	for _, w := range rec.Windows {
		available[w.Name] = w.Available
	}
	if fmt.Sprint(available) != "map[15d:false 24h:true 30d:false 7d:false]" {
		t.Errorf("the windows on offer: %v", available)
	}

	if code, _, errOut := run("--window", "2h"); code != 2 || !strings.Contains(errOut, `no window "2h": it is one of 24h, 7d, 15d, 30d`) {
		t.Errorf("an unknown window: code %d, %s", code, errOut)
	}
	if code, _, errOut := run("--apply"); code != 2 || !strings.Contains(errOut, "Usage: foreman-kit config recommend") {
		t.Errorf("an unknown option: code %d, %s", code, errOut)
	}

	// The real read, over a database with no history: nothing yet, and it says how much is missing.
	readSettingEvidence = realRead
	if code, out, errOut := run(); code != 0 || !strings.Contains(out, "none yet: a dispatcher has run for 0.0 of the 24 hours") {
		t.Errorf("over an empty database: code %d\n%s%s", code, out, errOut)
	}
}

// A cycle works out, as it begins, what the last 24 hours argue for changing, writes it in its log and
// tells its judgment, which passes it on in its report. It changes nothing, and says nothing when
// there is nothing to say.
func TestRunCycleLogsTheRecommendationsAndTellsTheJudgment(t *testing.T) {
	a := newQueueApp(t, "")
	daemonFile := settings.CycleEvery.FilePath()
	a.config("daemon", "cycle:\n  every: 60m\n  every_when_out_of_work: never\n")
	st := a.store()
	before, _ := os.ReadFile(filepath.Join(a.root, filepath.FromSlash(daemonFile)))

	// Without a day of dispatching on record the cycle says nothing about the settings.
	var prompt string
	log := runQueueCycle(t, a, st, "20261110T090000Z", func(p string) { prompt = p })
	if strings.Contains(log, "settings:") || strings.Contains(prompt, "**Settings**") {
		t.Fatalf("a cycle with no history recommended something:\n%s\n%s", log, prompt)
	}

	realRead := readSettingEvidence
	t.Cleanup(func() { readSettingEvidence = realRead })
	var askedFor time.Duration
	readSettingEvidence = func(ctx context.Context, st *store.Store, from, to time.Time) (store.SettingEvidence, error) {
		askedFor = to.Sub(from)
		return measuredEvidence(), nil
	}
	log = runQueueCycle(t, a, st, "20261110T100000Z", func(p string) { prompt = p })
	if askedFor != 24*time.Hour {
		t.Fatalf("the cycle asked for the last %s, want the last 24 hours", askedFor)
	}
	wantLog := "settings: the last 24h argue for cycle.every_when_out_of_work never -> 15m (in 8 of 8 cycles a worker was free with nothing to start before the next cycle began; 350 minutes less waiting in the stretch, with up to 3 judgments more an hour)\n"
	if !strings.Contains(log, wantLog) {
		t.Fatalf("expected %q in the cycle log:\n%s", wantLog, log)
	}
	wantPrompt := "- **Settings**: from what it recorded in the last 24 hours, the kit recommends changing `cycle.every_when_out_of_work` in `.foreman-kit/daemon.yaml` from never to 15m " +
		"(in 8 of 8 cycles a worker was free with nothing to start before the next cycle began; it would gain 350 minutes less waiting in the stretch, with up to 3 judgments more an hour; " +
		"`foreman-kit config set daemon.cycle.every_when_out_of_work 15m`). Never change a setting yourself: list each one in the report under \"Needs a human\", with its reason and its command.\n"
	if !strings.Contains(prompt, wantPrompt) {
		t.Fatalf("expected the judgment to be told:\n%s\ngot:\n%s", wantPrompt, prompt)
	}
	if after, _ := os.ReadFile(filepath.Join(a.root, filepath.FromSlash(daemonFile))); string(after) != string(before) {
		t.Fatalf("the cycle changed a setting:\n%s", after)
	}

	// An evaluation that fails does not stop the cycle.
	readSettingEvidence = func(ctx context.Context, st *store.Store, from, to time.Time) (store.SettingEvidence, error) {
		return store.SettingEvidence{}, fmt.Errorf("the database is busy")
	}
	log = runQueueCycle(t, a, st, "20261110T110000Z", func(p string) { prompt = p })
	if !strings.Contains(log, "settings: the recommendations could not be worked out (the database is busy)") || !strings.Contains(log, "== done") || strings.Contains(prompt, "**Settings**") {
		t.Fatalf("a cycle whose evaluation failed:\n%s", log)
	}
}

// The same window, written into a database as the kit would have recorded it and read back through
// the store: `foreman-kit config recommend` finds the delay the proposal worked out by hand.
func TestRecommendOverTheMeasuredWindowAsRecorded(t *testing.T) {
	a := newQueueApp(t, "")
	a.write(settings.CycleEvery.FilePath(), "cycle:\n  every: 60m\n  every_when_out_of_work: never\n")
	st := a.store()
	ctx := context.Background()

	// The store stamps what it writes with the clock: each row is written through it and then
	// placed where the window had it. The window ends an hour ago.
	base := time.Now().UTC().Add(-time.Duration(len(measuredWindow)+1) * time.Hour).Truncate(time.Second)
	at := func(hour int, minutes float64) time.Time {
		return base.Add(time.Duration(hour)*time.Hour + time.Duration(minutes*float64(time.Minute)))
	}
	place := func(query string, args ...any) {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	dispatcher, err := st.BeginDispatcherRun(ctx, "continuous", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDispatcherRun(ctx, dispatcher, "stopped"); err != nil {
		t.Fatal(err)
	}
	place(`UPDATE dispatcher_runs SET started_at = ?, finished_at = ? WHERE id = ?`,
		base.Add(-30*time.Hour).UnixMilli(), at(len(measuredWindow), 30).UnixMilli(), dispatcher)

	for i, h := range measuredWindow {
		stamp := at(i, 0).Format("20060102T150405Z")
		if err := st.BeginCycle(ctx, store.Cycle{Stamp: stamp, Trigger: store.CycleTriggerInterval, StartedAt: at(i, 0).Format(time.RFC3339Nano)}); err != nil {
			t.Fatal(err)
		}
		if err := st.FinishCycle(ctx, stamp, "done", at(i, h.queued).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		last := h.free
		if h.moved > last {
			last = h.moved
		}
		for worker, until := range []float64{h.free, last} {
			if until <= h.queued {
				continue
			}
			result := "stopped"
			if until == h.moved {
				result = "ready"
			}
			change := fmt.Sprintf("hour-%d-worker-%d", i, worker)
			id, err := st.BeginChangeRun(store.WithDispatcherRun(ctx, dispatcher), change, worker+1, "/wt/"+change, "feat/spec-"+change)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.FinishChangeRun(ctx, id, result); err != nil {
				t.Fatal(err)
			}
			place(`UPDATE change_runs SET started_at = ?, finished_at = ? WHERE id = ?`, at(i, h.queued).UnixMilli(), at(i, until).UnixMilli(), id)
		}
	}

	rec, err := Recommend(ctx, a.root, st, "24h")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Ready || rec.HistoryHours < 38 || rec.HistoryHours > 39 {
		t.Fatalf("the history on record: %+v", rec)
	}
	row := rowFor(rec, settings.CycleEveryWhenOutOfWork)
	if row == nil || len(rec.Rows) != 1 || row.Current != "never" || row.Recommended != "15m" ||
		row.Why != "in 8 of 8 cycles a worker was free with nothing to start before the next cycle began" ||
		row.Improvement != "350 minutes less waiting in the stretch, with up to 3 judgments more an hour" {
		t.Fatalf("the recommendation over the recorded window: %+v\nit saw: %s", rec.Rows, seenFor(rec, settings.CycleEveryWhenOutOfWork))
	}
	if got := seenFor(rec, settings.CycleEveryWhenOutOfWork); !strings.Contains(got, "373 min with a delay of 10, 350 min with a delay of 15, 315 min with a delay of 20, 240 min with a delay of 30") {
		t.Errorf("what the rule saw: %s", got)
	}
	// The records begin 39 hours ago: the last 24 hours can be asked for, a week cannot yet.
	if rec.Windows[0].Name != "24h" || !rec.Windows[0].Available || rec.Windows[1].Available {
		t.Errorf("the windows on offer: %+v", rec.Windows)
	}

	var out, errOut bytes.Buffer
	if code := RunRecommendCLI(ctx, a.root, st, nil, &out, &errOut); code != 0 ||
		!strings.Contains(out.String(), "cycle.every_when_out_of_work (.foreman-kit/daemon.yaml): never -> 15m\n") {
		t.Fatalf("config recommend: code %d\n%s%s", code, out.String(), errOut.String())
	}

	// With the delay set, the same records have nothing more to say.
	a.write(settings.CycleEvery.FilePath(), "cycle:\n  every: 60m\n  every_when_out_of_work: 15m\n")
	if rec, err := Recommend(ctx, a.root, st, "24h"); err != nil || len(rec.Rows) != 0 {
		t.Fatalf("with the delay set: %+v (err %v)", rec.Rows, err)
	}
}
