package cycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

var (
	sinceMinutesRe  = regexp.MustCompile(`^(\d+)m$`)
	sinceHoursRe    = regexp.MustCompile(`^(\d+)h$`)
	sinceDaysRe     = regexp.MustCompile(`^(\d+)d$`)
	kitStampRe      = regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z?$`)
	roadmapBulletRe = regexp.MustCompile(`(?m)^\s*(?:\d+\.|[-*])\s+\S`)
	checkedTaskRe   = regexp.MustCompile(`(?m)^\s*[-*+]\s+\[[xX]\]`)
)

// ResolveBaseBranch returns the base branch of the application at root: repository.base_branch of
// .foreman-kit/pipeline.yaml. It is "" when the file does not say: the key has no value of the kit's own,
// and nothing that fetches, rebases or pushes may run against a branch nobody named.
func ResolveBaseBranch(root string) string {
	return settings.String(root, settings.BaseBranch)
}

// RequireBaseBranch returns the base branch of the application at root, or why there is none.
func RequireBaseBranch(root string) (string, error) {
	if branch := ResolveBaseBranch(root); branch != "" {
		return branch, nil
	}
	return "", fmt.Errorf("%s is missing or not a branch name: set it to the branch every change starts from (foreman-kit config set %s <branch>)", settings.BaseBranch.Where(), settings.BaseBranch)
}

// RequireUsable returns why a process of the application at root that reads the configuration files
// called names starts no new work, or nil when it can: the application names no base branch, or one
// of the files lacks a key, holds a value the key does not accept, or writes a key nothing reads
// (settings.Reader.Unusable). A process does not work with a value nobody wrote: it says which key
// keeps it and runs again when the file is mended.
func RequireUsable(root string, names ...string) error {
	if _, err := RequireBaseBranch(root); err != nil {
		return err
	}
	if why := settings.Unusable(root, names...); why != "" {
		return errors.New(why + ": nothing new starts until it is mended")
	}
	if why := config.RepoConfigsUnusable(root); why != "" {
		return errors.New(why + ": nothing new starts until it is mended")
	}
	return nil
}

// ParseSince parses a relative period ("90m", "24h", "7d") or an RFC3339/ISO-8601 timestamp.
func ParseSince(text string, now time.Time) (time.Time, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "24h"
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if m := sinceMinutesRe.FindStringSubmatch(text); len(m) == 2 {
		n, _ := strconv.Atoi(m[1])
		return now.Add(-time.Duration(n) * time.Minute), nil
	}
	if m := sinceHoursRe.FindStringSubmatch(text); len(m) == 2 {
		n, _ := strconv.Atoi(m[1])
		return now.Add(-time.Duration(n) * time.Hour), nil
	}
	if m := sinceDaysRe.FindStringSubmatch(text); len(m) == 2 {
		n, _ := strconv.Atoi(m[1])
		return now.Add(-time.Duration(n) * 24 * time.Hour), nil
	}
	for _, layout := range []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UTC(), nil
		}
	}
	if t, err := ParseStampTime(text); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since takes 90m, 24h, 7d or a time, not %q", text)
}

// ParseStampTime parses a kit UTC stamp ("20261023T170001Z") into a time.Time.
func ParseStampTime(stamp string) (time.Time, error) {
	m := kitStampRe.FindStringSubmatch(strings.TrimSpace(stamp))
	if len(m) != 7 {
		return time.Time{}, fmt.Errorf("not a stamp: %q", stamp)
	}
	yr, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	dy, _ := strconv.Atoi(m[3])
	hr, _ := strconv.Atoi(m[4])
	mn, _ := strconv.Atoi(m[5])
	sc, _ := strconv.Atoi(m[6])
	return time.Date(yr, time.Month(mo), dy, hr, mn, sc, 0, time.UTC), nil
}

// RetryNote computes a human-readable retry note for a quarantined change.
func RetryNote(root string, q store.QuarantineRecord) string {
	pipeline := settings.At(root)
	maxRetries := pipeline.Int(settings.BlockedRetries)
	retryAfter := pipeline.Duration(settings.BlockedRetryAfter)
	if maxRetries <= 0 {
		return "automatic retries disabled"
	}
	retryable := map[string]bool{
		"gate_failure":            true,
		"incomplete_tasks":        true,
		"spec_invalid":            true,
		"plan_review":             true,
		"scenario_coverage":       true,
		"repair_stalled":          true,
		"rebase_conflict":         true,
		"rebase_failed":           true,
		"push_failed":             true,
		"manifest_stale":          true,
		"transient_error":         true,
		"environment_hold":        true,
		"environment_unavailable": true,
	}
	if !retryable[q.Reason] {
		return "needs a human"
	}
	if q.Retries >= maxRetries {
		return fmt.Sprintf("used all %d automatic retries", maxRetries)
	}
	if t, err := time.Parse(time.RFC3339, q.BlockedAt); err == nil {
		due := t.Add(retryAfter).UTC().Format(time.RFC3339)
		return fmt.Sprintf("retry %d of %d due at %s", q.Retries+1, maxRetries, due)
	}
	return fmt.Sprintf("retry %d of %d pending", q.Retries+1, maxRetries)
}

// idleAgentLine says of an agent that is idle now what it works on, for how long it has shown no
// activity and when the idle watchdog stops it.
func idleAgentLine(a store.IdleAgent) string {
	what := a.ChangeName
	switch {
	case what != "" && a.Phase != "":
		what += " (" + a.Phase + ")"
	case what == "" && a.CycleStamp != "":
		what = "cycle " + a.CycleStamp
	case what == "":
		what = a.ExecID
	}
	span := func(sec int) string {
		if sec < 120 {
			return fmt.Sprintf("%ds", sec)
		}
		return fmt.Sprintf("%d min", sec/60)
	}
	return fmt.Sprintf("%s on %s: no activity for %s, stopped at %s (%s)",
		a.Engine, what, span(a.IdleSeconds), span(a.LimitSeconds), settings.IdleTimeout.Path())
}

// Status writes the instant snapshot of guards, processes, queues, worktrees, and repositories (bin/cycle_status.sh).
func Status(ctx context.Context, root string, st *store.Store, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	nowUnix := time.Now().UTC().Unix()
	var ws store.WorkspaceState
	if st != nil {
		ws, _ = st.GetWorkspaceState(ctx)
	}

	paused := ws.Paused
	pauseReason := ws.PauseReason
	if !paused && fileExists(store.RuntimePath(root, "PAUSE")) {
		paused = true
		b, _ := os.ReadFile(store.RuntimePath(root, "PAUSE"))
		pauseReason = strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	}

	fmt.Fprintln(out, "== Guards")
	if paused {
		fmt.Fprintf(out, "PAUSED: YES (%s)\n", pauseReason)
	} else {
		fmt.Fprintln(out, "PAUSED: NO")
	}
	// A file that lacks a key keeps whatever reads it from starting new work: it is said here, with
	// the pause, because it stops the pipeline the same way.
	var every []string
	for _, f := range settings.Files() {
		every = append(every, f.Name)
	}
	why := settings.Unusable(root, every...)
	if why == "" {
		why = config.RepoConfigsUnusable(root)
	}
	if why != "" {
		fmt.Fprintf(out, "CONFIG: %s; what reads that file starts nothing new until it is mended\n", why)
	} else {
		fmt.Fprintln(out, "CONFIG: OK")
	}

	for _, lockName := range []string{"cycle", "dispatch", "visual_qa"} {
		label := strings.ToUpper(strings.ReplaceAll(lockName, "_", " ")) + " LOCK"
		var rec *store.LockRecord
		if st != nil {
			rec, _ = st.GetLock(ctx, lockName)
		}
		if rec != nil {
			mins := (nowUnix - rec.AcquiredAt) / 60
			if mins < 0 {
				mins = 0
			}
			fmt.Fprintf(out, "%s: pid %d (%d min old)\n", label, rec.PID, mins)
		} else {
			fmt.Fprintf(out, "%s: none\n", label)
		}
	}

	agentsCfg := LoadAgentsConfig(root)
	agentsList := strings.Join(agentsCfg.Agents, " ")
	if agentsList == "" {
		agentsList = "none selected (no .foreman-kit/agents.yaml)"
	}
	var roleParts []string
	for _, r := range []string{"plan", "build", "cycle"} {
		// The agents a call of this run uses: the ones --set gave it, else the file's.
		engs := RoleEngines(root, r)
		engStr := "none"
		if len(engs) > 0 {
			engStr = strings.Join(engs, ">")
		}
		roleParts = append(roleParts, fmt.Sprintf("%s: %s", r, engStr))
	}
	fmt.Fprintf(out, "AGENTS: %s (%s; selection: %s; permissions: %s)\n",
		agentsList, strings.Join(roleParts, ", "), agentsCfg.Order, agentsCfg.Permissions)

	if st != nil {
		if inUse, err := st.AgentsInUse(ctx); err == nil && len(inUse) > 0 {
			var keys []string
			for k := range inUse {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var parts []string
			for _, k := range keys {
				if inUse[k] > 1 {
					parts = append(parts, fmt.Sprintf("%s %d", k, inUse[k]))
				} else {
					parts = append(parts, k)
				}
			}
			fmt.Fprintf(out, "AGENTS IN USE: %s\n", strings.Join(parts, ", "))
		}
		if idle, err := st.ListIdleAgents(ctx); err == nil {
			// The store leaves out an agent whose orchestrator died: nothing watches it any more.
			for _, a := range idle {
				fmt.Fprintf(out, "AGENT IDLE: %s\n", idleAgentLine(a))
			}
		}
	}

	cycleStateRaw := ws.CycleStateJSON
	if (cycleStateRaw == "" || cycleStateRaw == "{}") && fileExists(filepath.Join(root, "log", "cycle", "state.json")) {
		if b, err := os.ReadFile(filepath.Join(root, "log", "cycle", "state.json")); err == nil {
			cycleStateRaw = string(b)
		}
	}
	if cycleStateRaw != "" && cycleStateRaw != "{}" {
		var cs map[string]any
		if json.Unmarshal([]byte(cycleStateRaw), &cs) == nil {
			if lc, ok := cs["last_cycle"].(map[string]any); ok {
				outcome := asString(lc["outcome"])
				if outcome == "" {
					outcome = "none"
				}
				fin := asString(lc["finished_at"])
				if fin == "" {
					fin = "-"
				}
				rep := asString(lc["report_path"])
				if rep == "" {
					rep = "-"
				}
				fmt.Fprintf(out, "LAST CYCLE: %s (finished %s, report %s)\n", outcome, fin, rep)
			}
		}
	}

	fmt.Fprintln(out, "== Processes")
	if st != nil {
		if dRec, _ := st.GetLock(ctx, "daemon"); dRec != nil && store.IsPIDAlive(dRec.PID) {
			meta := parseDaemonMetadata(dRec.MetadataJSON)
			// What --set gave the daemon is in no file: the line of the daemon says it.
			given := givenForTheRun(GivenToDaemon(dRec.MetadataJSON))
			if given != "" {
				given = ", given " + given
			}
			if webURL := asString(meta["web_url"]); webURL != "" {
				fmt.Fprintf(out, "RUNNING: daemon (pid %d, web %s%s)\n", dRec.PID, webURL, given)
			} else {
				fmt.Fprintf(out, "RUNNING: daemon (pid %d%s)\n", dRec.PID, given)
			}
		}
	}
	for _, daemon := range []string{"dispatcher", "merger"} {
		var rec *store.LockRecord
		if st != nil {
			rec, _ = st.GetLock(ctx, daemon)
		}
		if rec != nil && store.IsPIDAlive(rec.PID) {
			fmt.Fprintf(out, "RUNNING: %s (pid %d)\n", daemon, rec.PID)
		} else {
			fmt.Fprintf(out, "IDLE: %s\n", daemon)
		}
	}
	if ws.MergerState != "" && ws.MergerState != "IDLE" {
		fmt.Fprintf(out, "MERGER STATE: state=%s change=%s\n", ws.MergerState, ws.MergerChange)
	}

	fmt.Fprintln(out, "== Queues")
	var readyList []store.ReadyManifest
	if st != nil {
		readyList, _ = st.ListReadyQueue(ctx)
	}
	if problem := config.BacklogProblem(filepath.Join(root, ".foreman-kit", "agent_backlog.json")); problem != "" {
		fmt.Fprintf(out, "BACKLOG UNREADABLE: .foreman-kit/agent_backlog.json: %s; nothing new starts until it is valid JSON\n", problem)
	}
	fmt.Fprintf(out, "ready_to_merge: %d\n", len(readyList))
	if st != nil {
		// How full the queue is, as the next cycle will count it.
		if q, err := readQueue(ctx, &settings.Reader{Root: root, Stderr: io.Discard}, root, st); err == nil {
			if q.Full() {
				fmt.Fprintf(out, "QUEUE FULL: %s; the cycle queues nothing new and approved proposals wait until a change is integrated or abandoned\n", q)
			} else {
				fmt.Fprintf(out, "open_changes: %d (%s: %d)\n", q.Open, queueMaxOpenKey, q.MaxOpen)
			}
		}
		_ = VisualQAStatus(ctx, root, st, out)
	}

	if fileExists(filepath.Join(root, filepath.FromSlash(ProposalsFile))) {
		props, err := ReadProposals(root)
		if err != nil {
			fmt.Fprintln(out, "proposals: unreadable")
		} else {
			var waiting, approved []string
			for _, p := range props {
				switch {
				case asString(p["status"]) == "proposed":
					waiting = append(waiting, asString(p["change_name"]))
				case asString(p["status"]) == "approved" && asString(p["kind"]) != "prompt":
					approved = append(approved, asString(p["change_name"]))
				}
			}
			waitStr := "none"
			if len(waiting) > 0 {
				waitStr = strings.Join(waiting, ", ")
			}
			fmt.Fprintf(out, "proposals waiting for approval: %s\n", waitStr)
			if len(approved) > 0 {
				fmt.Fprintf(out, "proposals approved, queued by the next cycle while the queue has room: %s\n", strings.Join(approved, ", "))
			}
		}
	}

	if st != nil {
		blockedList, _ := st.ListQuarantine(ctx, "blocked")
		for _, b := range blockedList {
			note := RetryNote(root, b)
			noteStr := ""
			if note != "" {
				noteStr = fmt.Sprintf(" (%s)", note)
			}
			fmt.Fprintf(out, "BLOCKED: %s reason=%s since=%s%s\n", b.ChangeName, b.Reason, b.BlockedAt, noteStr)
		}
		for _, bb := range BrokenBases(ctx, root, st) {
			fmt.Fprintln(out, bb.StatusLine())
		}

		allLocks, _ := st.ListLocks(ctx)
		systemLocks := map[string]bool{
			"cycle": true, "dispatch": true, "visual_qa": true,
			"dispatcher": true, "merger": true, "record": true, "daemon": true,
		}
		for _, l := range allLocks {
			if !systemLocks[l.LockKey] {
				fmt.Fprintf(out, "LEASE: %s\n", l.LockKey)
			}
		}
	}

	fmt.Fprintln(out, "== Worktrees")
	if wtOut, err := gitx.RunGit(ctx, root, "worktree", "list"); err == nil {
		for _, line := range strings.Split(strings.TrimRight(wtOut, "\n"), "\n") {
			if strings.TrimSpace(line) != "" {
				fmt.Fprintf(out, "  %s\n", line)
			}
		}
	}

	fmt.Fprintln(out, "== Repositories")
	if dirExists(filepath.Join(root, ".git")) || fileExists(filepath.Join(root, ".git")) {
		br, _ := gitx.RunGit(ctx, root, "branch", "--show-current")
		sha, _ := gitx.RunGit(ctx, root, "rev-parse", "--short", "HEAD")
		behindNote := ""
		// The main checkout is not moved by a merge: say when it lags origin, or its repos/ and
		// backlog look older than what was integrated.
		if b := strings.TrimSpace(br); b != "" {
			if n, err := gitx.RunGit(ctx, root, "rev-list", "--count", "HEAD..origin/"+b); err == nil {
				if behind, _ := strconv.Atoi(strings.TrimSpace(n)); behind > 0 {
					behindNote = fmt.Sprintf(" behind origin/%s by %d (foreman-kit sync)", b, behind)
				}
			}
		}
		fmt.Fprintf(out, "  %-22s %-24s %s%s\n", "root", strings.TrimSpace(br), strings.TrimSpace(sha), behindNote)
		if repo, err := gitx.Open(root); err == nil {
			children, _ := repo.ChildPaths()
			for _, p := range children {
				d := filepath.Join(root, filepath.FromSlash(p))
				if !exists(filepath.Join(d, ".git")) {
					fmt.Fprintf(out, "  %-22s not initialized\n", p)
					continue
				}
				cbr, _ := gitx.RunGit(ctx, d, "branch", "--show-current")
				csha, _ := gitx.RunGit(ctx, d, "rev-parse", "--short", "HEAD")
				dirtyOut, _ := gitx.StatusPorcelain(ctx, d, true)
				dirtyLines := 0
				if strings.TrimSpace(dirtyOut) != "" {
					dirtyLines = len(strings.Split(strings.TrimRight(dirtyOut, "\n"), "\n"))
				}
				fmt.Fprintf(out, "  %-22s %-24s %s dirty=%d\n", p, strings.TrimSpace(cbr), strings.TrimSpace(csha), dirtyLines)
			}
			baseRef := "origin/" + ResolveBaseBranch(root)
			if _, err := gitx.RunGit(ctx, root, "rev-parse", "--verify", "--quiet", baseRef); err != nil {
				baseRef = "HEAD"
			}
			for _, p := range children {
				if rb := gitx.FindPointerRollback(ctx, root, baseRef, p, 200); rb != nil {
					fmt.Fprintf(out, "  POINTER ROLLED BACK: %s: umbrella commit %.7s moved its gitlink back from %.7s to %.7s (%d integrated commit(s) missing; a stale submodule committed with git commit -a?). The next change there builds on %.7s and its merge records it again; to restore it now: git -C %s checkout %.7s && git add %s && git commit\n",
						p, rb.Commit, rb.From, rb.To, rb.Lost, rb.From, p, rb.From, p)
				}
			}
		}
	}
	return nil
}

// SummarizeCycleLog extracts a structured summary from a cycle log text or execution.
func SummarizeCycleLog(stamp, text string, runningPID int) map[string]any {
	var outcome string
	switch {
	case strings.Contains(text, "wrote outside its scope"):
		outcome = "scope violation"
	case strings.Contains(text, "preflight: FAIL"):
		outcome = "preflight failed"
	case strings.Contains(text, "cycle: paused"):
		outcome = "paused"
	case strings.Contains(text, "== done"):
		if strings.Contains(text, "dispatch: already running") {
			outcome = "judged; an earlier dispatch was still running"
		} else {
			outcome = "done"
		}
	case runningPID > 0:
		stage := "starting"
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "== ") {
				fields := strings.Fields(strings.TrimPrefix(line, "== "))
				if len(fields) > 0 {
					stage = fields[0]
				}
			}
		}
		outcome = fmt.Sprintf("running (%s)", stage)
	default:
		outcome = "stopped before the end"
	}

	atStr := time.Now().UTC().Format(time.RFC3339)
	if t, err := ParseStampTime(stamp); err == nil {
		atStr = t.Format(time.RFC3339)
	}

	var strategy, judge, preflight any
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "strategy: ") {
			fields := strings.Fields(strings.TrimPrefix(line, "strategy: "))
			if len(fields) > 0 {
				strategy = fields[0]
			}
		} else if strings.HasPrefix(line, "== judgment (") && strings.HasSuffix(line, ")") {
			judge = strings.TrimSuffix(strings.TrimPrefix(line, "== judgment ("), ")")
		} else if strings.HasPrefix(line, "preflight: OK") || strings.HasPrefix(line, "preflight: FAIL") {
			preflight = strings.TrimSpace(line)
		}
	}

	return map[string]any{
		"stamp":     stamp,
		"at":        atStr,
		"outcome":   outcome,
		"strategy":  strategy,
		"judge":     judge,
		"preflight": preflight,
	}
}

// Progress builds the complete progress report from SQLite store and gitx in milliseconds (bin/cycle_progress.rb).
func Progress(ctx context.Context, root string, st *store.Store, since time.Time) (map[string]any, error) {
	if since.IsZero() {
		since = time.Now().UTC().Add(-24 * time.Hour)
	} else {
		since = since.UTC()
	}

	var ws store.WorkspaceState
	if st != nil {
		ws, _ = st.GetWorkspaceState(ctx)
	}
	var pausedVal any
	if ws.Paused {
		pausedVal = ws.PauseReason
	} else if fileExists(store.RuntimePath(root, "PAUSE")) {
		b, _ := os.ReadFile(store.RuntimePath(root, "PAUSE"))
		pausedVal = strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	}

	var cycleJudgingPID, dispatchPID, dispatcherPID, mergerPID any
	var dispatcherMode any // "continuous" for the daemon's dispatcher, "run" for a dispatch or a CLI
	var visualSession any
	var changesInFlight []map[string]any

	if st != nil {
		// LiveLock, not a live PID: a row whose heartbeat outlived its TTL is up for the taking even
		// while its owner lives (store.IsLockStale), and a reused PID belongs to another process.
		if l, _ := st.LiveLock(ctx, "cycle"); l != nil {
			cycleJudgingPID = l.PID
		}
		if l, _ := st.LiveLock(ctx, "dispatch"); l != nil {
			dispatchPID = l.PID
		}
		if l, _ := st.LiveLock(ctx, "dispatcher"); l != nil {
			dispatcherPID = l.PID
			dispatcherMode = "run"
			if asString(parseDaemonMetadata(l.MetadataJSON)["mode"]) == "continuous" {
				dispatcherMode = "continuous"
			}
		}
		if l, _ := st.LiveLock(ctx, "merger"); l != nil {
			mergerPID = l.PID
		}
		if vs := ActiveVisualSession(ctx, st); vs != "" {
			visualSession = vs
		}
		allLocks, _ := st.ListLocks(ctx)
		for _, l := range allLocks {
			if chg, ok := store.WorkerLeaseChange(l.LockKey); ok {
				changesInFlight = append(changesInFlight, map[string]any{
					"change":    chg,
					"since":     l.AcquiredAt,
					"heartbeat": l.HeartbeatAt,
				})
			}
		}
	}
	if changesInFlight == nil {
		changesInFlight = []map[string]any{}
	}

	var readyNames []string
	if st != nil {
		if rList, err := st.ListReadyQueue(ctx); err == nil {
			for _, r := range rList {
				readyNames = append(readyNames, r.ChangeName)
			}
		}
	}
	if readyNames == nil {
		readyNames = []string{}
	}

	nowSection := map[string]any{
		"paused":            pausedVal,
		"cycle_judging_pid": cycleJudgingPID,
		"dispatch_pid":      dispatchPID,
		"dispatcher_pid":    dispatcherPID,
		"dispatcher_mode":   dispatcherMode,
		"merger_pid":        mergerPID,
		"visual_session":    visualSession,
		"changes_in_flight": changesInFlight,
		"ready_to_merge":    readyNames,
	}

	// Integrated changes via gitx
	var integratedInPeriod []map[string]any
	everIntegrated := make(map[string]bool)
	for _, c := range ArchivedChanges(root) {
		everIntegrated[c] = true
	}
	if repo, err := gitx.Open(root); err == nil {
		baseBranch := ResolveBaseBranch(root)
		baseRef := "HEAD"
		if _, err := repo.RevParse("origin/" + baseBranch); err == nil {
			baseRef = "origin/" + baseBranch
		}
		if allInt, err := repo.CompletedSince("", baseRef); err == nil {
			for _, ic := range allInt {
				everIntegrated[ic.ChangeName] = true
				if t, err := time.Parse(time.RFC3339, ic.Committed); err == nil && !t.Before(since) {
					integratedInPeriod = append(integratedInPeriod, map[string]any{
						"change": ic.ChangeName,
						"at":     ic.Committed,
					})
				}
			}
		}
	}
	if integratedInPeriod == nil {
		integratedInPeriod = []map[string]any{}
	}

	// Cycles from store executions and runtime cycle/cycle-*.log
	var cyclesInPeriod []map[string]any
	var lastCycleSummary any
	seenCycleStamps := make(map[string]bool)
	if st != nil {
		execs, _ := st.ListExecutions(ctx, "cycle", "", 100)
		// Reverse to chronological order
		for i := len(execs) - 1; i >= 0; i-- {
			ex := execs[i]
			stamp := strings.TrimPrefix(ex.ExecID, "cycle-")
			seenCycleStamps[stamp] = true
			logText, _ := st.ReadFullLog(ctx, ex.ExecID)
			runPID := 0
			if ex.Status == "running" {
				runPID = ex.PID
			}
			sum := SummarizeCycleLog(stamp, logText, runPID)
			if ex.StartedAt != "" {
				sum["at"] = ex.StartedAt
			}
			if ex.Agent != "" && sum["judge"] == nil {
				sum["judge"] = ex.Agent
			}
			lastCycleSummary = sum
			if t, err := time.Parse(time.RFC3339, asString(sum["at"])); err == nil && !t.Before(since) {
				cyclesInPeriod = append(cyclesInPeriod, sum)
			}
		}
	}
	if logEntries, err := os.ReadDir(store.RuntimePath(root, "cycle")); err == nil {
		for _, e := range logEntries {
			name := e.Name()
			if !strings.HasPrefix(name, "cycle-") || !strings.HasSuffix(name, ".log") {
				continue
			}
			stamp := strings.TrimSuffix(strings.TrimPrefix(name, "cycle-"), ".log")
			if seenCycleStamps[stamp] {
				continue
			}
			b, err := os.ReadFile(store.RuntimePath(root, "cycle", name))
			if err != nil {
				continue
			}
			sum := SummarizeCycleLog(stamp, string(b), 0)
			lastCycleSummary = sum
			if stTime, err := ParseStampTime(stamp); err == nil && !stTime.Before(since) {
				cyclesInPeriod = append(cyclesInPeriod, sum)
			}
		}
	}
	if cyclesInPeriod == nil {
		cyclesInPeriod = []map[string]any{}
	}

	// Blocked records from store
	var blockedRecords []store.QuarantineRecord
	if st != nil {
		blockedRecords, _ = st.ListQuarantine(ctx, "blocked")
	}
	var blockedInPeriod []map[string]any
	blockedByReason := make(map[string]int)
	blockedNames := make(map[string]bool)
	var needsHumanBlocked []map[string]any
	for _, b := range blockedRecords {
		blockedNames[b.ChangeName] = true
		blockedByReason[b.Reason]++
		if t, err := time.Parse(time.RFC3339, b.BlockedAt); err == nil && !t.Before(since) {
			blockedInPeriod = append(blockedInPeriod, map[string]any{
				"change_name": b.ChangeName,
				"reason":      b.Reason,
				"blocked_at":  b.BlockedAt,
			})
		}
		needsHumanBlocked = append(needsHumanBlocked, map[string]any{
			"change_name": b.ChangeName,
			"reason":      b.Reason,
			"retry":       RetryNote(root, b),
		})
	}
	if blockedInPeriod == nil {
		blockedInPeriod = []map[string]any{}
	}
	if needsHumanBlocked == nil {
		needsHumanBlocked = []map[string]any{}
	}
	needsHumanBrokenBases := []map[string]any{}
	for _, bb := range BrokenBases(ctx, root, st) {
		item := bb.asMap()
		item["text"] = bb.NeedsHumanText()
		needsHumanBrokenBases = append(needsHumanBrokenBases, item)
	}

	// Proposals & Backlog
	proposals, _ := ReadProposals(root)
	backlog, _ := ReadJSONList(filepath.Join(root, filepath.FromSlash(BacklogFile)))
	inPeriodStr := func(ts string) bool {
		if ts == "" {
			return false
		}
		t, err := time.Parse(time.RFC3339, ts)
		return err == nil && !t.Before(since)
	}

	propProposed, propQueued, propIntegrated := 0, 0, 0
	var proposalsWaiting []map[string]any
	// Approved and not queued yet: the next review queues them while the queue has room.
	proposalsApproved := []string{}
	fromRoadmap := make(map[string]int)
	for _, p := range proposals {
		if inPeriodStr(asString(p["proposed_at"])) {
			propProposed++
		}
		if inPeriodStr(asString(p["queued_at"])) {
			propQueued++
		}
		if inPeriodStr(asString(p["integrated_at"])) {
			propIntegrated++
		}
		if asString(p["status"]) == "approved" && asString(p["kind"]) != "prompt" {
			proposalsApproved = append(proposalsApproved, asString(p["change_name"]))
		}
		if asString(p["status"]) == "proposed" {
			var reviewStatus any
			if rev, ok := p["review"].(map[string]any); ok {
				reviewStatus = rev["status"]
			}
			proposalsWaiting = append(proposalsWaiting, map[string]any{
				"change":    asString(p["change_name"]),
				"source":    p["source"],
				"evaluator": reviewStatus,
			})
		}
		if asString(p["source"]) == "roadmap" {
			fromRoadmap[asString(p["status"])]++
		}
	}
	if proposalsWaiting == nil {
		proposalsWaiting = []map[string]any{}
	}

	// Visual QA items from store
	var vqaItems []store.VisualQAItem
	if st != nil {
		vqaItems, _ = st.ListVisualQA(ctx, "")
	}
	visualChecks := make(map[string]int)
	visualQueueCounts := make(map[string]int)
	var visualCannotVerify []map[string]any
	for _, it := range vqaItems {
		switch it.Status {
		case "pending", "in_progress":
			visualQueueCounts[it.Status]++
		case "dropped":
		default:
			if inPeriodStr(it.ResolvedAt) {
				visualChecks[it.Status]++
				if it.Status == "cannot-verify" {
					visualCannotVerify = append(visualCannotVerify, map[string]any{
						"id":     it.ItemID,
						"change": it.ChangeName,
						"note":   it.VerdictNote,
					})
				}
			}
		}
	}
	if visualCannotVerify == nil {
		visualCannotVerify = []map[string]any{}
	}

	visualSessionsCount := 0
	if entries, err := os.ReadDir(store.RuntimePath(root, "cycle")); err == nil {
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, "visual-") && strings.HasSuffix(n, ".md") {
				stStr := strings.TrimSuffix(strings.TrimPrefix(n, "visual-"), ".md")
				if stTime, err := ParseStampTime(stStr); err == nil && !stTime.Before(since) {
					visualSessionsCount++
				}
			}
		}
	}

	// Events from store
	var evts []store.EventRecord
	if st != nil {
		evts, _ = st.QueryEvents(ctx, store.EventFilter{Since: since})
	}
	agentCalls := 0
	totalAgentSec := 0
	agentCallsByEngine := make(map[string]int)
	totalEvaluatorCost := 0.0
	for _, e := range evts {
		switch e.Event {
		case "agent.exit":
			agentCalls++
			if sec, ok := toInt(e.Payload["seconds"]); ok {
				totalAgentSec += sec
			}
			eng := e.Engine
			if eng == "" {
				eng = asString(e.Payload["engine"])
			}
			if eng != "" {
				agentCallsByEngine[eng]++
			}
		case "evaluator.answer":
			if cost, ok := e.Payload["cost_usd"].(float64); ok {
				totalEvaluatorCost += cost
			}
		}
	}

	inFlightSet := make(map[string]bool)
	for _, c := range changesInFlight {
		inFlightSet[asString(c["change"])] = true
	}
	readySet := make(map[string]bool)
	for _, r := range readyNames {
		readySet[r] = true
	}

	backlogByStatus := make(map[string][]string)
	seenBacklog := make(map[string]bool)
	for _, item := range backlog {
		name := asString(item["change_name"])
		if name == "" || seenBacklog[name] {
			continue
		}
		seenBacklog[name] = true
		var status string
		switch {
		case !ChangeNameRe.MatchString(name):
			status = "invalid name"
		case everIntegrated[name]:
			status = "integrated"
		case blockedNames[name]:
			status = "blocked"
		case readySet[name]:
			status = "ready to merge"
		case inFlightSet[name]:
			status = "in flight"
		default:
			status = "pending"
		}
		backlogByStatus[status] = append(backlogByStatus[status], name)
	}

	// Roadmap
	cycleSettings := cycleReader(root, nil)
	roadmapFile := cycleSettings.String(settings.RoadmapFile)
	var roadmapFileVal any
	filledIn := false
	itemsListed := 0
	if roadmapFile != "" {
		roadmapFileVal = roadmapFile
		if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(roadmapFile))); err == nil {
			text := string(b)
			filledIn = !strings.Contains(text, "{{")
			beforeDone := text
			if idx := strings.Index(strings.ToLower(text), "\n## done"); idx >= 0 {
				beforeDone = text[:idx]
			}
			itemsListed = len(roadmapBulletRe.FindAllString(beforeDone, -1))
		}
	}

	// Health
	var judgmentOutcome any
	if ws.CycleStateJSON != "" && ws.CycleStateJSON != "{}" {
		var cs map[string]any
		if json.Unmarshal([]byte(ws.CycleStateJSON), &cs) == nil {
			if lc, ok := cs["last_cycle"].(map[string]any); ok {
				judgmentOutcome = lc["outcome"]
			}
		}
	}

	// How full the queue is, as the next cycle will count it. open is null when it cannot be counted.
	queue := queueState{MaxOpen: cycleSettings.Int(settings.QueueMaxOpen)}
	queueSection := map[string]any{"max_open": queue.MaxOpen, "open": nil, "room": nil, "full": false}
	if open, err := openChanges(ctx, st, backlog); err == nil {
		queue.Open = open
		queueSection["open"], queueSection["room"], queueSection["full"] = queue.Open, queue.Room(), queue.Full()
	}

	return map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"since":        since.Format(time.RFC3339),
		"dashboard":    nowSection,
		"period": map[string]any{
			"cycles":                cyclesInPeriod,
			"integrated":            integratedInPeriod,
			"blocked":               blockedInPeriod,
			"proposals":             map[string]int{"proposed": propProposed, "queued": propQueued, "integrated": propIntegrated},
			"visual_checks":         visualChecks,
			"visual_sessions":       visualSessionsCount,
			"agent_calls":           agentCalls,
			"agent_minutes":         math.Round((float64(totalAgentSec)/60.0)*10) / 10,
			"agent_calls_by_engine": agentCallsByEngine,
			"evaluator_cost_usd":    math.Round(totalEvaluatorCost*1e6) / 1e6,
		},
		"pending": map[string]any{
			"backlog":            backlogByStatus,
			"queue":              queueSection,
			"proposals_waiting":  proposalsWaiting,
			"proposals_approved": proposalsApproved,
			"visual_queue":       visualQueueCounts,
		},
		"roadmap": map[string]any{
			"file":         roadmapFileVal,
			"filled_in":    filledIn,
			"items_listed": itemsListed,
			"proposals":    fromRoadmap,
		},
		"health": map[string]any{
			"last_cycle":        lastCycleSummary,
			"judgment_outcome":  judgmentOutcome,
			"blocked_by_reason": blockedByReason,
		},
		"needs_human": map[string]any{
			"paused":               pausedVal,
			"blocked":              needsHumanBlocked,
			"broken_bases":         needsHumanBrokenBases,
			"proposals_waiting":    len(proposalsWaiting),
			"visual_cannot_verify": visualCannotVerify,
		},
	}, nil
}

// FormatProgressText formats the Progress map into the human-readable CLI output matching bin/cycle_progress.rb.
func FormatProgressText(r map[string]any) string {
	now, _ := r["dashboard"].(map[string]any)
	p, _ := r["period"].(map[string]any)
	pending, _ := r["pending"].(map[string]any)
	rm, _ := r["roadmap"].(map[string]any)
	h, _ := r["health"].(map[string]any)
	n, _ := r["needs_human"].(map[string]any)

	var running []string
	if pid := now["cycle_judging_pid"]; pid != nil {
		running = append(running, fmt.Sprintf("a cycle is judging (pid %v)", pid))
	}
	if pid := now["dispatch_pid"]; pid != nil {
		running = append(running, fmt.Sprintf("a dispatch is running (pid %v)", pid))
	}
	if pid := now["dispatcher_pid"]; pid != nil && now["dispatch_pid"] == nil {
		if asString(now["dispatcher_mode"]) == "continuous" {
			running = append(running, fmt.Sprintf("the daemon's continuous dispatcher (pid %v)", pid))
		} else {
			running = append(running, fmt.Sprintf("a dispatcher started by hand (pid %v)", pid))
		}
	}
	if pid := now["merger_pid"]; pid != nil {
		running = append(running, fmt.Sprintf("the merger (pid %v)", pid))
	}
	if vs := now["visual_session"]; vs != nil && asString(vs) != "" {
		running = append(running, fmt.Sprintf("a visual QA session (%v)", vs))
	}

	var inFlightNames []string
	if cif, ok := now["changes_in_flight"].([]map[string]any); ok {
		for _, c := range cif {
			inFlightNames = append(inFlightNames, asString(c["change"]))
		}
	}
	readyNames, _ := now["ready_to_merge"].([]string)

	var lines []string
	lines = append(lines, fmt.Sprintf("== Dashboard (%s)", asString(r["generated_at"])))
	if now["paused"] != nil && asString(now["paused"]) != "" {
		lines = append(lines, fmt.Sprintf("  PAUSED: %v", now["paused"]))
	}
	lines = append(lines, fmt.Sprintf("  running: %s", joinOrNone(running)))
	lines = append(lines, fmt.Sprintf("  changes in flight: %s; ready to merge: %s", joinOrNone(inFlightNames), joinOrNone(readyNames)))

	cycles, _ := p["cycles"].([]map[string]any)
	var strategies []string
	seenStrat := make(map[string]bool)
	for _, c := range cycles {
		st := asString(c["strategy"])
		if st != "" && !seenStrat[st] {
			seenStrat[st] = true
			strategies = append(strategies, st)
		}
	}
	integrated, _ := p["integrated"].([]map[string]any)
	var intNames []string
	for _, ic := range integrated {
		intNames = append(intNames, asString(ic["change"]))
	}
	blockedPeriod, _ := p["blocked"].([]map[string]any)
	propsMap, _ := p["proposals"].(map[string]int)

	lines = append(lines, fmt.Sprintf("== Since %s", asString(r["since"])))
	lines = append(lines, fmt.Sprintf("  cycles: %d", len(cycles)))
	lines = append(lines, fmt.Sprintf("  strategies explored: %s", joinOrNone(strategies)))
	lines = append(lines, fmt.Sprintf("  integrated: %d (%s)", len(integrated), joinOrNone(intNames)))
	lines = append(lines, fmt.Sprintf("  blocked: %d", len(blockedPeriod)))
	lines = append(lines, fmt.Sprintf("  proposals: %d proposed, %d queued, %d integrated",
		propsMap["proposed"], propsMap["queued"], propsMap["integrated"]))
	lines = append(lines, fmt.Sprintf("  visual QA: %v sessions", p["visual_sessions"]))
	lines = append(lines, fmt.Sprintf("  agents: %v calls, %v min; evaluator: $%v",
		p["agent_calls"], p["agent_minutes"], p["evaluator_cost_usd"]))

	lines = append(lines, "== Pending")
	if bl, ok := pending["backlog"].(map[string][]string); ok {
		var keys []string
		for k := range bl {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, st := range keys {
			names := bl[st]
			lines = append(lines, fmt.Sprintf("  backlog %s: %d (%s)", st, len(names), joinOrNone(names)))
		}
	}
	var waitingNames []string
	if pw, ok := pending["proposals_waiting"].([]map[string]any); ok {
		for _, w := range pw {
			waitingNames = append(waitingNames, asString(w["change"]))
		}
	}
	if q, ok := pending["queue"].(map[string]any); ok && q["open"] != nil {
		line := fmt.Sprintf("  open changes: %v (%s: %v)", q["open"], queueMaxOpenKey, q["max_open"])
		if full, _ := q["full"].(bool); full {
			line += ": the queue is full, so the cycle queues nothing new"
		}
		lines = append(lines, line)
	}
	lines = append(lines, fmt.Sprintf("  proposals waiting for approval: %s", joinOrNone(waitingNames)))
	if approved, _ := pending["proposals_approved"].([]string); len(approved) > 0 {
		lines = append(lines, fmt.Sprintf("  proposals approved, queued by the next cycle while the queue has room: %s", strings.Join(approved, ", ")))
	}

	lines = append(lines, "== Roadmap")
	if rm["file"] == nil || asString(rm["file"]) == "" {
		lines = append(lines, "  none configured")
	} else if filled, _ := rm["filled_in"].(bool); !filled {
		lines = append(lines, fmt.Sprintf("  %v still has {{...}} placeholders", rm["file"]))
	} else {
		lines = append(lines, fmt.Sprintf("  %v: %v items listed", rm["file"], rm["items_listed"]))
	}

	lines = append(lines, "== Health")
	if lc, ok := h["last_cycle"].(map[string]any); ok && lc != nil {
		lines = append(lines, fmt.Sprintf("  last cycle: %s, %s", asString(lc["at"]), asString(lc["outcome"])))
	} else {
		lines = append(lines, "  last cycle: none yet")
	}
	jo := asString(h["judgment_outcome"])
	if jo == "" {
		jo = "none"
	}
	lines = append(lines, fmt.Sprintf("  judgment's last outcome: %s", jo))

	lines = append(lines, "== Needs a human")
	var asks []string
	if n["paused"] != nil && asString(n["paused"]) != "" {
		asks = append(asks, fmt.Sprintf("workspace is paused (%v)", n["paused"]))
	}
	if bl, ok := n["blocked"].([]map[string]any); ok {
		for _, b := range bl {
			asks = append(asks, fmt.Sprintf("%s is blocked (%s; %s)", asString(b["change_name"]), asString(b["reason"]), asString(b["retry"])))
		}
	}
	if bb, ok := n["broken_bases"].([]map[string]any); ok {
		for _, b := range bb {
			asks = append(asks, asString(b["text"]))
		}
	}
	if pw, ok := toInt(n["proposals_waiting"]); ok && pw > 0 {
		asks = append(asks, fmt.Sprintf("%d proposal(s) waiting for approval", pw))
	}
	if len(asks) == 0 {
		lines = append(lines, "  nothing")
	} else {
		for _, a := range asks {
			lines = append(lines, "  - "+a)
		}
	}
	return strings.Join(lines, "\n")
}

// CompletedSince lists integrated changes, child repository commits, archived changes, completed tasks, handoff lessons, and repair rounds between baseRev and headRev (bin/completed_since.sh).
func CompletedSince(ctx context.Context, root, baseRev, headRev string, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	if headRev == "" {
		headRev = "HEAD"
	}
	repo, err := gitx.Open(root)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "== Integrated changes")
	integrated, err := repo.CompletedSince(baseRev, headRev)
	if err == nil {
		for _, ic := range integrated {
			short := ic.SHA
			if len(short) > 7 {
				short = short[:7]
			}
			fmt.Fprintf(out, "  %s %s (%s)\n", short, ic.ChangeName, ic.Committed)
		}
	}

	fmt.Fprintln(out, "== Child repository commits")
	children, _ := repo.ChildPaths()
	for _, p := range children {
		oldSHA, err1 := repo.TreeEntryHash(baseRev, p)
		newSHA, err2 := repo.TreeEntryHash(headRev, p)
		if err1 != nil || err2 != nil || oldSHA == "" || newSHA == "" || oldSHA == newSHA {
			continue
		}
		fmt.Fprintf(out, "  %s:\n", p)
		childDir := filepath.Join(root, filepath.FromSlash(p))
		if logOut, err := gitx.RunGit(ctx, childDir, "log", "--format=    %h %s", oldSHA+".."+newSHA); err == nil {
			if strings.TrimSpace(logOut) != "" {
				fmt.Fprint(out, logOut)
			}
		} else {
			fmt.Fprintf(out, "    (fetch %s to list %s..%s)\n", p, oldSHA[:minInt(8, len(oldSHA))], newSHA[:minInt(8, len(newSHA))])
		}
	}

	fmt.Fprintln(out, "== Archived changes, completed tasks, handoff lessons and repair rounds")
	changedFiles, err := repo.ChangedFiles(baseRev, headRev)
	if err == nil {
		for _, f := range changedFiles {
			if !strings.HasPrefix(f, "openspec/changes/archive/") || !strings.HasSuffix(f, "/tasks.md") {
				continue
			}
			// Only newly added archive tasks.md (absent in baseRev)
			if _, err := repo.ReadBlob(baseRev, f); err == nil {
				continue
			}
			tasksBytes, err := repo.ReadBlob(headRev, f)
			if err != nil {
				continue
			}
			dir := filepath.ToSlash(filepath.Dir(f))
			baseName := filepath.Base(dir)
			doneCount := len(checkedTaskRe.FindAll(tasksBytes, -1))
			fmt.Fprintf(out, "  %s: %d task(s) complete\n", baseName, doneCount)

			if handoffBytes, err := repo.ReadBlob(headRev, dir+"/handoff.md"); err == nil {
				for _, line := range strings.Split(strings.TrimRight(string(handoffBytes), "\n"), "\n") {
					fmt.Fprintf(out, "    handoff: %s\n", line)
				}
			}
			if repairsBytes, err := repo.ReadBlob(headRev, dir+"/repairs.md"); err == nil {
				for _, rline := range parseRepairsSummary(dir+"/repairs.md", string(repairsBytes)) {
					fmt.Fprintln(out, rline)
				}
			}
		}
	}
	return nil
}

func parseRepairsSummary(relPath, content string) []string {
	var out []string
	fence := ""
	heading := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "```") {
			f := strings.TrimSuffix(line, "text")
			if fence == "" {
				fence = f
			} else if line == fence {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			heading = strings.TrimPrefix(line, "## ")
		} else if strings.HasPrefix(line, "Result: ") {
			res := strings.TrimPrefix(line, "Result: ")
			out = append(out, fmt.Sprintf("    repair: %s: %s (%s)", heading, res, relPath))
		}
	}
	return out
}

// rotatedHistoryPattern matches the files a rotation of HistoryFile creates (.foreman-kit/agent_history.<N>.json).
const rotatedHistoryPattern = ".foreman-kit/agent_history.*.json"

// syncRoot moves the main checkout forward to its upstream, restoring first the backlog and history
// files that hold a version origin published after HEAD (the merger writes what it published into
// the main checkout). It says what it did on out, and tells flag what needs attention. The caller
// fetches first.
//
// It rewrites the checkout in two steps, the restore and the merge, and once it begins it ends, even
// when ctx ends meanwhile (gitx.ToCompletion). Told to stop between the two, the checkout would keep
// the backlog of an older commit. Told to stop while git moves it, the files git had already written
// would count as local edits, and a checkout with local edits is never moved again: not by this
// function, which skips the pull, and not by the dispatcher. A caller already told to stop begins
// nothing.
func syncRoot(ctx context.Context, root string, out io.Writer, flag func(string)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	apply, done := gitx.ToCompletion(ctx)
	defer done()

	// S12: A backlog/history file holding an earlier version origin published after HEAD loses nothing when restored to HEAD.
	if isAncestor(apply, root, "HEAD", "@{u}") {
		otherDirty, _ := gitx.RunGit(apply, root, "status", "--porcelain", "--ignore-submodules=all", "--", ".", ":!"+BacklogFile, ":!"+HistoryFile, ":!"+rotatedHistoryPattern)
		if strings.TrimSpace(otherDirty) == "" {
			// The files a rotation of the history creates are among them: such a file is untracked here
			// until the pull brings the commit that added it, and left in place it made every later sync
			// answer "local changes present; skipping pull".
			toRestore, _ := gitx.DirtyPaths(apply, root, BacklogFile, HistoryFile, rotatedHistoryPattern)
			allPublished := true
			for _, bf := range toRestore {
				if !isPublishedSinceHead(apply, root, bf) {
					allPublished = false
					break
				}
			}
			if allPublished {
				for _, bf := range toRestore {
					_ = gitx.RestorePublished(apply, root, bf)
				}
			}
		}
	}

	dirtyOut, _ := gitx.RunGit(apply, root, "status", "--porcelain", "--ignore-submodules=all")
	if strings.TrimSpace(dirtyOut) != "" {
		fmt.Fprintln(out, "  root: local changes present; skipping pull")
		flag("root dirty")
	} else {
		brOut, err := gitx.RunGit(apply, root, "symbolic-ref", "-q", "--short", "HEAD")
		branch := strings.TrimSpace(brOut)
		if err != nil || branch == "" {
			branch = "detached"
		}
		// Count before the fast-forward: "up to date" only when there was nothing to pull.
		rootBehindStr, _ := gitx.RunGit(apply, root, "rev-list", "--count", "HEAD..@{u}")
		rootBehind, _ := strconv.Atoi(strings.TrimSpace(rootBehindStr))
		if _, err := gitx.RunGit(apply, root, "merge", "--ff-only", "--quiet", "@{u}"); err == nil {
			if rootBehind > 0 {
				fmt.Fprintf(out, "  root: pulled %d commit(s) [%s]\n", rootBehind, branch)
			} else {
				fmt.Fprintf(out, "  root: up to date with origin [%s]\n", branch)
			}
		} else if _, err2 := gitx.RunGit(apply, root, "rev-parse", "-q", "--verify", "@{u}"); err2 != nil {
			// No upstream configured (e.g. local test repo without upstream tracking)
			fmt.Fprintf(out, "  root: up to date [%s]\n", branch)
		} else {
			fmt.Fprintf(out, "  root: cannot fast-forward [%s]\n", branch)
			flag("root diverged")
		}
	}
	return nil
}

// SyncRepos brings the main checkout and child repositories up to date with origin (bin/sync_repos.sh).
func SyncRepos(ctx context.Context, root string, st *store.Store, publish bool, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	if publish && st != nil {
		for _, lockName := range []string{"dispatcher", "merger"} {
			if l, err := st.GetLock(ctx, lockName); err == nil && l != nil && store.IsPIDAlive(l.PID) {
				fmt.Fprintf(out, "Refused: %s.lock held by live PID %d; --publish would race the autonomous writers.\n", lockName, l.PID)
				return nil
			}
		}
	}

	var attention []string
	var published []string
	flag := func(msg string) {
		attention = append(attention, msg)
	}

	fmt.Fprintln(out, "== Umbrella")
	repo, err := gitx.Open(root)
	if err != nil {
		return err
	}
	_ = repo.Fetch(ctx, "origin")
	if err := syncRoot(ctx, root, out, flag); err != nil {
		return err
	}

	fmt.Fprintln(out, "== Child repositories")
	children, _ := repo.ChildPaths()
	for _, p := range children {
		// A sync told to stop begins no other repository.
		if err := ctx.Err(); err != nil {
			return err
		}
		childDir := filepath.Join(root, filepath.FromSlash(p))
		if !exists(filepath.Join(childDir, ".git")) {
			if _, err := gitx.RunGit(ctx, root, "submodule", "update", "--init", "--quiet", "--", p); err == nil {
				fmt.Fprintf(out, "  %s: at recorded pointer\n", p)
			} else {
				fmt.Fprintf(out, "  %s: ATTENTION (submodule update failed)\n", p)
				flag(p + " update failed")
			}
			continue
		}
		cDirty, _ := gitx.StatusPorcelain(ctx, childDir, true)
		if strings.TrimSpace(cDirty) != "" {
			fmt.Fprintf(out, "  %s: SKIPPED (uncommitted changes)\n", p)
			continue
		}
		brOut, _ := gitx.RunGit(ctx, childDir, "symbolic-ref", "-q", "--short", "HEAD")
		cBranch := strings.TrimSpace(brOut)
		if cBranch == "" {
			if _, err := gitx.RunGit(ctx, root, "submodule", "update", "--init", "--quiet", "--", p); err == nil {
				fmt.Fprintf(out, "  %s: at recorded pointer\n", p)
			} else {
				fmt.Fprintf(out, "  %s: ATTENTION (submodule update failed)\n", p)
				flag(p + " update failed")
			}
			continue
		}
		_, _ = gitx.RunGit(ctx, childDir, "fetch", "--quiet", "origin")
		if _, err := gitx.RunGit(ctx, childDir, "rev-parse", "-q", "--verify", "@{u}"); err != nil {
			fmt.Fprintf(out, "  %s: ATTENTION (fetch failed or no upstream for %s)\n", p, cBranch)
			flag(p + " no upstream")
			continue
		}
		aheadStr, _ := gitx.RunGit(ctx, childDir, "rev-list", "--count", "@{u}..HEAD")
		behindStr, _ := gitx.RunGit(ctx, childDir, "rev-list", "--count", "HEAD..@{u}")
		ahead, _ := strconv.Atoi(strings.TrimSpace(aheadStr))
		behind, _ := strconv.Atoi(strings.TrimSpace(behindStr))

		if behind > 0 && ahead == 0 {
			// Not stopped halfway, like the root: a child left with the files git had already written
			// would be skipped from then on for its uncommitted changes.
			if _, err := gitx.RunGitToCompletion(ctx, childDir, "merge", "--ff-only", "--quiet", "@{u}"); err == nil {
				fmt.Fprintf(out, "  %s: pulled %d commit(s)\n", p, behind)
			}
		} else if behind > 0 {
			fmt.Fprintf(out, "  %s: ATTENTION (diverged: %d ahead, %d behind)\n", p, ahead, behind)
			flag(p + " diverged")
			continue
		}
		if ahead > 0 {
			if publish {
				if _, err := gitx.RunGit(ctx, childDir, "push", "--quiet", "origin", "HEAD:"+cBranch); err == nil {
					fmt.Fprintf(out, "  %s: pushed %d commit(s)\n", p, ahead)
					published = append(published, p)
					continue
				}
			}
			fmt.Fprintf(out, "  %s: %d unpushed commit(s) on %s\n", p, ahead, cBranch)
			flag(p + " unpushed")
		} else if behind == 0 {
			fmt.Fprintf(out, "  %s: up to date [%s]\n", p, cBranch)
		}
	}

	if publish && len(published) > 0 {
		// Not "git add <child>": git skips, without an error, a child .gitmodules marks "ignore = all",
		// and the pointers of the commits just pushed were then never recorded.
		err1 := gitx.StageGitlinks(ctx, root, published...)
		_, err2 := gitx.RunGit(ctx, root, "commit", "--quiet", "-m", "chore(repos): update pointers for "+strings.Join(published, " "))
		err3 := repo.Push(ctx, "origin", "HEAD")
		if err1 == nil && err2 == nil && err3 == nil {
			fmt.Fprintf(out, "  root: recorded pointers for %s\n", strings.Join(published, " "))
		} else {
			flag("root pointer push failed")
		}
	}

	fmt.Fprintln(out, "========================================================================")
	if len(attention) == 0 {
		fmt.Fprintln(out, "result: ok")
	} else {
		fmt.Fprintf(out, "result: attention (%s)\n", strings.Join(attention, "; "))
	}
	return nil
}

// RunProgressCLI implements `foreman-kit progress [--since PERIOD] [--json]`.
func RunProgressCLI(ctx context.Context, root string, st *store.Store, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	sinceStr := "24h"
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--since" && i+1 < len(args):
			sinceStr = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--since="):
			sinceStr = strings.TrimPrefix(args[i], "--since=")
		case args[i] == "--json":
			jsonOut = true
		default:
			fmt.Fprintln(stderr, "Usage: foreman-kit progress [--since 24h|90m|7d|<time>] [--json]")
			return 2
		}
	}
	since, err := ParseSince(sinceStr, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(stderr, "cycle_progress: %v\n", err)
		return 2
	}
	rep, err := Progress(ctx, root, st, since)
	if err != nil {
		fmt.Fprintf(stderr, "cycle_progress: %v\n", err)
		return 1
	}
	if jsonOut {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(stdout, string(b))
	} else {
		fmt.Fprintln(stdout, FormatProgressText(rep))
	}
	return 0
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

func isPublishedSinceHead(ctx context.Context, root, relFile string) bool {
	blobOut, err := gitx.RunGit(ctx, root, "hash-object", "--", relFile)
	if err != nil {
		return false
	}
	blob := strings.TrimSpace(blobOut)
	if blob == "" {
		return false
	}
	revList, err := gitx.RunGit(ctx, root, "rev-list", "HEAD..@{u}", "--", relFile)
	if err != nil {
		return false
	}
	for _, c := range strings.Split(strings.TrimSpace(revList), "\n") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		cBlob, err := gitx.RunGit(ctx, root, "rev-parse", "-q", "--verify", c+":"+relFile)
		if err == nil && strings.TrimSpace(cBlob) == blob {
			return true
		}
	}
	return false
}
