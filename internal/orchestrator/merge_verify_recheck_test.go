package orchestrator

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// recheckClock is the merger's clock in the recheck tests: the base's verdicts and the recheck delay are
// measured with it, so an hour passes at once.
type recheckClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *recheckClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *recheckClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// newRecheckSandbox is a sandbox whose core regression fails while $MARKER_DIR/db-down exists (a
// service the regression needs is down), with a hotfix of a regression.when path on core's base, the
// held change core-feature, and merge.base_recheck_after as given: one automatic recheck that long
// after a failure, or never.
func newRecheckSandbox(t *testing.T, recheckAfter string) (*mcfSandbox, string, string) {
	t.Helper()
	sb := newMCFSandboxWith(t, mcfOptions{coreRegressionFailsWhen: `[ -e "$MARKER_DIR/db-down" ]`, baseRecheckAfter: recheckAfter})
	sb.buildCoreChange(t, "core-feature")
	hotfix := sb.pushCoreHotfix(t)
	dbDown := filepath.Join(sb.markers, "db-down")
	mustWriteFile(t, dbDown, "down\n")
	return sb, hotfix, dbDown
}

// A base check that failed for a reason that healed is checked again once, merge.base_recheck_after
// later, and the change it held integrates. The fixture retries no blocked change
// (blocked_changes.retries: 0): that key no longer turns the recheck of a base off.
func TestMergeVerifyBrokenBaseIsRecheckedOnceAndIntegratesWhenItHealed(t *testing.T) {
	sb, hotfix, dbDown := newRecheckSandbox(t, "1h")
	ctx := context.Background()
	clock := &recheckClock{now: time.Now().UTC()}
	m := sb.newMerger(t, MergerOptions{NowFunc: clock.Now, BaseRecheckInterval: time.Hour, Out: &bytes.Buffer{}})

	runMergerOnce(t, m)
	sb.requireHeld(t, "core-feature")
	if v := sb.verdict(t, hotfix, BaseCheckRegression); v == nil || v.Status != store.BaseVerdictFailed {
		t.Fatalf("expected a failed regression verdict, got %+v", v)
	}
	var status bytes.Buffer
	_ = cycle.Status(ctx, sb.umbrella, sb.st, &status)
	if !strings.Contains(status.String(), "checked again automatically once at ") {
		t.Fatalf("expected foreman-kit status to say when the base is checked again, got:\n%s", status.String())
	}

	// The service is back, but the hold lasts until the recheck is due.
	if err := os.Remove(dbDown); err != nil {
		t.Fatal(err)
	}
	runMergerOnce(t, m)
	sb.requireHeld(t, "core-feature")
	if got := sb.coreRuns(t, "regression"); len(got) != 1 {
		t.Fatalf("the base was checked again before the delay: %+v", got)
	}

	clock.Advance(61 * time.Minute)
	runMergerOnce(t, m)
	sb.requireIntegrated(t, "core-feature")
	if got := sb.coreRuns(t, "regression"); len(got) != 2 || got[1].head != hotfix {
		t.Fatalf("expected one more regression on the base alone, got %+v", got)
	}
	if evs := sb.events(t, "merge.base_rechecked"); len(evs) != 1 || evs[0].Payload["sha"] != hotfix {
		t.Fatalf("expected one merge.base_rechecked, got %+v", evs)
	}
	if bases, _ := sb.st.ListBrokenBases(ctx); len(bases) != 0 {
		t.Fatalf("expected no broken base after the recheck passed, got %+v", bases)
	}
}

// A base that still fails when it is checked again stays broken, says so, and is never rechecked again.
func TestMergeVerifyBrokenBaseIsRecheckedOnlyOnce(t *testing.T) {
	sb, hotfix, _ := newRecheckSandbox(t, "1h")
	ctx := context.Background()
	clock := &recheckClock{now: time.Now().UTC()}
	m := sb.newMerger(t, MergerOptions{NowFunc: clock.Now, BaseRecheckInterval: time.Hour, Out: &bytes.Buffer{}})

	runMergerOnce(t, m)
	sb.requireHeld(t, "core-feature")
	clock.Advance(61 * time.Minute)
	runMergerOnce(t, m)
	sb.requireHeld(t, "core-feature")
	broken := sb.events(t, "merge.base_broken")
	if len(broken) != 2 || broken[1].Payload["rechecked"] != true {
		t.Fatalf("expected a second merge.base_broken marked rechecked, got %+v", broken)
	}
	clock.Advance(3 * time.Hour)
	runMergerOnce(t, m)
	sb.requireHeld(t, "core-feature")
	if got := sb.coreRuns(t, "regression"); len(got) != 2 {
		t.Fatalf("expected exactly one recheck, got %d regression runs: %+v", len(got), got)
	}
	if evs := sb.events(t, "merge.base_rechecked"); len(evs) != 1 {
		t.Fatalf("expected one merge.base_rechecked, got %+v", evs)
	}
	bases := cycle.BrokenBases(ctx, sb.umbrella, sb.st)
	if len(bases) != 1 || bases[0].SHA != hotfix || bases[0].RecheckedAt == "" || bases[0].RecheckAt != "" {
		t.Fatalf("expected the broken base with its recheck spent, got %+v", bases)
	}
	if !strings.Contains(bases[0].StatusLine(), "it still failed when checked again automatically at ") {
		t.Fatalf("unexpected status line: %s", bases[0].StatusLine())
	}
}

// merge.base_recheck_after: never: a broken base waits for the base to move or for a person.
func TestMergeVerifyNoRecheckWhenTheKeySaysNever(t *testing.T) {
	sb, _, _ := newRecheckSandbox(t, "never")
	ctx := context.Background()
	clock := &recheckClock{now: time.Now().UTC()}
	m := sb.newMerger(t, MergerOptions{NowFunc: clock.Now, BaseRecheckInterval: time.Hour, Out: &bytes.Buffer{}})

	runMergerOnce(t, m)
	clock.Advance(5 * time.Hour)
	runMergerOnce(t, m)
	sb.requireHeld(t, "core-feature")
	if evs := sb.events(t, "merge.base_rechecked"); len(evs) != 0 {
		t.Fatalf("a base was rechecked with merge.base_recheck_after: never: %+v", evs)
	}
	if bases := cycle.BrokenBases(ctx, sb.umbrella, sb.st); len(bases) != 1 || bases[0].RecheckAt != "" {
		t.Fatalf("expected no recheck announced, got %+v", bases)
	}
}
