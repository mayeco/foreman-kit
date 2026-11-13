package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

const (
	// ExitEnginesDown (75) indicates every engine failed without changing the worktree.
	ExitEnginesDown = 75
	// ExitTimeout (124) indicates an engine call timed out or stalled.
	ExitTimeout = 124
	// ExitNotInstalled (127) indicates an engine binary is not installed or unknown.
	ExitNotInstalled = 127
	// ExitConfig (78) indicates the call did not start: .foreman-kit/agents.yaml cannot be used.
	ExitConfig = 78
)

// QUOTA_PATTERN, ENGINE_CONFIG_PATTERN, and CONTEXT_OVERFLOW_PATTERN match usage limits, auth errors, and context window overflows.
const (
	QUOTA_PATTERN            = `hit your [a-z ]*limit|usage limit|session limit|weekly limit|out of credits|credit balance is too low|insufficient_quota|quota exceeded|exceeded your current quota|reached your [a-z ]*(quota|limit)|individual quota reached|insufficient (balance|credits)|usage_not_included`
	ENGINE_CONFIG_PATTERN    = `authentication (failed|timed out)|not authenticated|not logged in|no providers configured|no authentication information found|unauthenticated|401 unauthorized|missing bearer or basic authentication`
	CONTEXT_OVERFLOW_PATTERN = `prompt is too long|input is too long for requested model|exceeds the context window|input token count.*exceeds the maximum|maximum prompt length is \d+|reduce the length of the messages|maximum context length is \d+ tokens|exceeds the limit of \d+|exceeds the available context size|greater than the context length|context window exceeds limit|exceeded model token limit|context[_ ]length[_ ]exceeded`
)

var (
	QuotaPatternRe           = regexp.MustCompile(`(?i)(` + QUOTA_PATTERN + `)`)
	EngineConfigPatternRe    = regexp.MustCompile(`(?i)(` + ENGINE_CONFIG_PATTERN + `)`)
	ContextOverflowPatternRe = regexp.MustCompile(`(?i)(` + CONTEXT_OVERFLOW_PATTERN + `)`)
	waitSecondsBodyRe        = regexp.MustCompile(`(?i)(?:wait|retry after)\s+(\d+)\s+seconds?`)
	agentExecClaimCounter    int64
)

// AgentExecOptions configures an AgentExec invocation.
type AgentExecOptions struct {
	Workdir        string
	AppRoot        string
	Prompt         string
	PromptFile     string
	Engines        string // comma-separated engine list
	Role           string // optional role ("plan", "build", "cycle")
	Change         string // optional change name for agent.exit event
	Phase          string // optional phase ("plan", "plan-repair-1", "apply", "repair-1", "cycle")
	Slot           int
	TimeoutSeconds int
	Env            []string
	Store          *store.Store
	ExecID         string
	SkipEvent      bool
	WatchdogPoll   time.Duration
	GracePeriod    time.Duration
	Stdout         io.Writer
	Stderr         io.Writer
}

// AgentExecResult holds the result of an AgentExec call.
type AgentExecResult struct {
	ExitCode   int
	UsedEngine string
	Output     string
}

// OutOfQuota reports whether the last 40 lines of output match QUOTA_PATTERN.
func OutOfQuota(output string) bool {
	return QuotaPatternRe.MatchString(tailLines(output, 40))
}

// EngineConfigFailure reports whether the last 40 lines of output match ENGINE_CONFIG_PATTERN.
func EngineConfigFailure(output string) bool {
	return EngineConfigPatternRe.MatchString(tailLines(output, 40))
}

// IsContextOverflow reports whether the last 40 lines of output indicate a non-retryable context window overflow (Pattern 2 from zendev-sh/goai).
func IsContextOverflow(output string) bool {
	return ContextOverflowPatternRe.MatchString(tailLines(output, 40))
}

// ParseRetryFromBody extracts an explicit "wait N seconds" / "retry after N seconds" duration from error output (Pattern 1 from zendev-sh/goai).
func ParseRetryFromBody(output string) time.Duration {
	m := waitSecondsBodyRe.FindStringSubmatch(tailLines(output, 40))
	if len(m) == 2 {
		if sec, err := strconv.Atoi(m[1]); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	return 0
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ChangesLeft returns a summary (up to 120 lines) of uncommitted and untracked changes in dir and its child repos using gitx.
func ChangesLeft(ctx context.Context, dir string) string {
	if ctx == nil {
		ctx = context.Background()
	}
	var lines []string
	appendLines := func(prefix, text string) {
		for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			if strings.TrimSpace(l) == "" {
				continue
			}
			if prefix != "" {
				lines = append(lines, prefix+": "+l)
			} else {
				lines = append(lines, l)
			}
		}
	}

	var childPaths []string
	if repo, err := gitx.Open(dir); err == nil {
		childPaths, _ = repo.ChildPaths()
	}

	if st, err := gitx.RunGit(ctx, dir, "status", "--short", "--untracked-files=all"); err == nil {
		appendLines("", st)
	}
	for _, rel := range childPaths {
		subDir := filepath.Join(dir, filepath.FromSlash(rel))
		if st, err := gitx.RunGit(ctx, subDir, "status", "--short", "--untracked-files=all"); err == nil {
			appendLines(rel, st)
		}
	}
	if diffStat, err := gitx.RunGit(ctx, dir, "diff", "--stat", "HEAD"); err == nil {
		appendLines("", diffStat)
	}
	for _, rel := range childPaths {
		subDir := filepath.Join(dir, filepath.FromSlash(rel))
		if diffStat, err := gitx.RunGit(ctx, subDir, "diff", "--stat", "HEAD"); err == nil {
			appendLines(rel, diffStat)
		}
	}

	if len(lines) > 120 {
		lines = lines[:120]
	}
	return strings.Join(lines, "\n")
}

// ContinuationNote builds the prompt section appended when a previous engine attempt stopped after modifying the worktree.
func ContinuationNote(ctx context.Context, dir, whoStopped string) string {
	changes := ChangesLeft(ctx, dir)
	return fmt.Sprintf("\n## A previous attempt stopped before finishing\n\n%s worked on this same task and stopped. The work directory keeps what it changed:\n\n```\n%s\n```\n\nContinue from this state instead of starting over: read the changes (git diff, and inside each\nrepository under repos/), keep what is right, fix what is wrong and finish the rest.",
		whoStopped, changes)
}

// ExpandCommand returns the executable and the arguments of one command of an agent, exactly as
// .foreman-kit/agents.yaml writes it: the command is split as a shell would split it and its placeholders
// are replaced, nothing is added. {{prompt}} is the prompt as one argument, {{prompt_file}} the path
// of a file that holds it, {{workdir}} the directory the agent works in and {{timeout}} the limit of
// the call in seconds. {{args}}, as a word of its own, becomes the arguments the role gives the
// agent; a command without it gets them after its last word.
func ExpandCommand(spec config.AgentCommandSpec, command any, engine, prompt, promptFile, workdir string, timeoutSeconds int, args []string) (name string, argv []string, closeStdin bool, err error) {
	words, err := config.CommandWords(command)
	if err != nil {
		return "", nil, false, fmt.Errorf("commands.%s: %w", engine, err)
	}
	if len(words) == 0 || words[0] == "" {
		return "", nil, false, fmt.Errorf("commands.%s: empty command", engine)
	}
	replacer := strings.NewReplacer(
		"{{prompt}}", prompt,
		"{{prompt_file}}", promptFile,
		"{{workdir}}", workdir,
		"{{timeout}}", strconv.Itoa(timeoutSeconds),
		"{{args}}", strings.Join(args, " "),
	)
	placed := false
	argv = make([]string, 0, len(words)+len(args))
	for _, word := range words[1:] {
		if word == "{{args}}" {
			argv = append(argv, args...)
			placed = true
			continue
		}
		if strings.Contains(word, "{{args}}") {
			placed = true
		}
		argv = append(argv, replacer.Replace(word))
	}
	if !placed {
		argv = append(argv, args...)
	}
	closeStdin = false
	if spec.StdinNull != nil {
		closeStdin = *spec.StdinNull
	} else if meta, ok := config.SUPPORTED[engine]; ok {
		closeStdin = meta.StdinNull
	}
	return words[0], argv, closeStdin, nil
}

// ResolveEngineCommand returns the executable and the arguments of one call of engine: its run
// command, or its restricted command while permissions is restricted, as commands.<engine> of
// .foreman-kit/agents.yaml writes it. An agent without a restricted command cannot be used in that mode.
// An agent the file gives no commands runs the ones the kit ships for it, when it knows the agent;
// "stub" runs the script AGENT_STUB names, for tests.
func ResolveEngineCommand(appRoot, engine, prompt, promptFile, workdir, permissions string, timeoutSeconds int, args []string) (name string, argv []string, closeStdin bool, err error) {
	if engine == "stub" {
		stubScript := os.Getenv("AGENT_STUB")
		if stubScript == "" {
			return "", nil, false, fmt.Errorf("AGENT_STUB must point to a stub script")
		}
		return "bash", append([]string{stubScript, promptFile}, args...), false, nil
	}
	spec, _, known := config.CommandsFor(appRoot, engine)
	if !known {
		return "", nil, false, fmt.Errorf("%s has no commands (%s.%s in %s)", engine, settings.AgentCommands.Path(), engine, settings.AgentCommands.FilePath())
	}
	command := spec.Run
	if permissions == settings.PermissionsRestricted {
		if spec.Restricted == nil {
			return "", nil, false, fmt.Errorf("%s has no restricted command (%s.%s.restricted in %s): it cannot be used while %s is %s",
				engine, settings.AgentCommands.Path(), engine, settings.AgentCommands.FilePath(), settings.AgentPermissions.Path(), settings.PermissionsRestricted)
		}
		command = spec.Restricted
	}
	if command == nil {
		return "", nil, false, fmt.Errorf("%s has no run command (%s.%s.run in %s)", engine, settings.AgentCommands.Path(), engine, settings.AgentCommands.FilePath())
	}
	return ExpandCommand(spec, command, engine, prompt, promptFile, workdir, timeoutSeconds, args)
}

// roleFromPhase maps a phase ("plan", "plan-repair-1", "apply", "repair-1", "build", "cycle")
// to its role, or "" when it has none.
func roleFromPhase(phase string) string {
	switch {
	case strings.HasPrefix(phase, "plan"):
		return "plan"
	case strings.HasPrefix(phase, "apply"), strings.HasPrefix(phase, "repair"), strings.HasPrefix(phase, "build"):
		return "build"
	case strings.HasPrefix(phase, "cycle"):
		return "cycle"
	}
	return ""
}

// AgentExec executes an AI agent call across the configured role/fallback engines with bounded timeouts,
// idle watchdog, retries, quota/auth detection, continuation notes, and store event emission.
func AgentExec(ctx context.Context, opts AgentExecOptions) (*AgentExecResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	workdir := opts.Workdir
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	absWorkdir, err := filepath.Abs(workdir)
	if err == nil {
		workdir = absWorkdir
	} else {
		absWorkdir = workdir
	}

	appRoot := agentExecAppRoot(opts.AppRoot, workdir)

	basePrompt := opts.Prompt
	if basePrompt == "" && opts.PromptFile != "" {
		b, err := os.ReadFile(opts.PromptFile)
		if err != nil {
			return &AgentExecResult{ExitCode: 1}, err
		}
		basePrompt = string(b)
	}

	// The role says which limit and which arguments the call gets: the caller's, or the one of the
	// phase the call runs in.
	argsRole := opts.Role
	if argsRole == "" {
		phase := opts.Phase
		if phase == "" {
			phase = os.Getenv("FOREMANKIT_PHASE")
		}
		argsRole = roleFromPhase(phase)
	}

	limits := settings.At(appRoot)
	timeoutSec := int(config.RoleTimeout(appRoot, argsRole) / time.Second)
	if opts.TimeoutSeconds > 0 {
		timeoutSec = opts.TimeoutSeconds
	}
	idleSec := limits.Seconds(settings.IdleTimeout)
	idleCPUPercent := limits.Int(settings.IdleCPUPercent)
	retries := limits.Int(settings.AgentRetries)
	retryDelaySec := limits.Seconds(settings.AgentRetryAfter)
	permissions := config.PermissionsTo(appRoot, stderr)
	selection := config.SelectionTo(appRoot, stderr)

	// The agents of the call: the ones it names, else the ones of its role. A call that names
	// neither, and runs in no phase, has no agent: nothing is picked in its place.
	engines := strings.TrimSpace(opts.Engines)
	if engines == "" && argsRole != "" {
		engines, _ = config.Engine(appRoot, argsRole)
	}
	if engines == "" {
		why := "name the agent of the call (foreman-kit agent-exec <workdir> <prompt-file> <agent>)"
		if argsRole != "" {
			// A role without agents does not run.
			why = settings.RoleAgents(argsRole).Where() + " lists none"
		}
		fmt.Fprintf(stderr, "agent_exec: no agent to run: %s\n", why)
		return &AgentExecResult{ExitCode: ExitNotInstalled}, fmt.Errorf("no agent to run: %s", why)
	}

	st := opts.Store
	if st == nil {
		if opened, err := store.Open(appRoot); err == nil {
			st = opened
			defer st.Close()
		}
	}
	// What this call writes (the engine claim, the attempts, the exit event) belongs to an execution:
	// the one the caller recorded for it, or the one the caller itself runs under (a cycle's agent).
	attemptExec := opts.ExecID
	if attemptExec != "" {
		ctx = store.WithExecution(ctx, attemptExec)
	} else {
		attemptExec = store.ScopeFrom(ctx).ExecID
	}

	claimID := store.AgentClaimID(atomic.AddInt64(&agentExecClaimCounter, 1), os.Getpid())
	arranged, err := config.Arrange(ctx, appRoot, st, engines, claimID)
	if err != nil || len(arranged) == 0 {
		arranged = config.Names(engines)
	}
	defer func() {
		_ = config.Release(context.Background(), appRoot, st, claimID)
	}()
	if len(arranged) == 0 {
		// A list made only of separators or blanks (",") names no agent.
		fmt.Fprintf(stderr, "agent_exec: no agent named in %q\n", engines)
		return &AgentExecResult{ExitCode: ExitNotInstalled}, fmt.Errorf("no agent named in %q", engines)
	}

	if selection != settings.SelectionInOrder && len(arranged) >= 2 {
		fmt.Fprintf(stderr, "agent_exec: selection %s: %s\n", selection, strings.Join(arranged, ","))
	}

	tmpPromptFile, err := os.CreateTemp("", "agent-prompt.*")
	if err != nil {
		return &AgentExecResult{ExitCode: 1}, err
	}
	runPromptPath := tmpPromptFile.Name()
	_ = tmpPromptFile.Close()
	defer os.Remove(runPromptPath)

	// An agent searches the memory with agent-memory, whose search index is this checkout's own and
	// does not follow what git brought since it was built (a rebase, a worktree kept from the last
	// session). Refreshed here, before the state of the tree is taken: it changes no tracked file.
	if err := memory.Refresh(ctx, workdir); err != nil {
		fmt.Fprintf(stderr, "agent_exec: could not refresh the search index of the memory: %v\n", err)
	}

	started := time.Now()
	startState := TreeState(ctx, workdir)
	currentPrompt := basePrompt
	rc := ExitNotInstalled
	ran := false
	usedEngine := arranged[0]
	var combinedOut bytes.Buffer

	for i, engine := range arranged {
		engine = strings.TrimSpace(engine)
		if engine == "" {
			continue
		}
		if i > 0 {
			_ = config.Claim(ctx, appRoot, st, engine, claimID)
		}

		tryNum := 0
		for {
			_ = os.WriteFile(runPromptPath, []byte(currentPrompt), 0o600)
			rc = 0
			var attemptBuf bytes.Buffer
			var idleStalled bool
			var idleSeconds int
			var startErr error

			// Each try of each engine is an attempt of the execution, with its own outcome: which engine
			// was out of quota, which one stalled and which one did the work is then a row, not a line
			// of the output.
			var attemptID int64
			if st != nil && attemptExec != "" {
				attemptID, _ = st.BeginAttempt(context.WithoutCancel(ctx), attemptExec, engine, tryNum)
			}
			attemptCtx := store.WithAttempt(ctx, attemptID)

			if engine != "stub" {
				binToLookup := config.ExecutableBinary(appRoot, engine)
				if _, err := exec.LookPath(binToLookup); err != nil {
					if _, ok := config.SUPPORTED[engine]; !ok && binToLookup == engine {
						fmt.Fprintf(attemptBufWriter(&attemptBuf, stderr), "agent_exec: unknown agent %s: %s gives it no commands and it is not on PATH\n", engine, settings.AgentCommands.FilePath())
					} else {
						fmt.Fprintf(attemptBufWriter(&attemptBuf, stderr), "agent_exec: %s is not installed\n", engine)
					}
					rc = ExitNotInstalled
				}
			}

			// A call of a detached agent the kit stopped here may still be working: no call starts in
			// this directory until its processes end.
			if rc == 0 {
				if left := waitForStoppedCall(ctx, workdir, opts.GracePeriod, opts.WatchdogPoll); len(left) > 0 {
					msg := workdirBusyMessage(workdir, left)
					_, _ = io.WriteString(attemptBufWriter(&attemptBuf, stderr), msg)
					combinedOut.WriteString(msg)
					rc = ExitWorkdirBusy
				}
			}

			var detachedSpec config.AgentCommandSpec
			if engine != "stub" {
				detachedSpec, _, _ = config.CommandsFor(appRoot, engine)
			}
			runPID := 0

			if rc == 0 {
				extra := config.ExtraArgsForRole(appRoot, engine, argsRole, stderr)
				if len(extra) > 0 {
					label := engine
					if argsRole != "" {
						label = engine + " (" + argsRole + ")"
					}
					fmt.Fprintf(attemptBufWriter(&attemptBuf, stderr), "agent_exec: %s args: %s\n", label, strings.Join(extra, " "))
				}
				binName, binArgs, closeStdin, buildErr := ResolveEngineCommand(
					appRoot, engine, currentPrompt, runPromptPath, workdir, permissions, timeoutSec, extra,
				)
				if buildErr != nil {
					fmt.Fprintf(attemptBufWriter(&attemptBuf, stderr), "agent_exec: %v\n", buildErr)
					rc = ExitNotInstalled
				} else {
					var stdinReader io.Reader
					if closeStdin {
						stdinReader = bytes.NewReader(nil)
					}
					baseEnv := opts.Env
					if len(baseEnv) == 0 {
						baseEnv = os.Environ()
					}
					envMap := make(map[string]string, len(baseEnv)+8)
					var envOrder []string
					setEnv := func(k, v string) {
						if _, exists := envMap[k]; !exists {
							envOrder = append(envOrder, k)
						}
						envMap[k] = v
					}
					for _, kv := range baseEnv {
						if idx := strings.IndexByte(kv, '='); idx > 0 {
							setEnv(kv[:idx], kv[idx+1:])
						}
					}
					setEnv("FOREMANKIT_PROMPT_FILE", runPromptPath)
					setEnv("FOREMANKIT_WORKDIR", absWorkdir)
					setEnv("FOREMANKIT_APP_ROOT", absWorkdir)
					setEnv("FOREMANKIT_MAIN_ROOT", store.MainRoot(absWorkdir))
					setEnv("FOREMANKIT_CHANGE", opts.Change)
					setEnv("FOREMANKIT_PHASE", opts.Phase)
					setEnv("FOREMANKIT_ROLE", opts.Role)
					setEnv("FOREMANKIT_ENGINE", engine)
					if opts.Slot > 0 {
						setEnv("FOREMANKIT_SLOT", strconv.Itoa(opts.Slot))
					}
					runEnv := make([]string, 0, len(envOrder))
					for _, k := range envOrder {
						runEnv = append(runEnv, k+"="+envMap[k])
					}
					// One writer for both streams: os/exec then shares a single pipe and copy goroutine,
					// so stdout, attemptBuf and combinedOut never receive concurrent writes and the
					// engine's stdout/stderr interleaving is preserved. The buffers come first:
					// io.MultiWriter stops at the first writer that fails, and a stdout whose reader
					// went away must not cut the output the quota and failure checks below read.
					attemptOut := io.MultiWriter(&attemptBuf, &combinedOut, stdout)
					idleLimit := time.Duration(idleSec) * time.Second
					idle := &idleReport{
						st:        st,
						ctx:       context.WithoutCancel(attemptCtx),
						attemptID: attemptID,
						limit:     idleLimit,
						after:     idleNoticeAfter(idleLimit, opts.WatchdogPoll),
					}
					runRes, runErr := RunBounded(attemptCtx, RunOptions{
						Dir:            workdir,
						Env:            runEnv,
						Stdin:          stdinReader,
						Stdout:         attemptOut,
						Stderr:         attemptOut,
						Timeout:        time.Duration(timeoutSec) * time.Second,
						GracePeriod:    opts.GracePeriod,
						IdleTimeout:    idleLimit,
						IdleCPUPercent: float64(idleCPUPercent),
						OnIdle:         idle.observe,
						WatchdogPoll:   opts.WatchdogPoll,
						Store:          st,
						ExecID:         opts.ExecID,
						ActivityPaths:  activityPathsOf(detachedSpec),
					}, binName, binArgs...)
					if runErr != nil {
						// The engine never ran (its file cannot be executed, the prompt is too large an
						// argument for execve): say why, or the lines below report an exit code, a retry
						// and a fallback with no cause.
						msg := fmt.Sprintf("agent_exec: %s could not be started: %v\n", engine, runErr)
						_, _ = io.WriteString(stderr, msg)
						combinedOut.WriteString(msg)
						startErr = runErr
					}
					if runRes != nil {
						rc = runRes.ExitCode
						idleStalled = runRes.IdleStalled
						idleSeconds = runRes.IdleSeconds
						runPID = runRes.PID
					} else {
						rc = 1
					}
				}
			}

			if idleStalled {
				msg := fmt.Sprintf("agent_exec: %s stalled: no work of its processes (CPU), no change in the work directory and no output for %ds; stopped it\n", engine, idleSeconds)
				_, _ = io.WriteString(stderr, msg)
				combinedOut.WriteString(msg)
				if rc == 0 {
					rc = ExitTimeout
				}
			}

			// Killing the process of a detached agent does not stop its work, which goes on in a service
			// outside it: its stop command does, and no other call starts here until it has.
			if detachedSpec.Detached && runPID > 0 && rc != 0 && stoppedByKit(attemptCtx, rc, idleStalled) {
				stopDetachedCall(attemptCtx, detachedSpec, engine, workdir, runPID, attemptBufWriter(&attemptBuf, io.MultiWriter(stderr, &combinedOut)))
			}

			exitLine := fmt.Sprintf("agent_exec: engine %s exited %d\n", engine, rc)
			_, _ = io.WriteString(stderr, exitLine)
			combinedOut.WriteString(exitLine)
			usedEngine = engine
			if attemptID != 0 {
				_ = st.FinishAttempt(context.WithoutCancel(ctx), attemptID, rc, attemptOutcome(ctx, rc, idleStalled, startErr, attemptBuf.String()), idleSeconds)
			}

			if rc == 0 {
				if st != nil {
					_ = st.RecordEngineHealth(ctx, store.EngineHealthRecord{
						Engine:    engine,
						Installed: true,
						Status:    "ok",
					})
				}
				break
			}
			if rc == ExitWorkdirBusy {
				break
			}
			if rc == ExitNotInstalled {
				if st != nil {
					_ = st.RecordEngineHealth(ctx, store.EngineHealthRecord{
						Engine:        engine,
						Installed:     false,
						Status:        "not_installed",
						LastError:     "not installed",
						CooldownUntil: time.Now().UTC().Add(60 * time.Minute).Unix(),
					})
				}
				break
			}
			ran = true

			if TreeState(ctx, workdir) != startState {
				reason := fmt.Sprintf("exit %d", rc)
				if rc == ExitTimeout {
					reason = "it hit the time limit"
				}
				if idleStalled {
					reason = fmt.Sprintf("it stalled, doing nothing for %ds", idleSeconds)
				}
				currentPrompt = basePrompt + "\n" + ContinuationNote(ctx, workdir, fmt.Sprintf("%s (%s)", engine, reason))
			}

			attemptText := attemptBuf.String()
			if OutOfQuota(attemptText) {
				if st != nil {
					_ = st.RecordEngineHealth(ctx, store.EngineHealthRecord{
						Engine:        engine,
						Installed:     true,
						Status:        "quota_exceeded",
						LastError:     "out of quota",
						CooldownUntil: time.Now().UTC().Add(30 * time.Minute).Unix(),
					})
				}
				msg := fmt.Sprintf("agent_exec: %s is out of quota or over a usage limit; not trying it again\n", engine)
				_, _ = io.WriteString(stderr, msg)
				combinedOut.WriteString(msg)
				break
			}
			if EngineConfigFailure(attemptText) {
				if st != nil {
					_ = st.RecordEngineHealth(ctx, store.EngineHealthRecord{
						Engine:        engine,
						Installed:     true,
						Status:        "auth_failed",
						LastError:     "auth or provider configuration failure",
						CooldownUntil: time.Now().UTC().Add(60 * time.Minute).Unix(),
					})
				}
				msg := fmt.Sprintf("agent_exec: %s has an authentication or provider configuration failure; not trying it again\n", engine)
				_, _ = io.WriteString(stderr, msg)
				combinedOut.WriteString(msg)
				break
			}
			if IsContextOverflow(attemptText) {
				// Pattern 2 (zendev-sh/goai): Context window overflow is non-retryable on the same engine
				msg := fmt.Sprintf("agent_exec: %s exceeded context window limit; not trying it again\n", engine)
				_, _ = io.WriteString(stderr, msg)
				combinedOut.WriteString(msg)
				break
			}
			if errors.Is(startErr, syscall.E2BIG) {
				// execve refused the command line, of which the prompt is one argument: the same
				// command cannot start after the retry delay either.
				msg := fmt.Sprintf("agent_exec: the command line of %s, with the prompt as an argument, is too large to start; not trying it again\n", engine)
				_, _ = io.WriteString(stderr, msg)
				combinedOut.WriteString(msg)
				break
			}
			if idleStalled {
				// The engine sat through the whole idle limit without doing anything. What froze it (a
				// usage limit it waits out instead of failing, a connection that never answers, a prompt
				// it cannot get past) is rarely gone a moment later, and a second try that froze the same
				// way held the worker for the idle limit once more: twice the limit lost before the next
				// engine, or the dispatcher's own backoff, had its turn.
				msg := fmt.Sprintf("agent_exec: %s stalled; not trying it again in this call: what stalled it is likely still there\n", engine)
				_, _ = io.WriteString(stderr, msg)
				combinedOut.WriteString(msg)
				break
			}

			tryNum++
			if tryNum > retries {
				break
			}
			// Pattern 1 (zendev-sh/goai): Respect short Retry-After from body (<60s) with jitter, or fail fast if >=60s
			delayDur := time.Duration(retryDelaySec) * time.Second
			if bodyWait := ParseRetryFromBody(attemptText); bodyWait > 0 {
				if bodyWait >= 60*time.Second {
					break
				}
				jitter := time.Duration(rand.IntN(1000)) * time.Millisecond
				delayDur = bodyWait + jitter
			}
			retryMsg := fmt.Sprintf("agent_exec: trying %s again (%d of %d) in %ds\n", engine, tryNum, retries, int(delayDur.Seconds()))
			_, _ = io.WriteString(stderr, retryMsg)
			combinedOut.WriteString(retryMsg)

			if delayDur > 0 {
				if err := sleepWithContext(ctx, delayDur); err != nil {
					rc = 143
					break
				}
			}
		}

		if rc == 0 || rc == ExitWorkdirBusy || ctx.Err() != nil {
			break
		}
		if i+1 < len(arranged) {
			next := strings.TrimSpace(arranged[i+1])
			if next != "" {
				fbMsg := fmt.Sprintf("agent_exec: %s failed (exit %d); trying %s\n", engine, rc, next)
				_, _ = io.WriteString(stderr, fbMsg)
				combinedOut.WriteString(fbMsg)
			}
		}
	}

	// Not after a cancellation: the engine was stopped, which says nothing about whether it could work,
	// and under an ended context git cannot be run, so TreeState reads every tree as an untouched one.
	if rc != 0 && ran && ctx.Err() == nil && TreeState(ctx, workdir) == startState {
		downMsg := "agent_exec: no engine could work (each failed without changing anything); exit 75: try again later\n"
		_, _ = io.WriteString(stderr, downMsg)
		combinedOut.WriteString(downMsg)
		rc = ExitEnginesDown
	}

	if st != nil && !opts.SkipEvent {
		change := opts.Change
		if change == "" {
			change = os.Getenv("FOREMANKIT_CHANGE")
		}
		phase := opts.Phase
		if phase == "" {
			phase = os.Getenv("FOREMANKIT_PHASE")
		}
		role := opts.Role
		if role == "" {
			role = roleFromPhase(phase)
		}
		payload := map[string]any{
			"code":           rc,
			"seconds":        int(math.Round(time.Since(started).Seconds())),
			"engine":         usedEngine,
			"engines":        engines,
			"memory_version": memory.Fingerprint(workdir),
		}
		if role != "" {
			if pv, err := config.Version(appRoot, role); err == nil {
				payload["prompt_version"] = pv
			}
		}
		// Under the scope of the caller, without its cancellation: the event names the execution it
		// belongs to (the cycle that called the agent) also when the call was stopped.
		_ = st.EmitEvent(context.WithoutCancel(ctx), store.EventInput{
			Event:   "agent.exit",
			Change:  change,
			Engine:  usedEngine,
			Phase:   phase,
			Payload: payload,
		})
	}

	return &AgentExecResult{
		ExitCode:   rc,
		UsedEngine: usedEngine,
		Output:     combinedOut.String(),
	}, nil
}

// idleReport records in the store when an attempt goes idle and when it works again, from what the
// idle watchdog reports after each of its polls: that is what `foreman-kit status` and the portal
// show while it happens. A stalled agent used to be visible only once it had been stopped.
type idleReport struct {
	st        *store.Store
	ctx       context.Context // the scope of the attempt, without its cancellation
	attemptID int64
	limit     time.Duration // idle time that gets the attempt stopped
	after     time.Duration // idle time worth telling
	open      bool          // the attempt has an open idle period
}

// observe is RunOptions.OnIdle: it runs on the watchdog's goroutine and only writes to the store.
func (r *idleReport) observe(idle time.Duration) {
	if r.st == nil || r.attemptID <= 0 || r.limit <= 0 {
		return
	}
	switch {
	case !r.open && idle >= r.after:
		if r.st.BeginIdlePeriod(r.ctx, r.attemptID, time.Now().Add(-idle), r.limit) == nil {
			r.open = true
		}
	case r.open && idle == 0:
		if r.st.EndIdlePeriod(r.ctx, r.attemptID) == nil {
			r.open = false
		}
	}
}

// idleNoticeAfter returns how long an agent has to show no activity before that is told: a quarter
// of the limit that gets it stopped, so that the minutes an agent spends reading and thinking before
// it writes anything stay out of it, and at least two polls of the watchdog, whose first one has
// nothing to compare with.
func idleNoticeAfter(limit, poll time.Duration) time.Duration {
	after := limit / 4
	if twoPolls := 2 * watchdogPoll(limit, poll); after < twoPolls {
		after = twoPolls
	}
	return after
}

// attemptOutcome names how one try of an engine ended, from what AgentExec decides on it: a reason
// not to try the engine again comes before how the process ended.
func attemptOutcome(ctx context.Context, rc int, idleStalled bool, startErr error, output string) string {
	switch {
	case rc == 0:
		return store.AttemptOK
	case rc == ExitNotInstalled:
		return store.AttemptNotInstalled
	case ctx.Err() != nil:
		return store.AttemptCancelled
	case OutOfQuota(output):
		return store.AttemptQuotaExceeded
	case EngineConfigFailure(output):
		return store.AttemptAuthFailed
	case IsContextOverflow(output):
		return store.AttemptContextOverflow
	case errors.Is(startErr, syscall.E2BIG):
		return store.AttemptTooLarge
	case idleStalled:
		return store.AttemptIdleStalled
	case rc == ExitTimeout:
		return store.AttemptTimeout
	}
	return store.AttemptError
}

// agentExecAppRoot returns the application a call in workdir belongs to: the one given, else the one
// workdir is in, else the one the process runs in.
func agentExecAppRoot(given, workdir string) string {
	if given != "" {
		return given
	}
	if abs, err := filepath.Abs(workdir); err == nil {
		workdir = abs
	}
	appRoot := config.AppRoot(workdir)
	if !fileExists(filepath.Join(appRoot, ".foreman-kit", "agents.yaml")) && !fileExists(filepath.Join(appRoot, ".foreman-kit", "pipeline.yaml")) {
		appRoot = config.AppRoot("")
	}
	return appRoot
}

// RunAgentExecCLI implements `foreman-kit agent-exec <workdir> <prompt-file> [engine[,engine...]]`.
func RunAgentExecCLI(ctx context.Context, st *store.Store, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if len(args) < 2 || len(args) > 3 {
		fmt.Fprintln(stderr, "usage: foreman-kit agent-exec <workdir> <prompt-file> [agent[,agent...]]")
		return 2
	}
	engines := ""
	if len(args) == 3 {
		engines = args[2]
	}
	// A call started by hand is new work: it does not run on a .foreman-kit/agents.yaml that lacks a key
	// or holds one it cannot use. The calls of a change already being built are not stopped here:
	// the dispatcher and the cycle check before they start one.
	if why := settings.Unusable(agentExecAppRoot("", args[0]), settings.AgentCallFiles...); why != "" {
		fmt.Fprintf(stderr, "agent_exec: %s: no agent runs until it is mended\n", why)
		return ExitConfig
	}
	res, err := AgentExec(ctx, AgentExecOptions{
		Workdir:    args[0],
		PromptFile: args[1],
		Engines:    engines,
		Store:      st,
		Stdout:     stdout,
		Stderr:     stderr,
	})
	if res != nil {
		return res.ExitCode
	}
	if err != nil {
		return 1
	}
	return 0
}

func attemptBufWriter(buf *bytes.Buffer, w io.Writer) io.Writer {
	if w == nil {
		return buf
	}
	return io.MultiWriter(buf, w)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
