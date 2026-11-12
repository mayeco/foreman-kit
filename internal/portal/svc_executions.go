package portal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// executions starts, follows and stops what the portal launches: an agent on a prompt, or an
// operation of the kit. Each is a row of executions, which the store keeps; what lives here is only
// what dies with the process: the way to cancel each one that is running.
type executions struct {
	root  string
	store *store.Store
	kit   Kit

	mu      sync.Mutex
	running map[string]*runningExec

	shutdownInit sync.Once
	shutdownOnce sync.Once
	shutdownCh   chan struct{}
}

// runningExec is an execution this process started and has not seen end.
type runningExec struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func newExecutions(root string, st *store.Store, kit Kit) *executions {
	return &executions{root: root, store: st, kit: kit, running: make(map[string]*runningExec)}
}

// Exit code an execution is recorded with when the portal stopped it: the one a process ended by
// SIGTERM reports.
const stoppedExitCode = 143

// The operations of the kit the portal can start, and how each is named in the list of executions.
var kitOperations = map[string]string{
	"dispatch":           "foreman-kit dispatch",
	"merge":              "foreman-kit merge",
	"gates":              "foreman-kit gates --all",
	"preflight":          "foreman-kit preflight",
	"preflight-services": "foreman-kit preflight --services",
}

// ---- shutdown --------------------------------------------------------------------------------

// done is closed when the portal starts shutting down. Streams watch it: http.Server.Shutdown
// neither cancels the context of a request nor closes its connection.
func (x *executions) done() <-chan struct{} {
	x.shutdownInit.Do(func() { x.shutdownCh = make(chan struct{}) })
	return x.shutdownCh
}

// beginShutdown ends the streams and cancels every execution this process started, so that none
// outlives the portal.
func (x *executions) beginShutdown() {
	x.done()
	x.shutdownOnce.Do(func() {
		close(x.shutdownCh)
		x.mu.Lock()
		cancels := make([]context.CancelFunc, 0, len(x.running))
		for _, re := range x.running {
			cancels = append(cancels, re.cancel)
		}
		x.mu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
	})
}

// wait blocks until every execution this process started has ended, or until timeout.
func (x *executions) wait(timeout time.Duration) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		x.mu.Lock()
		var pending *runningExec
		for _, re := range x.running {
			pending = re
			break
		}
		x.mu.Unlock()
		if pending == nil {
			return
		}
		select {
		case <-pending.done:
		case <-deadline.C:
			return
		}
	}
}

func (x *executions) register(id string, cancel context.CancelFunc) *runningExec {
	x.mu.Lock()
	defer x.mu.Unlock()
	re := &runningExec{cancel: cancel, done: make(chan struct{})}
	x.running[id] = re
	// beginShutdown cancels the executions registered when it runs. One registered afterwards, by a
	// request still in flight when the server began shutting down, is canceled here: nothing else
	// would stop it. shutdownCh is closed before beginShutdown takes the lock, so an execution is
	// either collected there or canceled here.
	select {
	case <-x.done():
		cancel()
	default:
	}
	return re
}

func (x *executions) unregister(id string, re *runningExec) {
	x.mu.Lock()
	if cur, ok := x.running[id]; ok && cur == re {
		delete(x.running, id)
	}
	x.mu.Unlock()
	close(re.done)
}

// owns reports whether this process started the execution id and has not seen it end.
func (x *executions) owns(id string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.running[id] != nil
}

// ---- what a prompt can run on -------------------------------------------------------------------

// EngineChoice is an engine a person can run a prompt on.
type EngineChoice struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// Installed says the command of the engine is on PATH; Selected, that this application uses it
	// (.foreman-kit/agents.yaml).
	Installed bool `json:"installed"`
	Selected  bool `json:"selected"`
	// Roles are the roles of this application that try the engine, in the order the kit names them.
	Roles []string `json:"roles"`
}

// Engines lists the engines the kit knows, in the order it knows them.
func (x *executions) Engines() []EngineChoice {
	selected := map[string]bool{}
	for _, name := range cycle.LoadAgentsConfig(x.root).Agents {
		selected[name] = true
	}
	roles := map[string][]string{}
	for _, role := range settings.Roles {
		for _, name := range cycle.RoleEngines(x.root, role) {
			roles[name] = append(roles[name], role)
		}
	}
	out := make([]EngineChoice, 0, len(cycle.SupportedAgentOrder))
	for _, name := range cycle.SupportedAgentOrder {
		// An agent runs with the command the application writes for it: that is the one to find.
		_, err := exec.LookPath(config.ExecutableBinary(x.root, name))
		choice := EngineChoice{Name: name, Label: cycle.SupportedAgents[name].Label, Installed: err == nil, Selected: selected[name], Roles: roles[name]}
		if choice.Roles == nil {
			choice.Roles = []string{}
		}
		out = append(out, choice)
	}
	return out
}

// SettingValue is a setting as it stands in this application: the value the commands of the kit
// read, written the way its file holds it, and where a person changes it.
type SettingValue struct {
	// Value is "" when nothing usable sets the key: Problem then says why (it is missing, or what is
	// wrong with what the file holds).
	Value   string `json:"value"`
	Problem string `json:"problem,omitempty"`
	// Key names the setting as `foreman-kit config set` takes it, and Where says it with its file.
	Key   string `json:"key"`
	Where string `json:"where"`
	// OverriddenBy says what replaces, for this process, what the file holds, with its value: the
	// catalog of settings names it.
	OverriddenBy string `json:"overridden_by,omitempty"`
}

// AgentSetup is how this application runs its agents: how it picks among the ones a role names,
// what it lets them do, and for how long one may show no activity before the kit stops it.
type AgentSetup struct {
	Selection   SettingValue `json:"selection"`
	Permissions SettingValue `json:"permissions"`
	IdleTimeout SettingValue `json:"idle_timeout"`
}

// AgentSetup reads how this application runs its agents, as the catalog of settings describes each
// key: the value in effect (what replaces the key for this process, or what its file holds), and
// nothing in the place of a key the kit cannot use.
func (x *executions) AgentSetup() AgentSetup {
	r := &settings.Reader{Root: x.root, Stderr: io.Discard}
	value := func(key settings.Key) SettingValue {
		d := r.Describe(key)
		v := SettingValue{Value: d.InEffect, Key: string(key), Where: key.Where(), OverriddenBy: d.OverriddenBy}
		if v.Value == "" {
			v.Problem = d.Problem
		}
		return v
	}
	return AgentSetup{
		Selection:   value(settings.AgentSelection),
		Permissions: value(settings.AgentPermissions),
		IdleTimeout: value(settings.IdleTimeout),
	}
}

// stubEngine is the engine of the simulations: the script AGENT_STUB names stands in for an agent.
const stubEngine = "stub"

// Ping runs the ping of an engine and records how it answered as the health of the engine. It is
// 404 for a name the kit runs no engine under.
func (x *executions) Ping(ctx context.Context, name string) (PingResult, error) {
	name = strings.TrimSpace(name)
	var res PingResult
	switch _, known := cycle.SupportedAgents[name]; {
	case name == stubEngine:
		res = PingResult{Engine: name, Status: "ok", Health: "ok", Output: "pong"}
		if stub := strings.TrimSpace(os.Getenv("AGENT_STUB")); stub == "" || !isFile(stub) {
			res = PingResult{Engine: name, Status: "not_installed", Health: "not_installed", Detail: "AGENT_STUB is not set"}
		}
	case known:
		res = x.kit.PingEngine(ctx, x.root, name)
	default:
		return res, errNotFound("the kit runs no engine named %s", name)
	}
	if res.Health == "" {
		// A kit that says how the ping ended and not what that makes of the engine.
		res.Health = map[string]string{"ok": "ok", "timeout": "timeout", "not_installed": "not_installed"}[res.Status]
		if res.Health == "" {
			res.Health = "config_error"
		}
	}
	if err := x.store.RecordEnginePing(ctx, name, res.Health, res.LatencyMS, res.Problem()); err != nil {
		return res, err
	}
	return res, nil
}

// autoEngine asks for the agents this application uses, tried the way the application picks among
// its agents. Nothing runs in the place of an agent the application does not name: an application
// that names none has no agent to run a prompt on.
const autoEngine = "auto"

// DefaultEngine returns the engine a prompt runs on when none is asked for: the first agent this
// application uses, or autoEngine when it names none.
func (x *executions) DefaultEngine() string {
	if cfg := cycle.LoadAgentsConfig(x.root); len(cfg.Agents) > 0 {
		return cfg.Agents[0]
	}
	return autoEngine
}

// HasWorktree reports whether change has a worktree now: an agent or the gates run in one only
// while the change is being worked on.
func (x *executions) HasWorktree(change string) bool {
	return cycle.ChangeNameRe.MatchString(change) && isDir(worktreeOf(x.root, change))
}

// ---- starting ---------------------------------------------------------------------------------

// maxPromptBytes is the limit in bytes of an agent prompt sent from the portal, measured after
// surrounding white space is trimmed.
const maxPromptBytes = 256 << 10

// AgentRun is a prompt to run an agent on.
type AgentRun struct {
	// Engine is an agent the kit knows, "auto" for the agents of this application as it picks among
	// them, or "" for the first of them.
	Engine string
	Prompt string
	// Change runs the agent in the worktree of that change; "" runs it at the root of the workspace.
	Change string

	// What the execution continues or repeats, when it does.
	parent, relation, summary string
}

// StartAgent starts an agent on a prompt, in the background, and returns the execution it created.
func (x *executions) StartAgent(ctx context.Context, in AgentRun) (store.Execution, error) {
	if len(strings.TrimSpace(in.Prompt)) > maxPromptBytes {
		return store.Execution{}, errInvalid("the prompt is longer than %d bytes", maxPromptBytes)
	}
	return x.startAgent(ctx, in)
}

func (x *executions) startAgent(ctx context.Context, in AgentRun) (store.Execution, error) {
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return store.Execution{}, errInvalid("the prompt is empty")
	}
	engine := strings.TrimSpace(in.Engine)
	if engine != "" && engine != autoEngine && engine != "stub" {
		if _, ok := cycle.SupportedAgents[engine]; !ok {
			return store.Execution{}, errInvalid("%q is not an agent the kit knows", engine)
		}
	}
	if engine == "" {
		engine = x.DefaultEngine()
	}
	// The execution records the agent that was asked for; the kit is given the agents to run. They
	// differ for autoEngine, which a follow-up asks for again: the application may use others by then.
	engines := engine
	if engine == autoEngine {
		agents := cycle.LoadAgentsConfig(x.root).Agents
		if len(agents) == 0 {
			return store.Execution{}, errConflict("this application uses no agent to run the prompt on: %s lists none", settings.Agents.FilePath())
		}
		engines = strings.Join(agents, ",")
	}
	workdir, err := x.workdirOf(in.Change)
	if err != nil {
		return store.Execution{}, err
	}

	id := fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102T150405Z"), randomHex(4))
	ex := store.Execution{
		ExecID:         id,
		Kind:           "portal_run",
		ChangeName:     strings.TrimSpace(in.Change),
		Agent:          engine,
		Status:         "running",
		PID:            os.Getpid(),
		PromptText:     prompt,
		Workdir:        workdir,
		ParentExecID:   in.parent,
		ParentRelation: in.relation,
		Summary:        in.summary,
	}
	if err := x.store.CreateExecution(ctx, ex); err != nil {
		return store.Execution{}, err
	}
	x.launch(id, func(runCtx context.Context) (string, int) {
		// The agent runs under this execution: each try of each engine is then an attempt of it, and
		// an agent that goes idle shows on its page like one of a worker does.
		out, code, err := x.kit.RunAgent(store.WithExecution(runCtx, id), workdir, engines, prompt)
		return failureNote(runCtx, out, code, "the agent", err)
	})
	return x.created(ctx, ex)
}

// StartOperation starts an operation of the kit in the background and returns the execution it
// created. The operation is one of kitOperations at the root of the workspace; with a change, it is
// "gates" in the worktree of that change.
func (x *executions) StartOperation(ctx context.Context, operation, change string) (store.Execution, error) {
	command, known := kitOperations[operation]
	if !known {
		return store.Execution{}, errInvalid("%q is not an operation the portal starts", operation)
	}
	name := operation
	if change = strings.TrimSpace(change); change != "" {
		if operation != "gates" {
			return store.Execution{}, errInvalid("only the gates run in the worktree of a change")
		}
		name = "gates-" + change
	}
	dir, err := x.workdirOf(change)
	if err != nil {
		return store.Execution{}, err
	}

	id := fmt.Sprintf("%s-%s-%s", time.Now().UTC().Format("20060102T150405Z"), name, randomHex(4))
	ex := store.Execution{
		ExecID:      id,
		Kind:        "portal_job",
		ChangeName:  change,
		Phase:       name,
		JobKind:     name,
		CommandText: command,
		Status:      "running",
		PID:         os.Getpid(),
		Workdir:     dir,
	}
	if err := x.store.CreateExecution(ctx, ex); err != nil {
		return store.Execution{}, err
	}
	x.launch(id, func(runCtx context.Context) (string, int) {
		// The operation runs under this execution: its output is recorded while it runs.
		out, code, err := x.kit.RunOperation(store.WithExecution(runCtx, id), dir, name)
		return failureNote(runCtx, out, code, name, err)
	})
	return x.created(ctx, ex)
}

// failureNote returns the output and the exit code of work the kit ran, with the reason the kit gave
// when it could not run it: without it the execution would say that it failed, and not why. Work
// that was stopped needs no reason: it is recorded as stopped.
func failureNote(ctx context.Context, out string, code int, what string, err error) (string, int) {
	if err == nil || ctx.Err() != nil {
		return out, code
	}
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if code == 0 {
		code = 1
	}
	return out + fmt.Sprintf("portal: %s: %v\n", what, err), code
}

// created returns the row of an execution just started, as the store has it.
func (x *executions) created(ctx context.Context, ex store.Execution) (store.Execution, error) {
	got, err := x.store.GetExecution(ctx, ex.ExecID)
	if err != nil {
		return store.Execution{}, err
	}
	if got == nil {
		return store.Execution{}, fmt.Errorf("execution %s was created and is not on record", ex.ExecID)
	}
	return *got, nil
}

// launch runs work in the background as the execution id, and records how it ended. Whoever records
// the end first has it: the work that finished, or the stop that arrived. Each writes only while the
// execution is still running, so neither writes over the other.
func (x *executions) launch(id string, work func(ctx context.Context) (output string, exitCode int)) {
	ctx, cancel := context.WithCancel(context.Background())
	re := x.register(id, cancel)
	go func() {
		defer cancel()
		defer x.unregister(id, re)
		// The store outlives the request and the work: its writes do not stop with either.
		record := context.Background()
		defer func() {
			// A panic in what the portal launched ends that execution, not the daemon that hosts the portal.
			if r := recover(); r != nil {
				x.logWrite(id, x.store.AppendLogChunk(record, id, "combined", fmt.Sprintf("\nportal: the execution panicked: %v\n", r)))
				_, err := x.store.FinishRunningExecution(record, id, "failed", 1)
				x.logWrite(id, err)
			}
		}()
		out, code := work(ctx)
		if out != "" {
			x.logWrite(id, x.store.AppendLogChunk(record, id, "combined", out))
		}
		if ctx.Err() != nil {
			_, err := x.store.FinishRunningExecution(record, id, "stopped", stoppedExitCode)
			x.logWrite(id, err)
			return
		}
		status := "succeeded"
		if code != 0 {
			status = "failed"
		}
		_, err := x.store.FinishRunningExecution(record, id, status, code)
		x.logWrite(id, err)
	}()
}

// logWrite reports a write about an execution that failed. The execution itself goes on or has
// ended: there is nobody to return the error to.
func (x *executions) logWrite(id string, err error) {
	if err != nil {
		log.Printf("portal: execution %s: %v", id, err)
	}
}

// ---- continuing, repeating, stopping -----------------------------------------------------------

// agentExecution returns the agent run id, which a person started from the portal.
func (x *executions) agentExecution(ctx context.Context, id string) (*store.Execution, error) {
	ex, err := x.store.GetExecution(ctx, id)
	if err != nil {
		return nil, err
	}
	if ex == nil {
		return nil, errNotFound("there is no execution %q", id)
	}
	if ex.Kind != "portal_run" {
		return nil, errConflict("the execution %s is not an agent run started from the portal: only those are continued or run again", id)
	}
	return ex, nil
}

// changeOfWorkdir returns the change whose worktree dir is, "" when it is not one that still exists.
func (x *executions) changeOfWorkdir(dir string) string {
	if dir == "" || !isDir(dir) {
		return ""
	}
	if filepath.Clean(filepath.Dir(dir)) != filepath.Clean(settings.WorktreesBase(x.root)) {
		return ""
	}
	name, ok := strings.CutPrefix(filepath.Base(dir), "worker-")
	if !ok {
		return ""
	}
	return name
}

// Continuation is a follow-up to an agent run.
type Continuation struct {
	Prompt string
	// Engine runs the follow-up on another agent; "" keeps the one of the run.
	Engine string
	// WithContext gives the agent the prompt of the run and the end of its output before the follow-up.
	WithContext bool
}

// continuedOutput is how much of the end of a run's output a follow-up carries as context.
const continuedOutput = 6000

// Continue starts a follow-up to the agent run id, in the directory it ran in, and returns the
// execution it created.
func (x *executions) Continue(ctx context.Context, id string, in Continuation) (store.Execution, error) {
	prev, err := x.agentExecution(ctx, id)
	if err != nil {
		return store.Execution{}, err
	}
	follow := strings.TrimSpace(in.Prompt)
	if follow == "" {
		return store.Execution{}, errInvalid("the follow-up is empty")
	}
	if len(follow) > maxPromptBytes {
		return store.Execution{}, errInvalid("the follow-up is longer than %d bytes", maxPromptBytes)
	}
	text := follow
	if in.WithContext {
		out, err := x.store.ReadFullLog(ctx, id)
		if err != nil {
			return store.Execution{}, err
		}
		if len(out) > continuedOutput {
			out = out[len(out)-continuedOutput:]
		}
		text = fmt.Sprintf("You are continuing an earlier agent run in the same working directory.\n\n## Previous request\n\n%s\n\n## Previous output (last part)\n\n%s\n\n## Follow-up\n\n%s\n",
			prev.PromptText, out, follow)
	}
	engine := strings.TrimSpace(in.Engine)
	if engine == "" {
		engine = prev.Agent
	}
	return x.startAgent(ctx, AgentRun{
		Engine: engine, Prompt: text, Change: x.changeOfWorkdir(prev.Workdir),
		parent: id, relation: "continue", summary: follow,
	})
}

// Rerun starts the agent run id again, with the same prompt in the same directory, and returns the
// execution it created. engine runs it on another agent; "" keeps the one of the run.
func (x *executions) Rerun(ctx context.Context, id, engine string) (store.Execution, error) {
	prev, err := x.agentExecution(ctx, id)
	if err != nil {
		return store.Execution{}, err
	}
	if engine = strings.TrimSpace(engine); engine == "" {
		engine = prev.Agent
	}
	return x.StartAgent(ctx, AgentRun{
		Engine: engine, Prompt: prev.PromptText, Change: x.changeOfWorkdir(prev.Workdir),
		parent: id, relation: "rerun", summary: prev.Summary,
	})
}

// stopGrace is how long Stop waits for the work to notice that it was canceled before it answers.
const stopGrace = 500 * time.Millisecond

// Stop stops an execution the portal started and returns it as it is recorded afterwards. It
// cancels the work, which stops the processes it started; it never signals the process the
// execution names as its owner, which is a portal: this one or another.
func (x *executions) Stop(ctx context.Context, id string) (store.Execution, error) {
	ex, err := x.store.GetExecution(ctx, id)
	if err != nil {
		return store.Execution{}, err
	}
	if ex == nil {
		return store.Execution{}, errNotFound("there is no execution %q", id)
	}
	if ex.Kind != "portal_run" && ex.Kind != "portal_job" {
		return store.Execution{}, errConflict("the execution %s was not started from the portal: the dispatcher or the cycle that runs it stops it", id)
	}
	if ex.Status != "running" {
		return store.Execution{}, errConflict("the execution %s is not running: it ended as %s", id, ex.Status)
	}

	x.mu.Lock()
	re := x.running[id]
	x.mu.Unlock()
	if re == nil && ex.PID > 0 && ex.PID != os.Getpid() && store.IsPIDAlive(ex.PID) {
		return store.Execution{}, errConflict("the execution %s was started by another foreman-kit process (pid %d), which is still running: stop it from the portal of that process", id, ex.PID)
	}
	if re != nil {
		re.cancel()
	}
	// Recorded as stopped at once, without waiting for the work to end. Only an execution still
	// running is: one that ended by itself in the meantime keeps how it ended, and nothing recorded is
	// written over. For an execution whose portal died, this record is all there is to do here; the
	// next orchestrator stops the agent that portal left behind.
	if _, err := x.store.FinishRunningExecution(ctx, id, "stopped", stoppedExitCode); err != nil {
		return store.Execution{}, err
	}
	if re != nil {
		select {
		case <-re.done:
		case <-time.After(stopGrace):
		}
	}
	got, err := x.store.GetExecution(ctx, id)
	if err != nil {
		return store.Execution{}, err
	}
	if got == nil {
		return store.Execution{}, errNotFound("there is no execution %q", id)
	}
	if got.Status != "stopped" {
		return store.Execution{}, errConflict("the execution %s ended by itself as %s before it could be stopped", id, got.Status)
	}
	return *got, nil
}

// ---- where an execution runs --------------------------------------------------------------------

// worktreeOf returns the worktree a worker builds change in: under the directory the application
// keeps its worktrees in, as its settings say.
func worktreeOf(root, change string) string {
	return filepath.Join(settings.WorktreesBase(root), "worker-"+change)
}

// workdirOf returns the directory an execution runs in: the root of the workspace, or the worktree
// of change. The directory is checked to be inside the workspace once every symbolic link on the way
// is followed.
func (x *executions) workdirOf(change string) (string, error) {
	change = strings.TrimSpace(change)
	if change == "" {
		return insideWorkspace(x.root, x.root)
	}
	if !cycle.ChangeNameRe.MatchString(change) {
		return "", errInvalid("%q is not the name of a change", change)
	}
	wt := worktreeOf(x.root, change)
	if !isDir(wt) {
		return "", errConflict("the change %s has no worktree: an agent or the gates run in one only while the change is being worked on", change)
	}
	return insideWorkspace(x.root, wt)
}

// isWithinDir reports whether child is parent or inside it.
func isWithinDir(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && filepath.IsLocal(rel)
}

// insideWorkspace returns dir as an absolute path when it is the workspace at root or inside it,
// with every symbolic link of dir and of its ancestors followed: a link that leads out of the
// workspace is refused, wherever on the path it is.
func insideWorkspace(root, dir string) (string, error) {
	outside := errInvalid("the directory %s is not inside the workspace", dir)
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("the root of the workspace cannot be resolved: %w", err)
	}
	rootReal := rootAbs
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootReal = resolved
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return "", outside
	}
	if resolved, err := filepath.EvalSymlinks(dirAbs); err == nil {
		if !isWithinDir(resolved, rootReal) {
			return "", outside
		}
	} else {
		if !isWithinDir(dirAbs, rootAbs) && !isWithinDir(dirAbs, rootReal) {
			return "", outside
		}
		for cur := filepath.Dir(dirAbs); cur != "/" && cur != "."; cur = filepath.Dir(cur) {
			if ancReal, err := filepath.EvalSymlinks(cur); err == nil {
				if !isWithinDir(ancReal, rootReal) {
					return "", outside
				}
				break
			}
			if filepath.Dir(cur) == cur {
				break
			}
		}
	}
	if !isDir(dirAbs) {
		return "", errInvalid("the directory %s does not exist", dir)
	}
	return dirAbs, nil
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	// crypto/rand does not fail on the systems the kit runs on; an id of zeros would still be unique
	// with its timestamp.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
