package proc

import (
	"testing"
	"time"
)

// TestIdleTrackerIgnoresTheCPUAFrozenCLIUsesForItsTimers follows an agent CLI that froze (on a
// connection that never answers, on a usage limit it waits out) and keeps waking up for its timers:
// about 1% of a core, which is what an idle Claude Code process was measured to use. Counted as "one
// more CPU second than at the last activity", those wake-ups added up to a second every couple of
// minutes and reset the idle clock each time: the frozen agent was never stopped before its time
// limit (two hours by default) and held its worker until then.
func TestIdleTrackerIgnoresTheCPUAFrozenCLIUsesForItsTimers(t *testing.T) {
	const every = time.Minute
	const idleLimit = 20 * time.Minute
	const cli = 100
	tr := idleTracker{cpuShare: workShare}
	var meter cpuMeter

	cpu := 42.0 // what the CLI used while it worked
	tr.observe(meter.used(macTree(map[int]float64{cli: cpu})), "quiet", every)
	var idle time.Duration
	for poll := 2; poll <= int(idleLimit/every); poll++ {
		cpu += 0.6 // 1% of a core for a minute
		idle = tr.observe(meter.used(macTree(map[int]float64{cli: cpu})), "quiet", every)
		if want := time.Duration(poll) * every; idle != want {
			t.Fatalf("poll %d: the CLI used 0.6 CPU seconds in a minute, which is not work: idle = %v, want %v", poll, idle, want)
		}
	}
	if idle < idleLimit {
		t.Fatalf("after %v of timer wake-ups only (%.0f CPU seconds in all) the frozen CLI is not stalled: idle = %v", idleLimit, cpu-42, idle)
	}
}

// TestIdleTrackerCountsCPUByItsRateBetweenTwoPolls: what counts is the CPU used since the previous
// poll against the time between the two, at the share of a core the tracker takes as work.
func TestIdleTrackerCountsCPUByItsRateBetweenTwoPolls(t *testing.T) {
	const every = time.Minute
	for _, c := range []struct {
		name    string
		share   float64
		used    time.Duration
		elapsed time.Duration
		working bool
	}{
		{"exactly the share", 0.05, 3 * time.Second, every, true},
		{"just under the share", 0.05, 2990 * time.Millisecond, every, false},
		{"a test run that mostly waits", 0.05, 5 * time.Second, every, true},
		{"a build", 0.05, 4 * time.Minute, every, true},
		{"the same CPU over a poll that took three times as long", 0.05, 5 * time.Second, 3 * every, false},
		{"no CPU at all", 0.05, 0, every, false},
		{"share 0: a single tick counts", 0, 10 * time.Millisecond, every, true},
		{"share 0: no CPU is still no activity", 0, 0, every, false},
		{"a short poll keeps the same rate", 0.05, 50 * time.Millisecond, time.Second, true},
		{"a short poll under the rate", 0.05, 40 * time.Millisecond, time.Second, false},
	} {
		tr := idleTracker{cpuShare: c.share}
		tr.observe(0, "quiet", every)
		idle := tr.observe(c.used, "quiet", c.elapsed)
		if working := idle == 0; working != c.working {
			t.Errorf("%s: %v of CPU in %v at share %v: working = %v, want %v (idle %v)", c.name, c.used, c.elapsed, c.share, working, c.working, idle)
		}
	}
}

// TestIdleTrackerMeasuresIdleTimeByTheTimeBetweenPolls: a poll that came late (git on a large work
// directory took longer than the interval, and the ticker dropped ticks) adds the time that passed,
// not one interval.
func TestIdleTrackerMeasuresIdleTimeByTheTimeBetweenPolls(t *testing.T) {
	tr := idleTracker{cpuShare: workShare}
	tr.observe(0, "quiet", time.Minute)
	if idle := tr.observe(0, "quiet", 3*time.Minute); idle != 4*time.Minute {
		t.Fatalf("idle = %v after polls one and three minutes apart, want 4m", idle)
	}
}

func linuxProc(ownSec, reapedSec float64) procCPU {
	return procCPU{own: time.Duration(ownSec * float64(time.Second)), reaped: time.Duration(reapedSec * float64(time.Second))}
}

// TestCPUMeterCountsWhatTheTreeUsedBetweenTwoSamples covers both ways the system reports CPU time:
// Linux adds the time of a child that exits to the parent that waits for it, macOS adds it nowhere.
func TestCPUMeterCountsWhatTheTreeUsedBetweenTwoSamples(t *testing.T) {
	sec := func(d time.Duration) float64 { return d.Seconds() }
	const cli, build, tests, orphan = 100, 101, 102, 103

	t.Run("the first sample has nothing to compare with", func(t *testing.T) {
		var m cpuMeter
		if used := m.used(cpuSample{cli: linuxProc(500, 20)}); used != 0 {
			t.Fatalf("used = %v for the first sample, want 0", used)
		}
	})

	t.Run("macOS: a command that exited takes its time with it, the others still count", func(t *testing.T) {
		var m cpuMeter
		m.used(macTree(map[int]float64{cli: 30, build: 400}))
		// The build exited; meanwhile the CLI used 1 second and started a test run that used 5.
		if used := m.used(macTree(map[int]float64{cli: 31, tests: 5})); sec(used) != 6 {
			t.Fatalf("used = %v, want 6s (the fall of the total by 394 seconds hid it)", used)
		}
		if used := m.used(macTree(map[int]float64{cli: 31, tests: 5})); used != 0 {
			t.Fatalf("used = %v with no CPU use since the previous sample, want 0", used)
		}
	})

	t.Run("Linux: a command that ran and exited between two samples counts through its parent", func(t *testing.T) {
		var m cpuMeter
		m.used(cpuSample{cli: linuxProc(30, 0)})
		if used := m.used(cpuSample{cli: linuxProc(30, 3)}); sec(used) != 3 {
			t.Fatalf("used = %v, want the 3s of the command the CLI reaped", used)
		}
	})

	t.Run("Linux: a command seen running is not counted again when it is reaped", func(t *testing.T) {
		var m cpuMeter
		m.used(cpuSample{cli: linuxProc(30, 0), build: linuxProc(400, 0)})
		// The build used 20 more seconds and exited (the CLI now carries its 420), the CLI used 1 and
		// a test run started and used 5.
		if used := m.used(cpuSample{cli: linuxProc(31, 420), tests: linuxProc(5, 0)}); sec(used) != 26 {
			t.Fatalf("used = %v, want 26s (1 + 5 + the last 20 of the build)", used)
		}
	})

	t.Run("a sample that could not be read is not a tree that restarted", func(t *testing.T) {
		var m cpuMeter
		m.used(macTree(map[int]float64{cli: 300}))
		if used := m.used(cpuSample{}); used != 0 {
			t.Fatalf("used = %v for an empty sample, want 0", used)
		}
		// Read again: the CLI used a tenth of a second meanwhile, not the 300 seconds of its whole life.
		if used := m.used(macTree(map[int]float64{cli: 300.1})); used != 100*time.Millisecond {
			t.Fatalf("used = %v after a sample that could not be read, want 100ms", used)
		}
	})

	t.Run("Linux: a process that left the tree without being reaped hides nothing", func(t *testing.T) {
		var m cpuMeter
		m.used(cpuSample{cli: linuxProc(30, 0), orphan: linuxProc(50, 0)})
		// The orphan's parent died: it runs on under init, outside the tree. The CLI used half a second.
		if used := m.used(cpuSample{cli: linuxProc(30.5, 0)}); used != 500*time.Millisecond {
			t.Fatalf("used = %v, want 500ms", used)
		}
	})
}

func TestParsePSTimes(t *testing.T) {
	out := "  412   0:00.07\n95416   4:31.26\n  777  1:02:03\n  888 2-03:04:05\n  junk line\n  999  not-a-time\n"
	got := parsePSTimes(out)
	want := map[int]time.Duration{
		412:   70 * time.Millisecond,
		95416: 4*time.Minute + 31*time.Second + 260*time.Millisecond,
		777:   time.Hour + 2*time.Minute + 3*time.Second,
		888:   51*time.Hour + 4*time.Minute + 5*time.Second,
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d processes, want %d: %v", len(got), len(want), got)
	}
	for pid, w := range want {
		if got[pid] != w {
			t.Errorf("pid %d: %v, want %v", pid, got[pid], w)
		}
	}
}

func TestParseProcStatCPU(t *testing.T) {
	// A command name with spaces and parentheses; utime 1234, stime 66, cutime 300, cstime 50 ticks.
	stat := "4242 (node (agent) worker) S 4000 4242 4242 0 -1 4194304 5000 9000 0 0 1234 66 300 50 20 0 11 0 123456 1000000 2000 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0"
	ppid, cpu, ok := parseProcStatCPU(stat)
	if !ok || ppid != 4000 {
		t.Fatalf("ppid = %d ok = %v, want 4000 true", ppid, ok)
	}
	if cpu.own != 13*time.Second || cpu.reaped != 3500*time.Millisecond {
		t.Fatalf("own = %v reaped = %v, want 13s and 3.5s", cpu.own, cpu.reaped)
	}
	if _, _, ok := parseProcStatCPU("4242 (truncated) S 4000"); ok {
		t.Fatal("a truncated stat line was accepted")
	}
}
