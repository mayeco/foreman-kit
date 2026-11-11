package portal

import (
	"context"
	"net/http"
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

// These tests call the service that starts and stops what the portal launches, without HTTP.

// waitExecution waits until the execution id is no longer running, and returns it.
func waitExecution(t *testing.T, st *store.Store, id string) store.Execution {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ex, err := st.GetExecution(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if ex != nil && ex.Status != "running" {
			return *ex
		}
		if time.Now().After(deadline) {
			t.Fatalf("the execution %s is still running: %+v", id, ex)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitWork waits until the work of an execution this process started is over.
func waitWork(t *testing.T, x *executions, id string) {
	t.Helper()
	x.mu.Lock()
	re := x.running[id]
	x.mu.Unlock()
	if re == nil {
		return
	}
	select {
	case <-re.done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the work of %s did not end", id)
	}
}

func statusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	return answerOf(err).status
}

func TestAnAgentRunIsRecordedWithItsPromptItsOutputAndItsEnd(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	ctx := context.Background()

	ex, err := srv.exec.StartAgent(ctx, AgentRun{Engine: "claude", Prompt: "  Check the status of the system  "})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Kind != "portal_run" || ex.Status != "running" || ex.PromptText != "Check the status of the system" || ex.PID != os.Getpid() {
		t.Fatalf("the execution an agent run starts as = %+v", ex)
	}
	if real, _ := filepath.EvalSymlinks(root); ex.Workdir != root && ex.Workdir != real {
		t.Errorf("an agent run with no change runs in %q, want the root of the workspace %q", ex.Workdir, root)
	}
	done := waitExecution(t, st, ex.ExecID)
	if done.Status != "succeeded" || done.ExitCode != 0 {
		t.Fatalf("the agent run ended as %s (exit %d)", done.Status, done.ExitCode)
	}
	if out, _ := st.ReadFullLog(ctx, ex.ExecID); !strings.Contains(out, "executed prompt: Check the status of the system") {
		t.Errorf("the output of the run was not recorded: %q", out)
	}

	// With no engine asked for, it runs on the first one this application uses.
	ex, err = srv.exec.StartAgent(ctx, AgentRun{Prompt: "hello"})
	if err != nil || ex.Agent != "claude" {
		t.Fatalf("an agent run with no engine = %+v, %v; want the first engine of the application", ex, err)
	}
	waitExecution(t, st, ex.ExecID)

	for name, in := range map[string]AgentRun{
		"an empty prompt":           {Engine: "claude", Prompt: "   "},
		"an engine nobody knows":    {Engine: "no-such-engine", Prompt: "hello"},
		"a name that is no change":  {Engine: "claude", Prompt: "hello", Change: "../etc"},
		"a change with no worktree": {Engine: "claude", Prompt: "hello", Change: "no-worktree-yet"},
	} {
		_, err := srv.exec.StartAgent(ctx, in)
		want := http.StatusBadRequest
		if name == "a change with no worktree" {
			want = http.StatusConflict
		}
		if statusOf(err) != want {
			t.Errorf("%s: %v (status %d), want %d", name, err, statusOf(err), want)
		}
	}
	if runs, _ := st.ListExecutions(ctx, "portal_run", "", 10); len(runs) != 2 {
		t.Errorf("a request that was refused left an execution behind: %d agent runs on record, want 2", len(runs))
	}
}

func TestAFailedRunSaysWhyTheKitCouldNotRunIt(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	ctx := context.Background()
	kitOf(srv).agent = func(context.Context, string, string, string) (string, int, error) {
		return "partial output", 0, exec.ErrNotFound
	}
	ex, err := srv.exec.StartAgent(ctx, AgentRun{Engine: "claude", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitExecution(t, st, ex.ExecID)
	if done.Status != "failed" || done.ExitCode == 0 {
		t.Fatalf("a run the kit could not run ended as %s (exit %d), want failed", done.Status, done.ExitCode)
	}
	out, _ := st.ReadFullLog(ctx, ex.ExecID)
	if !strings.Contains(out, "partial output\n") || !strings.Contains(out, exec.ErrNotFound.Error()) {
		t.Errorf("the output does not say why the run failed: %q", out)
	}
}

// An agent works in the root of the workspace or in the worktree of a change, and nowhere else: a
// request names a change, never a directory, and a worktree that is a link out of the workspace is
// refused.
func TestAnAgentRunsOnlyInsideTheWorkspace(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	ctx := context.Background()
	var ranIn string
	kitOf(srv).agent = func(_ context.Context, workdir, _, _ string) (string, int, error) {
		ranIn = workdir
		return "ok", 0, nil
	}

	wt := filepath.Join(root, ".worktrees", "worker-feat-alpha")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	ex, err := srv.exec.StartAgent(ctx, AgentRun{Engine: "claude", Prompt: "look at the worktree", Change: "feat-alpha"})
	if err != nil {
		t.Fatal(err)
	}
	waitExecution(t, st, ex.ExecID)
	waitWork(t, srv.exec, ex.ExecID)
	if ex.ChangeName != "feat-alpha" || filepath.Base(ex.Workdir) != "worker-feat-alpha" || ranIn != ex.Workdir {
		t.Fatalf("the agent ran in %q (recorded %q, change %q), want the worktree of feat-alpha", ranIn, ex.Workdir, ex.ChangeName)
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".worktrees", "worker-feat-escape")); err != nil {
		t.Fatal(err)
	}
	ranIn = ""
	if _, err := srv.exec.StartAgent(ctx, AgentRun{Engine: "claude", Prompt: "escape", Change: "feat-escape"}); statusOf(err) != http.StatusBadRequest {
		t.Fatalf("a worktree that links out of the workspace: %v (status %d), want it refused", err, statusOf(err))
	}
	if ranIn != "" {
		t.Fatalf("the agent ran in %q, outside the workspace", ranIn)
	}
}

func TestAnOperationOfTheKitRunsUnderItsName(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	ctx := context.Background()
	type call struct{ dir, operation string }
	calls := make(chan call, 4)
	kitOf(srv).operation = func(_ context.Context, dir, operation string) (string, int, error) {
		calls <- call{dir, operation}
		return operation + " done\n", 0, nil
	}

	ex, err := srv.exec.StartOperation(ctx, "merge", "")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Kind != "portal_job" || ex.JobKind != "merge" || ex.CommandText != "foreman-kit merge" {
		t.Fatalf("the execution of an operation = %+v", ex)
	}
	if got := <-calls; got.operation != "merge" {
		t.Fatalf("the kit ran %+v, want merge at the root", got)
	}
	if done := waitExecution(t, st, ex.ExecID); done.Status != "succeeded" {
		t.Fatalf("the operation ended as %s", done.Status)
	}

	// The gates of a change run in its worktree, under a name that says so.
	wt := filepath.Join(root, ".worktrees", "worker-demo")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	ex, err = srv.exec.StartOperation(ctx, "gates", "demo")
	if err != nil {
		t.Fatal(err)
	}
	got := <-calls
	realWT, _ := filepath.EvalSymlinks(wt)
	realDir, _ := filepath.EvalSymlinks(got.dir)
	if got.operation != "gates-demo" || realDir != realWT || ex.ChangeName != "demo" || ex.JobKind != "gates-demo" {
		t.Fatalf("the gates of a change ran as %+v (execution %+v), want gates-demo in its worktree", got, ex)
	}
	waitExecution(t, st, ex.ExecID)

	if _, err := srv.exec.StartOperation(ctx, "sh -c anything", ""); statusOf(err) != http.StatusBadRequest {
		t.Errorf("an operation the portal does not start: %v, want it refused", err)
	}
	if _, err := srv.exec.StartOperation(ctx, "dispatch", "demo"); statusOf(err) != http.StatusBadRequest {
		t.Errorf("the dispatcher in the worktree of a change: %v, want it refused", err)
	}
	if _, err := srv.exec.StartOperation(ctx, "gates", "no-worktree-yet"); statusOf(err) != http.StatusConflict {
		t.Errorf("the gates of a change with no worktree: %v, want a conflict", err)
	}
}

func TestAFollowUpContinuesARunAndARerunRepeatsIt(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	ctx := context.Background()
	x := srv.exec

	first, err := x.StartAgent(ctx, AgentRun{Engine: "hermes", Prompt: "hello from the portal"})
	if err != nil {
		t.Fatal(err)
	}
	waitExecution(t, st, first.ExecID)
	waitWork(t, x, first.ExecID)

	// A follow-up with context carries the request and the output it continues.
	cont, err := x.Continue(ctx, first.ExecID, Continuation{Prompt: "and now step two", WithContext: true})
	if err != nil {
		t.Fatal(err)
	}
	if cont.ParentExecID != first.ExecID || cont.ParentRelation != "continue" || cont.Summary != "and now step two" || cont.Agent != "hermes" {
		t.Fatalf("the follow-up = %+v", cont)
	}
	for _, want := range []string{"hello from the portal", "executed prompt: hello from the portal", "and now step two"} {
		if !strings.Contains(cont.PromptText, want) {
			t.Errorf("the prompt of a follow-up with context lacks %q:\n%s", want, cont.PromptText)
		}
	}
	// Without context it is the follow-up alone, on the engine asked for.
	bare, err := x.Continue(ctx, first.ExecID, Continuation{Prompt: "only this", Engine: "claude"})
	if err != nil || bare.PromptText != "only this" || bare.Agent != "claude" {
		t.Fatalf("a follow-up without context = %+v, %v", bare, err)
	}

	again, err := x.Rerun(ctx, first.ExecID, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.ParentExecID != first.ExecID || again.ParentRelation != "rerun" || again.PromptText != "hello from the portal" || again.Agent != "hermes" {
		t.Fatalf("the rerun = %+v", again)
	}
	for _, id := range []string{cont.ExecID, bare.ExecID, again.ExecID} {
		waitExecution(t, st, id)
	}

	if _, err := x.Continue(ctx, first.ExecID, Continuation{Prompt: "  "}); statusOf(err) != http.StatusBadRequest {
		t.Errorf("an empty follow-up: %v, want it refused", err)
	}
	if _, err := x.Continue(ctx, "no-such-execution", Continuation{Prompt: "x"}); statusOf(err) != http.StatusNotFound {
		t.Errorf("a follow-up to an execution that does not exist: %v, want not found", err)
	}
	op, err := x.StartOperation(ctx, "dispatch", "")
	if err != nil {
		t.Fatal(err)
	}
	waitExecution(t, st, op.ExecID)
	if _, err := x.Continue(ctx, op.ExecID, Continuation{Prompt: "x"}); statusOf(err) != http.StatusConflict {
		t.Errorf("a follow-up to an operation of the kit: %v, want a conflict", err)
	}
	if _, err := x.Rerun(ctx, op.ExecID, ""); statusOf(err) != http.StatusConflict {
		t.Errorf("a rerun of an operation of the kit: %v, want a conflict", err)
	}
}

// An execution does not depend on the request that started it: the request ends, or its client goes
// away, and the work goes on and is recorded when it ends.
func TestAnExecutionOutlivesTheRequestThatStartedIt(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	release := make(chan struct{})
	kitOf(srv).agent = func(ctx context.Context, _, _, _ string) (string, int, error) {
		select {
		case <-release:
			return "the work was finished\n", 0, nil
		case <-ctx.Done():
			return "the work was cut short\n", 143, ctx.Err()
		}
	}

	request, disconnect := context.WithCancel(context.Background())
	ex, err := srv.exec.StartAgent(request, AgentRun{Engine: "claude", Prompt: "do the work"})
	if err != nil {
		t.Fatal(err)
	}
	disconnect()
	time.Sleep(50 * time.Millisecond)
	close(release)

	done := waitExecution(t, st, ex.ExecID)
	if done.Status != "succeeded" {
		t.Fatalf("the run of a request that went away ended as %s (exit %d), want succeeded", done.Status, done.ExitCode)
	}
	if out, _ := st.ReadFullLog(context.Background(), ex.ExecID); !strings.Contains(out, "the work was finished") {
		t.Fatalf("the output of the run was not recorded: %q", out)
	}
}

// TestAStopAndTheEndOfTheWorkNeverWriteOverEachOther stops executions at the moment they end by
// themselves. Whoever records the end first has it. A stop that answered without an error stopped
// the execution; one that came after the work had been recorded says so and changes nothing.
func TestAStopAndTheEndOfTheWorkNeverWriteOverEachOther(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	ctx := context.Background()
	x := srv.exec

	stops, late := 0, 0
	const rounds = 200
	for i := 0; i < rounds; i++ {
		ex, err := x.StartOperation(ctx, "dispatch", "")
		if err != nil {
			t.Fatal(err)
		}
		x.mu.Lock()
		re := x.running[ex.ExecID]
		x.mu.Unlock()

		// Sweep the stop across the moment the work ends and is recorded.
		time.Sleep(time.Duration(i%20) * 50 * time.Microsecond)
		stopped, err := x.Stop(ctx, ex.ExecID)
		if re != nil {
			select {
			case <-re.done:
			case <-time.After(30 * time.Second):
				t.Fatalf("round %d: the work did not end", i)
			}
		}
		got, gerr := st.GetExecution(ctx, ex.ExecID)
		if gerr != nil || got == nil {
			t.Fatalf("round %d: %v", i, gerr)
		}
		switch statusOf(err) {
		case http.StatusOK:
			stops++
			if stopped.Status != "stopped" || got.Status != "stopped" || got.ExitCode != stoppedExitCode {
				t.Fatalf("round %d: the stop answered, and the execution is recorded as %s (exit %d)", i, got.Status, got.ExitCode)
			}
		case http.StatusConflict:
			late++
			if got.Status != "succeeded" || got.ExitCode != 0 {
				t.Fatalf("round %d: the stop came late (%v), and the execution is recorded as %s (exit %d), want as it ended", i, err, got.Status, got.ExitCode)
			}
		default:
			t.Fatalf("round %d: Stop: %v", i, err)
		}
	}
	t.Logf("%d stops landed before the work was recorded, %d after", stops, late)
}

func startSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return pid
}

// The process an execution names is the portal that started it. Stop never signals it: for an
// execution of another portal that is alive it refuses, and for one whose portal died it closes
// the record.
func TestStopNeverSignalsTheProcessAnExecutionNames(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	ctx := context.Background()

	otherPortal := startSleeper(t)
	if err := st.CreateExecution(ctx, store.Execution{ExecID: "run-of-another-portal", Kind: "portal_run", Status: "running", PID: otherPortal}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.exec.Stop(ctx, "run-of-another-portal"); statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), strconv.Itoa(otherPortal)) {
		t.Fatalf("stopping the run of another live portal: %v, want a conflict that names its process", err)
	}
	time.Sleep(150 * time.Millisecond)
	if !store.IsPIDAlive(otherPortal) {
		t.Fatal("Stop signaled the process that started the execution (another portal)")
	}
	if ex, _ := st.GetExecution(ctx, "run-of-another-portal"); ex == nil || ex.Status != "running" {
		t.Fatalf("the run of another live portal is recorded as %+v, want still running", ex)
	}

	// A portal that died left its execution marked as running: stopping it closes the record.
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateExecution(ctx, store.Execution{ExecID: "run-of-a-dead-portal", Kind: "portal_job", Status: "running", PID: dead.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	stopped, err := srv.exec.Stop(ctx, "run-of-a-dead-portal")
	if err != nil || stopped.Status != "stopped" || stopped.ExitCode != stoppedExitCode {
		t.Fatalf("stopping the execution of a dead portal = %+v, %v", stopped, err)
	}

	// What the portal did not start, it does not stop; nor what is over, nor what does not exist.
	if err := st.CreateExecution(ctx, store.Execution{ExecID: "worker-build", Kind: "worker", ChangeName: "feat-alpha", Status: "running", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.exec.Stop(ctx, "worker-build"); statusOf(err) != http.StatusConflict {
		t.Errorf("stopping the execution of a worker: %v, want a conflict", err)
	}
	if _, err := srv.exec.Stop(ctx, "run-of-a-dead-portal"); statusOf(err) != http.StatusConflict {
		t.Errorf("stopping an execution that is over: %v, want a conflict", err)
	}
	if _, err := srv.exec.Stop(ctx, "no-such-execution"); statusOf(err) != http.StatusNotFound {
		t.Errorf("stopping an execution that does not exist: %v, want not found", err)
	}
}

// TestShutdownStopsTheProcessesThePortalStarted: shutting the portal down cancels what it launched
// and waits for it, so no process outlives the portal or the daemon that hosts it. The Kit is the
// real one, running a stand-in for the foreman-kit executable.
func TestShutdownStopsTheProcessesThePortalStarted(t *testing.T) {
	_, st, root := setupTestPortal(t)
	ctx := context.Background()
	pidFile := filepath.Join(t.TempDir(), "pid")
	exe := writeExecutable(t, t.TempDir(), "foreman-kit", `echo $$ > "`+pidFile+`"; echo "started $1"; exec sleep 30`)
	srv, err := New(root, st, NewKit(st, exe))
	if err != nil {
		t.Fatal(err)
	}
	_, shutdown, err := srv.StartBackground(ctx, "127.0.0.1", 47600, false)
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	ex, err := srv.exec.StartOperation(ctx, "dispatch", "")
	if err != nil {
		t.Fatal(err)
	}

	pid := 0
	for deadline := time.Now().Add(10 * time.Second); pid == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if raw, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
	}
	if pid == 0 {
		t.Fatal("the operation never started its process")
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	// What the process prints is on record while it runs, not only when it ends.
	var out string
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(out, "started dispatch") && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		out, _ = st.ReadFullLog(ctx, ex.ExecID)
	}
	if !strings.Contains(out, "started dispatch") {
		t.Fatalf("the output of a running operation is not on record: %q", out)
	}

	sdCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	started := time.Now()
	_ = shutdown(sdCtx)
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("the shutdown took %v", elapsed)
	}
	if store.IsPIDAlive(pid) {
		t.Fatalf("the process %d outlived the shutdown of the portal", pid)
	}
	if got, _ := st.GetExecution(ctx, ex.ExecID); got == nil || got.Status != "stopped" {
		t.Fatalf("the execution is recorded as %+v, want stopped", got)
	}
}

// A request still in flight when the portal starts shutting down may register its execution after
// the shutdown canceled the ones it found. Here the request waits on the INSERT of its execution
// while the one writer connection of the store is busy; the write goes through once the shutdown
// has canceled what was registered before. The late execution is stopped like the others.
func TestAnExecutionRegisteredAfterTheShutdownBeganIsCanceled(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	canceled := make(chan struct{})
	kitOf(srv).agent = func(ctx context.Context, _, _, _ string) (string, int, error) {
		select {
		case <-ctx.Done():
			close(canceled)
			return "", 143, nil
		case <-time.After(30 * time.Second):
			return "the agent finished", 0, nil
		}
	}
	ctx := context.Background()
	url, shutdown, err := srv.StartBackground(ctx, "127.0.0.1", 47650, false)
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}

	// An execution registered before the shutdown: its cancellation tells the test that the shutdown
	// has collected and canceled everything registered so far.
	collected := make(chan struct{})
	earlier := srv.exec.register("registered-before-shutdown", func() { close(collected) })

	writer, err := st.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = writer.Close()
		}
	}
	defer release()

	reqDone := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, url+"/api/executions", strings.NewReader(`{"engine": "claude", "prompt": "run while the portal shuts down"}`))
		if err != nil {
			reqDone <- err
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		reqDone <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); st.DB().Stats().WaitCount == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the request never reached the INSERT of its execution")
		}
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		sdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_ = shutdown(sdCtx)
	}()
	select {
	case <-collected:
	case <-time.After(10 * time.Second):
		t.Fatal("the shutdown never canceled the executions registered before it")
	}
	srv.exec.unregister("registered-before-shutdown", earlier)
	// The execution of the request in flight registers now, after the shutdown collected the others.
	release()

	select {
	case <-canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("an execution registered after the portal began shutting down was never canceled")
	}
	select {
	case <-shutdownDone:
	case <-time.After(20 * time.Second):
		t.Fatal("the shutdown did not return once the execution was canceled")
	}
	if err := <-reqDone; err != nil {
		t.Fatalf("the request in flight failed: %v", err)
	}
	runs, err := st.ListExecutions(ctx, "portal_run", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != "stopped" {
		t.Fatalf("want the execution recorded as stopped, got %+v", runs)
	}
}

func TestEnginesAPromptCanRunOn(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	engines := srv.exec.Engines()
	if len(engines) < 5 {
		t.Fatalf("the engines the kit knows = %+v", engines)
	}
	selected := map[string]bool{}
	for _, e := range engines {
		if e.Label == "" {
			t.Errorf("the engine %s has no label", e.Name)
		}
		if e.Selected {
			selected[e.Name] = true
		}
	}
	// The test workspace was set up with claude and hermes.
	if len(selected) != 2 || !selected["claude"] || !selected["hermes"] {
		t.Errorf("the engines this application uses = %v, want claude and hermes", selected)
	}
	if got := srv.exec.DefaultEngine(); got != "claude" {
		t.Errorf("the engine a prompt runs on by default = %q, want the first one of the application", got)
	}
}
