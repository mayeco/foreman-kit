package orchestrator

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/proc"
	"github.com/mayeco/foreman-kit/internal/store"
)

// recordCoreAt moves the umbrella's gitlink of repos/core to sha and pushes it: the base the next
// workers build on, which the merger never checked.
func (sb *mcfSandbox) recordCoreAt(t *testing.T, sha string) {
	t.Helper()
	core := filepath.Join(sb.umbrella, "repos", "core")
	mcfGit(t, core, "fetch", "--quiet", "origin")
	mcfGit(t, core, "checkout", "--quiet", "--detach", sha)
	mcfGit(t, sb.umbrella, "add", "repos/core")
	mcfGit(t, sb.umbrella, "commit", "--quiet", "-m", "chore: record core at "+sha[:8])
	mcfGit(t, sb.umbrella, "push", "--quiet", "origin", "master")
	mcfGit(t, sb.umbrella, "fetch", "--quiet", "origin")
}

func (sb *mcfSandbox) baseAccept(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	verify := func(ctx context.Context, repo, sha string) (string, error) {
		return VerifyBaseForAccept(ctx, sb.umbrella, sb.st, repo, sha, &out)
	}
	code := cycle.RunBaseCLIWith(context.Background(), sb.umbrella, sb.st, append([]string{"accept"}, args...), &out, &errOut, verify)
	return code, out.String(), errOut.String()
}

// TestBaseAcceptChecksABaseOnlyAWorkerSawFail: a test fails on the base the umbrella records, a worker
// meets it in its BUILD and the agent asks for foreman-kit base accept, but the merger never checked
// that base, so there was no verdict to accept ("no failed verdict"). Accept now checks the base on its
// own first, accepts what fails there, and the worker's gates then see that failure as accepted.
func TestBaseAcceptChecksABaseOnlyAWorkerSawFail(t *testing.T) {
	sb := newMCFSandboxWith(t, mcfOptions{
		coreTestFailsWhen: `{ found=1; for f in BROKEN_*; do [ -e "$f" ] || continue; echo "--- FAIL: Test$f (0.00s)"; found=0; done; [ $found = 0 ]; }`,
	})
	ctx := context.Background()
	broken := sb.pushCoreCommit(t, "BROKEN_A", "x\n", "test: a test that fails on the base")
	sb.recordCoreAt(t, broken)
	coreHead := mcfGit(t, filepath.Join(sb.umbrella, "repos", "core"), "rev-parse", "HEAD")

	// Without the check, as before: nothing to accept.
	var out, errOut bytes.Buffer
	if code := cycle.RunBaseCLI(ctx, sb.umbrella, sb.st, []string{"accept", "repos/core"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "has no broken base") {
		t.Fatalf("accept without a check: code %d, %q", code, errOut.String())
	}

	code, said, errSaid := sb.baseAccept(t, "core")
	if code != 0 {
		t.Fatalf("base accept: code %d\n%s%s", code, said, errSaid)
	}
	for _, want := range []string{"has no verdict yet: checking it on its own", "test gate: failed on the base alone", "regression: passed on the base alone", "accepted: repos/core at " + broken[:8]} {
		if !strings.Contains(said, want) {
			t.Errorf("base accept does not say %q:\n%s", want, said)
		}
	}
	if v := sb.verdict(t, broken, BaseCheckGatePrefix+"test"); v == nil || v.Status != store.BaseVerdictAccepted || !strings.Contains(v.Excerpt, "TestBROKEN_A") {
		t.Fatalf("the test gate's verdict on the base: %+v", v)
	}
	if v := sb.verdict(t, broken, BaseCheckRegression); v == nil || v.Status != store.BaseVerdictPassed {
		t.Fatalf("the regression's verdict on the base: %+v", v)
	}

	// The main checkout is as it was, and the worktrees of the check are gone.
	if got := mcfGit(t, filepath.Join(sb.umbrella, "repos", "core"), "rev-parse", "HEAD"); got != coreHead {
		t.Errorf("core in the main checkout moved from %s to %s", coreHead, got)
	}
	if dirty := mcfGit(t, sb.umbrella, "status", "--porcelain"); dirty != "" {
		t.Errorf("the main checkout was changed:\n%s", dirty)
	}
	if list := mcfGit(t, sb.umbrella, "worktree", "list"); strings.Contains(list, "_base_accept") {
		t.Errorf("the worktree of the check is left:\n%s", list)
	}
	if entries, _ := os.ReadDir(filepath.Join(sb.umbrella, ".worktrees")); len(entries) != 0 {
		t.Errorf("something is left under .worktrees: %v", entries)
	}
	// Accepting it again says so, and checks nothing.
	if code, _, errSaid := sb.baseAccept(t, "core"); code != 1 || !strings.Contains(errSaid, "was accepted already") {
		t.Errorf("a second accept: code %d %q", code, errSaid)
	}

	// A worker's gates on that base see the failure as accepted, not as the change's.
	agentWT := filepath.Join(sb.tmp, "agent-view")
	mcfGit(t, sb.tmp, "clone", "-q", "-b", "master", sb.umbrellaBare, agentWT)
	mcfGit(t, agentWT, "submodule", "update", "--init", "-q", "repos/core")
	var gout, gerr bytes.Buffer
	if rc := proc.RunGatesCLIWith(ctx, agentWT, sb.st, []string{"--repo", "core"}, &gout, &gerr, GateAcceptor(sb.st, agentWT)); rc != 0 || !strings.Contains(gout.String(), "---- [accepted] core:test") {
		t.Fatalf("gates on the accepted base: rc=%d\n%s%s", rc, gout.String(), gerr.String())
	}
}

// TestBaseAcceptRefusesABaseThatPasses: when every check passes on the base alone, what failed in the
// worker is the change's, and nothing is accepted.
func TestBaseAcceptRefusesABaseThatPasses(t *testing.T) {
	sb := newMCFSandbox(t, "")
	head := mcfGit(t, filepath.Join(sb.umbrella, "repos", "core"), "rev-parse", "HEAD")
	code, said, errSaid := sb.baseAccept(t, "repos/core")
	if code != 1 || !strings.Contains(errSaid, "passes every check on its own: what fails is the change's") {
		t.Fatalf("accept of a base that passes: code %d\n%s%s", code, said, errSaid)
	}
	if v := sb.verdict(t, head, BaseCheckGatePrefix+"test"); v == nil || v.Status != store.BaseVerdictPassed {
		t.Fatalf("the verdict of the passing base: %+v", v)
	}
	if broken, _ := sb.st.ListBrokenBases(context.Background()); len(broken) != 0 {
		t.Fatalf("a base that passes is listed as broken: %+v", broken)
	}
	// A second accept reads the verdicts it recorded and runs nothing again.
	before := len(sb.coreRuns(t, "test"))
	if code, _, errSaid := sb.baseAccept(t, "repos/core", head[:8]); code != 1 || !strings.Contains(errSaid, "passes every check") {
		t.Fatalf("second accept: code %d %q", code, errSaid)
	}
	if after := len(sb.coreRuns(t, "test")); after != before {
		t.Errorf("the second accept ran the test gate again (%d runs, then %d)", before, after)
	}
}
