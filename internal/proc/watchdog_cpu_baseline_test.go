package proc

import (
	"testing"
	"time"
)

// workShare is the CPU share the trackers of these tests take as work: the default of
// limits.agent_idle_cpu_percent.
const workShare = 0.05

// macTree builds a sample of a process tree as macOS reports it: each process has only its own time.
func macTree(seconds map[int]float64) cpuSample {
	s := make(cpuSample, len(seconds))
	for pid, sec := range seconds {
		s[pid] = procCPU{own: time.Duration(sec * float64(time.Second))}
	}
	return s
}

// TestIdleTrackerSeesCPUAfterTheTreeTotalFell follows an agent that runs a heavy build and then a long,
// quieter test run without printing anything or touching a file the watchdog looks at. When the build
// exits, its CPU time leaves the tree's total (macOS never adds it to the parent). The test run's CPU
// use must still count as activity; measured against the build's peak it stayed invisible, and the agent
// was stopped as stalled while its tests were running.
func TestIdleTrackerSeesCPUAfterTheTreeTotalFell(t *testing.T) {
	const every = time.Minute
	const idleLimit = 20 * time.Minute
	const agent, build, tests = 100, 101, 102
	tr := idleTracker{cpuShare: workShare}
	var meter cpuMeter

	tr.observe(meter.used(macTree(map[int]float64{agent: 30})), "quiet", every) // the agent alone
	if idle := tr.observe(meter.used(macTree(map[int]float64{agent: 30, build: 400})), "quiet", every); idle != 0 {
		t.Fatalf("a build that used 400 CPU seconds is activity, idle = %v", idle)
	}
	// The build exits and the agent waits before it starts anything else: the total fell by 400 seconds.
	if idle := tr.observe(meter.used(macTree(map[int]float64{agent: 30.5})), "quiet", every); idle != every {
		t.Fatalf("a fall of the total is not activity by itself, idle = %v, want %v", idle, every)
	}
	cpu := 0.0
	for poll := 1; poll <= int(idleLimit/every); poll++ {
		cpu += 5 // a test run that mostly waits on a database
		used := meter.used(macTree(map[int]float64{agent: 30.5, tests: cpu}))
		if idle := tr.observe(used, "quiet", every); idle >= idleLimit {
			t.Fatalf("poll %d: the tree used 5 CPU seconds a minute for %v and was declared stalled (idle = %v)", poll, time.Duration(poll)*every, idle)
		} else if idle != 0 {
			t.Fatalf("poll %d: 5 more CPU seconds than at the previous poll is activity, idle = %v (used %v)", poll, idle, used)
		}
	}
}

// TestIdleTrackerStillStallsAfterTheTreeTotalFell keeps the watchdog's purpose: a tree that stops using
// CPU right after a process left it is idle from then on.
func TestIdleTrackerStillStallsAfterTheTreeTotalFell(t *testing.T) {
	const every = 30 * time.Second
	const agent, build = 100, 101
	tr := idleTracker{cpuShare: workShare}
	var meter cpuMeter

	tr.observe(meter.used(macTree(map[int]float64{agent: 10})), "quiet", every)
	tr.observe(meter.used(macTree(map[int]float64{agent: 10, build: 190})), "quiet", every)
	var idle time.Duration
	for poll := 0; poll < 10; poll++ {
		idle = tr.observe(meter.used(macTree(map[int]float64{agent: 10})), "quiet", every)
	}
	if want := 10 * every; idle != want {
		t.Fatalf("idle = %v after 10 polls without CPU use, output or file changes, want %v", idle, want)
	}
	if idle = tr.observe(meter.used(macTree(map[int]float64{agent: 10})), "changed", every); idle != 0 {
		t.Fatalf("a changed activity state is activity, idle = %v", idle)
	}
}
