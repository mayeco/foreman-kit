package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

const preflightAuthPattern = `Not logged in|authentication_error|Invalid API key|Please run /login|401 Unauthorized|No authentication information found|No models available|No API key for`

var (
	preflightAuthRe = regexp.MustCompile(`(?i)(` + preflightAuthPattern + `)`)
	ansiEscapeRe    = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

// PreflightOptions configures RunPreflight.
type PreflightOptions struct {
	Root         string
	ServicesOnly bool
	PingTimeout  time.Duration
	Store        *store.Store
	Stdout       io.Writer
	Stderr       io.Writer
}

// PreflightResult summarizes the outcome of RunPreflight.
type PreflightResult struct {
	OK             bool
	ExitCode       int
	WorkingEngines []string
	ServicesDown   []string
	JudgeStatus    string
	Output         string
}

type reqCheck struct {
	name string
	cmd  string
}

// extractRepoRequires returns the services the repository whose repo.config.yaml is at cfgPath needs
// running (required_services), each with the command that checks it.
func extractRepoRequires(cfgPath string) []reqCheck {
	raw, _, err := config.ReadRepoConfig("", filepath.Dir(cfgPath))
	if err != nil {
		return nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	rootMap := doc.Content[0]
	var out []reqCheck
	for i := 0; i+1 < len(rootMap.Content); i += 2 {
		if rootMap.Content[i].Value != "required_services" {
			continue
		}
		reqNode := rootMap.Content[i+1]
		switch reqNode.Kind {
		case yaml.MappingNode:
			for j := 0; j+1 < len(reqNode.Content); j += 2 {
				k := reqNode.Content[j].Value
				v := strings.TrimSpace(reqNode.Content[j+1].Value)
				if k != "" && v != "" {
					out = append(out, reqCheck{name: k, cmd: v})
				}
			}
		case yaml.SequenceNode:
			for idx, item := range reqNode.Content {
				v := strings.TrimSpace(item.Value)
				if v != "" {
					out = append(out, reqCheck{name: fmt.Sprintf("check-%d", idx+1), cmd: v})
				}
			}
		}
	}
	return out
}

func checkRequiredServices(ctx context.Context, root string, st *store.Store, out io.Writer) []string {
	matches, _ := filepath.Glob(filepath.Join(root, "repos", "*", "repo.config.yaml"))
	sort.Strings(matches)
	anyChecked := false
	var down []string

	for _, cfgPath := range matches {
		if !fileExists(cfgPath) {
			continue
		}
		rel, err := filepath.Rel(root, filepath.Dir(cfgPath))
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		checks := extractRepoRequires(cfgPath)
		for _, ch := range checks {
			anyChecked = true
			var buf bytes.Buffer
			res, _ := RepoExec(ctx, RepoExecOptions{
				Root:        root,
				Timeout:     30 * time.Second,
				GracePeriod: 10 * time.Second,
				Store:       st,
				Stdout:      &buf,
				Stderr:      &buf,
			}, filepath.Join(root, filepath.FromSlash(rel)), []string{"bash", "-c", ch.cmd})
			if res != nil && res.ExitCode == 0 {
				fmt.Fprintf(out, "  %s %s: OK\n", rel, ch.name)
			} else {
				lastLine := lastNonEmptyLine(buf.String(), 160)
				fmt.Fprintf(out, "  %s %s: DOWN (%s: %s)\n", rel, ch.name, ch.cmd, lastLine)
				down = append(down, fmt.Sprintf("%s:%s", rel, ch.name))
			}
		}
	}
	if !anyChecked {
		fmt.Fprintln(out, "  none required")
	}
	return down
}

// VerifyKitInstallation checks .foreman-kit/kit.lock and embedded skill assets in root.
func VerifyKitInstallation(root string, out io.Writer) bool {
	if out == nil {
		out = io.Discard
	}
	lockPath := filepath.Join(root, ".foreman-kit/kit.lock")
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		fmt.Fprintln(out, "kit: no .foreman-kit/kit.lock (not installed with install.sh); not verified")
		return true
	}
	var lock map[string]any
	if err := json.Unmarshal(raw, &lock); err != nil {
		fmt.Fprintf(out, "kit: invalid .foreman-kit/kit.lock: %v\n", err)
		return false
	}

	version := fmt.Sprint(lock["version"])
	edited := make(map[string]string)
	kitFileCount := 0

	embeddedSkills, _ := assets.WalkEmbedded("skills")

	if filesMap, ok := lock["files"].(map[string]any); ok {
		var rels []string
		for r := range filesMap {
			rels = append(rels, r)
		}
		sort.Strings(rels)
		for _, rel := range rels {
			fm, ok := filesMap[rel].(map[string]any)
			if !ok || fm["class"] != "kit" {
				continue
			}
			kitFileCount++
			wantSha, _ := fm["sha256"].(string)
			if wantSha == "" {
				// Fallback to embedded skill hash if sha256 omitted
				for _, skillDir := range config.SkillsDirs(config.SupportedOrder) {
					if strings.HasPrefix(rel, skillDir+"/") {
						sub := strings.TrimPrefix(rel, skillDir+"/")
						if embBytes, ok := embeddedSkills[sub]; ok {
							wantSha = assets.SHA256Bytes(embBytes)
						}
						break
					}
				}
			}
			full := filepath.Join(root, filepath.FromSlash(rel))
			b, err := os.ReadFile(full)
			if err != nil {
				edited[rel] = "missing"
			} else if wantSha != "" && assets.SHA256Bytes(b) != wantSha {
				edited[rel] = "modified"
			}
		}
	}

	if lockAgentsRaw, ok := lock["agents"].([]any); ok {
		var lockAgents []string
		for _, a := range lockAgentsRaw {
			lockAgents = append(lockAgents, fmt.Sprint(a))
		}
		if cfg := config.Load(root); cfg != nil && len(cfg.Agents) > 0 {
			if strings.Join(cfg.Agents, ",") != strings.Join(lockAgents, ",") {
				edited[".foreman-kit/agents.yaml"] = fmt.Sprintf("lists %s, skills installed for %s: run foreman-kit upgrade --agents ...",
					strings.Join(cfg.Agents, ", "), strings.Join(lockAgents, ", "))
			}
		}
	}

	if len(edited) == 0 {
		fmt.Fprintf(out, "kit: OK (v%s, %d kit files match .foreman-kit/kit.lock)\n", version, kitFileCount)
		return true
	}

	var keys []string
	for k := range edited {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s (%s)", k, edited[k]))
	}
	fmt.Fprintf(out, "kit: DRIFT from v%s: %s\n", version, strings.Join(parts, ", "))
	return false
}

// pingPrompt is what the preflight asks each agent.
const pingPrompt = "Reply with ONLY the word: pong"

// pingCommandSeconds is what {{timeout}} says in a ping command.
const pingCommandSeconds = 45

// ResolvePingArgs returns the executable and the arguments of the preflight's ping of engine: its
// ping command as commands.<engine> of .foreman-kit/agents.yaml writes it, or its run command when it has
// none. extra is what {{args}} becomes. An agent without commands is asked the way a CLI most often
// takes a prompt.
func ResolvePingArgs(root, engine string, extra []string) (string, []string, bool) {
	spec, _, known := config.CommandsFor(root, engine)
	if known {
		command := spec.Ping
		if command == nil {
			command = spec.Run
		}
		if command != nil {
			if binName, args, closeIn, err := ExpandCommand(spec, command, engine, pingPrompt, "", root, pingCommandSeconds, extra); err == nil {
				return binName, args, closeIn
			}
		}
	}
	args := append([]string{}, extra...)
	return engine, append(args, "-p", pingPrompt), false
}

// toolchain (git, openspec), kit drift against .foreman-kit/kit.lock and embedded assets,
// required child repo services, concurrent pings to selected agents, and the evaluator's offline check and canary.
func RunPreflight(ctx context.Context, opts PreflightOptions) (*PreflightResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var outBuf bytes.Buffer
	stdout := io.Writer(&outBuf)
	if opts.Stdout != nil {
		// The buffer comes first: io.MultiWriter stops at the first writer that fails.
		stdout = io.MultiWriter(&outBuf, opts.Stdout)
	}

	root := opts.Root
	if root == "" {
		root = config.AppRoot("")
	}

	if opts.ServicesOnly {
		fmt.Fprintln(stdout, "== Services")
		down := checkRequiredServices(ctx, root, opts.Store, stdout)
		if len(down) > 0 {
			fmt.Fprintf(stdout, "preflight: FAIL (a service a repository requires is down: %s; start it, the next run checks again)\n", strings.Join(down, " "))
			return &PreflightResult{
				OK:           false,
				ExitCode:     1,
				ServicesDown: down,
				Output:       outBuf.String(),
			}, nil
		}
		fmt.Fprintln(stdout, "preflight: OK (services)")
		return &PreflightResult{
			OK:       true,
			ExitCode: 0,
			Output:   outBuf.String(),
		}, nil
	}

	// 0. The configuration files: what is missing or cannot be used in each one. A process starts
	// no new work on a file it cannot use, so nothing below would run as the files say.
	fmt.Fprintln(stdout, "== Configuration")
	var unusableFiles []string
	for _, f := range settings.Files() {
		problems := settings.At(root).Problems(f.Name)
		if len(problems) == 0 {
			fmt.Fprintf(stdout, "  %s: ok\n", f.Path)
			continue
		}
		unusableFiles = append(unusableFiles, f.Path)
		// A file an older kit wrote lacks every key of today: one line says so. Of any other file,
		// the first problems; `foreman-kit config check` lists them all.
		const shown = 3
		for i, p := range problems {
			if p.Former {
				fmt.Fprintf(stdout, "  %s: FAILED (written for an older foreman-kit: foreman-kit update rewrites it)\n", f.Path)
				break
			}
			if i == shown {
				fmt.Fprintf(stdout, "  %s: FAILED (and %d more)\n", f.Path, len(problems)-shown)
				break
			}
			fmt.Fprintf(stdout, "  %s: FAILED (%s)\n", f.Path, p.Text)
		}
	}

	if older := config.RepoConfigsUnusable(root); older != "" {
		file, _, _ := strings.Cut(older, " ")
		unusableFiles = append(unusableFiles, file)
		fmt.Fprintf(stdout, "  %s: FAILED (written for an older foreman-kit: foreman-kit update rewrites it)\n", file)
	}

	// A variable an older foreman-kit read is no file's problem, and it keeps what read its file
	// from starting just the same: whoever exports it expects that it holds.
	var formerVariables []string
	for _, v := range settings.FormerVariables() {
		if strings.TrimSpace(os.Getenv(v.Name)) != "" {
			formerVariables = append(formerVariables, v.Name)
		}
	}
	for _, p := range settings.FormerVariablesSet() {
		fmt.Fprintf(stdout, "  the environment: FAILED (%s)\n", p.Text)
	}

	pingTimeout := opts.PingTimeout
	if pingTimeout <= 0 {
		pingTimeout = (&settings.Reader{Root: root, Stderr: io.Discard}).Duration(settings.PingTimeout)
	}

	// 1. Toolchain
	fmt.Fprintf(stdout, "== Toolchain (%s, %s %s)\n", time.Now().UTC().Format(time.RFC3339), runtime.GOOS, runtime.GOARCH)
	missingTool := false
	gitProblem := ""
	for _, tool := range []string{"git", "openspec", memory.Binary} {
		if tool == "git" {
			ver, path, err := gitx.Version(ctx)
			switch {
			case path == "":
				fmt.Fprintf(stdout, "  git: NOT FOUND\n")
				missingTool = true
			case err != nil:
				fmt.Fprintf(stdout, "  git: unreadable version (minimum %s) (%s)\n", gitx.MinVersion, path)
				gitProblem = fmt.Sprintf("git version at %s cannot be read (minimum %s)", path, gitx.MinVersion)
			default:
				fmt.Fprintf(stdout, "  git: %s (minimum %s) (%s)\n", ver, gitx.MinVersion, path)
				if !gitx.VersionAtLeast(ver, gitx.MinVersion) {
					gitProblem = fmt.Sprintf("git %s at %s is older than the minimum %s", ver, path, gitx.MinVersion)
				}
			}
			continue
		}
		if path, err := exec.LookPath(tool); err == nil {
			// agent-memory says its version with a command of its own.
			versionArg := "--version"
			if tool == memory.Binary {
				versionArg = "version"
			}
			verOut, _ := exec.Command(tool, versionArg).CombinedOutput()
			firstLine := strings.TrimSpace(strings.SplitN(string(verOut), "\n", 2)[0])
			if tool == memory.Binary {
				// Agents run it themselves, from the PATH they are started with, which is this one:
				// where it was found says whether it stays there (a directory of one version of a
				// toolchain goes away with that version).
				firstLine += " (" + path + ")"
			}
			fmt.Fprintf(stdout, "  %s: %s\n", tool, firstLine)
		} else {
			fmt.Fprintf(stdout, "  %s: NOT FOUND\n", tool)
			if tool == memory.Binary {
				fmt.Fprintf(stdout, "  install it, the agents read and write the memory with it:\n%s\n", memory.InstallHelp)
			}
			missingTool = true
		}
	}

	// 2. Kit drift
	fmt.Fprintln(stdout, "== Kit")
	var kitBuf bytes.Buffer
	kitOK := VerifyKitInstallation(root, &kitBuf)
	for _, line := range strings.Split(strings.TrimRight(kitBuf.String(), "\n"), "\n") {
		if line != "" {
			fmt.Fprintf(stdout, "  %s\n", line)
		}
	}

	// The memory: an application without one was installed before agent-memory kept it.
	fmt.Fprintln(stdout, "== Memory")
	memoryOK := memory.Present(root)
	if memoryOK {
		fmt.Fprintf(stdout, "  %s: OK (%d files)\n", memory.Dir, len(memory.Files(root)))
		if memory.LegacyPresent(root) {
			fmt.Fprintf(stdout, "  %s and %s/ are no longer read: `foreman-kit update` takes them into %s/\n", memory.LegacyFile, memory.LegacyDir, memory.Dir)
		}
	} else {
		fmt.Fprintf(stdout, "  %s: MISSING\n", memory.Dir)
	}

	// 3. Services
	fmt.Fprintln(stdout, "== Services")
	servicesDown := checkRequiredServices(ctx, root, opts.Store, stdout)

	// 4. Agent engines (concurrent pings)
	fmt.Fprintln(stdout, "== Agent engines")
	cfg := config.Load(root)
	selectedSet := make(map[string]bool)
	hasExplicitSelection := false
	if cfg != nil && len(cfg.Agents) > 0 {
		hasExplicitSelection = true
		for _, a := range cfg.Agents {
			selectedSet[a] = true
		}
	}

	var rolePairs, emptyRoles []string
	roleEnginesMap := make(map[string][]string)
	hasStubRole := false
	for _, role := range config.RolesOrder {
		engStr, _ := config.Engine(root, role)
		if engStr == "" {
			// A role without agents does not run: nothing is picked in its place.
			rolePairs = append(rolePairs, role+":none")
			emptyRoles = append(emptyRoles, role)
		} else {
			rolePairs = append(rolePairs, role+":"+engStr)
			list := config.Names(engStr)
			roleEnginesMap[role] = list
			for _, e := range list {
				if e == "stub" {
					hasStubRole = true
				}
				if hasExplicitSelection {
					selectedSet[e] = true
				}
			}
		}
	}

	var toProbe []string
	probedSet := make(map[string]bool)
	for _, name := range config.SupportedOrder {
		if !hasExplicitSelection || selectedSet[name] {
			toProbe = append(toProbe, name)
			probedSet[name] = true
		}
	}
	if cfg != nil {
		for _, name := range cfg.Agents {
			if name != "" && name != "stub" && !probedSet[name] {
				toProbe = append(toProbe, name)
				probedSet[name] = true
			}
		}
	}
	for _, list := range roleEnginesMap {
		for _, name := range list {
			if name != "" && name != "stub" && !probedSet[name] {
				toProbe = append(toProbe, name)
				probedSet[name] = true
			}
		}
	}

	type probeOutcome struct {
		name    string
		line    string
		working bool
	}
	outcomes := make([]probeOutcome, len(toProbe))
	var wg sync.WaitGroup
	env := BuildSlotEnv(os.Environ(), root, 0)

	for idx, name := range toProbe {
		wg.Add(1)
		go func(i int, eng string) {
			defer wg.Done()
			binToCheck := config.ExecutableBinary(root, eng)
			if _, err := exec.LookPath(binToCheck); err != nil {
				outcomes[i] = probeOutcome{
					name: eng,
					line: fmt.Sprintf("  %s: not installed", eng),
				}
				return
			}
			extra := config.ExtraArgsWithStderr(root, eng, io.Discard)
			binName, binArgs, closeIn := ResolvePingArgs(root, eng, extra)
			var stdinReader io.Reader
			if closeIn {
				stdinReader = bytes.NewReader(nil)
			}
			res, runErr := RunBounded(ctx, RunOptions{
				Dir:         root,
				Env:         env,
				Stdin:       stdinReader,
				Timeout:     pingTimeout,
				GracePeriod: 10 * time.Second,
			}, binName, binArgs...)

			rc := 1
			outText := ""
			if res != nil {
				rc = res.ExitCode
				outText = res.Output
			}
			if runErr != nil && strings.TrimSpace(outText) == "" {
				// The engine never ran: its reason is all there is to report.
				outText = runErr.Error()
			}

			if preflightAuthRe.MatchString(outText) {
				outcomes[i] = probeOutcome{
					name: eng,
					line: fmt.Sprintf("  %s: FAILED (authentication)", eng),
				}
			} else if rc != 0 && rc != 124 {
				cleanOut := ansiEscapeRe.ReplaceAllString(outText, "")
				last := lastNonEmptyLine(cleanOut, 160)
				outcomes[i] = probeOutcome{
					name: eng,
					line: fmt.Sprintf("  %s: FAILED (exit %d: %s)", eng, rc, last),
				}
			} else if rc == 0 && strings.TrimSpace(outText) != "" {
				outcomes[i] = probeOutcome{
					name:    eng,
					line:    fmt.Sprintf("  %s: OK", eng),
					working: true,
				}
			} else {
				outcomes[i] = probeOutcome{
					name: eng,
					line: fmt.Sprintf("  %s: WARN (empty response or timeout)", eng),
				}
			}
		}(idx, name)
	}
	wg.Wait()

	var working []string
	workingSet := make(map[string]bool)
	for _, o := range outcomes {
		if o.line != "" {
			fmt.Fprintln(stdout, o.line)
		}
		if o.working {
			working = append(working, o.name)
			workingSet[o.name] = true
		}
	}
	if hasStubRole && len(working) == 0 {
		working = append(working, "stub")
		workingSet["stub"] = true
	}

	var roleFails []string
	for _, role := range config.RolesOrder {
		list := roleEnginesMap[role]
		if len(list) == 0 {
			continue
		}
		okEng := ""
		var downEngs []string
		for _, eng := range list {
			if eng == "stub" || workingSet[eng] {
				if okEng == "" {
					okEng = eng
				}
			} else {
				downEngs = append(downEngs, eng)
			}
		}
		if okEng == "" {
			roleFails = append(roleFails, fmt.Sprintf("%s:%s", role, strings.Join(list, ",")))
		} else if len(downEngs) > 0 {
			fmt.Fprintf(stdout, "  %s: WARN runs on %s; not working in its list: %s\n", role, okEng, strings.Join(downEngs, " "))
		}
	}

	fmt.Fprintf(stdout, "  roles: %s\n", strings.Join(rolePairs, " "))
	fmt.Fprintf(stdout, "  selection: %s\n", config.SelectionTo(root, io.Discard))

	// 5. The evaluator (internal/evaluator)
	fmt.Fprintln(stdout, "== Evaluator")
	judgeStatus := checkEvaluatorPreflight(ctx, root, opts.Store, stdout)

	fmt.Fprintln(stdout, "========================================================================")
	fail := func(msg string) (*PreflightResult, error) {
		fmt.Fprintln(stdout, msg)
		return &PreflightResult{
			OK:             false,
			ExitCode:       1,
			WorkingEngines: working,
			ServicesDown:   servicesDown,
			JudgeStatus:    judgeStatus,
			Output:         outBuf.String(),
		}, nil
	}

	if missingTool {
		return fail("preflight: FAIL (toolchain incomplete)")
	}
	if gitProblem != "" {
		return fail(fmt.Sprintf("preflight: FAIL (%s)", gitProblem))
	}
	if !kitOK {
		return fail("preflight: FAIL (kit differs from .foreman-kit/kit.lock: upgrade or reinstall it)")
	}
	if !memoryOK {
		return fail(fmt.Sprintf("preflight: FAIL (the application has no %s/: foreman-kit update creates it with %s and takes in what %s and %s/ hold)", memory.Dir, memory.Binary, memory.LegacyFile, memory.LegacyDir))
	}
	if len(formerVariables) > 0 {
		return fail(fmt.Sprintf("preflight: FAIL (%s set in this environment and no longer read: what read them starts nothing until they are out of it; foreman-kit config check says what does the job of each one)", strings.Join(formerVariables, ", ")))
	}
	if len(unusableFiles) > 0 {
		return fail(fmt.Sprintf("preflight: FAIL (%s cannot be used as written: nothing new starts until it is mended; foreman-kit config check says what each one lacks, and foreman-kit update rewrites the files of an older kit)", strings.Join(unusableFiles, ", ")))
	}
	if len(servicesDown) > 0 {
		return fail(fmt.Sprintf("preflight: FAIL (a service a repository requires is down: %s; start it, the next run checks again)", strings.Join(servicesDown, " ")))
	}
	if len(working) == 0 {
		return fail("preflight: FAIL (no working AI engine)")
	}
	if len(emptyRoles) > 0 {
		return fail(fmt.Sprintf("preflight: FAIL (a role lists no agent in %s, so it does not run: %s)", settings.Agents.FilePath(), strings.Join(emptyRoles, " ")))
	}
	if len(roleFails) > 0 {
		return fail(fmt.Sprintf("preflight: FAIL (no engine of a role is working: %s)", strings.Join(roleFails, " ")))
	}
	if judgeStatus == "nokey" {
		return fail("preflight: FAIL (gates of .foreman-kit/evaluator.yaml are advisory or enforce but TYPESAFE_API_KEY is not set: export it or set every gate to mode: off)")
	}
	if judgeStatus == "invalid" {
		return fail("preflight: FAIL (.foreman-kit/evaluator.yaml or its questions cannot be used: see the evaluator lines above)")
	}
	// when_unavailable: stop means no cycle judges and no worker starts while the evaluator does
	// not answer. An evaluator that is off has nothing to answer.
	if judgeStatus != "ok" && judgeStatus != "off" {
		if pol, err := evaluator.LoadPolicy(root); err == nil && pol.StopsWhenUnavailable() {
			return fail(fmt.Sprintf("preflight: FAIL (%s is %s and the evaluator is %s)", settings.EvaluatorWhenUnavailable.Where(), settings.UnavailableStop, judgeStatus))
		}
	}

	fmt.Fprintf(stdout, "preflight: OK (engines: %s; roles: %s; judge: %s)\n",
		strings.Join(working, " "), strings.Join(rolePairs, " "), judgeStatus)

	return &PreflightResult{
		OK:             true,
		ExitCode:       0,
		WorkingEngines: working,
		ServicesDown:   servicesDown,
		JudgeStatus:    judgeStatus,
		Output:         outBuf.String(),
	}, nil
}

func checkEvaluatorPreflight(ctx context.Context, root string, st *store.Store, out io.Writer) string {
	if settings.EvaluatorOffForThisRun() {
		fmt.Fprintf(out, "  evaluator: off for this run (--set %s=false)\n", settings.EvaluatorEnabled)
		return "off"
	}

	judge, err := evaluator.NewJudge(root, st)
	if err != nil {
		fmt.Fprintf(out, "  evaluator: INVALID policy: %v\n", err)
		return "invalid"
	}
	if !judge.Policy().Enabled {
		fmt.Fprintf(out, "  evaluator: disabled (%s: false)\n", settings.EvaluatorEnabled.Where())
		return "off"
	}
	model := judge.Policy().EffectiveModel()

	// Validate all gate question sets offline
	gateNames := []string{
		"canary",
		"change_risk",
		"concurrent_work_loss",
		"failure_triage",
		"system_error_triage",
		"policy_violation_triage",
		"agent_outcome",
		"semantic_merge_conflict",
		"push_rejection",
		"plan_review",
		"proposal_review",
		"repair_progress",
		"scenario_coverage",
		"task_priority",
	}
	invalid := false
	for _, g := range gateNames {
		if _, _, err := evaluator.LoadGateTemplate(root, g, judge.Policy()); err != nil {
			fmt.Fprintf(out, "  evaluator: INVALID gate questions %s (%v)\n", g, err)
			invalid = true
		}
	}
	if invalid {
		return "invalid"
	}

	if err := judge.Policy().RequireAPIKeyForActiveGates(); err != nil {
		fmt.Fprintf(out, "  evaluator: FAILED (%v)\n", err)
		return "nokey"
	}

	if evaluator.ResolveAPIKey() == "" {
		fmt.Fprintln(out, "  evaluator: not configured (no TYPESAFE_API_KEY)")
		return "missing"
	}

	fmt.Fprintf(out, "  evaluator: native go-jev client, model %s\n", model)
	started := time.Now()
	evalRes, rc, evalErr := judge.EvaluateTemplate(ctx, "canary", nil, []string{
		"all_gates_passed >= 0.8",
		"reports_failure <= 0.2",
	}, false)
	latencyMS := time.Since(started).Milliseconds()

	summary := "unparseable response"
	if evalRes != nil {
		yesVal := evalRes.Answers["all_gates_passed"]["noul"]
		noVal := evalRes.Answers["reports_failure"]["noul"]
		summary = fmt.Sprintf("yes=%v no=%v answered_by=%s %dms", yesVal, noVal, evalRes.Model, latencyMS)
	}

	switch rc {
	case 0:
		fmt.Fprintf(out, "  evaluator: OK (canary %s)\n", summary)
		return "ok"
	case 10:
		fmt.Fprintf(out, "  evaluator: FAILED (canary answered wrong: %s; enforce gates must not be trusted)\n", summary)
		return "drift"
	case 3:
		fmt.Fprintln(out, "  evaluator: FAILED (authentication: run 'jev auth login' or export TYPESAFE_API_KEY)")
		return "auth"
	case 5, 6, 124:
		fmt.Fprintf(out, "  evaluator: UNAVAILABLE (rate limit, network or timeout; exit %d)\n", rc)
		return "unavailable"
	default:
		errText := ""
		if evalErr != nil {
			errText = evalErr.Error()
			if len(errText) > 240 {
				errText = errText[len(errText)-240:]
			}
		}
		fmt.Fprintf(out, "  evaluator: ERROR (exit %d: %s)\n", rc, errText)
		return "unavailable"
	}
}

// RunPreflightCLI implements `foreman-kit preflight [--services]`.
func RunPreflightCLI(ctx context.Context, root string, st *store.Store, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	servicesOnly := false
	for _, a := range args {
		if a == "--services" {
			servicesOnly = true
		} else {
			fmt.Fprintln(stderr, "Usage: foreman-kit preflight [--services]")
			return 2
		}
	}
	res, _ := RunPreflight(ctx, PreflightOptions{
		Root:         root,
		ServicesOnly: servicesOnly,
		Store:        st,
		Stdout:       stdout,
		Stderr:       stderr,
	})
	if res != nil {
		return res.ExitCode
	}
	return 1
}

func lastNonEmptyLine(text string, maxChars int) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != "" {
			if maxChars > 0 && len(trimmed) > maxChars {
				return trimmed[:maxChars]
			}
			return trimmed
		}
	}
	return ""
}
