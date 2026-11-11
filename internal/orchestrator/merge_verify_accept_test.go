package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// Accepting a base that fails a fast gate on its own integrates the changes it held, as base accept
// promises ("integrate them anyway"): the same gate fails again with each held change, because it fails
// on the base, and that failure must not quarantine them.
func TestMergeVerifyAcceptedGateBrokenBaseIntegratesTheHeldChange(t *testing.T) {
	sb := newMCFSandbox(t, "")
	sb.buildCoreChange(t, "core-feature")
	broken := sb.pushCoreChanges(t, map[string]string{"hotfix.txt": "hotfix\n", "BROKEN": "broken\n"}, nil, "fix(core): a hotfix that breaks the tests")
	t.Setenv("USER", "alice")
	ctx := context.Background()

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	sb.requireHeld(t, "core-feature")
	if v := sb.verdict(t, broken, BaseCheckGatePrefix+"test"); v == nil || v.Status != store.BaseVerdictFailed {
		t.Fatalf("expected a failed gate:test verdict, got %+v", v)
	}

	var out, errOut bytes.Buffer
	if code := cycle.RunBaseCLI(ctx, sb.umbrella, sb.st, []string{"accept", "repos/core"}, &out, &errOut); code != 0 {
		t.Fatalf("base accept failed (%d): %s", code, errOut.String())
	}

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	sb.requireIntegrated(t, "core-feature")
	if q, _ := sb.st.GetQuarantine(ctx, "core-feature"); q != nil {
		t.Fatalf("an accepted base quarantined its held change: %+v", q)
	}
	if evs := sb.events(t, "merge.gates_accepted"); len(evs) != 1 || !strings.Contains(fmt.Sprint(evs[0].Payload["gates"]), "core:test") {
		t.Fatalf("expected one merge.gates_accepted naming core:test, got %+v", evs)
	}
}

// On an accepted base, a failing gate whose check has no accepted verdict there is still the change's
// failure: the human accepted the test gate, not the regression the change broke.
func TestMergeVerifyAcceptedBaseStillQuarantinesAnUnacceptedFailure(t *testing.T) {
	sb := newMCFSandboxWith(t, mcfOptions{
		coreTestFailsWhen:       "[ -e hotfix.txt ]",
		coreRegressionFailsWhen: "[ -e hotfix.txt ] && [ -e config/locales/fr.yml ]",
	})
	sb.buildCoreChangeWriting(t, "core-locale", map[string]string{"config/locales/fr.yml": "fr:\n  saludo: \"Salut\"\n"})
	hotfix := sb.pushCoreCommit(t, "hotfix.txt", "hotfix\n", "fix(core): a hotfix that breaks the tests")
	ctx := context.Background()

	// The base's test gate failed on its own and a human accepted it.
	if _, err := sb.st.RecordBaseVerification(ctx, store.BaseVerification{RepoPath: "repos/core", BaseSHA: hotfix, CheckName: BaseCheckGatePrefix + "test", Status: store.BaseVerdictFailed, ExitCode: 1}); err != nil {
		t.Fatal(err)
	}
	if n, err := sb.st.AcceptBaseVerifications(ctx, "repos/core", hotfix, "alice", time.Now()); err != nil || n != 1 {
		t.Fatalf("accept: n=%d err=%v", n, err)
	}

	runMergerOnce(t, sb.newMerger(t, MergerOptions{}))
	q, err := sb.st.GetQuarantine(ctx, "core-locale")
	if err != nil || q == nil || q.Reason != "gate_failure" {
		t.Fatalf("expected core-locale quarantined as gate_failure, got %+v (err=%v)", q, err)
	}
	if evs := sb.events(t, "merge.gates_accepted"); len(evs) != 0 {
		t.Fatalf("an unaccepted failure was integrated anyway: %+v", evs)
	}
}
