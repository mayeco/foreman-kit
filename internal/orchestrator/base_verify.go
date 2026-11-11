package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/proc"
	"github.com/mayeco/foreman-kit/internal/store"
)

// merge.outside_commits: verify. When the merger rebases a change of a child repository onto the
// repository's current base and that base holds commits the umbrella does not record yet (pushed
// outside the pipeline, typically a human hotfix), the change's own tests are measured exactly as in
// "change", and the outside commits are tested once on their own:
//
//   - when their files trigger regression.when, the repository's regression runs on the base alone
//     before the change's gates (the "regression" check);
//   - when the change's gates fail, each failing gate of that repository is rerun on the base alone
//     (the "gate:<key>" check, or "regression" for a regression gate) to tell whether the base or the
//     change broke it.
//
// Each verdict is cached per (repository, base sha, check) in the base_verifications table. A base
// with a failed, unaccepted verdict is broken: the merger holds that repository's ready changes in
// the merge queue (no quarantine, no retry spent, nothing pushed) with a single merge.base_broken event,
// until the repository's base moves (looked at every BaseRecheckInterval) or a human accepts it
// (foreman-kit base accept), after which they integrate with their own tests only.

// Check names of base verifications.
const (
	// BaseCheckRegression is the repository's whole regression set on the base alone.
	BaseCheckRegression = "regression"
	// BaseCheckGatePrefix prefixes one fast gate on the base alone: "gate:test".
	BaseCheckGatePrefix = "gate:"
)

// BaseCheckRequest asks for one check of a child repository's base alone. The merger checks the child
// out at that base in the integration worktree before the call and restores it afterwards.
type BaseCheckRequest struct {
	// Change is the change whose merge needs the verdict (for the gate_runs log only).
	Change string
	// Worktree is the integration worktree.
	Worktree string
	// Child is the child repository's path in the umbrella, e.g. repos/core.
	Child string
	// Check is BaseCheckRegression or BaseCheckGatePrefix + gate key.
	Check string
	// ChangedFiles are the check's FOREMANKIT_CHANGED_FILES: the files the outside commits changed for the
	// regression, the change's own files for the rerun of a failing gate (so a gate that runs only the tests
	// related to its changed files runs the same tests on the base alone).
	ChangedFiles []string
	Slot         int
	Timeout      time.Duration
}

// BaseCheckResult is the outcome of a BaseCheckRunnerFunc call.
type BaseCheckResult struct {
	Output   string
	ExitCode int
	OK       bool
	// Inconclusive means the check did not run to a result: a command could not start, or the caller
	// cancelled it. It says nothing about the base, so the merger records no verdict.
	Inconclusive bool
	// RoundID is the gate round the check's commands were recorded under (0 when they were not).
	RoundID int64
}

// baseVerdictInconclusive is the status baseVerdict returns, and never records, for an Inconclusive
// check.
const baseVerdictInconclusive = "inconclusive"

// BaseCheckRunnerFunc runs one check of a child repository's base alone.
type BaseCheckRunnerFunc func(ctx context.Context, req BaseCheckRequest) (BaseCheckResult, error)

// DefaultBaseCheckRunner returns the BaseCheckRunnerFunc that runs a check with the same command line
// and environment as the gates (childGateRun): for "regression", the repository's setup gate, if any,
// then its regression commands in the order the regression sweep uses; for "gate:<key>", the setup
// gate, if any, then that gate. It stops at the first failing command. The time limit starts once the
// check holds what it needs: a regression check first waits for any other regression suite of the
// process (a worker's BUILD verification), and that wait says nothing about the base.
func DefaultBaseCheckRunner(st *store.Store) BaseCheckRunnerFunc {
	run := defaultBaseCheckRunner(st)
	if st == nil {
		return run
	}
	// The commands of one check are one gate round, which closes with the result of the check.
	return func(ctx context.Context, req BaseCheckRequest) (BaseCheckResult, error) {
		roundID, _ := st.BeginGateRound(context.WithoutCancel(ctx), store.GateRound{
			Context:    "base_verify",
			ChangeName: req.Change,
			Slot:       req.Slot,
		})
		res, err := run(store.WithGateRound(ctx, roundID), req)
		result := "passed"
		switch {
		case ctx.Err() != nil || res.Inconclusive:
			result = "stopped"
		case err != nil || !res.OK:
			result = "failed"
		}
		_ = st.FinishGateRound(context.WithoutCancel(ctx), roundID, result)
		res.RoundID = roundID
		return res, err
	}
}

func defaultBaseCheckRunner(st *store.Store) BaseCheckRunnerFunc {
	return func(ctx context.Context, req BaseCheckRequest) (BaseCheckResult, error) {
		timeout := req.Timeout
		if timeout <= 0 {
			timeout = 45 * time.Minute
		}

		var buf bytes.Buffer
		name := filepath.Base(req.Child)
		childDir := filepath.Join(req.Worktree, filepath.FromSlash(req.Child))
		// A base that still holds the file an older foreman-kit read is checked with the file of the
		// main checkout, where foreman-kit update rewrote it.
		raw, cfgPath, err := config.ReadRepoConfig("", childDir)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(&buf, "---- [FAIL] %s: no repo.config.yaml on the base\n", req.Child)
			return BaseCheckResult{Output: buf.String(), ExitCode: 2}, nil
		}
		if err != nil {
			fmt.Fprintf(&buf, "---- [FAIL] %s: %v\n", req.Child, err)
			return BaseCheckResult{Output: buf.String(), ExitCode: 2}, nil
		}
		cfg, err := parseChildGateConfig(raw)
		if err != nil {
			fmt.Fprintf(&buf, "---- [FAIL] %s: invalid repo.config.yaml on the base: %v\n", req.Child, err)
			return BaseCheckResult{Output: buf.String(), ExitCode: 2}, nil
		}

		type command struct{ key, cmd string }
		var cmds []command
		setup := strings.TrimSpace(cfg.Gates["setup"])
		if strings.Contains(setup, "{{") {
			setup = ""
		}
		if req.Check == BaseCheckRegression {
			sweep, err := proc.RegressionSweepGates(raw)
			if err != nil {
				fmt.Fprintf(&buf, "---- [FAIL] %s: invalid repo.config.yaml on the base: %v\n", req.Child, err)
				return BaseCheckResult{Output: buf.String(), ExitCode: 2}, nil
			}
			if len(sweep) == 0 {
				// Nothing stands in for a regression that has no command: the gates do not run in its place.
				fmt.Fprintf(&buf, "---- [info] %s: the base has no regression command: nothing runs on it\n", req.Child)
			}
			for _, g := range sweep {
				key := "regression:" + g.Name
				if g.Name == "setup" {
					key = "setup"
				}
				cmds = append(cmds, command{key: key, cmd: strings.TrimSpace(g.Cmd)})
			}
		} else if key, ok := strings.CutPrefix(req.Check, BaseCheckGatePrefix); ok {
			gateCmd := strings.TrimSpace(cfg.Gates[key])
			if gateCmd == "" || strings.Contains(gateCmd, "{{") {
				fmt.Fprintf(&buf, "---- [FAIL] %s: the base has no %s gate\n", req.Child, key)
				return BaseCheckResult{Output: buf.String(), ExitCode: 2}, nil
			}
			if setup != "" && key != "setup" {
				cmds = append(cmds, command{key: "setup", cmd: setup})
			}
			cmds = append(cmds, command{key: key, cmd: gateCmd})
		} else {
			return BaseCheckResult{}, fmt.Errorf("base check %q: want %s or %s<key>", req.Check, BaseCheckRegression, BaseCheckGatePrefix)
		}

		if es := cfg.envScriptRel(); es != "" && !fileExists(filepath.Join(childDir, filepath.FromSlash(es))) {
			fmt.Fprintf(&buf, "repo_exec: env_script %s of %s does not exist\n", es, cfgPath)
			return BaseCheckResult{Output: buf.String(), ExitCode: 2}, nil
		}
		holdsRegression := req.Check == BaseCheckRegression
		if holdsRegression {
			heavyRegressionMu.Lock()
			defer heavyRegressionMu.Unlock()
			if err := ctx.Err(); err != nil {
				return BaseCheckResult{}, err
			}
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		runner := childGateRun{
			Worktree:           req.Worktree,
			ChildDir:           childDir,
			EnvScript:          cfg.envScriptRel(),
			Slot:               req.Slot,
			Changed:            req.ChangedFiles,
			Timeout:            timeout,
			Store:              st,
			RegressionLockHeld: holdsRegression,
		}
		for _, c := range cmds {
			if c.cmd == "" {
				continue
			}
			label := name + ":base:" + c.key
			fmt.Fprintf(&buf, "---- [gate] %s: %s\n", label, c.cmd)
			o := runner.exec(runCtx, c.key, c.cmd)
			buf.WriteString(o.Output)
			excerpt := ""
			if !o.OK {
				excerpt = ExtractGateFailureExcerpt(label, o.Output, o.ExitCode)
			}
			if st != nil {
				// Not runCtx: a command its deadline stopped is the run most worth logging.
				_ = st.RecordGateRun(context.WithoutCancel(ctx), store.GateRunRecord{
					ChangeName:     req.Change,
					Context:        "base_verify",
					RepoName:       name,
					RepoPath:       req.Child,
					GateName:       "base:" + c.key,
					Command:        c.cmd,
					Slot:           req.Slot,
					ExitCode:       o.ExitCode,
					DurationMS:     o.Duration.Milliseconds(),
					FailureExcerpt: excerpt,
				})
			}
			if !o.OK {
				rc := o.ExitCode
				if rc == 0 {
					rc = 1
				}
				fmt.Fprintf(&buf, "---- [FAIL] %s (exit %d)\n", label, rc)
				if !o.Started || ctx.Err() != nil {
					fmt.Fprintf(&buf, "---- [info] %s did not run on the base alone (it could not start, or was cancelled): no verdict\n", label)
					return BaseCheckResult{Output: buf.String(), ExitCode: rc, Inconclusive: true}, nil
				}
				return BaseCheckResult{Output: buf.String(), ExitCode: rc}, nil
			}
			fmt.Fprintf(&buf, "---- [pass] %s\n", label)
		}
		return BaseCheckResult{Output: buf.String(), OK: true}, nil
	}
}

// baseHeldError means the change was not integrated because a child repository's base fails a check
// on its own: it stays in the merge queue, unblocked, until the base moves or a human accepts it.
type baseHeldError struct {
	repo, sha string
}

func (e *baseHeldError) Error() string {
	return fmt.Sprintf("%s's base %s fails on its own", e.repo, shortSHA(e.sha))
}

// childBase is what the merger knows, in one integration attempt, about the base a touched child was
// rebased onto when that base holds commits pushed outside the pipeline.
type childBase struct {
	cp             string
	childDir       string
	pointer        string // the gitlink of cp at the integration base
	onto           string // the commit the change was rebased onto
	rebased        string // the rebased head (branch integrate/<change>)
	outsideCommits int
	outsideFiles   []string
	accepted       bool // a human accepted this base: the change's own tests only
	// acceptedSHA is the base a human accepted: onto itself, or an ancestor of it whose acceptance onto
	// inherits because no commit was pushed outside the pipeline since (the merger's own integrations).
	acceptedSHA string
	hadFailed   bool // a failed verdict for (cp, onto) existed already: its alert went out
	rechecked   bool // this attempt spent the base's one automatic recheck
	newlyFailed []string
	excerpt     string
	// siblings are the other children this attempt rebased: a check of cp's base alone checks them out
	// at their own base too, so it never runs against the change's version of a sibling repository.
	siblings map[string]siblingHeads
}

// siblingHeads is where a sibling child repository sits in one integration attempt.
type siblingHeads struct {
	onto, rebased string
}

// recheckState is when the merger last read the remote base of a repository whose base is broken.
type recheckState struct {
	sha    string // the broken sha the recheck was about
	at     time.Time
	remote string // the remote base read then ("" when it could not be read)
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func (m *Merger) verifyMode() bool {
	return m.settings.OutsideCommits == config.OutsideCommitsVerify
}

// baseLookInterval is how often the merger asks the remote whether a broken base moved.
const baseLookInterval = 5 * time.Minute

func (m *Merger) recheckInterval() time.Duration {
	if m.opts.BaseRecheckInterval > 0 {
		return m.opts.BaseRecheckInterval
	}
	return baseLookInterval
}

// heldByBrokenBase reports whether manifest must wait because a child repository it touches has a
// broken base, without the expensive work of an integration attempt: it reads the broken bases from
// the database (so an acceptance takes effect on the next pass) and the touched children from the
// umbrella's object database (no fetch). A repository due for a recheck has its remote base read; when
// that base moved off the broken sha, the change integrates normally and its new base gets a new verdict.
func (m *Merger) heldByBrokenBase(ctx context.Context, manifest store.ReadyManifest) bool {
	if m.store == nil || !m.verifyMode() {
		return false
	}
	broken, err := m.store.ListBrokenBases(ctx)
	if err != nil || len(broken) == 0 {
		return false
	}
	byRepo := make(map[string]string, len(broken))
	for _, b := range broken {
		byRepo[b.Repo] = b.SHA
	}
	rootRepo, err := gitx.Open(m.root)
	if err != nil {
		return false
	}
	head := strings.TrimSpace(manifest.HeadSHA)
	if head == "" {
		head = manifest.Branch
	}
	touched, err := rootRepo.TouchedChildren(m.resolveBaseRef(ctx, m.root), head)
	if err != nil {
		return false // the integration attempt reports what is wrong with the manifest
	}
	for _, cp := range touched {
		sha, ok := byRepo[cp]
		if !ok || manifest.ChildrenPushed[cp] != "" {
			continue
		}
		if verdicts, err := m.store.ListBaseVerifications(ctx, cp, sha); err == nil && m.baseRecheckDue(ctx, cp, sha, verdicts) {
			continue // verifyBases spends the recheck and checks the base again
		}
		if m.baseMovedOff(ctx, cp, sha) {
			continue
		}
		return true
	}
	return false
}

// baseRecheckDue reports whether repository cp's broken base sha is due for its one automatic recheck.
// A check can fail on the base alone for a reason that heals (a service that was down, a flaky test),
// and otherwise only a moved base or a person lifts the hold. It comes merge.base_recheck_after after
// the base's last failed verdict, once per base, and never while that key says never.
func (m *Merger) baseRecheckDue(ctx context.Context, cp, sha string, verdicts []store.BaseVerification) bool {
	delay := m.baseRecheckDelay()
	if delay <= 0 {
		return false
	}
	var last time.Time
	for _, v := range verdicts {
		if v.Status != store.BaseVerdictFailed {
			continue
		}
		t, err := time.Parse(time.RFC3339, v.CreatedAt)
		if err != nil {
			return false
		}
		if t.After(last) {
			last = t
		}
	}
	if last.IsZero() || m.nowFunc().Sub(last) < delay {
		return false
	}
	at, err := m.store.BaseRecheckedAt(ctx, cp, sha)
	return err == nil && at == ""
}

// baseRecheckDelay is how long after its last failed verdict a broken base is checked again:
// merge.base_recheck_after (0: never).
func (m *Merger) baseRecheckDelay() time.Duration {
	return m.settings.BaseRecheckAfter
}

// baseMovedOff reports whether repository cp's remote base is no longer the broken sha, reading it
// with git ls-remote at most once every recheckInterval, whatever it read last time. A base read as
// moved lets the held changes be attempted; when an attempt still fetches the broken sha (the base was
// force-pushed back, or the fetch failed), verifyBases records that in rechecks, so they are not
// attempted again on every pass until the next recheck.
func (m *Merger) baseMovedOff(ctx context.Context, cp, sha string) bool {
	now := time.Now()
	if st, ok := m.rechecks[cp]; ok && st.sha == sha && now.Sub(st.at) < m.recheckInterval() {
		return st.remote != "" && st.remote != sha
	}
	remote := m.remoteChildBase(ctx, cp)
	m.rechecks[cp] = recheckState{sha: sha, at: now, remote: remote}
	if remote != "" && remote != sha {
		m.logf("merger: %s's base moved off the broken %s (now %s): integrating its changes again", cp, shortSHA(sha), shortSHA(remote))
		// The broken sha is no longer the base: resolve it now, not only when an attempt on the new base
		// gets that far. An attempt that ends otherwise (sent back for a conflict) would leave it listed
		// as BASE BROKEN, announced again by every merger process. An attempt that still fetches the
		// broken sha (forced back) reopens it in verifyBases.
		if resolved, err := m.store.ResolveBrokenBases(ctx, cp, remote, m.nowFunc()); err == nil && len(resolved) > 0 {
			m.movedOff[cp] = append(m.movedOff[cp], resolved...)
		}
		return true
	}
	return false
}

// remoteChildBase reads the commit child repository cp's base branch points to on the remote, from the
// integration worktree's checkout of cp (the one rebaseChild fetches into, with the base branch it reads
// there), else from the main checkout's. It returns "" when neither is a repository of its own: in a
// directory without a valid .git, git would run in the umbrella and read the umbrella remote's branch
// of the same name.
func (m *Merger) remoteChildBase(ctx context.Context, cp string) string {
	for _, umbrella := range []string{m.integrationWorktree(), m.root} {
		dir := filepath.Join(umbrella, filepath.FromSlash(cp))
		if !gitx.IsWorktreeRoot(ctx, dir) {
			continue
		}
		repo, err := gitx.Open(umbrella)
		if err != nil {
			continue
		}
		ref := "refs/heads/" + repo.ChildBaseBranch(cp)
		out, err := gitx.RunGit(ctx, dir, "ls-remote", m.settings.Remote, ref)
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(out, "\n") {
			if fields := strings.Fields(line); len(fields) == 2 && fields[1] == ref {
				return fields[0]
			}
		}
		return ""
	}
	return ""
}

// verifyBases runs the base regression check (merge.outside_commits: verify) for every child the
// change touched and this attempt rebased, and returns what it learned about each base holding outside
// commits, for gate-failure attribution. It returns a *baseHeldError when a base is broken.
func (m *Merger) verifyBases(ctx context.Context, change string, wtRepo *gitx.Repo, integrationBaseSHA string, childOnto, childRebased map[string]string) (map[string]*childBase, error) {
	wt := m.integrationWorktree()
	var cps []string
	for cp := range childOnto {
		cps = append(cps, cp)
	}
	sort.Strings(cps)
	bases := make(map[string]*childBase)
	for _, cp := range cps {
		onto := childOnto[cp]
		if onto == "" {
			continue
		}
		childDir := filepath.Join(wt, filepath.FromSlash(cp))
		pointer, _ := wtRepo.TreeEntryHash(integrationBaseSHA, cp)
		n := countCommits(ctx, childDir, pointer, onto)
		if n == 0 {
			// No outside commits: exactly as merge.outside_commits: ignore, unless the base descends
			// from one a human accepted. The merger's own integration on an accepted base is a new sha
			// holding the same failing check; without the acceptance carried over, the next change would
			// fail that check and be quarantined as if it had broken it.
			if acceptedSHA := m.carriedAcceptance(ctx, cp, childDir, onto); acceptedSHA != "" {
				bases[cp] = &childBase{cp: cp, childDir: childDir, pointer: pointer, onto: onto, rebased: childRebased[cp],
					accepted: true, acceptedSHA: acceptedSHA, siblings: make(map[string]siblingHeads)}
				m.logf("merger: %s's base %s descends from %s, which a human accepted, with no outside commit since: %s integrates with its own tests only", cp, shortSHA(onto), shortSHA(acceptedSHA), change)
			}
			m.baseHealthy(ctx, cp, onto)
			continue
		}
		// New outside commits: an acceptance of an older base of cp no longer describes it.
		if n, err := m.store.ExpireAcceptedBases(ctx, cp, onto, m.nowFunc()); err == nil && n > 0 {
			m.logf("merger: %s: %d outside commit(s) since a base a human accepted: that acceptance no longer carries", cp, countCommits(ctx, childDir, pointer, onto))
		}
		bc := &childBase{
			cp:             cp,
			childDir:       childDir,
			pointer:        pointer,
			onto:           onto,
			rebased:        childRebased[cp],
			outsideCommits: n,
			outsideFiles:   diffNames(ctx, childDir, pointer, onto),
			siblings:       make(map[string]siblingHeads),
		}
		for other, otherOnto := range childOnto {
			if other != cp && otherOnto != "" {
				bc.siblings[other] = siblingHeads{onto: otherOnto, rebased: childRebased[other]}
			}
		}
		verdicts, err := m.store.ListBaseVerifications(ctx, cp, onto)
		if err != nil {
			return nil, err
		}
		for _, v := range verdicts {
			switch v.Status {
			case store.BaseVerdictAccepted:
				bc.accepted, bc.acceptedSHA = true, onto
			case store.BaseVerdictFailed:
				bc.hadFailed = true
			}
		}
		bases[cp] = bc
		if bc.accepted {
			m.logf("merger: %s's base %s was accepted: %s integrates with its own tests only", cp, shortSHA(onto), change)
			m.baseHealthy(ctx, cp, onto)
			continue
		}
		if bc.hadFailed && m.baseRecheckDue(ctx, cp, onto, verdicts) {
			if ok, err := m.store.RecheckBrokenBase(ctx, cp, onto, m.nowFunc()); err == nil && ok {
				// Its failed verdicts are gone: the checks below compute them again, and a base that
				// fails again is broken for good (merge.base_broken says it was rechecked).
				bc.hadFailed, bc.rechecked = false, true
				_ = m.store.EmitEvent(ctx, store.EventInput{
					Event:   "merge.base_rechecked",
					Change:  change,
					Payload: map[string]any{"repo": cp, "sha": onto},
				})
				m.logf("merger: %s's base %s failed on its own: checking it again once, in case what failed has healed", cp, shortSHA(onto))
			}
		}
		if bc.hadFailed {
			// The base this attempt fetched is still a broken sha: it is the repository's broken base
			// again (superseding any other), and its remote is read again only after recheckInterval.
			_, _ = m.store.ResolveBrokenBases(ctx, cp, onto, m.nowFunc())
			_ = m.store.ReopenBrokenBase(ctx, cp, onto)
			m.rechecks[cp] = recheckState{sha: onto, at: time.Now(), remote: onto}
			return nil, &baseHeldError{repo: cp, sha: onto}
		}

		shown, err := gitx.RunGit(ctx, childDir, "show", onto+":repo.config.yaml")
		if err != nil {
			m.baseHealthy(ctx, cp, onto)
			continue
		}
		// A base that still holds the file an older foreman-kit read is judged by the file of the
		// main checkout, where foreman-kit update rewrote it.
		raw, _, err := config.RepoConfigOfToday("", childDir, []byte(shown), onto+":"+config.RepoConfigFile)
		if err != nil {
			m.baseHealthy(ctx, cp, onto)
			continue
		}
		cfg, err := parseChildGateConfig(raw)
		if err != nil {
			m.baseHealthy(ctx, cp, onto)
			continue
		}
		match := proc.MatchRegressionWhen(cfg.RegressionWhen, bc.outsideFiles)
		if !match.Triggered() || !cfg.hasRegressionCommands() {
			m.baseHealthy(ctx, cp, onto)
			continue
		}
		m.updateState(ctx, MergerStateVerifyingBase, change)
		m.logf("merger: %s: %d outside commit(s) on %s, %s matches regression.when: verifying the regression on the base alone", cp, n, shortSHA(onto), match.Hit)
		v, err := m.baseVerdict(ctx, change, bc, BaseCheckRegression, bc.outsideFiles)
		if err != nil {
			return nil, err
		}
		switch v.Status {
		case store.BaseVerdictFailed:
			m.markBaseBroken(ctx, bc)
			return nil, &baseHeldError{repo: cp, sha: onto}
		case baseVerdictInconclusive:
			// Nothing is known about this base: the change goes on with its own tests, as in "change".
			continue
		}
		m.baseHealthy(ctx, cp, onto)
	}
	return bases, nil
}

// baseCheckForGate names the base check that reruns a failing gate: "regression" for a regression
// gate, "gate:<key>" for a fast gate, "" for a failure no command reproduces (an invalid
// repo.config.yaml, a missing env_script).
func baseCheckForGate(gate string) string {
	switch {
	case strings.HasPrefix(gate, "regression:"):
		return BaseCheckRegression
	case gate == "config" || gate == "env_script" || gate == "":
		return ""
	}
	return BaseCheckGatePrefix + gate
}

// acceptedOnBase reports whether failing gate f of child bc, whose base a human accepted (directly, or
// an ancestor with no outside commit since), is covered by that acceptance (acceptedCovers); when it is
// not, the second result says why.
func (m *Merger) acceptedOnBase(ctx context.Context, bc *childBase, f GateFailure, output string) (bool, string) {
	check := baseCheckForGate(f.Gate)
	if check == "" {
		return false, ""
	}
	sha := bc.acceptedSHA
	if sha == "" {
		sha = bc.onto
	}
	v, err := m.store.GetBaseVerification(ctx, bc.cp, sha, check)
	if err != nil || v == nil || v.Status != store.BaseVerdictAccepted {
		return false, ""
	}
	return acceptedCovers(v, filepath.Base(bc.cp)+":"+f.Gate, output)
}

// acceptedCovers reports whether accepted verdict v covers the failure of the gate labelled label in a
// gate run's output. A human accepts the tests that failed on that base, not the whole gate: when the
// run names failing tests the accepted base did not, they are the change's, and the second result says
// which. When the accepted base's output named no test in a known format, the gate is accepted whole.
func acceptedCovers(v *store.BaseVerification, label, output string) (bool, string) {
	baseIDs := FailingTests(v.Excerpt)
	if len(baseIDs) == 0 {
		return true, ""
	}
	changeIDs := FailingTests(gateSegment(output, label))
	if len(changeIDs) == 0 {
		return false, fmt.Sprintf("%s fails differently than on the accepted base %s (no failing test named): this change's failure", label, shortSHA(v.BaseSHA))
	}
	known := make(map[string]bool, len(baseIDs))
	for _, id := range baseIDs {
		known[id] = true
	}
	var extra []string
	for _, id := range changeIDs {
		if !known[id] {
			extra = append(extra, id)
		}
	}
	if len(extra) > 0 {
		return false, fmt.Sprintf("%s: besides what failed on the accepted base %s, this change fails %s", label, shortSHA(v.BaseSHA), strings.Join(extra, ", "))
	}
	return true, ""
}

// acceptedBaseFor returns the newest base of cp a human accepted that base is or descends from, or "".
// With strict, base itself does not count (the caller checks it). The caller has established that no
// commit was pushed outside the pipeline since, so its failing checks are the accepted ones or fewer.
func acceptedBaseFor(ctx context.Context, st *store.Store, cp, childDir, base string, strict bool) string {
	if st == nil || base == "" {
		return ""
	}
	verdicts, err := st.ListBaseVerifications(ctx, cp, "")
	if err != nil {
		return ""
	}
	best, bestAt := "", ""
	for _, v := range verdicts {
		if v.Status != store.BaseVerdictAccepted || v.ResolvedAt != "" || (strict && v.BaseSHA == base) || v.AcceptedAt < bestAt {
			continue
		}
		if v.BaseSHA != base {
			if _, err := gitx.RunGit(ctx, childDir, "merge-base", "--is-ancestor", v.BaseSHA, base); err != nil {
				continue
			}
		}
		best, bestAt = v.BaseSHA, v.AcceptedAt
	}
	return best
}

// carriedAcceptance returns the newest accepted base of cp that onto strictly descends from, or "".
func (m *Merger) carriedAcceptance(ctx context.Context, cp, childDir, onto string) string {
	return acceptedBaseFor(ctx, m.store, cp, childDir, onto, true)
}

// failuresAcceptedOnBase reports whether every failure of a worker's gate run is covered by the
// acceptance of its child's base (the gitlink at baseRef, or an ancestor a human accepted): after
// foreman-kit base accept, the base the next changes build on still fails those tests, and without
// this every one of them would spend its repair rounds on them and end as gate_failure. It returns
// the accepted gates, or why one is not.
func failuresAcceptedOnBase(ctx context.Context, st *store.Store, wt, baseRef string, res GateRunResult) (bool, []string, []string) {
	if st == nil || len(res.Failures) == 0 {
		return false, nil, nil
	}
	var accepted, why []string
	for _, f := range res.Failures {
		ok, reason := AcceptedBaseFailure(ctx, st, wt, baseRef, f, res.Output)
		if !ok {
			if reason != "" {
				why = append(why, reason)
			}
			return false, nil, why
		}
		accepted = append(accepted, filepath.Base(f.Child)+":"+f.Gate)
	}
	return true, accepted, nil
}

// AcceptedBaseFailure reports whether failure f of a gate run in the worktree wt (output: the run's
// output) is covered by the acceptance of its child's base (the gitlink at baseRef, or an ancestor a
// human accepted, not expired), as acceptedCovers decides; when it is not, the second result says
// why ("" when no acceptance applies at all). foreman-kit gates uses it too, so that an agent running
// the gates sees a failure its accepted base has as accepted, not as its own.
func AcceptedBaseFailure(ctx context.Context, st *store.Store, wt, baseRef string, f GateFailure, output string) (bool, string) {
	check := baseCheckForGate(f.Gate)
	if st == nil || f.Child == "" || check == "" {
		return false, ""
	}
	wtRepo, err := gitx.Open(wt)
	if err != nil {
		return false, ""
	}
	base, err := wtRepo.TreeEntryHash(baseRef, f.Child)
	if err != nil {
		return false, ""
	}
	childDir := filepath.Join(wt, filepath.FromSlash(f.Child))
	sha := acceptedBaseFor(ctx, st, f.Child, childDir, base, false)
	if sha == "" {
		return false, ""
	}
	v, err := st.GetBaseVerification(ctx, f.Child, sha, check)
	if err != nil || v == nil || v.Status != store.BaseVerdictAccepted {
		return false, ""
	}
	return acceptedCovers(v, filepath.Base(f.Child)+":"+f.Gate, output)
}

// attributeGateFailure decides, for a failed gate run, whether the base or the change broke the
// failing gates of the children whose base holds outside commits, by rerunning each on the base alone.
// It returns nil when every failure is a check that fails on a base a human accepted, so the change
// integrates (merge.gates_accepted); a *baseHeldError when every failure belongs to such children and
// every one of them also fails on the base alone; otherwise a gate_failure BlockedError whose details
// name the gates that pass on the base alone. Failures of the OpenSpec layer and of children without
// outside commits are never attributed, nor are the unaccepted failures of an accepted base.
func (m *Merger) attributeGateFailure(ctx context.Context, change string, gateRes GateRunResult, bases map[string]*childBase) error {
	byChild := make(map[string][]GateFailure)
	allAttributable := true
	allAccepted := len(gateRes.Failures) > 0
	var acceptedGates, notAccepted []string
	for _, f := range gateRes.Failures {
		bc := bases[f.Child]
		switch {
		case f.Child == "" || bc == nil:
			allAttributable, allAccepted = false, false
		case bc.accepted:
			allAttributable = false
			if ok, why := m.acceptedOnBase(ctx, bc, f, gateRes.Output); ok {
				acceptedGates = append(acceptedGates, filepath.Base(f.Child)+":"+f.Gate)
			} else {
				allAccepted = false
				if why != "" {
					notAccepted = append(notAccepted, why)
				}
			}
		default:
			allAccepted = false
			byChild[f.Child] = append(byChild[f.Child], f)
		}
	}
	if allAccepted {
		m.logf("merger: %s: %s also fail(s) on a base a human accepted: integrating anyway", change, strings.Join(acceptedGates, " "))
		_ = m.store.EmitEvent(ctx, store.EventInput{
			Event:   "merge.gates_accepted",
			Change:  change,
			Payload: map[string]any{"gates": acceptedGates},
		})
		return nil
	}
	details := tailLines(gateRes.Output, 120)
	for _, why := range notAccepted {
		m.logf("merger: %s: %s", change, why)
		details += "\n---- [info] " + why
	}
	if len(byChild) == 0 {
		return NewBlockedError("gate_failure", details)
	}
	m.updateState(ctx, MergerStateVerifyingBase, change)
	var cps []string
	for cp := range byChild {
		cps = append(cps, cp)
	}
	sort.Strings(cps)
	var passOnBase, notRun []string
	var held *baseHeldError
	for _, cp := range cps {
		bc := bases[cp]
		// A failing fast gate reruns with the files it saw at merge, the change's own (as verify measures
		// them): a gate that runs only the tests related to FOREMANKIT_CHANGED_FILES then runs the same tests
		// on the base alone. The regression, the whole suite, reruns with the outside files.
		ownFiles := diffNames(ctx, bc.childDir, bc.onto, bc.rebased)
		verdicts := make(map[string]string)
		allFail := true
		for _, f := range byChild[cp] {
			check := baseCheckForGate(f.Gate)
			if check == "" {
				allFail = false
				continue
			}
			status, ok := verdicts[check]
			if !ok {
				files := ownFiles
				if check == BaseCheckRegression {
					files = bc.outsideFiles
				}
				m.logf("merger: %s: %s fails at merge with %d outside commit(s) on %s: rerunning it on the base alone", cp, f.Gate, bc.outsideCommits, shortSHA(bc.onto))
				v, err := m.baseVerdict(ctx, change, bc, check, files)
				if err != nil {
					return err
				}
				status = v.Status
				verdicts[check] = status
			}
			switch status {
			case store.BaseVerdictFailed:
			case baseVerdictInconclusive:
				// Unknown on the base alone: the failure stays the change's (a retry tells again).
				allFail = false
				notRun = append(notRun, filepath.Base(cp)+":"+f.Gate)
			default:
				allFail = false
				passOnBase = append(passOnBase, filepath.Base(cp)+":"+f.Gate)
			}
		}
		m.markBaseBroken(ctx, bc)
		if !allFail {
			allAttributable = false
		} else if held == nil {
			held = &baseHeldError{repo: cp, sha: bc.onto}
		}
	}
	if allAttributable && held != nil {
		return held
	}
	if len(passOnBase) > 0 {
		details += fmt.Sprintf("\n---- [info] passes on the base alone, so this change broke it: %s", strings.Join(passOnBase, " "))
	}
	if len(notRun) > 0 {
		details += fmt.Sprintf("\n---- [info] could not run on the base alone, so it counts as this change's: %s", strings.Join(notRun, " "))
	}
	return NewBlockedError("gate_failure", details)
}

// baseVerdict returns the verdict of check on bc's base alone, run with files as its
// FOREMANKIT_CHANGED_FILES. The regression's verdict, and a failed or accepted one, is reused: computed at
// most once per repository, sha and check. A passed gate verdict is not: a gate's result depends on the
// files it runs with, the failing change's own, which differ from one change to the next.
func (m *Merger) baseVerdict(ctx context.Context, change string, bc *childBase, check string, files []string) (store.BaseVerification, error) {
	if v, err := m.store.GetBaseVerification(ctx, bc.cp, bc.onto, check); err == nil && v != nil {
		if check == BaseCheckRegression || v.Status != store.BaseVerdictPassed {
			return *v, nil
		}
	}
	res, err := m.runOnBase(ctx, change, bc, check, files)
	if err != nil {
		return store.BaseVerification{}, err
	}
	if err := ctx.Err(); err != nil {
		return store.BaseVerification{}, err // stopped, not judged: never cache it
	}
	if res.Inconclusive {
		m.logf("merger: %s at %s: %s did not run on the base alone (it could not start): no verdict", bc.cp, shortSHA(bc.onto), store.BaseCheckLabel(check))
		return store.BaseVerification{RepoPath: bc.cp, BaseSHA: bc.onto, CheckName: check, Status: baseVerdictInconclusive, ExitCode: res.ExitCode, Excerpt: RedactExcerpt(FailureExcerpt(res.Output), 40, 4000)}, nil
	}
	rec := store.BaseVerification{
		RepoPath:       bc.cp,
		BaseSHA:        bc.onto,
		CheckName:      check,
		Status:         store.BaseVerdictPassed,
		ExitCode:       res.ExitCode,
		PointerSHA:     bc.pointer,
		OutsideCommits: bc.outsideCommits,
		CreatedAt:      m.nowFunc().UTC().Format(time.RFC3339),
	}
	if !res.OK {
		rec.Status = store.BaseVerdictFailed
		if rec.ExitCode == 0 {
			rec.ExitCode = 1
		}
		rec.Excerpt = RedactExcerpt(FailureExcerpt(res.Output), 40, 4000)
	}
	// The verdict points at the gate round that computed it.
	if _, err := m.store.RecordBaseVerification(store.WithGateRound(ctx, res.RoundID), rec); err != nil {
		return store.BaseVerification{}, err
	}
	if v, err := m.store.GetBaseVerification(ctx, bc.cp, bc.onto, check); err == nil && v != nil {
		rec = *v
	}
	if rec.Status == store.BaseVerdictFailed {
		bc.newlyFailed = append(bc.newlyFailed, check)
		bc.excerpt = rec.Excerpt
	}
	m.logf("merger: %s at %s: %s %s on the base alone", bc.cp, shortSHA(bc.onto), store.BaseCheckLabel(check), rec.Status)
	return rec, nil
}

// runOnBase checks child bc and its siblings out at their bases in the integration worktree, runs check
// there through MergerOptions.BaseCheckRunner in BaseCheckSlot, and restores each of them on
// integrate/<change> at its rebased head.
func (m *Merger) runOnBase(ctx context.Context, change string, bc *childBase, check string, files []string) (BaseCheckResult, error) {
	// The base alone: cp and every sibling this attempt rebased at their own base, so the check sees
	// neither the change's commits in cp nor its version of a sibling repository.
	type child struct{ cp, dir, onto, rebased string }
	wt := m.integrationWorktree()
	children := []child{{bc.cp, bc.childDir, bc.onto, bc.rebased}}
	others := make([]string, 0, len(bc.siblings))
	for cp := range bc.siblings {
		others = append(others, cp)
	}
	sort.Strings(others)
	for _, cp := range others {
		sib := bc.siblings[cp]
		children = append(children, child{cp, filepath.Join(wt, filepath.FromSlash(cp)), sib.onto, sib.rebased})
	}
	moved := 0
	restore := func() error {
		var firstErr error
		for _, c := range children[:moved] {
			if _, err := gitx.RunGit(ctx, c.dir, "checkout", "--quiet", "--force", "integrate/"+change); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("merger: %s: cannot restore integrate/%s after the base check: %w", c.cp, change, err)
				}
				continue
			}
			// -x: the base's ignored files (build output, installed dependencies) never reach the
			// change's gates either.
			_, _ = gitx.RunGit(ctx, c.dir, cleanIgnoredArgs()...)
			head, err := gitx.RunGit(ctx, c.dir, "rev-parse", "HEAD")
			if (err != nil || (c.rebased != "" && strings.TrimSpace(head) != c.rebased)) && firstErr == nil {
				firstErr = fmt.Errorf("merger: %s: integrate/%s is at %s after the base check, want %s", c.cp, change, strings.TrimSpace(head), c.rebased)
			}
		}
		return firstErr
	}
	for _, c := range children {
		if _, err := gitx.RunGit(ctx, c.dir, "checkout", "--quiet", "--force", "--detach", c.onto); err != nil {
			if rerr := restore(); rerr != nil {
				return BaseCheckResult{}, fmt.Errorf("merger: %s: cannot check out the base %s: %w (and %v)", c.cp, c.onto, err, rerr)
			}
			return BaseCheckResult{}, fmt.Errorf("merger: %s: cannot check out the base %s: %w", c.cp, c.onto, err)
		}
		moved++
		// -x: ignored files too (node_modules, build output, file databases), so the check never runs
		// with what the change's gates built or installed; its setup gate prepares the base again.
		_, _ = gitx.RunGit(ctx, c.dir, cleanIgnoredArgs()...)
	}
	res, runErr := m.opts.BaseCheckRunner(ctx, BaseCheckRequest{
		Change:       change,
		Worktree:     wt,
		Child:        bc.cp,
		Check:        check,
		ChangedFiles: files,
		Slot:         BaseCheckSlot,
		Timeout:      m.settings.GateTimeout,
	})
	if err := restore(); err != nil {
		return BaseCheckResult{}, err
	}
	if runErr != nil {
		return BaseCheckResult{}, runErr
	}
	return res, nil
}

// markBaseBroken records that bc's base fails on its own after this attempt found new failed verdicts:
// older broken shas of the repository are superseded, and the first time (cp, onto) is found broken it
// emits the single merge.base_broken event and logs one line.
func (m *Merger) markBaseBroken(ctx context.Context, bc *childBase) {
	if len(bc.newlyFailed) == 0 {
		return
	}
	bc.newlyFailed = nil
	_, _ = m.store.ResolveBrokenBases(ctx, bc.cp, bc.onto, m.nowFunc())
	m.rechecks[bc.cp] = recheckState{sha: bc.onto, at: time.Now(), remote: bc.onto}
	if bc.hadFailed {
		return
	}
	bc.hadFailed = true
	var failedChecks []string
	if verdicts, err := m.store.ListBaseVerifications(ctx, bc.cp, bc.onto); err == nil {
		for _, v := range verdicts {
			if v.Status == store.BaseVerdictFailed {
				failedChecks = append(failedChecks, v.CheckName)
			}
		}
	}
	_ = m.store.EmitEvent(ctx, store.EventInput{
		Event: "merge.base_broken",
		Payload: map[string]any{
			"repo":            bc.cp,
			"sha":             bc.onto,
			"pointer":         bc.pointer,
			"outside_commits": bc.outsideCommits,
			"failed_checks":   failedChecks,
			"excerpt":         RedactExcerpt(bc.excerpt, 40, 4000),
			"rechecked":       bc.rechecked,
		},
	})
	b := store.BrokenBase{FailedChecks: failedChecks}
	again := ""
	if bc.rechecked {
		again = " again after its automatic recheck"
	}
	m.logf("merger: BASE BROKEN: %s at %s: %s %s on the base alone%s (%d outside commit(s)); holding its changes until the base moves, or foreman-kit base accept %s %s",
		bc.cp, shortSHA(bc.onto), b.ChecksText(), b.ChecksVerb(), again, bc.outsideCommits, bc.cp, shortSHA(bc.onto))
}

// baseHealthy records that repository cp's base onto does not fail on its own: an older broken base of
// the repository is resolved, and merge.base_recovered is emitted.
func (m *Merger) baseHealthy(ctx context.Context, cp, onto string) {
	resolved, err := m.store.ResolveBrokenBases(ctx, cp, onto, m.nowFunc())
	if err != nil {
		return
	}
	// Bases resolved earlier because the base moved off them (baseMovedOff) are announced now.
	for _, sha := range m.movedOff[cp] {
		if sha != onto && !slices.Contains(resolved, sha) {
			resolved = append(resolved, sha)
		}
	}
	delete(m.movedOff, cp)
	if len(resolved) == 0 {
		return
	}
	sort.Strings(resolved)
	delete(m.rechecks, cp)
	_ = m.store.EmitEvent(ctx, store.EventInput{
		Event: "merge.base_recovered",
		Payload: map[string]any{
			"repo":     cp,
			"sha":      onto,
			"previous": resolved,
		},
	})
	m.logf("merger: %s's base recovered at %s (was broken at %s): its changes integrate again", cp, shortSHA(onto), shortSHA(resolved[len(resolved)-1]))
}

// countCommits returns how many commits from..to holds (git rev-list --count), 0 when unknown.
func countCommits(ctx context.Context, dir, from, to string) int {
	if from == "" || to == "" || from == to {
		return 0
	}
	out, err := gitx.RunGit(ctx, dir, "rev-list", "--count", from+".."+to)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// diffNames lists, unquoted, the files that differ between from and to (git diff --name-only -z
// --no-renames): a rename lists its source and its destination, so a file moved out of a
// regression.when path counts there, like a deletion.
func diffNames(ctx context.Context, dir, from, to string) []string {
	if from == "" || to == "" {
		return nil
	}
	out, err := gitx.RunGit(ctx, dir, "diff", "--name-only", "-z", "--no-renames", from, to, "--")
	if err != nil {
		return nil
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	return files
}

// heldByBrokenBases maps each ready change that a broken base holds (merge.outside_commits:
// verify) to how foreman-kit dispatch --dry-run explains it: "built: held, repos/core's base fails its
// regression". It reads only the database and the umbrella's object database.
func heldByBrokenBases(ctx context.Context, root string, st *store.Store, ready []store.ReadyManifest, mode string) map[string]string {
	if st == nil || mode != config.OutsideCommitsVerify || len(ready) == 0 {
		return nil
	}
	broken, err := st.ListBrokenBases(ctx)
	if err != nil || len(broken) == 0 {
		return nil
	}
	byRepo := make(map[string]store.BrokenBase, len(broken))
	for _, b := range broken {
		byRepo[b.Repo] = b
	}
	repo, err := gitx.Open(root)
	if err != nil {
		return nil
	}
	baseRef := "HEAD"
	settings := LoadPipelineSettings(root)
	for _, ref := range []string{settings.Remote + "/" + settings.BaseBranch, settings.BaseBranch} {
		if _, err := gitx.RunGit(ctx, root, "rev-parse", "--verify", "--quiet", ref); err == nil {
			baseRef = ref
			break
		}
	}
	held := make(map[string]string)
	for _, r := range ready {
		head := strings.TrimSpace(r.HeadSHA)
		if head == "" {
			head = r.Branch
		}
		touched, err := repo.TouchedChildren(baseRef, head)
		if err != nil {
			continue
		}
		for _, cp := range touched {
			if b, ok := byRepo[cp]; ok && r.ChildrenPushed[cp] == "" {
				held[r.ChangeName] = fmt.Sprintf("built: held, %s's base fails its %s", cp, b.ChecksText())
				break
			}
		}
	}
	return held
}

// cleanIgnoredArgs is git clean of untracked and ignored files around a check of a base alone, keeping
// the local config files (localChildConfigs) the gates need.
func cleanIgnoredArgs() []string {
	args := []string{"clean", "-fdqx"}
	for _, rel := range localChildConfigs {
		args = append(args, "-e", rel)
	}
	return args
}

// GateAcceptor returns the GatesOptions.AcceptFailures of foreman-kit gates run in the worktree wt:
// each failed "<repo>:<gate>" covered by the acceptance of that repository's base, with the note an
// agent reads (the failure is the base's, a human accepted it, the change need not fix it).
func GateAcceptor(st *store.Store, wt string) func(ctx context.Context, failed []string, childByName map[string]string, base, output string) map[string]string {
	return func(ctx context.Context, failed []string, childByName map[string]string, base, output string) map[string]string {
		out := map[string]string{}
		for _, label := range failed {
			name, gate, ok := strings.Cut(label, ":")
			child := childByName[name]
			if !ok || child == "" {
				continue
			}
			if covered, _ := AcceptedBaseFailure(ctx, st, wt, base, GateFailure{Child: child, Gate: gate}, output); covered {
				out[label] = "fails only with the tests that fail on " + child + "'s base, which a human accepted (foreman-kit base status): not this change's to fix"
			}
		}
		return out
	}
}
