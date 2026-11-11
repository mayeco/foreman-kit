package proc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

var (
	openspecChangePathRe = regexp.MustCompile(`^openspec/changes/([^/]+)/.*$`)
	digitsOnlyRe         = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// RepoExecOptions configures RepoExec.
type RepoExecOptions struct {
	Root        string
	Slot        int
	ExtraEnv    []string
	Timeout     time.Duration
	GracePeriod time.Duration
	Store       *store.Store
	ExecID      string
	// Stdin is what the command reads; nil gives it no input (/dev/null), as the gates and the
	// preflight checks run.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// GatesOptions configures RunGates.
type GatesOptions struct {
	Root       string
	Base       string
	All        bool
	Regression bool
	Repos      []string
	Slot       int
	Timeout    time.Duration
	Store      *store.Store
	ExecID     string
	Stdout     io.Writer
	Stderr     io.Writer
	// AcceptFailures, when set, is asked about the failed gates (labels "<repo>:<gate>", the child
	// paths by repo name, the base ref the run diffed against, and the run's output) and returns the
	// labels a human accepted on the repository's base (foreman-kit base accept) with a note for each:
	// they are reported as accepted and do not fail the run.
	AcceptFailures func(ctx context.Context, failed []string, childByName map[string]string, base, output string) (accepted map[string]string)
}

// GatesResult summarizes a RunGates execution.
type GatesResult struct {
	Passed   int
	Failed   []string
	Repos    []string
	ExitCode int
	Output   string
}

// BuildSlotEnv returns environment variables with PATH prefix and FOREMANKIT_SLOT / TEST_ENV_NUMBER / PORT applied.
func BuildSlotEnv(baseEnv []string, root string, slot int) []string {
	if len(baseEnv) == 0 {
		baseEnv = os.Environ()
	}
	envMap := make(map[string]string, len(baseEnv)+4)
	var order []string
	setVar := func(k, v string) {
		if _, exists := envMap[k]; !exists {
			order = append(order, k)
		}
		envMap[k] = v
	}
	for _, kv := range baseEnv {
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			setVar(kv[:idx], kv[idx+1:])
		}
	}

	prefix := envMap["FOREMANKIT_PATH_PREFIX"]
	if prefix == "" {
		home, _ := os.UserHomeDir()
		prefix = fmt.Sprintf("%s/.local/share/mise/shims:%s/.local/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/opt/homebrew/bin", home, home)
	}
	curPath := envMap["PATH"]
	binDir := filepath.Join(root, "bin")
	setVar("PATH", fmt.Sprintf("%s:%s:%s", prefix, binDir, curPath))

	if slot <= 0 {
		if slotStr := strings.TrimSpace(envMap["FOREMANKIT_SLOT"]); digitsOnlyRe.MatchString(slotStr) {
			slot, _ = strconv.Atoi(slotStr)
		}
	}
	if slot > 0 {
		slotStr := strconv.Itoa(slot)
		setVar("FOREMANKIT_SLOT", slotStr)
		// The scripts of a repository written before the kit was called Foreman read the OPENSPEC_ name.
		setVar("OPENSPEC_SLOT", slotStr)
		setVar("TEST_ENV_NUMBER", slotStr)
		setVar("PORT", strconv.Itoa(3000+slot*10))
	}
	// Guarantee OS-appropriate UTF-8 locale ("C.UTF-8" on Linux, "en_US.UTF-8" on macOS) and non-interactive git/CI variables
	utf8Loc := DefaultUTF8Locale()
	setVar("LANG", utf8Loc)
	setVar("LC_ALL", utf8Loc)
	setVar("CI", "1")
	setVar("NONINTERACTIVE", "1")
	setVar("GIT_TERMINAL_PROMPT", "0")
	setVar("GIT_MERGE_AUTOEDIT", "no")

	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+envMap[k])
	}
	return out
}

func resolveVisualQASlotAndRoot(ctx context.Context, root string, st *store.Store, curSlot int) (string, int) {
	if curSlot > 0 || os.Getenv("FOREMANKIT_SLOT") != "" {
		return root, curSlot
	}
	mainRoot := store.MainRoot(root)
	wtBase := settings.WorktreesBase(mainRoot)
	vqaWT := filepath.Join(wtBase, "_visual_qa")

	// Only redirect commands originating from mainRoot (or already inside _visual_qa);
	// commands inside other worktrees (.worktrees/_cycle, .worktrees/worker-*) stay in their own worktree.
	cleanRoot := filepath.Clean(root)
	if cleanRoot != filepath.Clean(mainRoot) && cleanRoot != filepath.Clean(vqaWT) {
		return root, curSlot
	}

	if st == nil {
		if opened, err := store.Open(mainRoot); err == nil {
			st = opened
			defer st.Close()
		}
	}

	// 1. Check SQLite store visual_qa lock (non-stale by TTL even after visual-qa begin PID exits)
	if st != nil {
		if rec, err := st.GetLock(ctx, "visual_qa"); err == nil && rec != nil {
			now := time.Now().UTC().Unix()
			hb := rec.HeartbeatAt
			if hb == 0 {
				hb = rec.AcquiredAt
			}
			expired := rec.TTLSeconds > 0 && (now-hb) >= int64(rec.TTLSeconds)
			if !expired && !store.IsLockStale(*rec, "", now) {
				if dirExists(vqaWT) {
					root = vqaWT
				}
				return root, rec.Slot
			}
		}
	}

	// 2. Check legacy log/visual_qa.lock directory
	lockDir := filepath.Join(mainRoot, "log", "visual_qa.lock")
	if dirExists(lockDir) {
		startedBytes, _ := os.ReadFile(filepath.Join(lockDir, "started"))
		ttlBytes, _ := os.ReadFile(filepath.Join(lockDir, "ttl"))
		started, _ := strconv.ParseInt(strings.TrimSpace(string(startedBytes)), 10, 64)
		ttl, _ := strconv.ParseInt(strings.TrimSpace(string(ttlBytes)), 10, 64)
		if time.Now().UTC().Unix()-started < ttl {
			slotBytes, _ := os.ReadFile(filepath.Join(lockDir, "slot"))
			slot, _ := strconv.Atoi(strings.TrimSpace(string(slotBytes)))
			if dirExists(vqaWT) {
				root = vqaWT
			}
			return root, slot
		}
	}
	return root, curSlot
}

// RepoExec runs a command in a child repository after sourcing its configured env_script (if any) via bash -c.
func RepoExec(ctx context.Context, opts RepoExecOptions, repoArg string, command []string) (*RunResult, error) {
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
	if repoArg == "" || len(command) == 0 {
		fmt.Fprintln(stderr, "usage: repo_exec.sh <repo> <command> [args...]")
		return &RunResult{ExitCode: 2}, fmt.Errorf("missing repo or command")
	}

	root := opts.Root
	if root == "" {
		root = config.AppRoot("")
	}
	origRoot := root
	root, slot := resolveVisualQASlotAndRoot(ctx, root, opts.Store, opts.Slot)

	var dir string
	if filepath.IsAbs(repoArg) {
		dir = repoArg
		if filepath.Clean(root) != filepath.Clean(origRoot) {
			if rel, err := filepath.Rel(origRoot, repoArg); err == nil && (rel == "repos" || strings.HasPrefix(rel, "repos"+string(filepath.Separator))) {
				dir = filepath.Join(root, rel)
			}
		}
	} else if strings.Contains(repoArg, "/") {
		dir = filepath.Join(root, filepath.FromSlash(repoArg))
	} else {
		dir = filepath.Join(root, "repos", repoArg)
	}

	if !dirExists(dir) {
		fmt.Fprintf(stderr, "repo_exec: no repository at %s\n", dir)
		return &RunResult{ExitCode: 2}, fmt.Errorf("no repository at %s", dir)
	}

	envScript := ""
	cfgPath := filepath.Join(dir, "repo.config.yaml")
	if fileExists(cfgPath) {
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			fmt.Fprintf(stderr, "repo_exec: %s is not valid YAML\n", cfgPath)
			return &RunResult{ExitCode: 2}, err
		}
		var cfg map[string]any
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			fmt.Fprintf(stderr, "repo_exec: %s is not valid YAML\n", cfgPath)
			return &RunResult{ExitCode: 2}, err
		}
		if es, ok := cfg["env_script"].(string); ok {
			envScript = strings.TrimSpace(es)
		}
	}

	if envScript != "" && !fileExists(filepath.Join(dir, filepath.FromSlash(envScript))) {
		fmt.Fprintf(stderr, "repo_exec: env_script %s of %s does not exist\n", envScript, cfgPath)
		return &RunResult{ExitCode: 2}, fmt.Errorf("env_script %s does not exist", envScript)
	}

	env := BuildSlotEnv(append(os.Environ(), opts.ExtraEnv...), root, slot)

	var execName string
	var execArgs []string
	if envScript != "" {
		// Source env_script in bash with set +u and FOREMANKIT_SLOT exported, then exec the target command.
		// The script is sourced with no input of its own: the stdin `foreman-kit exec` hands on is the
		// command's, and whatever the script, or a tool it runs, read from it was lost to the command.
		scriptCmd := `script="$1"; shift; set +u; . "./$script" </dev/null; exec "$@"`
		execName = "bash"
		execArgs = append([]string{"-c", scriptCmd, "_", envScript}, command...)
	} else {
		execName = command[0]
		execArgs = command[1:]
	}

	res, err := RunBounded(ctx, RunOptions{
		Dir:         dir,
		Env:         env,
		Stdin:       opts.Stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		Timeout:     opts.Timeout,
		GracePeriod: opts.GracePeriod,
		Store:       opts.Store,
		ExecID:      opts.ExecID,
	}, execName, execArgs...)
	if err != nil {
		// The command never ran (not on PATH, not executable, an argument or environment too large for
		// execve, such as a long FOREMANKIT_CHANGED_FILES). Without this line the caller sees an exit
		// code and nothing else.
		fmt.Fprintf(stderr, "repo_exec: cannot start the command: %v\n", err)
	}
	return res, err
}

// RunRepoExecCLI implements `foreman-kit exec <repo> <command> [args...]`.
func RunRepoExecCLI(ctx context.Context, root string, st *store.Store, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: repo_exec.sh <repo> <command> [args...]")
		return 2
	}
	res, _ := RepoExec(ctx, RepoExecOptions{
		Root:   root,
		Store:  st,
		Stdin:  forwardableStdin(os.Stdin),
		Stdout: stdout,
		Stderr: stderr,
	}, args[0], args[1:])
	if res != nil {
		return res.ExitCode
	}
	return 2
}

// forwardableStdin returns the stdin `foreman-kit exec` hands its command: a pipe or a file
// (`foreman-kit exec backend psql < seed.sql`), which the shell version inherited through
// exec "$@". A terminal is not handed on: RunBounded starts the command in its own process group,
// outside the terminal's foreground group, where a read from the terminal would stop it with SIGTTIN
// and leave exec waiting forever. The command then reads /dev/null, as it did before.
func forwardableStdin(f *os.File) io.Reader {
	if f == nil {
		return nil
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	return f
}

// MatchRegressionGlob evaluates whether relPath matches a regression.when glob pattern
// supporting *, ?, [...], {a,b}, and ** (matching 0 or more directory levels, including hidden files).
func MatchRegressionGlob(pattern, relPath string) bool {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	relPath = filepath.ToSlash(strings.TrimSpace(relPath))
	if pattern == "" || relPath == "" {
		return false
	}
	for _, expanded := range expandBraces(pattern) {
		if matchGlobSegments(strings.Split(expanded, "/"), strings.Split(relPath, "/")) {
			return true
		}
		if strings.HasSuffix(expanded, "/**") {
			alt := strings.TrimSuffix(expanded, "/**") + "/**/*"
			if matchGlobSegments(strings.Split(alt, "/"), strings.Split(relPath, "/")) {
				return true
			}
		}
	}
	return false
}

func expandBraces(pat string) []string {
	openIdx := strings.IndexByte(pat, '{')
	if openIdx < 0 {
		return []string{pat}
	}
	closeIdx := strings.IndexByte(pat[openIdx:], '}')
	if closeIdx < 0 {
		return []string{pat}
	}
	closeIdx += openIdx
	prefix := pat[:openIdx]
	suffix := pat[closeIdx+1:]
	options := strings.Split(pat[openIdx+1:closeIdx], ",")
	var out []string
	for _, opt := range options {
		out = append(out, expandBraces(prefix+opt+suffix)...)
	}
	return out
}

func matchGlobSegments(patSegs, pathSegs []string) bool {
	if len(patSegs) == 0 {
		return len(pathSegs) == 0
	}
	if patSegs[0] == "**" {
		// Collapse consecutive **
		for len(patSegs) > 1 && patSegs[1] == "**" {
			patSegs = patSegs[1:]
		}
		if len(patSegs) == 1 {
			return true
		}
		for i := 0; i <= len(pathSegs); i++ {
			if matchGlobSegments(patSegs[1:], pathSegs[i:]) {
				return true
			}
		}
		return false
	}
	if len(pathSegs) == 0 {
		return false
	}
	matched, err := filepath.Match(patSegs[0], pathSegs[0])
	if err != nil || !matched {
		return false
	}
	return matchGlobSegments(patSegs[1:], pathSegs[1:])
}

type orderedGate struct {
	name string
	cmd  string
}

// RepoGate is one named command of a child repository's repo.config.yaml.
type RepoGate struct {
	Name string
	Cmd  string
}

// RegressionSweepGates returns, in order, what the regression sweep (foreman-kit gates --regression)
// runs for a repository whose repo.config.yaml is raw: its setup command, if any, then its
// regression commands in file order. A repository without a regression command runs nothing.
// Un-instantiated {{...}} commands are left out (the sweep skips them with a warning).
func RegressionSweepGates(raw []byte) ([]RepoGate, error) {
	list, err := parseRepoGatesConfig(raw, true, nil, "", nil)
	if err != nil {
		return nil, err
	}
	var out []RepoGate
	for _, g := range list {
		if !strings.Contains(g.cmd, "{{") {
			out = append(out, RepoGate{Name: g.name, Cmd: g.cmd})
		}
	}
	return out, nil
}

func parseRepoGatesConfig(raw []byte, regressionSweep bool, changedFiles []string, repoName string, stderr io.Writer) ([]orderedGate, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}

	// A file written for an older foreman-kit is not read as one of today: its keys said other
	// things (gates.setup was a gate, an empty regression ran the gates).
	if older := config.RepoOfAnOlderKit(cfg); older != "" {
		return nil, fmt.Errorf("%s %s", config.RepoConfigFile, older)
	}

	gatesOrder, gatesMap := extractOrderedStringMap(&doc, "gates")
	regOrder, regMap := extractOrderedStringMap(&doc, "regression", "commands")
	// setup is not a gate: it prepares the environment, before the gates and before the regression.
	setupCmd, _ := cfg["setup"].(string)

	usableRegOrder := make([]string, 0, len(regOrder))
	usableRegMap := make(map[string]string)
	for _, k := range regOrder {
		v := strings.TrimSpace(regMap[k])
		if v != "" && !strings.Contains(v, "{{") {
			usableRegOrder = append(usableRegOrder, k)
			usableRegMap[k] = regMap[k]
		}
	}

	effectiveMap := make(map[string]string)
	var extraOrder []string

	if regressionSweep {
		// The sweep runs the regression commands, after setup. A repository that has none runs
		// nothing: its gates do not stand in for them.
		if len(usableRegOrder) == 0 {
			return nil, nil
		}
		effectiveMap["setup"] = setupCmd
		for _, k := range usableRegOrder {
			effectiveMap[k] = usableRegMap[k]
			extraOrder = append(extraOrder, k)
		}
	} else {
		effectiveMap["setup"] = setupCmd
		for _, k := range gatesOrder {
			effectiveMap[k] = gatesMap[k]
			extraOrder = append(extraOrder, k)
		}
		if len(usableRegOrder) > 0 {
			var globs []string
			regression, _ := cfg["regression"].(map[string]any)
			if rawGlobs, ok := regression["when"].([]any); ok {
				for _, g := range rawGlobs {
					if gs, ok := g.(string); ok && strings.TrimSpace(gs) != "" {
						globs = append(globs, strings.TrimSpace(gs))
					}
				}
			}
			match := MatchRegressionWhen(globs, changedFiles)
			if note := match.ExcludedNote(repoName); note != "" && stderr != nil {
				fmt.Fprintln(stderr, note)
			}
			if match.Triggered() {
				if stderr != nil {
					fmt.Fprintf(stderr, "---- [info] %s: %s matches regression.when: the regression runs after the gates\n", repoName, match.Hit)
				}
				for _, k := range usableRegOrder {
					rk := "regression:" + k
					effectiveMap[rk] = usableRegMap[k]
					extraOrder = append(extraOrder, rk)
				}
			}
		}
	}

	standard := []string{"setup", "lint", "typecheck", "test", "build"}
	seen := make(map[string]bool)
	var finalKeys []string
	for _, k := range standard {
		seen[k] = true
		finalKeys = append(finalKeys, k)
	}
	for _, k := range extraOrder {
		if !seen[k] {
			seen[k] = true
			finalKeys = append(finalKeys, k)
		}
	}

	var out []orderedGate
	for _, k := range finalKeys {
		v := effectiveMap[k]
		if strings.TrimSpace(v) != "" {
			out = append(out, orderedGate{name: k, cmd: v})
		}
	}
	return out, nil
}

// extractOrderedStringMap returns, in file order, the entries of the mapping the YAML document holds
// under path (a top-level key, or a key inside sections) whose values are text.
func extractOrderedStringMap(doc *yaml.Node, path ...string) ([]string, map[string]string) {
	outMap := make(map[string]string)
	var order []string
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return order, outMap
	}
	sec := doc.Content[0]
	for _, key := range path {
		var next *yaml.Node
		for i := 0; i+1 < len(sec.Content); i += 2 {
			if sec.Content[i].Value == key && sec.Content[i+1].Kind == yaml.MappingNode {
				next = sec.Content[i+1]
				break
			}
		}
		if next == nil {
			return order, outMap
		}
		sec = next
	}
	for j := 0; j+1 < len(sec.Content); j += 2 {
		k := sec.Content[j].Value
		vn := sec.Content[j+1]
		if vn.Kind == yaml.ScalarNode && vn.Tag != "!!null" {
			order = append(order, k)
			outMap[k] = vn.Value
		}
	}
	return order, outMap
}

func changedPathsInUmbrella(ctx context.Context, root, base string) []string {
	if base == "" {
		base = "HEAD"
	}
	seen := make(map[string]bool)
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}

	// -z: paths unquoted (plain output C-quotes non-ASCII names); --no-renames: both sides of a rename,
	// as in gitx.ChangedPaths, which reads the working tree's state below; --ignore-submodules=none: a
	// child whose gitlink moved is listed even when .gitmodules says "ignore = all" for it, or the gates
	// select no repository for a change that touched it.
	if diffOut, err := gitx.RunGit(ctx, root, "diff", "--name-only", "-z", "--no-renames", "--ignore-submodules=none", base, "--"); err == nil {
		for _, p := range strings.Split(diffOut, "\x00") {
			add(p)
		}
	}
	if paths, err := gitx.ChangedPaths(ctx, root, ""); err == nil {
		for _, p := range paths {
			add(p)
		}
	}
	sort.Strings(out)
	return out
}

func changedFilesInChildRepo(ctx context.Context, umbrellaRoot, childDir, repoName, base string) []string {
	if base == "" {
		base = "HEAD"
	}
	var childBase string
	if uRepo, err := gitx.Open(umbrellaRoot); err == nil {
		childBase, _ = uRepo.TreeEntryHash(base, "repos/"+repoName)
	}
	if childBase == "" {
		if out, err := gitx.RunGit(ctx, umbrellaRoot, "rev-parse", "--verify", "--quiet", fmt.Sprintf("%s:repos/%s", base, repoName)); err == nil {
			childBase = strings.TrimSpace(out)
		}
	}

	seen := make(map[string]bool)
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}

	// These are FOREMANKIT_CHANGED_FILES and what regression.when matches. -z: paths unquoted (plain
	// output C-quotes non-ASCII names, which then match no glob); --no-renames: both sides of a rename,
	// so a file moved out of a regression.when path still counts.
	if childBase != "" {
		if diffOut, err := gitx.RunGit(ctx, childDir, "diff", "--name-only", "-z", "--no-renames", childBase, "--"); err == nil {
			for _, p := range strings.Split(diffOut, "\x00") {
				add(p)
			}
		}
	}
	if lsOut, err := gitx.RunGit(ctx, childDir, "ls-files", "-z", "--others", "--exclude-standard"); err == nil {
		for _, p := range strings.Split(lsOut, "\x00") {
			add(p)
		}
	}
	sort.Strings(out)
	return out
}

// RunGates executes diff-aware quality gates across the OpenSpec layer and child repositories.
func RunGates(ctx context.Context, opts GatesOptions) (*GatesResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var outBuf bytes.Buffer
	stdout := io.Writer(&outBuf)
	if opts.Stdout != nil {
		// The buffer comes first: io.MultiWriter stops at the first writer that fails, and a caller's
		// writer whose reader went away must not cut the output the result carries.
		stdout = io.MultiWriter(&outBuf, opts.Stdout)
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	root := opts.Root
	if root == "" {
		root = config.AppRoot("")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = settings.Duration(root, settings.GateTimeout)
	}

	var childPaths []string
	if uRepo, err := gitx.Open(root); err == nil {
		childPaths, _ = uRepo.ChildPaths()
	}

	changed := changedPathsInUmbrella(ctx, root, opts.Base)

	var repos []string
	if opts.All {
		for _, p := range childPaths {
			repos = append(repos, filepath.Base(p))
		}
	} else if len(opts.Repos) > 0 {
		repos = append(repos, opts.Repos...)
	} else {
		for _, p := range childPaths {
			for _, ch := range changed {
				if ch == p || strings.HasPrefix(ch, p+"/") {
					repos = append(repos, filepath.Base(p))
					break
				}
			}
		}
	}

	passed := 0
	var failed []string

	// The gates of one call are one round: it opens with the first gate and closes when the call ends.
	var roundID int64
	defer func() {
		if roundID == 0 {
			return
		}
		result := "passed"
		if ctx.Err() != nil {
			result = "stopped"
		} else if len(failed) > 0 {
			result = "failed"
		}
		_ = opts.Store.FinishGateRound(context.WithoutCancel(ctx), roundID, result)
	}()

	runOneGate := func(label, dir, cmd string, extraEnv []string) {
		started := time.Now()
		if opts.Store != nil && roundID == 0 {
			roundID, _ = opts.Store.BeginGateRound(context.WithoutCancel(ctx), store.GateRound{
				Context:    "cli",
				ChangeName: os.Getenv("FOREMANKIT_CHANGE"),
				Slot:       opts.Slot,
				BaseRef:    opts.Base,
			})
		}
		fmt.Fprintf(stdout, "---- [gate] %s: %s\n", label, cmd)
		var res *RunResult
		if dir == root {
			env := BuildSlotEnv(append(os.Environ(), extraEnv...), root, opts.Slot)
			var err error
			res, err = RunBounded(ctx, RunOptions{
				Dir:     dir,
				Env:     env,
				Stdout:  stdout,
				Stderr:  stdout,
				Timeout: timeout,
				Store:   opts.Store,
				ExecID:  opts.ExecID,
			}, "bash", "-c", cmd)
			if err != nil {
				fmt.Fprintf(stdout, "repo_exec: cannot start the command: %v\n", err)
			}
		} else {
			res, _ = RepoExec(ctx, RepoExecOptions{
				Root:     root,
				Slot:     opts.Slot,
				ExtraEnv: extraEnv,
				Timeout:  timeout,
				Store:    opts.Store,
				ExecID:   opts.ExecID,
				Stdout:   stdout,
				Stderr:   stdout,
			}, dir, []string{"bash", "-c", cmd})
		}
		rc := 1
		rawOut := ""
		if res != nil {
			rc = res.ExitCode
			rawOut = res.Output
		}
		if opts.Store != nil {
			repoPart := "openspec"
			gatePart := label
			if parts := strings.SplitN(label, ":", 2); len(parts) == 2 {
				repoPart = parts[0]
				gatePart = parts[1]
			}
			repoPath := store.UmbrellaRepoPath
			if dir != root {
				if rel, err := filepath.Rel(root, dir); err == nil {
					repoPath = filepath.ToSlash(rel)
				}
			}
			excerpt := ""
			if rc != 0 {
				lines := strings.Split(strings.TrimRight(rawOut, "\n"), "\n")
				if len(lines) > 40 {
					lines = lines[len(lines)-40:]
				}
				excerpt = strings.Join(lines, "\n")
			}
			// Recorded also when ctx ended meanwhile (Ctrl-C): the gate that was interrupted is the
			// one whose run is worth keeping.
			_ = opts.Store.RecordGateRun(context.WithoutCancel(ctx), store.GateRunRecord{
				GateRoundID:    roundID,
				Context:        "cli",
				RepoName:       repoPart,
				RepoPath:       repoPath,
				GateName:       gatePart,
				Command:        cmd,
				Slot:           opts.Slot,
				ExitCode:       rc,
				DurationMS:     time.Since(started).Milliseconds(),
				FailureExcerpt: excerpt,
			})
		}
		if rc == 0 {
			elapsed := int(time.Since(started).Seconds())
			fmt.Fprintf(stdout, "---- [pass] %s (%ds)\n", label, elapsed)
			passed++
		} else {
			fmt.Fprintf(stdout, "---- [FAIL] %s (exit %d)\n", label, rc)
			failed = append(failed, label)
		}
	}

	// 1. OpenSpec layer
	openspecTouched := opts.All
	if !openspecTouched {
		for _, ch := range changed {
			if strings.HasPrefix(ch, "openspec/") {
				openspecTouched = true
				break
			}
		}
	}
	if openspecTouched {
		runOneGate("openspec:specs", root, `out="$(openspec validate --specs --strict --json 2>&1)" || { printf "%s\n" "$out"; exit 1; }`, nil)
		seenChanges := make(map[string]bool)
		var activeChanges []string
		for _, ch := range changed {
			if m := openspecChangePathRe.FindStringSubmatch(ch); len(m) == 2 && m[1] != "archive" && !seenChanges[m[1]] {
				seenChanges[m[1]] = true
				activeChanges = append(activeChanges, m[1])
			}
		}
		sort.Strings(activeChanges)
		for _, change := range activeChanges {
			if dirExists(filepath.Join(root, "openspec", "changes", change)) {
				cmd := fmt.Sprintf(`out="$(openspec validate '%s' --strict --json 2>&1)" || { printf '%%s\n' "$out"; exit 1; }`, change)
				runOneGate("openspec:"+change, root, cmd, nil)
			}
		}
	}

	// 2. Child repositories (sorted and deduplicated)
	uniqueReposMap := make(map[string]bool)
	var sortedRepos []string
	for _, r := range repos {
		if r != "" && !uniqueReposMap[r] {
			uniqueReposMap[r] = true
			sortedRepos = append(sortedRepos, r)
		}
	}
	sort.Strings(sortedRepos)

	for _, name := range sortedRepos {
		dir := filepath.Join(root, "repos", name)
		cfgFile := filepath.Join(dir, "repo.config.yaml")
		if !fileExists(cfgFile) {
			fmt.Fprintf(stdout, "---- [warn] repos/%s: no repo.config.yaml, no gates defined\n", name)
			continue
		}
		// A checkout cut before the keys of the file were renamed reads the file of the main
		// checkout, where foreman-kit update rewrote it.
		rawCfg, _, err := config.ReadRepoConfig("", dir)
		if err != nil {
			fmt.Fprintf(stdout, "---- [FAIL] repos/%s: %v\n", name, err)
			failed = append(failed, name+":config")
			continue
		}
		childChanged := changedFilesInChildRepo(ctx, root, dir, name, opts.Base)
		// OPENSPEC_CHANGED_FILES: the name a script written before the kit was called Foreman reads.
		extraEnv := []string{"FOREMANKIT_CHANGED_FILES=" + strings.Join(childChanged, "\n"), "OPENSPEC_CHANGED_FILES=" + strings.Join(childChanged, "\n")}

		gatesList, err := parseRepoGatesConfig(rawCfg, opts.Regression, childChanged, name, stderr)
		if err != nil {
			fmt.Fprintf(stdout, "---- [FAIL] repos/%s: %v\n", name, err)
			failed = append(failed, name+":config")
			continue
		}
		if opts.Regression && len(gatesList) == 0 {
			fmt.Fprintf(stdout, "---- [info] repos/%s: no regression command (regression.commands of its %s): the sweep runs nothing here\n", name, config.RepoConfigFile)
		}
		for _, g := range gatesList {
			if strings.Contains(g.cmd, "{{") {
				fmt.Fprintf(stdout, "---- [warn] repos/%s:%s not instantiated (%s); skipped\n", name, g.name, g.cmd)
			} else {
				runOneGate(fmt.Sprintf("%s:%s", name, g.name), dir, g.cmd, extraEnv)
			}
		}
	}

	if len(failed) > 0 && opts.AcceptFailures != nil {
		childByName := make(map[string]string, len(childPaths))
		for _, p := range childPaths {
			childByName[filepath.Base(p)] = p
		}
		base := opts.Base
		if base == "" {
			base = "HEAD"
		}
		if accepted := opts.AcceptFailures(ctx, failed, childByName, base, outBuf.String()); len(accepted) > 0 {
			var still []string
			for _, label := range failed {
				if note, ok := accepted[label]; ok {
					fmt.Fprintf(stdout, "---- [accepted] %s: %s\n", label, note)
					passed++
					continue
				}
				still = append(still, label)
			}
			failed = still
		}
	}

	fmt.Fprintln(stdout, "========================================================================")
	exitCode := 0
	if len(failed) == 0 {
		reposLabel := "none"
		if len(repos) > 0 {
			reposLabel = strings.Join(repos, " ")
		}
		fmt.Fprintf(stdout, "gates: PASS (%d executed; repos: %s)\n", passed, reposLabel)
	} else {
		fmt.Fprintf(stdout, "gates: FAIL (%s)\n", strings.Join(failed, " "))
		exitCode = 1
	}

	return &GatesResult{
		Passed:   passed,
		Failed:   failed,
		Repos:    sortedRepos,
		ExitCode: exitCode,
		Output:   outBuf.String(),
	}, nil
}

// RunGatesCLI implements `foreman-kit gates [--all] [--regression] [--base <ref>] [--repo <name>]`.
func RunGatesCLI(ctx context.Context, root string, st *store.Store, args []string, stdout, stderr io.Writer) int {
	return RunGatesCLIWith(ctx, root, st, args, stdout, stderr, nil)
}

// RunGatesCLIWith is RunGatesCLI with GatesOptions.AcceptFailures set to accept.
func RunGatesCLIWith(ctx context.Context, root string, st *store.Store, args []string, stdout, stderr io.Writer, accept func(ctx context.Context, failed []string, childByName map[string]string, base, output string) map[string]string) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	opts := GatesOptions{
		Root:           root,
		Store:          st,
		Stdout:         stdout,
		Stderr:         stderr,
		AcceptFailures: accept,
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--all":
			opts.All = true
		case "--regression":
			opts.Regression = true
		case "--base":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "run_gates: --base needs a ref")
				return 2
			}
			i++
			opts.Base = args[i]
		case "--repo":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "run_gates: --repo needs a name")
				return 2
			}
			i++
			opts.Repos = append(opts.Repos, args[i])
		case "-h", "--help":
			fmt.Fprintln(stdout, "Usage: foreman-kit gates [--all] [--regression] [--base <ref>] [--repo <name>]")
			return 0
		default:
			fmt.Fprintf(stderr, "run_gates: unknown argument %s\n", args[i])
			return 2
		}
	}

	res, _ := RunGates(ctx, opts)
	if res != nil {
		return res.ExitCode
	}
	return 1
}
