package proc

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/gitx"
)

// ExitWorkdirBusy (73) says a call did not start: processes of an earlier call the kit stopped still
// work in its directory.
const ExitWorkdirBusy = 73

// detachedStopTimeout bounds the stop command of a detached agent.
const detachedStopTimeout = time.Minute

// detachedGrace is how long a call waits for the processes a stopped detached call left in its
// directory when the caller gives no grace period.
const detachedGrace = 60 * time.Second

// detachedStoppedMarker returns the file that records, for the git work tree of dir, that a call of a
// detached agent was stopped there: inside the work tree's git directory, so it is never a file of
// the work tree itself. "" when dir is not in a git work tree.
func detachedStoppedMarker(ctx context.Context, dir string) string {
	out, err := gitx.RunGit(ctx, dir, "rev-parse", "--git-path", "foreman-detached-stopped")
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(out)
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p
}

// stoppedByKit reports whether a call ended because the kit stopped it: it stalled, hit its time
// limit, or its context ended (a cancellation, a shutdown).
func stoppedByKit(ctx context.Context, rc int, idleStalled bool) bool {
	return idleStalled || rc == ExitTimeout || rc == 143 || rc == 137 || ctx.Err() != nil
}

// stopDetachedCall runs, for a call of the detached agent engine that the kit stopped, the agent's stop
// command (with {{workdir}} and {{pid}}), and records in the work tree that the call was stopped, so
// that no later call starts there while its processes still work (waitForStoppedCall). A detached
// agent without a stop command holds no role; it can still be named for one call, and then only the
// record is written.
func stopDetachedCall(ctx context.Context, spec config.AgentCommandSpec, engine, workdir string, pid int, out io.Writer) {
	ctx = context.WithoutCancel(ctx)
	if spec.Stop != nil {
		words, err := config.CommandWords(spec.Stop)
		if err == nil && len(words) > 0 && words[0] != "" {
			replacer := strings.NewReplacer("{{workdir}}", workdir, "{{pid}}", strconv.Itoa(pid))
			argv := make([]string, 0, len(words)-1)
			for _, w := range words[1:] {
				argv = append(argv, replacer.Replace(w))
			}
			sctx, cancel := context.WithTimeout(ctx, detachedStopTimeout)
			cmd := exec.CommandContext(sctx, replacer.Replace(words[0]), argv...)
			cmd.Dir = workdir
			res, runErr := cmd.CombinedOutput()
			cancel()
			if runErr != nil {
				fmt.Fprintf(out, "agent_exec: the stop command of %s failed: %v %s\n", engine, runErr, strings.TrimSpace(string(res)))
			} else {
				fmt.Fprintf(out, "agent_exec: ran the stop command of %s\n", engine)
			}
		}
	} else {
		fmt.Fprintf(out, "agent_exec: %s is detached and has no stop command: its call may still be working in %s\n", engine, workdir)
	}
	if marker := detachedStoppedMarker(ctx, workdir); marker != "" {
		_ = os.MkdirAll(filepath.Dir(marker), 0o755)
		_ = os.WriteFile(marker, []byte(fmt.Sprintf("%s pid %d stopped %s\n", engine, pid, time.Now().UTC().Format(time.RFC3339))), 0o644)
	}
}

// waitForStoppedCall says whether a call may start in workdir: when a call of a detached agent was
// stopped there (stopDetachedCall), it waits up to grace for every process whose current directory
// is inside workdir to end. It returns the processes still there, or nil when the call may start.
func waitForStoppedCall(ctx context.Context, workdir string, grace, poll time.Duration) []DirProcess {
	marker := detachedStoppedMarker(ctx, workdir)
	if marker == "" {
		return nil
	}
	if _, err := os.Stat(marker); err != nil {
		return nil
	}
	if grace <= 0 {
		grace = detachedGrace
	}
	if left := WaitDirFree(ctx, workdir, grace, poll); len(left) > 0 {
		return left
	}
	_ = os.Remove(marker)
	return nil
}

// workdirBusyMessage says why a call did not start in workdir.
func workdirBusyMessage(workdir string, left []DirProcess) string {
	names := make([]string, 0, len(left))
	for _, p := range left {
		names = append(names, p.String())
	}
	return fmt.Sprintf("agent_exec: a call of a detached agent was stopped in %s and its processes still work there: %s; no call starts there until they end\n",
		workdir, strings.Join(names, "; "))
}

// activityPathsOf returns the files the watchdog watches for a call of an agent: those of its
// activity, for a detached agent; none for any other, whose work is in the process tree.
func activityPathsOf(spec config.AgentCommandSpec) []string {
	if !spec.Detached {
		return nil
	}
	return spec.Activity
}
