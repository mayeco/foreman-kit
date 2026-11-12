package cycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

const ExplorationFile = ".foreman-kit/exploration.yaml"

// ExplorationStrategy represents a strategy entry in .foreman-kit/exploration.yaml.
type ExplorationStrategy struct {
	Slug      string `yaml:"slug" json:"slug"`
	Name      string `yaml:"name" json:"name"`
	Kind      string `yaml:"kind" json:"kind"`
	Directive string `yaml:"directive" json:"directive"`
	// Session says who runs the strategy: the cycle or the visual session.
	Session string `yaml:"session,omitempty" json:"session,omitempty"`
	Enabled *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// RunsIn returns the session that runs the strategy: the visual session when it says so, else the
// cycle.
func (s ExplorationStrategy) RunsIn() string {
	if s.Session == settings.SessionVisualQA {
		return settings.SessionVisualQA
	}
	return settings.SessionCycle
}

var strategyKindInstructions = map[string]string{
	"defects":      "record each confirmed defect or regression as a backlog item in `.foreman-kit/agent_backlog.json` (foreman-kit-qa, \"Record a finding\"), at most %d this cycle",
	"improvements": "record each improvement as a proposal in `.foreman-kit/agent_proposals.json` (foreman-kit-qa, \"Record a finding\"), at most %d this cycle",
	"features":     "propose each missing capability as a proposal in `.foreman-kit/agent_proposals.json` (foreman-kit-qa, \"Record a finding\"), at most %d this cycle",
}

var proposalApprovalDescriptions = map[string]string{
	settings.ApprovalEvaluator: "after you exit, the script asks the evaluator (gate `proposal_review` of `" + settings.GateMode("proposal_review").FilePath() + "`) and queues the proposals it approves; the rest wait for a human",
	settings.ApprovalHuman:     "a human approves each one (`foreman-kit proposals approve <change>`); the script then queues it",
	settings.ApprovalNobody:    "the script queues every proposal after you exit, without review",
}

// LoadExplorationSelection returns how each run picks its strategy ("in_order" or "random").
func LoadExplorationSelection(root string) string {
	return settings.String(root, settings.ExplorationSelection)
}

// LoadExplorationStrategies loads the enabled strategies of .foreman-kit/exploration.yaml. An application
// without the file reads the strategies the kit ships, and a worktree cut before `foreman-kit
// update` rewrote the file reads the one of the main checkout (settings.FilesRoot).
func LoadExplorationStrategies(root string) []ExplorationStrategy {
	type expFile struct {
		Strategies []ExplorationStrategy `yaml:"strategies"`
	}
	var ef expFile
	p := filepath.Join(settings.FilesRoot(root), filepath.FromSlash(ExplorationFile))
	if b, err := os.ReadFile(p); err == nil {
		_ = yaml.Unmarshal(b, &ef)
	} else if b, err := assets.ReadSeed(ExplorationFile); err == nil {
		_ = yaml.Unmarshal(b, &ef)
	}
	var list []ExplorationStrategy
	for _, s := range ef.Strategies {
		if s.Slug == "" {
			continue
		}
		if s.Enabled != nil && !*s.Enabled {
			continue
		}
		list = append(list, s)
	}
	return list
}

// SessionStrategies returns the strategies of list that session runs (settings.SessionCycle or
// settings.SessionVisualQA). No strategy changes session on its own: one of a session that is off
// does not run.
func SessionStrategies(list []ExplorationStrategy, session string) []ExplorationStrategy {
	var out []ExplorationStrategy
	for _, s := range list {
		if s.RunsIn() == session {
			out = append(out, s)
		}
	}
	return out
}

// PickExplorationStrategy picks the next strategy as selection says ("in_order" or "random") and persists state to stateFile.
func PickExplorationStrategy(list []ExplorationStrategy, order, forced, stateFile string) (*ExplorationStrategy, error) {
	if forced != "" {
		for i := range list {
			if list[i].Slug == forced {
				cp := list[i]
				return &cp, nil
			}
		}
		return nil, fmt.Errorf("unknown strategy %s", forced)
	}
	if len(list) == 0 {
		return nil, nil
	}

	type rotState struct {
		Next   int      `json:"next"`
		Recent []string `json:"recent"`
	}
	var st rotState
	if stateFile != "" {
		if b, err := os.ReadFile(stateFile); err == nil {
			_ = json.Unmarshal(b, &st)
		}
	}

	slugs := make([]string, len(list))
	slugIdx := make(map[string]int, len(list))
	for i, s := range list {
		slugs[i] = s.Slug
		slugIdx[s.Slug] = i
	}

	var chosen ExplorationStrategy
	if order == settings.ExplorationRandom {
		recentSet := make(map[string]bool)
		for _, r := range st.Recent {
			if _, ok := slugIdx[r]; ok {
				recentSet[r] = true
			}
		}
		var fresh []ExplorationStrategy
		for _, s := range list {
			if !recentSet[s.Slug] {
				fresh = append(fresh, s)
			}
		}
		pool := fresh
		if len(pool) == 0 {
			pool = list
		}
		chosen = pool[rand.Intn(len(pool))]
	} else {
		idx := st.Next % len(list)
		if idx < 0 {
			idx = 0
		}
		chosen = list[idx]
	}

	st.Next = (slugIdx[chosen.Slug] + 1) % len(list)
	st.Recent = append(st.Recent, chosen.Slug)
	half := len(list) / 2
	if half < 1 {
		half = 1
	}
	if len(st.Recent) > half {
		st.Recent = st.Recent[len(st.Recent)-half:]
	}

	if stateFile != "" {
		if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err == nil {
			if b, err := json.MarshalIndent(st, "", "  "); err == nil {
				_ = os.WriteFile(stateFile, append(b, '\n'), 0o644)
			}
		}
	}
	return &chosen, nil
}

// RenderCyclePrompt renders the full judgment prompt for a scheduled cycle (bin/cycle_prompt.rb).
func RenderCyclePrompt(root string, strategy *ExplorationStrategy, cycleCfg map[string]any, dispatchPID string) (string, error) {
	return renderCyclePrompt(root, strategy, cycleCfg, cyclePlan{DispatchPID: dispatchPID, QueueOpen: -1})
}

// cyclePlan is what the script decided for one cycle before its judgment, and tells it in "This
// cycle".
type cyclePlan struct {
	// DispatchPID is the PID of the dispatch that runs, "" when none does. Continuous says the
	// daemon dispatches continuously (DispatchPID is then its dispatcher's, when known).
	DispatchPID string
	Continuous  bool
	// Strategy forces the exploration strategy; "": the next one of the rotation.
	Strategy string
	// QueueOpen is how many changes the cycle counted as open, which decides how many findings it
	// may record (quotaFor); negative when nobody counted them.
	QueueOpen int
	// Recommended are the settings the kit recommends changing, for the judgment to pass on.
	Recommended []Recommendation
	// Stamp names the cycle; "" when the prompt is rendered outside one. The judgment names its
	// report after it, which is where the script looks for it.
	Stamp string
	// StateDir is where the cycle's own files are, as an absolute path: its reports and state.json.
	// The judgment runs in .worktrees/_cycle, and a path relative to it would land in the worktree,
	// where the script never looks. "" when the prompt is rendered outside a cycle.
	StateDir string
	// VerifySince is the commit of the umbrella the judgment verifies what was integrated after
	// (foreman-kit completed-since), and VerifySinceWhat says what that commit is: the one the last
	// cycle ran on, or, before any cycle ran, the one that installed the kit. "" when neither is known.
	VerifySince, VerifySinceWhat string
}

// cycleReportName is the file name of the report of the cycle stamp.
func cycleReportName(stamp string) string { return stamp + ".md" }

// findCycleReport returns the report the judgment of the cycle stamp wrote under dir: the file named
// after the cycle, or else the newest report written since the cycle began (a judgment that follows
// a skill installed before the script told it the name calls it by the minute it wrote it). "" when
// there is none.
func findCycleReport(dir, stamp string, since time.Time) string {
	named := filepath.Join(dir, cycleReportName(stamp))
	if fileExists(named) {
		return named
	}
	entries, _ := os.ReadDir(dir)
	newest, at := "", since
	for _, e := range entries {
		info, err := e.Info()
		// A visual QA session writes its own reports there (visual-<stamp>.md).
		if err != nil || e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || strings.HasPrefix(e.Name(), "visual-") || info.ModTime().Before(at) {
			continue
		}
		newest, at = filepath.Join(dir, e.Name()), info.ModTime()
	}
	return newest
}

// renderCyclePrompt renders the judgment prompt of a cycle with the plan the script made for it.
func renderCyclePrompt(root string, strategy *ExplorationStrategy, cycleCfg map[string]any, plan cyclePlan) (string, error) {
	dispatchPID, continuous, queueOpen := plan.DispatchPID, plan.Continuous, plan.QueueOpen
	basePrompt, err := ReadPromptRaw(root, "cycle", true)
	if err != nil {
		return "", err
	}
	basePrompt = strings.TrimRight(HTMLCommentRe.ReplaceAllString(basePrompt, ""), "\n")

	r := cycleReader(root, cycleCfg)
	approval := proposalsApproval(r)
	promptApproval := r.String(settings.PromptEditsApprovedBy)

	strat := ExplorationStrategy{
		Slug:      "none",
		Name:      "None this cycle",
		Kind:      "defects",
		Directive: "No strategy of .foreman-kit/exploration.yaml runs in the cycle (session: " + settings.SessionCycle + "). Spend the exploration time on the regression sweep.",
	}
	if strategy != nil {
		strat = *strategy
	}
	if strat.Name == "" {
		strat.Name = strat.Slug
	}
	kind := strat.Kind
	if _, ok := strategyKindInstructions[kind]; !ok {
		kind = "defects"
	}

	quota := quotaFor(r, queueOpen)
	queueFull := quota.Counted && quota.Findings == 0
	budgetMin := int(r.Duration(settings.CycleBudget) / time.Minute)
	expMin := int(r.Duration(settings.CycleExploring) / time.Minute)

	var dispatchLine string
	if continuous {
		pidNote := ""
		if pid := strings.TrimSpace(dispatchPID); pid != "" {
			pidNote = fmt.Sprintf(" (pid %s)", pid)
		}
		dispatchLine = fmt.Sprintf("the daemon dispatches continuously%s: what you queue starts as soon as a worker is free and its dependencies are integrated. Verify what is already integrated, explore and queue as usual.", pidNote)
	} else if strings.TrimSpace(dispatchPID) != "" {
		dispatchLine = fmt.Sprintf("a dispatch is running (pid %s) and builds what earlier cycles queued. Verify what is already integrated, explore and queue as usual: it reads the backlog again as it goes, so what you queue starts as soon as a worker is free.", strings.TrimSpace(dispatchPID))
	} else {
		dispatchLine = "none is running; the script dispatches after you exit."
	}

	roadmapFile := r.String(settings.RoadmapFile)
	var roadmapLine string
	if queueFull {
		roadmapLine = "none this cycle: the queue is full (see \"Queue\"): skip \"Propose from the roadmap\"."
	} else if roadmapFile == "" || !fileExists(filepath.Join(root, filepath.FromSlash(roadmapFile))) {
		roadmapLine = fmt.Sprintf("none (`%s` in `%s` is empty or missing): skip \"Propose from the roadmap\".", settings.RoadmapFile.Path(), settings.RoadmapFile.FilePath())
	} else {
		b, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(roadmapFile)))
		if strings.Contains(string(b), "{{") {
			roadmapLine = fmt.Sprintf("`%s` still has `{{...}}` placeholders: skip \"Propose from the roadmap\" and say in the report that a human must fill it in.", roadmapFile)
		} else {
			maxNewRM := r.Int(settings.RoadmapMaxProposals)
			maxOpenRM := r.Int(settings.RoadmapMaxOpen)
			roadmapLine = fmt.Sprintf("`%s`: propose the next features it lists that are not built, queued or proposed yet, at most %d this cycle, and none while %d roadmap proposals are still proposed, approved or queued.", roadmapFile, maxNewRM, maxOpenRM)
		}
	}

	var indentedDirective []string
	for _, line := range strings.Split(strings.TrimSpace(strat.Directive), "\n") {
		indentedDirective = append(indentedDirective, "  "+line)
	}

	approvalDesc := proposalApprovalDescriptions[approval]

	var visualLine string
	if !r.Bool(settings.VisualQAEnabled) {
		visualLine = fmt.Sprintf("off (`%s` in `%s`): verify and explore here as well as you can.", settings.VisualQAEnabled.Path(), settings.VisualQAEnabled.FilePath())
	} else {
		visualLine = "a browser session in an agent app (`foreman-kit-visual-qa` skill) checks every integrated change in the application and explores the strategies of its own session. You run headless: when only a browser can confirm something (a rendered page, a flow, the console), ask it with `foreman-kit visual-qa request \"<what to check, where, and why>\" [--change <name>]` and say so in the report."
	}

	whoPrompts := "a human approves it (`foreman-kit proposals show <change>`, then `approve <change>`), and the next cycle applies it"
	if promptApproval == settings.ApprovalNobody {
		whoPrompts = fmt.Sprintf("the script applies it after you exit, without review (`%s: %s`)", settings.PromptEditsApprovedBy.Path(), settings.ApprovalNobody)
	}
	promptsLine := fmt.Sprintf("never edit `.foreman-kit/prompts/`; an edit of a prompt that evidence argues for is a prompt proposal (foreman-kit-cycle, \"Propose prompt edits\"): %s. Versions to compare: `foreman-kit prompt-report`.", whoPrompts)

	findingsLine := fmt.Sprintf(strategyKindInstructions[kind], quota.Findings) + ". Defects found outside this strategy follow the same rule; improvements and missing capabilities always become proposals."
	if queueFull {
		findingsLine = "none this cycle: the queue is full (see \"Queue\")."
	}

	reportLine := ""
	if plan.Stamp != "" {
		reports := "log/cycle/reports"
		if plan.StateDir != "" {
			reports = filepath.Join(plan.StateDir, "reports")
		}
		reportLine = fmt.Sprintf("- **Report**: write it as `%s` (this cycle is `%s`); the script records the report under that name.\n", filepath.Join(reports, cycleReportName(plan.Stamp)), plan.Stamp)
	}
	if plan.StateDir != "" {
		reportLine += fmt.Sprintf("- **State**: `%s` is the cycle's state between runs (the skill's `log/cycle/state.json`); read it and update it there. Your working directory is `.worktrees/_cycle`, so write these two files by these absolute paths, never under `log/` of the worktree.\n", filepath.Join(plan.StateDir, "state.json"))
	}
	if plan.VerifySince != "" {
		reportLine += fmt.Sprintf("- **Verify**: run `foreman-kit completed-since %s` (%s). It lists every change integrated since, with the lessons of its `handoff.md` and the rounds of its `repairs.md`: verify each one and curate its lessons, the first cycle too.\n", plan.VerifySince, plan.VerifySinceWhat)
	}

	return fmt.Sprintf(`%s

## This cycle (decided by the script)

- **Time**: %d minutes, at least %d of them on the exploration strategy below.
- **Dispatch**: %s
%s- **Exploration strategy**: %s (`+"`%s`"+`, kind `+"`%s`"+`).

%s

- **Its findings**: %s
%s- **Roadmap**: %s
- **Proposals** (`+"`.foreman-kit/agent_proposals.json`"+`, approval `+"`%s`"+`): %s. Add proposals with `+"`\"status\": \"proposed\"`"+` only; never change the status of an existing one.
- **Visual QA**: %s
- **Memory**: %s
- **Prompts**: %s
%s`,
		basePrompt,
		budgetMin, expMin,
		dispatchLine,
		reportLine,
		strat.Name, strat.Slug, kind,
		strings.Join(indentedDirective, "\n"),
		findingsLine,
		quota.promptLine(),
		roadmapLine,
		approval, approvalDesc,
		visualLine,
		MemoryPlanLine(root),
		promptsLine,
		recommendedLine(plan.Recommended),
	), nil
}

// RunCyclePromptCLI implements `foreman-kit cycle-prompt <output-file> [--dispatch-running PID | --dispatch-continuous [PID]] [--strategy SLUG] [--queue-open N]`.
// --queue-open is how many changes the cycle counted as open; without it the prompt does not say how
// full the queue is, and holds the judgment to the findings of one cycle alone.
func RunCyclePromptCLI(ctx context.Context, root string, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	plan := cyclePlan{QueueOpen: -1}
	setQueueOpen := func(text string) bool {
		n, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || n < 0 {
			fmt.Fprintf(stderr, "cycle_prompt: --queue-open takes the number of open changes, not %q\n", text)
			return false
		}
		plan.QueueOpen = n
		return true
	}
	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--dispatch-continuous":
			plan.Continuous = true
			if i+1 < len(args) && args[i+1] != "" && strings.Trim(args[i+1], "0123456789") == "" {
				plan.DispatchPID = args[i+1]
				i++
			}
		case strings.HasPrefix(args[i], "--dispatch-continuous="):
			plan.Continuous = true
			plan.DispatchPID = strings.TrimPrefix(args[i], "--dispatch-continuous=")
		case args[i] == "--dispatch-running" && i+1 < len(args):
			plan.DispatchPID = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--dispatch-running="):
			plan.DispatchPID = strings.TrimPrefix(args[i], "--dispatch-running=")
		case args[i] == "--strategy" && i+1 < len(args):
			plan.Strategy = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--strategy="):
			plan.Strategy = strings.TrimPrefix(args[i], "--strategy=")
		case args[i] == "--queue-open" && i+1 < len(args):
			if !setQueueOpen(args[i+1]) {
				return 2
			}
			i++
		case strings.HasPrefix(args[i], "--queue-open="):
			if !setQueueOpen(strings.TrimPrefix(args[i], "--queue-open=")) {
				return 2
			}
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "Usage: foreman-kit cycle-prompt <output-file> [--dispatch-running PID | --dispatch-continuous [PID]] [--strategy SLUG] [--queue-open N]")
		return 2
	}
	return writeCyclePrompt(root, positional[0], plan, stdout, stderr)
}

// writeCyclePrompt picks the exploration strategy of a cycle, renders its judgment prompt with the
// plan given and writes it to outPath. It says on stdout which strategy it picked.
func writeCyclePrompt(root, outPath string, plan cyclePlan, stdout, stderr io.Writer) int {
	strategies := SessionStrategies(LoadExplorationStrategies(root), settings.SessionCycle)
	stateFile := store.RuntimePath(root, "cycle", "strategy_state.json")
	chosen, err := PickExplorationStrategy(strategies, LoadExplorationSelection(root), plan.Strategy, stateFile)
	if err != nil {
		fmt.Fprintf(stderr, "cycle_prompt: %v\n", err)
		return 1
	}
	rendered, err := renderCyclePrompt(root, chosen, nil, plan)
	if err != nil {
		fmt.Fprintf(stderr, "cycle_prompt: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		fmt.Fprintf(stderr, "cycle_prompt: %v\n", err)
		return 1
	}
	if err := os.WriteFile(outPath, []byte(rendered), 0o644); err != nil {
		fmt.Fprintf(stderr, "cycle_prompt: %v\n", err)
		return 1
	}
	if chosen != nil {
		k := chosen.Kind
		if k == "" {
			k = "defects"
		}
		fmt.Fprintf(stdout, "strategy: %s (%s)\n", chosen.Slug, k)
	} else {
		fmt.Fprintln(stdout, "strategy: none (all need a browser)")
	}
	return 0
}

// CycleOptions configures RunCycle and provides callback hooks for external orchestrator/proc actions.
type CycleOptions struct {
	Detach           bool
	Stamp            string
	Strategy         string
	SkipWorktreePrep bool
	PreflightFunc    func(ctx context.Context, root string, out io.Writer) error
	MergeFunc        func(ctx context.Context, root string, out io.Writer) error
	AgentExecFunc    func(ctx context.Context, workdir, promptFile, engine string, timeout time.Duration, out io.Writer) error
	DispatchFunc     func(ctx context.Context, root string, out io.Writer) error
	DetachSpawnFunc  func(ctx context.Context, root, stamp string) (int, error)
	// SkipMerge leaves integration to a continuous merger (the daemon's): the cycle does not drain
	// the merge queue first, it judges what was already integrated.
	SkipMerge bool
	// SkipDispatch leaves dispatching to a continuous dispatcher (the daemon's): the cycle ends after
	// recording, calling Notify so that dispatcher reads what this cycle queued at once.
	SkipDispatch bool
	Notify       func()
	// Trigger is what starts this cycle, a store.CycleTrigger* value the daemon gives. Empty: someone
	// asked for it, from the command line, a scheduler or the portal.
	Trigger string
}

type cycleLogWriter struct {
	ctx    context.Context
	st     *store.Store
	execID string
	out    io.Writer
	file   *os.File
	buf    bytes.Buffer
}

func (w *cycleLogWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	if w.out != nil {
		_, _ = w.out.Write(p)
	}
	if w.file != nil {
		_, _ = w.file.Write(p)
	}
	if w.st != nil && w.execID != "" {
		_ = w.st.AppendLogChunk(w.ctx, w.execID, "combined", string(p))
	}
	return len(p), nil
}

// newCycleStamp names a cycle that starts now: CYCLE_STAMP when the caller's environment gives one
// (a detached cycle is told its stamp that way), the UTC second otherwise. It is a variable so tests
// can start two cycles within one second.
var newCycleStamp = func() string {
	if stamp := strings.TrimSpace(os.Getenv("CYCLE_STAMP")); stamp != "" {
		return stamp
	}
	return time.Now().UTC().Format("20060102T150405Z")
}

// RunCycle executes one scheduled autonomous cycle (bin/run_cycle.sh).
func RunCycle(ctx context.Context, root string, st *store.Store, opts CycleOptions, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	if st != nil {
		if ws, err := st.GetWorkspaceState(ctx); err == nil && ws.Paused {
			fmt.Fprintf(out, "cycle: paused (%s)\n", ws.PauseReason)
			return nil
		}
	}
	if fileExists(store.RuntimePath(root, "PAUSE")) {
		b, _ := os.ReadFile(store.RuntimePath(root, "PAUSE"))
		reason := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
		fmt.Fprintf(out, "cycle: paused (%s)\n", reason)
		return nil
	}
	// A cycle syncs with, rebases onto and pushes to the base branch, and judges with what its files
	// say: without a base branch, or with a file it cannot use, it does not run.
	if err := RequireUsable(root, settings.CycleFiles...); err != nil {
		fmt.Fprintf(out, "cycle: %v\n", err)
		return err
	}
	if err := gitx.CheckVersion(ctx); err != nil {
		fmt.Fprintf(out, "cycle: %v\n", err)
		return err
	}

	stamp := opts.Stamp
	if stamp == "" {
		stamp = newCycleStamp()
	}

	if opts.Detach {
		if st != nil {
			if l, err := st.LiveLock(ctx, "cycle"); err == nil && l != nil {
				fmt.Fprintf(out, "cycle: already running (pid %d)\n", l.PID)
				return nil
			}
		}
		var pid int
		var err error
		if opts.DetachSpawnFunc != nil {
			pid, err = opts.DetachSpawnFunc(ctx, root, stamp)
		} else {
			pid, err = spawnDetachedCycleProcess(root, stamp, opts.Strategy)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "cycle: started pid=%d log=%s\n", pid, store.RuntimePath(root, "cycle", "cycle-"+stamp+".log"))
		return nil
	}

	pid := os.Getpid()
	if st != nil {
		_, err := st.AcquireLock(ctx, "cycle", store.LockOptions{
			OwnerType: "cycle",
			PID:       pid,
			Slot:      98,
			Stamp:     stamp,
			TTL:       2 * time.Hour,
		})
		if errors.Is(err, store.ErrLockHeld) {
			holderPID := 0
			if l, _ := st.GetLock(ctx, "cycle"); l != nil {
				holderPID = l.PID
			}
			fmt.Fprintf(out, "cycle: already running (pid %d)\n", holderPID)
			return nil
		}
		if err != nil {
			return err
		}
	}
	stopHB := st.StartLockHeartbeat(ctx, "cycle", pid, 2*time.Hour)
	cycleLockHeld := true
	defer func() {
		stopHB()
		if cycleLockHeld && st != nil {
			_ = st.ReleaseLock(context.Background(), "cycle", pid)
		}
	}()

	cycleDir := store.RuntimePath(root, "cycle")
	_ = os.MkdirAll(filepath.Join(cycleDir, "reports"), 0o755)
	cycleBegan := time.Now()
	logPath := filepath.Join(cycleDir, fmt.Sprintf("cycle-%s.log", stamp))
	logFile, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if logFile != nil {
		defer logFile.Close()
	}

	engines := RoleEngines(root, "cycle")
	engineCycle := strings.Join(engines, ",")
	execID := "cycle-" + stamp
	if st != nil {
		// The cycle is a row of cycles, and its execution, the one whose output this is, points at
		// it; so does what the cycle writes from here on, through the context.
		trigger := opts.Trigger
		if trigger == "" {
			trigger = store.CycleTriggerManual
		}
		_ = st.BeginCycle(ctx, store.Cycle{Stamp: stamp, PID: pid, LogPath: logPath, Trigger: trigger})
		ctx = store.WithCycle(ctx, stamp)
		_ = st.CreateExecution(ctx, store.Execution{
			ExecID:           execID,
			Kind:             "cycle",
			CycleStamp:       stamp,
			EnginesRequested: engineCycle,
			Role:             "cycle",
			Status:           "running",
			PID:              pid,
			StartedAt:        time.Now().UTC().Format(time.RFC3339),
		})
		// The agents the cycle calls are attempts of this execution, and their exit events name it.
		ctx = store.WithExecution(ctx, execID)
	}

	lw := &cycleLogWriter{
		ctx:    ctx,
		st:     st,
		execID: execID,
		out:    out,
		file:   logFile,
	}

	finished := false
	finishExec := func(status string, code int) {
		finished = true
		if st != nil {
			_ = st.FinishExecution(context.Background(), execID, status, code)
			sum := SummarizeCycleLog(stamp, lw.buf.String(), 0)
			// The cycle's own row says what it was and how it ended: the latest finished one is the
			// workspace's last cycle.
			reportPath := findCycleReport(filepath.Join(cycleDir, "reports"), stamp, cycleBegan)
			// The log names the judge as it ran it; only a plain engine name is recorded as one.
			judge := asString(sum["judge"])
			if strings.ContainsAny(judge, " \t/") {
				judge = ""
			}
			_ = st.UpdateCycle(context.Background(), store.Cycle{
				Stamp:         stamp,
				Strategy:      asString(sum["strategy"]),
				JudgeEngine:   judge,
				ReportPath:    reportPath,
				FindingsCount: -1,
			})
			_ = st.FinishCycle(context.Background(), stamp, asString(sum["outcome"]), "")
		}
	}
	// Every return below records the outcome first; only a panic unwinds past them. The daemon contains
	// it and keeps running, so without this the execution would stay "running" under the daemon's live
	// PID for as long as the daemon runs.
	defer func() {
		if !finished {
			finishExec("failed", 1)
		}
	}()

	fmt.Fprintf(lw, "== cycle %s\n", stamp)
	if st != nil {
		_ = st.SyncJSONToSQLite(ctx, root)
	}
	var dispatchRunningPID string
	if st != nil {
		if dl, _ := st.LiveLock(ctx, "dispatch"); dl != nil {
			dispatchRunningPID = fmt.Sprint(dl.PID)
		} else if dl, _ := st.LiveLock(ctx, "dispatcher"); dl != nil {
			dispatchRunningPID = fmt.Sprint(dl.PID)
		}
	}
	if opts.SkipDispatch {
		fmt.Fprintln(lw, "dispatch: continuous (the daemon's dispatcher): this cycle judges and records, and does not dispatch")
	} else if dispatchRunningPID != "" {
		fmt.Fprintf(lw, "dispatch in progress (pid %s): this cycle judges and records, and does not dispatch\n", dispatchRunningPID)
	}

	fmt.Fprintln(lw, "== preflight")
	if opts.PreflightFunc != nil {
		if err := opts.PreflightFunc(ctx, root, lw); err != nil {
			fmt.Fprintln(lw, "preflight: FAIL")
			finishExec("failed", 1)
			return err
		}
	} else {
		fmt.Fprintln(lw, "preflight: OK")
	}

	fmt.Fprintln(lw, "== merge")
	mergerRunning := false
	if st != nil && !opts.SkipMerge {
		if ml, _ := st.LiveLock(ctx, "merger"); ml != nil {
			mergerRunning = true
		}
	}
	if opts.SkipMerge {
		fmt.Fprintln(lw, "merge: continuous (the daemon's merger integrates each change as soon as it is ready)")
	} else if mergerRunning {
		fmt.Fprintln(lw, "merger running; not draining")
	} else if opts.MergeFunc != nil {
		_ = opts.MergeFunc(ctx, root, lw)
	}

	fmt.Fprintln(lw, "== sync")
	wtBase := settings.WorktreesBase(root)
	cycleWT := root
	var carriedEdits string
	baseBranch := ResolveBaseBranch(root)

	if !opts.SkipWorktreePrep && dirExists(filepath.Join(root, ".git")) {
		cycleWT = filepath.Join(wtBase, "_cycle")
		err := WithRecordLock(ctx, st, 30*time.Minute, func() error {
			vis := ActiveVisualSession(ctx, st)
			if vis != "" {
				fmt.Fprintf(lw, "visual QA session %s in progress: leaving main checkout untouched; judging in .worktrees/_cycle\n", vis)
				if repo, err := gitx.Open(root); err == nil {
					_ = repo.Fetch(ctx, "origin", baseBranch)
				}
			} else {
				if err := ScopeGuard(ctx, root, "cycle "+stamp, st, lw); err != nil {
					return err
				}
				_ = SyncRepos(ctx, root, st, false, lw)
			}
			baseRef := "HEAD"
			if _, err := gitx.RunGit(ctx, root, "rev-parse", "--verify", "--quiet", "origin/"+baseBranch); err == nil {
				baseRef = "origin/" + baseBranch
			}
			if err := PrepareSessionWorktree(ctx, root, cycleWT, baseRef, baseBranch, lw); err != nil {
				return err
			}
			if vis == "" {
				carriedEdits, _ = ScopeEdits(ctx, root)
				if carriedEdits != "" {
					_ = CarryProposals(ctx, cycleWT, root, lw)
				}
			}
			return nil
		})
		if err != nil {
			finishExec("failed", 1)
			return err
		}
	}

	fmt.Fprintln(lw, "== visual queue")
	// In the linked worktree .worktrees/_cycle, .git is a file, not a directory.
	if st != nil && (dirExists(filepath.Join(cycleWT, ".git")) || fileExists(filepath.Join(cycleWT, ".git"))) {
		_ = VisualQAEnqueueIntegrated(ctx, cycleWT, st, lw)
	}

	fmt.Fprintln(lw, "== plan")
	// What the backlog and the proposals list before the judgment: what they list after it, and did
	// not list here, is what this cycle added.
	listed := store.ListMirrored(cycleWT)
	// What the cycle may queue is decided here, before the judgment, from what is open now; the
	// judgment is told, and held to it after it exits.
	quota := planQuota(ctx, cycleWT, st, lw)
	promptFile := filepath.Join(cycleDir, fmt.Sprintf("prompt-%s.md", stamp))
	plan := cyclePlan{DispatchPID: dispatchRunningPID, Continuous: opts.SkipDispatch, Strategy: opts.Strategy, QueueOpen: -1, Stamp: stamp, StateDir: cycleDir}
	ensureCycleState(cycleDir, lw)
	plan.VerifySince, plan.VerifySinceWhat = verifySince(ctx, cycleWT, st, stamp)
	if head, err := gitx.RunGit(ctx, cycleWT, "rev-parse", "HEAD"); err == nil && st != nil {
		// The commit this cycle verifies up to: the next one verifies what was integrated after it.
		_ = st.UpdateCycle(ctx, store.Cycle{Stamp: stamp, UmbrellaSHA: strings.TrimSpace(head), FindingsCount: -1})
	}
	if quota.Counted {
		plan.QueueOpen = quota.Open
	}
	// What the last day of records argues for changing in the settings goes into the log and to
	// the judgment, which passes it on in its report. Nothing is changed.
	plan.Recommended = recommendForCycle(ctx, cycleWT, st, lw)
	if rc := writeCyclePrompt(cycleWT, promptFile, plan, lw, lw); rc != 0 {
		finishExec("failed", rc)
		return fmt.Errorf("cycle_prompt failed with code %d", rc)
	}

	engineLabel := engineCycle
	if engineLabel == "" {
		engineLabel = "no agent: " + settings.RoleAgents("cycle").Where() + " lists none"
	}
	fmt.Fprintf(lw, "== judgment (%s)\n", engineLabel)
	// The judgment's hard limit is its own key: nothing is added to the budget it was told.
	judgmentTimeout := settings.Duration(cycleWT, settings.RoleTimeout("cycle"))
	if opts.AgentExecFunc != nil {
		if err := opts.AgentExecFunc(ctx, cycleWT, promptFile, engineCycle, judgmentTimeout, lw); err != nil {
			fmt.Fprintln(lw, "cycle: agent exited non-zero")
		}
	}

	fmt.Fprintln(lw, "== scope guard")
	if dirExists(filepath.Join(cycleWT, ".git")) || fileExists(filepath.Join(cycleWT, ".git")) {
		if err := ScopeGuard(ctx, cycleWT, "cycle "+stamp, st, lw); err != nil {
			finishExec("failed", 1)
			return err
		}
	}

	fmt.Fprintln(lw, "== proposals")
	quota.enforce(ctx, cycleWT, stamp, filepath.Join(cycleDir, fmt.Sprintf("not-queued-%s.json", stamp)), st, lw)
	if err := ReviewProposals(ctx, cycleWT, st, nil, lw); err != nil {
		fmt.Fprintln(lw, "cycle: proposal review failed; proposals left as they were")
	}
	// What this cycle queued and proposed is on record as its own from here on. The cycle says it,
	// because it is the one that knows: the main checkout comes to list those items later, and with
	// a visual QA session in progress, much later.
	if st != nil {
		if err := st.RecordCycleWork(ctx, stamp, listed.AddedSince(cycleWT)); err != nil {
			fmt.Fprintf(lw, "cycle: what this cycle queued is not on record as its own: %v\n", err)
		}
	}

	fmt.Fprintln(lw, "== record")
	if dirExists(filepath.Join(cycleWT, ".git")) || fileExists(filepath.Join(cycleWT, ".git")) {
		_ = WithRecordLock(ctx, st, 30*time.Minute, func() error {
			if err := RecordSession(ctx, cycleWT, baseBranch, "chore(memory): scheduled cycle "+stamp, st, lw); err == nil {
				if ActiveVisualSession(ctx, st) == "" && carriedEdits != "" {
					_ = ReleaseScopeEdits(ctx, root, carriedEdits)
				}
			}
			if ActiveVisualSession(ctx, st) == "" {
				_ = SyncRepos(ctx, root, st, false, io.Discard)
			}
			return nil
		})
	}
	// The queue is what the main checkout lists, as this cycle left it.
	if st != nil {
		_ = st.SyncJSONToSQLite(ctx, root)
	}

	stopHB()
	if st != nil {
		_ = st.ReleaseLock(ctx, "cycle", pid)
		cycleLockHeld = false
	}

	if st != nil {
		if ws, err := st.GetWorkspaceState(ctx); err == nil && ws.Paused {
			fmt.Fprintln(lw, "cycle: paused by agent or operator")
			finishExec("succeeded", 0)
			return nil
		}
	}
	if fileExists(store.RuntimePath(root, "PAUSE")) {
		fmt.Fprintln(lw, "cycle: paused by agent or operator")
		finishExec("succeeded", 0)
		return nil
	}

	if opts.SkipDispatch {
		fmt.Fprintln(lw, "== dispatch (continuous: the daemon's dispatcher picks up what this cycle queued)")
		if opts.Notify != nil {
			opts.Notify()
		}
		fmt.Fprintln(lw, "== done")
		finishExec("succeeded", 0)
		return nil
	}

	fmt.Fprintln(lw, "== dispatch")
	if st != nil {
		_, err := st.AcquireLock(ctx, "dispatch", store.LockOptions{
			OwnerType: "dispatch",
			PID:       pid,
			Stamp:     stamp,
			TTL:       2 * time.Hour,
		})
		if errors.Is(err, store.ErrLockHeld) {
			fmt.Fprintf(lw, "dispatch: already running (pid %s); it picks up what this cycle queued when a worker is free\n", dispatchRunningPID)
			fmt.Fprintln(lw, "== done")
			finishExec("succeeded", 0)
			return nil
		}
		defer func() {
			_ = st.ReleaseLock(context.Background(), "dispatch", pid)
		}()
		if dl, _ := st.LiveLock(ctx, "dispatcher"); dl != nil {
			if asString(parseDaemonMetadata(dl.MetadataJSON)["mode"]) == "continuous" {
				fmt.Fprintf(lw, "dispatch: the daemon dispatches continuously (pid %d); it picks up what this cycle queued\n", dl.PID)
			} else {
				fmt.Fprintf(lw, "dispatch: a dispatcher started by hand is running (pid %d); not starting another\n", dl.PID)
			}
			fmt.Fprintln(lw, "== done")
			finishExec("succeeded", 0)
			return nil
		}
	}

	if opts.DispatchFunc != nil {
		_ = opts.DispatchFunc(ctx, root, lw)
	}
	fmt.Fprintln(lw, "== done")
	finishExec("succeeded", 0)
	return nil
}

// spawnDetachedCycleProcess starts `foreman-kit cycle` in its own session and returns its PID. The stamp
// travels in CYCLE_STAMP and a forced strategy as --strategy, so the detached cycle is the one asked for.
func spawnDetachedCycleProcess(root, stamp, strategy string) (int, error) {
	exe, err := kitExecutable()
	if err != nil {
		return 0, err
	}
	args := []string{"cycle"}
	if strategy != "" {
		args = append(args, "--strategy", strategy)
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = root
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "FOREMANKIT_APP_ROOT=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "CYCLE_STAMP="+stamp)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err == nil {
		defer devNull.Close()
		cmd.Stdin = devNull
		cmd.Stdout = devNull
		cmd.Stderr = devNull
	}
	pid, _, err := startDetached(cmd)
	return pid, err
}

// RunCycleCLI implements `foreman-kit cycle [--detach] [--strategy SLUG]`.
func RunCycleCLI(ctx context.Context, root string, st *store.Store, opts CycleOptions, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--detach":
			opts.Detach = true
		case args[i] == "--strategy" && i+1 < len(args):
			opts.Strategy = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--strategy="):
			opts.Strategy = strings.TrimPrefix(args[i], "--strategy=")
		default:
			fmt.Fprintln(stderr, "Usage: foreman-kit cycle [--detach] [--strategy SLUG]")
			return 2
		}
	}
	if err := RunCycle(ctx, root, st, opts, stdout); err != nil {
		fmt.Fprintf(stderr, "cycle: %v\n", err)
		return 1
	}
	return 0
}

// cycleStateSkeleton is the state.json a cycle starts from when none exists: the fields the skill
// foreman-kit-cycle names, empty.
const cycleStateSkeleton = `{
  "last_cycle": {"started_at": null, "finished_at": null, "umbrella_sha": null, "outcome": null, "report_path": null},
  "test_baseline": {"measured_at": null, "passing": null, "failing": null, "known_failures": []},
  "exploration_state": {"history": []},
  "verified": {},
  "needs_triage": [],
  "notes_for_next_cycle": [],
  "journal": []
}
`

// ensureCycleState creates the cycle's state.json in dir when there is none.
func ensureCycleState(dir string, out io.Writer) {
	p := filepath.Join(dir, "state.json")
	if fileExists(p) {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err == nil {
		if err := os.WriteFile(p, []byte(cycleStateSkeleton), 0o644); err == nil {
			fmt.Fprintf(out, "cycle: created %s\n", p)
		}
	}
}

// verifySince returns the commit of the umbrella this cycle verifies the integrated changes after,
// and what that commit is: the one the latest earlier cycle that ended ran on, or, when no cycle
// has, the one that installed the kit (the first to add .foreman-kit/kit.lock). A first cycle that
// verified nothing left the lessons of every change integrated before it out of the memory for good.
func verifySince(ctx context.Context, wt string, st *store.Store, stamp string) (string, string) {
	if st != nil {
		if cycles, err := st.ListCycles(ctx, 50); err == nil {
			for _, c := range cycles {
				if c.Stamp == stamp || c.UmbrellaSHA == "" || c.FinishedAt == "" {
					continue
				}
				if _, err := gitx.RunGit(ctx, wt, "cat-file", "-e", c.UmbrellaSHA+"^{commit}"); err != nil {
					continue
				}
				return c.UmbrellaSHA, "the commit the cycle " + c.Stamp + " ran on"
			}
		}
	}
	out, err := gitx.RunGit(ctx, wt, "log", "--format=%H", "--diff-filter=A", "--", ".foreman-kit/kit.lock")
	if err != nil {
		return "", ""
	}
	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) == 0 {
		return "", ""
	}
	return lines[len(lines)-1], "the commit that installed the kit: no cycle ran before this one"
}
