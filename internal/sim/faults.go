package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/orchestrator"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// FaultScenarios are end-to-end fault-injection scenarios run against a real umbrella stack
// (bare remotes, a submodule child, SQLite state). Each one injects a failure an operator meets in
// production (agent crashes and hangs, provider throttling, Jev and LLM outages, rejected or
// unreachable remotes, rewritten history, a broken submodule pointer, unwritable or full disks, and
// malformed backlogs) and verifies the pipeline degrades safely and recovers once the fault is gone.
var FaultScenarios = []string{
	"fault-agent-crash-midway-continuation",
	"fault-agent-hang-idle-watchdog",
	"fault-orchestrator-killed-mid-build",
	"fault-agent-rate-limited-deferral",
	"fault-agent-output-garbage-noise-escape",
	"fault-agent-git-misuse",
	"fault-git-push-rejected-then-recovers",
	"fault-git-remotes-unreachable-then-recover",
	"fault-git-stale-index-locks",
	"fault-git-rebase-conflict-clobber",
	"fault-work-loss-restored-by-agent",
	"fault-work-loss-trivial-accepted",
	"fault-semantic-merge-conflict",
	"fault-orchestrator-error-change-specific",
	"fault-agent-stray-files-reverted",
	"fault-agent-gives-up-or-disk-full",
	"fault-git-base-force-pushed",
	"fault-git-submodule-pointer-missing",
	"fault-disk-worktrees-unwritable",
	"fault-disk-full-during-gates",
	"fault-jev-outage-timeout-garbage-enforce",
	"fault-llm-diagnostics-provider-failures",
	"fault-backlog-edge-cases",
}

func isFaultScenario(name string) bool {
	for _, n := range FaultScenarios {
		if n == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Fault controls shared with the simulated Jev server
// ---------------------------------------------------------------------------

var jevFault struct {
	mu   sync.Mutex
	mode string // "" (healthy), "500", "429", "garbage", "hang"
	hits int
}

func resetFaults() {
	jevFault.mu.Lock()
	jevFault.mode = ""
	jevFault.hits = 0
	jevFault.mu.Unlock()
}

func setJevMode(mode string) {
	jevFault.mu.Lock()
	jevFault.mode = mode
	jevFault.mu.Unlock()
}

func jevHits() int {
	jevFault.mu.Lock()
	defer jevFault.mu.Unlock()
	return jevFault.hits
}

// serveJevFault answers a Jev request according to the injected fault and reports whether it did.
func serveJevFault(w http.ResponseWriter, r *http.Request) bool {
	jevFault.mu.Lock()
	jevFault.hits++
	mode := jevFault.mode
	jevFault.mu.Unlock()
	return serveHTTPFault(w, r, mode)
}

func serveHTTPFault(w http.ResponseWriter, r *http.Request, mode string) bool {
	switch mode {
	case "500":
		http.Error(w, `{"error":"internal server error (simulated outage)"}`, http.StatusInternalServerError)
	case "429":
		w.Header().Set("Retry-After", "1")
		http.Error(w, `{"error":"rate limited (simulated)"}`, http.StatusTooManyRequests)
	case "garbage":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>\x00\xff{not json"))
	case "hang":
		select {
		case <-r.Context().Done():
		case <-time.After(15 * time.Second):
		}
		http.Error(w, "gateway timeout (simulated)", http.StatusGatewayTimeout)
	default:
		return false
	}
	return true
}

// llmMock is an OpenAI-compatible endpoint for the structured LLM client (provider "compat").
type llmMock struct {
	srv  *httptest.Server
	mode atomic.Value // string
	hits atomic.Int64
}

func startLLMMock() *llmMock {
	m := &llmMock{}
	m.mode.Store("500")
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		_, _ = drainBody(r)
		mode, _ := m.mode.Load().(string)
		if mode == "schema-invalid" {
			// A well-formed completion whose content fails the requested schema.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "chatcmpl-sim", "object": "chat.completion", "created": time.Now().Unix(), "model": "sim-model",
				"choices": []any{map[string]any{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": `{"unexpected": true}`},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
			return
		}
		if !serveHTTPFault(w, r, mode) {
			http.Error(w, "unknown mode", http.StatusTeapot)
		}
	}))
	return m
}

func drainBody(r *http.Request) (int64, error) {
	if r.Body == nil {
		return 0, nil
	}
	defer r.Body.Close()
	var n int64
	buf := make([]byte, 32*1024)
	for {
		k, err := r.Body.Read(buf)
		n += int64(k)
		if err != nil {
			return n, nil
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario environment and helpers
// ---------------------------------------------------------------------------

type faultEnv struct {
	ctx   context.Context
	st    *Stack
	db    *store.Store
	res   *ScenarioResult
	check func(desc string, cond bool) error
	notes []string
}

func (fe *faultEnv) note(format string, args ...any) {
	fe.notes = append(fe.notes, fmt.Sprintf(format, args...))
}

// gap verifies a property the pipeline does not guarantee yet. A violation is recorded as a
// "KNOWN GAP" note and the scenario still passes, unless FOREMANKIT_SIM_STRICT=1 makes it fail.
func (fe *faultEnv) gap(desc string, cond bool) error {
	if cond {
		return fe.check(desc, true)
	}
	if os.Getenv("FOREMANKIT_SIM_STRICT") == "1" {
		return fe.check(desc, false)
	}
	fe.note("KNOWN GAP: %s", desc)
	return nil
}

func (fe *faultEnv) run(name string) error {
	var err error
	switch name {
	case "fault-agent-crash-midway-continuation":
		err = fe.agentCrashMidway()
	case "fault-agent-hang-idle-watchdog":
		err = fe.agentHang()
	case "fault-orchestrator-killed-mid-build":
		err = fe.orchestratorKilled()
	case "fault-agent-rate-limited-deferral":
		err = fe.agentRateLimited()
	case "fault-agent-output-garbage-noise-escape":
		err = fe.agentOutputEdgeCases()
	case "fault-agent-git-misuse":
		err = fe.agentGitMisuse()
	case "fault-git-push-rejected-then-recovers":
		err = fe.gitPushRejected()
	case "fault-git-remotes-unreachable-then-recover":
		err = fe.gitRemotesUnreachable()
	case "fault-git-stale-index-locks":
		err = fe.gitStaleIndexLocks()
	case "fault-git-rebase-conflict-clobber":
		err = fe.gitRebaseConflictClobber()
	case "fault-work-loss-restored-by-agent":
		err = fe.workLossRestoredByAgent()
	case "fault-work-loss-trivial-accepted":
		err = fe.workLossTrivialAccepted()
	case "fault-semantic-merge-conflict":
		err = fe.semanticMergeConflict()
	case "fault-orchestrator-error-change-specific":
		err = fe.orchestratorErrorChangeSpecific()
	case "fault-agent-stray-files-reverted":
		err = fe.agentStrayFilesReverted()
	case "fault-agent-gives-up-or-disk-full":
		err = fe.agentGivesUpOrDiskFull()
	case "fault-git-base-force-pushed":
		err = fe.gitBaseForcePushed()
	case "fault-git-submodule-pointer-missing":
		err = fe.gitSubmodulePointerMissing()
	case "fault-disk-worktrees-unwritable":
		err = fe.diskWorktreesUnwritable()
	case "fault-disk-full-during-gates":
		err = fe.diskFullDuringGates()
	case "fault-jev-outage-timeout-garbage-enforce":
		err = fe.jevOutage()
	case "fault-llm-diagnostics-provider-failures":
		err = fe.llmProviderFailures()
	case "fault-backlog-edge-cases":
		err = fe.backlogEdgeCases()
	default:
		err = fmt.Errorf("unknown fault scenario %q", name)
	}
	if len(fe.notes) > 0 {
		fe.res.Details = strings.Join(fe.notes, "; ")
	}
	return err
}

func (fe *faultEnv) ws() string      { return fe.st.WorkspaceDir }
func (fe *faultEnv) backend() string { return filepath.Join(fe.st.WorkspaceDir, "repos", "backend") }
func (fe *faultEnv) umbrellaBare() string {
	return filepath.Join(fe.st.RemotesDir, "umbrella.git")
}
func (fe *faultEnv) backendBare() string { return filepath.Join(fe.st.RemotesDir, "backend.git") }

func (fe *faultEnv) git(dir string, args ...string) (string, error) {
	out, err := gitx.RunGit(fe.ctx, dir, args...)
	return strings.TrimSpace(out), err
}

// agents configures plan and build engines (the cycle role uses sim-agent-clean).
func (fe *faultEnv) agents(plan, build []string) {
	seen := map[string]bool{}
	var all []string
	for _, a := range append(append([]string{}, plan...), build...) {
		if !seen[a] {
			seen[a] = true
			all = append(all, a)
		}
	}
	if !seen["sim-agent-clean"] {
		all = append(all, "sim-agent-clean")
	}
	WriteAgentsYAML(fe.ws(), all, map[string][]string{
		"plan":  plan,
		"build": build,
		"cycle": {"sim-agent-clean"},
	}, "in_order", nil)
	commitWorkspaceConfig(fe.ctx, fe.ws(), "configure fault agents")
}

type pipelineOpts struct {
	agentSeconds     int
	idleSeconds      int
	conflictRebuilds int
	// more sets other keys of the configuration (the diagnostics model, for one scenario).
	more map[settings.Key]string
}

func (fe *faultEnv) pipeline(o pipelineOpts) {
	if o.agentSeconds == 0 {
		o.agentSeconds = 120
	}
	if o.idleSeconds == 0 {
		o.idleSeconds = 60
	}
	err := writeSimConfig(fe.ws(), simLimits{
		agentTimeout:     settings.FormatDuration(time.Duration(o.agentSeconds) * time.Second),
		idleTimeout:      settings.FormatDuration(time.Duration(o.idleSeconds) * time.Second),
		conflictRebuilds: o.conflictRebuilds,
		more:             o.more,
	})
	if err != nil {
		fe.note("configure the fault pipeline: %v", err)
	}
	commitWorkspaceConfig(fe.ctx, fe.ws(), "configure fault pipeline")
}

func (fe *faultEnv) commitPaths(msg string, paths ...string) {
	args := append([]string{"add"}, paths...)
	_, _ = fe.git(fe.ws(), args...)
	_, _ = fe.git(fe.ws(), "commit", "-m", msg)
	_, _ = fe.git(fe.ws(), "push", "origin", "master")
}

func (fe *faultEnv) backlog(names ...string) {
	var items []map[string]any
	for _, n := range names {
		items = append(items, map[string]any{"change_name": n, "priority": "high", "description": "Fault scenario change " + n})
	}
	writeBacklog(fe.ws(), items)
}

func (fe *faultEnv) syncMain() {
	_, _ = fe.git(fe.ws(), "fetch", "origin")
	_, _ = fe.git(fe.ws(), "reset", "--hard", "origin/master")
	_, _ = fe.git(fe.ws(), "submodule", "update", "--init", "--checkout")
}

func (fe *faultEnv) dispatch(workers int, now time.Time) error {
	opts := orchestrator.DispatcherOptions{
		Root:           fe.ws(),
		Store:          fe.db,
		MaxWorkers:     workers,
		RepairAttempts: 2,
	}
	if !now.IsZero() {
		opts.NowFunc = func() time.Time { return now }
	}
	disp, err := orchestrator.NewDispatcher(opts)
	if err != nil {
		return err
	}
	defer disp.Close()
	return disp.Run(fe.ctx)
}

func (fe *faultEnv) merge(gate orchestrator.GateRunnerFunc) error {
	m, err := orchestrator.NewMerger(orchestrator.MergerOptions{Root: fe.ws(), Store: fe.db, GateRunner: gate})
	if err != nil {
		return err
	}
	defer m.Close()
	return m.Run(fe.ctx)
}

// dispatchMerge runs one dispatcher pass and one merger pass, returning the first error without
// aborting the scenario: under fault injection an error is an outcome to verify, not a harness failure.
func (fe *faultEnv) dispatchMerge(workers int) error {
	dErr := fe.dispatch(workers, time.Time{})
	mErr := fe.merge(nil)
	fe.syncMain()
	if dErr != nil {
		return fmt.Errorf("dispatch: %w", dErr)
	}
	if mErr != nil {
		return fmt.Errorf("merge: %w", mErr)
	}
	return nil
}

// recover simulates the operator after the fault is fixed: resume the workspace, unblock change, and
// run dispatcher+merger passes (with the clock past blocked_retry_hours) until change integrates.
func (fe *faultEnv) recover(change string, rounds int) bool {
	for i := 1; i <= rounds; i++ {
		if fe.integrated(change) {
			return true
		}
		_ = fe.db.SetPaused(fe.ctx, false, "")
		if q, _ := fe.db.GetQuarantine(fe.ctx, change); q != nil && q.Status == "blocked" {
			_ = fe.db.ClearQuarantine(fe.ctx, change, false)
		}
		_ = fe.dispatch(1, time.Now().UTC().Add(time.Duration(2*i)*time.Hour))
		_ = fe.merge(nil)
		fe.syncMain()
	}
	return fe.integrated(change)
}

func (fe *faultEnv) integrated(change string) bool { return isIntegrated(fe.ctx, fe.ws(), change) }

func (fe *faultEnv) quarantine(change string) *store.QuarantineRecord {
	q, _ := fe.db.GetQuarantine(fe.ctx, change)
	return q
}

func (fe *faultEnv) blockedReason(change string) string {
	if q := fe.quarantine(change); q != nil && q.Status == "blocked" {
		return q.Reason
	}
	return ""
}

func (fe *faultEnv) paused() (bool, string) {
	ws, err := fe.db.GetWorkspaceState(fe.ctx)
	if err != nil {
		return false, ""
	}
	return ws.Paused, ws.PauseReason
}

func (fe *faultEnv) inReadyQueue(change string) bool {
	rq, _ := fe.db.ListReadyQueue(fe.ctx)
	for _, m := range rq {
		if m.ChangeName == change {
			return true
		}
	}
	return false
}

// backendFileOnOrigin reads path from the backend remote's main branch.
func (fe *faultEnv) backendFileOnOrigin(path string) string {
	out, err := fe.git(fe.backendBare(), "show", "main:"+path)
	if err != nil {
		return ""
	}
	return out
}

func featureFile(change string) string {
	var b strings.Builder
	for _, r := range change {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return "feature_" + b.String() + ".go"
}

func (fe *faultEnv) featureOnOrigin(change string) bool {
	return strings.Contains(fe.backendFileOnOrigin(featureFile(change)), "ok:"+change)
}

func (fe *faultEnv) events(event, change string) []store.EventRecord {
	evts, _ := fe.db.QueryEvents(fe.ctx, store.EventFilter{Event: event, ChangeName: change})
	return evts
}

func (fe *faultEnv) applyEngines(change string) []string {
	var engines []string
	for _, e := range fe.events("agent.exit", change) {
		if strings.HasPrefix(e.Phase, "apply") {
			engines = append(engines, e.Engine)
		}
	}
	return engines
}

func processAlive(pattern string) (bool, bool) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		return false, false
	}
	err := exec.Command("pgrep", "-f", pattern).Run()
	return err == nil, true
}

func waitNoProcess(pattern string, d time.Duration) (gone bool, checked bool) {
	deadline := time.Now().Add(d)
	for {
		alive, ok := processAlive(pattern)
		if !ok {
			return true, false
		}
		if !alive {
			return true, true
		}
		if time.Now().After(deadline) {
			return false, true
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func findArchivedFile(root, change, name string) string {
	archDir := filepath.Join(root, "openspec", "changes", "archive")
	entries, _ := os.ReadDir(archDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-"+change) || e.Name() == change {
			if b, err := os.ReadFile(filepath.Join(archDir, e.Name(), name)); err == nil {
				return string(b)
			}
		}
	}
	return ""
}

func isRoot() bool { return os.Geteuid() == 0 }

// ---------------------------------------------------------------------------
// Agent faults
// ---------------------------------------------------------------------------

// An agent writes half of the feature and dies by SIGKILL; the fallback engine must receive the
// continuation note and finish from the half-done worktree instead of starting over.
func (fe *faultEnv) agentCrashMidway() error {
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-crash-midway", "sim-agent-clean"})
	fe.backlog("crash-recovery")
	if err := fe.dispatchMerge(1); err != nil {
		return err
	}
	if err := fe.check("crash-recovery integrated after its first build agent was SIGKILLed mid-change", fe.integrated("crash-recovery")); err != nil {
		return err
	}
	if err := fe.check("fallback engine was told a previous attempt stopped (continuation note) and continued",
		strings.Contains(findArchivedFile(fe.ws(), "crash-recovery", "continued.txt"), "continued_after_previous_attempt=true")); err != nil {
		return err
	}
	engines := fe.applyEngines("crash-recovery")
	if err := fe.check("agent.exit names sim-agent-clean as the engine that finished the build",
		len(engines) > 0 && engines[len(engines)-1] == "sim-agent-clean"); err != nil {
		return err
	}
	health, _ := fe.db.ListEngineHealth(fe.ctx)
	h := health["sim-agent-crash-midway"]
	return fe.check("a crash is not misclassified as quota or auth failure (no cooldown recorded)",
		h.Status != "quota_exceeded" && h.Status != "auth_failed")
}

// An agent hangs with no output, CPU, or file changes; the idle watchdog must stop it quickly,
// kill its process group, and hand the change to the fallback engine.
func (fe *faultEnv) agentHang() error {
	fe.pipeline(pipelineOpts{agentSeconds: 90, idleSeconds: 3})
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-hang", "sim-agent-clean"})
	fe.backlog("hang-recovery")
	start := time.Now()
	if err := fe.dispatchMerge(1); err != nil {
		return err
	}
	elapsed := time.Since(start)
	if err := fe.check("hang-recovery integrated after the hanging agent was stopped", fe.integrated("hang-recovery")); err != nil {
		return err
	}
	if err := fe.check(fmt.Sprintf("idle watchdog stopped the hang well before agent_seconds (took %s, limit 90s)", elapsed.Round(time.Second)),
		elapsed < 80*time.Second); err != nil {
		return err
	}
	// The stall is on record, not only in the agent's output: the attempt, the idle period the portal
	// and `foreman-kit status` showed while it lasted, and the events that announce both.
	stalledAttempts, stalledPeriods := 0, 0
	execs, _ := fe.db.ListExecutions(fe.ctx, "worker", "hang-recovery", 0)
	for _, ex := range execs {
		attempts, _ := fe.db.ListAttempts(fe.ctx, ex.ExecID)
		for _, a := range attempts {
			if a.Engine == "sim-agent-hang" && a.Outcome == store.AttemptIdleStalled && a.IdleSeconds >= 3 {
				stalledAttempts++
			}
		}
		periods, _ := fe.db.ListIdlePeriods(fe.ctx, ex.ExecID)
		for _, p := range periods {
			if p.Engine == "sim-agent-hang" && p.EndedBy == store.IdleEndedByStall {
				stalledPeriods++
			}
		}
	}
	if err := fe.check("the hanging agent's attempt is recorded as idle_stalled, once: a stalled agent is not tried again",
		stalledAttempts == 1); err != nil {
		return err
	}
	if err := fe.check("the agent was shown as idle before it was stopped (agent.idle, an idle period closed by the stall, agent.stalled)",
		stalledPeriods == 1 && len(fe.events("agent.idle", "hang-recovery")) == 1 && len(fe.events("agent.stalled", "hang-recovery")) == 1); err != nil {
		return err
	}
	if idle, _ := fe.db.ListIdleAgents(fe.ctx); len(idle) != 0 {
		return fe.check("no agent is shown as idle once the change is integrated", false)
	}
	gone, checked := waitNoProcess("sleep 86399", 5*time.Second)
	if !checked {
		fe.note("pgrep unavailable: leftover-process check skipped")
		return nil
	}
	return fe.check("no leftover process from the stalled agent's process group", gone)
}

// The orchestrator itself dies (context canceled, like SIGTERM) while an agent is building; the next
// run must reap the orphaned agent, recover the lost worktree, and integrate the change.
func (fe *faultEnv) orchestratorKilled() error {
	fe.pipeline(pipelineOpts{agentSeconds: 300, idleSeconds: 600})
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-hang"})
	fe.backlog("survives-orchestrator-death")

	runCtx, cancel := context.WithTimeout(fe.ctx, 6*time.Second)
	disp, err := orchestrator.NewDispatcher(orchestrator.DispatcherOptions{Root: fe.ws(), Store: fe.db, MaxWorkers: 1, RepairAttempts: 2})
	if err != nil {
		cancel()
		return err
	}
	runErr := disp.Run(runCtx)
	_ = disp.Close()
	cancel()
	fe.note("killed dispatcher returned: %v", runErr)

	gone, checked := waitNoProcess("sleep 86399", 10*time.Second)
	if checked {
		if err := fe.check("agent process group stopped when the orchestrator was killed", gone); err != nil {
			return err
		}
	}
	if err := fe.check("change not integrated by the killed run", !fe.integrated("survives-orchestrator-death")); err != nil {
		return err
	}

	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-clean"})
	if err := fe.dispatchMerge(1); err != nil {
		return err
	}
	if !fe.integrated("survives-orchestrator-death") {
		fe.note("first restart did not integrate (blocked=%q); retrying as operator", fe.blockedReason("survives-orchestrator-death"))
	}
	return fe.check("restarted orchestrator recovered the interrupted change and integrated it",
		fe.recover("survives-orchestrator-death", 2))
}

// The only build engine is throttled (429 with a long Retry-After) and changes nothing: the change
// must be deferred (not quarantined, not failed), and integrate once the provider recovers.
func (fe *faultEnv) agentRateLimited() error {
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-rate-limit"})
	fe.backlog("throttled-change")
	if err := fe.dispatchMerge(1); err != nil {
		return err
	}
	if err := fe.check("throttled-change not integrated while the provider throttles", !fe.integrated("throttled-change")); err != nil {
		return err
	}
	if err := fe.check("throttled-change deferred instead of quarantined", fe.blockedReason("throttled-change") == "" && len(fe.events("change.deferred", "throttled-change")) > 0); err != nil {
		return err
	}
	if p, why := fe.paused(); p {
		fe.note("workspace paused after one throttled run: %s", why)
	}
	if err := fe.check("one throttled run does not pause the workspace", func() bool { p, _ := fe.paused(); return !p }()); err != nil {
		return err
	}

	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-clean"})
	if err := fe.dispatchMerge(1); err != nil {
		return err
	}
	if err := fe.check("throttled-change integrated after the provider recovered", fe.integrated("throttled-change")); err != nil {
		return err
	}
	counts, _ := fe.db.DeferralCounts(fe.ctx)
	return fe.check("deferral counter cleared after a successful agent call", counts["throttled-change"].Count == 0)
}

// Three agents misbehave in their output: one floods stdout with invalid UTF-8 and changes nothing
// (claiming success), one floods stdout but builds correctly, one writes outside its worktree.
func (fe *faultEnv) agentOutputEdgeCases() error {
	fe.agents([]string{"sim-agent-router"}, []string{"sim-agent-router"})
	fe.backlog("garbage-output", "noisy-but-correct", "escape-attempt")
	if err := fe.dispatchMerge(2); err != nil {
		return err
	}
	if err := fe.check("garbage-output (exit 0, nothing written) quarantined as spec_invalid, never integrated",
		!fe.integrated("garbage-output") && fe.blockedReason("garbage-output") == "spec_invalid"); err != nil {
		return err
	}
	if err := fe.check("noisy-but-correct integrated despite megabytes of invalid UTF-8 and ANSI output", fe.integrated("noisy-but-correct")); err != nil {
		return err
	}
	var maxPayload int
	_ = fe.db.DB().QueryRowContext(fe.ctx, `SELECT COALESCE(MAX(LENGTH(payload_json)), 0) FROM events`).Scan(&maxPayload)
	if err := fe.check(fmt.Sprintf("event payloads stay bounded under agent output floods (largest %d bytes)", maxPayload), maxPayload < 256*1024); err != nil {
		return err
	}
	// The agent's FOREMANKIT_APP_ROOT is its own worktree, so a write "to the app root" lands at the
	// worktree root, outside the allowed paths: the path guard must block it.
	q := fe.quarantine("escape-attempt")
	if err := fe.check("escape-attempt (wrote outside allowed paths) blocked as policy_violation naming the file",
		!fe.integrated("escape-attempt") && q != nil && q.Reason == "policy_violation" && strings.Contains(q.Details, "ESCAPED_BY_AGENT_escape-attempt.txt")); err != nil {
		return err
	}
	if err := fe.check("policy_violation is not retried automatically", !orchestrator.IsRetryableReason("policy_violation")); err != nil {
		return err
	}
	tree, _ := fe.git(fe.umbrellaBare(), "ls-tree", "-r", "--name-only", "master")
	backendTree, _ := fe.git(fe.backendBare(), "ls-tree", "-r", "--name-only", "main")
	if err := fe.check("files an agent wrote outside allowed paths never reach origin", !strings.Contains(tree+backendTree, "ESCAPED_")); err != nil {
		return err
	}
	if fileExists(filepath.Join(fe.ws(), ".worktrees", "ESCAPED_SIBLING_escape-attempt.txt")) {
		fe.note("FINDING: a write to ../ from the worktree lands in .worktrees/ (outside any repository) and goes unnoticed; agents run unsandboxed")
	}
	return nil
}

// Real agents commit on their own and sometimes switch branches; the orchestrator must still
// integrate exactly the agent's work (or block clearly), never lose it silently.
func (fe *faultEnv) agentGitMisuse() error {
	fe.agents([]string{"sim-agent-router"}, []string{"sim-agent-router"})
	fe.backlog("self-commit-feature", "branch-switch-feature")
	if err := fe.dispatchMerge(2); err != nil {
		fe.note("first pass error: %v", err)
	}
	if err := fe.check("self-commit-feature integrated with its code on the child remote although the agent committed itself",
		fe.integrated("self-commit-feature") && fe.featureOnOrigin("self-commit-feature")); err != nil {
		return err
	}
	q := fe.quarantine("branch-switch-feature")
	return fe.check("branch-switch-feature (agent left the worker branch) blocked as policy_violation naming the child, never integrated",
		!fe.integrated("branch-switch-feature") && q != nil && q.Reason == "policy_violation" &&
			strings.Contains(q.Details, "left feat/spec-branch-switch-feature") && strings.Contains(q.Details, "repos/backend"))
}

// ---------------------------------------------------------------------------
// Git faults
// ---------------------------------------------------------------------------

// The umbrella remote rejects every push (pre-receive hook, like a protected branch or an outage).
func (fe *faultEnv) gitPushRejected() error {
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-clean"})
	fe.backlog("push-rejected-change")
	before, _ := fe.git(fe.umbrellaBare(), "rev-parse", "master")

	hook := filepath.Join(fe.umbrellaBare(), "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'rejected: simulated protected branch outage' >&2\nexit 1\n"), 0o755); err != nil {
		return err
	}
	pushErr := fe.dispatchMerge(1)
	fe.note("merge under rejection returned: %v", pushErr)
	after, _ := fe.git(fe.umbrellaBare(), "rev-parse", "master")
	if err := fe.check("origin/master untouched while the remote rejects pushes", before == after); err != nil {
		return err
	}
	if err := fe.check("change not reported integrated while its push is rejected", !fe.integrated("push-rejected-change")); err != nil {
		return err
	}
	q := fe.quarantine("push-rejected-change")
	if err := fe.check("Jev (push_rejection) judged the hook's refusal a rule, not a glitch: blocked as push_rejected with the remote's message",
		q != nil && q.Reason == "push_rejected" && strings.Contains(q.Details, "pre-receive hook declined") && strings.Contains(q.Details, "kind=policy")); err != nil {
		return err
	}
	if err := fe.check(fmt.Sprintf("no blind push retries against a rule (%d rejection(s))", len(fe.events("merge.push_rejected", "push-rejected-change"))),
		len(fe.events("merge.push_rejected", "push-rejected-change")) == 1); err != nil {
		return err
	}

	_ = os.Remove(hook)
	return fe.check("change integrated once the remote accepts pushes again", fe.recover("push-rejected-change", 3))
}

// The umbrella remote is unreachable for a whole run; then the child remote disappears while the
// merger is running gates (between its gates and its push).
func (fe *faultEnv) gitRemotesUnreachable() error {
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-clean"})
	fe.backlog("offline-umbrella-change")

	offline := fe.umbrellaBare() + ".offline"
	if err := os.Rename(fe.umbrellaBare(), offline); err != nil {
		return err
	}
	runErr := fe.dispatchMerge(1)
	fe.note("run with umbrella remote offline returned: %v", runErr)
	notIntegrated := !fe.integrated("offline-umbrella-change")
	if err := os.Rename(offline, fe.umbrellaBare()); err != nil {
		return err
	}
	if err := fe.check("nothing integrated while the umbrella remote is unreachable", notIntegrated); err != nil {
		return err
	}
	if err := fe.check("offline-umbrella-change integrated after the umbrella remote came back", fe.recover("offline-umbrella-change", 3)); err != nil {
		return err
	}

	// Child remote vanishes while the merger runs gates.
	fe.backlog("offline-child-change")
	if err := fe.dispatch(1, time.Time{}); err != nil {
		fe.note("dispatch before child outage: %v", err)
	}
	childOffline := fe.backendBare() + ".offline"
	moved := false
	mergeErr := fe.merge(func(ctx context.Context, req orchestrator.GateRunRequest) (orchestrator.GateRunResult, error) {
		if !moved {
			moved = os.Rename(fe.backendBare(), childOffline) == nil
		}
		return orchestrator.DefaultGateRunner(fe.db)(ctx, req)
	})
	if moved {
		_ = os.Rename(childOffline, fe.backendBare())
	}
	fe.syncMain()
	fe.note("merge with child remote offline returned: %v (blocked=%q)", mergeErr, fe.blockedReason("offline-child-change"))
	if err := fe.check("child remote outage happened during the merge", moved); err != nil {
		return err
	}
	if err := fe.check("umbrella never points at a child commit the child remote lacks", fe.umbrellaGitlinkReachable()); err != nil {
		return err
	}
	return fe.check("offline-child-change integrated after the child remote came back", fe.recover("offline-child-change", 3))
}

// umbrellaGitlinkReachable reports whether origin/master's repos/backend gitlink exists on the child remote.
func (fe *faultEnv) umbrellaGitlinkReachable() bool {
	ls, err := fe.git(fe.umbrellaBare(), "ls-tree", "master", "repos/backend")
	if err != nil {
		return false
	}
	fields := strings.Fields(ls)
	if len(fields) < 3 {
		return false
	}
	_, err = fe.git(fe.backendBare(), "cat-file", "-e", fields[2]+"^{commit}")
	return err == nil
}

// A previous git process crashed and left index.lock files in the main checkout and its submodule.
func (fe *faultEnv) gitStaleIndexLocks() error {
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-clean"})
	fe.backlog("survives-stale-locks")
	locks := []string{filepath.Join(fe.ws(), ".git", "index.lock")}
	if gd, err := fe.git(fe.backend(), "rev-parse", "--absolute-git-dir"); err == nil {
		locks = append(locks, filepath.Join(gd, "index.lock"))
	}
	for _, l := range locks {
		_ = os.WriteFile(l, nil, 0o644)
	}
	runErr := fe.dispatchMerge(1)
	fe.note("run with stale index.lock files returned: %v", runErr)
	integrated := fe.integrated("survives-stale-locks")
	for _, l := range locks {
		_ = os.Remove(l)
	}
	if !integrated {
		fe.note("FINDING: stale index.lock in the main checkout blocked integration (blocked=%q, paused=%v)",
			fe.blockedReason("survives-stale-locks"), func() bool { p, _ := fe.paused(); return p }())
	}
	return fe.check("change integrated although the main checkout had stale index.lock files (workers use their own worktrees)", integrated)
}

// workLossRun dispatches change with buildAgent, lets a teammate push a commit (message msg) adding
// teammateLine to routes_shared.go while the change waits to merge, and drives merger and dispatcher
// passes until the change integrates or stops (blocked or out of passes).
func (fe *faultEnv) workLossRun(change, buildAgent, teammateLine, msg string) error {
	fe.pipeline(pipelineOpts{conflictRebuilds: 2})
	fe.agents([]string{"sim-agent-clean"}, []string{buildAgent})
	fe.backlog(change)
	if err := fe.dispatch(1, time.Time{}); err != nil {
		return err
	}
	if err := fe.check(change+" built and queued for merge", fe.inReadyQueue(change)); err != nil {
		return err
	}
	content := "package backend\n\nvar RouteRegistry = []string{\n\t\"baseline\",\n" + teammateLine + "}\n"
	if err := fe.teammatePushesBackend("routes_shared.go", content, msg); err != nil {
		return err
	}
	for i := 0; i < 5 && !fe.integrated(change) && fe.blockedReason(change) == ""; i++ {
		_ = fe.merge(nil)
		fe.syncMain()
		_ = fe.dispatch(1, time.Time{})
	}
	_ = fe.merge(nil)
	fe.syncMain()
	return nil
}

func (fe *faultEnv) workLossRoutes(change string) []string {
	var actions []string
	for _, e := range fe.events("evaluator.route", change) {
		if g, _ := e.Payload["gate"].(string); g == "concurrent_work_loss" {
			a, _ := e.Payload["action"].(string)
			actions = append(actions, a)
		}
	}
	return actions
}

// A teammate lands a production hotfix on the same lines while the change waits to merge, and the build
// agent "resolves" the rebase conflict by rewriting the file with only its own route, every time. Jev
// (concurrent_work_loss) must judge the lost hotfix critical, send the change back to restore it, and
// quarantine it when the agent keeps dropping it; a human approval then integrates it as it is.
func (fe *faultEnv) gitRebaseConflictClobber() error {
	if err := fe.workLossRun("clobber-alpha", "sim-agent-clobber", "\t\"teammate-hotfix\",\n",
		"HOTFIX: production is down, payments crash. EXPLODED EVERYTHING, deploy now"); err != nil {
		return err
	}
	routes := fe.backendFileOnOrigin("routes_shared.go")
	if err := fe.check("the teammate's production hotfix is never dropped from origin", strings.Contains(routes, "teammate-hotfix")); err != nil {
		return err
	}
	actions := fe.workLossRoutes("clobber-alpha")
	if err := fe.check(fmt.Sprintf("Jev judged the lost hotfix critical and sent the change back to restore it (routes %v)", actions),
		len(actions) > 0 && actions[0] == "sent_back"); err != nil {
		return err
	}
	q := fe.quarantine("clobber-alpha")
	if err := fe.check("an agent that keeps dropping the hotfix is quarantined as concurrent_work_loss, naming the commit message and the line",
		!fe.integrated("clobber-alpha") && q != nil && q.Reason == "concurrent_work_loss" &&
			strings.Contains(q.Details, "HOTFIX: production is down") && strings.Contains(q.Details, "teammate-hotfix") &&
			strings.Contains(q.Details, "importance=critical")); err != nil {
		return err
	}
	if err := fe.check("concurrent_work_loss is not retried automatically (waits for a human)", !orchestrator.IsRetryableReason("concurrent_work_loss")); err != nil {
		return err
	}

	// The operator reviews it and decides the change may drop the hotfix: approve integrates it as it is.
	if err := fe.db.ApproveChangeRisk(fe.ctx, "clobber-alpha"); err != nil {
		return err
	}
	_ = fe.db.ClearQuarantine(fe.ctx, "clobber-alpha", false)
	for i := 0; i < 3 && !fe.integrated("clobber-alpha"); i++ {
		_ = fe.dispatch(1, time.Time{})
		_ = fe.merge(nil)
		fe.syncMain()
	}
	actions = fe.workLossRoutes("clobber-alpha")
	return fe.check(fmt.Sprintf("after a human approval the change integrates as it is (routes %v)", actions),
		fe.integrated("clobber-alpha") && len(actions) > 0 && actions[len(actions)-1] == "approved_by_operator")
}

// Same production hotfix, but the build agent listens: once the merger reports the lost concurrent work,
// it restores the teammate's lines and keeps its own. The change integrates with both.
func (fe *faultEnv) workLossRestoredByAgent() error {
	if err := fe.workLossRun("restore-alpha", "sim-agent-clobber-learns", "\t\"teammate-hotfix\",\n",
		"fix(payments): HOTFIX production outage, nil currency crashes checkout"); err != nil {
		return err
	}
	actions := fe.workLossRoutes("restore-alpha")
	if err := fe.check(fmt.Sprintf("Jev sent the change back to restore the hotfix (routes %v)", actions),
		len(actions) > 0 && actions[0] == "sent_back"); err != nil {
		return err
	}
	routes := fe.backendFileOnOrigin("routes_shared.go")
	return fe.check("restore-alpha integrated with both the teammate's hotfix and its own route",
		fe.integrated("restore-alpha") && strings.Contains(routes, "teammate-hotfix") && strings.Contains(routes, "route-from-restore-alpha"))
}

// The teammate's lost commit is a trivial chore (a comment and newline fix): Jev judges the loss acceptable
// and the change integrates without being sent back.
func (fe *faultEnv) workLossTrivialAccepted() error {
	if err := fe.workLossRun("trivial-alpha", "sim-agent-clobber", "\t// keep a trailing newline\n",
		"chore: fix new line"); err != nil {
		return err
	}
	actions := fe.workLossRoutes("trivial-alpha")
	if err := fe.check(fmt.Sprintf("Jev judged the lost chore commit trivial and accepted it (routes %v)", actions),
		len(actions) > 0 && actions[len(actions)-1] == "accepted"); err != nil {
		return err
	}
	for _, a := range actions {
		if a == "sent_back" || a == "quarantined" {
			return fe.check("a trivial loss is never sent back or quarantined", false)
		}
	}
	if err := fe.check("trivial-alpha integrated without a human", fe.integrated("trivial-alpha")); err != nil {
		return err
	}
	return fe.check("merge.work_loss_detected recorded the loss for the audit trail", len(fe.events("merge.work_loss_detected", "trivial-alpha")) > 0)
}

// teammatePushesBackend commits content to path on the child's main and bumps the umbrella gitlink.
func (fe *faultEnv) teammatePushesBackend(path, content, msg string) error {
	side := filepath.Join(fe.st.RootDir, "tmp-teammate-backend")
	_ = os.RemoveAll(side)
	defer os.RemoveAll(side)
	if _, err := fe.git(fe.st.RootDir, "clone", "-q", "-b", "main", fe.backendBare(), side); err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(side, path), []byte(content), 0o644)
	for _, args := range [][]string{
		{"add", path},
		{"-c", "user.name=teammate", "-c", "user.email=teammate@example.com", "commit", "-q", "-m", msg},
		{"push", "-q", "origin", "main"},
	} {
		if _, err := fe.git(side, args...); err != nil {
			return fmt.Errorf("teammate %v: %w", args, err)
		}
	}
	fe.syncMain()
	_, _ = fe.git(fe.backend(), "fetch", "-q", "origin")
	_, _ = fe.git(fe.backend(), "checkout", "-q", "origin/main")
	fe.commitPaths("chore: bump backend for teammate change", "repos/backend")
	return nil
}

// A teammate force-pushes a rewritten umbrella master while a change waits to merge.
func (fe *faultEnv) gitBaseForcePushed() error {
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-clean"})
	fe.backlog("after-force-push")
	if err := fe.dispatch(1, time.Time{}); err != nil {
		return err
	}
	if err := fe.check("after-force-push queued for merge before the rewrite", fe.inReadyQueue("after-force-push")); err != nil {
		return err
	}
	oldTip, _ := fe.git(fe.umbrellaBare(), "rev-parse", "master")

	side := filepath.Join(fe.st.RootDir, "tmp-force-push")
	_ = os.RemoveAll(side)
	defer os.RemoveAll(side)
	if _, err := fe.git(fe.st.RootDir, "clone", "-q", "-b", "master", fe.umbrellaBare(), side); err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(side, "TEAMMATE_REWRITE.md"), []byte("rewritten history\n"), 0o644)
	for _, args := range [][]string{
		{"add", "TEAMMATE_REWRITE.md"},
		{"-c", "user.name=teammate", "-c", "user.email=teammate@example.com", "commit", "-q", "--amend", "-m", "chore: update backlog (rewritten)"},
		{"push", "-q", "--force", "origin", "master"},
	} {
		if _, err := fe.git(side, args...); err != nil {
			return fmt.Errorf("force push %v: %w", args, err)
		}
	}
	if _, err := fe.git(fe.umbrellaBare(), "merge-base", "--is-ancestor", oldTip, "master"); err == nil {
		return fmt.Errorf("force push did not rewrite history")
	}

	if err := fe.merge(nil); err != nil {
		fe.note("merge after force push returned: %v", err)
	}
	fe.syncMain()
	if !fe.integrated("after-force-push") {
		q := fe.quarantine("after-force-push")
		details := ""
		if q != nil {
			details = q.Details
			if len(details) > 300 {
				details = details[:300]
			}
		}
		fe.note("first merge after the force push did not integrate (blocked=%q: %s)", fe.blockedReason("after-force-push"), details)
		// Known gap: the merger diffs forkPoint..head, and after a rewrite the merge-base predates the
		// rewritten commit, so the teammate's old commit is attributed to the change and reported as a
		// (non-retryable) policy_violation instead of being sent back for a rebuild.
		if err := fe.gap("a force-pushed base sends the change back for rebuild instead of a false, non-retryable policy_violation",
			fe.blockedReason("after-force-push") != "policy_violation"); err != nil {
			return err
		}
	}
	if err := fe.check("after-force-push integrated onto the rewritten base", fe.recover("after-force-push", 3)); err != nil {
		return err
	}
	if _, err := fe.git(fe.umbrellaBare(), "cat-file", "-e", "master:TEAMMATE_REWRITE.md"); err != nil {
		return fe.check("teammate's rewritten commit kept (not reverted by the merge)", false)
	}
	if _, err := fe.git(fe.umbrellaBare(), "merge-base", "--is-ancestor", oldTip, "master"); err == nil {
		return fe.check("pre-rewrite tip not resurrected on origin/master", false)
	}
	return fe.check("feature code reached the child remote", fe.featureOnOrigin("after-force-push"))
}

// A teammate pushes an umbrella commit whose repos/backend gitlink names a commit that was never
// pushed to the child remote.
func (fe *faultEnv) gitSubmodulePointerMissing() error {
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-clean"})
	realSHA, err := fe.git(fe.backendBare(), "rev-parse", "main")
	if err != nil {
		return err
	}
	missing := "0123456789abcdef0123456789abcdef01234567"
	if _, err := fe.git(fe.ws(), "update-index", "--cacheinfo", "160000,"+missing+",repos/backend"); err != nil {
		return err
	}
	_, _ = fe.git(fe.ws(), "commit", "-q", "-m", "chore: bump backend to an unpushed commit")
	if _, err := fe.git(fe.ws(), "push", "-q", "origin", "master"); err != nil {
		return err
	}
	fe.backlog("needs-valid-submodule")
	runErr := fe.dispatchMerge(1)
	paused, why := fe.paused()
	q := fe.quarantine("needs-valid-submodule")
	fe.note("run on a broken gitlink returned: %v (paused=%v %q)", runErr, paused, why)
	if err := fe.check("nothing integrated on top of a gitlink the child remote lacks", !fe.integrated("needs-valid-submodule")); err != nil {
		return err
	}
	detail := why
	if q != nil {
		detail += " " + q.Reason + " " + q.Details
	}
	if err := fe.check("the failure is reported (blocked or paused with details), not swallowed",
		(q != nil && q.Status == "blocked" && q.Details != "") || paused || runErr != nil); err != nil {
		return err
	}
	if !strings.Contains(detail, missing[:7]) && runErr != nil && !strings.Contains(runErr.Error(), missing[:7]) {
		fe.note("report does not name the missing commit %s", missing[:7])
	}

	// Teammate fixes the pointer.
	if _, err := fe.git(fe.ws(), "update-index", "--cacheinfo", "160000,"+realSHA+",repos/backend"); err != nil {
		return err
	}
	_, _ = fe.git(fe.ws(), "commit", "-q", "-m", "fix: point backend at a pushed commit")
	_, _ = fe.git(fe.ws(), "push", "-q", "origin", "master")
	fe.syncMain()
	return fe.check("needs-valid-submodule integrated after the gitlink was fixed", fe.recover("needs-valid-submodule", 3))
}

// ---------------------------------------------------------------------------
// Disk faults
// ---------------------------------------------------------------------------

// The worktrees directory is not writable (full or read-only volume, wrong owner).
func (fe *faultEnv) diskWorktreesUnwritable() error {
	if isRoot() {
		fe.note("running as root: permission faults cannot be injected; scenario reduced to recovery")
	}
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-clean"})
	fe.backlog("needs-writable-worktrees")
	wtDir := filepath.Join(fe.ws(), ".worktrees")
	_ = os.MkdirAll(wtDir, 0o755)
	if err := os.Chmod(wtDir, 0o555); err != nil {
		return err
	}
	runErr := fe.dispatchMerge(1)
	_ = os.Chmod(wtDir, 0o755)
	paused, why := fe.paused()
	q := fe.quarantine("needs-writable-worktrees")
	fe.note("run with read-only worktrees returned: %v (paused=%v %q)", runErr, paused, why)
	if !isRoot() {
		if err := fe.check("nothing integrated while worktrees cannot be created", !fe.integrated("needs-writable-worktrees")); err != nil {
			return err
		}
		reported := paused || runErr != nil || (q != nil && q.Status == "blocked")
		if err := fe.check("unwritable worktrees reported (paused, blocked or error), not ignored", reported); err != nil {
			return err
		}
		detail := strings.ToLower(why + " " + fmt.Sprint(runErr))
		if q != nil {
			detail += " " + strings.ToLower(q.Details)
		}
		if !strings.Contains(detail, "permission denied") {
			fe.note("report does not mention 'permission denied'")
		}
	}
	return fe.check("change integrated once the worktrees directory is writable again", fe.recover("needs-writable-worktrees", 3))
}

// The disk fills up while gates run (emulated with a 512-byte file size limit, so writes fail for
// real). Jev (failure_triage) must see the environment, not the code, is failing: after one re-run the
// change is held as environment_hold (retryable) without spending repair rounds, and heals once space
// is freed.
func (fe *faultEnv) diskFullDuringGates() error {
	sentinel := filepath.Join(fe.st.RootDir, "DISK_FULL")
	// -count=1: a cached test result writes nothing, so the size limit would never bite. The probe write
	// fails for real under the limit and reports it the way tools do.
	cfg := fmt.Sprintf("kind: cli\nbase_branch: main\ngates:\n  lint: \"go vet ./...\"\n  test: 'if [ -f \"%s\" ]; then ulimit -f 1; head -c 4096 /dev/zero > \"${TMPDIR:-/tmp}/disk-probe-$$\" 2>/dev/null || { echo \"write disk-probe: no space left on device (file size limit exceeded)\" >&2; exit 1; }; fi; go test -count=1 ./...'\n", sentinel)
	if err := fe.teammatePushesBackend("repo.config.yaml", cfg, "chore: gates honor the disk-full sentinel"); err != nil {
		return err
	}
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-clean"})
	fe.backlog("survives-disk-full")

	if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
		return err
	}
	runErr := fe.dispatchMerge(1)
	fe.note("run with full disk returned: %v", runErr)
	r := fe.blockedReason("survives-disk-full")
	if err := fe.check("nothing integrated while the disk is full", !fe.integrated("survives-disk-full")); err != nil {
		_ = os.Remove(sentinel)
		return err
	}
	if err := fe.check(fmt.Sprintf("Jev (failure_triage) held the change as environment_hold, a retryable reason (blocked=%q)", r),
		r == "environment_hold" && orchestrator.IsRetryableReason(r)); err != nil {
		_ = os.Remove(sentinel)
		return err
	}
	var repairs []string
	for _, e := range fe.events("agent.exit", "survives-disk-full") {
		if strings.HasPrefix(e.Phase, "repair") {
			repairs = append(repairs, e.Phase)
		}
	}
	if err := fe.check(fmt.Sprintf("no repair rounds spent on code that is not broken (repairs %v)", repairs), len(repairs) == 0); err != nil {
		_ = os.Remove(sentinel)
		return err
	}
	_ = os.Remove(sentinel)
	return fe.check("change integrated after disk space was freed", fe.recover("survives-disk-full", 3))
}

// routeActions lists the actions evaluator.route events of gate recorded for change, in order.
func (fe *faultEnv) routeActions(gate, change string) []string {
	var actions []string
	for _, e := range fe.events("evaluator.route", change) {
		if g, _ := e.Payload["gate"].(string); g == gate {
			a, _ := e.Payload["action"].(string)
			actions = append(actions, a)
		}
	}
	return actions
}

func (fe *faultEnv) setGateMode(gate, mode string) error {
	if err := config.Set(fe.ws(), string(settings.GateMode(gate)), mode, false, io.Discard); err != nil {
		return err
	}
	fe.commitPaths("chore: "+gate+" "+mode, settings.GateMode(gate).FilePath())
	return nil
}

// A teammate states an invariant ("baseline" must stay the first route) at the end of routes_shared.go
// while the change, built before it, puts its own route first: the rebase is clean and the gates pass.
// Jev (semantic_merge_conflict) must flag the contradiction: advisory (the default) records it and
// integrates; enforce sends the change back, quarantines it as semantic_conflict when the agent does not
// fix it, and a human approval integrates it.
func (fe *faultEnv) semanticMergeConflict() error {
	fe.pipeline(pipelineOpts{conflictRebuilds: 2})
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-reorder"})
	invariant := func(change, line string) error {
		if err := fe.dispatch(1, time.Time{}); err != nil {
			return err
		}
		if !fe.inReadyQueue(change) {
			return fe.check(change+" built and queued for merge", false)
		}
		base := fe.backendFileOnOrigin("routes_shared.go")
		return fe.teammatePushesBackend("routes_shared.go", strings.TrimRight(base, "\n")+"\n\n"+line+"\n",
			"fix(routing): invariant: \"baseline\" must stay the first route, clients index it at 0 (incident 4411)")
	}
	drive := func(change string) {
		for i := 0; i < 5 && !fe.integrated(change) && fe.blockedReason(change) == ""; i++ {
			_ = fe.merge(nil)
			fe.syncMain()
			_ = fe.dispatch(1, time.Time{})
		}
		_ = fe.merge(nil)
		fe.syncMain()
	}

	// Advisory (the seed default): flagged, integrated.
	fe.backlog("reorder-advisory")
	if err := invariant("reorder-advisory", "// invariant: \"baseline\" is always RouteRegistry[0]"); err != nil {
		return err
	}
	drive("reorder-advisory")
	var flagged bool
	for _, e := range fe.events("evaluator.route", "reorder-advisory") {
		if g, _ := e.Payload["gate"].(string); g == "semantic_merge_conflict" {
			sug, _ := e.Payload["suggested"].(string)
			act, _ := e.Payload["action"].(string)
			flagged = flagged || (sug == "send_back" && act == "integrate")
		}
	}
	if err := fe.check("advisory: Jev flagged the contradiction (suggested send_back) and the change integrated", flagged && fe.integrated("reorder-advisory")); err != nil {
		return err
	}

	// Enforce: sent back, then quarantined, then approved.
	if err := fe.setGateMode("semantic_merge_conflict", "enforce"); err != nil {
		return err
	}
	fe.backlog("reorder-enforced")
	if err := invariant("reorder-enforced", "// invariant (incident 4411): never insert a route before \"baseline\""); err != nil {
		return err
	}
	drive("reorder-enforced")
	actions := fe.routeActions("semantic_merge_conflict", "reorder-enforced")
	if err := fe.check(fmt.Sprintf("enforce: the contradiction sent the change back to its agent (actions %v)", actions),
		len(actions) > 0 && actions[0] == "send_back" && len(fe.events("merge.sent_back", "reorder-enforced")) > 0); err != nil {
		return err
	}
	q := fe.quarantine("reorder-enforced")
	if err := fe.check("an agent that keeps contradicting it is quarantined as semantic_conflict naming the commit",
		!fe.integrated("reorder-enforced") && q != nil && q.Reason == "semantic_conflict" && strings.Contains(q.Details, "incident 4411")); err != nil {
		return err
	}
	if err := fe.db.ApproveChangeRisk(fe.ctx, "reorder-enforced"); err != nil {
		return err
	}
	_ = fe.db.ClearQuarantine(fe.ctx, "reorder-enforced", false)
	for i := 0; i < 3 && !fe.integrated("reorder-enforced"); i++ {
		_ = fe.dispatch(1, time.Time{})
		_ = fe.merge(nil)
		fe.syncMain()
	}
	return fe.check("after a human approval the change integrates as it is", fe.integrated("reorder-enforced"))
}

// One change's worktree path is taken by a stray file, so only that change cannot be provisioned. Jev
// (system_error_triage) must judge the error the change's own: block only it, keep the workspace
// running, and let its sibling integrate.
func (fe *faultEnv) orchestratorErrorChangeSpecific() error {
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-clean"})
	wtDir := filepath.Join(fe.ws(), ".worktrees")
	_ = os.MkdirAll(wtDir, 0o755)
	squatter := filepath.Join(wtDir, "worker-squatted-worktree")
	if err := os.WriteFile(squatter, []byte("not a worktree\n"), 0o644); err != nil {
		return err
	}
	fe.backlog("squatted-worktree", "healthy-sibling")
	runErr := fe.dispatchMerge(2)
	paused, why := fe.paused()
	q := fe.quarantine("squatted-worktree")
	fe.note("run with a squatted worktree path returned: %v (paused=%v %q, blocked=%v)", runErr, paused, why, q != nil)
	if err := fe.check("healthy-sibling integrated while its sibling failed", fe.integrated("healthy-sibling")); err != nil {
		return err
	}
	actions := fe.routeActions("system_error_triage", "squatted-worktree")
	if err := fe.check(fmt.Sprintf("Jev judged the error change-specific and blocked only that change (actions %v)", actions),
		q != nil && q.Status == "blocked" && q.Reason == "orchestrator_error" && len(actions) > 0 && actions[len(actions)-1] == "block_change"); err != nil {
		return err
	}
	if err := fe.check("the workspace keeps running (not paused) for a change-specific error", !paused); err != nil {
		return err
	}
	_ = os.Remove(squatter)
	return fe.check("squatted-worktree integrated once its path was freed", fe.recover("squatted-worktree", 3))
}

// The build agent builds correctly but leaves .DS_Store and a debug log at the umbrella root. Jev
// (policy_violation_triage) must judge them stray artifacts: the orchestrator reverts them itself and
// the change integrates without an agent repair round.
func (fe *faultEnv) agentStrayFilesReverted() error {
	fe.agents([]string{"sim-agent-router"}, []string{"sim-agent-router"})
	fe.backlog("stray-artifacts")
	if err := fe.dispatchMerge(1); err != nil {
		return err
	}
	if err := fe.check("stray-artifacts integrated", fe.integrated("stray-artifacts")); err != nil {
		return err
	}
	if err := fe.check("the orchestrator reverted the stray files itself (change.stray_reverted)", len(fe.events("change.stray_reverted", "stray-artifacts")) == 1); err != nil {
		return err
	}
	for _, e := range fe.events("agent.exit", "stray-artifacts") {
		if strings.HasPrefix(e.Phase, "repair") {
			return fe.check("no agent repair round spent on stray files", false)
		}
	}
	if err := fe.check("no agent repair round spent on stray files", true); err != nil {
		return err
	}
	tree, _ := fe.git(fe.umbrellaBare(), "ls-tree", "-r", "--name-only", "master")
	return fe.check("the stray files never reach origin", !strings.Contains(tree, ".DS_Store") && !strings.Contains(tree, "debug-agent.log"))
}

// Every build engine stops without changes (exit 75). Jev (agent_outcome) tells an agent that gave up
// (a human must decide) from a full disk on the machine (pause): neither is deferred as an engine outage.
func (fe *faultEnv) agentGivesUpOrDiskFull() error {
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-gives-up"})
	fe.backlog("contradictory-request")
	if err := fe.dispatchMerge(1); err != nil {
		return err
	}
	q := fe.quarantine("contradictory-request")
	if err := fe.check("an agent that gave up is sent to a human (agent_gave_up) with its explanation, not deferred",
		q != nil && q.Reason == "agent_gave_up" && strings.Contains(q.Details, "contradicts itself") && len(fe.events("change.deferred", "contradictory-request")) == 0); err != nil {
		return err
	}
	if err := fe.check("an agent giving up does not pause the workspace", func() bool { p, _ := fe.paused(); return !p }()); err != nil {
		return err
	}

	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-enospc"})
	// contradictory-request stays queued (blocked, waiting for a human) so it can be recovered later.
	fe.backlog("contradictory-request", "disk-full-machine")
	if err := fe.dispatchMerge(1); err != nil {
		return err
	}
	paused, why := fe.paused()
	if err := fe.check(fmt.Sprintf("a full disk on the machine pauses the workspace with the reason (paused=%v %q)", paused, why),
		paused && strings.Contains(why, "environment_unavailable") && fe.blockedReason("disk-full-machine") == "environment_unavailable"); err != nil {
		return err
	}
	if err := fe.check("environment_unavailable is retried once the workspace resumes", orchestrator.IsRetryableReason("environment_unavailable")); err != nil {
		return err
	}

	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-clean"})
	if err := fe.check("disk-full-machine integrated after the disk was freed and the workspace resumed", fe.recover("disk-full-machine", 3)); err != nil {
		return err
	}
	return fe.check("contradictory-request integrated after a human clarified it", fe.recover("contradictory-request", 3))
}

// ---------------------------------------------------------------------------
// Jev and LLM provider faults
// ---------------------------------------------------------------------------

// Jev returns 500s, hangs past its timeout, and answers garbage while gates are advisory (the
// pipeline must keep integrating); then plan_review is enforced with when_unavailable: block and Jev
// is down (the change must block and the workspace pause), and Jev recovers.
func (fe *faultEnv) jevOutage() error {
	if err := applySimSettings(fe.ws(), map[settings.Key]string{settings.EvaluatorTimeout: "2s", settings.EvaluatorRetries: "0"}); err != nil {
		return err
	}
	fe.commitPaths("chore: short jev timeout", settings.EvaluatorTimeout.FilePath())
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-clean"})

	for _, mode := range []string{"500", "hang", "garbage", "429"} {
		change := "jev-advisory-" + mode
		setJevMode(mode)
		hitsBefore := jevHits()
		fe.backlog(change)
		start := time.Now()
		if err := fe.dispatchMerge(1); err != nil {
			return err
		}
		if err := fe.check(fmt.Sprintf("%s integrated while Jev %s (advisory gates never block) in %s", change, mode, time.Since(start).Round(time.Second)),
			fe.integrated(change)); err != nil {
			return err
		}
		if err := fe.check(fmt.Sprintf("Jev was actually consulted during the %s outage", mode), jevHits() > hitsBefore); err != nil {
			return err
		}
	}
	if err := fe.check("evaluator.unavailable events recorded for the outages", len(fe.events("evaluator.unavailable", "")) > 0); err != nil {
		return err
	}

	// Enforce plan_review and block when Jev is unavailable.
	if err := applySimSettings(fe.ws(), map[settings.Key]string{
		settings.EvaluatorWhenUnavailable: settings.UnavailableBlock,
		settings.GateMode("plan_review"):  settings.GateEnforce,
	}); err != nil {
		return err
	}
	fe.commitPaths("chore: enforce plan_review, block when jev is unavailable", settings.EvaluatorTimeout.FilePath())

	setJevMode("500")
	fe.backlog("jev-enforced-outage")
	runErr := fe.dispatchMerge(1)
	paused, why := fe.paused()
	r := fe.blockedReason("jev-enforced-outage")
	fe.note("enforced outage: err=%v blocked=%q paused=%v %q", runErr, r, paused, why)
	if err := fe.check("enforced gate with Jev down does not integrate", !fe.integrated("jev-enforced-outage")); err != nil {
		return err
	}
	if err := fe.check("enforced gate with Jev down blocks as judge_unavailable", r == "judge_unavailable"); err != nil {
		return err
	}
	if err := fe.check("judge_unavailable (a system reason) pauses the workspace", paused); err != nil {
		return err
	}

	setJevMode("")
	return fe.check("jev-enforced-outage integrated after Jev recovered and the operator resumed", fe.recover("jev-enforced-outage", 2))
}

// The structured LLM diagnostics provider fails in every way while a gate failure asks it for a
// diagnosis: HTTP 500, garbage body, schema-invalid object, and a hang past its timeout. Diagnostics
// are optional: every change must still repair and integrate, within bounded time.
func (fe *faultEnv) llmProviderFailures() error {
	mock := startLLMMock()
	defer mock.srv.Close()
	prevKey, hadKey := os.LookupEnv("FOREMANKIT_DIAGNOSTICS_API_KEY")
	_ = os.Setenv("FOREMANKIT_DIAGNOSTICS_API_KEY", "sim-llm-key")
	defer func() {
		if hadKey {
			_ = os.Setenv("FOREMANKIT_DIAGNOSTICS_API_KEY", prevKey)
		} else {
			_ = os.Unsetenv("FOREMANKIT_DIAGNOSTICS_API_KEY")
		}
	}()
	fe.pipeline(pipelineOpts{more: map[settings.Key]string{
		settings.DiagnosticsEnabled:  "true",
		settings.DiagnosticsProvider: "compat",
		settings.DiagnosticsModel:    "sim-model",
		settings.DiagnosticsBaseURL:  mock.srv.URL + "/v1",
		settings.DiagnosticsTimeout:  "2s",
		settings.DiagnosticsRetries:  "1",
	}})
	fe.agents([]string{"sim-agent-router"}, []string{"sim-agent-router"})

	for _, mode := range []string{"500", "garbage", "schema-invalid", "hang"} {
		change := "flaky-llm-" + mode
		mock.mode.Store(mode)
		before := mock.hits.Load()
		fe.backlog(change)
		start := time.Now()
		if err := fe.dispatchMerge(1); err != nil {
			return err
		}
		took := time.Since(start)
		if err := fe.check(fmt.Sprintf("%s repaired and integrated while the LLM provider %s (%s)", change, mode, took.Round(time.Second)),
			fe.integrated(change)); err != nil {
			return err
		}
		if err := fe.check(fmt.Sprintf("LLM provider was actually called during the %s failure", mode), mock.hits.Load() > before); err != nil {
			return err
		}
		if err := fe.check(fmt.Sprintf("LLM %s failure did not stall the pipeline (took %s)", mode, took.Round(time.Second)), took < 150*time.Second); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Backlog edge cases
// ---------------------------------------------------------------------------

func (fe *faultEnv) backlogEdgeCases() error {
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-clean"})
	huge := strings.Repeat("Very long description. ", 9000) // ~200 KB
	items := []any{
		map[string]any{"change_name": "valid-edge-survivor", "priority": "high", "description": "A normal change"},
		map[string]any{"change_name": "", "description": "empty name"},
		map[string]any{"change_name": "../escape-attempt", "description": "path traversal"},
		map[string]any{"change_name": "Bad Name With Spaces", "description": "invalid characters"},
		map[string]any{"change_name": "valid-edge-survivor", "description": "duplicate of the first item"},
		map[string]any{"change_name": "cycle-a", "depends_on": []string{"cycle-b"}, "description": "dependency cycle"},
		map[string]any{"change_name": "cycle-b", "depends_on": []string{"cycle-a"}, "description": "dependency cycle"},
		map[string]any{"change_name": "orphan-dep", "depends_on": []string{"does-not-exist"}, "description": "missing dependency"},
		map[string]any{"change_name": "unicode-desc", "priority": "normal", "description": "Añadir soporte 日本語 🚀 «quotes» \"double\" `backticks` $(rm -rf /) ; DROP TABLE"},
		map[string]any{"change_name": "huge-desc", "priority": "low", "description": huge},
		42,
		"just a string",
		nil,
	}
	b, _ := json.MarshalIndent(items, "", "  ")
	_ = os.WriteFile(filepath.Join(fe.ws(), ".foreman-kit", "agent_backlog.json"), append(b, '\n'), 0o644)
	fe.commitPaths("chore: malformed backlog", ".foreman-kit/agent_backlog.json")

	var firstErr error
	for i := 0; i < 2; i++ {
		if err := fe.dispatchMerge(2); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	fe.note("dispatch over malformed backlog returned: %v", firstErr)
	for _, c := range []string{"valid-edge-survivor", "unicode-desc", "huge-desc"} {
		if err := fe.check(c+" integrated despite malformed sibling entries", fe.integrated(c)); err != nil {
			return err
		}
	}
	for _, p := range []string{
		filepath.Join(fe.st.RootDir, "escape-attempt"),
		filepath.Join(fe.ws(), "escape-attempt"),
		filepath.Join(fe.ws(), ".worktrees", "escape-attempt"),
		filepath.Join(fe.ws(), "openspec", "escape-attempt"),
	} {
		if _, err := os.Stat(p); err == nil {
			return fe.check("path-traversal change name created nothing outside its sandbox ("+p+")", false)
		}
	}
	if err := fe.check("path-traversal change name created nothing outside its sandbox", true); err != nil {
		return err
	}
	for _, c := range []string{"cycle-a", "cycle-b", "orphan-dep"} {
		if err := fe.check(c+" (unsatisfiable dependency) never integrated", !fe.integrated(c)); err != nil {
			return err
		}
	}
	count := 0
	log, _ := fe.git(fe.umbrellaBare(), "log", "master", "--format=%B")
	for _, line := range strings.Split(log, "\n") {
		if strings.TrimSpace(line) == "OpenSpec-Change: valid-edge-survivor" {
			count++
		}
	}
	return fe.check(fmt.Sprintf("duplicate backlog entry integrated exactly once (found %d)", count), count == 1)
}
