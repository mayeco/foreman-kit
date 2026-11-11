package proc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

// stallTestWorkspace is a workspace whose work tree stays clean while the store is written: the state
// database of a test workspace lives in the tree (a real one's does not), and every row written to it
// would otherwise read as a change in the agent's work directory.
func stallTestWorkspace(t *testing.T, execID, change string) (string, *store.Store, string) {
	t.Helper()
	root, st := setupProcTestWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte("state.db*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateExecution(context.Background(), store.Execution{
		ExecID:     execID,
		Kind:       "worker",
		ChangeName: change,
		Phase:      "apply",
		Status:     "running",
		PID:        os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	return root, st, binDir
}

func writeEngine(t *testing.T, binDir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/usr/bin/env bash\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func eventNames(t *testing.T, st *store.Store, execID string) []string {
	t.Helper()
	recs, err := st.QueryEvents(context.Background(), store.EventFilter{ExecID: execID})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range recs {
		names = append(names, r.Event)
	}
	return names
}

// TestAgentExecDoesNotRetryAnEngineThatStalled: an engine that froze was stopped at the idle limit and
// then tried again like any other failure. What froze it was still there, the second try sat through
// the idle limit as well, and a frozen agent cost its worker twice the limit (some 40 minutes with the
// defaults) before the next engine or the dispatcher's backoff had its turn.
func TestAgentExecDoesNotRetryAnEngineThatStalled(t *testing.T) {
	root, st, binDir := stallTestWorkspace(t, "exec-stall", "stall-change")
	settingstest.Give(t, settings.AgentRetries, "1")
	settingstest.Give(t, settings.AgentRetryAfter, "0s")
	settingstest.Give(t, settings.IdleTimeout, "1s")

	calls := filepath.Join(binDir, "claude.calls")
	writeEngine(t, binDir, "claude", "echo call >> \""+calls+"\"\nsleep 30\n")
	writeEngine(t, binDir, "hermes", "echo \"hermes did the work\" > \"$FOREMANKIT_WORKDIR/done.txt\"\necho hermes finished\n")

	var out, errOut bytes.Buffer
	res, err := AgentExec(context.Background(), AgentExecOptions{
		Workdir:      root,
		AppRoot:      root,
		Prompt:       "do the work",
		Engines:      "claude,hermes",
		Change:       "stall-change",
		Phase:        "apply",
		Store:        st,
		ExecID:       "exec-stall",
		SkipEvent:    true,
		WatchdogPoll: 100 * time.Millisecond,
		GracePeriod:  200 * time.Millisecond,
		Stdout:       &out,
		Stderr:       &errOut,
	})
	if err != nil || res == nil {
		t.Fatalf("AgentExec: res=%+v err=%v", res, err)
	}
	if res.ExitCode != 0 || res.UsedEngine != "hermes" {
		t.Fatalf("expected the next engine of the role to do the work, got exit %d by %q:\n%s", res.ExitCode, res.UsedEngine, res.Output)
	}
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "call") != 1 {
		t.Fatalf("the stalled engine was started %d times, want once:\n%s", strings.Count(string(b), "call"), res.Output)
	}
	if strings.Contains(res.Output, "trying claude again") {
		t.Fatalf("the stalled engine was tried again:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "claude stalled: no work") || !strings.Contains(res.Output, "claude stalled; not trying it again in this call") {
		t.Fatalf("the output does not say that the engine stalled and why it was not tried again:\n%s", res.Output)
	}

	attempts, err := st.ListAttempts(context.Background(), "exec-stall")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].Engine != "claude" || attempts[0].Outcome != store.AttemptIdleStalled ||
		attempts[0].IdleSeconds < 1 || attempts[1].Engine != "hermes" || attempts[1].Outcome != store.AttemptOK {
		t.Fatalf("attempts = %+v, want claude idle_stalled then hermes ok", attempts)
	}
	names := strings.Join(eventNames(t, st, "exec-stall"), " ")
	if !strings.Contains(names, "agent.stalled") {
		t.Fatalf("no agent.stalled event announces the stall: %s", names)
	}
}

// TestAgentExecWithOneEngineEndsAfterAStall: with no other engine in the role, the call ends after
// the one stall, as an engine outage the dispatcher waits out, instead of a second idle limit.
func TestAgentExecWithOneEngineEndsAfterAStall(t *testing.T) {
	root, st, binDir := stallTestWorkspace(t, "exec-stall-one", "stall-change")
	settingstest.Give(t, settings.AgentRetries, "2")
	settingstest.Give(t, settings.AgentRetryAfter, "0s")
	settingstest.Give(t, settings.IdleTimeout, "1s")

	calls := filepath.Join(binDir, "claude.calls")
	writeEngine(t, binDir, "claude", "echo call >> \""+calls+"\"\nsleep 30\n")

	var out, errOut bytes.Buffer
	started := time.Now()
	res, err := AgentExec(context.Background(), AgentExecOptions{
		Workdir:      root,
		AppRoot:      root,
		Prompt:       "do the work",
		Engines:      "claude",
		Change:       "stall-change",
		Phase:        "apply",
		Store:        st,
		ExecID:       "exec-stall-one",
		SkipEvent:    true,
		WatchdogPoll: 100 * time.Millisecond,
		GracePeriod:  200 * time.Millisecond,
		Stdout:       &out,
		Stderr:       &errOut,
	})
	if err != nil || res == nil {
		t.Fatalf("AgentExec: res=%+v err=%v", res, err)
	}
	if res.ExitCode != ExitEnginesDown {
		t.Fatalf("exit = %d, want %d (the only engine stalled and changed nothing):\n%s", res.ExitCode, ExitEnginesDown, res.Output)
	}
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "call") != 1 {
		t.Fatalf("the stalled engine was started %d times in %v, want once:\n%s", strings.Count(string(b), "call"), time.Since(started), res.Output)
	}
}

// TestAgentExecShowsAnAgentThatIsIdleWhileItIsIdle: nothing showed that an agent had gone quiet until
// the watchdog stopped it. From a quarter of the idle limit on, the attempt has an open idle period
// (what foreman-kit status and the portal read) and an agent.idle event; when the agent works again
// the period closes and agent.resumed says so.
func TestAgentExecShowsAnAgentThatIsIdleWhileItIsIdle(t *testing.T) {
	root, st, binDir := stallTestWorkspace(t, "exec-idle", "idle-change")
	settingstest.Give(t, settings.AgentRetries, "0")
	settingstest.Give(t, settings.IdleTimeout, "4s") // told from 1s on

	writeEngine(t, binDir, "claude", "sleep 2.5\necho \"the work\" > \"$FOREMANKIT_WORKDIR/done.txt\"\nsleep 0.5\necho finished\n")

	ctx := context.Background()
	var out, errOut bytes.Buffer
	done := make(chan *AgentExecResult, 1)
	go func() {
		res, _ := AgentExec(ctx, AgentExecOptions{
			Workdir:      root,
			AppRoot:      root,
			Prompt:       "do the work",
			Engines:      "claude",
			Change:       "idle-change",
			Phase:        "apply",
			Store:        st,
			ExecID:       "exec-idle",
			SkipEvent:    true,
			WatchdogPoll: 100 * time.Millisecond,
			GracePeriod:  200 * time.Millisecond,
			Stdout:       &out,
			Stderr:       &errOut,
		})
		done <- res
	}()

	var idle []store.IdleAgent
	deadline := time.Now().Add(20 * time.Second)
	for len(idle) == 0 {
		select {
		case res := <-done:
			t.Fatalf("the agent finished (%+v) and was never shown as idle", res)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the idle agent was never shown")
		}
		time.Sleep(20 * time.Millisecond)
		var err error
		if idle, err = st.ListIdleAgents(ctx); err != nil {
			t.Fatal(err)
		}
	}
	a := idle[0]
	if len(idle) != 1 || a.ExecID != "exec-idle" || a.ChangeName != "idle-change" || a.Phase != "apply" || a.Engine != "claude" ||
		a.Kind != "worker" || a.LimitSeconds != 4 || a.IdleSince == "" {
		t.Fatalf("idle agents = %+v, want the claude attempt of exec-idle with its limit of 4s", idle)
	}

	res := <-done
	if res == nil || res.ExitCode != 0 {
		t.Fatalf("the agent that went on working was not left to finish: %+v\n%s", res, out.String())
	}
	if idle, _ := st.ListIdleAgents(ctx); len(idle) != 0 {
		t.Fatalf("a finished agent is still shown as idle: %+v", idle)
	}
	periods, err := st.ListIdlePeriods(ctx, "exec-idle")
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != 1 || periods[0].EndedBy != store.IdleEndedByActivity || periods[0].Seconds < 1 || periods[0].LimitSeconds != 4 {
		t.Fatalf("idle periods = %+v, want one of a second or more that ended when the agent worked again", periods)
	}
	names := strings.Join(eventNames(t, st, "exec-idle"), " ")
	if !strings.Contains(names, "agent.idle") || !strings.Contains(names, "agent.resumed") || strings.Contains(names, "agent.stalled") {
		t.Fatalf("events = %s, want agent.idle then agent.resumed and no agent.stalled", names)
	}
}

func TestIdleNoticeAfter(t *testing.T) {
	for _, c := range []struct {
		limit, poll, want time.Duration
	}{
		{20 * time.Minute, 0, 5 * time.Minute},                 // a quarter of the limit
		{2 * time.Minute, 0, time.Minute},                      // polls every 30s: never at the first one
		{3 * time.Second, 0, 2 * time.Second},                  // polls every second at the least
		{4 * time.Second, 100 * time.Millisecond, time.Second}, // a custom poll
	} {
		if got := idleNoticeAfter(c.limit, c.poll); got != c.want {
			t.Errorf("idleNoticeAfter(%v, %v) = %v, want %v", c.limit, c.poll, got, c.want)
		}
	}
}
