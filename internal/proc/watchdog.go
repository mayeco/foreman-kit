package proc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/store"
)

// WatchdogConfig configures the inactivity watchdog for a running process tree.
type WatchdogConfig struct {
	PID     int
	WorkDir string
	// TmpDir is the temporary directory the watched process has to itself (workers get a per-change
	// scratch directory); empty when it has none, and then no temporary directory is watched.
	TmpDir       string
	IdleLimit    time.Duration
	PollInterval time.Duration
	// CPUPercent is the share of one core, in percent, the process tree has to use between two polls
	// for its CPU use to count as activity; 0 counts any CPU use. See idleTracker.
	CPUPercent  float64
	OutputBytes func() int64
	// OnPoll, when set, is told after every poll how long the tree has shown no activity (0: it did
	// something since the previous poll). It runs on the goroutine of WatchIdle.
	OnPoll  func(idle time.Duration)
	OnStall func(stalledSeconds int)
	// ActivityPaths are files a detached agent writes as it works (its session store, its log): a
	// change of their size or modification time is activity, because the agent's work happens in a
	// service outside the watched tree.
	ActivityPaths []string
}

// watchdogPoll returns how often WatchIdle looks at a process tree: custom when it is set, otherwise
// a quarter of limit, never more often than every second nor less often than every minute.
func watchdogPoll(limit, custom time.Duration) time.Duration {
	if custom > 0 {
		return custom
	}
	every := limit / 4
	if every < time.Second {
		every = time.Second
	}
	if every > 60*time.Second {
		every = 60 * time.Second
	}
	return every
}

// WatchIdle polls the CPU time the process tree uses, the git worktree state (including untracked file
// contents) and the output byte count. When the tree shows no activity for IdleLimit, it invokes OnStall.
func WatchIdle(ctx context.Context, cfg WatchdogConfig) {
	if cfg.PID <= 0 || cfg.IdleLimit <= 0 {
		return
	}
	every := watchdogPoll(cfg.IdleLimit, cfg.PollInterval)
	outBytes := func() int64 {
		if cfg.OutputBytes != nil {
			return cfg.OutputBytes()
		}
		return 0
	}

	tracker := idleTracker{cpuShare: cfg.CPUPercent / 100}
	var meter cpuMeter

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	last := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !store.IsPIDAlive(cfg.PID) || ctx.Err() != nil {
				return
			}
			// The time since the previous poll, measured: a poll that takes longer than the interval
			// (git on a large work directory) makes the ticker drop ticks, and the time they stood for
			// still passed. Never less than the interval, which is what the ticker spaces its ticks by:
			// a tick handled late makes the next one look early, and the idle limit would then be
			// missed by a hair and only seen a whole poll later.
			now := time.Now()
			elapsed := now.Sub(last)
			if elapsed < every {
				elapsed = every
			}
			last = now

			used := meter.used(sampleTreeCPU(cfg.PID))
			state := ActivityState(ctx, cfg.WorkDir, cfg.TmpDir, outBytes()) + FilesState(cfg.ActivityPaths)
			since := tracker.observe(used, state, elapsed)
			if cfg.OnPoll != nil {
				cfg.OnPoll(since)
			}
			if since >= cfg.IdleLimit {
				sec := int(since.Seconds())
				if sec <= 0 {
					sec = 1
				}
				if cfg.OnStall != nil {
					cfg.OnStall(sec)
				}
				return
			}
		}
	}
}

// idleTracker measures, from one poll of WatchIdle to the next, how long a process tree has shown no
// activity: a different ActivityState, or CPU use at a rate that is work.
//
// CPU counts by its rate between two polls, never by what adds up over the idle time. A CLI that
// froze (on a connection that never answers, on a usage limit it waits out instead of failing) still
// wakes up for its timers: an idle Claude Code process uses about 1% of a core, the same as while it
// only waits for its model, so its own CPU time tells nothing. Counted as "one more CPU second since
// the last activity", that trickle reset the idle clock every couple of minutes and a frozen agent
// was never stopped before its time limit. What tells work from waiting is a command the agent runs
// (a test run, a build), which uses far more; cpuShare is the share of one core the tree has to
// reach between two polls (0: any CPU use counts, which only stops a tree that uses none).
type idleTracker struct {
	cpuShare float64
	started  bool
	state    string // ActivityState at the last activity
	idle     time.Duration
}

// observe records one poll: the CPU time the tree used since the previous poll, its ActivityState now,
// and the time between the two polls. It returns how long the tree has been idle.
func (t *idleTracker) observe(cpuUsed time.Duration, state string, elapsed time.Duration) time.Duration {
	working := cpuUsed > 0 && float64(cpuUsed) >= t.cpuShare*float64(elapsed)
	switch {
	case !t.started:
		// The first poll has nothing to compare with: the tree is taken as idle since it started, so
		// one that never does anything is stopped at the limit, not a poll later.
		t.started = true
		t.state = state
		t.idle += elapsed
	case working || state != t.state:
		t.idle = 0
		t.state = state
	default:
		t.idle += elapsed
	}
	return t.idle
}

// procCPU is the CPU time of one process: what it used itself, and what the children it has waited
// for used (Linux adds an exited child's time to the parent that reaps it; macOS adds it nowhere, so
// reaped stays 0 there).
type procCPU struct {
	own    time.Duration
	reaped time.Duration
}

// cpuSample is the CPU time of every process of a tree at one instant, by process ID.
type cpuSample map[int]procCPU

// cpuMeter turns successive samples of a process tree into the CPU time the tree used between them.
type cpuMeter struct {
	started bool
	prev    cpuSample
}

// used returns the CPU time the tree used between the previous sample and s (0 for the first one).
//
// It is not the difference of two totals. The total over the processes in the tree falls whenever one
// leaves it: on macOS with every command that exits, on Linux when a process loses its parent and is
// no longer under the root. Measured against a total, the CPU the tree used meanwhile disappeared
// behind that fall, and a quiet test run after a heavier build read as a stall. So each process still
// in the tree counts for what it gained (a new one, for all its time), and the processes that exited
// count for what their parents gained by reaping them, less what was already counted while they ran.
func (m *cpuMeter) used(s cpuSample) time.Duration {
	if len(s) == 0 {
		// Nothing could be read (ps failed, or the tree is gone): that says nothing about the CPU it
		// used. Kept as the baseline, an empty sample would make every process look new at the next
		// one, and all the time it ever used would count as work.
		return 0
	}
	prev := m.prev
	m.prev = s
	if !m.started {
		m.started = true
		return 0
	}
	var own, reaped time.Duration
	for pid, now := range s {
		before := prev[pid]
		if d := now.own - before.own; d > 0 {
			own += d
		}
		reaped += now.reaped - before.reaped
	}
	for pid, before := range prev {
		if _, alive := s[pid]; !alive {
			reaped -= before.own + before.reaped
		}
	}
	if reaped < 0 {
		// A process left the tree without being reaped inside it: its time went with it.
		reaped = 0
	}
	return own + reaped
}

// TreePIDs returns rootPID and all of its descendant PIDs, using /proc natively on Linux
// and falling back to pgrep -P on macOS/BSD.
func TreePIDs(rootPID int) []int {
	if rootPID <= 0 {
		return nil
	}
	if runtime.GOOS == "linux" {
		if pids, ok := linuxTreePIDs(rootPID); ok {
			return pids
		}
	}
	seen := map[int]bool{rootPID: true}
	all := []int{rootPID}
	frontier := []int{rootPID}

	for len(frontier) > 0 {
		var next []int
		for _, p := range frontier {
			out, err := exec.Command("pgrep", "-P", strconv.Itoa(p)).Output()
			if err != nil {
				continue
			}
			for _, field := range strings.Fields(string(out)) {
				if child, err := strconv.Atoi(field); err == nil && child > 0 && !seen[child] {
					seen[child] = true
					all = append(all, child)
					next = append(next, child)
				}
			}
		}
		frontier = next
	}
	return all
}

// linuxTreePIDs discovers rootPID and all its descendants directly from /proc without spawning pgrep.
func linuxTreePIDs(rootPID int) ([]int, bool) {
	if _, err := os.Stat(fmt.Sprintf("/proc/%d/stat", rootPID)); err != nil {
		return nil, false
	}
	childrenMap := make(map[int][]int)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if ppid, _, ok := parseLinuxProcStat(pid); ok && ppid > 0 {
			childrenMap[ppid] = append(childrenMap[ppid], pid)
		}
	}
	seen := map[int]bool{rootPID: true}
	all := []int{rootPID}
	frontier := []int{rootPID}
	for len(frontier) > 0 {
		var next []int
		for _, p := range frontier {
			for _, child := range childrenMap[p] {
				if !seen[child] {
					seen[child] = true
					all = append(all, child)
					next = append(next, child)
				}
			}
		}
		frontier = next
	}
	return all, true
}

// linuxClockTick is the length of one tick of the CPU times in /proc/<pid>/stat: USER_HZ (_SC_CLK_TCK)
// is 100 ticks per second on every Linux architecture Go supports.
const linuxClockTick = 10 * time.Millisecond

// parseLinuxProcStat reads /proc/<pid>/stat and returns the process's parent and its CPU time.
func parseLinuxProcStat(pid int) (int, procCPU, bool) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, procCPU{}, false
	}
	return parseProcStatCPU(string(raw))
}

// parseProcStatCPU extracts the parent (field 4) and the CPU times (utime and stime, fields 14 and 15;
// cutime and cstime of the children waited for, fields 16 and 17) from the content of /proc/<pid>/stat.
// The command name (field 2) can contain spaces and parentheses, so the fields are located after its
// last closing parenthesis.
func parseProcStatCPU(stat string) (int, procCPU, bool) {
	closeParen := strings.LastIndexByte(stat, ')')
	if closeParen < 0 || closeParen+2 >= len(stat) {
		return 0, procCPU{}, false
	}
	// Fields after ") ": 0=state, 1=ppid, ..., 11=utime, 12=stime, 13=cutime, 14=cstime
	fields := strings.Fields(stat[closeParen+2:])
	if len(fields) < 15 {
		return 0, procCPU{}, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, procCPU{}, false
	}
	utime, _ := strconv.ParseInt(fields[11], 10, 64)
	stime, _ := strconv.ParseInt(fields[12], 10, 64)
	cutime, _ := strconv.ParseInt(fields[13], 10, 64)
	cstime, _ := strconv.ParseInt(fields[14], 10, 64)
	return ppid, procCPU{
		own:    time.Duration(utime+stime) * linuxClockTick,
		reaped: time.Duration(cutime+cstime) * linuxClockTick,
	}, true
}

// sampleTreeCPU returns the CPU time used so far by rootPID and each of its descendants, read from
// /proc on Linux and from ps elsewhere. A process that exits while it is being read is left out.
func sampleTreeCPU(rootPID int) cpuSample {
	pids := TreePIDs(rootPID)
	sample := make(cpuSample, len(pids))
	if len(pids) == 0 {
		return sample
	}
	if runtime.GOOS == "linux" {
		for _, p := range pids {
			if _, cpu, ok := parseLinuxProcStat(p); ok {
				sample[p] = cpu
			}
		}
		if len(sample) > 0 {
			return sample
		}
	}
	pidStrs := make([]string, len(pids))
	for i, p := range pids {
		pidStrs[i] = strconv.Itoa(p)
	}
	// ps exits non-zero when one of the processes is gone and still lists the others.
	out, _ := exec.Command("ps", "-o", "pid=,time=", "-p", strings.Join(pidStrs, ",")).Output()
	for pid, own := range parsePSTimes(string(out)) {
		sample[pid] = procCPU{own: own}
	}
	return sample
}

// parsePSTimes parses the lines of `ps -o pid=,time=` into the CPU time of each process.
func parsePSTimes(output string) map[int]time.Duration {
	times := make(map[int]time.Duration)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		if t, ok := parsePSTime(fields[1]); ok {
			times[pid] = t
		}
	}
	return times
}

// parsePSTime parses one CPU time as ps prints it: M:SS.ss on macOS, [[D-]HH:]MM:SS on Linux and BSD.
func parsePSTime(field string) (time.Duration, bool) {
	days := 0.0
	if dash := strings.IndexByte(field, '-'); dash >= 0 {
		d, err := strconv.ParseFloat(field[:dash], 64)
		if err != nil || d < 0 {
			return 0, false
		}
		days, field = d, field[dash+1:]
	}
	sec := 0.0
	for _, part := range strings.Split(field, ":") {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil || v < 0 {
			return 0, false
		}
		sec = sec*60 + v
	}
	return time.Duration(math.Round((days*86400 + sec) * float64(time.Second))), true
}

// TreeState computes a deterministic SHA-256 digest of the git working tree status and diff against HEAD
// in dir and all its child repositories.
func TreeState(ctx context.Context, dir string) string {
	if ctx == nil {
		ctx = context.Background()
	}
	h := sha256.New()
	writeRepoState := func(repoDir string) {
		if st, err := gitx.StatusPorcelain(ctx, repoDir, true); err == nil {
			h.Write([]byte(st))
		}
		if diff, err := gitx.RunGit(ctx, repoDir, "--no-optional-locks", "diff", "HEAD"); err == nil {
			h.Write([]byte(diff))
		}
	}
	writeRepoState(dir)
	if repo, err := gitx.Open(dir); err == nil {
		if children, err := repo.ChildPaths(); err == nil {
			for _, rel := range children {
				subDir := filepath.Join(dir, filepath.FromSlash(rel))
				if info, err := os.Stat(subDir); err == nil && info.IsDir() {
					writeRepoState(subDir)
				}
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FilesState returns the size and modification time of each of paths ("~/" is the home directory),
// or "" when there are none: what WatchIdle compares from one poll to the next for a detached agent.
// A path that does not exist (yet) contributes that it does not.
func FilesState(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range paths {
		p = ExpandHome(p)
		if info, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "\x00%s:%d:%d", p, info.Size(), info.ModTime().UnixNano())
		} else {
			fmt.Fprintf(&b, "\x00%s:-", p)
		}
	}
	return b.String()
}

// ExpandHome returns p with a leading "~/" (or a lone "~") replaced by the home directory.
func ExpandHome(p string) string {
	p = strings.TrimSpace(p)
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
}

// privateTmpDir returns tmpDir, the TMPDIR a command runs with, when the command has that directory to
// itself, like the per-change scratch directory of a worker, and "" when it is the temporary directory
// this process uses as well. Every program of the machine creates and deletes files in that one: its
// entries change all the time and say nothing about the command, so watching it kept a stalled command
// "active" for as long as anything else was busy there.
func privateTmpDir(tmpDir string) string {
	if tmpDir == "" {
		return ""
	}
	resolve := func(p string) string {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return real
		}
		return filepath.Clean(p)
	}
	if resolve(tmpDir) == resolve(os.TempDir()) {
		return ""
	}
	return tmpDir
}

// ActivityState computes a digest of TreeState, untracked file contents, outputBytes, and the entries
// of tmpDir, the temporary directory the watched process has to itself (empty: none).
func ActivityState(ctx context.Context, dir, tmpDir string, outputBytes int64) string {
	if ctx == nil {
		ctx = context.Background()
	}
	h := sha256.New()
	h.Write([]byte(TreeState(ctx, dir)))
	h.Write([]byte{0})

	hashUntracked := func(repoDir string) {
		out, err := gitx.RunGit(ctx, repoDir, "ls-files", "--others", "--exclude-standard")
		if err != nil {
			return
		}
		var files []string
		for _, line := range strings.Split(out, "\n") {
			f := strings.TrimSpace(line)
			if f != "" {
				files = append(files, f)
			}
		}
		sort.Strings(files)
		for _, f := range files {
			full := filepath.Join(repoDir, filepath.FromSlash(f))
			if b, err := os.ReadFile(full); err == nil {
				sum := sha256.Sum256(b)
				h.Write([]byte(f))
				h.Write(sum[:])
			}
		}
	}
	hashUntracked(dir)
	if repo, err := gitx.Open(dir); err == nil {
		if children, err := repo.ChildPaths(); err == nil {
			for _, rel := range children {
				subDir := filepath.Join(dir, filepath.FromSlash(rel))
				if info, err := os.Stat(subDir); err == nil && info.IsDir() {
					hashUntracked(subDir)
				}
			}
		}
	}

	fmt.Fprintf(h, "\x00%d", outputBytes)
	// Also include the file activity of the process's own TMPDIR (workers run with a per-change scratch
	// TMPDIR), so agents writing gate logs there are not falsely killed as idle.
	if tmpDir != "" {
		if entries, err := os.ReadDir(tmpDir); err == nil {
			for _, e := range entries {
				if info, err := e.Info(); err == nil {
					fmt.Fprintf(h, "\x00tmp:%s:%d:%d", e.Name(), info.Size(), info.ModTime().UnixNano())
				}
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
