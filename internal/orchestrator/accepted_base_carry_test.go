package orchestrator

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/proc"
)

// TestAnAcceptedBaseCarriesToTheChangesBuiltOnIt reproduces a real run: a person pushed a broken test
// straight to core's main, the merger held core's change, and a human ran foreman-kit base accept. The
// held change integrated; but the merger's own push is a new base holding the same failing test, with
// no outside commit, so nothing remembered the acceptance: the next change failed that test in its
// BUILD and at merge, as if it had broken it. The acceptance now carries to the bases built on the
// accepted one, for the tests that failed there only: a change that breaks another test is stopped.
func TestAnAcceptedBaseCarriesToTheChangesBuiltOnIt(t *testing.T) {
	// Core's test gate prints a Go-style failure for every BROKEN_* file at its root.
	sb := newMCFSandboxWith(t, mcfOptions{
		outsideCommits:    "verify",
		coreTestFailsWhen: `{ found=1; for f in BROKEN_*; do [ -e "$f" ] || continue; echo "--- FAIL: Test$f (0.00s)"; found=0; done; [ $found = 0 ]; }`,
	})
	ctx := context.Background()

	if n := sb.runCoreDispatch(t, "add-a", map[string]string{"a.txt": "a\n"}); n != 1 {
		t.Fatalf("add-a: %d BUILD calls", n)
	}
	hotfix := sb.pushCoreCommit(t, "BROKEN_A", "x\n", "test: a test pushed by hand that fails")
	sb.mergerRun(t)
	broken, _ := sb.st.ListBrokenBases(ctx)
	if len(broken) != 1 || broken[0].SHA != hotfix {
		t.Fatalf("broken bases = %+v, want core at the hotfix", broken)
	}
	if !strings.Contains(broken[0].Excerpt, "--- FAIL: TestBROKEN_A") {
		t.Fatalf("the alert does not name the failing test: %q", broken[0].Excerpt)
	}
	if _, err := sb.st.AcceptBaseVerifications(ctx, "repos/core", hotfix, "tester", time.Now()); err != nil {
		t.Fatal(err)
	}
	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-a")

	// An agent running foreman-kit gates on that base (as the BUILD prompt asks) sees the failure as
	// accepted, not as its own: it used to stop and ask a human about a test it never touched.
	agentWT := filepath.Join(sb.tmp, "agent-view")
	mcfGit(t, sb.tmp, "clone", "-q", "-b", "master", sb.umbrellaBare, agentWT)
	mcfGit(t, agentWT, "submodule", "update", "--init", "-q", "repos/core")
	var gout, gerr bytes.Buffer
	if rc := proc.RunGatesCLIWith(ctx, agentWT, sb.st, []string{"--repo", "core"}, &gout, &gerr, GateAcceptor(sb.st, agentWT)); rc != 0 || !strings.Contains(gout.String(), "---- [accepted] core:test") {
		t.Fatalf("gates with the acceptor: rc=%d\n%s%s", rc, gout.String(), gerr.String())
	}
	gout.Reset()
	if rc := proc.RunGatesCLIWith(ctx, agentWT, sb.st, []string{"--repo", "core"}, &gout, &gerr, nil); rc == 0 {
		t.Fatalf("gates without the acceptor should fail on the accepted base:\n%s", gout.String())
	}

	// The next change builds on the merger's push: same failing test, no outside commit.
	if n := sb.runCoreDispatch(t, "add-b", map[string]string{"b.txt": "b\n"}); n != 1 {
		t.Fatalf("add-b: %d BUILD calls", n)
	}
	var accepted int
	_ = sb.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE event = 'change.gates_accepted' AND change_name = 'add-b'`).Scan(&accepted)
	if accepted == 0 {
		qs, _ := sb.st.ListQuarantine(ctx, "")
		t.Fatalf("add-b's BUILD gates were not accepted on the accepted base; quarantine=%+v", qs)
	}
	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-b")
	if broken, _ := sb.st.ListBrokenBases(ctx); len(broken) != 0 {
		t.Fatalf("a new broken base was raised for the merger's own push: %+v", broken)
	}

	// A change that breaks another test of the same gate is not covered by the acceptance.
	sb.runCoreDispatch(t, "add-c", map[string]string{"BROKEN_C": "c\n"})
	q, _ := sb.st.GetQuarantine(ctx, "add-c")
	if q == nil || q.Status != "blocked" || q.Reason != "gate_failure" || !strings.Contains(q.Details, "go:TestBROKEN_C") {
		t.Fatalf("add-c quarantine = %+v, want gate_failure naming go:TestBROKEN_C", q)
	}

	// A person fixes the test outside the pipeline; the next change integrates over the fix. From then
	// on the acceptance no longer carries: a change that breaks that same test again is its failure.
	sb.pushCoreChanges(t, nil, []string{"BROKEN_A"}, "test: fix the broken test")
	sb.runCoreDispatch(t, "add-d", map[string]string{"d.txt": "d\n"})
	sb.mergerRun(t)
	sb.requireIntegrated(t, "add-d")
	sb.runCoreDispatch(t, "add-e", map[string]string{"BROKEN_A": "again\n"})
	if q, _ := sb.st.GetQuarantine(ctx, "add-e"); q == nil || q.Status != "blocked" || q.Reason != "gate_failure" {
		ready, _ := sb.st.ListReadyQueue(ctx)
		t.Fatalf("add-e quarantine = %+v (ready=%+v), want gate_failure: the fixed test's acceptance must not carry", q, ready)
	}
}
