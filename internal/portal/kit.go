package portal

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/orchestrator"
	"github.com/mayeco/foreman-kit/internal/proc"
	"github.com/mayeco/foreman-kit/internal/store"
)

// Kit is what the portal asks the rest of foreman-kit to carry out: the work that starts a process,
// which a page cannot do by reading or writing rows. A Server is built with one and has no other way
// to run an agent, a kit operation, a ping or a cycle: there is no stand-in that runs nothing and
// reports success. Tests give the Server a double of their own.
type Kit interface {
	// RunAgent runs engine on prompt in workdir and returns its exit code. engine is one agent or
	// several, separated by commas: the kit picks among them the way the application says. With an
	// execution in the scope of ctx, what the agent prints is recorded as the output of that execution while it runs,
	// and output is only what was not recorded that way; with none, output is all of it.
	RunAgent(ctx context.Context, workdir, engine, prompt string) (output string, exitCode int, err error)
	// RunOperation runs a kit operation in dir and returns its exit code, with its output as RunAgent
	// returns the one of an agent. The operations are "dispatch", "merge", "gates" (the gates of every
	// repository as the main checkout has it: the base), "gates-<change>" (the gates of what that
	// change touched, in its worktree), "preflight" and "preflight-services".
	RunOperation(ctx context.Context, dir, operation string) (output string, exitCode int, err error)
	// PingEngine runs the ping command of an engine and says whether it answered.
	PingEngine(ctx context.Context, root, engine string) PingResult
	// StartCycle starts a cycle that outlives the request and returns what starting it printed.
	StartCycle(ctx context.Context, root string) (output string, err error)
	// SimulateDispatch asks the dispatcher what it would start on its next pass and why each change
	// of the backlog waits, without starting anything: its report, and the report as it prints it.
	SimulateDispatch(ctx context.Context, root string) (report *orchestrator.DryRunReport, text string, err error)
	// Recommend evaluates the settings of the application over the stretch called window ("" for
	// the one the kit evaluates by default) from what the kit has on record, and says which ones
	// those records argue for changing, as `foreman-kit config recommend` does. It changes nothing.
	Recommend(ctx context.Context, root, window string) (cycle.Recommendations, error)
}

// PingResult is what a ping of an engine found.
type PingResult struct {
	Engine string `json:"engine"`
	// Status is "ok", "failed", "timeout" or "not_installed".
	Status string `json:"status"`
	// Health is what the ping amounts to as the health of the engine, a value of
	// engines.health_status. A ping that failed is "quota_exceeded" or "auth_failed" when what the
	// engine printed says so, read the way a run of the kit reads it, and "config_error" otherwise.
	Health    string `json:"health"`
	Code      int    `json:"code"`
	LatencyMS int64  `json:"latency_ms"`
	// Output is the last line the engine printed; Detail, why the ping could not run.
	Output string `json:"output,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// kitPingTimeout bounds one ping of an engine started from the portal.
const kitPingTimeout = 90 * time.Second

// NewKit returns the Kit of a real installation. Operations run exe (the foreman-kit executable) as
// a subcommand in the directory of the operation; an agent runs through proc.AgentExec, a ping runs
// the engine's configured ping command (the one preflight uses), and a cycle starts detached.
func NewKit(st *store.Store, exe string) Kit {
	return &execKit{st: st, exe: exe}
}

type execKit struct {
	st  *store.Store
	exe string
}

// executionOutput records what is written to it as the output of an execution, as it is written:
// whoever follows the execution reads it while the work goes on, not when it ends.
type executionOutput struct {
	st     *store.Store
	execID string
	stream string
}

func (w executionOutput) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// The output is recorded whatever became of the request or of the work that writes it.
	if err := w.st.AppendLogChunk(context.Background(), w.execID, w.stream, string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (k *execKit) run(ctx context.Context, dir string, args ...string) (string, int, error) {
	opts := proc.RunOptions{Dir: dir, Env: os.Environ()}
	execID := store.ScopeFrom(ctx).ExecID
	if execID != "" && k.st != nil {
		out := executionOutput{st: k.st, execID: execID, stream: "combined"}
		opts.Stdout, opts.Stderr = out, out
	}
	res, err := proc.RunBounded(ctx, opts, k.exe, args...)
	switch {
	case res == nil:
		return "", 1, err
	case opts.Stdout != nil:
		return "", res.ExitCode, err
	}
	return res.Output, res.ExitCode, err
}

func (k *execKit) RunOperation(ctx context.Context, dir, operation string) (string, int, error) {
	switch {
	case operation == "dispatch", operation == "merge":
		return k.run(ctx, dir, operation)
	case operation == "gates":
		// At the root the main checkout is the base. The gates check what differs from HEAD unless
		// they are told otherwise, and there nothing does: they are told to check every repository.
		return k.run(ctx, dir, "gates", "--all")
	case strings.HasPrefix(operation, "gates-"):
		return k.run(ctx, dir, "gates")
	case operation == "preflight":
		return k.run(ctx, dir, "preflight")
	case operation == "preflight-services":
		return k.run(ctx, dir, "preflight", "--services")
	}
	return "", 2, fmt.Errorf("no kit operation named %q", operation)
}

func (k *execKit) RunAgent(ctx context.Context, workdir, engine, prompt string) (string, int, error) {
	var out bytes.Buffer
	opts := proc.AgentExecOptions{Workdir: workdir, Prompt: prompt, Engines: engine, Store: k.st, Stdout: &out, Stderr: &out}
	if execID := store.ScopeFrom(ctx).ExecID; execID != "" && k.st != nil {
		// Given the execution, AgentExec records what each engine prints as its output while it runs,
		// and records the process group of each engine, so that an agent this process dies without
		// stopping is stopped by the next orchestrator like one of a worker. What AgentExec itself has
		// to say (an engine that could not be started, a retry, a change of engine) goes to the same
		// output, in its place.
		opts.ExecID = execID
		opts.Stdout = io.Discard
		opts.Stderr = executionOutput{st: k.st, execID: execID, stream: "system"}
	}
	res, err := proc.AgentExec(ctx, opts)
	if res == nil {
		return out.String(), 1, err
	}
	return out.String(), res.ExitCode, err
}

func (k *execKit) StartCycle(ctx context.Context, root string) (string, error) {
	var out strings.Builder
	err := cycle.RunCycle(ctx, root, k.st, cycle.CycleOptions{Detach: true}, &out)
	return out.String(), err
}

func (k *execKit) Recommend(ctx context.Context, root, window string) (cycle.Recommendations, error) {
	return cycle.Recommend(ctx, root, k.st, window)
}

func (k *execKit) SimulateDispatch(ctx context.Context, root string) (*orchestrator.DryRunReport, string, error) {
	// A dispatcher of its own, which only looks: the one that builds, if one runs, is another process
	// or another loop, and is not touched.
	disp, err := orchestrator.NewDispatcher(orchestrator.DispatcherOptions{Root: root, Store: k.st, DryRun: true, Out: io.Discard})
	if err != nil {
		return nil, "", err
	}
	// It works on the store of the portal, which is not its own to close: its Close has nothing to
	// fail at.
	defer func() { _ = disp.Close() }()
	rep, err := disp.DryRun(ctx)
	if err != nil {
		return nil, "", err
	}
	return rep, rep.FormatText(orchestrator.LoadPipelineSettings(root).Workers, 0), nil
}

func (k *execKit) PingEngine(ctx context.Context, root, engine string) PingResult {
	bin := config.ExecutableBinary(root, engine)
	if _, err := exec.LookPath(bin); err != nil {
		return PingResult{Engine: engine, Status: "not_installed", Health: "not_installed", Detail: fmt.Sprintf("%s not found on PATH", bin)}
	}
	binName, binArgs, closeStdin := proc.ResolvePingArgs(root, engine, config.ExtraArgsWithStderr(root, engine, io.Discard))
	var stdin io.Reader
	if closeStdin {
		stdin = bytes.NewReader(nil)
	}
	started := time.Now()
	res, err := proc.RunBounded(ctx, proc.RunOptions{
		Dir:         root,
		Env:         proc.BuildSlotEnv(os.Environ(), root, 0),
		Stdin:       stdin,
		Timeout:     kitPingTimeout,
		GracePeriod: 10 * time.Second,
	}, binName, binArgs...)
	out := PingResult{Engine: engine, LatencyMS: time.Since(started).Milliseconds()}
	if res == nil {
		out.Status, out.Health, out.Code = "failed", "config_error", 1
		if err != nil {
			out.Detail = err.Error()
		}
		return out
	}
	out.Code = res.ExitCode
	out.Output = firstNonEmptyLine(lastLines(res.Output, 1), "")
	if err != nil {
		// The command could not be started (a script whose interpreter is missing, a file of another
		// architecture): there is no output, and without the reason the ping only says "failed".
		out.Detail = err.Error()
	}
	switch {
	case res.TimedOut:
		out.Status, out.Health = "timeout", "timeout"
	case res.ExitCode == 0:
		out.Status, out.Health = "ok", "ok"
	case proc.OutOfQuota(res.Output):
		out.Status, out.Health = "failed", "quota_exceeded"
	case proc.EngineConfigFailure(res.Output):
		out.Status, out.Health = "failed", "auth_failed"
	default:
		out.Status, out.Health = "failed", "config_error"
	}
	return out
}

// Problem says why a ping did not answer, "" for one that did: why it could not run, or how it
// ended and the last line the engine printed.
func (p PingResult) Problem() string {
	switch {
	case p.Status == "ok":
		return ""
	case p.Detail != "":
		return p.Detail
	case p.Status == "timeout":
		return fmt.Sprintf("no answer in %s", kitPingTimeout)
	case p.Output != "":
		return fmt.Sprintf("exit %d: %s", p.Code, p.Output)
	}
	return fmt.Sprintf("exit %d", p.Code)
}

// Summary says in a sentence how the ping went.
func (p PingResult) Summary() string {
	if p.Status == "ok" {
		return fmt.Sprintf("%s answered in %s", p.Engine, millisText(p.LatencyMS))
	}
	return fmt.Sprintf("%s did not answer: %s", p.Engine, p.Problem())
}

// lastLines returns the last n non-empty lines of s.
func lastLines(s string, n int) string {
	var kept []string
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			kept = append([]string{line}, kept...)
		}
	}
	return strings.Join(kept, "\n")
}
