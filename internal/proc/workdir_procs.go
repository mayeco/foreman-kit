package proc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DirProcess is a process whose current directory is inside a directory the kit works in.
type DirProcess struct {
	PID     int
	Dir     string
	Command string
}

func (p DirProcess) String() string {
	if p.Command == "" {
		return fmt.Sprintf("pid %d in %s", p.PID, p.Dir)
	}
	return fmt.Sprintf("pid %d (%s) in %s", p.PID, p.Command, p.Dir)
}

// ProcessesIn returns the processes of this user, other than this one, whose current directory is dir
// or lies inside it: on Linux from /proc/<pid>/cwd, on macOS from lsof. A detached agent's call the
// kit stopped can leave such processes behind, still working in a worktree (its shell commands run
// there). It returns nil when it cannot tell.
func ProcessesIn(ctx context.Context, dir string) []DirProcess {
	root := resolvedDir(dir)
	if root == "" {
		return nil
	}
	var found []DirProcess
	if runtime.GOOS == "linux" {
		found = linuxProcessesIn(root)
	} else {
		found = lsofProcessesIn(ctx, root)
	}
	self := os.Getpid()
	out := found[:0]
	for _, p := range found {
		if p.PID != self {
			out = append(out, p)
		}
	}
	return out
}

func resolvedDir(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return filepath.Clean(abs)
}

// inside reports whether path is root or lies inside it.
func inside(root, path string) bool {
	path = filepath.Clean(path)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func linuxProcessesIn(root string) []DirProcess {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []DirProcess
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil || !inside(root, cwd) {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		out = append(out, DirProcess{PID: pid, Dir: cwd, Command: shortCommand(strings.ReplaceAll(string(cmd), "\x00", " "))})
	}
	return out
}

func lsofProcessesIn(ctx context.Context, root string) []DirProcess {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// -d cwd: only the current directory of each process; -F pcn: its pid, command and path.
	out, err := exec.CommandContext(cctx, "lsof", "-w", "-n", "-P", "-d", "cwd", "-F", "pcn").Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	return parseLsofCwd(string(out), root)
}

// parseLsofCwd reads the output of lsof -d cwd -F pcn (lines p<pid>, c<command>, n<path>) and keeps
// the processes whose current directory is inside root.
func parseLsofCwd(output, root string) []DirProcess {
	var out []DirProcess
	var cur DirProcess
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ := strconv.Atoi(line[1:])
			cur = DirProcess{PID: pid}
		case 'c':
			cur.Command = line[1:]
		case 'n':
			if cur.PID > 0 && inside(root, line[1:]) {
				cur.Dir = line[1:]
				out = append(out, cur)
			}
		}
	}
	return out
}

func shortCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if len(cmd) > 120 {
		return cmd[:120] + "..."
	}
	return cmd
}

// WaitDirFree waits, up to grace, until no process other than this one has its current directory
// inside dir, polling every poll (a second when 0). It returns the processes still there when grace
// ran out, or nil.
func WaitDirFree(ctx context.Context, dir string, grace, poll time.Duration) []DirProcess {
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(grace)
	for {
		left := ProcessesIn(ctx, dir)
		if len(left) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return left
		}
		select {
		case <-ctx.Done():
			return left
		case <-time.After(poll):
		}
	}
}
