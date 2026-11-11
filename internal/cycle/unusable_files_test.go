package cycle

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// withoutLine removes from the configuration file called name of root the line that reads line,
// once indentation is set aside: the key it wrote is then missing from the file.
func withoutLine(t *testing.T, root, name, line string) {
	t.Helper()
	path := filepath.Join(root, ".foreman-kit", name+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	found := false
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) == line {
			found = true
			continue
		}
		kept = append(kept, l)
	}
	if !found {
		t.Fatalf("%s has no line %q to remove:\n%s", path, line, raw)
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A cycle and a visual session judge with what their files say. Neither starts on a file that lacks
// a key: it says which key keeps it, instead of working with a value nobody wrote. A file neither of
// them reads does not keep them.
func TestNoCycleAndNoVisualSessionOnAFileThatLacksAKey(t *testing.T) {
	ctx := context.Background()
	runCycle := func(root string) (judged bool, out string, err error) {
		var buf bytes.Buffer
		err = RunCycle(ctx, root, openTestStore(t, root), CycleOptions{
			SkipWorktreePrep: true,
			AgentExecFunc: func(context.Context, string, string, string, time.Duration, io.Writer) error {
				judged = true
				return nil
			},
		}, &buf)
		return judged, buf.String(), err
	}
	beginVisual := func(root string) (string, error) {
		var buf bytes.Buffer
		err := VisualQABegin(ctx, root, openTestStore(t, root), VisualQAOptions{SkipWorktreePrep: true}, &buf)
		return buf.String(), err
	}

	for _, c := range []struct {
		file, line, names string
		cycle, visual     bool // whether each one waits for the file
	}{
		{"cycle", "max_open: 8", ".foreman-kit/cycle.yaml: queue.max_open: missing (the kit ships 8)", true, false},
		{"agents", "idle_cpu_percent: 5", ".foreman-kit/agents.yaml: limits.idle_cpu_percent: missing (the kit ships 5)", true, false},
		{"exploration", "selection: in_order", ".foreman-kit/exploration.yaml: selection: missing (the kit ships in_order)", true, true},
		{"visual_qa", "max_checks: 5", ".foreman-kit/visual_qa.yaml: max_checks: missing (the kit ships 5)", true, true},
		{"pipeline", "workers: 2", ".foreman-kit/pipeline.yaml: dispatch.workers: missing (the kit ships 2)", true, true},
	} {
		root := bareApp(t)
		withoutLine(t, root, c.file, c.line)
		if judged, out, err := runCycle(root); c.cycle {
			if err == nil || judged || !strings.Contains(out, "cycle: "+c.names) || !strings.Contains(out, "nothing new starts until it is mended") {
				t.Errorf("config/%s.yaml without %q: a cycle ran (judged: %v, err: %v):\n%s", c.file, c.line, judged, err, out)
			}
		}
		out, err := beginVisual(root)
		if waits := err != nil && strings.Contains(out, "visual-qa: "+c.names); waits != c.visual {
			t.Errorf("config/%s.yaml without %q: the visual session waits = %v, want %v (err: %v):\n%s", c.file, c.line, waits, c.visual, err, out)
		}
	}

	// What only the daemon reads keeps neither of them.
	root := bareApp(t)
	withoutLine(t, root, "daemon", "every: 60m")
	if _, out, _ := runCycle(root); strings.Contains(out, ".foreman-kit/daemon.yaml") {
		t.Errorf("a cycle waits for a file it does not read:\n%s", out)
	}
	if out, _ := beginVisual(root); strings.Contains(out, ".foreman-kit/daemon.yaml") {
		t.Errorf("a visual session waits for a file it does not read:\n%s", out)
	}

	// A file an older kit wrote is named as that.
	root = bareApp(t)
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "cycle.yaml"), []byte("qa_time_budget_minutes: 45\nproposals:\n  approval: jev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, out, err := runCycle(root); err == nil || !strings.Contains(out, "cycle: .foreman-kit/cycle.yaml holds keys an older foreman-kit read") || !strings.Contains(out, "foreman-kit update rewrites it") {
		t.Errorf("a cycle on a file of an older kit (err: %v):\n%s", err, out)
	}
}

// The daemon reads .foreman-kit/daemon.yaml once, when it starts: it does not start on one that lacks a
// key. The files of what it runs do not keep it: its dispatcher, its merger and its cycle each wait
// for their own. A daemon that is already running is still reported as running.
func TestDaemonDoesNotStartOnADaemonFileThatLacksAKey(t *testing.T) {
	detachedSpawnStandIn(t, "exec sleep 60")
	start := func(root string) (int, string, string) {
		var out, errOut bytes.Buffer
		rc := StartDaemon(context.Background(), root, openTestStore(t, root), DaemonOptions{Detach: true}, &out, &errOut)
		if m := detachedSpawnPIDRe.FindStringSubmatch(out.String() + errOut.String()); m != nil {
			if pid, err := strconv.Atoi(m[1]); err == nil && pid > 1 {
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			}
		}
		return rc, out.String(), errOut.String()
	}

	root := bareApp(t)
	withoutLine(t, root, "daemon", "every: 60m")
	if rc, out, errOut := start(root); rc == 0 || strings.Contains(out, "daemon: started") ||
		!strings.Contains(errOut, "daemon: .foreman-kit/daemon.yaml: cycle.every: missing (the kit ships 60m)") || !strings.Contains(errOut, "it does not start until it is mended") {
		t.Fatalf("a daemon started on a .foreman-kit/daemon.yaml without cycle.every (rc=%d): stdout %q stderr %q", rc, out, errOut)
	}

	root = bareApp(t)
	withoutLine(t, root, "cycle", "max_open: 8")
	if rc, out, errOut := start(root); rc != 0 || !strings.Contains(out, "daemon: started pid=") {
		t.Fatalf("a daemon did not start because of a file its cycle reads (rc=%d): stdout %q stderr %q", rc, out, errOut)
	}
}

// `foreman-kit daemon restart` stops the daemon and starts it again. With a .foreman-kit/daemon.yaml that
// lacks a key the second half would be refused: the daemon that is running is then left alone, so a
// restart never takes down a pipeline that works.
func TestDaemonRestartLeavesTheRunningDaemonWhenItsFileLacksAKey(t *testing.T) {
	ctx := context.Background()
	root := bareApp(t)
	st := openTestStore(t, root)

	// A process that stands for the daemon that is running, holding its lock.
	running := exec.Command("sleep", "60")
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = running.Process.Kill()
		_, _ = running.Process.Wait()
	})
	if _, err := st.AcquireLock(ctx, "daemon", store.LockOptions{OwnerType: "daemon", PID: running.Process.Pid, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}

	withoutLine(t, root, "daemon", "every: 60m")
	var out, errOut bytes.Buffer
	rc := RunDaemonCLI(ctx, root, st, DaemonOptions{}, []string{"restart"}, &out, &errOut)
	if rc == 0 || !strings.Contains(errOut.String(), "daemon: .foreman-kit/daemon.yaml: cycle.every: missing (the kit ships 60m)") ||
		!strings.Contains(errOut.String(), "not restarted, and the daemon that is running was left as it is") {
		t.Fatalf("restart on a .foreman-kit/daemon.yaml without cycle.every (rc=%d): stdout %q stderr %q", rc, out.String(), errOut.String())
	}
	if !store.IsPIDAlive(running.Process.Pid) {
		t.Fatal("the restart stopped the daemon that was running")
	}
	if dl := liveDaemonLock(ctx, st); dl == nil || dl.PID != running.Process.Pid {
		t.Fatalf("the daemon that was running lost its lock: %+v", dl)
	}
}

// `foreman-kit status` says, with the pause, when a configuration file keeps work from starting.
func TestStatusSaysWhenAConfigurationFileKeepsWorkFromStarting(t *testing.T) {
	root := bareApp(t)
	st := openTestStore(t, root)
	var out bytes.Buffer
	if err := Status(context.Background(), root, st, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "PAUSED: NO\nCONFIG: OK\n") {
		t.Fatalf("status on whole files:\n%s", out.String())
	}
	withoutLine(t, root, "agents", "idle_cpu_percent: 5")
	out.Reset()
	if err := Status(context.Background(), root, st, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "CONFIG: .foreman-kit/agents.yaml: limits.idle_cpu_percent: missing (the kit ships 5) (foreman-kit config check); what reads that file starts nothing new until it is mended\n") {
		t.Fatalf("status on a .foreman-kit/agents.yaml that lacks a key:\n%s", out.String())
	}
}
