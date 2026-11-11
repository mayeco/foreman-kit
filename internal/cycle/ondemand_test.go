package cycle

import (
	"testing"
	"time"
)

// Each condition of the on-demand cycle, on its own: what the daemon answers when that one fails and
// every earlier one holds.
func TestDecideAdvanceNamesWhatItWaitsFor(t *testing.T) {
	start := time.Date(2026, 11, 10, 5, 18, 20, 0, time.UTC)
	queued := start.Add(7 * time.Minute)
	free := DispatchReport{Workers: 2, Running: 1, At: queued.Add(time.Second)}
	// Every condition holds at minute 20: the delay passed, the dispatcher looked at what the cycle
	// queued and has a worker free, the queue has room, and something moved.
	ready := advanceInput{
		Now: start.Add(20 * time.Minute), IdleDelay: 15 * time.Minute,
		LastStart: start, QueuedAt: queued, Report: free, HaveReport: true, Moved: true,
	}
	with := func(edit func(*advanceInput)) advanceInput {
		in := ready
		edit(&in)
		return in
	}

	for _, tc := range []struct {
		name string
		in   advanceInput
		want advanceState
	}{
		{"every condition holds", ready, advanceNow},
		{"the setting is 0", with(func(in *advanceInput) { in.IdleDelay = 0 }), advanceOff},
		{"no cycle on record", with(func(in *advanceInput) { in.LastStart = time.Time{} }), advanceNoCycle},
		{"the delay has not passed", with(func(in *advanceInput) { in.Now = start.Add(14*time.Minute + 59*time.Second) }), advanceDelay},
		{"the delay passes on the dot", with(func(in *advanceInput) { in.Now = start.Add(15 * time.Minute) }), advanceNow},
		{"no dispatcher is passing", with(func(in *advanceInput) { in.HaveReport = false }), advanceNoReport},
		{"the dispatcher has not looked since the cycle queued", with(func(in *advanceInput) { in.Report.At = queued.Add(-time.Second) }), advanceNoReport},
		{"a pass that began as the cycle handed over its work read it afterwards", with(func(in *advanceInput) { in.Report.At = queued }), advanceNow},
		{"every worker is building", with(func(in *advanceInput) { in.Report.Running = 2 }), advanceBusy},
		{"an item waits for a worker", with(func(in *advanceInput) { in.Report.Waiting = 1 }), advanceBusy},
		{"the pipeline is halted", with(func(in *advanceInput) { in.Report.Halted = true }), advanceBusy},
		{"as many changes are open as the application allows", with(func(in *advanceInput) { in.QueueFull = true }), advanceQueueFull},
		{"the queue is full and nothing moved", with(func(in *advanceInput) { in.QueueFull, in.Moved = true, false }), advanceQueueFull},
		{"the queue is full while every worker builds", with(func(in *advanceInput) { in.QueueFull, in.Report.Running = true, 2 }), advanceBusy},
		{"nothing moved in the last two cycles", with(func(in *advanceInput) { in.Moved = false }), advanceNoMovement},
	} {
		got := decideAdvance(tc.in)
		if got.State != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got.State, tc.want)
		}
		wantNotBefore := start.Add(15 * time.Minute)
		if tc.want == advanceOff || tc.want == advanceNoCycle {
			wantNotBefore = time.Time{}
		}
		if !got.NotBefore.Equal(wantNotBefore) {
			t.Errorf("%s: NotBefore = %s, want %s", tc.name, got.NotBefore, wantNotBefore)
		}
	}
}

// measuredHour is one of the eight hours measured in real_test on 2026-11-10, from 05:18 to 13:18 UTC
// with 2 workers and a cycle every 60 minutes, as its daemon.log tells it. Minutes count from the start
// of the hour's cycle.
type measuredHour struct {
	cycle string
	// queued is when the judgment ended and the dispatcher picked up what it queued.
	queued float64
	// free is when a worker was first free with nothing left to start.
	free float64
	// moved is when the first change of the hour was built, blocked or integrated; -1 when none was
	// (every run ended held, or the cycle queued nothing).
	moved float64
	// previousMoved says whether the cycle before this one moved anything.
	previousMoved bool
}

var measuredWindow = []measuredHour{
	{cycle: "05:18", queued: 7.4, free: 11.95, moved: 11.95, previousMoved: false},
	{cycle: "06:18", queued: 7.9, free: 11.18, moved: 13.08, previousMoved: true},
	{cycle: "07:18", queued: 5.53, free: 13.93, moved: 13.93, previousMoved: true},
	{cycle: "08:18", queued: 6.47, free: 8.58, moved: -1, previousMoved: true},
	{cycle: "09:18", queued: 6.37, free: 12.85, moved: 12.85, previousMoved: false},
	{cycle: "10:18", queued: 8.37, free: 12.0, moved: 13.83, previousMoved: true},
	{cycle: "11:18", queued: 6.72, free: 6.72, moved: -1, previousMoved: true},
	{cycle: "12:18", queued: 6.33, free: 6.4, moved: 24.72, previousMoved: false},
}

// advanceSecond replays an hour second by second and returns the first second at which the next cycle
// would have started before its tick, or 3600 when it would have waited for the tick.
func advanceSecond(h measuredHour, idleDelay time.Duration) int {
	start := time.Date(2026, 11, 10, 5, 18, 20, 0, time.UTC)
	at := func(minutes float64) time.Time { return start.Add(time.Duration(minutes * float64(time.Minute))) }
	for second := 0; second < 3600; second++ {
		now := start.Add(time.Duration(second) * time.Second)
		minute := float64(second) / 60
		running := 2
		if minute >= h.free {
			running = 1
		}
		in := advanceInput{
			Now:       now,
			IdleDelay: idleDelay,
			LastStart: start,
			QueuedAt:  at(h.queued),
			// The dispatcher passes at least every 30 seconds: its latest report is a second old.
			Report:     DispatchReport{Workers: 2, Running: running, At: now.Add(-time.Second)},
			HaveReport: true,
			Moved:      h.previousMoved || (h.moved >= 0 && minute >= h.moved),
		}
		if decideAdvance(in).State == advanceNow {
			return second
		}
	}
	return 3600
}

// minutesSaved is how many minutes earlier than its tick the next cycle would have started, summed
// over the measured hours and rounded to the minute.
func minutesSaved(idleDelay time.Duration) int {
	seconds := 0
	for _, h := range measuredWindow {
		seconds += 3600 - advanceSecond(h, idleDelay)
	}
	return (seconds + 30) / 60
}

// With a delay of 15 minutes, the rule would have started the next cycle at minute 15 in seven of the
// eight measured hours and during minute 25 in the other, instead of minute 60: 350 minutes of waiting
// less. The two hours that moved nothing (08:18 and 11:18) are advanced too, because the cycle before
// each of them did move: that is the one retry the brake allows.
func TestDecideAdvanceOverTheMeasuredWindow(t *testing.T) {
	want := []int{15, 15, 15, 15, 15, 15, 15, 25}
	for i, h := range measuredWindow {
		got := (advanceSecond(h, 15*time.Minute) + 59) / 60 // the minute it falls in, rounded up
		if got != want[i] {
			t.Errorf("cycle of %s: the next one would start at minute %d, want %d", h.cycle, got, want[i])
		}
	}
	if got := minutesSaved(15 * time.Minute); got != 350 {
		t.Errorf("minutes of waiting removed over the eight hours: got %d, want 350", got)
	}
}

// The brake: an hour whose cycle moved nothing, after another that moved nothing, waits for its tick.
// With the setting at 0, every hour does.
func TestDecideAdvanceBrakesAndTurnsOff(t *testing.T) {
	stuck := measuredHour{cycle: "stuck", queued: 6.5, free: 8, moved: -1, previousMoved: false}
	if got := advanceSecond(stuck, 15*time.Minute); got != 3600 {
		t.Errorf("two cycles in a row without movement: the next one would start at second %d, want the tick", got)
	}
	for _, h := range measuredWindow {
		if got := advanceSecond(h, 0); got != 3600 {
			t.Errorf("cycle of %s with the setting at 0: the next one would start at second %d, want the tick", h.cycle, got)
		}
	}
}

// A longer delay only moves the advance later. These are the figures the settings recommendation
// compares to choose a delay: the wait each one removes over the measured window.
func TestDecideAdvanceWithOtherDelays(t *testing.T) {
	for _, tc := range []struct {
		delay time.Duration
		want  int
	}{
		{10 * time.Minute, 373},
		{15 * time.Minute, 350},
		{20 * time.Minute, 315},
		{30 * time.Minute, 240},
	} {
		if got := minutesSaved(tc.delay); got != tc.want {
			t.Errorf("delay of %s: minutes of waiting removed = %d, want %d", tc.delay, got, tc.want)
		}
	}
}
