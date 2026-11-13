package proc

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// giveCommands writes commands.<name> of the workspace's .foreman-kit/agents.yaml.
func giveCommands(t *testing.T, root, name string, entry map[string]any) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(config.AgentsFile))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := yaml.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	commands, _ := data["commands"].(map[string]any)
	if commands == nil {
		commands = map[string]any{}
	}
	commands[name] = entry
	data["commands"] = commands
	out, err := yaml.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runDetachedCall(t *testing.T, root string, engine string) (*AgentExecResult, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	res, err := AgentExec(context.Background(), AgentExecOptions{
		Workdir:      root,
		AppRoot:      root,
		Prompt:       "do the work",
		Engines:      engine,
		Change:       "detached-change",
		Phase:        "apply",
		SkipEvent:    true,
		WatchdogPoll: 100 * time.Millisecond,
		GracePeriod:  300 * time.Millisecond,
		Stdout:       &out,
		Stderr:       &errOut,
	})
	if err != nil && res == nil {
		t.Fatalf("AgentExec: %v", err)
	}
	return res, errOut.String()
}

// A detached agent's client prints nothing and uses no CPU while its service works: the files its
// entry lists in activity keep the call alive, and without them the same call stalls.
func TestAgentExecWatchesADetachedCallThroughItsActivity(t *testing.T) {
	root, _, binDir := stallTestWorkspace(t, "exec-detached-activity", "detached-change")
	settingstest.Give(t, settings.AgentRetries, "0")
	settingstest.Give(t, settings.IdleTimeout, "1s")
	writeEngine(t, binDir, "gw", "sleep 3\necho the gateway finished\n")

	session := filepath.Join(t.TempDir(), "state.db")
	giveCommands(t, root, "gw", map[string]any{"run": "gw {{args}}", "detached": true, "stop": "true", "activity": []any{session}})

	// The service: outside the process tree of the call, it records progress every 200ms.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
				f, err := os.OpenFile(session, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err == nil {
					_, _ = f.WriteString("reply " + strconv.Itoa(i) + "\n")
					_ = f.Close()
				}
			}
		}
	}()
	res, stderr := runDetachedCall(t, root, "gw")
	close(stop)
	wg.Wait()
	if res.ExitCode != 0 || strings.Contains(stderr, "stalled") {
		t.Fatalf("a detached call whose session store grew was stopped as idle: exit %d\n%s", res.ExitCode, stderr)
	}

	// The same call without activity files: nothing it can see moves, and it stalls.
	giveCommands(t, root, "gw", map[string]any{"run": "gw {{args}}", "detached": true, "stop": "true"})
	res, stderr = runDetachedCall(t, root, "gw")
	if res.ExitCode == 0 || !strings.Contains(stderr, "gw stalled") {
		t.Fatalf("a detached call without activity files was not stopped as idle: exit %d\n%s", res.ExitCode, stderr)
	}
}

// startLeftover is what a detached agent's service leaves behind: a process of its own session,
// out of the call's process group, whose current directory is the work tree. It writes its pid to
// pidFile.
func leftoverScript(pidFile string) string {
	return "cd \"$FOREMANKIT_WORKDIR\"\n" +
		"perl -MPOSIX -e 'POSIX::setsid(); open(my $f, \">\", $ARGV[0]); print $f $$; close $f; exec \"sleep\", \"60\"' \"" + pidFile + "\" </dev/null >/dev/null 2>&1 &\n" +
		"sleep 30\n"
}

func killLeftover(t *testing.T, pidFile string) {
	t.Helper()
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(b)) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s was never written", path)
}

// When the kit stops a detached call, it runs the agent's stop command: one that stops what the
// call left in the work tree lets the next call start there.
func TestAgentExecRunsTheStopCommandOfAStoppedDetachedCall(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl starts the leftover process in a session of its own")
	}
	root, _, binDir := stallTestWorkspace(t, "exec-detached-stop", "detached-change")
	settingstest.Give(t, settings.AgentRetries, "0")
	settingstest.Give(t, settings.IdleTimeout, "1s")
	pidFile := filepath.Join(t.TempDir(), "leftover.pid")
	killLeftover(t, pidFile)
	writeEngine(t, binDir, "gw", leftoverScript(pidFile))
	writeEngine(t, binDir, "okagent", "echo done\n")
	stopped := filepath.Join(t.TempDir(), "stopped")
	giveCommands(t, root, "gw", map[string]any{
		"run":      "gw {{args}}",
		"detached": true,
		"stop":     []any{"bash", "-c", "echo {{workdir}} {{pid}} > " + stopped + "; kill $(cat " + pidFile + ")"},
	})
	giveCommands(t, root, "okagent", map[string]any{"run": "okagent {{args}}"})

	res, stderr := runDetachedCall(t, root, "gw")
	if res.ExitCode == 0 || !strings.Contains(stderr, "gw stalled") || !strings.Contains(stderr, "ran the stop command of gw") {
		t.Fatalf("the stalled detached call was not stopped with its stop command: exit %d\n%s", res.ExitCode, stderr)
	}
	b, err := os.ReadFile(stopped)
	if err != nil {
		t.Fatalf("the stop command did not run: %v", err)
	}
	if fields := strings.Fields(string(b)); len(fields) != 2 || fields[0] != root || fields[1] == "0" {
		t.Fatalf("the stop command got %q, want the work directory and the pid of the call", string(b))
	}

	res, stderr = runDetachedCall(t, root, "okagent")
	if res.ExitCode != 0 {
		t.Fatalf("the next call did not start although the stop command ended the leftover: exit %d\n%s", res.ExitCode, stderr)
	}
}

// A stopped detached call whose processes still work in the work tree keeps any other call from
// starting there: it exits ExitWorkdirBusy and names them.
func TestAgentExecStartsNoCallOverAStoppedDetachedCallStillWorking(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl starts the leftover process in a session of its own")
	}
	root, _, binDir := stallTestWorkspace(t, "exec-detached-busy", "detached-change")
	settingstest.Give(t, settings.AgentRetries, "0")
	settingstest.Give(t, settings.IdleTimeout, "1s")
	pidFile := filepath.Join(t.TempDir(), "leftover.pid")
	killLeftover(t, pidFile)
	writeEngine(t, binDir, "gw", leftoverScript(pidFile))
	calls := filepath.Join(t.TempDir(), "okagent.calls")
	writeEngine(t, binDir, "okagent", "echo call >> \""+calls+"\"\necho done\n")
	giveCommands(t, root, "gw", map[string]any{"run": "gw {{args}}", "detached": true, "stop": "true"})
	giveCommands(t, root, "okagent", map[string]any{"run": "okagent {{args}}"})

	if res, stderr := runDetachedCall(t, root, "gw"); res.ExitCode == 0 {
		t.Fatalf("the detached call was expected to stall:\n%s", stderr)
	}
	waitForFile(t, pidFile)

	res, stderr := runDetachedCall(t, root, "okagent")
	if res.ExitCode != ExitWorkdirBusy {
		t.Fatalf("a call started over a stopped detached call still working: exit %d\n%s", res.ExitCode, stderr)
	}
	if !strings.Contains(stderr, "its processes still work there") || !strings.Contains(stderr, "sleep") {
		t.Fatalf("the refusal does not name the process still working:\n%s", stderr)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Fatal("the agent of the refused call ran")
	}

	// Once the leftover ends, the next call starts.
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	_ = syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(200 * time.Millisecond)
	if res, stderr := runDetachedCall(t, root, "okagent"); res.ExitCode != 0 {
		t.Fatalf("the call did not start once the leftover ended: exit %d\n%s", res.ExitCode, stderr)
	}
}

func TestParseLsofCwdKeepsTheProcessesInsideTheDirectory(t *testing.T) {
	out := "p10\ncbash\nn/work/tree\np11\ncsleep\nn/work/tree/repos/a\np12\nczsh\nn/work/tree-other\np13\nn/elsewhere\n"
	got := parseLsofCwd(out, "/work/tree")
	if len(got) != 2 || got[0].PID != 10 || got[0].Command != "bash" || got[1].PID != 11 || got[1].Dir != "/work/tree/repos/a" {
		t.Fatalf("parseLsofCwd = %+v, want pids 10 and 11 only", got)
	}
}

func TestProcessesInFindsAProcessWorkingInTheDirectory(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("sleep", "30")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	found := ProcessesIn(context.Background(), dir)
	ok := false
	for _, p := range found {
		if p.PID == cmd.Process.Pid {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("ProcessesIn(%s) = %+v, missing pid %d", dir, found, cmd.Process.Pid)
	}
	if left := WaitDirFree(context.Background(), dir, 200*time.Millisecond, 50*time.Millisecond); len(left) == 0 {
		t.Fatal("WaitDirFree said the directory was free while a process works in it")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if left := WaitDirFree(context.Background(), dir, 2*time.Second, 50*time.Millisecond); len(left) != 0 {
		t.Fatalf("WaitDirFree still finds %+v after the process ended", left)
	}
}

func TestFilesStateChangesWhenAnActivityFileGrows(t *testing.T) {
	f := filepath.Join(t.TempDir(), "agent.log")
	before := FilesState([]string{f})
	if err := os.WriteFile(f, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	created := FilesState([]string{f})
	if created == before {
		t.Fatal("creating the file did not change the state")
	}
	if err := os.WriteFile(f, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if FilesState([]string{f}) == created {
		t.Fatal("growing the file did not change the state")
	}
	if FilesState(nil) != "" {
		t.Fatal("no files must give no state")
	}
}
