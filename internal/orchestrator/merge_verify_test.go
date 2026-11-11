package orchestrator

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// coreLogLine is one line of $MARKER_DIR/core.log: a run of core's test gate or regression.
type coreLogLine struct {
	gate, slot, head, files string
}

func (sb *mcfSandbox) coreLog(t *testing.T) []coreLogLine {
	t.Helper()
	var out []coreLogLine
	for _, l := range sb.marker("core.log") {
		var ll coreLogLine
		rest := l
		if i := strings.Index(rest, " files="); i >= 0 {
			ll.files = strings.TrimSpace(rest[i+len(" files="):])
			rest = rest[:i]
		}
		for _, f := range strings.Fields(rest) {
			k, v, _ := strings.Cut(f, "=")
			switch k {
			case "gate":
				ll.gate = v
			case "slot":
				ll.slot = v
			case "head":
				ll.head = v
			}
		}
		out = append(out, ll)
	}
	return out
}

// coreRuns returns core.log's runs of gate in the merger: its gates (slot 99) and its checks of a base alone (BaseCheckSlot).
func (sb *mcfSandbox) coreRuns(t *testing.T, gate string) []coreLogLine {
	t.Helper()
	var out []coreLogLine
	for _, l := range sb.coreLog(t) {
		if l.gate == gate && (l.slot == "99" || l.slot == strconv.Itoa(BaseCheckSlot)) {
			out = append(out, l)
		}
	}
	return out
}

func (sb *mcfSandbox) newMerger(t *testing.T, opts MergerOptions) *Merger {
	t.Helper()
	opts.Root = sb.umbrella
	opts.Store = sb.st
	m, err := NewMerger(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func runMergerOnce(t *testing.T, m *Merger) {
	t.Helper()
	if err := m.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (sb *mcfSandbox) events(t *testing.T, name string) []store.EventRecord {
	t.Helper()
	evs, err := sb.st.QueryEvents(context.Background(), store.EventFilter{Event: name})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func (sb *mcfSandbox) readyNames(t *testing.T) []string {
	t.Helper()
	ready, err := sb.st.ListReadyQueue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range ready {
		names = append(names, r.ChangeName)
	}
	return names
}

// requireHeld checks that every change is still in the merge queue, never quarantined (so no retry spent).
func (sb *mcfSandbox) requireHeld(t *testing.T, changes ...string) {
	t.Helper()
	ready := sb.readyNames(t)
	for _, c := range changes {
		found := false
		for _, r := range ready {
			found = found || r == c
		}
		if !found {
			q, _ := sb.st.GetQuarantine(context.Background(), c)
			t.Fatalf("%s left the merge queue: ready=%v quarantine=%+v", c, ready, q)
		}
		if q, _ := sb.st.GetQuarantine(context.Background(), c); q != nil {
			t.Fatalf("%s has a quarantine row (status %s, reason %s, retries %d)", c, q.Status, q.Reason, q.Retries)
		}
	}
	if qs, _ := sb.st.ListQuarantine(context.Background(), ""); len(qs) != 0 {
		t.Fatalf("expected no quarantine rows, got %+v", qs)
	}
}

func (sb *mcfSandbox) verdict(t *testing.T, sha, check string) *store.BaseVerification {
	t.Helper()
	v, err := sb.st.GetBaseVerification(context.Background(), "repos/core", sha, check)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// a. verify, a hotfix of a file that triggers nothing: no base check, the change integrates, and no
// base record exists.
func TestMergeVerifyHotfixThatTriggersNothingRunsNoBaseCheck(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.buildCoreChange(t, "core-feature")
	hotfix := sb.pushCoreCommit(t, "notes.txt", "release notes\n", "docs(core): release notes")

	req, out := sb.mergerRun(t)
	if req.OutsideCommits != "verify" {
		t.Fatalf("merge.outside_commits as the kit ships it = %q, want verify", req.OutsideCommits)
	}
	for _, l := range sb.coreLog(t) {
		if l.head == hotfix {
			t.Fatalf("a check ran on the base alone for a hotfix that triggers nothing: %+v", l)
		}
	}
	if got := sb.marker("core.regression"); len(got) != 0 {
		t.Fatalf("the regression ran: %v", got)
	}
	if line := mergerLine(t, sb.marker("core.ran")); line != "slot=99 files=feature.txt" {
		t.Fatalf("core's gates at merge got %q, want only the change's feature.txt", line)
	}
	if list, _ := sb.st.ListBaseVerifications(context.Background(), "repos/core", ""); len(list) != 0 {
		t.Fatalf("expected no base record, got %+v", list)
	}
	if !strings.Contains(out, "is verify, so only the change's own commits count") {
		t.Fatalf("expected the gate output to explain the hotfix, got:\n%s", out)
	}
	sb.requireIntegrated(t, "core-feature")
}

// b. verify, a hotfix that triggers regression.when and whose regression passes on the base: the
// regression runs exactly once, on the base alone, with the hotfix's file as FOREMANKIT_CHANGED_FILES; the
// change's gates get only the change's files; the change integrates and a passed verdict is cached.
func TestMergeVerifyRunsTheRegressionOnceOnTheBaseAlone(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.buildCoreChange(t, "core-feature")
	hotfix := sb.pushCoreHotfix(t)

	_, _ = sb.mergerRun(t)
	regs := sb.coreRuns(t, "regression")
	if len(regs) != 1 || regs[0].head != hotfix || regs[0].files != "config/locales/es.yml" {
		t.Fatalf("expected one regression run on the base alone (%s) with the hotfix's file, got %+v", hotfix, sb.coreLog(t))
	}
	if got := sb.marker("core.regression"); len(got) != 1 || got[0] != "slot="+strconv.Itoa(BaseCheckSlot) {
		t.Fatalf("expected the regression to run exactly once, in the merger, got %v", got)
	}
	if line := mergerLine(t, sb.marker("core.ran")); line != "slot=99 files=feature.txt" {
		t.Fatalf("core's gates at merge got %q, want only the change's feature.txt", line)
	}
	tests := sb.coreRuns(t, "test")
	if len(tests) != 1 || tests[0].head == hotfix {
		t.Fatalf("expected the change's test gate to run once with the change, got %+v", tests)
	}
	if v := sb.verdict(t, hotfix, BaseCheckRegression); v == nil || v.Status != store.BaseVerdictPassed || v.OutsideCommits != 1 {
		t.Fatalf("expected a passed regression verdict for %s, got %+v", hotfix, v)
	}
	if evs := sb.events(t, "merge.base_broken"); len(evs) != 0 {
		t.Fatalf("unexpected merge.base_broken: %+v", evs)
	}
	sb.requireIntegrated(t, "core-feature")
}

// c and d. verify, a hotfix that breaks the regression: both ready changes of core are held (still in
// the merge queue, no quarantine, no retry spent) behind a single alert while a change of mobile
// integrates, the base regression runs once, status and progress show the broken base; once a second
// hotfix fixes the base and the recheck interval passes, both integrate and merge.base_recovered is
// emitted.
func TestMergeVerifyHoldsChangesOnABrokenBaseUntilItMoves(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.buildCoreChanges(t, map[string]map[string]string{
		"core-feature":   {"feature.txt": "core payload\n"},
		"core-other":     {"other.txt": "other payload\n"},
		"mobile-feature": {"repos/mobile/mobile_feature.txt": "mobile payload\n"},
	})
	broken := sb.pushCoreCommit(t, "config/locales/BROKEN", "broken\n", "fix(core): a hotfix that breaks the regression")
	ctx := context.Background()

	var mergerLog bytes.Buffer
	m := sb.newMerger(t, MergerOptions{BaseRecheckInterval: time.Hour, Out: &mergerLog})
	runMergerOnce(t, m)

	sb.requireHeld(t, "core-feature", "core-other")
	sb.requireIntegrated(t, "mobile-feature")
	if line := mergerLine(t, sb.marker("mobile.ran")); line != "slot=99 files=mobile_feature.txt" {
		t.Fatalf("mobile's gates at merge got %q", line)
	}
	regs := sb.coreRuns(t, "regression")
	if len(regs) != 1 || regs[0].head != broken || regs[0].files != "config/locales/BROKEN" {
		t.Fatalf("expected the regression once on the broken base alone, got %+v", sb.coreLog(t))
	}
	for _, l := range sb.marker("core.ran") {
		if strings.HasPrefix(l, "slot=99 ") {
			t.Fatalf("a held change's gates ran: %v", sb.marker("core.ran"))
		}
	}
	evs := sb.events(t, "merge.base_broken")
	if len(evs) != 1 {
		t.Fatalf("expected exactly one merge.base_broken, got %+v", evs)
	}
	p := evs[0].Payload
	if p["repo"] != "repos/core" || p["sha"] != broken || p["outside_commits"] != float64(1) || !reflect.DeepEqual(p["failed_checks"], []any{"regression"}) || !strings.Contains(p["excerpt"].(string), "BROKEN is present") {
		t.Fatalf("unexpected merge.base_broken payload: %+v", p)
	}
	if v := sb.verdict(t, broken, BaseCheckRegression); v == nil || v.Status != store.BaseVerdictFailed {
		t.Fatalf("expected a failed regression verdict, got %+v", v)
	}
	if strings.Count(mergerLog.String(), "BASE BROKEN") != 1 {
		t.Fatalf("expected one BASE BROKEN line in the merger log, got:\n%s", mergerLog.String())
	}

	short := broken[:8]
	var status bytes.Buffer
	if err := cycle.Status(ctx, sb.umbrella, sb.st, &status); err != nil {
		t.Fatal(err)
	}
	wantLine := "BASE BROKEN: repos/core at " + short + " since "
	if !strings.Contains(status.String(), wantLine) ||
		!strings.Contains(status.String(), ": regression fails on the base alone (1 outside commit(s)); holding: core-feature, core-other; foreman-kit base accept repos/core "+short+" to integrate anyway") {
		t.Fatalf("expected the broken base in foreman-kit status, got:\n%s", status.String())
	}
	prog, err := cycle.Progress(ctx, sb.umbrella, sb.st, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if nh, _ := prog["needs_human"].(map[string]any); nh == nil || len(nh["broken_bases"].([]map[string]any)) != 1 {
		t.Fatalf("expected one broken base in needs_human, got %+v", prog["needs_human"])
	}
	text := cycle.FormatProgressText(prog)
	if i := strings.Index(text, "== Needs a human"); i < 0 || !strings.Contains(text[i:], "repos/core's base at "+short+" fails its regression on its own (1 outside commit(s)), holding core-feature, core-other") {
		t.Fatalf("expected the broken base under Needs a human, got:\n%s", text)
	}

	disp, err := NewDispatcher(DispatcherOptions{Root: sb.umbrella, Store: sb.st, MaxWorkers: 1})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := disp.DryRun(ctx)
	_ = disp.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range rep.Items {
		if (it.Change == "core-feature" || it.Change == "core-other") && it.Why != "built: held, repos/core's base fails its regression" {
			t.Fatalf("dispatch --dry-run explains %s as %q", it.Change, it.Why)
		}
	}

	// Another merger (a CLI run, a restarted daemon) reads the held state from the database: it
	// rechecks the remote base, finds it unchanged, and reruns nothing.
	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	sb.requireHeld(t, "core-feature", "core-other")
	if got := sb.coreRuns(t, "regression"); len(got) != 1 {
		t.Fatalf("the regression ran again on the same broken base: %+v", got)
	}
	if evs := sb.events(t, "merge.base_broken"); len(evs) != 1 {
		t.Fatalf("expected still one merge.base_broken, got %d", len(evs))
	}

	// d. A second hotfix fixes the base (and an accent, so the new base gets its own verdict).
	fixed := sb.pushCoreChanges(t, map[string]string{"config/locales/es.yml": "es:\n  saludo: \"Canción\"\n"}, []string{"config/locales/BROKEN"}, "fix(core): remove BROKEN")

	// Before the recheck interval passes, nothing happens.
	runMergerOnce(t, m)
	sb.requireHeld(t, "core-feature", "core-other")
	if got := sb.coreRuns(t, "regression"); len(got) != 1 {
		t.Fatalf("the merger rechecked before the interval: %+v", got)
	}

	m.opts.BaseRecheckInterval = 20 * time.Millisecond
	time.Sleep(50 * time.Millisecond)
	runMergerOnce(t, m)
	sb.requireIntegrated(t, "core-feature")
	sb.requireIntegrated(t, "core-other")
	if v := sb.verdict(t, fixed, BaseCheckRegression); v == nil || v.Status != store.BaseVerdictPassed {
		t.Fatalf("expected a passed regression verdict for the fixed base, got %+v", v)
	}
	if got := sb.coreRuns(t, "regression"); len(got) != 2 || got[1].head != fixed || got[1].files != "config/locales/es.yml" {
		t.Fatalf("expected one more regression run, on the fixed base alone, got %+v", got)
	}
	recovered := sb.events(t, "merge.base_recovered")
	if len(recovered) != 1 || recovered[0].Payload["sha"] != fixed || !reflect.DeepEqual(recovered[0].Payload["previous"], []any{broken}) {
		t.Fatalf("expected one merge.base_recovered for %s, got %+v", fixed, recovered)
	}
	if bases := cycle.BrokenBases(ctx, sb.umbrella, sb.st); len(bases) != 0 {
		t.Fatalf("expected no broken base left, got %+v", bases)
	}
	if evs := sb.events(t, "merge.base_broken"); len(evs) != 1 {
		t.Fatalf("expected still one merge.base_broken, got %d", len(evs))
	}
}

// e. Accepting a broken base makes the held change integrate with its own tests only, without
// running the base regression again.
func TestMergeVerifyAcceptedBaseIntegratesWithTheChangesOwnTests(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.buildCoreChange(t, "core-feature")
	broken := sb.pushCoreCommit(t, "config/locales/BROKEN", "broken\n", "fix(core): a hotfix that breaks the regression")
	t.Setenv("USER", "alice")
	ctx := context.Background()

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	sb.requireHeld(t, "core-feature")

	var out, errOut bytes.Buffer
	if code := cycle.RunBaseCLI(ctx, sb.umbrella, sb.st, []string{"accept", "repos/core"}, &out, &errOut); code != 0 {
		t.Fatalf("base accept failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "accepted: repos/core at "+broken[:8]) {
		t.Fatalf("unexpected base accept output: %s", out.String())
	}
	accepted := sb.events(t, "merge.base_accepted")
	if len(accepted) != 1 || accepted[0].Payload["sha"] != broken || accepted[0].Payload["by"] != "alice" {
		t.Fatalf("expected one merge.base_accepted by alice, got %+v", accepted)
	}
	if v := sb.verdict(t, broken, BaseCheckRegression); v == nil || v.Status != store.BaseVerdictAccepted || v.AcceptedBy != "alice" {
		t.Fatalf("expected the verdict accepted by alice, got %+v", v)
	}

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	sb.requireIntegrated(t, "core-feature")
	if got := sb.coreRuns(t, "regression"); len(got) != 1 {
		t.Fatalf("the base regression ran again after the base was accepted: %+v", got)
	}
	if line := mergerLine(t, sb.marker("core.ran")); line != "slot=99 files=feature.txt" {
		t.Fatalf("core's gates at merge got %q, want only the change's feature.txt", line)
	}
}

// f. Attribution, the base broke it: a hotfix outside regression.when breaks core's test gate. The
// change's gates fail, the rerun on the base alone fails too, so the change is held, not quarantined.
func TestMergeVerifyHoldsAChangeWhoseFailingGateAlsoFailsOnTheBaseAlone(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.buildCoreChange(t, "core-feature")
	hotfix := sb.pushCoreChanges(t, map[string]string{"hotfix.txt": "hotfix\n", "BROKEN": "broken\n"}, nil, "fix(core): a hotfix that breaks the tests")
	ctx := context.Background()

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	sb.requireHeld(t, "core-feature")
	if got := sb.coreRuns(t, "regression"); len(got) != 0 {
		t.Fatalf("a hotfix outside regression.when ran the regression: %+v", got)
	}
	tests := sb.coreRuns(t, "test")
	if len(tests) != 2 || tests[0].head == hotfix || tests[0].files != "feature.txt" || tests[1].head != hotfix || tests[1].files != "feature.txt" {
		t.Fatalf("expected the change's test run, then one on the base alone with the change's own files, got %+v", tests)
	}
	if v := sb.verdict(t, hotfix, BaseCheckGatePrefix+"test"); v == nil || v.Status != store.BaseVerdictFailed {
		t.Fatalf("expected a failed gate:test verdict, got %+v", v)
	}
	evs := sb.events(t, "merge.base_broken")
	if len(evs) != 1 || !reflect.DeepEqual(evs[0].Payload["failed_checks"], []any{"gate:test"}) {
		t.Fatalf("expected one merge.base_broken for gate:test, got %+v", evs)
	}
	var status bytes.Buffer
	_ = cycle.Status(ctx, sb.umbrella, sb.st, &status)
	if !strings.Contains(status.String(), "BASE BROKEN: repos/core at "+hotfix[:8]) || !strings.Contains(status.String(), "test gate fails on the base alone") {
		t.Fatalf("expected the broken base in foreman-kit status, got:\n%s", status.String())
	}
}

// g. Attribution, the change broke it: core's test gate fails only with both the change's feature.txt
// and the hotfix's hotfix.txt. It passes on the base alone, so the change is quarantined as
// gate_failure and the details say which gate passes on the base alone.
func TestMergeVerifyQuarantinesAChangeWhoseFailingGatePassesOnTheBaseAlone(t *testing.T) {
	sb := newMCFSandboxWith(t, mcfOptions{coreTestFailsWhen: "[ -e feature.txt ] && [ -e hotfix.txt ]"})
	sb.buildCoreChange(t, "core-feature")
	hotfix := sb.pushCoreCommit(t, "hotfix.txt", "hotfix\n", "fix(core): an innocent hotfix")
	ctx := context.Background()

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	q, err := sb.st.GetQuarantine(ctx, "core-feature")
	if err != nil || q == nil || q.Reason != "gate_failure" {
		t.Fatalf("expected core-feature quarantined as gate_failure, got %+v (err=%v)", q, err)
	}
	if !strings.Contains(q.Details, "passes on the base alone, so this change broke it: core:test") {
		t.Fatalf("expected the details to name the gate passing on the base alone, got:\n%s", q.Details)
	}
	for _, r := range sb.readyNames(t) {
		if r == "core-feature" {
			t.Fatalf("a quarantined change stayed in the merge queue")
		}
	}
	tests := sb.coreRuns(t, "test")
	if len(tests) != 2 || tests[1].head != hotfix || tests[1].files != "feature.txt" {
		t.Fatalf("expected the change's test run, then one on the base alone, got %+v", tests)
	}
	if v := sb.verdict(t, hotfix, BaseCheckGatePrefix+"test"); v == nil || v.Status != store.BaseVerdictPassed {
		t.Fatalf("expected a passed gate:test verdict, got %+v", v)
	}
	if evs := sb.events(t, "merge.base_broken"); len(evs) != 0 {
		t.Fatalf("unexpected merge.base_broken: %+v", evs)
	}
	if bases, _ := sb.st.ListBrokenBases(ctx); len(bases) != 0 {
		t.Fatalf("expected no broken base, got %+v", bases)
	}
}

// The merger skips a held change cheaply and does not count it, so Serve never spins on it.
func TestMergeVerifyHeldChangeIsNotCountedAsHandled(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.buildCoreChange(t, "core-feature")
	_ = sb.pushCoreCommit(t, "config/locales/BROKEN", "broken\n", "fix(core): a hotfix that breaks the regression")

	m := sb.newMerger(t, MergerOptions{BaseRecheckInterval: time.Hour, Out: &bytes.Buffer{}})
	ctx := context.Background()
	if n := m.drainQueue(ctx); n != 1 {
		t.Fatalf("expected the first drain to attempt the change once, got %d", n)
	}
	sb.requireHeld(t, "core-feature")
	if n := m.drainQueue(ctx); n != 0 {
		t.Fatalf("expected a held change to be skipped without counting, got %d", n)
	}
	if _, err := os.Stat(m.integrationWorktree()); err != nil {
		t.Fatal(err)
	}
}
