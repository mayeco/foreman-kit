package portal

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/store"
)

func seedRichPortalWorkspace(t *testing.T, st *store.Store, root string) {
	t.Helper()
	ctx := context.Background()

	// Backlog, History, Proposals
	_ = cycle.WriteJSONList(filepath.Join(root, cycle.BacklogFile), []map[string]any{
		{"change_name": "feat-alpha", "priority": "high", "description": "Alpha feature", "queued_at": "2026-11-08T10:00:00Z"},
		{"change_name": "feat-waiting", "priority": "normal", "description": "Waiting on alpha", "depends_on": []any{"feat-alpha"}},
	})
	_ = cycle.WriteJSONList(filepath.Join(root, cycle.HistoryFile), []map[string]any{
		{"change_name": "feat-done", "priority": "high", "description": "Completed feature", "integrated_at": "2026-11-08T09:00:00Z"},
	})
	_ = cycle.WriteProposals(root, []map[string]any{
		{"change_name": "prop-beta", "status": "proposed", "source": "roadmap", "priority": "normal", "description": "Beta proposal"},
	})

	// Worktree with artifacts & tasks
	wtChangeDir := filepath.Join(root, ".worktrees", "worker-feat-alpha", "openspec", "changes", "feat-alpha")
	_ = os.MkdirAll(filepath.Join(wtChangeDir, "specs", "auth"), 0o755)
	_ = os.WriteFile(filepath.Join(wtChangeDir, "proposal.md"), []byte("# Proposal feat-alpha\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wtChangeDir, "design.md"), []byte("# Design feat-alpha\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wtChangeDir, "tasks.md"), []byte("- [x] 1.1 Task one\n- [ ] 1.2 Task two\n* [x] 1.3 Star task\n+ [ ] 1.4 Plus task\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wtChangeDir, "specs", "auth", "spec.md"), []byte("## ADDED Requirements\n"), 0o644)

	// Worker prompt & log files on disk (for I33 compatibility)
	workerLogDir := store.RuntimePath(root, "workers", "feat-alpha")
	_ = os.MkdirAll(workerLogDir, 0o755)
	_ = os.WriteFile(filepath.Join(workerLogDir, "20261108T110000Z-plan.prompt.md"), []byte("PLAN PROMPT FOR feat-alpha"), 0o644)
	_ = os.WriteFile(store.RuntimePath(root, "workers", "feat-alpha.log"), []byte("worker log line for feat-alpha\n"), 0o644)

	// Cycle logs and reports
	cycleRepDir := store.RuntimePath(root, "cycle", "reports")
	_ = os.MkdirAll(cycleRepDir, 0o755)
	_ = os.WriteFile(store.RuntimePath(root, "cycle", "cycle-20261108T120000Z.log"), []byte("== cycle 20261108T120000Z\npreflight: OK\nstrategy: security-and-access-control (defects)\n== judgment (claude)\n== done\n"), 0o644)
	_ = os.WriteFile(store.RuntimePath(root, "cycle", "prompt-20261108T120000Z.md"), []byte("# cycle prompt 20261108T120000Z\n"), 0o644)
	_ = os.WriteFile(filepath.Join(cycleRepDir, "20261108-1201.md"), []byte("# Cycle Report 20261108-1201\n"), 0o644)
	_ = os.WriteFile(store.RuntimePath(root, "cycle", "visual-20261108T123000Z.md"), []byte("# Visual QA Plan\n"), 0o644)
	_ = os.WriteFile(filepath.Join(cycleRepDir, "visual-20261108T123000Z.md"), []byte("# Visual QA Report\n"), 0o644)

	// A child repository, for the setup
	childDir := filepath.Join(root, "repos", "web")
	_ = os.MkdirAll(childDir, 0o755)
	_ = os.WriteFile(filepath.Join(childDir, "package.json"), []byte(`{"scripts":{"dev":"next dev","test":"vitest run"},"dependencies":{"next":"15"}}`), 0o644)
	_ = cycle.SetupRepo(root, "web", true, io.Discard)

	// SQLite records: Quarantine, ReadyQueue, Executions, LogChunks, Events, VisualQA, Locks
	_ = st.BlockChange(ctx, store.QuarantineRecord{
		ChangeName: "blocked-gamma",
		Reason:     "gate_failure",
		Details:    "lint failed",
		Retries:    1,
		Rebuilds:   2,
	})
	_ = st.EnqueueReady(ctx, store.ReadyManifest{
		ChangeName:   "ready-delta",
		Branch:       "feat/spec-ready-delta",
		Worktree:     filepath.Join(root, ".worktrees", "worker-ready-delta"),
		HeadSHA:      "1234567890abcdef1234567890abcdef12345678",
		Capabilities: []string{"auth"},
	})
	_ = st.CreateExecution(ctx, store.Execution{
		ExecID:    "cycle-20261108T120000Z",
		Kind:      "cycle",
		Agent:     "claude",
		Status:    "succeeded",
		StartedAt: "2026-11-08T12:00:00Z",
	})
	_ = st.AppendLogChunk(ctx, "cycle-20261108T120000Z", "combined", "== cycle 20261108T120000Z\npreflight: OK\nstrategy: security-and-access-control (defects)\n== done\n")

	_ = st.CreateExecution(ctx, store.Execution{
		ExecID:     "20261108T110000Z-plan",
		Kind:       "worker",
		ChangeName: "feat-alpha",
		Phase:      "plan",
		Agent:      "claude",
		Status:     "succeeded",
		PromptText: "PLAN PROMPT FOR feat-alpha",
		StartedAt:  "2026-11-08T11:00:00Z",
	})
	_ = st.AppendLogChunk(ctx, "20261108T110000Z-plan", "combined", "plan output for feat-alpha\n")

	_ = st.EnqueueVisualQA(ctx, store.VisualQAItem{
		ItemID:     "1",
		ChangeName: "feat-done",
		What:       "integrated in abcdef123456",
		Status:     "pending",
		CreatedAt:  "2026-11-08T09:05:00Z",
	})

	_ = st.EmitEvent(ctx, store.EventInput{
		Event:  "agent.exit",
		Change: "feat-alpha",
		Phase:  "plan",
		Engine: "claude",
		Payload: map[string]any{
			"change":         "feat-alpha",
			"phase":          "plan",
			"code":           0,
			"engine":         "claude",
			"prompt_version": "111111111111",
			"memory_version": "222222222222",
			"seconds":        12,
		},
	})
	_ = st.EmitEvent(ctx, store.EventInput{
		Event:  "evaluator.answer",
		Change: "feat-alpha",
		Payload: map[string]any{
			"gate":     "scenario_coverage",
			"change":   "feat-alpha",
			"mode":     "advisory",
			"status":   "passed",
			"answers":  `{"every_scenario_tested":0.95}`,
			"cost_usd": 0.0002,
			"cached":   false,
		},
	})
}

// TestStressPortalConcurrentRESTEndpoints hammers all REST GET endpoints with 25 concurrent worker goroutines
// while a background writer mutates SQLite and config files, verifying 0 errors and valid JSON responses.
func TestStressPortalConcurrentRESTEndpoints(t *testing.T) {
	ctx := context.Background()
	srv, st, root := setupTestPortal(t)
	seedRichPortalWorkspace(t, st, root)

	// An agent run and an operation of the kit, so that the reads of an execution have both kinds.
	run1, err := srv.exec.StartAgent(ctx, AgentRun{Engine: "claude", Prompt: "initial diagnostic prompt"})
	if err != nil {
		t.Fatalf("StartAgent failed: %v", err)
	}
	run1ID := run1.ExecID
	job1, err := srv.exec.StartOperation(ctx, "gates", "")
	if err != nil {
		t.Fatalf("StartOperation failed: %v", err)
	}
	job1ID := job1.ExecID
	for _, id := range []string{run1ID, job1ID} {
		waitExecution(t, st, id)
		waitWork(t, srv.exec, id)
	}

	endpoints := []string{
		"/api/dashboard",
		"/api/inbox",
		"/api/system/processes",
		"/api/agents",
		"/api/changes",
		"/api/changes?state=building&q=feat",
		"/api/changes/feat-alpha",
		"/api/changes/feat-alpha?limit=5",
		"/api/changes/feat-alpha/events",
		"/api/changes/feat-alpha/events?event=agent.exit&limit=5",
		"/api/changes/feat-alpha/files/proposal.md",
		"/api/executions/20261108T110000Z-plan",
		"/api/executions",
		"/api/executions?kind=worker&change=feat-alpha",
		"/api/executions?kind=portal_run&status=succeeded&limit=5",
		"/api/executions/" + run1ID,
		"/api/executions/" + job1ID,
		"/api/executions/" + run1ID + "/log",
		"/api/executions/cycle-20261108T120000Z",
		"/api/executions/cycle-20261108T120000Z/log?limit=50",
		"/api/cycles",
		"/api/cycles/20261108T120000Z",
		"/api/events",
		"/api/events?change=feat-alpha",
		"/api/events?event=agent.exit&limit=5",
		"/api/backlog",
		"/api/backlog?status=approved",
		"/api/merge",
		"/api/inbox?kind=blocked",
		"/api/system/settings",
		"/api/system/recommendations",
		"/api/system/health",
		"/api/system/setup",
		"/api/quality",
		"/api/visual",
		"/api/system/prompts",
		"/api/system/prompts/build",
	}

	// Verify path traversal is blocked with 404
	for _, badPath := range []string{
		"/api/changes/feat-alpha/files/..%2F..%2Fconfig%2Fagents.yaml",
		"/api/changes/feat-alpha/files/specs%2F..%2F..%2F..%2F..%2Fconfig%2Fagents.yaml",
		"/api/changes/feat-alpha/files/not-an-artifact.txt",
		"/api/changes/..%2Ffeat-alpha/files/proposal.md",
		"/api/nope",
	} {
		code, _, _, _ := doReq(t, srv, http.MethodGet, badPath, nil)
		if code != http.StatusNotFound {
			t.Fatalf("expected 404 for %s, got %d", badPath, code)
		}
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()
	client := &http.Client{Timeout: 10 * time.Second}

	stopMutator := make(chan struct{})
	var mutatorWG sync.WaitGroup
	mutatorWG.Add(1)
	go func() {
		defer mutatorWG.Done()
		seq := 10
		for {
			select {
			case <-stopMutator:
				return
			default:
				seq++
				_ = st.EmitEvent(ctx, store.EventInput{
					Event:   "worker.heartbeat",
					Change:  "feat-alpha",
					Payload: map[string]any{"seq": seq},
				})
				_ = st.AppendLogChunk(ctx, "cycle-20261108T120000Z", "combined", fmt.Sprintf("line %d\n", seq))
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	const workers = 25
	const rounds = 4
	errCh := make(chan error, workers*rounds*len(endpoints))
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				for idx, ep := range endpoints {
					target := ts.URL + ep
					resp, err := client.Get(target)
					if err != nil {
						errCh <- fmt.Errorf("worker %d round %d GET %s error: %v", workerID, r, ep, err)
						continue
					}
					body, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						errCh <- fmt.Errorf("worker %d round %d GET %s status=%d body=%s", workerID, r, ep, resp.StatusCode, string(body))
						continue
					}
					var parsed map[string]any
					if err := json.Unmarshal(body, &parsed); err != nil || len(parsed) == 0 {
						errCh <- fmt.Errorf("worker %d round %d GET %s invalid/empty JSON (idx=%d): %v body=%s", workerID, r, ep, idx, err, string(body))
					}
				}
			}
		}(w)
	}

	wg.Wait()
	close(stopMutator)
	mutatorWG.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}
}

// TestStressPortalConcurrentSSEStreaming tests real-time SSE streaming on /api/executions/EXEC_ID/log/stream
// with multiple concurrent clients receiving chunks as they are inserted.
func TestStressPortalConcurrentSSEStreaming(t *testing.T) {
	ctx := context.Background()
	srv, st, _ := setupTestPortal(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	execID := "exec-live-sse-stress"
	if err := st.CreateExecution(ctx, store.Execution{
		ExecID:     execID,
		Kind:       "portal_run",
		ChangeName: "feat-sse",
		Agent:      "claude",
		Status:     "running",
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("CreateExecution failed: %v", err)
	}

	const numClients = 10
	const numChunks = 15

	var logClientsReady sync.WaitGroup
	var logClientsDone sync.WaitGroup
	logClientsReady.Add(numClients)
	logClientsDone.Add(numClients)
	logErrCh := make(chan error, numClients)

	for c := 0; c < numClients; c++ {
		go func(clientID int) {
			defer logClientsDone.Done()
			reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, ts.URL+"/api/executions/"+execID+"/log/stream", nil)
			if err != nil {
				logErrCh <- err
				logClientsReady.Done()
				return
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				logErrCh <- fmt.Errorf("log client %d connect failed: %v", clientID, err)
				logClientsReady.Done()
				return
			}
			defer resp.Body.Close()
			logClientsReady.Done()

			var receivedChunks []string
			gotDone := false
			scanner := bufio.NewScanner(resp.Body)
			var curEvent string
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "event: ") {
					curEvent = strings.TrimPrefix(line, "event: ")
				} else if strings.HasPrefix(line, "data: ") {
					data := strings.TrimPrefix(line, "data: ")
					switch curEvent {
					case "log":
						var chunk store.LogChunk
						if err := json.Unmarshal([]byte(data), &chunk); err == nil {
							receivedChunks = append(receivedChunks, strings.TrimSpace(chunk.Content))
						}
					case "done":
						gotDone = true
					}
				}
			}
			if len(receivedChunks) != numChunks {
				logErrCh <- fmt.Errorf("log client %d expected %d chunks, got %d (%v)", clientID, numChunks, len(receivedChunks), receivedChunks)
				return
			}
			for i := 0; i < numChunks; i++ {
				want := fmt.Sprintf("live-chunk-%02d", i+1)
				if receivedChunks[i] != want {
					logErrCh <- fmt.Errorf("log client %d chunk %d got %q, want %q", clientID, i, receivedChunks[i], want)
					return
				}
			}
			if !gotDone {
				logErrCh <- fmt.Errorf("log client %d did not receive event: done", clientID)
			}
		}(c)
	}

	// Wait for every client to have its stream open
	logClientsReady.Wait()

	// Insert chunks in real time
	for i := 1; i <= numChunks; i++ {
		_ = st.AppendLogChunk(ctx, execID, "combined", fmt.Sprintf("live-chunk-%02d\n", i))
		time.Sleep(15 * time.Millisecond)
	}
	_ = st.FinishExecution(ctx, execID, "succeeded", 0)

	logClientsDone.Wait()
	close(logErrCh)

	for err := range logErrCh {
		t.Fatal(err)
	}
}

// TestStressPortalInteractiveActionsAndStopExecution tests the full interactive lifecycle:
//   - an agent run -> live SSE log streaming -> stop (verifying it cancels the work in milliseconds
//     without touching os.Getpid() and records status=stopped in SQLite)
//   - an operation of the kit that is a real subprocess group (Setpgid: true) -> stop killing the
//     process group in milliseconds
//   - a follow-up and a rerun of an agent run, and stopping a follow-up that is running
//   - Pause/Resume/ResumeAndMerge, Unblock + ApproveRisk + ResetRetries, ReclaimLease, Worker Validate/Gates/Reset,
//     and concurrent Backlog & Proposals management
func TestStressPortalInteractiveActionsAndStopExecution(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := cycle.Init(ctx, root, cycle.KitOptions{
		Agents: "claude,hermes",
		Roles:  map[string]string{"plan": "claude", "build": "hermes", "cycle": "claude"},
	}, io.Discard); err != nil {
		t.Fatalf("cycle.Init failed: %v", err)
	}
	// This test queues more changes than the queue takes by default (queue.max_open), and it is not
	// about that cap: with it, the proposal approved below would wait for room instead of being queued.
	cyclePath := filepath.Join(root, ".foreman-kit", "cycle.yaml")
	cycleYAML, err := os.ReadFile(cyclePath)
	if err != nil || strings.Count(string(cycleYAML), "  max_open: 8\n") != 1 {
		t.Fatalf("the seed of .foreman-kit/cycle.yaml does not write queue.max_open: 8 once (err %v)", err)
	}
	if err := os.WriteFile(cyclePath, []byte(strings.Replace(string(cycleYAML), "  max_open: 8\n", "  max_open: 100\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "master"},
		{"config", "user.name", "Portal Stress"},
		{"config", "user.email", "stress@example.com"},
		{"add", "-A"},
		{"commit", "-m", "initial commit"},
	} {
		if _, err := gitx.RunGit(ctx, root, args...); err != nil {
			t.Fatalf("git %v failed: %v", args, err)
		}
	}

	dbPath := filepath.Join(t.TempDir(), "portal_stress.db")
	wsID, canon := store.WorkspaceID(root)
	st, err := store.OpenAt(dbPath, wsID, canon)
	if err != nil {
		t.Fatalf("store.OpenAt failed: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	runStartedCh := make(chan string, 10)
	runCanceledCh := make(chan string, 10)

	srv, kit := newTestServer(t, root, st)
	kit.agent = func(runCtx context.Context, workdir, agent, prompt string) (string, int, error) {
		if strings.Contains(prompt, "BLOCK_UNTIL_STOPPED") {
			// Find running portal_run execution and append streaming chunks while waiting for cancellation
			execs, _ := st.ListExecutions(context.Background(), "portal_run", "", 10)
			var myExecID string
			for _, ex := range execs {
				if ex.Status == "running" && ex.PromptText == prompt {
					myExecID = ex.ExecID
					break
				}
			}
			if myExecID != "" {
				_ = st.AppendLogChunk(context.Background(), myExecID, "combined", "streaming chunk before stop 1\n")
				_ = st.AppendLogChunk(context.Background(), myExecID, "combined", "streaming chunk before stop 2\n")
			}
			select {
			case runStartedCh <- myExecID:
			default:
			}
			<-runCtx.Done()
			select {
			case runCanceledCh <- myExecID:
			default:
			}
			return "", 143, runCtx.Err()
		}
		return fmt.Sprintf("pong from %s: %s", agent, prompt), 0, nil
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// =========================================================================
	// 1. An agent run -> live SSE log streaming -> stop in milliseconds
	// =========================================================================
	code, startData, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions", map[string]any{
		"engine": "claude",
		"prompt": "BLOCK_UNTIL_STOPPED primary run",
	})
	asyncRunID, _ := startedOf(t, code, startData, raw)

	// Wait for the background goroutine to start and write chunks
	select {
	case <-runStartedCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for async run %s to start", asyncRunID)
	}

	// Connect an SSE client to the stream of the output of the run
	sseDoneCh := make(chan string, 1)
	go func() {
		resp, err := http.Get(ts.URL + "/api/executions/" + asyncRunID + "/log/stream")
		if err != nil {
			sseDoneCh <- "ERROR: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		sseDoneCh <- string(b)
	}()

	// Give SSE client 40ms to receive the initial chunks, then call StopExecution
	time.Sleep(40 * time.Millisecond)
	stopStart := time.Now()
	code, stopData, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions/"+asyncRunID+"/stop", map[string]any{})
	stopElapsed := time.Since(stopStart)
	if code != http.StatusOK || stopData["status"] != "stopped" {
		t.Fatalf("POST /api/executions/%s/stop failed (%d): %s", asyncRunID, code, raw)
	}
	if stopElapsed > 250*time.Millisecond {
		t.Fatalf("expected StopExecution to stop goroutine in milliseconds, took %v", stopElapsed)
	}

	// Verify context was canceled immediately
	select {
	case <-runCanceledCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected runCtx to be canceled immediately by StopExecution")
	}

	// Verify SQLite status is "stopped" (143)
	exRec, err := st.GetExecution(ctx, asyncRunID)
	if err != nil || exRec == nil || exRec.Status != "stopped" || exRec.ExitCode != 143 {
		t.Fatalf("expected execution %s status=stopped code=143 in SQLite, got %+v", asyncRunID, exRec)
	}

	// Verify SSE stream received the chunks AND terminated with status="stopped"
	select {
	case sseBody := <-sseDoneCh:
		if !strings.Contains(sseBody, "streaming chunk before stop 1") ||
			!strings.Contains(sseBody, "streaming chunk before stop 2") ||
			!strings.Contains(sseBody, `"status":"stopped"`) {
			t.Fatalf("unexpected SSE stream output on stop:\n%s", sseBody)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("SSE stream did not close after StopExecution")
	}

	// =========================================================================
	// 2. An operation that is a real OS subprocess (Setpgid: true) -> stop kills its process group
	// =========================================================================
	pidFile := filepath.Join(t.TempDir(), "pid")
	kitExe := writeExecutable(t, t.TempDir(), "foreman-kit", `echo $$ > "`+pidFile+`"; exec sleep 60`)
	realSrv, err := New(root, st, NewKit(st, kitExe))
	if err != nil {
		t.Fatal(err)
	}
	job, err := realSrv.exec.StartOperation(ctx, "gates", "")
	if err != nil {
		t.Fatalf("StartOperation failed: %v", err)
	}
	jobID := job.ExecID

	// Poll briefly until the subprocess says which process it is
	var subprocPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rawPID, err := os.ReadFile(pidFile); err == nil {
			if subprocPID, _ = strconv.Atoi(strings.TrimSpace(string(rawPID))); subprocPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if subprocPID == 0 || subprocPID == os.Getpid() {
		t.Fatalf("expected the operation %s to run as a process of its own", jobID)
	}
	t.Cleanup(func() { _ = syscall.Kill(-subprocPID, syscall.SIGKILL) })

	stopJobStart := time.Now()
	code, stopJobData, raw, _ := doReq(t, realSrv, http.MethodPost, "/api/executions/"+jobID+"/stop", map[string]any{})
	stopJobElapsed := time.Since(stopJobStart)
	if code != http.StatusOK || stopJobData["status"] != "stopped" {
		t.Fatalf("POST /api/executions/%s/stop failed (%d): %s", jobID, code, raw)
	}
	if stopJobElapsed > time.Second {
		t.Fatalf("expected the stop of a subprocess in milliseconds, took %v", stopJobElapsed)
	}
	for deadline := time.Now().Add(2 * time.Second); store.IsPIDAlive(subprocPID) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if store.IsPIDAlive(subprocPID) {
		t.Fatalf("expected subprocess PID %d to be dead after the stop", subprocPID)
	}
	jEx, _ := st.GetExecution(ctx, jobID)
	if jEx == nil || jEx.Status != "stopped" || jEx.ExitCode != 143 {
		t.Fatalf("expected job %s status=stopped code=143 in SQLite, got %+v", jobID, jEx)
	}

	// =========================================================================
	// 3. A follow-up and a rerun of an agent run, and stopping a follow-up that is running
	// =========================================================================
	code, parentRun, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions", map[string]any{
		"engine": "claude",
		"prompt": "hello from portal",
	})
	parentID, _ := startedOf(t, code, parentRun, raw)
	if done := waitExecution(t, st, parentID); done.Status != "succeeded" {
		t.Fatalf("the agent run ended as %s", done.Status)
	}
	waitWork(t, srv.exec, parentID)
	if out, _ := st.ReadFullLog(ctx, parentID); !strings.Contains(out, "pong from claude") {
		t.Fatalf("the output of the agent run is not on record: %q", out)
	}

	// A follow-up with the context of the run
	code, contRun, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions/"+parentID+"/continue", map[string]any{
		"prompt":       "and now step two",
		"with_context": true,
	})
	contID, contEx0 := startedOf(t, code, contRun, raw)
	if asStr(contEx0["parent_exec_id"]) != parentID || asStr(contEx0["parent_relation"]) != "continue" ||
		!strings.Contains(asStr(contEx0["prompt_text"]), "hello from portal") ||
		!strings.Contains(asStr(contEx0["prompt_text"]), "and now step two") {
		t.Fatalf("the follow-up failed (%d): %s", code, raw)
	}

	// A rerun
	code, rerunRun, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions/"+parentID+"/rerun", map[string]any{})
	rerunID, rerunEx := startedOf(t, code, rerunRun, raw)
	if asStr(rerunEx["parent_exec_id"]) != parentID || asStr(rerunEx["parent_relation"]) != "rerun" ||
		asStr(rerunEx["prompt_text"]) != "hello from portal" {
		t.Fatalf("the rerun failed (%d): %s", code, raw)
	}
	for _, id := range []string{contID, rerunID} {
		waitExecution(t, st, id)
	}

	// The run lists both as what followed it
	code, parentDet, raw, _ := doReq(t, srv, http.MethodGet, "/api/executions/"+parentID, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/executions/%s failed: %s", parentID, raw)
	}
	children, _ := parentDet["continuations"].([]any)
	var rels []string
	for _, ch := range children {
		if m, ok := ch.(map[string]any); ok {
			rels = append(rels, asStr(m["parent_relation"]))
		}
	}
	sort.Strings(rels)
	if len(rels) != 2 || rels[0] != "continue" || rels[1] != "rerun" {
		t.Fatalf("expected the run to be followed by [continue rerun], got %v", rels)
	}

	// A follow-up stopped mid-flight
	code, asyncCont, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions/"+parentID+"/continue", map[string]any{
		"prompt": "BLOCK_UNTIL_STOPPED follow-up",
	})
	asyncContID, _ := startedOf(t, code, asyncCont, raw)
	select {
	case <-runStartedCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for the follow-up to start")
	}
	code, _, raw, _ = doReq(t, srv, http.MethodPost, "/api/executions/"+asyncContID+"/stop", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("stopping the follow-up failed: %s", raw)
	}
	contEx, _ := st.GetExecution(ctx, asyncContID)
	if contEx == nil || contEx.Status != "stopped" {
		t.Fatalf("expected a stopped follow-up, got %+v", contEx)
	}

	// =========================================================================
	// 4. Pause/Resume/ResumeAndMerge, Unblock + ApproveRisk, ReclaimLease, Worker actions,
	//    and concurrent Backlog & Proposals management
	// =========================================================================
	code, pauseResp, _, _ := doReq(t, srv, http.MethodPost, "/api/workspace/pause", map[string]any{"reason": "maintenance window"})
	if code != http.StatusOK || pauseResp["reason"] != "maintenance window" {
		t.Fatalf("pause failed: %+v", pauseResp)
	}
	// Resuming and running the merger are two actions, each with one effect.
	if code, resumed, raw, _ := doReq(t, srv, http.MethodPost, "/api/workspace/resume", map[string]any{}); code != http.StatusOK || resumed["paused"] != false {
		t.Fatalf("resume failed (%d): %s", code, raw)
	}
	if ws, _ := st.GetWorkspaceState(ctx); ws.Paused {
		t.Fatalf("resuming left the workspace paused")
	}
	code, mergeResp, raw, _ := doReq(t, srv, http.MethodPost, "/api/merger/runs", map[string]any{})
	mergeID, mergeEx := startedOf(t, code, mergeResp, raw)
	if asStr(mergeEx["job_kind"]) != "merge" {
		t.Fatalf("POST /api/merger/runs started %+v, want the merger", mergeEx)
	}
	waitExecution(t, st, mergeID)

	// Quarantine + .needs-human -> Unblock with approve_risk & reset_retries
	wtDemo := filepath.Join(root, ".worktrees", "worker-demo-change", "openspec", "changes", "demo-change")
	_ = os.MkdirAll(wtDemo, 0o755)
	_ = os.WriteFile(filepath.Join(wtDemo, "proposal.md"), []byte("# Demo Proposal\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wtDemo, "tasks.md"), []byte("- [x] 1.1 Done\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wtDemo, ".needs-human"), []byte("Which schema?"), 0o644)
	_ = st.BlockChange(ctx, store.QuarantineRecord{
		ChangeName: "demo-change",
		Reason:     "risk_review",
		Details:    "high risk migration",
		Retries:    1,
	})

	// Unblock it and start its retries over, then accept its risk: two actions, in the order that
	// keeps the acceptance (starting the counters over withdraws an accepted risk).
	code, unblockResp, raw, _ := doReq(t, srv, http.MethodPost, "/api/changes/demo-change/unblock", map[string]any{
		"reset_retries": true,
	})
	if code != http.StatusOK || unblockResp["state"] == "blocked" || unblockResp["block"] != nil {
		t.Fatalf("unblock failed (%d): %s", code, raw)
	}
	code, _, raw, _ = doReq(t, srv, http.MethodPost, "/api/changes/demo-change/risk-approval", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("risk-approval failed (%d): %s", code, raw)
	}
	// There is nothing left to unblock: the action says so and changes nothing.
	code, again, _, _ := doReq(t, srv, http.MethodPost, "/api/changes/demo-change/unblock", map[string]any{})
	if c, _ := errorOf(again); code != http.StatusConflict || c != "conflict" {
		t.Fatalf("unblocking a change that is not blocked = %d %v, want 409", code, again)
	}
	qDemo, _ := st.GetQuarantine(ctx, "demo-change")
	if qDemo == nil || (qDemo.Status != "cleared" && qDemo.Status != "approved") || !qDemo.ApprovedRisk || qDemo.Retries != 0 {
		t.Fatalf("expected demo-change quarantine cleared/approved, risk approved, retries=0; got %+v", qDemo)
	}
	if isFile(filepath.Join(wtDemo, ".needs-human")) {
		t.Fatalf("expected .needs-human to be removed by unblock")
	}

	// Live lease cannot be reclaimed; dead lease CAN be reclaimed
	_, _ = st.AcquireLock(ctx, store.WorkerLeaseKey("demo-change"), store.LockOptions{OwnerType: "worker", PID: os.Getpid(), TTL: 10 * time.Minute})
	code, _, _, _ = doReq(t, srv, http.MethodPost, "/api/changes/demo-change/reclaim", map[string]any{})
	if code != http.StatusConflict {
		t.Fatalf("expected 409 when reclaiming live lease, got %d", code)
	}
	_ = st.ReleaseLock(ctx, store.WorkerLeaseKey("demo-change"), 0)
	_, _ = st.AcquireLock(ctx, store.WorkerLeaseKey("demo-change"), store.LockOptions{OwnerType: "worker", PID: 999997, TTL: 10 * time.Minute})
	code, _, raw, _ = doReq(t, srv, http.MethodPost, "/api/changes/demo-change/reclaim", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("expected 200 when reclaiming dead lease, got %d: %s", code, raw)
	}
	if lease, _ := st.GetLock(ctx, store.WorkerLeaseKey("demo-change")); lease != nil {
		t.Fatalf("the lease of a dead worker was not given back: %+v", lease)
	}
	// With no lease left there is nothing to give back.
	if code, _, _, _ := doReq(t, srv, http.MethodPost, "/api/changes/demo-change/reclaim", map[string]any{}); code != http.StatusConflict {
		t.Fatalf("expected 409 when there is no lease to reclaim, got %d", code)
	}

	// Worker validate, agent, gates, reset
	code, valResp, _, _ := doReq(t, srv, http.MethodPost, "/api/changes/demo-change/validate", map[string]any{})
	if code != http.StatusOK || valResp["ok"] != true {
		t.Fatalf("worker validate failed: %+v", valResp)
	}
	code, wtAgentResp, raw, _ := doReq(t, srv, http.MethodPost, "/api/changes/demo-change/prompts", map[string]any{
		"engine": "claude",
		"prompt": "inspect worktree",
	})
	wtAgentID, wtAgentEx := startedOf(t, code, wtAgentResp, raw)
	if !strings.HasSuffix(asStr(wtAgentEx["workdir"]), "worker-demo-change") || asStr(wtAgentEx["change_name"]) != "demo-change" {
		t.Fatalf("worker agent failed: %+v", wtAgentResp)
	}
	waitExecution(t, st, wtAgentID)
	waitWork(t, srv.exec, wtAgentID)
	code, wtGatesResp, raw, _ := doReq(t, srv, http.MethodPost, "/api/changes/demo-change/gates", map[string]any{})
	wtGatesID, wtGatesEx := startedOf(t, code, wtGatesResp, raw)
	if asStr(wtGatesEx["job_kind"]) != "gates-demo-change" || !strings.HasSuffix(asStr(wtGatesEx["workdir"]), "worker-demo-change") {
		t.Fatalf("the gates of the worktree started as %+v", wtGatesEx)
	}
	waitExecution(t, st, wtGatesID)
	waitWork(t, srv.exec, wtGatesID)
	code, wtResetResp, raw, _ := doReq(t, srv, http.MethodPost, "/api/changes/demo-change/reset", map[string]any{})
	if code != http.StatusOK || wtResetResp["change_name"] != "demo-change" || isDir(filepath.Join(root, ".worktrees", "worker-demo-change")) {
		t.Fatalf("worker reset failed (%d): %s", code, raw)
	}
	// With its worktree gone, nothing runs in it.
	for action, payload := range map[string]map[string]any{
		"validate": {},
		"gates":    {},
		"prompts":  {"engine": "claude", "prompt": "inspect what is gone"},
	} {
		if code, body, _, _ := doReq(t, srv, http.MethodPost, "/api/changes/demo-change/"+action, payload); code != http.StatusConflict {
			t.Fatalf("POST %s for a change with no worktree = %d %v, want 409", action, code, body)
		}
	}

	// Concurrent Backlog & Proposals management
	const numItems = 20
	var bWG sync.WaitGroup
	var addFailures int32
	for i := 0; i < numItems; i++ {
		bWG.Add(1)
		go func(idx int) {
			defer bWG.Done()
			name := fmt.Sprintf("conc-item-%02d", idx)
			c, _, _, _ := doReq(t, srv, http.MethodPost, "/api/backlog", map[string]any{
				"change_name": name,
				"priority":    "high",
				"description": fmt.Sprintf("Concurrent item %d", idx),
			})
			if c != http.StatusCreated {
				atomic.AddInt32(&addFailures, 1)
			}
		}(i)
	}
	bWG.Wait()
	if addFailures != 0 {
		t.Fatalf("expected 0 concurrent backlog add failures, got %d", addFailures)
	}
	if gotLen := len(backlogItems(t, root)); gotLen != numItems {
		t.Fatalf("expected %d items in backlog after concurrent adds, got %d", numItems, gotLen)
	}

	// Concurrent Delete of half the backlog items
	for i := 0; i < numItems/2; i++ {
		bWG.Add(1)
		go func(idx int) {
			defer bWG.Done()
			name := fmt.Sprintf("conc-item-%02d", idx)
			c, _, _, _ := doReq(t, srv, http.MethodDelete, "/api/backlog/"+name, nil)
			if c != http.StatusOK {
				atomic.AddInt32(&addFailures, 1)
			}
		}(i)
	}
	bWG.Wait()
	if addFailures != 0 || len(backlogItems(t, root)) != numItems/2 {
		t.Fatalf("unexpected state after concurrent backlog deletes: failures=%d remaining=%d", addFailures, len(backlogItems(t, root)))
	}

	// Proposals approve, reject, and review
	_ = cycle.WriteProposals(root, []map[string]any{
		{"change_name": "prop-one", "status": "proposed", "source": "exploration", "priority": "high", "description": "One"},
		{"change_name": "prop-two", "status": "proposed", "source": "exploration", "priority": "low", "description": "Two"},
	})
	code, _, raw, _ = doReq(t, srv, http.MethodPost, "/api/proposals/prop-one/approve", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("approve prop-one failed: %s", raw)
	}
	code, _, raw, _ = doReq(t, srv, http.MethodPost, "/api/proposals/prop-two/reject", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("reject prop-two failed: %s", raw)
	}
	code, revResp, raw, _ := doReq(t, srv, http.MethodPost, "/api/proposals/review", map[string]any{})
	if _, said := revResp["output"].(string); code != http.StatusOK || !said {
		t.Fatalf("POST /api/proposals/review failed: %s", raw)
	}
	hasPropOneInBacklog := false
	for _, it := range backlogItems(t, root) {
		if asStr(it["change_name"]) == "prop-one" {
			hasPropOneInBacklog = true
			break
		}
	}
	if !hasPropOneInBacklog {
		t.Fatalf("expected approved prop-one to be queued into backlog after /api/proposals/review")
	}
}
